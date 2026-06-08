package session

// 文件说明：Freeze List 判定的聚焦单测——验证 ① Pinned 硬门 ② 不可逆大额处置 ③ 宪章红线命中
// ④ 社交授权显式禁令 ⑤ GateSurprise freeze 裁决 各路径冻结正确，普通动作不被误冻，
// 以及命运事件构造（buildFreezeFateEvent）的不可逆/路由属性。纯函数测试，无 DB。

import (
	"testing"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
)

// --- Pinned 硬门 ---

func TestShouldFreeze_PinnedItemSell(t *testing.T) {
	action := FreezeAction{
		Kind:       decision.ActionSellPinned,
		ItemID:     "mother_locket",
		ItemName:   "母亲的吊坠",
		ItemPinned: true,
	}
	res := shouldFreezeAction(OfflineCharter{}, action)
	if !res.Freeze || res.Reason != FreezeReasonPinnedItem {
		t.Fatalf("带 Pinned 标记的变卖应冻结(pinned_item)，得到 %+v", res)
	}
	if res.Detail != "母亲的吊坠" {
		t.Fatalf("冻结详情应为物品名，得到 %q", res.Detail)
	}
}

// Pinned 标记只对「处置类」动作生效；叛变(defect)不处置物品，Pinned 不应触发 pinned_item 分支。
func TestShouldFreeze_PinnedIrrelevantForNonDisposal(t *testing.T) {
	action := FreezeAction{Kind: decision.ActionDefect, ItemPinned: true}
	res := shouldFreezeAction(OfflineCharter{}, action)
	if res.Reason == FreezeReasonPinnedItem {
		t.Fatalf("叛变不处置物品，不应走 pinned_item 分支，得到 %+v", res)
	}
}

// --- 不可逆大额处置 ---

func TestShouldFreeze_HighStakesDisposal(t *testing.T) {
	action := FreezeAction{IsHighStakesDisposal: true, ItemName: "祖宅地契"}
	res := shouldFreezeAction(OfflineCharter{}, action)
	if !res.Freeze {
		t.Fatalf("不可逆大额处置应冻结，得到 %+v", res)
	}
}

// --- 宪章红线命中 ---

func TestShouldFreeze_CharterRedlineHit(t *testing.T) {
	charter := OfflineCharter{
		Redlines: []CharterRedline{
			{ID: "rl_1", Text: "绝不投靠北境蛮族"},
		},
	}
	action := FreezeAction{
		Kind:            decision.ActionDefect,
		TargetFactionID: "北境蛮族",
		Intent:          "我决意投靠北境蛮族",
	}
	res := shouldFreezeAction(charter, action)
	if !res.Freeze || res.Reason != FreezeReasonCharterRedline {
		t.Fatalf("触碰红线应冻结(charter_redline)，得到 %+v", res)
	}
	if res.Detail != "绝不投靠北境蛮族" {
		t.Fatalf("冻结详情应为红线原文，得到 %q", res.Detail)
	}
}

// 红线不相关的动作不应被红线冻结。
func TestShouldFreeze_CharterRedlineMiss(t *testing.T) {
	charter := OfflineCharter{
		Redlines: []CharterRedline{{ID: "rl_1", Text: "绝不投靠北境蛮族"}},
	}
	action := FreezeAction{Kind: decision.ActionRomance, Intent: "向同营的伙伴表白"}
	res := shouldFreezeAction(charter, action)
	if res.Reason == FreezeReasonCharterRedline {
		t.Fatalf("与红线无关的动作不应被红线冻结，得到 %+v", res)
	}
}

// --- 社交授权显式禁令 ---

func TestShouldFreeze_SocialMandateProhibition(t *testing.T) {
	charter := OfflineCharter{
		SocialMandates: []string{
			"可代我结盟北境", // 正向授权，不应触发冻结
			"勿与东海派结仇", // 禁令
		},
	}
	action := FreezeAction{
		Intent:          "去挑衅东海派，与东海派结仇",
		TargetFactionID: "东海派",
	}
	res := shouldFreezeAction(charter, action)
	if !res.Freeze || res.Reason != FreezeReasonSocialMandate {
		t.Fatalf("违背社交禁令应冻结(social_mandate)，得到 %+v", res)
	}
}

// 正向授权（不含否定词）绝不触发社交禁令冻结。
func TestShouldFreeze_SocialMandatePositiveNotFrozen(t *testing.T) {
	charter := OfflineCharter{SocialMandates: []string{"可代我结盟北境"}}
	action := FreezeAction{Intent: "去与北境结盟"}
	res := shouldFreezeAction(charter, action)
	if res.Freeze {
		t.Fatalf("正向社交授权不应冻结，得到 %+v", res)
	}
}

// --- GateSurprise freeze 裁决 ---

func TestShouldFreeze_GateSurpriseSellNonPinnedNoPressure(t *testing.T) {
	// 非永久锚物品、无债务/威胁压力 → GateSurprise 判 SELL_PINNED_NEEDS_PLAYER（freeze 上交玩家）。
	action := FreezeAction{
		Kind:       decision.ActionSellPinned,
		ItemID:     "rare_jade",
		ItemName:   "稀世玉佩",
		ItemPinned: false, // 不是永久锚 → 不走 pinned_item 硬门，落到 GateSurprise
	}
	res := shouldFreezeAction(OfflineCharter{}, action)
	if !res.Freeze || res.Reason != FreezeReasonGateSurprise {
		t.Fatalf("无压力卖非锚高价物应由 GateSurprise 冻结，得到 %+v", res)
	}
	if res.Detail != "SELL_PINNED_NEEDS_PLAYER" {
		t.Fatalf("门控理由应为 SELL_PINNED_NEEDS_PLAYER，得到 %q", res.Detail)
	}
}

