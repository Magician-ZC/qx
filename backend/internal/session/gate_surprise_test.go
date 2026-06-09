package session

// 文件说明：GateSurprise 硬前因门控接入决策链的聚焦测试——
// 验证戏剧性动作（恋爱/卖传家宝/叛变）的识别与门控判定、无前因即不放行、普通动作零影响。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/unit"
)

// --- gatedActionForChoice：戏剧性动作识别 ---

func TestGatedActionForChoice_Romance(t *testing.T) {
	choice := unitDecisionChoicePayload{Action: DecisionActionRomance, TargetUnitID: "u2"}
	got, ok := gatedActionForChoice(nil, choice)
	if !ok || got != decision.ActionRomance {
		t.Fatalf("romance 动作应识别为 ActionRomance，得到 (%q, %v)", got, ok)
	}
}

func TestGatedActionForChoice_DefectByKeyword(t *testing.T) {
	// 叛变意图须出现在 next_action（声明的下一步行动）里才进门控。
	choice := unitDecisionChoicePayload{
		Action:     DecisionActionMove,
		NextAction: "向对面阵营移动，投敌归顺。",
	}
	got, ok := gatedActionForChoice(nil, choice)
	if !ok || got != decision.ActionDefect {
		t.Fatalf("next_action 含叛变意图应识别为 ActionDefect，得到 (%q, %v)", got, ok)
	}
}

// TestGatedActionForChoice_DefectFalsePositivesAvoided 验证误杀修复：reasoning/speak/memory 里提到叛变
// （策略讨论/表忠/玩家指令回显）但 next_action 是正常动作 → 绝不进 defect 门控（否则强制态下会被静默替换成 Hold）。
func TestGatedActionForChoice_DefectFalsePositivesAvoided(t *testing.T) {
	cases := []unitDecisionChoicePayload{
		// 表忠：speak 说绝不倒戈，实际去进攻。
		{Action: DecisionActionAttack, Speak: "我对主公忠心耿耿，绝不倒戈！", NextAction: "突袭敌方前锋"},
		// 策略推理：reasoning 分析敌方可能叛变，实际移动占位。
		{Action: DecisionActionMove, Reasoning: "敌阵中或有人欲投敌，可设伏。", NextAction: "向东侧高地移动占据有利地形"},
		// 玩家指令回显：memory 记着「防止叛变」的命令，实际巡逻。
		{Action: DecisionActionMove, Memory: "主公令我提防营中叛变", NextAction: "沿营寨边界巡逻"},
	}
	for i, c := range cases {
		if got, ok := gatedActionForChoice(nil, c); ok {
			t.Fatalf("用例 %d 文本提及叛变但 next_action 正常，不应被门控，得到 %q", i, got)
		}
	}
}

func TestGatedActionForChoice_SellPinnedOffCatalog(t *testing.T) {
	// 目录外的具名独有遗物（父辈遗志）被卖出 → sell_pinned 门控。
	choice := unitDecisionChoicePayload{
		Action:    DecisionActionTrade,
		TradeKind: TradeActionKindSell,
		ItemID:    "father_blade_legacy",
		ItemName:  "父亲的断剑",
	}
	got, ok := gatedActionForChoice(nil, choice)
	if !ok || got != decision.ActionSellPinned {
		t.Fatalf("变卖目录外遗物应识别为 ActionSellPinned，得到 (%q, %v)", got, ok)
	}
}

// TestGatedActionForChoice_SellUpgradedCatalogLegacy 验证 §5 闭环：目录内装备（long_sword）被 upgradeItemToLegacy
// 刻成传家物（IsLegacy&&SoulBound&&Pinned）后，即便其 ID 在静态目录里（isPermanentAnchorItem 会漏判），
// 只要该角色实际持有这件升级实例，sell_pinned 门控仍触发——落实「升级后 LLM 自治也卖不掉」。
func TestGatedActionForChoice_SellUpgradedCatalogLegacy(t *testing.T) {
	actor := &unit.Record{ID: "u1"}
	actor.Inventory.Equipment = map[string]unit.ItemStack{
		"weapon": {ItemID: "long_sword", Quantity: 1, IsLegacy: true, SoulBound: true, Pinned: true},
	}
	// 目录启发式对 long_sword 返回 false（量产可买卖），但持有的是升级实例 → 仍门控。
	if isPermanentAnchorItem("long_sword", "") {
		t.Fatalf("前提失败：long_sword 不应被目录启发式判为永久锚")
	}
	choice := unitDecisionChoicePayload{Action: DecisionActionTrade, TradeKind: TradeActionKindSell, ItemID: "long_sword"}
	got, ok := gatedActionForChoice(actor, choice)
	if !ok || got != decision.ActionSellPinned {
		t.Fatalf("变卖升级后的目录内传家物应识别为 ActionSellPinned，得到 (%q, %v)", got, ok)
	}
	// 持有实例驱动 ItemIsPermanentAnchor=true → GateSurprise 判 PINNED_PERMANENT 剔除（连重压也卖不掉）。
	in := buildSurpriseGateInput(actor, choice, decision.ActionSellPinned)
	if !in.ItemIsPermanentAnchor {
		t.Fatalf("持有升级实例应令 ItemIsPermanentAnchor=true，得到 %+v", in)
	}
	res := decision.GateSurprise(decision.ActionSellPinned, in)
	if res.Decision != decision.GateReject || res.Reason != "PINNED_PERMANENT" {
		t.Fatalf("升级后的传家物应判 PINNED_PERMANENT 剔除，得到 %+v", res)
	}
}

