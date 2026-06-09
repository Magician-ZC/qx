package session

// 文件说明：副本(Dungeon)同步核心切片，把 engine/encounter 原语接入「多层逐层推进」的 PvE 玩法。
// 链路：逐层确定性 combat_roll 消耗战（层数越深敌越强，末层 floor boss 更强）→ 每层胜则累计 loot 继续 /
// HP 危急可反射撤退（保留已得 loot）/ 败则 encounter.DegradePenalty 后果分级闸结算并终止 → 通关后
// encounter.AllocateLoot 总分赃（排他件走 arbitration 胜率∝贡献、付费不进）→ 全程经 status.Mutator 留痕。
// 设计见 docs/PvE威胁系统.md「副本」。本波是【同步可玩核心】：单进程内一次性逐层跑完。
//
// 本波覆盖范围（同步核心）：
//   - 多层逐层推进、确定性难度递增、末层 boss、累计/总分赃、中途撤退保利、败北分级惩罚、HP 全经 Mutator。
// 留作后续（异步大工程，本文件刻意不实现）：
//   - 异步分段执行（每层一个可中断片段、跨回合续跑）；
//   - lazy catch-up（玩家离线期间副本进度的惰性补算）；
//   - charter 超时续命（offline_charter 接管断线玩家、超时自动撤退/续命决策）。
// 这些不阻塞同步核心的可玩性，整合见 GDD §7.3。

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

// ErrDungeonDisabled 在 QUNXIANG_DUNGEON 未开时由入口返回——关时零行为。
var ErrDungeonDisabled = fmt.Errorf("dungeon: 功能未启用（设 QUNXIANG_DUNGEON=true 开启）")

const (
	dungeonMinFloors = 1
	dungeonMaxFloors = 10 // 防滥用上限：单次副本最多 10 层
)

// dungeonEnabled 读 QUNXIANG_DUNGEON（true/1/yes/on 视为开），默认关 → RunDungeon 直接返回 ErrDungeonDisabled、零行为。
func dungeonEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_DUNGEON"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// DungeonFloorResult 单层结算结果。
type DungeonFloorResult struct {
	Floor       int
	IsBoss      bool // 末层 boss
	ThreatName  string
	Outcome     string // cleared / fled / wiped（本层全员倒下或队伍崩溃）
	Rounds      int
	DamageDealt int
	DamageTaken int
}

// DungeonResult 一次副本的整体结算。
type DungeonResult struct {
	DungeonID    string
	Floors       int                  // 计划层数
	FloorsClear  int                  // 实际通关层数
	Outcome      string               // cleared（通关）/ fled（撤退）/ wiped（陨落）
	FloorResults []DungeonFloorResult // 逐层结果
	Awards       []encounter.Award    // 总分赃（仅通关）
	Contribution map[string]float64   // 各参战单位累计贡献分
	PenaltyLayer map[string]int       // 败北时各单位经分级闸落地的后果层
	InboxCards   map[string]string    // 各参战单位的祖魂语气命运卡
}

// dungeonRun 是副本推进的内部可变态（不导出，逐层累计）。
type dungeonRun struct {
	id      string
	floors  int
	members []*memberCombatState // 复用 field boss 的队员战斗态（contrib 在副本内跨层累计）
	state   *State
}

// RunDungeon 同步跑通一次多层副本（设计 PvE威胁系统.md「副本」）。真实动作：会改参战单位 HP/士气/钱包并落命运收件箱。
//   - 逐层用确定性 combat_roll 跑一场遭遇，层数越深难度越高（FNV(sessionID+turn+floor) 定敌强）；末层为 floor boss（更强）。
//   - 每层胜则累计 loot 继续；HP 危急反射撤退（保留已得 loot、不再继续）；败则 DegradePenalty 结算并终止。
//   - 通关后 encounter.AllocateLoot 总分赃（排他件走 arbitration 胜率∝贡献、频率无关、付费不进）。
//   - HP/士气/钱包全部经 status.Mutator 留痕。best-effort 的命运收件箱投递失败不回滚战斗结算。

