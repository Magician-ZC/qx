package session

// 文件说明：reaction_queue.go，整理应激反应信号交给主行动 LLM 自主判断（不直接产出或覆盖动作）。

import (
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
)

// reactionSignalsForPrompt 把应激条件整理成提示词信号；不直接产出或覆盖动作。
func reactionSignalsForPrompt(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
) string {
	if actor == nil || !isBattleReady(*actor) {
		return "无"
	}

	signals := make([]string, 0, 6)
	if attacker, ally, ok := reactionRecentDownEvent(state, byID, actor, true); ok {
		signals = append(signals, fmt.Sprintf("刚看到同伴 %s 被 %s 重击击倒。", ally.DisplayName(), attacker.DisplayName()))
	} else if attacker, ally, ok := reactionRecentDownEvent(state, byID, actor, false); ok {
		signals = append(signals, fmt.Sprintf("附近同伴 %s 刚被 %s 击倒。", ally.DisplayName(), attacker.DisplayName()))
	}

	maxHP := float64(actor.Stats.Primary.Constitution * 10)
	if maxHP < 1 {
		maxHP = 100
	}
	hpRatio := clampFloat(float64(actor.Status.HP)/maxHP, 0, 1)
	if hpRatio <= 0.35 || actor.Status.HP <= 25 {
		signals = append(signals, fmt.Sprintf("自身生命偏低：HP=%d，保命压力很高。", actor.Status.HP))
	}

	if speaker, line, ok := reactionRecentSayToActor(state, byID, actor); ok && speaker != nil {
		signals = append(signals, fmt.Sprintf("%s 刚对我说：%s。", speaker.DisplayName(), limitTextRunes(line, 24)))
	}

	for _, targetID := range targetIDs {
		target := byID[targetID]
		if target == nil || !isBattleReady(*target) {
			continue
		}
		if hasExplicitNemesisMemoryForTarget(*actor, *target) {
			signals = append(signals, fmt.Sprintf("记忆中把 %s 视为高威胁或旧仇目标。", target.DisplayName()))
			break
		}
	}

	if target := flankedEnemyTarget(state, byID, actor, targetIDs); target != nil {
		signals = append(signals, fmt.Sprintf("我方已对 %s 形成夹击机会。", target.DisplayName()))
	}

	reach := attackReachWithWeather(state, *actor)
	inRange := make([]string, 0, 3)
	for _, targetID := range targetIDs {
		target := byID[targetID]
		if target == nil || !isBattleReady(*target) {
			continue
		}
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
		if distance <= reach {
			inRange = append(inRange, fmt.Sprintf("%s(距%d)", target.DisplayName(), distance))
			if len(inRange) >= 3 {
				break
			}
		}
	}
	if len(inRange) > 0 {
		signals = append(signals, fmt.Sprintf("攻击距离内目标：%s。", strings.Join(inRange, "、")))
	}

	if len(signals) == 0 {
		return "无"
	}
	return "- " + strings.Join(signals, "\n- ")
}

func reactionRecentSayToActor(state State, byID map[string]*unit.Record, actor *unit.Record) (*unit.Record, string, bool) {
	if actor == nil {
		return nil, "", false
	}
	recent := reactionRecentExecutionLogs(state, 16)
	for _, entry := range recent {
		if entry.Kind != "say" || entry.TargetUnitID != actor.ID || entry.ActorUnitID == actor.ID {
			continue
		}
		speaker := byID[entry.ActorUnitID]
		if speaker == nil || !isBattleReady(*speaker) {
			continue
		}
		if actorAlreadySpokeToUnitThisTurn(state, actor.ID, speaker.ID) {
			continue
		}
		if !communicationTargetVisible(state, actor, speaker) {
			continue
		}
		return speaker, extractSayLine(entry.Message), true
	}
	return nil, "", false
}

func actorAlreadySpokeToUnitThisTurn(state State, actorID string, targetID string) bool {
	for _, entry := range state.Logs {
		if entry.Turn != state.TurnState.Turn || entry.Phase != turns.PhaseExecution {
			continue
		}
		if entry.Kind == "say" && entry.ActorUnitID == actorID && entry.TargetUnitID == targetID {
			return true
		}
	}
	return false
}

func extractSayLine(message string) string {
	message = strings.TrimSpace(message)
	if index := strings.LastIndex(message, "说："); index >= 0 {
		return strings.TrimSpace(message[index+len("说："):])
	}
	return message
}

func hasExplicitNemesisMemoryForTarget(actor unit.Record, target unit.Record) bool {
	targetNames := []string{target.DisplayName(), target.Identity.Nickname}
	for _, highlight := range unit.RecentHighlights(actor, 6) {
		text := strings.ToLower(strings.TrimSpace(highlight))
		if text == "" || !containsAny(text, targetNames...) {
			continue
		}
		if isAutoNemesisReactionMemory(text) {
			continue
		}
		if containsAny(text, "宿敌", "仇敌", "仇人", "旧仇", "血债", "复仇", "死敌") {
			return true
		}
	}
	return false
}

func isAutoNemesisReactionMemory(text string) bool {
	return strings.Contains(text, "认出宿敌") && (strings.Contains(text, "盯死") || strings.Contains(text, "准备"))
}

// reactionRecentDownEvent 从最近执行日志中检索友军倒地事件。
func reactionRecentDownEvent(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	requiresHeavy bool,
) (*unit.Record, *unit.Record, bool) {
	recent := reactionRecentExecutionLogs(state, 18)
	for _, entry := range recent {
		if entry.Kind != "attack" || entry.ActorUnitID == actor.ID {
			continue
		}
		if !strings.Contains(entry.Message, "倒下") {
			continue
		}
		if requiresHeavy && !strings.Contains(entry.Message, "重击") {
			continue
		}
		ally := byID[entry.TargetUnitID]
		attacker := byID[entry.ActorUnitID]
		if ally == nil || attacker == nil {
			continue
		}
		if ally.FactionID != actor.FactionID || ally.ID == actor.ID {
			continue
		}
		if attacker.FactionID == actor.FactionID {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) > 3 {
			continue
		}
		return attacker, ally, true
	}
	return nil, nil, false
}

// reactionRecentExecutionLogs 抽取当前回合执行阶段的最近日志。
func reactionRecentExecutionLogs(state State, limit int) []LogEntry {
	if limit <= 0 || len(state.Logs) == 0 {
		return nil
	}
	entries := make([]LogEntry, 0, limit)
	for index := len(state.Logs) - 1; index >= 0 && len(entries) < limit; index-- {
		entry := state.Logs[index]
		if entry.Turn != state.TurnState.Turn || entry.Phase != turns.PhaseExecution {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// flankedEnemyTarget 查找被己方至少两人近身包夹的敌方目标。
func flankedEnemyTarget(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
) *unit.Record {
	for _, targetID := range targetIDs {
		target := byID[targetID]
		if target == nil || !isBattleReady(*target) {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > 1 {
			continue
		}
		adjacentAllies := 0
		for _, allyID := range alliedIDs(state, actor.FactionID) {
			ally := byID[allyID]
			if ally == nil || !isBattleReady(*ally) {
				continue
			}
			if unit.HexDistance(ally.Status.PositionQ, ally.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) <= 1 {
				adjacentAllies++
			}
		}
		if adjacentAllies >= 2 {
			return target
		}
	}
	return nil
}
