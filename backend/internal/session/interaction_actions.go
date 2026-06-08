package session

// 文件说明：执行阶段中的显式社交/交易动作，统一走单单位 LLM 决策与 AP 结算。

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

var executionTradeGoldOptions = []int{10, 20}

// buildDialogueCandidates 构造执行阶段的显式交谈候选。
func buildDialogueCandidates(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
) []decisionCandidate {
	targets := visibleCommunicationTargets(state, byID, actor)
	candidates := make([]decisionCandidate, 0, len(targets))
	for _, target := range targets {
		if dialogueBlockedByPolicy(state, actor, target) {
			continue
		}
		candidates = append(candidates, decisionCandidate{
			ID:           fmt.Sprintf("dialogue:%s", target.ID),
			Action:       DecisionActionDialogue,
			TargetUnitID: target.ID,
			Summary:      fmt.Sprintf("向视野内的 %s 说几句，交换眼下判断。", target.DisplayName()),
		})
	}
	return candidates
}

// buildSayCandidates 构造执行阶段的即时发言候选。
// 与 dialogue 不同，say 直接使用本次决策的 speak 文本落入对话历史，并给目标后续行动留下反应队列入口。
func buildSayCandidates(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
) []decisionCandidate {
	targets := visibleCommunicationTargets(state, byID, actor)
	candidates := make([]decisionCandidate, 0, len(targets))
	for _, target := range targets {
		if dialogueBlockedByPolicy(state, actor, target) {
			continue
		}
		candidates = append(candidates, decisionCandidate{
			ID:           fmt.Sprintf("say:%s", target.ID),
			Action:       DecisionActionSay,
			TargetUnitID: target.ID,
			Summary:      fmt.Sprintf("向视野内的 %s 说一句话；把 speak 当作实际台词写入对话历史，对方稍后可通过反应队列回应。", target.DisplayName()),
		})
	}
	return candidates
}

// buildTradeCandidates 构造执行阶段的单单位交易候选。
// 这里只放入 acting unit 自己就能发起的资源转移，不再走双人协商链。
func buildTradeCandidates(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
) []decisionCandidate {
	if actor == nil {
		return nil
	}

	targets := adjacentInteractionTargets(byID, actor)
	items := uniqueTradeableItems(*actor)
	candidates := make([]decisionCandidate, 0, len(targets)*(len(items)+len(executionTradeGoldOptions)))
	for _, target := range targets {
		if tradeBlockedByPolicy(state, actor, target) {
			continue
		}
		for _, itemID := range items {
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("trade:gift:%s:%s", target.ID, itemID),
				Action:       DecisionActionTrade,
				TradeKind:    TradeActionKindGift,
				ItemID:       itemID,
				TargetUnitID: target.ID,
				Summary:      fmt.Sprintf("把 %s 交给 %s；物品用途：%s。", displayItemName(itemID), target.DisplayName(), formatItemEffectByID(itemID)),
			})
			definition, found := item.Lookup(itemID)
			if !found {
				continue
			}
			for _, price := range purchasePriceOptions(*target, *actor, definition.Price) {
				if target.Status.Wallet < price {
					continue
				}
				candidates = append(candidates, decisionCandidate{
					ID:           fmt.Sprintf("trade:sell:%s:%s:%d", target.ID, itemID, price),
					Action:       DecisionActionTrade,
					TradeKind:    TradeActionKindSell,
					ItemID:       itemID,
					Price:        price,
					TargetUnitID: target.ID,
					Summary:      fmt.Sprintf("向 %s 提出 %d 金卖掉 %s；speak 必须写成对目标说的叫卖词。物品用途：%s。", target.DisplayName(), price, definition.DisplayName, formatItemEffectByID(itemID)),
				})
			}
		}
		for _, amount := range executionTradeGoldAmountOptions(actor.Status.Wallet) {
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("trade:gold:%s:%d", target.ID, amount),
				Action:       DecisionActionTrade,
				TradeKind:    TradeActionKindGold,
				GoldAmount:   amount,
				TargetUnitID: target.ID,
				Summary:      fmt.Sprintf("向 %s 调拨 %d 金。", target.DisplayName(), amount),
			})
		}
	}
	return candidates
}

