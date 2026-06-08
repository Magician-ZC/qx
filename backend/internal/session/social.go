package session

// 文件说明：处理行动间隙社交事件，串联相邻对话、自主交易与跨阵营情报触发逻辑。

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"

	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
)

const actionGapInteractionChance = 0.15

// maybeTriggerActionGapInteraction 在单位行动间隙尝试触发“相邻交流事件”。
// 只有满足存活、相邻、概率命中等条件时才会进入后续对话/交易/情报链路。
func (service *Service) maybeTriggerActionGapInteraction(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	actionIndex int,
) {
	if service == nil || state == nil || actor == nil || !isBattleReady(*actor) {
		return
	}

	partner := service.selectAdjacentConversationPartner(ctx, *state, byID, actor)
	if partner == nil {
		return
	}
	if !shouldTriggerActionGapInteraction(*state, *actor, *partner, actionIndex) {
		return
	}

	service.triggerActionGapInteraction(ctx, state, byID, actor, partner, decision)
}

// triggerActionGapInteraction 执行一次行动间隙交流。
// 会先生成双方对话种子，再记录对话，并串联“自主交易”与“情报事件”。
func (service *Service) triggerActionGapInteraction(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	partner *unit.Record,
	decision unitDecisionPayload,
) {
	if service == nil || state == nil || actor == nil || partner == nil {
		return
	}
	if unit.HexDistance(
		actor.Status.PositionQ,
		actor.Status.PositionR,
		partner.Status.PositionQ,
		partner.Status.PositionR,
	) != 1 {
		return
	}

	actorEvent := actionGapConversationSeedForActor(decision)
	if actorEvent == "" {
		actorEvent = fmt.Sprintf("我和 %s 简短聊了几句。", partner.DisplayName())
	}
	partnerEvent := actionGapConversationSeedForPartner(*actor, decision)
	if partnerEvent == "" {
		partnerEvent = fmt.Sprintf("我和 %s 简短聊了几句。", actor.DisplayName())
	}

	service.recordPairDialogueBestEffort(
		ctx,
		state,
		byID,
		actor,
		partner,
		actorEvent,
		partnerEvent,
		"",
	)
	service.maybeResolveActionGapTrade(ctx, state, byID, actor, partner)
	service.maybeResolveRomanceAndFamily(ctx, state, byID, actor, partner)
	service.maybeResolveIntelligenceEvent(
		ctx,
		state,
		actor,
		partner,
		"action_gap_conversation",
	)
}