// TestActorHoldsPermanentAnchor 验证持有实例判定：仅同 ID 且三标记齐备才算永久锚（背包/装备栏皆查）。
func TestActorHoldsPermanentAnchor(t *testing.T) {
	actor := &unit.Record{ID: "u1"}
	actor.Inventory.Equipment = map[string]unit.ItemStack{
		"weapon": {ItemID: "long_sword", Pinned: true}, // 仅 Pinned，未满三标记 → 非永久锚
	}
	actor.Inventory.Backpack = []unit.ItemStack{
		{ItemID: "spear", IsLegacy: true, SoulBound: true, Pinned: true}, // 满三标记 → 永久锚
	}
	if actorHoldsPermanentAnchor(actor, "long_sword") {
		t.Fatalf("仅 Pinned 的 long_sword 不应判为永久锚")
	}
	if !actorHoldsPermanentAnchor(actor, "spear") {
		t.Fatalf("三标记齐备的 spear 应判为永久锚")
	}
	if actorHoldsPermanentAnchor(actor, "dagger") {
		t.Fatalf("未持有的 dagger 不应判为永久锚")
	}
	if actorHoldsPermanentAnchor(nil, "spear") {
		t.Fatalf("nil actor 应安全返回 false")
	}
}

func TestGatedActionForChoice_OrdinaryActionsNotGated(t *testing.T) {
	cases := []unitDecisionChoicePayload{
		{Action: DecisionActionAttack},
		{Action: DecisionActionMove, Reasoning: "去采集木材补给。"},
		{Action: DecisionActionHold},
		// 普通可量产物品的买卖不进门控。
		{Action: DecisionActionTrade, TradeKind: TradeActionKindSell, ItemID: "ration", ItemName: "口粮"},
		{Action: DecisionActionTrade, TradeKind: TradeActionKindSell, ItemID: "long_sword"},
		// family 是已确认恋人后的共育，不在「突然恋爱」门控内。
		{Action: DecisionActionFamily, TargetUnitID: "u2"},
	}
	for i, c := range cases {
		if got, ok := gatedActionForChoice(nil, c); ok {
			t.Fatalf("用例 %d 不应被门控，得到 %q", i, got)
		}
	}
}

// --- 永久锚物品判定 ---

func TestIsPermanentAnchorItem(t *testing.T) {
	// 目录内可量产物品不是永久锚。
	if isPermanentAnchorItem("ration", "口粮") {
		t.Fatalf("口粮不应是永久锚物品")
	}
	if isPermanentAnchorItem("long_sword", "") {
		t.Fatalf("长剑不应是永久锚物品")
	}
	// 目录外具名物品（叙事独有遗物）是永久锚。
	if !isPermanentAnchorItem("mother_locket", "母亲的吊坠") {
		t.Fatalf("目录外具名遗物应是永久锚物品")
	}
	// 空标的不是。
	if isPermanentAnchorItem("", "") {
		t.Fatalf("空标的不应是永久锚物品")
	}
}

// --- evaluateSurpriseGate：门控判定（无 DB，关系前因为空）---

func TestEvaluateSurpriseGate_RomanceNoPriorBlocked(t *testing.T) {
	service := &Service{} // db==nil → 无关系积累
	actor := &unit.Record{ID: "u1", Status: unit.Status{HP: 100, Loyalty: 0.8}}
	choice := unitDecisionChoicePayload{Action: DecisionActionRomance, TargetUnitID: "u2"}
	res := service.evaluateSurpriseGate(context.Background(), actor, choice, decision.ActionRomance)
	if res.Decision != decision.GateReject {
		t.Fatalf("无关系积累的突然恋爱应被剔除，得到 %+v", res)
	}
}