// buildRomanceCandidates 构造执行阶段的显式恋爱/家庭候选，让 LLM 可以主动选择推进亲密关系。
func buildRomanceCandidates(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
) []decisionCandidate {
	if actor == nil {
		return nil
	}
	targets := adjacentInteractionTargets(byID, actor)
	candidates := make([]decisionCandidate, 0, len(targets))
	for _, target := range targets {
		if dialogueBlockedByPolicy(state, actor, target) {
			continue
		}
		switch {
		case actor.Social.LoverUnitID == "" && target.Social.LoverUnitID == "" &&
			hasEnoughPairDialogueTurns(state, actor.ID, target.ID, minRomanceDialogueTurns):
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("romance:%s", target.ID),
				Action:       DecisionActionRomance,
				TargetUnitID: target.ID,
				Summary:      fmt.Sprintf("向 %s 主动表露心意；需要至少 %d 个不同回合真实交流且仍需双方 LLM 同意。", target.DisplayName(), minRomanceDialogueTurns),
			})
		case canAttemptFamilyAction(state, actor, target):
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("family:%s", target.ID),
				Action:       DecisionActionFamily,
				TargetUnitID: target.ID,
				Summary:      fmt.Sprintf("和 %s 商量共同养育新生命；仍需双方 LLM 同意才会生效。", target.DisplayName()),
			})
		}
	}
	return candidates
}

func canAttemptFamilyAction(state State, actor *unit.Record, target *unit.Record) bool {
	if actor == nil || target == nil {
		return false
	}
	return actor.Social.LoverUnitID == target.ID &&
		target.Social.LoverUnitID == actor.ID &&
		pendingPregnancyForPair(state, actor.ID, target.ID) == nil &&
		state.TurnState.Turn-actor.Social.LastRomanceTurn >= 1 &&
		state.TurnState.Turn-target.Social.LastRomanceTurn >= 1
}

func executionTradeGoldAmountOptions(wallet int) []int {
	if wallet <= 0 {
		return nil
	}
	options := make([]int, 0, len(executionTradeGoldOptions))
	for _, amount := range executionTradeGoldOptions {
		if amount > 0 && wallet >= amount {
			options = append(options, amount)
		}
	}
	return options
}

func adjacentInteractionTargets(byID map[string]*unit.Record, actor *unit.Record) []*unit.Record {
	if actor == nil || len(byID) == 0 {
		return nil
	}
	targets := make([]*unit.Record, 0, 4)
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
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i int, j int) bool {
		if targets[i].DisplayName() == targets[j].DisplayName() {
			return targets[i].ID < targets[j].ID
		}
		return targets[i].DisplayName() < targets[j].DisplayName()
	})
	return targets
}

func visibleCommunicationTargets(state State, byID map[string]*unit.Record, actor *unit.Record) []*unit.Record {
	if actor == nil || len(byID) == 0 {
		return nil
	}
	targets := make([]*unit.Record, 0, 4)
	for _, target := range byID {
		if target == nil || target.ID == actor.ID || !isBattleReady(*target) {
			continue
		}
		if !communicationTargetVisible(state, actor, target) {
			continue
		}
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i int, j int) bool {
		if targets[i].DisplayName() == targets[j].DisplayName() {
			return targets[i].ID < targets[j].ID
		}
		return targets[i].DisplayName() < targets[j].DisplayName()
	})
	return targets
}

