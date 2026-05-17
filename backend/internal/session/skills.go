package session

// 文件说明：定义战斗技能目录并执行技能结算，联动 AP、目标合法性、状态修改与资源消耗。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// combatSkillDefinition 结构体用于承载该模块的核心数据。
type combatSkillDefinition struct {
	ID             string
	DisplayName    string
	APCost         int
	RequiresTarget bool
	TargetMode     string
}

var combatSkillCatalog = []combatSkillDefinition{
	{
		ID:          "battle_cry",
		DisplayName: "战吼",
		APCost:      1,
	},
	{
		ID:             "suppressive_strike",
		DisplayName:    "压制打击",
		APCost:         2,
		RequiresTarget: true,
		TargetMode:     "enemy",
	},
	{
		ID:             "fire_assault",
		DisplayName:    "火攻突袭",
		APCost:         2,
		RequiresTarget: true,
		TargetMode:     "enemy",
	},
	{
		ID:             "field_treatment",
		DisplayName:    "战地包扎",
		APCost:         2,
		RequiresTarget: true,
		TargetMode:     "ally_or_self",
	},
	{
		ID:          "last_stand",
		DisplayName: "背水一战",
		APCost:      2,
	},
}

// combatSkillByID 按技能 ID 查询技能定义。
func combatSkillByID(skillID string) (combatSkillDefinition, bool) {
	for _, definition := range combatSkillCatalog {
		if definition.ID == skillID {
			return definition, true
		}
	}
	return combatSkillDefinition{}, false
}

// combatSkillCost 返回技能 AP 消耗；未知技能回退到 1。
func combatSkillCost(skillID string) int {
	definition, ok := combatSkillByID(skillID)
	if !ok {
		return 1
	}
	if definition.APCost < 0 {
		return 0
	}
	return definition.APCost
}

// buildSkillCandidates 生成单位当前可施放的技能候选集。
// 候选会综合 AP、距离、目标合法性、物资与局部战况条件。
func buildSkillCandidates(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	remainingAP int,
) []decisionCandidate {
	if actor == nil || remainingAP <= 0 {
		return nil
	}

	candidates := make([]decisionCandidate, 0, 8)

	battleCry, _ := combatSkillByID("battle_cry")
	if battleCry.APCost <= remainingAP && shouldOfferBattleCry(state, byID, actor) {
		candidates = append(candidates, decisionCandidate{
			ID:      "skill:battle_cry",
			Action:  DecisionActionSkill,
			SkillID: battleCry.ID,
			APCost:  battleCry.APCost,
			Summary: "战吼鼓舞附近友军并稳住军心。",
		})
	}

	suppressiveStrike, _ := combatSkillByID("suppressive_strike")
	if suppressiveStrike.APCost <= remainingAP {
		reach := attackReachWithWeather(state, *actor)
		for _, targetID := range targetIDs {
			target := byID[targetID]
			if target == nil || !isBattleReady(*target) {
				continue
			}
			distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
			if distance > reach+1 {
				continue
			}
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("skill:suppressive_strike:%s", target.ID),
				Action:       DecisionActionSkill,
				SkillID:      suppressiveStrike.ID,
				APCost:       suppressiveStrike.APCost,
				TargetUnitID: target.ID,
				Summary:      fmt.Sprintf("对 %s 施放压制打击，压低其士气。", target.DisplayName()),
			})
		}
	}

	fireAssault, _ := combatSkillByID("fire_assault")
	if fireAssault.APCost <= remainingAP {
		reach := attackReachWithWeather(state, *actor)
		for _, targetID := range targetIDs {
			target := byID[targetID]
			if target == nil || !isBattleReady(*target) {
				continue
			}
			distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
			if distance > reach+1 {
				continue
			}

			summary := fmt.Sprintf("对 %s 施放火攻突袭，附加持续灼烧。", target.DisplayName())
			terrain := terrainAt(state.Map, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR})
			if state.Weather.Type == WeatherWindy && terrain == world.TerrainGrassland {
				summary = fmt.Sprintf("对 %s 施放火攻突袭（风助火势，草原伤害提升）。", target.DisplayName())
			}

			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("skill:fire_assault:%s", target.ID),
				Action:       DecisionActionSkill,
				SkillID:      fireAssault.ID,
				APCost:       fireAssault.APCost,
				TargetUnitID: target.ID,
				Summary:      summary,
			})
		}
	}

	fieldTreatment, _ := combatSkillByID("field_treatment")
	if fieldTreatment.APCost <= remainingAP && hasBackpackQuantity(*actor, "herb_bundle", 1) {
		if actor.Status.HP < 95 {
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("skill:field_treatment:%s", actor.ID),
				Action:       DecisionActionSkill,
				SkillID:      fieldTreatment.ID,
				APCost:       fieldTreatment.APCost,
				TargetUnitID: actor.ID,
				Summary:      "给自己施放战地包扎（消耗药草包，恢复生命）。",
			})
		}
		for _, allyID := range alliedIDs(state, actor.FactionID) {
			if allyID == actor.ID {
				continue
			}
			ally := byID[allyID]
			if ally == nil || !isBattleReady(*ally) {
				continue
			}
			if ally.Status.HP >= 95 {
				continue
			}
			if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) > 1 {
				continue
			}
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("skill:field_treatment:%s", ally.ID),
				Action:       DecisionActionSkill,
				SkillID:      fieldTreatment.ID,
				APCost:       fieldTreatment.APCost,
				TargetUnitID: ally.ID,
				Summary:      fmt.Sprintf("对 %s 施放战地包扎（消耗药草包，恢复生命）。", ally.DisplayName()),
			})
		}
	}

	lastStand, _ := combatSkillByID("last_stand")
	if lastStand.APCost <= remainingAP {
		if actor.Status.HP <= 45 || hasLocalEnemyAdvantage(state, byID, actor) {
			candidates = append(candidates, decisionCandidate{
				ID:      "skill:last_stand",
				Action:  DecisionActionSkill,
				SkillID: lastStand.ID,
				APCost:  lastStand.APCost,
				Summary: "背水一战，短时间内攻防都进入极端状态。",
			})
		}
	}

	return candidates
}

