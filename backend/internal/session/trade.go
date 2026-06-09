package session

// 文件说明：trade.go，单位自主交易系统，处理相邻交换、购买、赠与与金币调拨结算。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
)

// tradeItemSignal 定义“文本关键词 -> 物品 ID”的轻量映射，用于从对话上下文推断交易物品。
type tradeItemSignal struct {
	itemID   string
	keywords []string
}

type tradeConsentPayload struct {
	Accepted bool   `json:"accepted"`
	Reply    string `json:"reply"`
	Reason   string `json:"reason"`
	Memory   string `json:"memory,omitempty"`
}

var tradeConsentSchema = []byte(`{
  "type":"object",
  "properties":{
    "accepted":{"type":"boolean"},
    "reply":{"type":"string","minLength":1},
    "reason":{"type":"string","minLength":1},
    "memory":{"type":"string"}
  },
  "required":["accepted","reply","reason"],
  "additionalProperties":false
}`)

var deploymentTradeSignals = []tradeItemSignal{
	{itemID: "carrier_pigeon", keywords: []string{"信鸽", "传信", "通信", "联络"}},
	{itemID: "ration", keywords: []string{"口粮", "补给", "粮", "食物"}},
	{itemID: "herb_bundle", keywords: []string{"药草", "伤药", "治疗", "医药"}},
	{itemID: "wood", keywords: []string{"木材", "木头", "木料"}},
	{itemID: "stone", keywords: []string{"石料", "石头"}},
	{itemID: "iron_ore", keywords: []string{"铁矿", "矿石", "矿料"}},
	{itemID: "leather", keywords: []string{"皮革", "兽皮"}},
	{itemID: "rope", keywords: []string{"绳", "绳索"}},
	{itemID: "pickaxe", keywords: []string{"铁镐", "镐"}},
	{itemID: "long_sword", keywords: []string{"长剑", "剑"}},
}

const minimumTradeThreatDistance = 3

func (service *Service) requestTradeConsent(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	target *unit.Record,
	decision unitDecisionPayload,
) (tradeConsentPayload, ai.CompletionResult, LLMInteraction, bool) {
	proposal := tradeConsentProposal(decision, actor, target)
	systemPrompt := fmt.Sprintf(
		"你是《一念》中的单位 %s。有人向你提出战场交易，你必须只代表自己判断 accept/reject；可以因为敌我关系、风险、需要、信任、物品价值或当前局势拒绝，并写清理由。只能返回 JSON。",
		target.DisplayName(),
	)
	userPrompt := buildTradeConsentPrompt(state, byID, actor, target, proposal)
	if llmBudgetGuardrailActive(state) {
		payload := tradeConsentPayload{Accepted: true, Reply: "我接受。", Reason: "预算护栏下按低风险调拨处理。", Memory: "我接受了战场交易。"}
		result := budgetGuardrailResult(state)
		return payload, result, buildLLMInteraction(state, target.ID, "trade_consent", tradeConsentSummary(payload, actor, target), systemPrompt, userPrompt, result, ""), true
	}
	if service == nil || service.llm == nil {
		payload := tradeConsentPayload{Accepted: true, Reply: "我接受。", Reason: "没有额外反对理由。", Memory: "我接受了战场交易。"}
		result := ai.CompletionResult{Provider: "rules", Model: "fallback"}
		return payload, result, buildLLMInteraction(state, target.ID, "trade_consent", tradeConsentSummary(payload, actor, target), systemPrompt, userPrompt, result, ""), true
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskDialogue,
		SchemaName:     "session_trade_consent",
		ResponseSchema: tradeConsentSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.55,
		MaxTokens:      180,
		Timeout:        llmRequestTimeout,
	})
	if err != nil {
		payload := tradeConsentPayload{Accepted: false, Reply: "我先不答应。", Reason: "我没能及时判断这笔交易是否安全。", Memory: "我拒绝了一笔不明交易。"}
		return payload, result, buildLLMInteraction(state, target.ID, "trade_consent", tradeConsentSummary(payload, actor, target), systemPrompt, userPrompt, result, err.Error()), false
	}

	var payload tradeConsentPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		payload = tradeConsentPayload{Accepted: false, Reply: "我先不答应。", Reason: "交易回应格式异常，无法确认同意。", Memory: "我拒绝了一笔异常交易。"}
		cause := fmt.Sprintf("decode trade consent payload: %v", err)
		return payload, result, buildLLMInteraction(state, target.ID, "trade_consent", tradeConsentSummary(payload, actor, target), systemPrompt, userPrompt, result, cause), false
	}
	payload = normalizeTradeConsentPayload(payload)
	if payload.Reply == "" || payload.Reason == "" {
		if payload.Reply == "" {
			payload.Reply = "我先不答应。"
		}
		if payload.Reason == "" {
			payload.Reason = "没有说明清楚理由。"
		}
		payload.Accepted = false
		return payload, result, buildLLMInteraction(state, target.ID, "trade_consent", tradeConsentSummary(payload, actor, target), systemPrompt, userPrompt, result, "trade consent payload is incomplete"), false
	}
	return payload, result, buildLLMInteraction(state, target.ID, "trade_consent", tradeConsentSummary(payload, actor, target), systemPrompt, userPrompt, result, ""), true
}

