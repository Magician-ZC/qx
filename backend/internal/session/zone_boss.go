package session

// 文件说明：分区大世界阶段2 §3/§4——命运地图上的「区域 boss 挑战」与「区域副本进入」玩家直驱入口。
// 设计 docs/分区大世界设计方案-2026-06-10.md §4：复用既有 PvE 引擎（threat.go ResolveEliteEncounter /
// dungeon.go RunDungeonForSession），把它们接上「站位校验 + 区域等级缩放 + 防反复刷」的命运客户端入口。
//
//   - ChallengeZoneBoss：主角站在当前区 boss 坐标(BossCoord)附近(distance≤1)→按区域 BossLevel 构造 Threat→
//     ResolveEliteEncounter 全链结算→胜利则把 zoneID 记入 state.DefeatedBosses（防反复刷，失败可重试）。
//   - EnterZoneDungeon：主角站在当前区某 settlement(城镇)→floors 按区域等级派生→RunDungeonForSession 全链。
//
// 硬约束：guardPlayerAction 五道门；受保护字段经 Mutator（由 ResolveEliteEncounter/RunDungeon 内部保证）；
// FNV 确定性（boss Threat 的 ID 用稳定身份，掷骰可复现）；不新增 reason code（复用 THREAT_DEFEATED/ECONOMY_LOOT 等）。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const (
	// zoneBossReachDist 是「主角必须站在 boss 坐标附近」的六边形距离阈值（≤1：同格或相邻格才能开战）。
	zoneBossReachDist = 1
	// zoneDungeonFloorsPerLevel 把区域等级换算成副本层数的除数（zone.LevelMax/5），结果 clamp 到 [1,5]。
	zoneDungeonFloorsPerLevel = 5
	zoneDungeonMinFloors      = 1
	zoneDungeonMaxFloors      = 5
	// zoneBossLevelGuardGap 是「等级护栏」的软门阈值（设计 §3 等级护栏）：当区域 boss 等级
	// 高出主角等级 ≥ 此值时，ChallengeZoneBoss 直接返回「此地凶险」中文提示而不开战——
	// 引导玩家按等级带推进，避免低级角色反复撞墙白白损耗士气/装备。是软门（基于提示引导），
	// 不是硬锁；前端可据此提示做确认弹窗（现走 window.alert 兜底）。
	zoneBossLevelGuardGap = 5
)

// ChallengeZoneBoss 让主角挑战其当前区域的 boss（设计 §4 区域 boss）。真实动作：会真的改 HP/士气/钱包并落命运收件箱。
// 链路：guardPlayerAction 五道门 → 校验本区有 boss 且未被讨平 → 校验主角站在 BossCoord 附近(≤1) →
// 按 BossLevel 构造 Threat → ResolveEliteEncounter 全链结算（反射护栏/多回合战/分赃/惩罚/收件箱）→
// 胜利把 zoneID 记入 DefeatedBosses（防反复刷）+ Save。失败不标记（可重试）。
func (service *Service) ChallengeZoneBoss(ctx context.Context, sessionID, unitID string) (EliteEncounterResult, error) {
	if service == nil || service.units == nil {
		return EliteEncounterResult{}, fmt.Errorf("challenge zone boss: service unavailable")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return EliteEncounterResult{}, err
	}
	defer release()

	zone, ok := currentZone(&state)
	if !ok {
		return EliteEncounterResult{}, fmt.Errorf("这方世界尚未分疆裂土")
	}
	if zone.BossCoord == "" || zone.BossLevel <= 0 {
		return EliteEncounterResult{}, fmt.Errorf("这片天地没有可挑战的霸主")
	}
	// 防反复刷：本世界周期内该区 boss 已被讨平则拒绝再战（失败局不入集合，仍可重试）。
	if zoneBossDefeated(&state, zone.ID) {
		return EliteEncounterResult{}, fmt.Errorf("「%s」已被讨平，这片天地暂时太平了", zone.BossName)
	}
	// 站位校验：主角必须站在 boss 坐标附近（≤1 格）才能开战——不能隔着半张图遥控刷 boss。
	bossCoord, ok := parseCoordKey(zone.BossCoord)
	if !ok {
		return EliteEncounterResult{}, fmt.Errorf("这片天地的霸主踪迹不明")
	}
	if unit.HexDistance(rec.Status.PositionQ, rec.Status.PositionR, bossCoord.Q, bossCoord.R) > zoneBossReachDist {
		return EliteEncounterResult{}, fmt.Errorf("得先走到「%s」跟前，才能与它一较高下", zone.BossName)
	}
	// 等级护栏（设计 §3）：boss 高出主角 ≥zoneBossLevelGuardGap 级时返回「此地凶险」中文提示而不开战——
	// 引导玩家按等级带推进，避免低级角色被碾压（数值上打不动也打不死自己）却反复撞墙白白损耗。软门：基于提示引导、
	// 非硬锁；前端可据此中文消息做确认弹窗（现走 window.alert 兜底）。主角等级取 Growth.Level（非受保护字段，只读不改）。
	if msg, perilous := zoneBossPerilGuard(zone.BossName, zone.BossLevel, rec.Stats.Growth.Level); perilous {
		return EliteEncounterResult{}, fmt.Errorf("%s", msg)
	}

	// 按区域 BossLevel 构造 Threat（强度据等级缩放；RegionID=zoneID 让败北后果可走家乡遭劫扇出）。
	threat := zoneBossThreat(sessionID, zone.ID, zone.BossName, zone.BossLevel)

	result, err := service.ResolveEliteEncounter(ctx, &state, rec, threat)
	if err != nil {
		return result, err
	}

	// 胜利标记防刷 + 持久化（失败不标记，可重试）。ResolveEliteEncounter 已把主角 HP/士气/钱包经 Mutator 落库，
	// 这里 Save 的是 state（DefeatedBosses 集合 + 它内部 appendLog 的日志），与 travel 同口径用合并保存避免覆盖外部写。
	if result.Outcome == "defeated" {
		appendDefeatedBoss(&state, zone.ID)
		// 任务进度 hook（阶段3 §5.3）：讨平本区 boss → 推进匹配的 defeat_boss objective（target=zoneId）。
		// best-effort、纯逻辑（只改 state.ActiveQuests，随下方 saveSessionMergingExternalEvents 一并落库）。
		advanceQuestObjectives(&state, ObjectiveDefeatBoss, zone.ID, 1)
		// 入世界编年史（阶段4 §7.2 入史规则）：区域 boss 讨平 → boss_slain。best-effort，绝不影响结算。
		// worldTick=部署回合（state.TurnState.Turn）；WorldID 空（旧单图档）则 chronicle helper 内部 no-op。
		service.chronicleBossSlain(ctx, strings.TrimSpace(state.WorldID), state.TurnState.Turn, rec.ID, rec.DisplayName(), zone.BossName, zone.Name)
	}
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		return result, fmt.Errorf("challenge zone boss (save session): %w", err)
	}
	return result, nil
}