// RunDungeonForSession 是 HTTP 入口用的按 sessionID 包装：先加载会话态（保留 WorldID/turn 上下文供确定性与世界化），
// 加载失败则退化为仅含 ID 的最小 State（RunDungeon 仍按 unitIDs 从 DB 取单位、可跑核心结算）。
func (service *Service) RunDungeonForSession(ctx context.Context, sessionID string, unitIDs []string, floors int) (DungeonResult, error) {
	state := service.loadStateForFate(ctx, sessionID)
	if state == nil {
		state = &State{ID: sessionID}
	}
	return service.RunDungeon(ctx, state, unitIDs, floors)
}

func (service *Service) RunDungeon(ctx context.Context, state *State, unitIDs []string, floors int) (DungeonResult, error) {
	result := DungeonResult{
		Contribution: map[string]float64{},
		PenaltyLayer: map[string]int{},
		InboxCards:   map[string]string{},
	}
	if !dungeonEnabled() {
		return result, ErrDungeonDisabled
	}
	if service == nil || service.mutator == nil || service.units == nil || state == nil {
		return result, fmt.Errorf("run dungeon: missing dependencies")
	}
	if len(unitIDs) == 0 {
		return result, fmt.Errorf("run dungeon: empty party")
	}
	if floors < dungeonMinFloors {
		floors = dungeonMinFloors
	}
	if floors > dungeonMaxFloors {
		floors = dungeonMaxFloors
	}
	result.Floors = floors
	result.DungeonID = "dungeon_" + uuid.NewString()

	// 载入参战单位，建立逐层累计的战斗态。
	members := make([]*memberCombatState, 0, len(unitIDs))
	for _, id := range unitIDs {
		rec, err := service.units.GetByID(ctx, id)
		if err != nil {
			return result, fmt.Errorf("load party member %s: %w", id, err)
		}
		member := rec
		members = append(members, &memberCombatState{rec: &member, status: "contributed"})
	}

	run := &dungeonRun{id: result.DungeonID, floors: floors, members: members, state: state}

	// 逐层推进。败/退/全灭即终止。
	for floor := 1; floor <= floors; floor++ {
		floorRes, terminal, err := service.runDungeonFloor(ctx, run, floor)
		if err != nil {
			return result, err
		}
		result.FloorResults = append(result.FloorResults, floorRes)
		if floorRes.Outcome == "cleared" {
			result.FloorsClear++
		}
		if terminal != "" {
			result.Outcome = terminal
			break
		}
	}
	if result.Outcome == "" {
		result.Outcome = "cleared" // 跑满全部层数且未被中途终止
	}

	if err := service.settleDungeon(ctx, run, &result); err != nil {
		return result, err
	}
	return result, nil
}

// runDungeonFloor 跑通一层遭遇。返回该层结果与 terminal（""=继续；"fled"/"wiped"=终止整局）。
// 难度由确定性 FNV(sessionID+turn+floor) 与层深共同决定，末层是更强的 floor boss。
func (service *Service) runDungeonFloor(ctx context.Context, run *dungeonRun, floor int) (DungeonFloorResult, string, error) {
	threat := scaledDungeonFloor(run.state, run.members, floor, run.floors)
	// 同步核心：从满血、第 1 回合一次性跑完本层（不持久化中间态）；异步分段路径复用同一核心、传入续跑的 mobHP/startRound。
	return service.runDungeonFloorCore(ctx, run, floor, threat, threat.HPPool, 1)
}