func communicationTargetVisible(state State, actor *unit.Record, target *unit.Record) bool {
	if actor == nil || target == nil || actor.ID == target.ID || !isBattleReady(*target) {
		return false
	}
	baseRange := actor.Stats.Derived.Vision
	if baseRange <= 0 {
		baseRange = 5
	}
	visibleTiles, err := world.ComputeVisibleTiles(
		state.Map,
		world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR},
		baseRange,
	)
	if err != nil {
		return unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) <= baseRange
	}
	targetCoord := world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR}
	for _, coord := range visibleTiles {
		if coord == targetCoord {
			return true
		}
	}
	return false
}

func resolveDialogueTarget(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetUnitID string,
) (*unit.Record, error) {
	target, err := resolveVisibleCommunicationTarget(state, byID, actor, targetUnitID)
	if err != nil {
		return nil, err
	}
	if dialogueBlockedByPolicy(state, actor, target) {
		return nil, fmt.Errorf("cross-faction contact is currently forbidden")
	}
	return target, nil
}

func resolveVisibleCommunicationTarget(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetUnitID string,
) (*unit.Record, error) {
	if actor == nil {
		return nil, fmt.Errorf("actor is nil")
	}
	targetUnitID = strings.TrimSpace(targetUnitID)
	if targetUnitID == "" {
		return nil, fmt.Errorf("target_unit_id is required")
	}
	target, ok := byID[targetUnitID]
	if !ok || !isBattleReady(*target) {
		return nil, fmt.Errorf("target_unit_id %q is invalid", targetUnitID)
	}
	if target.ID == actor.ID {
		return nil, fmt.Errorf("target_unit_id %q cannot point to actor itself", targetUnitID)
	}
	if !communicationTargetVisible(state, actor, target) {
		return nil, fmt.Errorf("target_unit_id %q is not visible", targetUnitID)
	}
	return target, nil
}

func resolveTradeTarget(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetUnitID string,
) (*unit.Record, error) {
	target, err := resolveAdjacentInteractionTarget(byID, actor, targetUnitID)
	if err != nil {
		return nil, err
	}
	if tradeBlockedByPolicy(state, actor, target) {
		return nil, fmt.Errorf("cross-faction trade is currently forbidden")
	}
	return target, nil
}

func resolveAdjacentInteractionTarget(
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetUnitID string,
) (*unit.Record, error) {
	if actor == nil {
		return nil, fmt.Errorf("actor is nil")
	}
	targetUnitID = strings.TrimSpace(targetUnitID)
	if targetUnitID == "" {
		return nil, fmt.Errorf("target_unit_id is required")
	}
	target, ok := byID[targetUnitID]
	if !ok || !isBattleReady(*target) {
		return nil, fmt.Errorf("target_unit_id %q is invalid", targetUnitID)
	}
	if target.ID == actor.ID {
		return nil, fmt.Errorf("target_unit_id %q cannot point to actor itself", targetUnitID)
	}
	if unit.HexDistance(
		actor.Status.PositionQ,
		actor.Status.PositionR,
		target.Status.PositionQ,
		target.Status.PositionR,
	) != 1 {
		return nil, fmt.Errorf("target_unit_id %q is not adjacent", targetUnitID)
	}
	return target, nil
}

func dialogueBlockedByPolicy(state State, actor *unit.Record, target *unit.Record) bool {
	if actor == nil || target == nil || actor.FactionID == target.FactionID {
		return false
	}
	policy := diplomacyPolicyForFaction(state, state.PlayerFactionID)
	return policy.ForbidCrossFactionContact && isPlayerEnemyFactionPair(state, actor.FactionID, target.FactionID)
}

func tradeBlockedByPolicy(state State, actor *unit.Record, target *unit.Record) bool {
	return false
}