// maybeResolveActionGapTrade 在行动间隙里尝试结算单位自主交易。
// 支持赠送、购买、交换、金币调拨；每种都会同步日志、记忆、关系变化和跨阵营交互指标。
func (service *Service) maybeResolveActionGapTrade(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
) {
	if service == nil || state == nil || left == nil || right == nil {
		return
	}
	if !isTradeReadyInState(*state, byID, *left) || !isTradeReadyInState(*state, byID, *right) {
		return
	}
	if alreadyTradedThisTurn(*state, left.ID, right.ID) {
		return
	}

	policy := diplomacyPolicyForFaction(*state, state.PlayerFactionID)
	if left.FactionID != right.FactionID &&
		policy.ForbidCrossFactionTrade &&
		isPlayerEnemyFactionPair(*state, left.FactionID, right.FactionID) {
		appendLog(
			state,
			"strict_order_block_trade",
			fmt.Sprintf(
				"%s 与 %s 这回合遵令不交易。%s",
				left.DisplayName(),
				right.DisplayName(),
				policy.SourceText,
			),
			left.ID,
			right.ID,
		)
		return
	}

	candidates := buildDeploymentCandidates(*left, *right)
	if len(candidates) <= 1 {
		return
	}

	choice, result, interaction, err := service.generateDeploymentChoice(ctx, *state, byID, left, right, candidates)
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	if err != nil {
		appendLog(
			state,
			"dialogue_error",
			"我们这回合没谈成。",
			left.ID,
			right.ID,
		)
		return
	}

	candidate, ok := resolveDeploymentCandidate(candidates, choice.CandidateID)
	if !ok {
		return
	}
	if candidate.Kind == "hold" {
		appendDeploymentDialogue(state, byID[left.ID], byID[right.ID], choice, result)
		rememberDeploymentMemories(service, ctx, state, byID[left.ID], byID[right.ID], choice)
		appendLog(
			state,
			"trade_hold",
			actionGapTradeHoldMessage(choice, left, right),
			left.ID,
			right.ID,
		)
		return
	}

	trades := unit.NewTradeService(service.units)
	switch candidate.Kind {
	case "gift":
		nextActor, nextTarget, tradeErr := trades.GiftItem(ctx, candidate.ActorUnitID, candidate.TargetUnitID, candidate.ItemID)
		if tradeErr != nil {
			appendLog(
				state,
				"trade_blocked",
				actionGapTradeFailureMessage(choice, candidate, byID, tradeErr),
				candidate.ActorUnitID,
				candidate.TargetUnitID,
			)
			service.applyTradeRelationShiftBestEffort(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], false, "行动间隙交易失败")
			return
		}
		*byID[candidate.ActorUnitID] = nextActor
		*byID[candidate.TargetUnitID] = nextTarget
		appendDeploymentDialogue(state, byID[left.ID], byID[right.ID], choice, result)
		rememberDeploymentMemories(service, ctx, state, byID[left.ID], byID[right.ID], choice)
		appendLog(
			state,
			"trade",
			actionGapTradeSuccessMessage(choice, candidate, byID),
			candidate.ActorUnitID,
			candidate.TargetUnitID,
		)
		service.applyTradeRelationShiftBestEffort(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], true, "行动间隙自主交易")
		incrementCrossFactionInteraction(state, "cross_faction_trade", byID[candidate.ActorUnitID], byID[candidate.TargetUnitID])
		service.maybeResolveIntelligenceEvent(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], "action_gap_trade")
	case "purchase":
		definition, found := item.Lookup(candidate.ItemID)
		if !found {
			return
		}
		price := candidate.Price
		if price <= 0 {
			price = definition.Price
		}
		nextActor, nextTarget, tradeErr := trades.PurchaseItem(ctx, candidate.ActorUnitID, candidate.TargetUnitID, candidate.ItemID, price)
		if tradeErr != nil {
			appendLog(
				state,
				"trade_blocked",
				actionGapTradeFailureMessage(choice, candidate, byID, tradeErr),
				candidate.ActorUnitID,
				candidate.TargetUnitID,
			)
			service.applyTradeRelationShiftBestEffort(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], false, "行动间隙交易失败")
			return
		}
		*byID[candidate.ActorUnitID] = nextActor
		*byID[candidate.TargetUnitID] = nextTarget
		appendDeploymentDialogue(state, byID[left.ID], byID[right.ID], choice, result)
		rememberDeploymentMemories(service, ctx, state, byID[left.ID], byID[right.ID], choice)
		appendLog(
			state,
			"trade",
			actionGapTradeSuccessMessage(choice, candidate, byID),
			candidate.ActorUnitID,
			candidate.TargetUnitID,
		)
		service.applyTradeRelationShiftBestEffort(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], true, "行动间隙自主交易")
		incrementCrossFactionInteraction(state, "cross_faction_trade", byID[candidate.ActorUnitID], byID[candidate.TargetUnitID])
		service.maybeResolveIntelligenceEvent(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], "action_gap_trade")
	case "swap":
		nextLeft, nextRight, tradeErr := trades.SwapItems(ctx, left.ID, right.ID, candidate.ItemID, candidate.OtherItemID)
		if tradeErr != nil {
			appendLog(
				state,
				"trade_blocked",
				actionGapTradeFailureMessage(choice, candidate, byID, tradeErr),
				left.ID,
				right.ID,
			)
			service.applyTradeRelationShiftBestEffort(ctx, state, left, right, false, "行动间隙交易失败")
			return
		}
		*byID[left.ID] = nextLeft
		*byID[right.ID] = nextRight
		appendDeploymentDialogue(state, byID[left.ID], byID[right.ID], choice, result)
		rememberDeploymentMemories(service, ctx, state, byID[left.ID], byID[right.ID], choice)
		appendLog(
			state,
			"trade",
			actionGapTradeSuccessMessage(choice, candidate, byID),
			left.ID,
			right.ID,
		)
		service.applyTradeRelationShiftBestEffort(ctx, state, byID[left.ID], byID[right.ID], true, "行动间隙自主交易")
		incrementCrossFactionInteraction(state, "cross_faction_trade", byID[left.ID], byID[right.ID])
		service.maybeResolveIntelligenceEvent(ctx, state, byID[left.ID], byID[right.ID], "action_gap_trade")
	case "gold":
		nextActor, nextTarget, tradeErr := trades.TransferGold(ctx, candidate.ActorUnitID, candidate.TargetUnitID, candidate.GoldAmount)
		if tradeErr != nil {
			appendLog(
				state,
				"trade_blocked",
				actionGapTradeFailureMessage(choice, candidate, byID, tradeErr),
				candidate.ActorUnitID,
				candidate.TargetUnitID,
			)
			service.applyTradeRelationShiftBestEffort(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], false, "行动间隙交易失败")
			return
		}
		*byID[candidate.ActorUnitID] = nextActor
		*byID[candidate.TargetUnitID] = nextTarget
		appendDeploymentDialogue(state, byID[left.ID], byID[right.ID], choice, result)
		rememberDeploymentMemories(service, ctx, state, byID[left.ID], byID[right.ID], choice)
		appendLog(
			state,
			"trade",
			actionGapTradeSuccessMessage(choice, candidate, byID),
			candidate.ActorUnitID,
			candidate.TargetUnitID,
		)
		service.applyTradeRelationShiftBestEffort(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], true, "行动间隙自主交易")
		incrementCrossFactionInteraction(state, "cross_faction_trade", byID[candidate.ActorUnitID], byID[candidate.TargetUnitID])
		service.maybeResolveIntelligenceEvent(ctx, state, byID[candidate.ActorUnitID], byID[candidate.TargetUnitID], "action_gap_trade")
	}
}

