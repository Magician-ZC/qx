package session

// 文件说明：威胁事件(ThreatEvent)容器与单人 elite 遭遇结算，把 engine/encounter 原语接入 session 层。
// 链路：撞见精英怪 → 反射护栏先保命 → combat_roll 确定性多回合消耗战 → 胜利走 encounter.AllocateLoot 分赃 /
// 失败走 encounter.DegradePenalty 后果分级闸 → 全程经 status.Mutator 留痕 → 产出祖魂语气的命运收件箱卡。
// 设计见 docs/PvE威胁系统.md。单人 elite 不依赖 World Bus，可在现有 session 内跑通。

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

// ThreatTier 威胁档位（四档同一模型的参数化）。
type ThreatTier string

const (
	ThreatTierElite     ThreatTier = "elite"
	ThreatTierFieldBoss ThreatTier = "field_boss"
	ThreatTierDungeon   ThreatTier = "dungeon"
	ThreatTierWorldBoss ThreatTier = "world_boss"
)

// Threat 是一个威胁事件的轻量容器（单人 elite 先用内存态，不落新表）。
type Threat struct {
	ID       string
	Name     string
	Tier     ThreatTier
	RegionID string
	Power    int
	Attack   int
	Defense  int
	HPPool   int
	Severity float64
	Loot     []encounter.LootItem // 胜利掉落（elite 通常是少量货币/材料）
}

// EliteEncounterResult 单人 elite 遭遇结果。
type EliteEncounterResult struct {
	ThreatID     string
	Outcome      string // defeated / fled / down
	Rounds       int
	DamageDealt  int
	DamageTaken  int
	Contribution float64
	Awards       []encounter.Award
	PenaltyLayer int    // 失败时实际落地的后果层（经分级闸降级），0=未触发
	InboxCard    string // 祖魂语气的命运收件箱卡
}

const (
	eliteMaxRounds  = 30
	eliteFleeHP     = 25   // HP 低于此值反射撤退（HP 上限 100，≈0.25）
	eliteMissChance = 0.08 // 确定性掷骰命中判定
)

// TriggerEliteEncounter 为某角色生成一头与其战力相称的精英怪并跑通完整遭遇（供 API 触发）。
// 这是真实动作：会真的改动该角色的 HP/士气/钱包并落进命运收件箱。
func (service *Service) TriggerEliteEncounter(ctx context.Context, sessionID string, unitID string) (EliteEncounterResult, error) {
	if service == nil || service.units == nil {
		return EliteEncounterResult{}, fmt.Errorf("trigger elite encounter: missing dependencies")
	}
	actor, err := service.units.GetByID(ctx, unitID)
	if err != nil {
		return EliteEncounterResult{}, err
	}
	state := State{ID: sessionID}
	return service.ResolveEliteEncounter(ctx, &state, &actor, scaledElite(actor))
}

// scaledElite 生成一头与角色战力相称、通常可一战的精英怪。
func scaledElite(actor unit.Record) Threat {
	atk := actor.Status.Attack
	if atk < 4 {
		atk = 4
	}
	def := atk / 3
	if def < 1 {
		def = 1
	}
	hp := atk * 3
	if hp < 20 {
		hp = 20
	}
	eliteAtk := actor.Status.Defense
	if eliteAtk < 3 {
		eliteAtk = 3
	}
	return Threat{
		ID:       "elite_" + uuid.NewString(),
		Name:     "山魈",
		Tier:     ThreatTierElite,
		RegionID: "",
		Power:    atk * 4,
		Attack:   eliteAtk,
		Defense:  def,
		HPPool:   hp,
		Severity: 40,
		Loot:     []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: 15}},
	}
}

// candidateDefeatLayer 各档威胁失败时的候选后果层（再经分级闸按角色降级）。
func candidateDefeatLayer(tier ThreatTier) int {
	switch tier {
	case ThreatTierWorldBoss:
		return 3
	case ThreatTierFieldBoss, ThreatTierDungeon:
		return 2
	default: // elite
		return 1
	}
}

