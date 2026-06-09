package session

// 文件说明：最小阵营冲突战斗源（三阵营开放世界 F3，替代被删掉的固定敌方 NPC）。
//
// 背景：阵营开放世界把旧「固定对手阵营（EnemyUnitIDs 一开局即满）」改为「游历相遇时动态接入对手」。
// 本文件提供其最小切片——部署边界低频扫描：若主世界角色（带阵营）所在世界里有**敌对阵营**的人，
// 则以低频确定性（FNV）概率触发一次「阵营冲突遭遇」：
//   ① 据角色战力生成一名敌对阵营的对手单位（battle-ready、落库），append 进 state.EnemyUnitIDs
//      —— 让 updateOutcome 的胜负判定恢复（敌方非空）、且玩家可在战棋里手动接管这场战；
//   ② 经 SurfaceFateEvent 出一张命运卡（takeover=true，可一键接管打战棋），祖魂语气：
//      「她在路上撞见了{敌对阵营}的人，刀已出鞘……」。
//
// 阵营敌对关系（最小口径，设计：混乱vs秩序天然对立、自由游离）：
//   - order  ⇄ chaos：天然死敌（律法 vs 破立），互为敌对。
//   - freedom：游离——不主动与谁为敌，故不作为「主世界角色」一方触发冲突（freedom 角色不挑事）；
//     但 order/chaos 角色仍可撞见 freedom 的人吗？最小切片只取「order↔chaos」这对硬对立，freedom 不卷入，
//     避免一上来就三方混战。完整阵营战争（含 freedom 卷入条件、势力线、据点争夺）留后续。
//
// 纪律（对齐 social_scan.go / faction_switch.go）：
//   - flag 门控 QUNXIANG_FACTION_PVE **默认关** → 关时 maybeTriggerFactionConflict 直接早返回（零行为）。
//   - 低频确定性触发：每 N 个部署回合扫一次 + FNV(sessionID+turn+actor) 概率掷骰，绝不每回合开打。
//   - best-effort：缺依赖 / flag 关 / 不满足条件 / 写库失败 → 优雅返回（绝不阻断回合推进）。
//   - 确定性、付费不进（不读 wallet/billing）。受保护字段不直改（对手是新建单位，无既有受保护态变更）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// factionPvEFlagEnv 是阵营冲突遭遇的灰度开关环境变量名。**默认关** → 关时零行为。
const factionPvEFlagEnv = "QUNXIANG_FACTION_PVE"

const (
	// factionConflictEveryNTurns 阵营冲突扫描的部署回合周期：每 N 个部署回合扫一次（低频、与社交/撮合错频）。
	factionConflictEveryNTurns = 4
	// factionConflictProbability 单角色单次扫描真正触发冲突的确定性掷骰阈（小概率——低频 + 低概率双保险，绝不刷屏）。
	factionConflictProbability = 0.12
	// factionConflictMaxPerScan 单次扫描最多触发的冲突数（硬顶每回合冲突音量，避免一回合多场遭遇）。
	factionConflictMaxPerScan = 1
)

// factionPvEEnabled 读 QUNXIANG_FACTION_PVE（**默认关**）。仅显式 true/1/yes/on 才开启阵营冲突遭遇。
// 自包含解析，对齐 faction_switch.go / ambition_scoring.go 的 flag idiom。
func factionPvEEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride(factionPvEFlagEnv))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// hostileFactionFor 返回某阵营在最小切片里的「天然敌对阵营」ID；无对立（freedom 游离/未知）返回空串。
// 最小口径：order ⇄ chaos 互为死敌；freedom 游离不卷入（不挑事，故作为主动方时无对手）。纯函数、确定性。
func hostileFactionFor(factionID string) string {
	switch faction.Normalize(factionID) {
	case faction.IDOrder:
		return faction.IDChaos
	case faction.IDChaos:
		return faction.IDOrder
	default:
		return "" // freedom 游离 / 无阵营 → 无天然敌对方（不作为主动挑起冲突的一方）
	}
}

// factionConflictRoll 生成与会话上下文绑定的确定性掷骰值 [0,1)（FNV-32a，复用 combatActionRoll / factionSwitchRoll 口径）。
func factionConflictRoll(sessionID string, turn int, actorID string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(sessionID))
	_, _ = hasher.Write([]byte(actorID))
	_, _ = hasher.Write([]byte(fmt.Sprintf("|%d|factionconflict", turn)))
	return float64(hasher.Sum32()%10000) / 10000
}