// runDungeonFloorCore 是单层消耗战的**可续跑核心**（同步核心与异步分段共用）。
// 入参 mobHP/startRound 支持断点续跑：异步分段把上次留库的 boss_hp_remaining/floor_round 喂进来，敌血不重置、回合不回退。
// 返回该层结果（其中 floorRes.Rounds 是本层累计到的回合数）与 terminal（""=继续；"fled"/"wiped"=终止整局）。
// 确定性：salt 仅含 floor/round/unitID，与 mobHP 的起点无关——从满血一次跑完，与分段断点续跑得到逐字节一致的结果。
func (service *Service) runDungeonFloorCore(ctx context.Context, run *dungeonRun, floor int, threat Threat, mobHP int, startRound int) (DungeonFloorResult, string, error) {
	isBoss := floor == run.floors
	floorRes := DungeonFloorResult{Floor: floor, IsBoss: isBoss, ThreatName: threat.Name, Outcome: "wiped"}
	if startRound < 1 {
		startRound = 1
	}

	// 关键：combat_roll 把 target.ID 写进 FNV，故 mob 必须用**稳定**身份（session+floor），不能用 threat.ID 里的随机 UUID，
	// 否则同会话同回合两次副本的掷骰会发散、破坏确定性可复现（threat.ID 的 UUID 只用于 loot key/事件 Location 标签）。
	mob := unit.Record{ID: fmt.Sprintf("dgmob:%s:f%d", run.state.ID, floor)}
	mob.Status.Attack = threat.Attack
	mob.Status.Defense = threat.Defense
	turn := run.state.TurnState.Turn

	for round := startRound; round <= eliteMaxRounds; round++ {
		floorRes.Rounds = round

		anyActive := false
		fled := false
		for _, ms := range run.members {
			if ms.status != "contributed" {
				continue
			}
			if ms.rec.Status.HP < eliteFleeHP { // 反射护栏：危急先撤（撤退保利），零 LLM
				ms.status = "fled"
				fled = true
				continue
			}
			anyActive = true
			salt := fmt.Sprintf("dg_atk:%s:f%d:r%d", ms.rec.ID, floor, round)
			if combatActionRoll(*run.state, *ms.rec, mob, salt) >= eliteMissChance {
				dmg := eliteDamage(ms.rec.Status.Attack, threat.Defense)
				mobHP -= dmg
				ms.dealt += dmg
				ms.contrib.Damage += float64(dmg) // 跨层累计
				floorRes.DamageDealt += dmg
			}
		}
		if mobHP <= 0 {
			floorRes.Outcome = "cleared"
			return floorRes, "", nil
		}
		if !anyActive {
			// 无人可战：要么全撤（fled）、要么全倒（wiped）。
			if fled || service.dungeonAnyFled(run) {
				floorRes.Outcome = "fled"
				return floorRes, "fled", nil
			}
			floorRes.Outcome = "wiped"
			return floorRes, "wiped", nil
		}

		// 威胁反扑：专打当前残血的活跃队员（承伤计入其贡献，经 Mutator 落 HP）。
		if target := pickLowestHPActive(run.members); target != nil {
			salt := fmt.Sprintf("dg_strike:f%d:r%d", floor, round)
			if combatActionRoll(*run.state, mob, *target.rec, salt) >= eliteMissChance {
				dmg := eliteDamage(threat.Attack, target.rec.Status.Defense)
				reasonCode := events.ReasonCombatHit
				if target.rec.Status.HP-dmg <= 0 {
					reasonCode = events.ReasonCombatDown
				}
				res, err := service.applyEliteMutation(ctx, status.Mutation{
					UnitID:     target.rec.ID,
					Turn:       turn,
					Field:      status.FieldHP,
					Delta:      -float64(dmg),
					ReasonCode: reasonCode,
					ReasonText: fmt.Sprintf("副本第%d层的%s重创了她", floor, threat.Name),
					Location:   threat.ID,
				})
				if err != nil {
					return floorRes, "", fmt.Errorf("apply dungeon strike: %w", err)
				}
				*target.rec = res.Record
				target.taken += dmg
				target.contrib.Tank += float64(dmg)
				floorRes.DamageTaken += dmg
				if target.rec.Status.HP <= 0 {
					target.status = "down"
				}
			}
		}
	}

	// 回合耗尽未破：若已有队员撤退，按「撤退保利」口径终止（保留已得 loot、不施败北惩罚），与 anyActive==false 分支同口径；
	// 否则才按全灭走败北惩罚（对抗评审 low：此前一律硬判 wiped 会让本应保利的撤退局误吃惩罚）。
	if service.dungeonAnyFled(run) {
		floorRes.Outcome = "fled"
		return floorRes, "fled", nil
	}
	floorRes.Outcome = "wiped"
	return floorRes, "wiped", nil
}

// dungeonAnyFled 判断是否有队员已撤退（用于区分「全撤」与「全灭」终局）。
func (service *Service) dungeonAnyFled(run *dungeonRun) bool {
	for _, ms := range run.members {
		if ms.status == "fled" {
			return true
		}
	}
	return false
}