// alreadyTradedThisTurn 判断同一对单位在本回合是否已完成过交易，防止重复触发。
func alreadyTradedThisTurn(state State, leftID string, rightID string) bool {
	if strings.TrimSpace(leftID) == "" || strings.TrimSpace(rightID) == "" {
		return false
	}
	for _, entry := range state.Logs {
		if entry.Turn != state.TurnState.Turn || entry.Kind != "trade" {
			continue
		}
		if samePair(entry.ActorUnitID, entry.TargetUnitID, leftID, rightID) {
			return true
		}
	}
	return false
}

// samePair 判断两组 actor/target 是否表示同一对单位（忽略先后顺序）。
func samePair(actorA string, actorB string, leftID string, rightID string) bool {
	return (actorA == leftID && actorB == rightID) || (actorA == rightID && actorB == leftID)
}

// actionGapTradeSuccessMessage 生成交易成功文案，优先使用模型总结，缺失时回退本地文案。
func actionGapTradeSuccessMessage(
	choice deploymentChoicePayload,
	candidate deploymentCandidate,
	byID map[string]*unit.Record,
) string {
	if summary := deploymentChoiceNarrative(choice); summary != "" {
		return summary
	}
	fallback := strings.TrimSpace(candidate.Summary)
	if fallback != "" {
		return fallback
	}
	return fallbackActionGapTradeSummary(candidate, byID)
}

// actionGapTradeFailureMessage 生成交易失败文案，尽量保留模型摘要并补充“没谈成”语义。
func actionGapTradeFailureMessage(
	choice deploymentChoicePayload,
	candidate deploymentCandidate,
	byID map[string]*unit.Record,
	_ error,
) string {
	summary := deploymentChoiceNarrative(choice)
	if summary == "" {
		summary = strings.TrimSpace(candidate.Summary)
	}
	if summary == "" {
		summary = fallbackActionGapTradeSummary(candidate, byID)
	}
	if summary == "" {
		summary = "这回合我们没谈成。"
	}
	if containsAny(summary, "没谈成", "暂不成交", "观望") {
		return summary
	}
	return fmt.Sprintf("%s；这回合没谈成。", summary)
}

// actionGapTradeHoldMessage 生成“本轮观望不成交”文案。
func actionGapTradeHoldMessage(choice deploymentChoicePayload, _ *unit.Record, _ *unit.Record) string {
	summary := deploymentChoiceNarrative(choice)
	if summary != "" {
		return summary
	}
	return "我们这回合先不成交。"
}

// actionGapConversationSeedForActor 从行动决策中提取可复用的对话种子文本。
func actionGapConversationSeedForActor(decision unitDecisionPayload) string {
	for _, text := range []string{decision.NextAction, decision.Speak, decision.Memory, decision.Reasoning} {
		if value := strings.TrimSpace(text); value != "" {
			return value
		}
	}
	return ""
}

