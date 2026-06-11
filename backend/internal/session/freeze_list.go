package session

// 文件说明：Freeze List —— 高代价离线动作（卖传家宝/叛变/不可逆大额处置）的「冻结上交」判定与构造。
// 玩家不在场时单位据离线宪章自治，但有一类「覆水难收」的动作绝不能直接落地：它们必须被**冻结**、
// 自动上交命运待决策（PENDING_DECISION），等玩家回来拿主意。本文件提供：
//   - shouldFreezeAction：纯函数判定一个离线动作是否该被冻结，依据 ① 宪章红线（Redlines）② 社交授权
//     （SocialMandates，「勿与某派结仇」等显式禁令）③ 物品 Pinned 标记（传家宝绝不自动卖）④ GateSurprise 的
//     freeze 裁决（卖锚物无压力 / 叛变等高代价转折）。
//   - buildFreezeFateEvent：把一个被冻结的动作构造成「不可逆类」的 FateEvent（高重要度 → 命运层路由进待决策）。
//   - Service.FreezeAndSurrenderToFate：导出辅助，把冻结动作上交命运收件箱（经 SurfaceFateEvent），并写
//     FREEZE_INTERCEPT 流程留痕。**flag-gated**（QUNXIANG_FREEZE_LIST，默认关 → no-op 兜底），best-effort。
//
// 铁律遵循：纯逻辑判定确定性、无全局随机；不直接改保护状态字段（仅经既有 SurfaceFateEvent / EmitProcessEvent 留痕）；
// 新增 JSON 无（复用既有结构）；FreezeAndSurrenderToFate 不接主循环——拦截接线（动作落地前调用 + 转收件箱）由 Wire 阶段做。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
)

// freezeListEnabled 读 QUNXIANG_FREEZE_LIST，默认开（显式 false/0/no/off 可关 → FreezeAndSurrenderToFate 整段
// no-op、零行为变化、零 DB 写）。判定函数 shouldFreezeAction 本身是纯逻辑、不读 flag（供 Wire 在拦截点按需调用）。
func freezeListEnabled() bool {
	return featureflags.EnabledWithDefault("QUNXIANG_FREEZE_LIST", true)
}

// FreezeReason 标识一个动作被冻结的依据类别（供留痕/前端文案区分「凭什么拦」）。
type FreezeReason string

// 常量定义区：冻结依据的四类来源。
const (
	FreezeReasonNone           FreezeReason = ""                // 不冻结
	FreezeReasonPinnedItem     FreezeReason = "pinned_item"     // 物品 Pinned 标记（传家宝绝不自动卖/赠）
	FreezeReasonCharterRedline FreezeReason = "charter_redline" // 触碰离线宪章红线
	FreezeReasonSocialMandate  FreezeReason = "social_mandate"  // 违背社交授权显式禁令（勿与某派结仇等）
	FreezeReasonGateSurprise   FreezeReason = "gate_surprise"   // GateSurprise 判定须上交玩家（freeze）
)

// FreezeDecision 是 shouldFreezeAction 的判定结果：是否冻结、依据类别、可读理由（命运卡/留痕用）。
type FreezeDecision struct {
	Freeze    bool
	Reason    FreezeReason
	Detail    string               // 命中的红线原文 / 社交禁令 / 门控 reason，供命运卡与 FREEZE_INTERCEPT 留痕
	GatedKind decision.GatedAction // 若由 GateSurprise 触发，记下门控动作类别（romance/sell_pinned/defect）
}

// FreezeAction 是一个待判定的「离线自治动作」描述（纯数据，不依赖 LLM payload 类型，便于 Wire 在多处构造）。
// 由 Wire 阶段在动作落地前从 unitDecisionChoicePayload / 实际动作上下文填充。
type FreezeAction struct {
	Kind            decision.GatedAction // 动作的门控类别（romance/sell_pinned/defect；空=非门控动作）
	ItemID          string               // 处置标的物品 ID（卖/赠/弃时）
	ItemName        string               // 标的物品名（目录外具名遗物）
	ItemPinned      bool                 // 标的物品当前是否带 Pinned 标记（由 Wire 从单位库存读出后填入）
	TargetFactionID string               // 动作指向的对方阵营（叛变/结盟/结仇时，用于比对 SocialMandates）
	// Intent 是该动作声明的自然语言意图（next_action / reasoning 摘要），用于与红线/社交禁令做关键词比对。
	Intent string
	// IsHighStakesDisposal 标记「不可逆大额处置」（大额转账/销毁独有资产等），即便不是 Pinned 也须冻结上交。
	IsHighStakesDisposal bool
}