func buildTradeConsentPrompt(state State, byID map[string]*unit.Record, actor *unit.Record, target *unit.Record, proposal string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "当前回合：T%d / %s\n", state.TurnState.Turn, state.TurnState.Phase)
	fmt.Fprintf(&builder, "交易提议：%s\n", proposal)
	fmt.Fprintf(&builder, "提议方：%s[%s] HP=%d 饥饿=%d 钱包=%d 坐标=%d,%d\n", actor.DisplayName(), actor.FactionID, actor.Status.HP, actor.Status.Hunger, actor.Status.Wallet, actor.Status.PositionQ, actor.Status.PositionR)
	fmt.Fprintf(&builder, "你：%s[%s] HP=%d 饥饿=%d 钱包=%d 坐标=%d,%d\n", target.DisplayName(), target.FactionID, target.Status.HP, target.Status.Hunger, target.Status.Wallet, target.Status.PositionQ, target.Status.PositionR)
	_ = byID
	fmt.Fprintln(&builder, "判断规则：")
	fmt.Fprintln(&builder, "1. accepted=true 表示你自愿接受交易；accepted=false 表示拒绝。")
	fmt.Fprintln(&builder, "2. 敌我双方也可以交易，但你可以因为敌意、诈骗风险、战略价值、补给不足或不信任而拒绝。")
	fmt.Fprintln(&builder, "3. reply 是你对提议方说出口的一句话，reason 必须写清接受或拒绝理由；拒绝时尤其要具体。")
	fmt.Fprintln(&builder, "4. 不要替提议方决定，只代表你自己。")
	return builder.String()
}

func tradeConsentProposal(decision unitDecisionPayload, actor *unit.Record, target *unit.Record) string {
	actorName := "提议方"
	targetName := decision.TargetUnitID
	if actor != nil {
		actorName = actor.DisplayName()
	}
	if target != nil {
		targetName = target.DisplayName()
	}
	switch decision.TradeKind {
	case TradeActionKindGift:
		return fmt.Sprintf("%s 想把 %s 交给 %s。物品用途：%s。", actorName, displayItemName(decision.ItemID), targetName, formatItemEffectByID(decision.ItemID))
	case TradeActionKindGold:
		return fmt.Sprintf("%s 想向 %s 调拨 %d 金。", actorName, targetName, decision.GoldAmount)
	case TradeActionKindSell:
		return fmt.Sprintf("%s 想以 %d 金把 %s 卖给 %s。叫卖词：%s。物品用途：%s。", actorName, decision.Price, displayItemName(decision.ItemID), targetName, strings.TrimSpace(firstNonEmptyText(decision.Speak, decision.NextAction)), formatItemEffectByID(decision.ItemID))
	default:
		return fmt.Sprintf("%s 想和 %s 交易。", actorName, targetName)
	}
}