// executeDialogue 执行一次单单位发起的交谈。
func (service *Service) executeDialogue(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	fallbackText := strings.TrimSpace(firstNonEmptyText(decision.Speak, decision.NextAction))
	target, err := resolveDialogueTarget(*state, byID, actor, decision.TargetUnitID)
	if err != nil {
		appendLog(
			state,
			"hold",
			strings.TrimSpace(firstNonEmptyText(
				fallbackText,
				fmt.Sprintf("我想找人聊聊，但没说成。"),
			)),
			actor.ID,
			decision.TargetUnitID,
		)
		return nil
	}
	logText := appendDecisionDialogueMessage(state, actor, decision)

	if err := service.applyActionHungerCost(ctx, state, actor, "交谈"); err != nil {
		return err
	}

	actorLine := strings.TrimSpace(firstNonEmptyText(
		logText,
		decision.NextAction,
		fmt.Sprintf("我和 %s 说了几句。", target.DisplayName()),
	))
	replyPayload, result, interaction, ok := service.requestUnitDialogueReply(ctx, *state, byID, actor, target, actorLine)
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	appendAIDialogue(state, *target, replyPayload.Reply, result)
	if service != nil {
		_ = service.rememberUnitWithSource(ctx, target, state.TurnState.Turn, replyPayload.Memory, "unit_dialogue_reply", 1)
	}
	if !ok {
		appendLog(state, "dialogue_error", "对方这次只做了简短回应。", target.ID, actor.ID)
	}
	appendLog(
		state,
		"unit_dialogue",
		fmt.Sprintf("%s：%s；%s：%s", actor.DisplayName(), actorLine, target.DisplayName(), replyPayload.Reply),
		actor.ID,
		target.ID,
	)

	if actor.FactionID == target.FactionID {
		delta := relationDelta{
			Trust:     0.18,
			Fear:      -0.03,
			Affection: 0.14,
			Rivalry:   -0.06,
		}
		service.applyMutualRelationShiftBestEffort(ctx, state, actor, target, delta, delta, "主动交谈")
		return nil
	}

	delta := relationDelta{
		Trust:     0.08,
		Fear:      -0.02,
		Affection: 0.04,
		Rivalry:   -0.02,
	}
	incrementCrossFactionInteraction(state, "cross_faction_dialogue", actor, target)
	service.applyMutualRelationShiftBestEffort(ctx, state, actor, target, delta, delta, "主动跨阵营交谈")
	service.maybeResolveIntelligenceEvent(ctx, state, actor, target, "execution_dialogue")
	return nil
}

// executeSay 执行一次即时发言；这是轻量交谈动作，会直接进入对话历史和执行日志。
func (service *Service) executeSay(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	target, err := resolveDialogueTarget(*state, byID, actor, decision.TargetUnitID)
	line := strings.TrimSpace(firstNonEmptyText(decision.Speak, decision.NextAction, decision.Reasoning))
	if err != nil {
		appendLog(state, "say_blocked", strings.TrimSpace(firstNonEmptyText(line, "我想喊话，但没找准对象。")), actor.ID, decision.TargetUnitID)
		return nil
	}
	if line == "" {
		line = fmt.Sprintf("%s，我有话说。", target.DisplayName())
	}
	decision.Speak = strings.TrimSpace(line)
	line = appendDecisionDialogueMessage(state, actor, decision)
	if line == "" {
		line = decision.Speak
	}

	if err := service.applyActionHungerCost(ctx, state, actor, "喊话"); err != nil {
		return err
	}

	appendLog(
		state,
		"say",
		fmt.Sprintf("%s 对 %s 说：%s", actor.DisplayName(), target.DisplayName(), line),
		actor.ID,
		target.ID,
	)
	return nil
}