// scanFactionConflicts 在部署边界低频扫描本局玩家角色，按确定性概率触发阵营冲突遭遇（生成对手 + 命运卡）。
//
// 守卫：nil 依赖 / flag 关 / 未到周期 / 无候选角色 → no-op。每次扫描至多触发 factionConflictMaxPerScan 场。
// 全程 best-effort：单个角色触发失败只吞错（绝不影响其余角色与回合推进）。
// 触发的对手会 append 进 state.EnemyUnitIDs（调用方在边界结算后会 Save state），由 updateOutcome / 战棋接管消费。
func (service *Service) scanFactionConflicts(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || service.units == nil || service.db == nil || state == nil {
		return
	}
	if !factionPvEEnabled() {
		return // flag 关：零行为（不扫、不 roll、不造对手、不留痕）。
	}
	if ctx == nil {
		ctx = context.Background()
	}
	turn := state.TurnState.Turn
	// 低频触发：每 factionConflictEveryNTurns 个部署回合扫一次（确定性 turn 取模）。
	if factionConflictEveryNTurns <= 0 || turn%factionConflictEveryNTurns != 0 {
		return
	}

	triggered := 0
	for i := range units {
		if triggered >= factionConflictMaxPerScan {
			break // 每次扫描触发上限：硬顶冲突音量
		}
		actor := units[i]
		// 仅本局玩家阵营、存活、带阵营且有天然敌对方的角色才可能撞见敌对阵营的人。
		if actor.ID == "" || actor.Status.LifeState == unit.LifeStateDead {
			continue
		}
		if !isPlayerControlledUnit(state, actor.ID) {
			continue
		}
		hostile := hostileFactionFor(actor.Faction)
		if hostile == "" {
			continue // freedom 游离 / 无阵营 → 不主动挑起冲突
		}
		// 确定性概率掷骰：低频周期内仍只小概率真正撞上（绝不每个角色每周期都开打）。
		if factionConflictRoll(state.ID, turn, actor.ID) >= factionConflictProbability {
			continue
		}
		if service.triggerFactionConflict(ctx, state, &actor, hostile, turn) {
			triggered++
		}
	}
}

// triggerFactionConflict 为一名角色生成一名敌对阵营对手并接入战斗源（append EnemyUnitIDs + 命运卡可接管）。
//
// 链路：生成与角色战力相称的敌对阵营对手（battle-ready、落库）→ append 进 state.EnemyUnitIDs →
// FACTION_CONFLICT 流程留痕（best-effort）→ SurfaceFateEvent 出命运卡（takeover=true）。
// 返回是否真的触发了（造出对手并入队）。best-effort：任一步失败优雅返回（已入队的对手会随 state 落库）。
func (service *Service) triggerFactionConflict(ctx context.Context, state *State, actor *unit.Record, hostileFactionID string, turn int) bool {
	if service == nil || service.units == nil || state == nil || actor == nil {
		return false
	}
	hostileDef, ok := faction.Get(hostileFactionID)
	if !ok {
		return false
	}

	opponent, err := service.buildFactionConflictOpponent(state, actor, hostileFactionID, turn)
	if err != nil {
		return false
	}
	if err := service.units.Save(ctx, opponent); err != nil {
		return false // 造对手失败：best-effort 放弃本场（不入队、不留痕、不出卡）。
	}

	// 接入战斗源：append 进敌方队列——让 updateOutcome 的胜负判定恢复（敌方非空），且玩家可手动接管这场战。
	state.EnemyUnitIDs = append(state.EnemyUnitIDs, opponent.ID)
	// 把敌对阵营纳入外交战争关系（best-effort，确定性）：使 opposingIDs/视野判定把对手视为敌方。
	ensureFactionRelations(state)

	hostileZH := hostileDef.NameZH
	cardSummary := fmt.Sprintf("她在路上撞见了%s阵营的人，刀已出鞘……", hostileZH)

	// FACTION_CONFLICT 流程留痕（旁路，非状态变更；best-effort：吞错只继续）。
	if service.db != nil {
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:     state.ID,
			OwnerUnitID:   actor.ID,
			RelatedUnitID: opponent.ID,
			Code:          events.ReasonFactionConflict,
			Category:      events.CategoryLifecycle,
			Payload: map[string]any{
				"hostile_faction": hostileFactionID,
				"opponent_id":     opponent.ID,
				"actor_faction":   faction.Normalize(actor.Faction),
			},
			WorldID: state.WorldID,
			Tick:    turn,
		})
	}

	appendLog(state, "faction_conflict", cardSummary, actor.ID, opponent.ID)

	// 经命运卡 surface（可接管打战棋）：祖魂语气 + 可接管战上下文。best-effort，绝不阻断。
	_, _ = service.SurfaceFateEvent(ctx, state.ID, actor, FateEvent{
		ActorID:       actor.ID,
		TargetID:      actor.ID,
		ReasonCode:    events.ReasonFactionConflict,
		Importance:    7,
		EmotionWeight: -0.5,
		Summary:       cardSummary,
		Battle: &FateBattleContext{
			SessionID:  state.ID,
			ThreatID:   opponent.ID,
			ThreatName: fmt.Sprintf("%s阵营·%s", hostileZH, opponent.DisplayName()),
			Tier:       "faction_conflict",
			Takeover:   true, // 可一键接管打战棋（对手已入 EnemyUnitIDs，战棋主循环可推进这场战）
		},
	})

	service.pushRealtime(state.ID, "faction_conflict", map[string]any{
		"actor_id":        actor.ID,
		"opponent_id":     opponent.ID,
		"hostile_faction": hostileFactionID,
	})
	return true
}