// shouldOfferBattleCry 控制战吼候选出现时机，避免远敌开局把经济/施工机会全部挤掉。
func shouldOfferBattleCry(state State, byID map[string]*unit.Record, actor *unit.Record) bool {
	if actor == nil {
		return false
	}

	adjacentAllies := 0
	localNeed := actor.Status.Morale < 0.55 || actor.Status.HP < 85
	for _, allyID := range alliedIDs(state, actor.FactionID) {
		if allyID == actor.ID {
			continue
		}
		ally := byID[allyID]
		if ally == nil || !isBattleReady(*ally) {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) > 1 {
			continue
		}
		adjacentAllies++
		if ally.Status.HP < 90 || ally.Status.Morale < 0.60 {
			localNeed = true
		}
	}
	if adjacentAllies == 0 {
		return false
	}
	if nearestThreatDistance(state, byID, actor) <= 6 {
		return true
	}
	return localNeed
}

// executeSkill 执行技能决策并结算状态、日志、关系变化与道具消耗。
// 对无效目标或条件不足场景会优雅降级为 hold 日志，不直接报错中断回合。
func (service *Service) executeSkill(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	modifiers actionModifiers,
) error {
	_ = modifiers
	logText := decisionLogText(decision)
	definition, ok := combatSkillByID(decision.SkillID)
	if !ok {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				"我想用这个技能，但没识别出来，先待命。",
			)),
			actor.ID,
			"",
		)
		return nil
	}

	switch definition.ID {
	case "battle_cry":
		if err := service.applyActionHungerCost(ctx, state, actor, definition.DisplayName); err != nil {
			return err
		}
		if err := service.applyStatusMutation(
			ctx,
			state,
			actor,
			status.FieldMorale,
			0.08,
			events.ReasonEmotionReward,
			"我发出战吼，士气提升。",
		); err != nil {
			return err
		}
		for _, alliedID := range alliedIDs(*state, actor.FactionID) {
			if alliedID == actor.ID {
				continue
			}
			ally := byID[alliedID]
			if ally == nil || !isBattleReady(*ally) {
				continue
			}
			if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) > 1 {
				continue
			}
			if err := service.applyStatusMutation(
				ctx,
				state,
				ally,
				status.FieldMorale,
				0.05,
				events.ReasonEmotionReward,
				fmt.Sprintf("我受到 %s 战吼鼓舞。", actor.DisplayName()),
			); err != nil {
				return err
			}
		}
		appendLog(
			state,
			"skill",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				fmt.Sprintf("我施放技能【%s】鼓舞周围队友。", definition.DisplayName),
			)),
			actor.ID,
			"",
		)
		return nil

	case "suppressive_strike":
		target := resolveTarget(opposingIDs(*state, actor.FactionID), byID, decision.TargetUnitID, actor)
		if target == nil {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但没找到有效目标。", definition.DisplayName),
				)),
				actor.ID,
				decision.TargetUnitID,
			)
			return nil
		}
		reach := attackReachWithWeather(*state, *actor)
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > reach+1 {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但目标太远。", definition.DisplayName),
				)),
				actor.ID,
				target.ID,
			)
			return nil
		}
		if err := service.applyActionHungerCost(ctx, state, actor, definition.DisplayName); err != nil {
			return err
		}
		if err := service.applyStatusMutation(
			ctx,
			state,
			target,
			status.FieldMorale,
			-0.10,
			events.ReasonEmotionTrauma,
			fmt.Sprintf("我被 %s 的压制打击逼退心态。", actor.DisplayName()),
		); err != nil {
			return err
		}
		appendLog(
			state,
			"skill",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				fmt.Sprintf("我对 %s 施放技能【%s】，压制其士气。", target.DisplayName(), definition.DisplayName),
			)),
			actor.ID,
			target.ID,
		)
		actorDelta := relationDelta{
			Trust:     -0.16,
			Fear:      -0.02,
			Affection: -0.12,
			Rivalry:   0.66,
		}
		targetDelta := relationDelta{
			Trust:     -0.42,
			Fear:      0.38,
			Affection: -0.20,
			Rivalry:   0.92,
		}
		service.applyMutualRelationShiftBestEffort(ctx, state, actor, target, actorDelta, targetDelta, "压制打击")
		return nil

	case "field_treatment":
		if !hasBackpackQuantity(*actor, "herb_bundle", 1) {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但没有药草包。", definition.DisplayName),
				)),
				actor.ID,
				"",
			)
			return nil
		}
		target, ok := byID[decision.TargetUnitID]
		if !ok || !isBattleReady(*target) || target.FactionID != actor.FactionID {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但治疗目标无效。", definition.DisplayName),
				)),
				actor.ID,
				decision.TargetUnitID,
			)
			return nil
		}
		if target.ID != actor.ID && unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > 1 {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但离队友太远。", definition.DisplayName),
				)),
				actor.ID,
				target.ID,
			)
			return nil
		}
		if err := unit.ConsumeBackpackItem(actor, "herb_bundle", 1); err != nil {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但消耗药草包失败。", definition.DisplayName),
				)),
				actor.ID,
				target.ID,
			)
			return nil
		}
		if err := service.units.Save(ctx, *actor); err != nil {
			return err
		}
		if err := service.applyActionHungerCost(ctx, state, actor, definition.DisplayName); err != nil {
			return err
		}
		mutationReason := fmt.Sprintf("我对 %s 进行了战地包扎。", target.DisplayName())
		if target.ID == actor.ID {
			mutationReason = "我给自己进行了战地包扎。"
		}
		if err := service.applyStatusMutation(
			ctx,
			state,
			target,
			status.FieldHP,
			12,
			events.ReasonRelationRescue,
			mutationReason,
		); err != nil {
			return err
		}
		defaultLogText := fmt.Sprintf("我对 %s 施放技能【%s】，恢复其生命并稳定状态。", target.DisplayName(), definition.DisplayName)
		if target.ID == actor.ID {
			defaultLogText = fmt.Sprintf("我给自己施放技能【%s】，恢复生命并稳定状态。", definition.DisplayName)
		}
		appendLog(
			state,
			"skill",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				defaultLogText,
			)),
			actor.ID,
			target.ID,
		)
		if target.ID == actor.ID {
			return nil
		}
		actorDelta := relationDelta{
			Trust:     0.64,
			Fear:      -0.18,
			Affection: 0.48,
			Rivalry:   -0.16,
		}
		targetDelta := relationDelta{
			Trust:     1.24,
			Fear:      -0.34,
			Affection: 0.92,
			Rivalry:   -0.24,
		}
		service.applyMutualRelationShiftBestEffort(ctx, state, actor, target, actorDelta, targetDelta, "战地包扎救援")
		return nil

	case "fire_assault":
		target := resolveTarget(opposingIDs(*state, actor.FactionID), byID, decision.TargetUnitID, actor)
		if target == nil {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但没找到有效目标。", definition.DisplayName),
				)),
				actor.ID,
				decision.TargetUnitID,
			)
			return nil
		}
		reach := attackReachWithWeather(*state, *actor)
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > reach+1 {
			appendLog(
				state,
				"hold",
				strings.TrimSpace(firstNonEmptyText(
					logText,
					fmt.Sprintf("我想施放【%s】，但目标太远。", definition.DisplayName),
				)),
				actor.ID,
				target.ID,
			)
			return nil
		}
		if err := service.applyActionHungerCost(ctx, state, actor, definition.DisplayName); err != nil {
			return err
		}

		damage := fireAssaultDamage(*state, *target)
		if err := service.applyStatusMutation(
			ctx,
			state,
			target,
			status.FieldHP,
			-float64(damage),
			events.ReasonCombatHit,
			fmt.Sprintf("我对 %s 发动火攻突袭。", target.DisplayName()),
		); err != nil {
			return err
		}
		if err := service.applyStatusMutation(
			ctx,
			state,
			target,
			status.FieldMorale,
			-0.06,
			events.ReasonEmotionTrauma,
			fmt.Sprintf("我被 %s 的火攻震慑。", actor.DisplayName()),
		); err != nil {
			return err
		}
		appendLog(
			state,
			"skill",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				fmt.Sprintf("我对 %s 施放技能【%s】，造成 %d 伤害并压制士气。", target.DisplayName(), definition.DisplayName, damage),
			)),
			actor.ID,
			target.ID,
		)
		actorDelta := relationDelta{
			Trust:     -0.18,
			Fear:      -0.03,
			Affection: -0.14,
			Rivalry:   0.82,
		}
		targetDelta := relationDelta{
			Trust:     -0.48,
			Fear:      0.52,
			Affection: -0.24,
			Rivalry:   1.06,
		}
		service.applyMutualRelationShiftBestEffort(ctx, state, actor, target, actorDelta, targetDelta, "火攻突袭")
		return nil

	case "last_stand":
		applied := false
		if grantCombatEffect(actor, combatEffectGuarded, state.TurnState.Turn+1) {
			applied = true
		}
		if grantCombatEffect(actor, combatEffectFocused, state.TurnState.Turn+1) {
			applied = true
		}
		if grantCombatEffect(actor, combatEffectRage, state.TurnState.Turn+1) {
			applied = true
		}
		if applied {
			if err := service.units.Save(ctx, *actor); err != nil {
				return err
			}
		}
		if err := service.applyActionHungerCost(ctx, state, actor, definition.DisplayName); err != nil {
			return err
		}
		if err := service.applyStatusMutation(
			ctx,
			state,
			actor,
			status.FieldMorale,
			0.12,
			events.ReasonEmotionReward,
			"我进入背水一战状态。",
		); err != nil {
			return err
		}
		appendLog(
			state,
			"skill",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				fmt.Sprintf("我施放技能【%s】，短时间内进入高压战斗状态。", definition.DisplayName),
			)),
			actor.ID,
			"",
		)
		return nil
	}

	return nil
}

// fireAssaultDamage 计算火攻突袭伤害，受目标地形与天气联动修正。
func fireAssaultDamage(state State, target unit.Record) int {
	damage := 10
	terrain := terrainAt(state.Map, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR})
	if terrain == world.TerrainGrassland && state.Weather.Type == WeatherWindy {
		damage = int(float64(damage) * 1.5)
	}
	if state.Weather.Type == WeatherRainy {
		damage = int(float64(damage) * 0.7)
	}
	if damage < 1 {
		damage = 1
	}
	return damage
}