func eliteDamage(attack int, defense int) int {
	d := attack - defense
	if d < 1 {
		d = 1
	}
	return d
}

// ResolveEliteEncounter 跑通一次单人 elite 遭遇的完整确定性链路。
// 注意：威胁为内存态，结算用 combat_roll 确定性掷骰；角色 HP/士气/钱包变更全部经 status.Mutator 留痕。
func (service *Service) ResolveEliteEncounter(ctx context.Context, state *State, actor *unit.Record, threat Threat) (EliteEncounterResult, error) {
	result := EliteEncounterResult{ThreatID: threat.ID, Outcome: "down"}
	if service == nil || service.mutator == nil || state == nil || actor == nil {
		return result, fmt.Errorf("resolve elite encounter: missing dependencies")
	}

	// 合成威胁的伪单位记录，供 combat_roll 取确定性随机。
	elite := unit.Record{ID: "threat:" + threat.ID}
	elite.Status.Attack = threat.Attack
	elite.Status.Defense = threat.Defense
	eliteHP := threat.HPPool
	turn := state.TurnState.Turn

	var contribution encounter.Contribution
	for round := 1; round <= eliteMaxRounds; round++ {
		result.Rounds = round

		// 反射护栏（L1）：HP 危急先保命，零 LLM。
		if actor.Status.HP < eliteFleeHP {
			result.Outcome = "fled"
			break
		}

		// 角色攻击威胁（确定性命中 + 伤害）。
		if combatActionRoll(*state, *actor, elite, "elite_atk:r"+strconv.Itoa(round)) >= eliteMissChance {
			dmg := eliteDamage(actor.Status.Attack, threat.Defense)
			eliteHP -= dmg
			result.DamageDealt += dmg
			contribution.Damage += float64(dmg)
		}
		if eliteHP <= 0 {
			result.Outcome = "defeated"
			break
		}

		// 威胁反扑（确定性命中 + 伤害，经 status.Mutator 落到角色 HP 并留痕）。
		if combatActionRoll(*state, elite, *actor, "elite_strike:r"+strconv.Itoa(round)) >= eliteMissChance {
			dmg := eliteDamage(threat.Attack, actor.Status.Defense)
			reasonCode := events.ReasonCombatHit
			if actor.Status.HP-dmg <= 0 {
				reasonCode = events.ReasonCombatDown
			}
			// 注意：威胁是合成伪单位、非 units 行，不能进 Actors（events.actor_unit_id 有 FK）；
			// 威胁身份记在 ReasonText/Location，事件 actor 落回角色自身。
			res, err := service.mutator.Apply(ctx, status.Mutation{
				UnitID:     actor.ID,
				Turn:       turn,
				Field:      status.FieldHP,
				Delta:      -float64(dmg),
				ReasonCode: reasonCode,
				ReasonText: fmt.Sprintf("%s 在搏斗中伤到了她", threat.Name),
				Location:   threat.ID,
			})
			if err != nil {
				return result, fmt.Errorf("apply elite strike: %w", err)
			}
			*actor = res.Record
			result.DamageTaken += dmg
			contribution.Tank += float64(dmg)
		}
		if actor.Status.HP <= 0 {
			result.Outcome = "down"
			break
		}
	}

	result.Contribution = encounter.ContributionScore(contribution)

	if result.Outcome == "defeated" {
		if err := service.grantEliteLoot(ctx, state, actor, threat, result.Contribution, &result); err != nil {
			return result, err
		}
		result.InboxCard = fmt.Sprintf("她砍翻了那头%s，带着一身伤回来了。%s", threat.Name, lootBlurb(result.Awards))
	} else {
		layer, err := service.applyDefeatPenalty(ctx, state, actor, threat)
		if err != nil {
			return result, err
		}
		result.PenaltyLayer = layer
		verb := "没能拦住"
		if result.Outcome == "fled" {
			verb = "终究避开了"
		}
		result.InboxCard = fmt.Sprintf("她%s那头%s，伤得不轻，心气也低了一截——但她还在，命还攥在自己手里。", verb, threat.Name)
	}

	appendLog(state, "threat_encounter", result.InboxCard, actor.ID, elite.ID)

	// 把遭遇结果按相关性路由进命运收件箱（她自己的事，相关性由重要度/情绪决定）。
	importance, emotion := eliteOutcomeWeight(result.Outcome)
	if _, err := service.SurfaceFateEvent(ctx, state.ID, actor, FateEvent{
		ActorID:       actor.ID,
		TargetID:      actor.ID,
		ReasonCode:    events.ReasonInboxHighlight,
		Importance:    importance,
		EmotionWeight: emotion,
		Summary:       result.InboxCard,
	}); err != nil {
		return result, err
	}
	return result, nil
}

