package session

// 文件说明：本波「接主循环」接线（service.go/router.go Wire 阶段）的集成/单元测试。
// 覆盖：① 离线宪章服务方法 Set/Get/Clear 往返 + CHARTER_ACTIVATED/CHARTER_UPDATED 留痕；
// ② Freeze List 动作落地前拦截 maybeFreezeOfflineAction 的 flag 兜底/Pinned 命中/上交命运；
// ③ 纯函数助手 gatedActionForDecision/decisionIsOfflineDisposal/actorItemPinned；
// ④ settleAutonomyAtDeploymentBoundary 的 nil/空安全（best-effort，绝不 panic）。
// 全部对真实 SQLite 临时库跑通（参照 chronicle/decision_trace 测试范式）。

import (
	"context"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func newWireService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "wire.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewServiceWithColdStore(db, nil, nil), context.Background()
}

// seedWireUnit 落一个真实 unit 行（events 表对 units(id) 有外键，宪章留痕需单位先存在）。
func seedWireUnit(t *testing.T, service *Service, ctx context.Context, seed int64, sessionID, name string) unit.Record {
	t.Helper()
	rec := unit.BootstrapRecord(seed, sessionID, "player", name)
	if err := service.units.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
	return rec
}

// --- 离线宪章服务方法往返 ---

func TestCharterServiceRoundTrip(t *testing.T) {
	service, ctx := newWireService(t)
	rec := seedWireUnit(t, service, ctx, 1, "s1", "惊蛰")
	if err := service.sessions.Save(ctx, &State{ID: "s1", PlayerUnitIDs: []string{rec.ID}}); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	// 初始：未设立宪章。
	_, exists, err := service.GetUnitCharterForSession(ctx, "s1", rec.ID)
	if err != nil {
		t.Fatalf("读宪章失败: %v", err)
	}
	if exists {
		t.Fatalf("初始不应存在宪章")
	}

	// 设立（首次 → CHARTER_ACTIVATED）。
	stored, err := service.SetUnitCharterForSession(ctx, "s1", rec.ID, OfflineCharter{
		LongTermGoals:  []string{"  守住家园  ", ""},
		Redlines:       []CharterRedline{{Text: "绝不卖掉母亲的吊坠"}},
		SocialMandates: []string{"勿与商盟结仇"},
	})
	if err != nil {
		t.Fatalf("设立宪章失败: %v", err)
	}
	// NormalizeCharter：去空白条目、补稳定红线 ID。
	if len(stored.LongTermGoals) != 1 || stored.LongTermGoals[0] != "守住家园" {
		t.Fatalf("长期目标应去空白裁剪：%+v", stored.LongTermGoals)
	}
	if len(stored.Redlines) != 1 || stored.Redlines[0].ID == "" {
		t.Fatalf("红线应补稳定 ID：%+v", stored.Redlines)
	}

	// 读回应一致且 exists=true。
	got, exists, err := service.GetUnitCharterForSession(ctx, "s1", rec.ID)
	if err != nil || !exists {
		t.Fatalf("读回宪章失败: exists=%v err=%v", exists, err)
	}
	if len(got.SocialMandates) != 1 || got.SocialMandates[0] != "勿与商盟结仇" {
		t.Fatalf("社交授权应持久化：%+v", got.SocialMandates)
	}

	// 留痕：首次设立应落 CHARTER_ACTIVATED。
	if n := countEvents(t, service, ctx, rec.ID, events.ReasonCharterActivated); n != 1 {
		t.Fatalf("首次设立应落 1 条 CHARTER_ACTIVATED，得到 %d", n)
	}

	// 二次写（更新 → CHARTER_UPDATED）。
	if _, err := service.SetUnitCharterForSession(ctx, "s1", rec.ID, OfflineCharter{LongTermGoals: []string{"扩张"}}); err != nil {
		t.Fatalf("更新宪章失败: %v", err)
	}
	if n := countEvents(t, service, ctx, rec.ID, events.ReasonCharterUpdated); n != 1 {
		t.Fatalf("更新应落 1 条 CHARTER_UPDATED，得到 %d", n)
	}

	// 撤销（清除 → CHARTER_UPDATED，cleared=true）。
	if err := service.ClearUnitCharterForSession(ctx, "s1", rec.ID); err != nil {
		t.Fatalf("撤销宪章失败: %v", err)
	}
	_, exists, _ = service.GetUnitCharterForSession(ctx, "s1", rec.ID)
	if exists {
		t.Fatalf("撤销后不应再存在宪章")
	}
	if n := countEvents(t, service, ctx, rec.ID, events.ReasonCharterUpdated); n != 2 {
		t.Fatalf("撤销应再落 1 条 CHARTER_UPDATED（累计 2），得到 %d", n)
	}
}