func normalizeTradeConsentPayload(payload tradeConsentPayload) tradeConsentPayload {
	payload.Reply = limitTextRunes(strings.TrimSpace(payload.Reply), 28)
	payload.Reason = limitTextRunes(strings.TrimSpace(payload.Reason), 48)
	payload.Memory = limitTextRunes(strings.TrimSpace(payload.Memory), 24)
	if payload.Memory == "" {
		payload.Memory = limitTextRunes(payload.Reason, 24)
	}
	return payload
}

func tradeConsentSummary(payload tradeConsentPayload, actor *unit.Record, target *unit.Record) string {
	verdict := "拒绝"
	if payload.Accepted {
		verdict = "接受"
	}
	actorName := "对方"
	targetName := "目标"
	if actor != nil {
		actorName = actor.DisplayName()
	}
	if target != nil {
		targetName = target.DisplayName()
	}
	return fmt.Sprintf("%s %s 了 %s 的交易：%s", targetName, verdict, actorName, payload.Reason)
}

func (service *Service) recordTradeConsentDialogue(ctx context.Context, state *State, target *unit.Record, consent tradeConsentPayload, result ai.CompletionResult) {
	if state == nil || target == nil {
		return
	}
	appendAIDialogue(state, *target, consent.Reply, result)
	if service != nil {
		_ = service.rememberUnitWithSource(ctx, target, state.TurnState.Turn, consent.Memory, "trade_consent", 1)
	}
}

// applyTradeRelationShiftBestEffort 按交易结果对双方关系做增减益，并采用 best-effort 不阻断主流程。
func (service *Service) applyTradeRelationShiftBestEffort(
	ctx context.Context,
	state *State,
	left *unit.Record,
	right *unit.Record,
	success bool,
	reason string,
) {
	if service == nil || left == nil || right == nil || left.ID == right.ID {
		return
	}

	if success {
		delta := relationDelta{
			Trust:     0.72,
			Fear:      -0.12,
			Affection: 0.34,
			Rivalry:   -0.16,
		}
		if left.FactionID != right.FactionID {
			delta.Trust = 0.48
			delta.Fear = -0.08
			delta.Affection = 0.18
			delta.Rivalry = -0.10
		}
		service.applyMutualRelationShiftBestEffort(ctx, state, left, right, delta, delta, reason)
		return
	}

	delta := relationDelta{
		Trust:     -0.24,
		Fear:      0.08,
		Affection: -0.12,
		Rivalry:   0.30,
	}
	service.applyMutualRelationShiftBestEffort(ctx, state, left, right, delta, delta, reason)
}

// isTradeReadyInState 判断单位当前是否具备交易资格。
// 执行阶段下会额外检查周围威胁距离，避免贴近敌军时仍触发社交/交易。
func isTradeReadyInState(state State, byID map[string]*unit.Record, record unit.Record) bool {
	if record.Status.LifeState != unit.LifeStateActive || record.Status.HP <= 0 {
		return false
	}
	if state.TurnState.Phase != turns.PhaseExecution {
		return true
	}
	return nearestThreatDistance(state, byID, &record) >= minimumTradeThreatDistance
}

// deploymentTradeContext 聚合与指定单位相关的历史指令和对话文本，供规则/模型判断交易意图。
func deploymentTradeContext(state State, unitIDs ...string) string {
	allowed := make(map[string]struct{}, len(unitIDs))
	for _, unitID := range unitIDs {
		allowed[unitID] = struct{}{}
	}

	var builder strings.Builder
	builder.WriteString(strings.ToLower(directiveForFaction(state, state.PlayerFactionID)))
	builder.WriteRune('\n')
	for _, entry := range state.DialogueHistory {
		if len(allowed) == 0 {
			builder.WriteString(strings.ToLower(entry.Message))
			builder.WriteRune('\n')
			continue
		}
		if _, ok := allowed[entry.UnitID]; !ok {
			continue
		}
		builder.WriteString(strings.ToLower(entry.Message))
		builder.WriteRune('\n')
	}
	return builder.String()
}

