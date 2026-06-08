package session

// 文件说明：命运层本波四缺口的聚焦测试——红线锚（§4.1）、AttributionZH 派生（§5）、daysAlive 真实化、
// fate 后果接 D0-D3 分级闸（§4.3）、§4.3 provenance 强制、情境化 Copilot 选项（§4.5，向后兼容旧三键）。
// 前缀统一 fateProv*/fateChoice* 等，避免与既有命运测试撞名。纯函数测试无 DB；集成测试复用 newThreatTestService。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

// ---- §4.1 红线锚 ----

// TestFateRedlineAnchorRoutes 验证：单位 charter 的红线生成 Redline 锚后，触线类事件（叛变）经红线锚翻译相关性
// 从「无锚=自治不打扰」抬升到被路由——红线权重真正参与了 FateScore（修「红线锚从未生成」的死管线）。
func TestFateRedlineAnchorRoutes(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	owner := "u_owner"
	// 无 charter：陌生人的叛变事件无锚命中 → 相关性 0。
	relNoCharter, _ := eventRelevanceWithAnchor(
		service.buildRelevanceAnchorsWithState(ctx, nil, owner),
		FateEvent{ActorID: "stranger", TargetID: "stranger", ReasonCode: events.ReasonRelationBetray},
	)
	if relNoCharter != 0 {
		t.Fatalf("无红线锚时陌生人叛变事件相关性应为 0，得到 %.3f", relNoCharter)
	}

	// 有 charter（一条红线）：同一事件经红线锚翻译应过相关性阈（>=0.35）。
	state := &State{}
	SetUnitCharter(state, owner, OfflineCharter{
		Redlines: []CharterRedline{{Text: "永不背叛同伴", Severity: "absolute"}},
	})
	anchors := service.buildRelevanceAnchorsWithState(ctx, state, owner)
	hasRedline := false
	for _, a := range anchors {
		if a.Kind == relevance.Redline {
			hasRedline = true
		}
	}
	if !hasRedline {
		t.Fatalf("有红线的 charter 应生成 Redline 锚，实际锚：%+v", anchors)
	}
	relWithCharter, kind := eventRelevanceWithAnchor(
		anchors,
		FateEvent{ActorID: "stranger", TargetID: "stranger", ReasonCode: events.ReasonRelationBetray},
	)
	if relWithCharter < relevance.RelevanceGate {
		t.Fatalf("触线类事件经红线锚翻译应过相关性阈 %.2f，得到 %.3f", relevance.RelevanceGate, relWithCharter)
	}
	if kind != string(relevance.Redline) {
		t.Fatalf("命中里最重锚类别应为 redline，得到 %q", kind)
	}

	// 非触线类事件（如普通经济奖励）不触红线锚 → 不被红线点亮。
	relBenign, _ := eventRelevanceWithAnchor(
		anchors,
		FateEvent{ActorID: "stranger", TargetID: "stranger", ReasonCode: events.ReasonEconomyReward},
	)
	if relBenign != 0 {
		t.Fatalf("非触线类事件不应点亮红线锚，得到 %.3f", relBenign)
	}
}

// ---- §5 AttributionZH 派生 ----

func TestFateDeriveProvenance(t *testing.T) {
	// 命中锚类别 + 摘要 → 「引子：摘要」。
	got := deriveFateProvenance(FateEvent{Summary: "老吴在雪夜里倒下"}, string(relevance.Relation))
	if got != "因为这事关她在乎的人：老吴在雪夜里倒下" {
		t.Fatalf("关系锚 + 摘要派生不符：%q", got)
	}
	// 红线锚引子。
	if got := deriveFateProvenance(FateEvent{Summary: "X"}, string(relevance.Redline)); got != "因为这触到了她划下的红线：X" {
		t.Fatalf("红线锚派生不符：%q", got)
	}
	// 无锚但有摘要（自身事件）→ 直接用摘要。
	if got := deriveFateProvenance(FateEvent{Summary: "她独自走完了那条路"}, ""); got != "她独自走完了那条路" {
		t.Fatalf("无锚自身事件应直接用摘要，得到 %q", got)
	}
	// 既无锚也无摘要 → 空串（跳过尾注）。
	if got := deriveFateProvenance(FateEvent{}, ""); got != "" {
		t.Fatalf("无前因应返回空串，得到 %q", got)
	}
}