func countEvents(t *testing.T, service *Service, ctx context.Context, actorID string, code events.ReasonCode) int {
	t.Helper()
	var n int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		actorID, string(code),
	).Scan(&n); err != nil {
		t.Fatalf("统计事件失败: %v", err)
	}
	return n
}

// --- Freeze List 动作落地前拦截 ---

func TestMaybeFreezeOfflineAction_FlagGate(t *testing.T) {
	t.Setenv("QUNXIANG_FREEZE_LIST", "")
	service, ctx := newWireService(t)
	actor := unit.BootstrapRecord(2, "s1", "player", "行舟")
	_ = unit.AddBackpackItem(&actor, "ration", 1)
	for i := range actor.Inventory.Backpack {
		if actor.Inventory.Backpack[i].ItemID == "ration" {
			actor.Inventory.Backpack[i].Pinned = true
		}
	}
	state := &State{ID: "s1"}
	SetUnitCharter(state, actor.ID, OfflineCharter{Redlines: []CharterRedline{{Text: "绝不卖掉传家宝"}}})
	dec := unitDecisionPayload{Action: DecisionActionTrade, ItemID: "ration"}
	// flag 关：恒不拦截。
	if service.maybeFreezeOfflineAction(ctx, state, &actor, dec) {
		t.Fatalf("QUNXIANG_FREEZE_LIST 关时不应拦截")
	}
}

func TestMaybeFreezeOfflineAction_PinnedFrozen(t *testing.T) {
	t.Setenv("QUNXIANG_FREEZE_LIST", "on")
	service, ctx := newWireService(t)
	rec := seedWireUnit(t, service, ctx, 3, "s1", "折棠")
	if err := service.sessions.Save(ctx, &State{ID: "s1", PlayerUnitIDs: []string{rec.ID}}); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}
	// 给单位一个 Pinned 背包物（传家宝）。
	if err := unit.AddBackpackItem(&rec, "ration", 1); err != nil {
		t.Fatalf("加物品失败: %v", err)
	}
	for i := range rec.Inventory.Backpack {
		if rec.Inventory.Backpack[i].ItemID == "ration" {
			rec.Inventory.Backpack[i].Pinned = true
		}
	}
	state := &State{ID: "s1"}
	dec := unitDecisionPayload{Action: DecisionActionTrade, TradeKind: TradeActionKindSell, ItemID: "ration", ItemName: "传家宝"}
	// flag 开 + 卖出 Pinned 标的：应拦截并上交命运（返回 true）。
	if !service.maybeFreezeOfflineAction(ctx, state, &rec, dec) {
		t.Fatalf("flag 开 + 卖出 Pinned 标的应被冻结上交命运")
	}
	// 非处置类、无门控的动作（如观望）不应被拦截。
	if service.maybeFreezeOfflineAction(ctx, state, &rec, unitDecisionPayload{Action: DecisionActionObserve}) {
		t.Fatalf("普通观望动作不应被冻结")
	}
	// 普通交易（卖非 Pinned、不触红线/禁令）不应被冻结——修「开 flag 后每笔交易都被冻」过宽 bug 的回归守护。
	plain := unitDecisionPayload{Action: DecisionActionTrade, TradeKind: TradeActionKindSell, ItemID: "ration", ItemName: "一捆草料"}
	for i := range rec.Inventory.Backpack {
		if rec.Inventory.Backpack[i].ItemID == "ration" {
			rec.Inventory.Backpack[i].Pinned = false
		}
	}
	if service.maybeFreezeOfflineAction(ctx, state, &rec, plain) {
		t.Fatalf("普通非 Pinned、不触红线的交易不应被冻结")
	}
	// 买入方向（非处置）也不应被冻结。
	if service.maybeFreezeOfflineAction(ctx, state, &rec, unitDecisionPayload{Action: DecisionActionTrade, TradeKind: TradeActionKind("buy"), ItemID: "ration"}) {
		t.Fatalf("买入交易不应被冻结")
	}
}