// zoneBossThreat 据区域 boss 等级构造一头 Threat（设计 §4：强度按 BossLevel 缩放，名字=BossName，Tier=field_boss）。
// 字段构造参考 scaledElite 的语义（Attack/Defense/HPPool/Severity/Loot），但**用 BossLevel 定强度而非 actor 战力**——
// 高等级区域的 boss 显著强（魔兽式分级体验），低级角色闯高级区会被碾压（设计 §3 等级护栏的结算落地）。
//   - HP：随等级线性放大（base 60 + level·12），单人多回合消耗战可破但需硬撑；
//   - Attack：level·1.5 + 6（单击有威胁、逼出承伤与撤退抉择，但 ResolveEliteEncounter 的反射护栏在 HP<25 时保命）；
//   - Defense：level/2 + 2（高等级 boss 更肉，玩家攻低则每击伤害被 eliteDamage 夹到下限 1，拖长战线）；
//   - Tier：field_boss（候选后果层=2，败北惩罚比 elite 重一档，符合「区域霸主」分量）；
//   - Loot：货币随等级递增 + 一件 epic 排他遗物（走 arbitration 归属，单人独得）。
//
// ID 用稳定身份（session+zone），不掺随机 UUID——combat_roll 把 target.ID 写进 FNV，稳定身份保证同会话同区掷骰可复现。
func zoneBossThreat(sessionID, zoneID, bossName string, bossLevel int) Threat {
	if bossLevel < 1 {
		bossLevel = 1
	}
	name := bossName
	if name == "" {
		name = "盘踞此地的霸主"
	}
	hp := 60 + bossLevel*12
	atk := bossLevel*3/2 + 6
	def := bossLevel/2 + 2
	return Threat{
		// 稳定身份（session+zone）：boss 唯一锚于「这一局这一区」，掷骰可复现；loot key/事件 Location 标签亦用此 ID。
		ID:       fmt.Sprintf("zoneboss_%s_%s", sessionID, zoneID),
		Name:     name,
		Tier:     ThreatTierFieldBoss, // 候选后果层=2（区域霸主分量重于路边精英）
		RegionID: zoneID,              // 败北后果可走家乡遭劫扇出（propagateDefeatToRegion）
		Power:    bossLevel * 20,
		Attack:   atk,
		Defense:  def,
		HPPool:   hp,
		Severity: 50 + float64(bossLevel)*2,
		Loot: []encounter.LootItem{
			{ID: "gold", Rarity: encounter.Common, Quantity: 20 + bossLevel*4},
			{ID: "zone_boss_relic", Rarity: encounter.Epic, Quantity: 1}, // 排他遗物，走 arbitration 归属（单人独得）
		},
	}
}

