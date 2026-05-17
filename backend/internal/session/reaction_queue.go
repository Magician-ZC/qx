package session

// 文件说明：reaction_queue.go，整理应激反应信号给主行动 LLM；旧规则决策函数仅保留给排查。

import (
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// reactionTrigger 类型定义用于统一该模块的数据表达。
type reactionTrigger string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	reactionOnFriendDown   reactionTrigger = "on_friend_down"
	reactionOnEnterRange   reactionTrigger = "on_enter_range"
	reactionOnLowHP        reactionTrigger = "on_low_hp"
	reactionOnFlank        reactionTrigger = "on_flank"
	reactionOnSeeNemesis   reactionTrigger = "on_see_nemesis"
	reactionOnAllyExecuted reactionTrigger = "on_ally_executed"
	reactionOnHeardSay     reactionTrigger = "on_heard_say"
)

// reactionQueueDecision 表示一次规则触发的应激决策结果。
// 当前主执行链路不再直接采用该动作；应激信息通过 reactionSignalsForPrompt 交给 LLM 自主判断。
type reactionQueueDecision struct {
	Trigger  reactionTrigger
	Decision unitDecisionPayload
	Note     string
}

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

// resolveReactionQueueDecision 按触发优先级选择一条应激反应决策。
// Deprecated: 主执行链路应调用 LLM；该函数仅保留给旧路径或排查使用。
func resolveReactionQueueDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	remainingAP int,
) (reactionQueueDecision, bool) {
	if actor == nil || !isBattleReady(*actor) || remainingAP <= 0 {
		return reactionQueueDecision{}, false
	}

	candidates := buildDecisionCandidates(state, byID, actor, targetIDs, remainingAP)
	if len(candidates) == 0 {
		return reactionQueueDecision{}, false
	}

	if decision, ok := reactionOnAllyExecutedDecision(state, byID, actor, targetIDs, candidates); ok {
		return decision, true
	}
	if decision, ok := reactionOnFriendDownDecision(state, byID, actor, targetIDs, candidates); ok {
		return decision, true
	}
	if decision, ok := reactionOnLowHPDecision(state, byID, actor, targetIDs, candidates); ok {
		return decision, true
	}
	if decision, ok := reactionOnHeardSayDecision(state, byID, actor, candidates); ok {
		return decision, true
	}
	if decision, ok := reactionOnSeeNemesisDecision(state, byID, actor, targetIDs, candidates); ok {
		return decision, true
	}
	if decision, ok := reactionOnFlankDecision(state, byID, actor, targetIDs, candidates); ok {
		return decision, true
	}
	if decision, ok := reactionOnEnterRangeDecision(state, byID, actor, targetIDs, candidates); ok {
		return decision, true
	}
	return reactionQueueDecision{}, false
}

// reactionOnAllyExecutedDecision 处理“目睹同伴被重击击倒”触发。
func reactionOnAllyExecutedDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (reactionQueueDecision, bool) {
	attacker, ally, ok := reactionRecentDownEvent(state, byID, actor, true)
	if !ok {
		return reactionQueueDecision{}, false
	}

	if decision, ok := attackReactionDecision(
		candidates,
		attacker.ID,
		fmt.Sprintf("刚看到同伴 %s 被重击击倒，我要立刻反击。", ally.DisplayName()),
	); ok {
		return reactionQueueDecision{
			Trigger:  reactionOnAllyExecuted,
			Decision: decision,
			Note:     fmt.Sprintf("见到同伴 %s 被重击倒下，优先压制 %s。", ally.DisplayName(), attacker.DisplayName()),
		}, true
	}

	return reactionQueueDecision{}, false
}

// reactionOnFriendDownDecision 处理“附近友军倒下”触发。
func reactionOnFriendDownDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (reactionQueueDecision, bool) {
	attacker, ally, ok := reactionRecentDownEvent(state, byID, actor, false)
	if !ok {
		return reactionQueueDecision{}, false
	}

	if decision, ok := attackReactionDecision(
		candidates,
		attacker.ID,
		fmt.Sprintf("同伴 %s 刚倒下，我先压制 %s。", ally.DisplayName(), attacker.DisplayName()),
	); ok {
		return reactionQueueDecision{
			Trigger:  reactionOnFriendDown,
			Decision: decision,
			Note:     fmt.Sprintf("同伴 %s 倒下，抢占决策对 %s 进行应激反击。", ally.DisplayName(), attacker.DisplayName()),
		}, true
	}

	if decision, ok := decisionFromCandidateID(
		candidates,
		"observe",
		"同伴刚倒下，我先观察敌势找反击窗口。",
	); ok {
		return reactionQueueDecision{
			Trigger:  reactionOnFriendDown,
			Decision: decision,
			Note:     fmt.Sprintf("同伴 %s 倒下，改为观察稳态。", ally.DisplayName()),
		}, true
	}
	return reactionQueueDecision{}, false
}