// --- 纯函数助手 ---

func TestGatedActionForDecision(t *testing.T) {
	sell := unitDecisionPayload{Action: DecisionActionTrade, TradeKind: TradeActionKindSell}
	cases := []struct {
		name   string
		dec    unitDecisionPayload
		pinned bool
		want   decision.GatedAction
	}{
		{"恋爱→romance", unitDecisionPayload{Action: DecisionActionRomance}, false, decision.ActionRomance},
		{"卖出 Pinned→sell_pinned", sell, true, decision.ActionSellPinned},
		{"卖出非 Pinned→空（普通交易不进门控）", sell, false, ""},
		{"买入→空", unitDecisionPayload{Action: DecisionActionTrade, TradeKind: TradeActionKind("buy")}, false, ""},
		{"观望→空", unitDecisionPayload{Action: DecisionActionObserve}, false, ""},
		{"攻击→空", unitDecisionPayload{Action: DecisionActionAttack}, false, ""},
	}
	for _, c := range cases {
		if got := gatedActionForDecision(c.dec, c.pinned); got != c.want {
			t.Fatalf("%s: gatedActionForDecision=%q，期望 %q", c.name, got, c.want)
		}
	}
}

func TestActorItemPinned(t *testing.T) {
	rec := unit.BootstrapRecord(4, "s1", "player", "甲")
	_ = unit.AddBackpackItem(&rec, "ration", 1)
	for i := range rec.Inventory.Backpack {
		if rec.Inventory.Backpack[i].ItemID == "ration" {
			rec.Inventory.Backpack[i].Pinned = true
		}
	}
	if !actorItemPinned(rec, "ration") {
		t.Fatalf("背包中 Pinned 物品应被识别")
	}
	if !actorItemPinned(rec, "  RATION ") {
		t.Fatalf("应大小写/空白不敏感")
	}
	if actorItemPinned(rec, "rope") {
		t.Fatalf("不存在的物品不应判为 Pinned")
	}
	if actorItemPinned(rec, "") {
		t.Fatalf("空 itemID 应返回 false")
	}
}

// --- 回合边界自治结算 best-effort 安全 ---

func TestSettleAutonomyAtDeploymentBoundary_Safe(t *testing.T) {
	service, ctx := newWireService(t)
	// nil/空入参不应 panic。
	service.settleAutonomyAtDeploymentBoundary(ctx, nil, nil)
	service.settleAutonomyAtDeploymentBoundary(ctx, &State{ID: "s1"}, nil)
	// 含一个活单位 + 一个已死单位：不 panic，已死单位被跳过。
	alive := unit.BootstrapRecord(5, "s1", "player", "活")
	dead := unit.BootstrapRecord(6, "s1", "player", "亡")
	dead.Status.LifeState = unit.LifeStateDead
	state := &State{ID: "s1"}
	state.TurnState.Turn = 24 // 命中 goalReassessCadence 边界（无 LLM 时走确定性 fallback）。
	service.settleAutonomyAtDeploymentBoundary(ctx, state, []unit.Record{alive, dead})
}