// resolveRequestedGift 尝试按上下文解析并执行“赠与物品”交易。
// 命中后会写回库存、记录对话与日志。
func (service *Service) resolveRequestedGift(
	ctx context.Context,
	state *State,
	trades *unit.TradeService,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	contextText string,
) bool {
	if !containsAny(contextText, "给", "交给", "递给", "分给") {
		return false
	}

	itemID, ok := requestedTradeItem(contextText)
	if !ok {
		return false
	}

	giver, receiver := resolveItemDirection(left, right, itemID)
	if giver == nil || receiver == nil {
		return false
	}

	nextGiver, nextReceiver, err := trades.GiftItem(ctx, giver.ID, receiver.ID, itemID)
	if err != nil {
		appendLog(
			state,
			"trade_blocked",
			fmt.Sprintf("我想把 %s 交给 %s，这回合没谈成。", displayItemName(itemID), receiver.DisplayName()),
			giver.ID,
			receiver.ID,
		)
		return true
	}

	*byID[giver.ID] = nextGiver
	*byID[receiver.ID] = nextReceiver
	service.recordTradeDialogueBestEffort(
		ctx,
		state,
		byID,
		giver,
		receiver,
		fmt.Sprintf("我刚和 %s 说定，把 %s 交给他。", receiver.DisplayName(), displayItemName(itemID)),
		fmt.Sprintf("%s 刚把 %s 交给我，让我自己去处理。", giver.DisplayName(), displayItemName(itemID)),
	)
	appendLog(
		state,
		"trade",
		fmt.Sprintf("我把 %s 交给了 %s。", displayItemName(itemID), receiver.DisplayName()),
		giver.ID,
		receiver.ID,
	)
	return true
}

// resolveRequestedPurchase 尝试按上下文解析并执行“用金币购买物品”交易。
func (service *Service) resolveRequestedPurchase(
	ctx context.Context,
	state *State,
	trades *unit.TradeService,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	contextText string,
) bool {
	itemID, ok := requestedTradeItem(contextText)
	if !ok {
		return false
	}
	if !containsAny(contextText, "买", "购买", "花钱", "出钱", displayItemName(itemID)) {
		return false
	}

	buyer, seller := resolvePurchaseDirection(left, right, itemID)
	if buyer == nil || seller == nil {
		return false
	}

	definition, found := item.Lookup(itemID)
	if !found {
		return false
	}

	nextBuyer, nextSeller, err := trades.PurchaseItem(ctx, buyer.ID, seller.ID, itemID, definition.Price)
	if err != nil {
		appendLog(
			state,
			"trade_blocked",
			fmt.Sprintf("我想向 %s 买下 %s，这回合没谈成。", seller.DisplayName(), definition.DisplayName),
			buyer.ID,
			seller.ID,
		)
		return true
	}

	*byID[buyer.ID] = nextBuyer
	*byID[seller.ID] = nextSeller
	service.recordTradeDialogueBestEffort(
		ctx,
		state,
		byID,
		buyer,
		seller,
		fmt.Sprintf("我刚和 %s 谈妥，用钱买下了 %s。", seller.DisplayName(), definition.DisplayName),
		fmt.Sprintf("%s 刚来和我谈，已经出钱买走了 %s。", buyer.DisplayName(), definition.DisplayName),
	)
	appendLog(
		state,
		"trade",
		fmt.Sprintf("我花 %d 金向 %s 买下了 %s。", definition.Price, seller.DisplayName(), definition.DisplayName),
		buyer.ID,
		seller.ID,
	)

	// 锚自动 upsert（设计 耦合 §1.3）：交易成交即把买卖双方互为 debt_grudge_love 锚——一桩买卖结下的人情/关系
	// 是她日后会惦记的弦，喂 relevance.Score。best-effort：吞错只记日志，绝不阻断交易主链路（已成交）。
	service.bestEffortLitTradeDebtAnchor(ctx, state, buyer, seller, definition.DisplayName)
	return true
}