// buildFactionConflictOpponent 生成一名与角色战力相称的敌对阵营对手（battle-ready、带敌对阵营道德底色）。
// 复用 bootstrapBattleUnit 底座（带武器/护甲/地图落点），再覆写敌对阵营字段（Faction/MoralAlignment/出身/落点）。
// 落点取敌方阵营出生侧（与玩家错位，避免初始重叠）；确定性：seed 取 FNV(sessionID+turn+actor)。
func (service *Service) buildFactionConflictOpponent(state *State, actor *unit.Record, hostileFactionID string, turn int) (unit.Record, error) {
	if state == nil || actor == nil {
		return unit.Record{}, fmt.Errorf("build faction conflict opponent: nil state/actor")
	}
	hostileDef, ok := faction.Get(hostileFactionID)
	if !ok {
		return unit.Record{}, fmt.Errorf("build faction conflict opponent: invalid faction %q", hostileFactionID)
	}

	// 确定性 seed（绑会话/回合/角色，可复现）。
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("factionconflict|%s|%d|%s", state.ID, turn, actor.ID)))
	seed := int64(h.Sum64() >> 1) // 右移避负，落正范围

	name := factionConflictOpponentName(hostileFactionID, h.Sum64())
	// 对手落在敌方阵营侧（与玩家角色错位）；坐标取角色镜像偏移的稳定点（无需地图也能给个合法落点）。
	coord := world.Coord{Q: actor.Status.PositionQ + 2, R: actor.Status.PositionR + 1}

	// 关键：以敌方阵营 ID（state.EnemyFactionID）落 FactionID，使 opposingIDs 把对手视为敌方（与玩家阵营对立）。
	// EnemyFactionID 缺省时回落一个稳定的对立阵营标识，仍与玩家 FactionID 不同即可。
	enemyTeam := strings.TrimSpace(state.EnemyFactionID)
	if enemyTeam == "" || enemyTeam == state.PlayerFactionID {
		enemyTeam = "faction_conflict_" + hostileFactionID
	}

	opponent, err := bootstrapBattleUnit(seed, state.ID, enemyTeam, name, coord)
	if err != nil {
		return unit.Record{}, err
	}
	// 唯一 ID（避免与既有单位撞键）。
	opponent.ID = "fconflict_" + uuid.NewString()
	// 敌对阵营道德底色（非保护字段，直接写——不走 Mutator，仿 faction_spawn）。
	opponent.Faction = hostileFactionID
	opponent.MoralAlignment = faction.PerturbBaseline(hostileFactionID, seed, "fconflict", factionMoralJitter)
	opponent.Identity.Lineage = factionNPCLineagePrefix + "游骑·" + hostileDef.NameZH
	opponent.Identity.Biography = fmt.Sprintf("%s阵营的游骑，在路上与她狭路相逢。%s", hostileDef.NameZH, hostileDef.MoralCreed)
	opponent.Ambition = unit.DeriveAmbition(seed, opponent.Identity.Lineage, opponent.Identity.Biography)
	// 注意：world_id/region_id 是 DB 列（经 SetUnitScope 赋值），非 Record 内嵌字段；
	// 本对手是本局战斗源单位，作用域跟随会话即可，无需额外入世界标记（最小切片）。
	return opponent, nil
}

// factionConflictOpponentName 据阵营 + 哈希确定性生成对手名（复用阵营名字池，加敌对阵营出身）。
func factionConflictOpponentName(hostileFactionID string, h uint64) string {
	given := factionGivenM
	if h%2 == 0 {
		given = factionGivenF
	}
	return pickFromPool(factionSurnames, h>>3) + pickFromPool(given, h>>11)
}

// isPlayerControlledUnit best-effort 判定某单位是否本局玩家可控角色（在 PlayerUnitIDs 里）。
func isPlayerControlledUnit(state *State, unitID string) bool {
	if state == nil || unitID == "" {
		return false
	}
	for _, id := range state.PlayerUnitIDs {
		if id == unitID {
			return true
		}
	}
	return false
}