// scaledDungeonFloor 生成与队伍战力相称、层深递增难度的本层威胁；末层是更强的 floor boss。
// 确定性：用 FNV(sessionID+turn+floor) 在 [-10%,+10%] 内微调敌强，保证同会话同回合同层可复算。
func scaledDungeonFloor(state *State, members []*memberCombatState, floor int, totalFloors int) Threat {
	totalAtk, maxDef := 0, 1
	for _, ms := range members {
		if ms == nil || ms.rec == nil {
			continue
		}
		totalAtk += maxInt(1, ms.rec.Status.Attack)
		if ms.rec.Status.Defense > maxDef {
			maxDef = ms.rec.Status.Defense
		}
	}
	party := maxInt(1, len(members))
	isBoss := floor == totalFloors

	// 层深难度系数：第 1 层 1.0，每深一层 +15%；末层 boss 再 ×1.6。
	depthScale := 1.0 + 0.15*float64(floor-1)
	hpBase := float64(totalAtk*3) * depthScale
	bossAtkBonus := 0
	if isBoss {
		hpBase *= 1.6
		bossAtkBonus = 3
	}

	// 确定性微调（±10%），用 combat_roll 同源 FNV 口径保证可复现。
	wobble := dungeonFloorWobble(state, floor)
	hp := int(hpBase * (0.9 + 0.2*wobble))
	if hp < 20 {
		hp = 20
	}
	atk := maxDef + 3 + int(float64(floor)*0.8) + bossAtkBonus // 单击有威胁、逼出承伤/撤退抉择
	def := maxInt(2, totalAtk/(party*4+1)+floor/2)

	name := dungeonFloorName(floor, isBoss)
	loot := dungeonFloorLoot(floor, isBoss)

	return Threat{
		// 稳定身份（session+floor）：combat_roll 把 target.ID 写进 FNV，故绝不能用随机 uuid，否则同会话同层每次掷骰不同→破坏确定性。
		ID:       fmt.Sprintf("dgf_%s_f%d", state.ID, floor),
		Name:     name,
		Tier:     ThreatTierDungeon,
		Power:    totalAtk * (4 + floor),
		Attack:   atk,
		Defense:  def,
		HPPool:   hp,
		Severity: 50 + float64(floor)*5,
		Loot:     loot,
	}
}

// dungeonFloorWobble 用 combat_roll 同源 FNV 口径产出 [0,1) 的确定性微调因子（敌强 ±10%）。
func dungeonFloorWobble(state *State, floor int) float64 {
	// 复用 combatActionRoll 的确定性口径：以「副本层」为 salt 的稳定哈希。
	probe := unit.Record{ID: "dungeon_floor"}
	return combatActionRoll(*state, probe, probe, "dg_wobble:f"+strconv.Itoa(floor))
}

var dungeonMobNames = []string{"洞穴蝙蝠群", "苔藓巨蛛", "幽影游魂", "石化守卫", "深渊毒蝎", "古墓傀儡", "暗河鳄", "腐骨食尸鬼", "熔岩蜥蜴"}

func dungeonFloorName(floor int, isBoss bool) string {
	if isBoss {
		return "副本之主·" + dungeonMobNames[(floor-1)%len(dungeonMobNames)]
	}
	return dungeonMobNames[(floor-1)%len(dungeonMobNames)]
}

// dungeonFloorLoot 本层掉落（唯一来源口径）：每层给少量可分割货币（随层深递增），末层 boss 额外掉一件唯一排他遗物
// （走 arbitration 归属）。**确定性、与队伍规模无关**——分赃由 AllocateLoot 按贡献比例瓜分，不需要 party 倍数；
// 这也让结算期能仅凭 (floor,isBoss) 完整复算本层 loot（见 lootSpec），无需回取开打时的队伍规模。
func dungeonFloorLoot(floor int, isBoss bool) []encounter.LootItem {
	gold := 15 + floor*5
	loot := []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: gold}}
	if isBoss {
		loot = append(loot, encounter.LootItem{ID: "dungeon_relic", Rarity: encounter.Epic, Quantity: 1})
	}
	return loot
}