// shouldFreezeAction 判定一个离线自治动作是否该被冻结、自动上交命运待决策。返回 true 即「绝不直接落地」。
// 纯函数、确定性、无 DB、无随机：仅看动作描述 + 该单位的宪章/库存快照。判定优先级（任一命中即冻结）：
//  1. 物品 Pinned 硬门：标的物品带 Pinned 标记（传家宝）→ 绝不自动卖/赠/弃。
//  2. 不可逆大额处置：IsHighStakesDisposal=true（销毁独有资产/大额转账）→ 上交玩家。
//  3. 宪章红线：动作意图/标的触碰任一条 Redlines（按红线原文做关键词包含比对）→ 冻结。
//  4. 社交授权禁令：SocialMandates 里的「勿/不可/禁止 …」类显式禁令被该动作违背 → 冻结。
//  5. GateSurprise freeze 裁决：把动作类别 + 前因喂 GateSurprise，裁决为 GateFreeze（如无压力卖锚物）→ 冻结。
//
// 注意：GateReject（前因不足，从候选剔除）不在本函数职责内——那是「不该发生、丢弃」，由决策链处理；
// 本函数只负责「该发生但太重、不能擅自落地、上交玩家」的冻结一类。
func shouldFreezeAction(charter OfflineCharter, action FreezeAction) FreezeDecision {
	// 1) Pinned 硬门：传家宝绝不自动处置。
	if action.ItemPinned && isDisposalKind(action.Kind) {
		return FreezeDecision{
			Freeze:    true,
			Reason:    FreezeReasonPinnedItem,
			Detail:    freezeItemLabel(action),
			GatedKind: action.Kind,
		}
	}

	// 2) 不可逆大额处置：即便非 Pinned，也不能擅自销毁/挥霍独有资产。
	if action.IsHighStakesDisposal {
		return FreezeDecision{
			Freeze:    true,
			Reason:    FreezeReasonPinnedItem, // 复用 pinned_item 语义（不可逆处置同属「资产硬门」）
			Detail:    freezeItemLabel(action),
			GatedKind: action.Kind,
		}
	}

	// 3) 宪章红线：动作意图/标的触碰任一条红线。
	if text, ok := charterRedlineHit(charter, action); ok {
		return FreezeDecision{
			Freeze:    true,
			Reason:    FreezeReasonCharterRedline,
			Detail:    text,
			GatedKind: action.Kind,
		}
	}

	// 4) 社交授权显式禁令：动作违背 SocialMandates 里的「勿/不可/禁止」类条目。
	if text, ok := socialMandateProhibits(charter, action); ok {
		return FreezeDecision{
			Freeze:    true,
			Reason:    FreezeReasonSocialMandate,
			Detail:    text,
			GatedKind: action.Kind,
		}
	}

	// 5) GateSurprise freeze 裁决（如无债务/威胁压力的卖锚物 → SELL_PINNED_NEEDS_PLAYER）。
	if action.Kind != "" {
		gate := decision.GateSurprise(action.Kind, freezeGateInput(action))
		if gate.Decision == decision.GateFreeze {
			return FreezeDecision{
				Freeze:    true,
				Reason:    FreezeReasonGateSurprise,
				Detail:    gate.Reason,
				GatedKind: action.Kind,
			}
		}
	}

	return FreezeDecision{Freeze: false, Reason: FreezeReasonNone}
}