// reactionOnLowHPDecision 处理“自身低血量”触发。
func reactionOnLowHPDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (reactionQueueDecision, bool) {
	maxHP := float64(actor.Stats.Primary.Constitution * 10)
	if maxHP < 1 {
		maxHP = 100
	}
	hpRatio := clampFloat(float64(actor.Status.HP)/maxHP, 0, 1)
	if hpRatio > 0.35 && actor.Status.HP > 25 {
		return reactionQueueDecision{}, false
	}

	if enemy := nearestBattleReady(targetIDs, byID, actor); enemy != nil {
		if decision, ok := retreatMoveDecision(*actor, *enemy, candidates); ok {
			return reactionQueueDecision{
				Trigger:  reactionOnLowHP,
				Decision: decision,
				Note:     fmt.Sprintf("生命过低，先拉开与 %s 的距离。", enemy.DisplayName()),
			}, true
		}
	}
	if decision, ok := decisionFromCandidateID(candidates, "defend", "我伤势太重，先架起防守稳住。"); ok {
		return reactionQueueDecision{
			Trigger:  reactionOnLowHP,
			Decision: decision,
			Note:     "生命过低，抢占防御动作。",
		}, true
	}
	return reactionQueueDecision{}, false
}

// reactionOnHeardSayDecision 处理“刚有人对我发言”的即时回应。
func reactionOnHeardSayDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	candidates []decisionCandidate,
) (reactionQueueDecision, bool) {
	speaker, line, ok := reactionRecentSayToActor(state, byID, actor)
	if !ok || speaker == nil {
		return reactionQueueDecision{}, false
	}
	decision, ok := decisionFromCandidateID(
		candidates,
		fmt.Sprintf("say:%s", speaker.ID),
		fmt.Sprintf("%s 刚对我说“%s”，我立刻回应。", speaker.DisplayName(), limitTextRunes(line, 12)),
	)
	if !ok {
		return reactionQueueDecision{}, false
	}
	return reactionQueueDecision{
		Trigger:  reactionOnHeardSay,
		Decision: decision,
		Note:     fmt.Sprintf("听到 %s 的话后进入反应队列回应。", speaker.DisplayName()),
	}, true
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

// reactionOnSeeNemesisDecision 处理“识别宿敌”触发。
func reactionOnSeeNemesisDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (reactionQueueDecision, bool) {
	for _, targetID := range targetIDs {
		target := byID[targetID]
		if target == nil || !isBattleReady(*target) {
			continue
		}
		if !hasExplicitNemesisMemoryForTarget(*actor, *target) {
			continue
		}
		decision, ok := attackReactionDecision(
			candidates,
			target.ID,
			fmt.Sprintf("我把 %s 视为高威胁目标，这一拍先压住他。", target.DisplayName()),
		)
		if !ok {
			continue
		}
		return reactionQueueDecision{
			Trigger:  reactionOnSeeNemesis,
			Decision: decision,
			Note:     fmt.Sprintf("识别高威胁目标 %s，抢占攻击窗口。", target.DisplayName()),
		}, true
	}
	return reactionQueueDecision{}, false
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

// reactionOnFlankDecision 处理“形成夹击”触发。
func reactionOnFlankDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (reactionQueueDecision, bool) {
	target := flankedEnemyTarget(state, byID, actor, targetIDs)
	if target == nil {
		return reactionQueueDecision{}, false
	}
	decision, ok := attackReactionDecision(
		candidates,
		target.ID,
		fmt.Sprintf("我和队友已经形成夹击，继续压住 %s。", target.DisplayName()),
	)
	if !ok {
		return reactionQueueDecision{}, false
	}
	return reactionQueueDecision{
		Trigger:  reactionOnFlank,
		Decision: decision,
		Note:     fmt.Sprintf("对 %s 形成夹击，反应队列抢占输出。", target.DisplayName()),
	}, true
}

// reactionOnEnterRangeDecision 处理“敌军进入射程”触发。
func reactionOnEnterRangeDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (reactionQueueDecision, bool) {
	reach := attackReachWithWeather(state, *actor)
	for _, targetID := range targetIDs {
		target := byID[targetID]
		if target == nil || !isBattleReady(*target) {
			continue
		}
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
		if distance > reach {
			continue
		}
		decision, ok := attackReactionDecision(
			candidates,
			target.ID,
			fmt.Sprintf("%s 进入射程，先打断对方节奏。", target.DisplayName()),
		)
		if !ok {
			continue
		}
		return reactionQueueDecision{
			Trigger:  reactionOnEnterRange,
			Decision: decision,
			Note:     fmt.Sprintf("敌军 %s 进入射程，抢占先手打击。", target.DisplayName()),
		}, true
	}
	return reactionQueueDecision{}, false
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

// retreatMoveDecision 在可选移动中寻找“远离威胁最多”的撤退方案。
func retreatMoveDecision(
	actor unit.Record,
	threat unit.Record,
	candidates []decisionCandidate,
) (unitDecisionPayload, bool) {
	currentDistance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, threat.Status.PositionQ, threat.Status.PositionR)
	bestDistance := currentDistance
	bestID := ""
	for _, candidate := range candidates {
		if candidate.Action != DecisionActionMove {
			continue
		}
		distance := unit.HexDistance(candidate.TargetQ, candidate.TargetR, threat.Status.PositionQ, threat.Status.PositionR)
		if distance > bestDistance {
			bestDistance = distance
			bestID = candidate.ID
		}
	}
	if bestID == "" {
		return unitDecisionPayload{}, false
	}
	return decisionFromCandidateID(
		candidates,
		bestID,
		fmt.Sprintf("我先后撤拉开和 %s 的距离。", threat.DisplayName()),
	)
}

// attackReactionDecision 按重击>冲锋>普攻顺序选择攻击候选。
func attackReactionDecision(
	candidates []decisionCandidate,
	targetID string,
	reasoning string,
) (unitDecisionPayload, bool) {
	if decision, ok := decisionFromCandidateID(candidates, fmt.Sprintf("heavy:%s", targetID), reasoning); ok {
		return decision, true
	}
	if decision, ok := decisionFromCandidateID(candidates, fmt.Sprintf("charge:%s", targetID), reasoning); ok {
		return decision, true
	}
	if decision, ok := decisionFromCandidateID(candidates, fmt.Sprintf("attack:%s", targetID), reasoning); ok {
		return decision, true
	}
	return unitDecisionPayload{}, false
}

// decisionFromCandidateID 按候选 ID 还原决策，并清空叙述字段交给反思流程生成。
func decisionFromCandidateID(
	candidates []decisionCandidate,
	candidateID string,
	reasoning string,
) (unitDecisionPayload, bool) {
	candidateID = strings.TrimSpace(candidateID)
	if candidateID == "" {
		return unitDecisionPayload{}, false
	}
	for _, candidate := range candidates {
		if candidate.ID != candidateID {
			continue
		}
		decision := decisionFromCandidate(candidate, unitDecisionChoicePayload{Reasoning: strings.TrimSpace(reasoning)})
		// ReactionQueue is rule-triggered; keep narration fields empty so the same-turn
		// reflection path can produce AI-native `next_action/speak/memory`.
		decision.NextAction = ""
		decision.Speak = ""
		decision.Memory = ""
		return decision, true
	}
	return unitDecisionPayload{}, false
}

// formatReactionTrigger 规范化触发器名称用于日志与埋点。
func formatReactionTrigger(trigger reactionTrigger) string {
	switch trigger {
	case reactionOnFriendDown:
		return "on_friend_down"
	case reactionOnEnterRange:
		return "on_enter_range"
	case reactionOnLowHP:
		return "on_low_hp"
	case reactionOnFlank:
		return "on_flank"
	case reactionOnSeeNemesis:
		return "on_see_nemesis"
	case reactionOnAllyExecuted:
		return "on_ally_executed"
	case reactionOnHeardSay:
		return "on_heard_say"
	default:
		return string(trigger)
	}
}

// reactionDecisionTargetCoord 返回反应决策的目标坐标文本（仅移动动作）。
func reactionDecisionTargetCoord(decision unitDecisionPayload) string {
	if decision.Action != DecisionActionMove {
		return ""
	}
	coord := decisionCoord(decision)
	return fmt.Sprintf("%d,%d", coord.Q, coord.R)
}

// reactionDecisionTargetRecord 通过目标 unit_id 读取目标记录。
func reactionDecisionTargetRecord(byID map[string]*unit.Record, targetUnitID string) *unit.Record {
	if byID == nil || strings.TrimSpace(targetUnitID) == "" {
		return nil
	}
	return byID[targetUnitID]
}

// reactionDecisionDistance 计算决策执行后与目标的距离。
func reactionDecisionDistance(actor unit.Record, decision unitDecisionPayload, target *unit.Record) int {
	if target == nil {
		return 0
	}
	switch decision.Action {
	case DecisionActionMove:
		coord := decisionCoord(decision)
		return unit.HexDistance(coord.Q, coord.R, target.Status.PositionQ, target.Status.PositionR)
	default:
		return unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
	}
}

// reactionDecisionCoord 返回决策对应坐标。
func reactionDecisionCoord(decision unitDecisionPayload) world.Coord {
	return decisionCoord(decision)
}
