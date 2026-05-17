package session

// 文件说明：补充设施相关动作，包括敌方设施攻击、己方设施拆除与对应候选生成。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// structureTargetLabel 返回面向当前行动者的设施目标文案。
func structureTargetLabel(structure Structure, actorFactionID string) string {
	side := "敌方"
	if structure.FactionID == actorFactionID {
		side = "己方"
	}
	return fmt.Sprintf("%s%s", side, structureDisplayName(structure.Type))
}

// structureHostileToActor 判断设施是否对当前单位处于敌对关系。
func structureHostileToActor(state State, actor *unit.Record, structure Structure) bool {
	if actor == nil {
		return false
	}
	if strings.TrimSpace(structure.FactionID) == "" || structure.FactionID == actor.FactionID {
		return false
	}
	return factionRelationBetween(state, actor.FactionID, structure.FactionID) == FactionRelationWar
}

// resolveHostileStructureTarget 按结构 ID 解析当前单位可攻击的敌对设施。
func resolveHostileStructureTarget(state State, actor *unit.Record, structureID string) (int, *Structure) {
	structureID = strings.TrimSpace(structureID)
	if actor == nil || structureID == "" {
		return -1, nil
	}
	index := structureIndexByID(state.Structures, structureID)
	if index < 0 {
		return -1, nil
	}
	structure := &state.Structures[index]
	if !structureHostileToActor(state, actor, *structure) {
		return -1, nil
	}
	return index, structure
}

// resolveFriendlyStructureAtActor 解析单位脚下、可被己方拆除的设施。
func resolveFriendlyStructureAtActor(state State, actor *unit.Record, structureID string) (int, *Structure) {
	if actor == nil {
		return -1, nil
	}
	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	if strings.TrimSpace(structureID) == "" {
		structure := structureAt(state.Structures, coord)
		if structure == nil || structure.FactionID != actor.FactionID {
			return -1, nil
		}
		return structureIndexByID(state.Structures, structure.ID), structure
	}
	index := structureIndexByID(state.Structures, strings.TrimSpace(structureID))
	if index < 0 {
		return -1, nil
	}
	structure := &state.Structures[index]
	if structure.FactionID != actor.FactionID {
		return -1, nil
	}
	if structure.Q != coord.Q || structure.R != coord.R {
		return -1, nil
	}
	return index, structure
}

// buildDemolishCandidate 生成己方设施拆除候选。
func buildDemolishCandidate(structure Structure) decisionCandidate {
	return decisionCandidate{
		ID:            fmt.Sprintf("demolish:%s", structure.ID),
		Action:        DecisionActionDemolish,
		StructureID:   structure.ID,
		StructureType: structure.Type,
		Summary:       fmt.Sprintf("拆除脚下的%s，腾空这一格。", structureDisplayName(structure.Type)),
	}
}

// buildStructureCombatCandidates 生成对敌方设施的攻击候选。
func buildStructureCombatCandidates(state State, actor *unit.Record, remainingAP int) []decisionCandidate {
	if actor == nil || remainingAP <= 0 {
		return nil
	}

	reach := attackReachWithWeather(state, *actor)
	weatherMoveMultiplier := weatherAdjustedMoveMultiplier(state, *actor)
	canCharge := remainingAP >= decisionActionCost(DecisionActionCharge) && effectiveMoveRange(actor.Status.Move, weatherMoveMultiplier) >= 1
	candidates := make([]decisionCandidate, 0, 6)

	for _, structure := range state.Structures {
		if !structureHostileToActor(state, actor, structure) {
			continue
		}
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, structure.Q, structure.R)
		targetLabel := structureTargetLabel(structure, actor.FactionID)
		if remainingAP >= decisionActionCost(DecisionActionAttack) && distance <= reach {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("attack_structure:%s", structure.ID),
				Action:        DecisionActionAttack,
				StructureID:   structure.ID,
				StructureType: structure.Type,
				Summary:       fmt.Sprintf("攻击%s。", targetLabel),
			})
		}
		if remainingAP >= decisionActionCost(DecisionActionHeavyAttack) && distance <= reach+1 {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("heavy_structure:%s", structure.ID),
				Action:        DecisionActionHeavyAttack,
				StructureID:   structure.ID,
				StructureType: structure.Type,
				Summary:       fmt.Sprintf("重击%s（破坏工事更坚决，但有落空风险）。", targetLabel),
			})
		}
		if canCharge && distance <= reach+2 {
			candidates = append(candidates, decisionCandidate{
				ID:            fmt.Sprintf("charge_structure:%s", structure.ID),
				Action:        DecisionActionCharge,
				StructureID:   structure.ID,
				StructureType: structure.Type,
				Summary:       fmt.Sprintf("冲锋逼近并攻击%s。", targetLabel),
			})
		}
	}
	return candidates
}

// removeStructureAt 删除指定索引的设施。
func removeStructureAt(state *State, index int) {
	if state == nil || index < 0 || index >= len(state.Structures) {
		return
	}
	state.Structures = append(state.Structures[:index], state.Structures[index+1:]...)
}

