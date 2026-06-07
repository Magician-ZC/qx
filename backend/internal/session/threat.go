package session

// 文件说明：威胁事件(ThreatEvent)容器与单人 elite 遭遇结算，把 engine/encounter 原语接入 session 层。
// 链路：撞见精英怪 → 反射护栏先保命 → combat_roll 确定性多回合消耗战 → 胜利走 encounter.AllocateLoot 分赃 /
// 失败走 encounter.DegradePenalty 后果分级闸 → 全程经 status.Mutator 留痕 → 产出祖魂语气的命运收件箱卡。
// 设计见 docs/PvE威胁系统.md。单人 elite 不依赖 World Bus，可在现有 session 内跑通。

import (
	"context"
	"fmt"
	"strconv"

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
	return result, nil
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