// isDisposalKind 判断动作类别是否属于「处置标的物品」一类（卖/赠/弃才适用 Pinned 硬门）。
// sell_pinned 是变卖/赠出锚物的门控类别；其它（romance/defect）不处置物品，Pinned 标记对其无意义。
func isDisposalKind(kind decision.GatedAction) bool {
	return kind == decision.ActionSellPinned
}

// freezeGateInput 从 FreezeAction 构造 decision.GateInput（仅填本函数可定的字段；关系/忠诚等前因由 Wire
// 在有 DB 时通过另一路径补齐，本纯函数判定保守——缺前因时 GateSurprise 更倾向 freeze/reject，符合红线）。
func freezeGateInput(action FreezeAction) decision.GateInput {
	in := decision.GateInput{}
	// sell_pinned：标的是否父辈遗志类永久锚（目录外具名遗物）。注意——这里只看「是否 Pinned/具名遗物」，
	// 债务/威胁压力由 Wire 在有单位快照时补（默认无压力 → GateSurprise 判 freeze 上交玩家，最保守）。
	if action.Kind == decision.ActionSellPinned {
		in.ItemIsPermanentAnchor = action.ItemPinned
	}
	return in
}

// charterRedlineHit 判断动作是否触碰宪章里的任一条红线（按红线原文与动作意图/标的做关键词包含双向比对）。
// 返回命中的红线原文与 true。确定性、无随机。
func charterRedlineHit(charter OfflineCharter, action FreezeAction) (string, bool) {
	haystack := freezeActionHaystack(action)
	if haystack == "" {
		return "", false
	}
	for _, redline := range charter.Redlines {
		text := strings.TrimSpace(redline.Text)
		if text == "" {
			continue
		}
		if redlineMatchesAction(text, haystack) {
			return text, true
		}
	}
	return "", false
}

// socialMandateProhibits 判断动作是否违背 SocialMandates 里的「显式禁令」（含「勿/不可/不准/禁止/切勿/别」等否定词）。
// 只对带否定词的授权条目做禁令比对（「可代我结盟」类正向授权不触发冻结）；命中返回禁令原文与 true。
func socialMandateProhibits(charter OfflineCharter, action FreezeAction) (string, bool) {
	haystack := freezeActionHaystack(action)
	if haystack == "" {
		return "", false
	}
	for _, mandate := range charter.SocialMandates {
		text := strings.TrimSpace(mandate)
		if text == "" || !mandateIsProhibition(text) {
			continue
		}
		if redlineMatchesAction(text, haystack) {
			return text, true
		}
	}
	return "", false
}

// freezeProhibitionMarkers 是社交授权条目里标识「这是一条禁令」的否定词。
var freezeProhibitionMarkers = []string{"勿", "不可", "不准", "不得", "禁止", "切勿", "别", "莫", "严禁"}

// mandateIsProhibition 判断一条社交授权是否为「禁令」（含否定词）。正向授权（「可…」「允许…」）返回 false。
func mandateIsProhibition(text string) bool {
	return containsAny(text, freezeProhibitionMarkers...)
}

// redlineMatchesAction 判断一条红线/禁令原文与动作上下文（haystack）是否相关。中文无空格分词，故用**双向 2-gram 重合**：
// 把红线原文切成有意义词块，对每个词块取其连续 2-rune 片段集，只要任一 2-gram 出现在动作上下文里即视为命中
// （反之「东海派结仇」与动作里的「东海派」靠共享 2-gram「东海/海派」即可相关，无需精确同词块）。
// 这是保守启发式（宁可多拦上交玩家，不让 LLM 擅自越线）；确定性、无随机。
func redlineMatchesAction(rule string, haystack string) bool {
	if strings.TrimSpace(haystack) == "" {
		return false
	}
	for _, token := range freezeMeaningfulTokens(rule) {
		// 短词块（恰 2 rune）直接整块比对；更长词块拆成 2-gram 逐一比对（容忍未分词的「东海派结仇」）。
		for _, gram := range bigrams(token) {
			if strings.Contains(haystack, gram) {
				return true
			}
		}
	}
	return false
}