// executeDemolish 执行己方设施拆除。
func (service *Service) executeDemolish(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	logText := decisionLogText(decision)
	index, structure := resolveFriendlyStructureAtActor(*state, actor, decision.StructureID)
	if structure == nil {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				logText,
				"我想拆掉脚下设施，但目标已经不对了。",
			)),
			actor.ID,
			"",
		)
		return nil
	}

	structureName := structureDisplayName(structure.Type)
	if err := service.applyActionHungerCost(ctx, state, actor, "拆除设施"); err != nil {
		return err
	}
	removeStructureAt(state, index)
	appendLog(
		state,
		"demolish",
		strings.TrimSpace(firstNonEmptyText(
			logText,
			fmt.Sprintf("我拆除了脚下的%s，腾出了这块地。", structureName),
		)),
		actor.ID,
		"",
	)
	return nil
}

// executeEngageStructureWithStyle 执行针对敌方设施的攻击/冲锋/重击。
func (service *Service) executeEngageStructureWithStyle(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	modifiers actionModifiers,
	style combatAttackStyle,
	maxAdvanceSteps int,
) error {
	aiText := decisionLogText(decision)
	preferredStructureID := strings.TrimSpace(decision.StructureID)
	_, structure := resolveHostileStructureTarget(*state, actor, preferredStructureID)
	if structure == nil {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				"我暂时没找到可拆的敌方设施。",
			)),
			actor.ID,
			"",
		)
		return nil
	}

	reach := attackReachWithWeather(*state, *actor)
	moveLimit := effectiveMoveRange(actor.Status.Move, modifiers.MoveMultiplier*weatherAdjustedMoveMultiplier(*state, *actor))
	targetCoord := world.Coord{Q: structure.Q, R: structure.R}
	before := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	steps := 0
	if maxAdvanceSteps < 0 {
		maxAdvanceSteps = 0
	}
	if moveLimit > 0 && maxAdvanceSteps > 0 {
		advanceSteps := maxAdvanceSteps
		if advanceSteps > moveLimit {
			advanceSteps = moveLimit
		}
		advanceSteps = weatherAdjustedAdvanceSteps(*state, *actor, advanceSteps)
		var err error
		steps, err = moveActorToward(
			state.Map,
			byID,
			actor,
			targetCoord,
			advanceSteps,
			reach,
		)
		if err != nil {
			return err
		}
	}
	if steps > 0 {
		if err := service.units.Save(ctx, *actor); err != nil {
			return err
		}
		appendLog(
			state,
			"move",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				fmt.Sprintf("我从 %d,%d 压向 %d,%d，准备拆掉%s。", before.Q, before.R, actor.Status.PositionQ, actor.Status.PositionR, structureTargetLabel(*structure, actor.FactionID)),
			)),
			actor.ID,
			"",
		)
		if err := service.applyActionHungerCost(ctx, state, actor, style.Label+"逼近"); err != nil {
			return err
		}
		if triggered, err := service.triggerTrapAt(ctx, state, actor); err != nil {
			return err
		} else if triggered && !isBattleReady(*actor) {
			return nil
		}
	}

	index, structure := resolveHostileStructureTarget(*state, actor, preferredStructureID)
	if structure == nil {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				"我逼近后发现目标设施已经被清掉了。",
			)),
			actor.ID,
			"",
		)
		return nil
	}
	reach = attackReachWithWeather(*state, *actor)
	if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, structure.Q, structure.R) > reach {
		appendLog(
			state,
			"advance",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				fmt.Sprintf("我继续朝%s逼近。", structureTargetLabel(*structure, actor.FactionID)),
			)),
			actor.ID,
			"",
		)
		return nil
	}
	return service.applyAttackToStructure(ctx, state, actor, index, style, aiText)
}

// applyAttackToStructure 结算一次针对设施的攻击；命中后会直接摧毁目标设施。
func (service *Service) applyAttackToStructure(
	ctx context.Context,
	state *State,
	attacker *unit.Record,
	structureIndex int,
	style combatAttackStyle,
	aiText string,
) error {
	if structureIndex < 0 || structureIndex >= len(state.Structures) {
		return nil
	}

	target := state.Structures[structureIndex]
	dummyTarget := unit.Record{ID: target.ID}
	dummyTarget.Status.PositionQ = target.Q
	dummyTarget.Status.PositionR = target.R

	if err := service.applyActionHungerCost(ctx, state, attacker, style.Label); err != nil {
		return err
	}
	if style.MissChance > 0 && combatActionRoll(*state, *attacker, dummyTarget, style.Label) < style.MissChance {
		appendLog(
			state,
			"attack_miss",
			strings.TrimSpace(firstNonEmptyText(
				aiText,
				fmt.Sprintf("我对%s发起%s，但没有拆中要害。", structureTargetLabel(target, attacker.FactionID), style.Label),
			)),
			attacker.ID,
			"",
		)
		return nil
	}

	removeStructureAt(state, structureIndex)
	appendLog(
		state,
		"attack",
		strings.TrimSpace(firstNonEmptyText(
			aiText,
			fmt.Sprintf("我对%s发起%s，直接将其摧毁。", structureTargetLabel(target, attacker.FactionID), style.Label),
		)),
		attacker.ID,
		"",
	)
	return nil
}