// tradeDebtAnchorWeight 是交易债务锚的默认权重（一桩成交结下的关系，权重中性偏轻——非满权的红线/血脉）。
const tradeDebtAnchorWeight = 0.5

// bestEffortLitTradeDebtAnchor 为成交的买卖双方互落一根 debt_grudge_love 锚（双向）。best-effort：任一方失败只记日志，
// 绝不回滚已成交的交易、不阻断主链路。
func (service *Service) bestEffortLitTradeDebtAnchor(ctx context.Context, state *State, buyer, seller *unit.Record, itemName string) {
	if service == nil || buyer == nil || seller == nil {
		return
	}
	buyerLabel := fmt.Sprintf("与 %s 的交易（购入%s）", seller.DisplayName(), itemName)
	sellerLabel := fmt.Sprintf("与 %s 的交易（售出%s）", buyer.DisplayName(), itemName)
	if err := service.UpsertDebtAnchor(ctx, state.ID, buyer.ID, seller.ID, tradeDebtAnchorWeight, buyerLabel); err != nil {
		appendLog(state, "trade_debt_anchor_failed", fmt.Sprintf("%s 的交易债务锚落定失败：%v", buyer.DisplayName(), err), buyer.ID, seller.ID)
	}
	if err := service.UpsertDebtAnchor(ctx, state.ID, seller.ID, buyer.ID, tradeDebtAnchorWeight, sellerLabel); err != nil {
		appendLog(state, "trade_debt_anchor_failed", fmt.Sprintf("%s 的交易债务锚落定失败：%v", seller.DisplayName(), err), seller.ID, buyer.ID)
	}
}

// resolveRequestedSwap 尝试按上下文执行“双方互换首个可交易物品”。
func (service *Service) resolveRequestedSwap(
	ctx context.Context,
	state *State,
	trades *unit.TradeService,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	contextText string,
) bool {
	if !containsAny(contextText, "换", "互换", "交换") {
		return false
	}

	leftItemID := firstTradeableItem(*left)
	rightItemID := firstTradeableItem(*right)
	if leftItemID == "" || rightItemID == "" || leftItemID == rightItemID {
		return false
	}

	nextLeft, nextRight, err := trades.SwapItems(ctx, left.ID, right.ID, leftItemID, rightItemID)
	if err != nil {
		appendLog(
			state,
			"trade_blocked",
			fmt.Sprintf("我和 %s 想互换物品，这回合没谈成。", right.DisplayName()),
			left.ID,
			right.ID,
		)
		return true
	}

	*byID[left.ID] = nextLeft
	*byID[right.ID] = nextRight
	service.recordTradeDialogueBestEffort(
		ctx,
		state,
		byID,
		left,
		right,
		fmt.Sprintf("我刚和 %s 换了东西，我拿到了 %s。", right.DisplayName(), displayItemName(rightItemID)),
		fmt.Sprintf("我刚和 %s 互换了物资，我拿到了 %s。", left.DisplayName(), displayItemName(leftItemID)),
	)
	appendLog(
		state,
		"trade",
		fmt.Sprintf("我和 %s 互换了 %s 与 %s。", right.DisplayName(), displayItemName(leftItemID), displayItemName(rightItemID)),
		left.ID,
		right.ID,
	)
	return true
}

// resolveRequestedGoldTransfer 尝试按上下文执行“富方给穷方调拨金币”。
func (service *Service) resolveRequestedGoldTransfer(
	ctx context.Context,
	state *State,
	trades *unit.TradeService,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	contextText string,
) bool {
	if !containsAny(contextText, "金币", "钱", "路费", "经费", "拨", "补给") {
		return false
	}

	from, to := richerUnit(left, right)
	if from == nil || to == nil || from.Status.Wallet-to.Status.Wallet < 20 {
		return false
	}

	amount := 20
	nextFrom, nextTo, err := trades.TransferGold(ctx, from.ID, to.ID, amount)
	if err != nil {
		appendLog(
			state,
			"trade_blocked",
			fmt.Sprintf("我想给 %s 调拨金币，这回合没谈成。", to.DisplayName()),
			from.ID,
			to.ID,
		)
		return true
	}

	*byID[from.ID] = nextFrom
	*byID[to.ID] = nextTo
	service.recordTradeDialogueBestEffort(
		ctx,
		state,
		byID,
		from,
		to,
		fmt.Sprintf("我刚给 %s 拨了 %d 金，让他自己补缺物资。", to.DisplayName(), amount),
		fmt.Sprintf("%s 刚拨给我 %d 金，我得自己把缺口补上。", from.DisplayName(), amount),
	)
	appendLog(
		state,
		"trade",
		fmt.Sprintf("我向 %s 调拨了 %d 金。", to.DisplayName(), amount),
		from.ID,
		to.ID,
	)
	return true
}