// bigrams 返回字符串所有连续 2-rune 片段（长度 <2 时返回其自身、长度恰 2 时返回单元素切片）。确定性。
func bigrams(s string) []string {
	runes := []rune(s)
	if len(runes) < 2 {
		if len(runes) == 0 {
			return nil
		}
		return []string{s}
	}
	out := make([]string, 0, len(runes)-1)
	for i := 0; i+2 <= len(runes); i++ {
		out = append(out, string(runes[i:i+2]))
	}
	return out
}

// freezeMeaningfulTokens 从红线/禁令原文里提取「有意义的词块」用于比对：先按标点/否定词/常见连接词切分，
// 再剔除过短（<2 字/字符）与纯否定词的碎片。确定性。
func freezeMeaningfulTokens(rule string) []string {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return nil
	}
	// 切分符：标点 + 否定词 + 常见连接动词 + 常见关系动作短语（把「绝不与东海派结仇」切出「东海派」）。
	separators := []string{
		"，", "。", "、", "；", "：", "！", "？", " ", ",", ".", ";", ":", "!", "?",
		"绝不", "切勿", "禁止", "不准", "不得", "不可", "严禁",
		"结仇", "结盟", "投靠", "归顺", "背叛", "出卖", "变卖", "卖给", "送给",
		"勿", "莫", "别", "与", "和", "向", "对", "把", "将",
	}
	parts := []string{rule}
	for _, sep := range separators {
		next := make([]string, 0, len(parts))
		for _, p := range parts {
			next = append(next, strings.Split(p, sep)...)
		}
		parts = next
	}
	tokens := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// 至少 2 个 rune 才算有意义词块（单字噪声多、易误命中）。
		if runeLen(p) < 2 || seen[p] {
			continue
		}
		seen[p] = true
		tokens = append(tokens, p)
	}
	return tokens
}

// freezeActionHaystack 把动作的可比对文本（意图 + 标的物品名/ID + 目标阵营）拼成一个小写检索串。
func freezeActionHaystack(action FreezeAction) string {
	parts := []string{
		strings.TrimSpace(action.Intent),
		strings.TrimSpace(action.ItemName),
		strings.TrimSpace(action.ItemID),
		strings.TrimSpace(action.TargetFactionID),
	}
	joined := strings.Join(parts, " ")
	return strings.ToLower(strings.TrimSpace(joined))
}

// freezeItemLabel 返回标的物品的可读标签（优先具名，回落 ID，再回落通用「一件传家之物」）。
func freezeItemLabel(action FreezeAction) string {
	if name := strings.TrimSpace(action.ItemName); name != "" {
		return name
	}
	if id := strings.TrimSpace(action.ItemID); id != "" {
		return id
	}
	return "一件不可轻易处置之物"
}

// runeLen 返回字符串的 rune 数（中文按字计），用于词块长度阈值判断。
func runeLen(s string) int {
	return len([]rune(s))
}

// --- 冻结 → 命运待决策的构造与上交 ---

// buildFreezeFateEvent 把一个被冻结的动作构造成「不可逆类」FateEvent：以 owner 自身为 actor/target（这是她自己
// 正要做的事），重要度取高位 → 经命运层 fateScore 必走 RoutePending（进收件箱待玩家拿主意）。Summary/归因句据
// 冻结依据生成，便于命运卡说清「她正要做什么、凭什么被拦下」。纯函数、确定性。
func buildFreezeFateEvent(ownerID string, action FreezeAction, decisionRes FreezeDecision) FateEvent {
	return FateEvent{
		ActorID:       ownerID,
		TargetID:      ownerID,
		ReasonCode:    events.ReasonFreezeIntercept,
		Importance:    9,    // 高位 → 不可逆度 + 自身相关度齐高，命运层必路由进待决策
		EmotionWeight: -0.5, // 高代价转折带负向张力（要卖掉传家宝/要叛变…）
		Summary:       freezeSummary(action, decisionRes),
		AttributionZH: freezeAttribution(decisionRes),
	}
}