// ---- 野外Boss / 组队遭遇 ----

const fieldBossMinMeaningful = 1.0 // 贡献低于此值的纯蹭场者不进排他件排名（反白嫖），仍可分 common。

// FieldBossMemberOutcome 某队员在野外Boss遭遇中的结算。
type FieldBossMemberOutcome struct {
	UnitID       string
	Outcome      string // contributed / fled / down
	DamageDealt  int
	DamageTaken  int
	Contribution float64
	Awards       []encounter.Award
	PenaltyLayer int    // 失败时经分级闸落地的后果层，0=未触发
	InboxCard    string // 祖魂语气命运卡
}

// FieldBossResult 野外Boss/组队遭遇的整体结算。
type FieldBossResult struct {
	ThreatID string
	Victory  bool
	Rounds   int
	Members  []FieldBossMemberOutcome
}

// memberCombatState 是单个队员在战斗循环内的可变态（内部用）。
type memberCombatState struct {
	rec     *unit.Record
	contrib encounter.Contribution
	dealt   int
	taken   int
	status  string // contributed / fled / down
}

// TriggerFieldBoss 为一支队伍生成一头与其总战力相称的野外Boss并跑通组队遭遇（供 API 触发）。真实动作。
func (service *Service) TriggerFieldBoss(ctx context.Context, sessionID string, unitIDs []string) (FieldBossResult, error) {
	if service == nil || service.units == nil {
		return FieldBossResult{}, fmt.Errorf("trigger field boss: missing dependencies")
	}
	if len(unitIDs) == 0 {
		return FieldBossResult{}, fmt.Errorf("trigger field boss: empty party")
	}
	party := make([]*unit.Record, 0, len(unitIDs))
	for _, id := range unitIDs {
		rec, err := service.units.GetByID(ctx, id)
		if err != nil {
			return FieldBossResult{}, fmt.Errorf("load party member %s: %w", id, err)
		}
		member := rec
		party = append(party, &member)
	}
	state := State{ID: sessionID}
	return service.ResolveFieldBoss(ctx, &state, party, scaledFieldBoss(party))
}