// executeTrade 执行一次单单位发起的资源转移。
func (service *Service) executeTrade(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) error {
	fallbackText := strings.TrimSpace(firstNonEmptyText(decision.Speak, decision.NextAction))
	target, err := resolveTradeTarget(*state, byID, actor, decision.TargetUnitID)
	if err != nil {
		appendLog(
			state,
			"trade_blocked",
			strings.TrimSpace(firstNonEmptyText(
				fallbackText,
				"我想调拨物资，但这回合没办成。",
			)),
			actor.ID,
			decision.TargetUnitID,
		)
		return nil
	}
	appendDecisionDialogueMessage(state, actor, decision)
	if offerText := tradeOfferText(decision, target); offerText != "" {
		appendLog(state, "trade_offer", offerText, actor.ID, target.ID)
	}
	consent, result, interaction, ok := service.requestTradeConsent(ctx, *state, byID, actor, target, decision)
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	service.recordTradeConsentDialogue(ctx, state, target, consent, result)
	if !ok || !consent.Accepted {
		appendLog(state, "trade_rejected", tradeRejectedText(decision, target, consent), target.ID, actor.ID)
		service.applyTradeRelationShiftBestEffort(ctx, state, actor, target, false, "目标拒绝交易")
		return service.applyActionHungerCost(ctx, state, actor, "提出交易")
	}
	appendLog(state, "trade_accept", tradeAcceptedText(decision, target, consent), target.ID, actor.ID)

	trades := unit.NewTradeService(service.units)
	switch decision.TradeKind {
	case TradeActionKindGift:
		nextActor, nextTarget, tradeErr := trades.GiftItem(ctx, actor.ID, target.ID, decision.ItemID)
		if tradeErr != nil {
			appendLog(state, "trade_blocked", tradeFailureText(decision, target), actor.ID, target.ID)
			service.applyTradeRelationShiftBestEffort(ctx, state, actor, target, false, "主动赠与受阻")
			return nil
		}
		*byID[actor.ID] = nextActor
		*byID[target.ID] = nextTarget
	case TradeActionKindGold:
		nextActor, nextTarget, tradeErr := trades.TransferGold(ctx, actor.ID, target.ID, decision.GoldAmount)
		if tradeErr != nil {
			appendLog(state, "trade_blocked", tradeFailureText(decision, target), actor.ID, target.ID)
			service.applyTradeRelationShiftBestEffort(ctx, state, actor, target, false, "主动调拨失败")
			return nil
		}
		*byID[actor.ID] = nextActor
		*byID[target.ID] = nextTarget
	case TradeActionKindSell:
		nextBuyer, nextSeller, tradeErr := trades.PurchaseItem(ctx, target.ID, actor.ID, decision.ItemID, decision.Price)
		if tradeErr != nil {
			appendLog(state, "trade_blocked", tradeFailureText(decision, target), actor.ID, target.ID)
			service.applyTradeRelationShiftBestEffort(ctx, state, actor, target, false, "出售失败")
			return nil
		}
		*byID[target.ID] = nextBuyer
		*byID[actor.ID] = nextSeller
	default:
		appendLog(state, "trade_blocked", "我想调拨物资，但动作不成立。", actor.ID, target.ID)
		return nil
	}

	if err := service.applyActionHungerCost(ctx, state, actor, "调拨物资"); err != nil {
		return err
	}

	target = byID[target.ID]
	appendLog(state, "trade", tradeSuccessText(decision, target), actor.ID, target.ID)
	service.applyTradeRelationShiftBestEffort(ctx, state, actor, target, true, "主动调拨物资")
	if actor.FactionID != target.FactionID {
		incrementCrossFactionInteraction(state, "cross_faction_trade", actor, target)
	}
	service.maybeResolveIntelligenceEvent(ctx, state, actor, target, "execution_trade")
	return nil
}

func appendDecisionDialogueMessage(state *State, actor *unit.Record, decision unitDecisionPayload) string {
	if state == nil || actor == nil {
		return ""
	}
	message := strings.TrimSpace(firstNonEmptyText(decision.Speak, decision.NextAction))
	if message == "" {
		return ""
	}
	appendDialogue(state, DialogueMessage{
		ID:         uuid.NewString(),
		UnitID:     actor.ID,
		Speaker:    actor.DisplayName(),
		Message:    message,
		Turn:       state.TurnState.Turn,
		Phase:      state.TurnState.Phase,
		OccurredAt: time.Now().UTC(),
	})
	return message
}