// requestedTradeItem 根据关键词信号从上下文中提取被提及的目标物品。
func requestedTradeItem(contextText string) (string, bool) {
	for _, signal := range deploymentTradeSignals {
		if containsAny(contextText, signal.keywords...) {
			return signal.itemID, true
		}
	}
	return "", false
}

// resolveItemDirection 根据物品归属决定赠与方向：有货的一方为 giver，另一方为 receiver。
func resolveItemDirection(left *unit.Record, right *unit.Record, itemID string) (*unit.Record, *unit.Record) {
	leftHas := hasItem(*left, itemID)
	rightHas := hasItem(*right, itemID)
	switch {
	case leftHas && !rightHas:
		return left, right
	case rightHas && !leftHas:
		return right, left
	default:
		return nil, nil
	}
}

// resolvePurchaseDirection 复用物品归属推导买卖方向，返回 buyer 与 seller。
func resolvePurchaseDirection(left *unit.Record, right *unit.Record, itemID string) (*unit.Record, *unit.Record) {
	seller, buyer := resolveItemDirection(left, right, itemID)
	if seller == nil || buyer == nil {
		return nil, nil
	}
	return buyer, seller
}

// richerUnit 返回钱包更多的一方及更少的一方，用于金币调拨场景。
func richerUnit(left *unit.Record, right *unit.Record) (*unit.Record, *unit.Record) {
	switch {
	case left.Status.Wallet > right.Status.Wallet:
		return left, right
	case right.Status.Wallet > left.Status.Wallet:
		return right, left
	default:
		return nil, nil
	}
}

// hasItem 判断单位装备栏/背包中是否持有指定物品。
func hasItem(record unit.Record, itemID string) bool {
	for _, stack := range record.Inventory.Equipment {
		if stack.ItemID == itemID {
			return true
		}
	}
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == itemID {
			return true
		}
	}
	return false
}

// firstTradeableItem 返回单位首个可用于交换的物品 ID（优先背包，再看装备）。
func firstTradeableItem(record unit.Record) string {
	if len(record.Inventory.Backpack) > 0 {
		return record.Inventory.Backpack[0].ItemID
	}
	for _, stack := range record.Inventory.Equipment {
		if stack.ItemID != "" {
			return stack.ItemID
		}
	}
	return ""
}