// scaledFieldBoss 生成一头与队伍总战力相称、协力可破的野外Boss。
func scaledFieldBoss(party []*unit.Record) Threat {
	totalAtk, maxDef := 0, 1
	for _, p := range party {
		if p == nil {
			continue
		}
		totalAtk += maxInt(1, p.Status.Attack)
		if p.Status.Defense > maxDef {
			maxDef = p.Status.Defense
		}
	}
	hp := totalAtk * 3 // 全员命中约 3 回合可破
	if hp < 60 {
		hp = 60
	}
	bossAtk := maxDef + 4 // 单击有威胁但不致命，逼出承伤与撤退抉择
	return Threat{
		ID:       "fieldboss_" + uuid.NewString(),
		Name:     "蛮荒巨蜥",
		Tier:     ThreatTierFieldBoss,
		Power:    totalAtk * 5,
		Attack:   bossAtk,
		Defense:  maxInt(2, totalAtk/(len(party)*4+1)),
		HPPool:   hp,
		Severity: 65,
		Loot: []encounter.LootItem{
			{ID: "gold", Rarity: encounter.Common, Quantity: 30 * len(party)},
			{ID: "boss_relic", Rarity: encounter.Epic, Quantity: 1}, // 唯一排他件，走 arbitration 仲裁归属
		},
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// pickLowestHPActive 确定性选出当前血量最低的活跃队员（平局按 UnitID 升序），作为 Boss 的攻击目标。
func pickLowestHPActive(members []*memberCombatState) *memberCombatState {
	var target *memberCombatState
	for _, ms := range members {
		if ms.status != "contributed" {
			continue
		}
		if target == nil || ms.rec.Status.HP < target.rec.Status.HP ||
			(ms.rec.Status.HP == target.rec.Status.HP && ms.rec.ID < target.rec.ID) {
			target = ms
		}
	}
	return target
}

// fieldBossMemberWeight 把队员结局映射为命运重要度与情绪强度（驱动收件箱三档路由）。
func fieldBossMemberWeight(victory bool, memberOutcome string) (int, float64) {
	switch {
	case memberOutcome == "down":
		return 8, -0.7 // 被打倒，强相关
	case victory:
		return 5, 0.4 // 协力得胜
	case memberOutcome == "fled":
		return 6, -0.4
	default:
		return 6, -0.5 // 战败仍在场
	}
}

// ResolveFieldBoss 跑通一次组队野外Boss遭遇的完整确定性链路：
// 多回合 combat_roll 消耗战（队员轮流攻击、Boss 专打残血者、危急者反射撤退）→ 胜利按贡献分赃(含 epic 仲裁)
// /失败各自经分级闸降级 → 全程经 status.Mutator 留痕 → 每人一张祖魂语气命运卡入收件箱。
func (service *Service) ResolveFieldBoss(ctx context.Context, state *State, party []*unit.Record, threat Threat) (FieldBossResult, error) {
	result := FieldBossResult{ThreatID: threat.ID}
	if service == nil || service.mutator == nil || state == nil {
		return result, fmt.Errorf("resolve field boss: missing dependencies")
	}

	members := make([]*memberCombatState, 0, len(party))
	for _, p := range party {
		if p != nil {
			members = append(members, &memberCombatState{rec: p, status: "contributed"})
		}
	}
	if len(members) == 0 {
		return result, fmt.Errorf("resolve field boss: empty party")
	}

	boss := unit.Record{ID: "threat:" + threat.ID}
	boss.Status.Attack = threat.Attack
	boss.Status.Defense = threat.Defense
	bossHP := threat.HPPool
	turn := state.TurnState.Turn

	for round := 1; round <= eliteMaxRounds; round++ {
		result.Rounds = round

		anyActive := false
		for _, ms := range members {
			if ms.status != "contributed" {
				continue
			}
			if ms.rec.Status.HP < eliteFleeHP { // 反射护栏：危急先撤，零 LLM
				ms.status = "fled"
				continue
			}
			anyActive = true
			if combatActionRoll(*state, *ms.rec, boss, fmt.Sprintf("fb_atk:%s:r%d", ms.rec.ID, round)) >= eliteMissChance {
				dmg := eliteDamage(ms.rec.Status.Attack, threat.Defense)
				bossHP -= dmg
				ms.dealt += dmg
				ms.contrib.Damage += float64(dmg)
			}
		}
		if bossHP <= 0 {
			result.Victory = true
			break
		}
		if !anyActive {
			break // 全员撤退/倒下
		}

		// Boss 反扑：专打当前残血的活跃队员（承伤计入其贡献）。
		if target := pickLowestHPActive(members); target != nil {
			if combatActionRoll(*state, boss, *target.rec, fmt.Sprintf("fb_strike:r%d", round)) >= eliteMissChance {
				dmg := eliteDamage(threat.Attack, target.rec.Status.Defense)
				reasonCode := events.ReasonCombatHit
				if target.rec.Status.HP-dmg <= 0 {
					reasonCode = events.ReasonCombatDown
				}
				res, err := service.mutator.Apply(ctx, status.Mutation{
					UnitID:     target.rec.ID,
					Turn:       turn,
					Field:      status.FieldHP,
					Delta:      -float64(dmg),
					ReasonCode: reasonCode,
					ReasonText: fmt.Sprintf("%s 一击砸在她身上", threat.Name),
					Location:   threat.ID,
				})
				if err != nil {
					return result, fmt.Errorf("apply field boss strike: %w", err)
				}
				*target.rec = res.Record
				target.taken += dmg
				target.contrib.Tank += float64(dmg)
				if target.rec.Status.HP <= 0 {
					target.status = "down"
				}
			}
		}
	}

	if err := service.settleFieldBoss(ctx, state, threat, members, &result); err != nil {
		return result, err
	}
	return result, nil
}

// settleFieldBoss 在战斗循环后完成分赃/惩罚、留痕与命运收件箱投递。
func (service *Service) settleFieldBoss(ctx context.Context, state *State, threat Threat, members []*memberCombatState, result *FieldBossResult) error {
	turn := state.TurnState.Turn
	participants := make([]encounter.Participant, 0, len(members))
	for _, ms := range members {
		participants = append(participants, encounter.Participant{UnitID: ms.rec.ID, Score: encounter.ContributionScore(ms.contrib)})
	}

	awardsByUnit := map[string][]encounter.Award{}
	goldByUnit := map[string]int{}
	if result.Victory {
		key := fmt.Sprintf("%s|fieldboss|%s|%d", state.ID, threat.ID, turn)
		for _, a := range encounter.AllocateLoot(key, threat.Loot, participants, fieldBossMinMeaningful) {
			awardsByUnit[a.UnitID] = append(awardsByUnit[a.UnitID], a)
			if a.ItemID == "gold" {
				goldByUnit[a.UnitID] += a.Quantity
			}
		}
	}

	for _, ms := range members {
		outcome := FieldBossMemberOutcome{
			UnitID:       ms.rec.ID,
			Outcome:      ms.status,
			DamageDealt:  ms.dealt,
			DamageTaken:  ms.taken,
			Contribution: encounter.ContributionScore(ms.contrib),
			Awards:       awardsByUnit[ms.rec.ID],
		}

		if result.Victory {
			if gold := goldByUnit[ms.rec.ID]; gold > 0 {
				res, err := service.mutator.Apply(ctx, status.Mutation{
					UnitID:     ms.rec.ID,
					Turn:       turn,
					Field:      status.FieldWallet,
					Delta:      float64(gold),
					ReasonCode: events.ReasonEconomyLoot,
					ReasonText: fmt.Sprintf("讨平%s后的分成", threat.Name),
					Actors:     []string{ms.rec.ID},
				})
				if err != nil {
					return fmt.Errorf("grant field boss loot: %w", err)
				}
				*ms.rec = res.Record
			}
			outcome.InboxCard = fieldBossVictoryCard(threat, ms.status, outcome.Awards)
		} else {
			layer, err := service.applyDefeatPenalty(ctx, state, ms.rec, threat)
			if err != nil {
				return err
			}
			outcome.PenaltyLayer = layer
			outcome.InboxCard = fmt.Sprintf("她和同伴没能拿下那头%s，伤痕累累退了下来——但人都还在，命还攥在自己手里。", threat.Name)
		}

		appendLog(state, "field_boss_encounter", outcome.InboxCard, ms.rec.ID, threat.ID)
		importance, emotion := fieldBossMemberWeight(result.Victory, ms.status)
		if _, err := service.SurfaceFateEvent(ctx, state.ID, ms.rec, FateEvent{
			ActorID:       ms.rec.ID,
			TargetID:      ms.rec.ID,
			ReasonCode:    events.ReasonInboxHighlight,
			Importance:    importance,
			EmotionWeight: emotion,
			Summary:       outcome.InboxCard,
		}); err != nil {
			return err
		}
		result.Members = append(result.Members, outcome)
	}
	return nil
}

// fieldBossVictoryCard 渲染胜利方某队员的祖魂语气命运卡（含其分得的战利品）。
func fieldBossVictoryCard(threat Threat, memberOutcome string, awards []encounter.Award) string {
	gotRelic := false
	for _, a := range awards {
		if a.ItemID == "boss_relic" && a.Reason == encounter.AwardWon {
			gotRelic = true
		}
	}
	prefix := fmt.Sprintf("她和同伴合力讨平了那头%s。", threat.Name)
	if memberOutcome == "down" {
		prefix = fmt.Sprintf("她在讨伐%s时倒下了，是同伴把这一仗打完、也把她拖了回来。", threat.Name)
	}
	if gotRelic {
		return prefix + "那件众人觊觎的遗物，最后落在了她手里。" + lootBlurb(awards)
	}
	return prefix + lootBlurb(awards)
}

// eliteOutcomeWeight 把遭遇结局映射为命运重要度与情绪强度（驱动收件箱三档路由）。
func eliteOutcomeWeight(outcome string) (int, float64) {
	switch outcome {
	case "down":
		return 8, -0.7 // 濒死，强相关，应升级待决策
	case "fled":
		return 6, -0.4
	default: // defeated
		return 5, 0.4
	}
}

// grantEliteLoot 按贡献分配掉落（单人=独享），货币类经 Mutator 落 Wallet 并留痕；排他件记入结果（入库为后续）。
func (service *Service) grantEliteLoot(ctx context.Context, state *State, actor *unit.Record, threat Threat, score float64, result *EliteEncounterResult) error {
	if len(threat.Loot) == 0 {
		return nil
	}
	key := fmt.Sprintf("%s|elite|%s|%d", state.ID, threat.ID, state.TurnState.Turn)
	awards := encounter.AllocateLoot(key, threat.Loot, []encounter.Participant{{UnitID: actor.ID, Score: score}}, 0)
	result.Awards = awards

	gold := 0
	for _, a := range awards {
		if a.UnitID != actor.ID {
			continue
		}
		if a.ItemID == "gold" {
			gold += a.Quantity
		}
	}
	if gold > 0 {
		res, err := service.mutator.Apply(ctx, status.Mutation{
			UnitID:     actor.ID,
			Turn:       state.TurnState.Turn,
			Field:      status.FieldWallet,
			Delta:      float64(gold),
			ReasonCode: events.ReasonEconomyLoot,
			ReasonText: fmt.Sprintf("从%s身上得来的进项", threat.Name),
			Actors:     []string{actor.ID},
		})
		if err != nil {
			return fmt.Errorf("grant elite loot: %w", err)
		}
		*actor = res.Record
	}
	return nil
}

// applyDefeatPenalty 失败惩罚经后果分级闸降级（elite 候选层=1，恒落「可恢复」：士气重挫），经 Mutator 留痕。
func (service *Service) applyDefeatPenalty(ctx context.Context, state *State, actor *unit.Record, threat Threat) (int, error) {
	candidate := candidateDefeatLayer(threat.Tier)
	// 牵挂/在世天数尚未在 session 落地，单人 elite 用保守值——分级闸对新角色硬锁，最坏只到候选层。
	layer := encounter.DegradePenalty(candidate, 0, 0)

	res, err := service.mutator.Apply(ctx, status.Mutation{
		UnitID:     actor.ID,
		Turn:       state.TurnState.Turn,
		Field:      status.FieldMorale,
		Delta:      -0.15,
		ReasonCode: events.ReasonEmotionTrauma,
		ReasonText: fmt.Sprintf("没能赢下那头%s，心气受了挫", threat.Name),
		Actors:     []string{actor.ID},
	})
	if err != nil {
		return layer, fmt.Errorf("apply defeat penalty: %w", err)
	}
	*actor = res.Record
	return layer, nil
}

func lootBlurb(awards []encounter.Award) string {
	gold := 0
	for _, a := range awards {
		if a.ItemID == "gold" {
			gold += a.Quantity
		}
	}
	if gold > 0 {
		return fmt.Sprintf("腰间多了 %d 枚铜钱。", gold)
	}
	return ""
}