func tradeSuccessText(decision unitDecisionPayload, target *unit.Record) string {
	targetName := decision.TargetUnitID
	if target != nil {
		targetName = target.DisplayName()
	}
	switch decision.TradeKind {
	case TradeActionKindGift:
		return fmt.Sprintf("我把 %s 交给了 %s。", displayItemName(decision.ItemID), targetName)
	case TradeActionKindGold:
		return fmt.Sprintf("我向 %s 调拨了 %d 金。", targetName, decision.GoldAmount)
	case TradeActionKindSell:
		return fmt.Sprintf("我以 %d 金把 %s 卖给了 %s。", decision.Price, displayItemName(decision.ItemID), targetName)
	default:
		return "我把这笔物资调拨办完了。"
	}
}

func tradeOfferText(decision unitDecisionPayload, target *unit.Record) string {
	targetName := decision.TargetUnitID
	if target != nil {
		targetName = target.DisplayName()
	}
	line := strings.TrimSpace(firstNonEmptyText(decision.Speak, decision.NextAction))
	switch decision.TradeKind {
	case TradeActionKindGift:
		if line == "" {
			line = fmt.Sprintf("%s，这个%s给你。", targetName, displayItemName(decision.ItemID))
		}
		return fmt.Sprintf("我向 %s 提出赠与 %s：%s", targetName, displayItemName(decision.ItemID), line)
	case TradeActionKindGold:
		if line == "" {
			line = fmt.Sprintf("%s，我给你调 %d 金。", targetName, decision.GoldAmount)
		}
		return fmt.Sprintf("我向 %s 提出调拨 %d 金：%s", targetName, decision.GoldAmount, line)
	case TradeActionKindSell:
		if line == "" {
			line = fmt.Sprintf("%s，%d 金买这件%s吗？", targetName, decision.Price, displayItemName(decision.ItemID))
		}
		return fmt.Sprintf("我向 %s 开价 %d 金出售 %s：%s", targetName, decision.Price, displayItemName(decision.ItemID), line)
	default:
		return line
	}
}

func tradeAcceptedText(decision unitDecisionPayload, target *unit.Record, consent tradeConsentPayload) string {
	targetName := decision.TargetUnitID
	if target != nil {
		targetName = target.DisplayName()
	}
	reply := strings.TrimSpace(consent.Reply)
	if reply == "" {
		reply = "我接受。"
	}
	return fmt.Sprintf("%s 接受交易：%s", targetName, reply)
}

func tradeRejectedText(decision unitDecisionPayload, target *unit.Record, consent tradeConsentPayload) string {
	targetName := decision.TargetUnitID
	if target != nil {
		targetName = target.DisplayName()
	}
	reason := strings.TrimSpace(consent.Reason)
	if reason == "" {
		reason = "对方没有同意这笔交易。"
	}
	reply := strings.TrimSpace(consent.Reply)
	if reply != "" && reply != reason {
		return fmt.Sprintf("%s 拒绝交易：%s（理由：%s）", targetName, reply, reason)
	}
	return fmt.Sprintf("%s 拒绝交易：%s", targetName, reason)
}

func tradeFailureText(decision unitDecisionPayload, target *unit.Record) string {
	targetName := decision.TargetUnitID
	if target != nil {
		targetName = target.DisplayName()
	}
	switch decision.TradeKind {
	case TradeActionKindGift:
		return fmt.Sprintf("我想把 %s 交给 %s，但这回合没办成。", displayItemName(decision.ItemID), targetName)
	case TradeActionKindGold:
		return fmt.Sprintf("我想向 %s 调拨 %d 金，但这回合没办成。", targetName, decision.GoldAmount)
	case TradeActionKindSell:
		return fmt.Sprintf("我想以 %d 金把 %s 卖给 %s，但这回合没办成。", decision.Price, displayItemName(decision.ItemID), targetName)
	default:
		return "我想调拨物资，但这回合没办成。"
	}
}