// zoneBossPerilGuard 是「等级护栏」软门（设计 §3）：boss 等级高出主角 ≥zoneBossLevelGuardGap 级时，
// 返回 (中文「此地凶险」提示, true)；否则 ("", false)。纯函数（无 I/O），便于单测与前端口径对齐。
//   - bossLevel：区域 boss 等级（=zone.BossLevel，=区域 LevelMax）。
//   - playerLevel：主角等级（rec.Stats.Growth.Level）。
//
// 调用方据 true 拒绝开战并把提示透出给玩家（前端可做确认弹窗）。
func zoneBossPerilGuard(bossName string, bossLevel, playerLevel int) (string, bool) {
	if bossLevel-playerLevel < zoneBossLevelGuardGap {
		return "", false
	}
	name := bossName
	if name == "" {
		name = "盘踞此地的霸主"
	}
	return fmt.Sprintf(
		"此地凶险，「%s」是 Lv%d 的霸主，她（Lv%d）还远未到能撼动它的时候——先在低等级带历练吧",
		name, bossLevel, playerLevel,
	), true
}

// zoneBossDefeated 判定某区 boss 是否已被讨平（在 state.DefeatedBosses 集合里）。
func zoneBossDefeated(state *State, zoneID string) bool {
	if state == nil {
		return false
	}
	for _, id := range state.DefeatedBosses {
		if id == zoneID {
			return true
		}
	}
	return false
}

// appendDefeatedBoss 幂等地把 zoneID 记入 state.DefeatedBosses（已在则 no-op）。
func appendDefeatedBoss(state *State, zoneID string) {
	if state == nil || zoneID == "" || zoneBossDefeated(state, zoneID) {
		return
	}
	state.DefeatedBosses = append(state.DefeatedBosses, zoneID)
}

// EnterZoneDungeon 让主角进入其当前区域城镇里的副本（设计 §4 副本）。真实动作：会改参战单位 HP/士气/钱包并落命运收件箱。
// 链路：guardPlayerAction 五道门 → 校验本区有副本入口(DungeonCoord) → 校验主角站在某 settlement(城镇)处 →
// floors 按区域等级派生(zone.LevelMax/5, clamp[1,5]) → RunDungeonForSession 全链（逐层消耗战/撤退保利/败北分级）。
// 注意：RunDungeon 受 QUNXIANG_DUNGEON flag 门控，关时返回 ErrDungeonDisabled（路由当 409 透出）。
func (service *Service) EnterZoneDungeon(ctx context.Context, sessionID, unitID string, floors int) (DungeonResult, error) {
	if service == nil || service.units == nil {
		return DungeonResult{}, fmt.Errorf("enter zone dungeon: service unavailable")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return DungeonResult{}, err
	}

	zone, ok := currentZone(&state)
	if !ok {
		release()
		return DungeonResult{}, fmt.Errorf("这方世界尚未分疆裂土")
	}
	if zone.DungeonCoord == "" {
		release()
		return DungeonResult{}, fmt.Errorf("这片天地没有可探的秘境")
	}
	// 站位校验：主角必须站在本区某城镇（settlement）处才能进副本（副本入口锚在城镇）。
	if !unitAtAnySettlement(rec, zone) {
		release()
		return DungeonResult{}, fmt.Errorf("得先回到「%s」的城镇，才能从那里探入秘境", zone.Name)
	}
	// floors：请求未指定（≤0）则按区域等级派生（zone.LevelMax/5），clamp 到 [1,5]。
	if floors <= 0 {
		floors = zoneDungeonFloors(zone.LevelMax)
	}
	if floors < zoneDungeonMinFloors {
		floors = zoneDungeonMinFloors
	}
	if floors > zoneDungeonMaxFloors {
		floors = zoneDungeonMaxFloors
	}
	// 先释放直驱锁，再跑副本：RunDungeonForSession 内部按 sessionID 自行重新加载会话态并经 Mutator 落库
	// （单人副本，无并发写者），与 ChallengeZoneBoss「持锁内 ResolveEliteEncounter」不同——dungeon 是独立的 session 级
	// 入口（既有 /dungeon 端点亦不持直驱锁）。持锁期间只做站位/floors 校验，校验完即放锁，避免长战斗久占锁阻塞其它直驱。
	release()

	return service.RunDungeonForSession(ctx, sessionID, []string{unitID}, floors)
}

// unitAtAnySettlement 判定主角是否正站在本区任一城镇坐标处（副本入口锚在城镇）。
// 容错：DungeonCoord 必在 settlements 之列（worldgen 取 settlements[0]），但用全体 settlements 判更宽松——
// 站在本区任意城镇都能下副本（避免只认入口那一格的苛刻判定）。无城镇/坐标解析失败 → false。
func unitAtAnySettlement(rec *unit.Record, zone world.Zone) bool {
	if rec == nil {
		return false
	}
	for _, key := range zone.Settlements {
		coord, ok := parseCoordKey(key)
		if !ok {
			continue
		}
		if rec.Status.PositionQ == coord.Q && rec.Status.PositionR == coord.R {
			return true
		}
	}
	return false
}

// zoneDungeonFloors 把区域等级换算成副本层数：LevelMax/5，clamp 到 [1,5]。
func zoneDungeonFloors(levelMax int) int {
	floors := levelMax / zoneDungeonFloorsPerLevel
	if floors < zoneDungeonMinFloors {
		floors = zoneDungeonMinFloors
	}
	if floors > zoneDungeonMaxFloors {
		floors = zoneDungeonMaxFloors
	}
	return floors
}