// --- 普通动作不冻结 ---

func TestShouldFreeze_OrdinaryActionsNotFrozen(t *testing.T) {
	charter := OfflineCharter{
		Redlines:       []CharterRedline{{ID: "rl_1", Text: "绝不投靠北境蛮族"}},
		SocialMandates: []string{"勿与东海派结仇"},
	}
	cases := []FreezeAction{
		{}, // 空动作
		{Kind: decision.ActionRomance, Intent: "巡逻营地"},
		{Intent: "采集木材补给"},
		{Intent: "向东侧高地移动"},
	}
	for i, c := range cases {
		res := shouldFreezeAction(charter, c)
		if res.Freeze {
			t.Fatalf("用例 %d 普通动作不应冻结，得到 %+v", i, res)
		}
	}
}

// --- 优先级：Pinned 硬门先于 GateSurprise ---

func TestShouldFreeze_PinnedTakesPriorityOverGate(t *testing.T) {
	// 即便目录外具名遗物会让 GateSurprise 判 reject(PINNED_PERMANENT)，Pinned 硬门应先命中 pinned_item。
	action := FreezeAction{
		Kind:       decision.ActionSellPinned,
		ItemID:     "father_blade_legacy",
		ItemName:   "父亲的断剑",
		ItemPinned: true,
	}
	res := shouldFreezeAction(OfflineCharter{}, action)
	if res.Reason != FreezeReasonPinnedItem {
		t.Fatalf("Pinned 硬门应优先于 GateSurprise，得到 %+v", res)
	}
}

// --- 词块提取 ---

func TestFreezeMeaningfulTokens(t *testing.T) {
	tokens := freezeMeaningfulTokens("绝不与东海派结仇")
	hasEastSea := false
	for _, tk := range tokens {
		if tk == "东海派" {
			hasEastSea = true
		}
		// 否定词碎片不应作为词块。
		if tk == "绝不" || tk == "与" || tk == "勿" {
			t.Fatalf("否定词/连接词不应作为词块，得到 %v", tokens)
		}
	}
	if !hasEastSea {
		t.Fatalf("应切出实质词块「东海派」，得到 %v", tokens)
	}
}

func TestMandateIsProhibition(t *testing.T) {
	if !mandateIsProhibition("勿与某派结仇") {
		t.Fatalf("含否定词应识别为禁令")
	}
	if mandateIsProhibition("可代我结盟北境") {
		t.Fatalf("正向授权不应识别为禁令")
	}
}

// --- 命运事件构造 ---

func TestBuildFreezeFateEvent_RoutesToPending(t *testing.T) {
	action := FreezeAction{Kind: decision.ActionSellPinned, ItemName: "母亲的吊坠", ItemPinned: true}
	res := FreezeDecision{Freeze: true, Reason: FreezeReasonPinnedItem, Detail: "母亲的吊坠"}
	ev := buildFreezeFateEvent("u1", action, res)

	if ev.ActorID != "u1" || ev.TargetID != "u1" {
		t.Fatalf("冻结事件 actor/target 应为 owner 自身，得到 actor=%q target=%q", ev.ActorID, ev.TargetID)
	}
	if ev.ReasonCode != events.ReasonFreezeIntercept {
		t.Fatalf("冻结事件 reason 应为 FREEZE_INTERCEPT，得到 %q", ev.ReasonCode)
	}
	// 不可逆类高重要度 → 命运层必路由进待决策（收件箱）。
	fateScore := fateIrreversibility(ev) * (float64(ev.Importance) / 10.0) * fateEmotionIntensity(ev)
	if relevance.RouteFor(fateScore) != relevance.RoutePending {
		t.Fatalf("冻结事件应路由进待决策，fateScore=%f route=%v", fateScore, relevance.RouteFor(fateScore))
	}
	if ev.Summary == "" {
		t.Fatalf("冻结事件应带命运卡正文")
	}
	if ev.AttributionZH == "" {
		t.Fatalf("冻结事件应带「凭什么被拦」归因句")
	}
}

func TestFreezeAttributionPerReason(t *testing.T) {
	cases := map[FreezeReason]bool{
		FreezeReasonPinnedItem:     true,
		FreezeReasonCharterRedline: true,
		FreezeReasonSocialMandate:  true,
		FreezeReasonGateSurprise:   true,
		FreezeReasonNone:           false,
	}
	for reason, wantNonEmpty := range cases {
		got := freezeAttribution(FreezeDecision{Reason: reason, Detail: "X"})
		if (got != "") != wantNonEmpty {
			t.Fatalf("reason=%q 的归因句非空预期=%v，得到 %q", reason, wantNonEmpty, got)
		}
	}
}

// --- flag 默认关：FreezeAndSurrenderToFate no-op 兜底 ---

func TestFreezeAndSurrenderToFate_FlagOffNoop(t *testing.T) {
	// QUNXIANG_FREEZE_LIST 未设（默认关）→ 即使 service/db 为 nil 也不应 panic、不报错、零行为。
	service := &Service{}
	routing, err := service.FreezeAndSurrenderToFate(
		t.Context(), "sess1", nil,
		FreezeAction{Kind: decision.ActionSellPinned},
		FreezeDecision{Freeze: true, Reason: FreezeReasonPinnedItem},
	)
	if err != nil {
		t.Fatalf("flag 关时应 no-op 不报错，得到 %v", err)
	}
	if routing.Route != "" || routing.DecisionID != "" {
		t.Fatalf("flag 关时应返回零值路由，得到 %+v", routing)
	}
}
