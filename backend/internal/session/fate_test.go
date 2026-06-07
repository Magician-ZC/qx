package session

// 文件说明：命运相关性路由与命运收件箱的 DB 集成测试——关系锚翻译、自治过滤、待决策入箱与处理。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

func TestSurfaceFateEvent_FriendPendingAndResolve(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	a := unit.BootstrapRecord(2, "s1", "player", "阿采")
	b := unit.BootstrapRecord(4, "s1", "player", "老吴")
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := repo.Save(ctx, b); err != nil {
		t.Fatalf("save b: %v", err)
	}
	// 阿采对老吴有强羁绊（强度 16/20→weight 0.8→relevance 0.8→待决策）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, b.ID, 8.0, 0.0, 8.0, 0.0,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}

	ev := FateEvent{ActorID: b.ID, TargetID: b.ID, ReasonCode: events.ReasonCombatDown, Importance: 9, Summary: "老吴在北岭的雪夜里倒下了"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &a, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RoutePending {
		t.Fatalf("密友倒下经强关系锚应升级待决策，得到 %s（rel=%.3f）", routing.Route, routing.Relevance)
	}
	if routing.DecisionID == "" || !contains(routing.Card, "老吴") {
		t.Fatalf("待决策应有 DecisionID 与含密友名的祖魂卡：%+v", routing)
	}

	inbox, err := service.OpenFateInbox(ctx, a.ID)
	if err != nil {
		t.Fatalf("open inbox: %v", err)
	}
	if len(inbox) != 1 || inbox[0].DecisionID != routing.DecisionID {
		t.Fatalf("收件箱应有 1 条待决策，得到 %d", len(inbox))
	}

	// 处理后应出箱。
	if err := service.ResolveFateDecision(ctx, "s1", a.ID, routing.DecisionID, "acknowledge"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	inbox2, _ := service.OpenFateInbox(ctx, a.ID)
	if len(inbox2) != 0 {
		t.Fatalf("处理后收件箱应为空，得到 %d", len(inbox2))
	}
}

func TestWorldizeDeath_SurfacesToMourners(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	mourner := unit.BootstrapRecord(2, "s1", "player", "阿采")
	fallen := unit.BootstrapRecord(4, "s1", "player", "老吴")
	stranger := unit.BootstrapRecord(6, "s1", "player", "路人")
	for _, r := range []unit.Record{mourner, fallen, stranger} {
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	// 阿采深爱老吴；路人与老吴无关系。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		mourner.ID, fallen.ID, 8.0, 0.0, 9.0, 0.0,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}

	surfaced, err := service.WorldizeDeath(ctx, "s1", fallen)
	if err != nil {
		t.Fatalf("worldize death: %v", err)
	}
	if surfaced != 1 {
		t.Fatalf("应只惊动 1 个在乎她的人，得到 %d", surfaced)
	}
	// 阿采的收件箱应收到老吴之死。
	inbox, _ := service.OpenFateInbox(ctx, mourner.ID)
	if len(inbox) != 1 || !contains(inbox[0].Narrative, "老吴") {
		t.Fatalf("哀悼者收件箱应有老吴之死，得到 %+v", inbox)
	}
	// 路人无感。
	if box, _ := service.OpenFateInbox(ctx, stranger.ID); len(box) != 0 {
		t.Fatalf("无关者不应被惊动")
	}
}

func TestSurfaceFateEvent_StrangerAutonomous(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(6, "s1", "player", "阿采")
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}

	before := eventCount(t, db)
	// 陌生人的事，无锚命中 → 自治不打扰，不写流程事件。
	ev := FateEvent{ActorID: "stranger_id", TargetID: "stranger_id", ReasonCode: events.ReasonCombatDown, Importance: 9, Summary: "远方某人出事了"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &a, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RouteAutonomous {
		t.Fatalf("陌生人的事应自治不打扰，得到 %s", routing.Route)
	}
	if eventCount(t, db) != before {
		t.Fatalf("自治不打扰不应写流程事件")
	}
	if inbox, _ := service.OpenFateInbox(ctx, a.ID); len(inbox) != 0 {
		t.Fatalf("收件箱应为空")
	}
}

func TestSurfaceFateEvent_SelfHighlightNotPending(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(8, "s1", "player", "阿采")
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 她自己的中等重要度事件(importance 5 → rel 0.5) → 高光卡，不升级待决策。
	ev := FateEvent{ActorID: a.ID, TargetID: a.ID, ReasonCode: events.ReasonInboxHighlight, Importance: 5, Summary: "她砍翻了一头独狼"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &a, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RouteHighlight {
		t.Fatalf("自身中等事件应进高光卡，得到 %s（rel=%.3f）", routing.Route, routing.Relevance)
	}
	if inbox, _ := service.OpenFateInbox(ctx, a.ID); len(inbox) != 0 {
		t.Fatalf("高光卡不入待决策收件箱")
	}
}