func TestEvaluateSurpriseGate_SellParentLegacyRejected(t *testing.T) {
	service := &Service{}
	actor := &unit.Record{ID: "u1", Status: unit.Status{HP: 100, Wallet: 0, InCombat: true}}
	// 即使有债务/威胁压力，父辈遗志（目录外具名遗物）也绝不可卖。
	choice := unitDecisionChoicePayload{
		Action: DecisionActionTrade, TradeKind: TradeActionKindSell,
		ItemID: "father_blade_legacy", ItemName: "父亲的断剑",
	}
	res := service.evaluateSurpriseGate(context.Background(), actor, choice, decision.ActionSellPinned)
	if res.Decision != decision.GateReject || res.Reason != "PINNED_PERMANENT" {
		t.Fatalf("父辈遗志应判 PINNED_PERMANENT 剔除，得到 %+v", res)
	}
}

func TestEvaluateSurpriseGate_DefectUngroundedRejected(t *testing.T) {
	service := &Service{}
	// 忠诚高 + 无负面记忆/关系恶化/势力衰退 → 纯野心不足以叛变。
	actor := &unit.Record{ID: "u1", Status: unit.Status{HP: 100, Loyalty: 0.9, Morale: 0.8}}
	choice := unitDecisionChoicePayload{Action: DecisionActionMove, Reasoning: "我要叛变。"}
	res := service.evaluateSurpriseGate(context.Background(), actor, choice, decision.ActionDefect)
	if res.Decision != decision.GateReject || res.Reason != "DEFECT_UNGROUNDED" {
		t.Fatalf("无前因的叛变应判 DEFECT_UNGROUNDED 剔除，得到 %+v", res)
	}
}

func TestEvaluateSurpriseGate_DefectGroundedAllowed(t *testing.T) {
	service := &Service{}
	// 忠诚崩坏 + 带关系恶化前因（归因含 relation）→ 允许自治叛变。
	actor := &unit.Record{ID: "u1", Status: unit.Status{HP: 100, Loyalty: 0.2, Morale: 0.1}}
	choice := unitDecisionChoicePayload{
		Action:    DecisionActionMove,
		Reasoning: "积怨已深，我要叛变投敌。",
		Attribution: &attributionPayload{
			Primary:     causeRefPayload{Kind: string(decision.CauseRelation), RefID: "u9", Weight: 0.6},
			NarrativeZH: "长期被排挤，她决意倒戈",
		},
	}
	res := service.evaluateSurpriseGate(context.Background(), actor, choice, decision.ActionDefect)
	if res.Decision != decision.GateAllow {
		t.Fatalf("忠诚崩坏且有关系恶化前因的叛变应放行，得到 %+v", res)
	}
}

// --- buildSurpriseGateInput：纯前因映射 ---

func TestBuildSurpriseGateInput_LoyaltyAndPressure(t *testing.T) {
	actor := &unit.Record{Status: unit.Status{Loyalty: 0.3, Wallet: 0, InCombat: true, Morale: 0.1}}
	choice := unitDecisionChoicePayload{
		Reasoning: "叛变",
		Attribution: &attributionPayload{
			Primary:    causeRefPayload{Kind: string(decision.CauseMemory), RefID: "m1", Weight: 0.6},
			Supporting: []causeRefPayload{{Kind: string(decision.CauseRelation), RefID: "u9", Weight: 0.6}},
		},
	}
	in := buildSurpriseGateInput(actor, choice, decision.ActionDefect)
	if in.Loyalty != 0.3 {
		t.Fatalf("忠诚应映射为 0.3，得到 %v", in.Loyalty)
	}
	if !in.HasDebtPressure || !in.HasThreatPressure || !in.HasFactionDeclinePressure {
		t.Fatalf("债务/威胁/势力衰退压力应全部触发，得到 %+v", in)
	}
	if !in.HasNegativeMemory || !in.HasRelationDecay {
		t.Fatalf("归因含 memory/relation 应映射为负面记忆/关系恶化佐证，得到 %+v", in)
	}
}

// --- 遥测：进入门控会计数 ---

func TestSurpriseGateStatsMonotonic(t *testing.T) {
	beforeTotal, beforeBlocked := SurpriseGateStats()
	surpriseGateTotal.Add(1)
	surpriseGateBlocked.Add(1)
	t.Cleanup(func() {
		surpriseGateTotal.Add(-1)
		surpriseGateBlocked.Add(-1)
	})
	afterTotal, afterBlocked := SurpriseGateStats()
	if afterTotal != beforeTotal+1 || afterBlocked != beforeBlocked+1 {
		t.Fatalf("遥测计数应单调递增：before=(%d,%d) after=(%d,%d)",
			beforeTotal, beforeBlocked, afterTotal, afterBlocked)
	}
}