// containsAny 判断文本中是否包含任一候选关键词（默认按小写匹配）。
func containsAny(text string, candidates ...string) bool {
	for _, candidate := range candidates {
		if candidate != "" && strings.Contains(text, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}

// displayItemName 读取物品展示名；查不到定义时回退到原始 itemID。
func displayItemName(itemID string) string {
	definition, ok := item.Lookup(itemID)
	if !ok {
		return itemID
	}
	return definition.DisplayName
}

// buildDeploymentCandidates 为两名单位构建交易候选集合。
// 候选覆盖观望、赠与、购买、互换和金币调拨，供后续 AI 选择。
func buildDeploymentCandidates(left unit.Record, right unit.Record) []deploymentCandidate {
	candidates := []deploymentCandidate{{
		ID:      "hold",
		Kind:    "hold",
		Summary: "暂时不成交，各自先观望。",
	}}

	appendGiftCandidates := func(from unit.Record, to unit.Record) {
		for _, itemID := range uniqueTradeableItems(from) {
			candidates = append(candidates, deploymentCandidate{
				ID:           fmt.Sprintf("gift:%s:%s:%s", from.ID, to.ID, itemID),
				Kind:         "gift",
				ActorUnitID:  from.ID,
				TargetUnitID: to.ID,
				ItemID:       itemID,
				Summary:      fmt.Sprintf("%s 把 %s 送给 %s（用途：%s）", from.DisplayName(), displayItemName(itemID), to.DisplayName(), formatItemEffectByID(itemID)),
			})
		}
	}
	appendGiftCandidates(left, right)
	appendGiftCandidates(right, left)

	appendPurchaseCandidates := func(buyer unit.Record, seller unit.Record) {
		for _, itemID := range uniqueTradeableItems(seller) {
			definition, found := item.Lookup(itemID)
			if !found {
				continue
			}
			for _, price := range purchasePriceOptions(buyer, seller, definition.Price) {
				if buyer.Status.Wallet < price {
					continue
				}
				candidates = append(candidates, deploymentCandidate{
					ID:           fmt.Sprintf("purchase:%s:%s:%s:%d", buyer.ID, seller.ID, itemID, price),
					Kind:         "purchase",
					ActorUnitID:  buyer.ID,
					TargetUnitID: seller.ID,
					ItemID:       itemID,
					Price:        price,
					Summary:      fmt.Sprintf("%s 出价 %d 金向 %s 买 %s（用途：%s）", buyer.DisplayName(), price, seller.DisplayName(), definition.DisplayName, formatItemEffectByID(itemID)),
				})
			}
		}
	}
	appendPurchaseCandidates(left, right)
	appendPurchaseCandidates(right, left)

	leftItemID := firstTradeableItem(left)
	rightItemID := firstTradeableItem(right)
	if leftItemID != "" && rightItemID != "" && leftItemID != rightItemID {
		candidates = append(candidates, deploymentCandidate{
			ID:           fmt.Sprintf("swap:%s:%s", leftItemID, rightItemID),
			Kind:         "swap",
			ActorUnitID:  left.ID,
			TargetUnitID: right.ID,
			ItemID:       leftItemID,
			OtherItemID:  rightItemID,
			Summary:      fmt.Sprintf("%s 与 %s 互换 %s（%s）和 %s（%s）", left.DisplayName(), right.DisplayName(), displayItemName(leftItemID), formatItemEffectByID(leftItemID), displayItemName(rightItemID), formatItemEffectByID(rightItemID)),
		})
	}

	from, to := richerUnit(&left, &right)
	if from != nil && to != nil && from.Status.Wallet-to.Status.Wallet >= 20 {
		candidates = append(candidates, deploymentCandidate{
			ID:           fmt.Sprintf("gold:%s:%s:20", from.ID, to.ID),
			Kind:         "gold",
			ActorUnitID:  from.ID,
			TargetUnitID: to.ID,
			GoldAmount:   20,
			Summary:      fmt.Sprintf("%s 给 %s 调拨 20 金", from.DisplayName(), to.DisplayName()),
		})
	}

	return candidates
}

// uniqueTradeableItems 提取单位可交易物品去重列表（背包+装备）。
func uniqueTradeableItems(record unit.Record) []string {
	seen := map[string]struct{}{}
	items := make([]string, 0, len(record.Inventory.Backpack)+len(record.Inventory.Equipment))
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == "" || stack.Quantity <= 0 {
			continue
		}
		if _, ok := seen[stack.ItemID]; ok {
			continue
		}
		seen[stack.ItemID] = struct{}{}
		items = append(items, stack.ItemID)
	}
	for _, stack := range record.Inventory.Equipment {
		if stack.ItemID == "" {
			continue
		}
		if _, ok := seen[stack.ItemID]; ok {
			continue
		}
		seen[stack.ItemID] = struct{}{}
		items = append(items, stack.ItemID)
	}
	return items
}

// purchasePriceOptions 生成购买报价档位；跨阵营提供低/中/高三档，同阵营仅基础价。
func purchasePriceOptions(buyer unit.Record, seller unit.Record, basePrice int) []int {
	if basePrice <= 0 {
		return nil
	}
	if buyer.FactionID == seller.FactionID {
		return []int{basePrice}
	}

	lowPrice := basePrice * 8 / 10
	if lowPrice < 1 {
		lowPrice = 1
	}
	highPrice := (basePrice*12 + 9) / 10
	if highPrice < 1 {
		highPrice = 1
	}

	options := make([]int, 0, 3)
	seen := map[int]struct{}{}
	for _, price := range []int{lowPrice, basePrice, highPrice} {
		if price <= 0 {
			continue
		}
		if _, ok := seen[price]; ok {
			continue
		}
		seen[price] = struct{}{}
		options = append(options, price)
	}
	return options
}

// appendDeploymentDialogue 把交易选择生成的双方台词写入对话历史，并追加汇总日志。
func appendDeploymentDialogue(
	state *State,
	left *unit.Record,
	right *unit.Record,
	choice deploymentChoicePayload,
	result ai.CompletionResult,
) {
	if state == nil || left == nil || right == nil {
		return
	}

	if choice.LeftLine != "" {
		appendAIDialogue(state, *left, choice.LeftLine, result)
	}
	if choice.RightLine != "" {
		appendAIDialogue(state, *right, choice.RightLine, result)
	}
	appendLog(
		state,
		"unit_dialogue",
		deploymentDialogueSummary(choice, left, right),
		left.ID,
		right.ID,
	)
}

// rememberDeploymentMemories 把交易后双方记忆写入记忆系统，作为后续决策上下文。
func rememberDeploymentMemories(service *Service, ctx context.Context, state *State, left *unit.Record, right *unit.Record, choice deploymentChoicePayload) {
	if service == nil || state == nil {
		return
	}
	service.rememberUnitBestEffort(ctx, left, state.TurnState.Turn, choice.LeftMemory)
	service.rememberUnitBestEffort(ctx, right, state.TurnState.Turn, choice.RightMemory)
}

// deploymentChoiceNarrative 统一提取交易选择的叙述文本，按 Summary/台词/Reasoning/Memory 依次回退。
func deploymentChoiceNarrative(choice deploymentChoicePayload) string {
	if summary := strings.TrimSpace(choice.Summary); summary != "" {
		return summary
	}
	leftLine := strings.TrimSpace(choice.LeftLine)
	rightLine := strings.TrimSpace(choice.RightLine)
	switch {
	case leftLine != "" && rightLine != "":
		return fmt.Sprintf("%s；%s", leftLine, rightLine)
	case leftLine != "":
		return leftLine
	case rightLine != "":
		return rightLine
	}
	if reasoning := strings.TrimSpace(choice.Reasoning); reasoning != "" {
		return reasoning
	}
	leftMemory := strings.TrimSpace(choice.LeftMemory)
	rightMemory := strings.TrimSpace(choice.RightMemory)
	switch {
	case leftMemory != "" && rightMemory != "":
		return fmt.Sprintf("%s；%s", leftMemory, rightMemory)
	case leftMemory != "":
		return leftMemory
	case rightMemory != "":
		return rightMemory
	default:
		return ""
	}
}

// deploymentDialogueSummary 生成“单位对话日志”中的摘要文本。
func deploymentDialogueSummary(choice deploymentChoicePayload, left *unit.Record, right *unit.Record) string {
	summary := deploymentChoiceNarrative(choice)
	if summary != "" {
		return summary
	}
	return "我们先把话说开，再自己处理这笔交易。"
}

// deploymentTradeSuccessMessage 生成交易成功日志文案，优先复用模型给出的叙事摘要。
func deploymentTradeSuccessMessage(
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
	return fallbackDeploymentTradeSummary(candidate, byID)
}

// fallbackDeploymentTradeSummary 在缺失模型摘要时，按交易类型拼接基础说明句。
func fallbackDeploymentTradeSummary(candidate deploymentCandidate, byID map[string]*unit.Record) string {
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