// TestFateCardCarriesAttribution 验证渲染管线：SurfaceFateEvent 自动派生 AttributionZH，命运卡尾注非空（活管线）。
func TestFateCardCarriesAttribution(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(20, "s1", "player", "阿采")
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	ev := FateEvent{ActorID: a.ID, TargetID: a.ID, ReasonCode: events.ReasonInboxHighlight, Importance: 5, Summary: "她砍翻了一头独狼"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &a, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	// 自身事件无外部锚，AttributionZH 由摘要派生 → 命运卡应带「（…她砍翻了一头独狼）」尾注。
	if !contains(routing.Card, "她砍翻了一头独狼") {
		t.Fatalf("命运卡应带归因尾注，得到 %q", routing.Card)
	}
}

// ---- §4.3 D0-D3 分级闸 ----

func TestFatePenaltyLayerMapping(t *testing.T) {
	// |delta| 量级 → 候选层（与 fatePenaltyMagnitudeForLayer 互逆）。
	if penaltyLayerForMagnitude(0.40) != 3 || penaltyLayerForMagnitude(0.10) != 2 || penaltyLayerForMagnitude(0.03) != 1 {
		t.Fatalf("penaltyLayerForMagnitude 阈值不符")
	}
	// 低牵挂+陪伴浅（cap=1）：重负向后果被降级到层1 的轻幅度。
	gated := gateFateConsequencesByPenalty(
		[]fateConsequence{{Field: status.FieldMorale, Delta: -0.50, ReasonCode: events.ReasonEmotionTrauma}},
		0, 0, // care=0 daysAlive=0 → PenaltyCap=1
	)
	if gated[0].Delta != -fatePenaltyMagnitudeForLayer(1) {
		t.Fatalf("低牵挂应把不可逆重击降到层1 幅度 %.3f，得到 %.3f", -fatePenaltyMagnitudeForLayer(1), gated[0].Delta)
	}
	// 与 encounter.DegradePenalty 一致：高牵挂+久陪伴（cap=3）保留层3。
	if encounter.PenaltyCap(80, 8) != 3 {
		t.Fatalf("前置：高牵挂+久陪伴 cap 应为 3")
	}
	gatedHigh := gateFateConsequencesByPenalty(
		[]fateConsequence{{Field: status.FieldMorale, Delta: -0.50, ReasonCode: events.ReasonEmotionTrauma}},
		80, 8,
	)
	if gatedHigh[0].Delta != -fatePenaltyMagnitudeForLayer(3) {
		t.Fatalf("高牵挂应保留层3 幅度 %.3f，得到 %.3f", -fatePenaltyMagnitudeForLayer(3), gatedHigh[0].Delta)
	}
	// 正向后果不进闸（原样）。
	pos := gateFateConsequencesByPenalty(
		[]fateConsequence{{Field: status.FieldMorale, Delta: +0.05, ReasonCode: events.ReasonEmotionReward}},
		0, 0,
	)
	if pos[0].Delta != 0.05 {
		t.Fatalf("正向后果不应被惩罚闸改动，得到 %.3f", pos[0].Delta)
	}
}

// ---- §4.5 情境化 Copilot 选项 + resolveClass 折算 ----

func TestFateBuildChoices(t *testing.T) {
	// 不可逆类：avenge(urge)/mourn(acknowledge)/let_her(let_her)。
	irr := buildFateChoices(FateEvent{ReasonCode: events.ReasonCombatDown}, "")
	if len(irr) != 3 || irr[0].ID != "avenge" || irr[0].ResolveClass != "urge" {
		t.Fatalf("不可逆类情境选项不符：%+v", irr)
	}
	// 触红线类（红线锚命中）：forbid(urge)/allow(let_her)。
	red := buildFateChoices(FateEvent{ReasonCode: events.ReasonEconomyReward}, string(relevance.Redline))
	if len(red) != 2 || red[0].ID != "forbid" || red[1].ResolveClass != "let_her" {
		t.Fatalf("触红线类情境选项不符：%+v", red)
	}
	// 日常类：urge/let_her/acknowledge。
	day := buildFateChoices(FateEvent{ReasonCode: events.ReasonEconomyReward}, string(relevance.Relation))
	if len(day) != 3 {
		t.Fatalf("日常类应有 3 个选项，得到 %d", len(day))
	}
}

func TestFateResolveChoiceClass(t *testing.T) {
	choices := []fateChoice{{ID: "avenge", ResolveClass: "urge"}, {ID: "mourn", ResolveClass: "acknowledge"}}
	// 情境 id 折算回基础后果类。
	if resolveFateChoiceClass("avenge", choices) != "urge" {
		t.Fatalf("avenge 应折算为 urge")
	}
	if resolveFateChoiceClass("mourn", choices) != "acknowledge" {
		t.Fatalf("mourn 应折算为 acknowledge")
	}
	// 向后兼容：旧三键不在 choices 里 → 原样返回（由 fateConsequencesFor 兜底）。
	if resolveFateChoiceClass("let_her", choices) != "let_her" {
		t.Fatalf("旧三键应原样返回")
	}
	if resolveFateChoiceClass("acknowledge", nil) != "acknowledge" {
		t.Fatalf("无 choices 时旧三键应原样返回")
	}
}

// TestFateResolveContextualChoiceEndToEnd 验证：玩家用情境化选项 id（mourn=acknowledge）处理待决策后正常出箱、施加暖意。
func TestFateResolveContextualChoiceEndToEnd(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(21, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 自身高 stakes 事件（带摘要 → AttributionZH 自动派生非空，满足不可逆 provenance 要求）。
	ev := FateEvent{ActorID: hero.ID, TargetID: hero.ID, ReasonCode: events.ReasonCombatDown, Importance: 9, EmotionWeight: -0.85, Summary: "她在断崖边重伤倒地"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &hero, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RoutePending || routing.DecisionID == "" {
		t.Fatalf("高 stakes 自身事件应路由 pending，得到 %s", routing.Route)
	}
	// 收件箱应带情境化选项。
	inbox, _ := service.OpenFateInbox(ctx, hero.ID)
	if len(inbox) != 1 || len(inbox[0].Choices) == 0 {
		t.Fatalf("待决策应携带情境化选项，得到 %+v", inbox)
	}
	// 用情境 id「mourn」（=acknowledge 安全类）处理 → 应成功出箱。
	if err := service.ResolveFateDecision(ctx, "s1", hero.ID, routing.DecisionID, "mourn"); err != nil {
		t.Fatalf("情境选项 resolve 应成功：%v", err)
	}
	if box, _ := service.OpenFateInbox(ctx, hero.ID); len(box) != 0 {
		t.Fatalf("处理后应出箱，得到 %d", len(box))
	}
	// DB 侧复核写了 DECISION_RESOLVED。
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ? AND payload_json LIKE ?`,
		hero.ID, string(events.ReasonDecisionResolved), "%"+routing.DecisionID+"%",
	).Scan(&n)
	if n != 1 {
		t.Fatalf("应写 1 条 DECISION_RESOLVED，得到 %d", n)
	}
}

// TestFateProvenanceEnforcementRejectsUngroundedIrreversibleUrge 验证 §4.3：不可逆类 + urge 越界 + 无可解析前因 → 拒绝。
func TestFateProvenanceEnforcementRejectsUngroundedIrreversibleUrge(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(22, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 自身不可逆事件，**无摘要无锚** → AttributionZH 派生为空（无可解析前因）。
	ev := FateEvent{ActorID: hero.ID, TargetID: hero.ID, ReasonCode: events.ReasonRelationBetray, Importance: 9, EmotionWeight: -0.85}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &hero, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RoutePending {
		t.Fatalf("应路由 pending，得到 %s", routing.Route)
	}
	// 用 urge 越界处理无源不可逆命运 → 应被拒绝（绝不在没有「为什么」时悄悄落子）。
	if err := service.ResolveFateDecision(ctx, "s1", hero.ID, routing.DecisionID, "urge"); err == nil {
		t.Fatalf("无源不可逆命运的 urge 越界应被拒绝")
	}
	// 但安全降级路径（let_her）始终放行。
	if err := service.ResolveFateDecision(ctx, "s1", hero.ID, routing.DecisionID, "let_her"); err != nil {
		t.Fatalf("安全降级 let_her 应放行：%v", err)
	}
}