// settleDungeon 在所有层跑完后完成总分赃/败北惩罚、留痕与命运收件箱投递。
// 通关：把每层累计的 loot 汇总，按累计贡献一次性 AllocateLoot 总分赃（排他件走 arbitration）。
// 撤退：保留已通关层的 loot（按已清层汇总分赃），不施败北惩罚。
// 陨落(wiped)：不分赃，各自经分级闸落败北惩罚（绝不触碰 lives，D0-D3 硬锁）。
func (service *Service) settleDungeon(ctx context.Context, run *dungeonRun, result *DungeonResult) error {
	turn := run.state.TurnState.Turn

	participants := make([]encounter.Participant, 0, len(run.members))
	for _, ms := range run.members {
		score := encounter.ContributionScore(ms.contrib)
		participants = append(participants, encounter.Participant{UnitID: ms.rec.ID, Score: score})
		result.Contribution[ms.rec.ID] = score
	}

	awardsByUnit := map[string][]encounter.Award{}
	goldByUnit := map[string]int{}

	// 通关或撤退都按「已清通关层」汇总 loot 分赃（撤退保留已得；陨落不分）。
	if result.Outcome == "cleared" || result.Outcome == "fled" {
		clearedLoot := dungeonClearedLoot(result)
		if len(clearedLoot) > 0 {
			// 分赃 key 用稳定身份（session+turn），不掺 run.id 的随机 UUID——保证同会话同回合同队的排他件归属
			// （arbitration 胜率∝贡献）可复算、可复现，与确定性口径一致。
			key := fmt.Sprintf("%s|dungeon|%d", run.state.ID, turn)
			for _, a := range encounter.AllocateLoot(key, clearedLoot, participants, dungeonMinMeaningful) {
				awardsByUnit[a.UnitID] = append(awardsByUnit[a.UnitID], a)
				result.Awards = append(result.Awards, a)
				if a.ItemID == "gold" {
					goldByUnit[a.UnitID] += a.Quantity
				}
			}
		}
	}

	for _, ms := range run.members {
		// 分赃落库：货币经 Mutator 落 Wallet。
		if gold := goldByUnit[ms.rec.ID]; gold > 0 {
			res, err := service.applyEliteMutation(ctx, status.Mutation{
				UnitID:     ms.rec.ID,
				Turn:       turn,
				Field:      status.FieldWallet,
				Delta:      float64(gold),
				ReasonCode: events.ReasonEconomyLoot,
				ReasonText: "闯副本得来的进项",
				Actors:     []string{ms.rec.ID},
			})
			if err != nil {
				return fmt.Errorf("grant dungeon loot: %w", err)
			}
			*ms.rec = res.Record
		}

		// 陨落：败北惩罚（经分级闸降级）。撤退/通关不施惩罚。
		if result.Outcome == "wiped" {
			layer, err := service.applyDungeonDefeatPenalty(ctx, run.state, ms.rec)
			if err != nil {
				return err
			}
			result.PenaltyLayer[ms.rec.ID] = layer
		}

		card := dungeonMemberCard(result, ms, awardsByUnit[ms.rec.ID])
		result.InboxCards[ms.rec.ID] = card

		appendLog(run.state, "dungeon_encounter", card, ms.rec.ID, run.id)

		// best-effort 命运收件箱：失败不回滚已落地的战斗结算。
		importance, emotion := dungeonMemberWeight(result.Outcome, ms.status)
		if _, err := service.SurfaceFateEvent(ctx, run.state.ID, ms.rec, FateEvent{
			ActorID:       ms.rec.ID,
			TargetID:      ms.rec.ID,
			ReasonCode:    events.ReasonInboxHighlight,
			Importance:    importance,
			EmotionWeight: emotion,
			Summary:       card,
		}); err != nil {
			return err
		}
	}
	return nil
}

const dungeonMinMeaningful = 1.0 // 贡献低于此值的纯蹭场者不进排他件排名（反白嫖），仍可分 common。