// actionGapConversationSeedForPartner 为对话另一方构造带说话人前缀的对话种子。
func actionGapConversationSeedForPartner(actor unit.Record, decision unitDecisionPayload) string {
	seed := actionGapConversationSeedForActor(decision)
	if seed == "" {
		return ""
	}
	if strings.TrimSpace(actor.DisplayName()) == "" {
		return seed
	}
	return fmt.Sprintf("%s：%s", actor.DisplayName(), seed)
}

// fallbackActionGapTradeSummary 在模型未给出摘要时，根据交易类型拼接兜底叙述。
func fallbackActionGapTradeSummary(candidate deploymentCandidate, byID map[string]*unit.Record) string {
	targetName := candidate.TargetUnitID
	if record, ok := byID[candidate.TargetUnitID]; ok && record != nil {
		targetName = record.DisplayName()
	}

	switch candidate.Kind {
	case "gift":
		return fmt.Sprintf("我把 %s 交给了 %s。", displayItemName(candidate.ItemID), targetName)
	case "purchase":
		return fmt.Sprintf("我向 %s 买下了 %s。", targetName, displayItemName(candidate.ItemID))
	case "swap":
		return fmt.Sprintf("我和 %s 互换了 %s 与 %s。", targetName, displayItemName(candidate.ItemID), displayItemName(candidate.OtherItemID))
	case "gold":
		return fmt.Sprintf("我向 %s 调拨了 %d 金。", targetName, candidate.GoldAmount)
	default:
		return "我把这笔交易办完了。"
	}
}

// selectAdjacentConversationPartner 从相邻单位中选出最可能交流的对象。
// 评分综合阵营关系、人格社交倾向、互信层级、血量压力和外交禁令。
func (service *Service) selectAdjacentConversationPartner(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
) *unit.Record {
	if actor == nil || len(byID) == 0 {
		return nil
	}
	relationByTarget := service.loadOutgoingRelationMap(ctx, actor.ID)
	policy := diplomacyPolicyForFaction(state, state.PlayerFactionID)

	type scoredTarget struct {
		record *unit.Record
		score  float64
	}
	candidates := make([]scoredTarget, 0, 4)
	for _, target := range byID {
		if target == nil || target.ID == actor.ID || !isBattleReady(*target) {
			continue
		}
		if unit.HexDistance(
			actor.Status.PositionQ,
			actor.Status.PositionR,
			target.Status.PositionQ,
			target.Status.PositionR,
		) != 1 {
			continue
		}
		if actor.FactionID != target.FactionID &&
			policy.ForbidCrossFactionContact &&
			isPlayerEnemyFactionPair(state, actor.FactionID, target.FactionID) {
			continue
		}

		score := 0.2 + actor.Personality.Sociability*0.4
		if actor.FactionID == target.FactionID {
			score += 1.0
		}
		if isTrustedCompanion(*actor, *target) {
			score += 0.6
		}
		if relation, ok := relationByTarget[target.ID]; ok {
			score += relationAffinityFromScores(relation.Trust, relation.Affection, relation.Rivalry, relation.Fear)
			switch relationTierFromScores(relation.Trust, relation.Affection, relation.Rivalry, relation.Fear) {
			case "羁绊":
				score += 0.45
			case "熟人":
				score += 0.12
			case "敌视":
				score -= 0.35
			}
		}
		if target.Status.HP <= 45 {
			score += 0.2
		}
		candidates = append(candidates, scoredTarget{
			record: target,
			score:  score,
		})
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i int, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].record.ID < candidates[j].record.ID
		}
		return candidates[i].score > candidates[j].score
	})
	return candidates[0].record
}

// shouldTriggerActionGapInteraction 根据确定性随机值判断本次是否触发行动间隙互动。
func shouldTriggerActionGapInteraction(state State, actor unit.Record, partner unit.Record, actionIndex int) bool {
	return actionGapInteractionRoll(state, actor, partner, actionIndex) < actionGapInteractionChance
}

// actionGapInteractionRoll 生成可复现的互动随机值，保证回放时结果一致。
func actionGapInteractionRoll(state State, actor unit.Record, partner unit.Record, actionIndex int) float64 {
	if actionIndex < 1 {
		actionIndex = 1
	}
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", state.TurnState.Turn)))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", actionIndex)))
	_, _ = hasher.Write([]byte(actor.ID))
	_, _ = hasher.Write([]byte(partner.ID))
	return float64(hasher.Sum32()%10000) / 10000
}