// freezeSummary 生成命运卡正文：「她正要 X，但 Y」。X 据动作类别，Y 据冻结依据。
func freezeSummary(action FreezeAction, res FreezeDecision) string {
	var intent string
	switch action.Kind {
	case decision.ActionSellPinned:
		intent = "她正打算把「" + freezeItemLabel(action) + "」变卖出手"
	case decision.ActionDefect:
		intent = "她动了叛投他方的念头"
	case decision.ActionRomance:
		intent = "她正要做出一桩牵动感情的决定"
	default:
		if action.IsHighStakesDisposal {
			intent = "她正要做一桩覆水难收的处置"
		} else {
			intent = "她正要做一桩你不在场时不该擅自定夺的事"
		}
	}
	return intent + "，这件事在等你回来拿个主意。"
}

// freezeAttribution 生成命运卡的「凭什么被拦」一句（来自冻结依据）。
func freezeAttribution(res FreezeDecision) string {
	switch res.Reason {
	case FreezeReasonPinnedItem:
		return "这是绝不可随手处置之物"
	case FreezeReasonCharterRedline:
		if d := strings.TrimSpace(res.Detail); d != "" {
			return "这触到了你立下的红线：" + d
		}
		return "这触到了你立下的红线"
	case FreezeReasonSocialMandate:
		if d := strings.TrimSpace(res.Detail); d != "" {
			return "这违背了你的嘱托：" + d
		}
		return "这违背了你的社交嘱托"
	case FreezeReasonGateSurprise:
		return "这是覆水难收的大事，不该由她擅自定夺"
	default:
		return ""
	}
}

// FreezeAndSurrenderToFate 把一个被冻结的离线动作上交命运收件箱：构造不可逆类 FateEvent 经 SurfaceFateEvent
// 路由进待决策，并写 FREEZE_INTERCEPT 流程留痕。返回命运路由结果（含待决策 ID，供调用方关联拦截上下文）。
//
// **flag-gated**：QUNXIANG_FREEZE_LIST 未开 → 直接 no-op 返回（zero 值 + nil），零行为变化、零 DB 写。
// **best-effort 语义由调用方掌握**：本函数返回 error 供 Wire 决定是否吞错；Wire 在动作落地前调用——返回 freeze=true
// 时拦截动作（不落地）并改走本函数，绝不影响主循环结算。owner 缺失时返回错误（不静默吞，便于 Wire 排障）。
//
// 注意：调用方应先用 shouldFreezeAction 判定 freeze=true 再调本函数；本函数不重复判定（解耦：判定纯函数、
// 上交带副作用），但仍内部用传入的 FreezeDecision 渲染命运卡/留痕。
func (service *Service) FreezeAndSurrenderToFate(ctx context.Context, sessionID string, owner *unit.Record, action FreezeAction, res FreezeDecision) (FateRouting, error) {
	if !freezeListEnabled() {
		return FateRouting{}, nil // flag 关：no-op 兜底
	}
	if service == nil || service.db == nil || owner == nil {
		return FateRouting{}, fmt.Errorf("freeze surrender: missing dependencies")
	}
	ev := buildFreezeFateEvent(owner.ID, action, res)
	routing, err := service.SurfaceFateEvent(ctx, sessionID, owner, ev)
	if err != nil {
		return routing, err
	}
	// FREEZE_INTERCEPT 流程留痕（旁路事件，不改保护状态）：记下「拦了什么、凭什么拦、关联的待决策 ID」，
	// 供复盘/回响与前端「她正要做的事被拦下了」提示。best-effort——留痕失败不影响已成功的命运路由。
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:     sessionID,
		OwnerUnitID:   owner.ID,
		RelatedUnitID: owner.ID,
		Code:          events.ReasonFreezeIntercept,
		Category:      events.CategoryLifecycle,
		Payload: map[string]any{
			"freeze_reason": string(res.Reason),
			"detail":        res.Detail,
			"gated_kind":    string(res.GatedKind),
			"item_id":       action.ItemID,
			"item_name":     action.ItemName,
			"decision_id":   routing.DecisionID,
			"route":         string(routing.Route),
		},
	})
	return routing, nil
}