// dungeonClearedLoot 汇总「已通关层」的掉落（每层一份本层 loot），用于通关/撤退分赃。
// 注意：威胁是逐层 ad-hoc 生成的，settle 时仅凭 (floor,isBoss) 重建确定性 loot 清单（与开打时 dungeonFloorLoot 同口径），按 ID 合并同类。
func dungeonClearedLoot(result *DungeonResult) []encounter.LootItem {
	merged := map[string]*encounter.LootItem{}
	order := make([]string, 0)
	add := func(item encounter.LootItem) {
		if existing, ok := merged[item.ID]; ok {
			existing.Quantity += item.Quantity
			return
		}
		cp := item
		merged[item.ID] = &cp
		order = append(order, item.ID)
	}
	for _, fr := range result.FloorResults {
		if fr.Outcome != "cleared" {
			continue
		}
		for _, item := range fr.lootSpec() {
			add(item)
		}
	}
	out := make([]encounter.LootItem, 0, len(order))
	for _, id := range order {
		out = append(out, *merged[id])
	}
	return out
}

// lootSpec 按该层结果重建本层掉落清单——直接复用 dungeonFloorLoot 唯一口径（确定性，与开打时完全一致）。
func (fr DungeonFloorResult) lootSpec() []encounter.LootItem {
	return dungeonFloorLoot(fr.Floor, fr.IsBoss)
}

// applyDungeonDefeatPenalty 副本陨落的败北惩罚：候选层=dungeon(=2)，经分级闸按角色牵挂/在世天数降级。
func (service *Service) applyDungeonDefeatPenalty(ctx context.Context, state *State, actor *unit.Record) (int, error) {
	candidate := candidateDefeatLayer(ThreatTierDungeon)
	care := service.ComputeAttachment(ctx, actor.ID, actor.Status.Loyalty, state.TurnState.Turn)
	layer := encounter.DegradePenalty(candidate, care, state.TurnState.Turn)

	res, err := service.applyEliteMutation(ctx, status.Mutation{
		UnitID:     actor.ID,
		Turn:       state.TurnState.Turn,
		Field:      status.FieldMorale,
		Delta:      -defeatMoraleHitForLayer(layer), // 代价随分级层放大，绝不触碰 lives（D0-D3 保护）
		ReasonCode: events.ReasonEmotionTrauma,
		ReasonText: "副本里折戟，心气受了挫",
		Actors:     []string{actor.ID},
	})
	if err != nil {
		return layer, fmt.Errorf("apply dungeon defeat penalty: %w", err)
	}
	*actor = res.Record
	return layer, nil
}

// dungeonMemberWeight 把副本结局映射为命运重要度与情绪强度（驱动收件箱三档路由）。
func dungeonMemberWeight(outcome string, memberStatus string) (int, float64) {
	switch {
	case memberStatus == "down":
		return 8, -0.7 // 在副本里倒下，强相关
	case outcome == "cleared":
		return 6, 0.5 // 通关高光
	case outcome == "fled":
		return 6, -0.3
	default: // wiped 但本人没倒
		return 7, -0.6
	}
}

// dungeonMemberCard 渲染某队员的祖魂语气命运卡。
func dungeonMemberCard(result *DungeonResult, ms *memberCombatState, awards []encounter.Award) string {
	gotRelic := false
	gold := 0
	for _, a := range awards {
		if a.ItemID == "dungeon_relic" && a.Reason == encounter.AwardWon {
			gotRelic = true
		}
		if a.ItemID == "gold" {
			gold += a.Quantity
		}
	}

	var b strings.Builder
	switch result.Outcome {
	case "cleared":
		if ms.status == "down" {
			b.WriteString(fmt.Sprintf("她在副本里倒下过，是同伴把这趟%d层走到了底、也把她拖了回来。", result.FloorsClear))
		} else {
			b.WriteString(fmt.Sprintf("她和同伴把这座副本一路打到了底，整整%d层，活着出来了。", result.FloorsClear))
		}
	case "fled":
		b.WriteString(fmt.Sprintf("她们闯到第%d层见势不妙，及时退了出来——已经到手的不会再丢。", result.FloorsClear))
	default: // wiped
		b.WriteString(fmt.Sprintf("她们没能闯过副本第%d层，伤痕累累退了下来——但人都还在，命还攥在自己手里。", result.FloorsClear+1))
	}
	if gotRelic {
		b.WriteString("那件众人觊觎的副本遗物，最后落在了她手里。")
	}
	if gold > 0 {
		b.WriteString(fmt.Sprintf("腰间多了 %d 枚铜钱。", gold))
	}
	return b.String()
}
