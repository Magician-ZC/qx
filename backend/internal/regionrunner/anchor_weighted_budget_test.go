package regionrunner

// 文件说明：NPC 锚加权预算（设计 §1.5）的纯函数与留痕测试。
// 覆盖：①anchorWeightedBudgetPerMille = 0.25 + 0.75·density 的单调/夹边界/确定性；②isHighAnchorDensity 阈值；
// ③emitAnchorWeightedEvent 写 ReasonAnchorWeightedEvent 留痕（对真实 SQLite）；④付费不进（预算只由 density 决定）。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// TestAnchorWeightedBudgetPerMille_Formula 验证 0.25 + 0.75·density 的地板/封顶/单调/确定性。
func TestAnchorWeightedBudgetPerMille_Formula(t *testing.T) {
	// 地板：零锚 NPC → 250‰（25% 预算）。
	if b := anchorWeightedBudgetPerMille(0); b != anchorBudgetFloorPerMille {
		t.Fatalf("density=0 预算应为地板 %d‰，得 %d", anchorBudgetFloorPerMille, b)
	}
	// 封顶：满锚 NPC → 1000‰（100% 预算）。
	if b := anchorWeightedBudgetPerMille(1); b != 1000 {
		t.Fatalf("density=1 预算应为 1000‰，得 %d", b)
	}
	// 中点：density=0.5 → 0.25 + 0.375 = 0.625 → 625‰。
	if b := anchorWeightedBudgetPerMille(0.5); b != 625 {
		t.Fatalf("density=0.5 预算应为 625‰，得 %d", b)
	}
	// 越界自夹：负→地板、>1→封顶。
	if anchorWeightedBudgetPerMille(-0.3) != anchorBudgetFloorPerMille {
		t.Fatalf("density<0 应夹到地板")
	}
	if anchorWeightedBudgetPerMille(2) != 1000 {
		t.Fatalf("density>1 应夹到封顶")
	}
	// 单调不减：density 升高，预算不降。
	prev := -1
	for _, d := range []float64{0, 0.1, 0.25, 0.5, 0.75, 0.9, 1.0} {
		b := anchorWeightedBudgetPerMille(d)
		if b < prev {
			t.Fatalf("预算应随 density 单调不减：density=%.2f 预算=%d < 前一个 %d", d, b, prev)
		}
		prev = b
	}
	// 确定性：同输入两次相等。
	if anchorWeightedBudgetPerMille(0.37) != anchorWeightedBudgetPerMille(0.37) {
		t.Fatalf("确定性：同 density 两次应相等")
	}
}

// TestIsHighAnchorDensity 验证高锚阈值（>0.5）。
func TestIsHighAnchorDensity(t *testing.T) {
	if isHighAnchorDensity(0.5) {
		t.Fatalf("0.5 不应判高锚（阈值是 >0.5）")
	}
	if !isHighAnchorDensity(0.51) {
		t.Fatalf("0.51 应判高锚")
	}
	if isHighAnchorDensity(0) {
		t.Fatalf("零锚不应判高锚")
	}
}

// TestEmitAnchorWeightedEvent_Trail 验证高锚 NPC 命中时写 ReasonAnchorWeightedEvent 留痕（含 budget/density payload）。
func TestEmitAnchorWeightedEvent_Trail(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	// FK：actor_unit_id 引用真实单位，故先存一个单位。
	repo := unit.NewRepository(r.db)
	rec := unit.BootstrapRecord(7, "s1", "player", "她")
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("存单位失败: %v", err)
	}

	r.emitAnchorWeightedEvent(ctx, "s1", rec.ID, "region_north", 0.8, 42)

	var count int
	var payload string
	if err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(payload_json),'') FROM events WHERE reason_code = ?`,
		string(events.ReasonAnchorWeightedEvent)).Scan(&count, &payload); err != nil {
		t.Fatalf("查留痕失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("应有 1 条 ANCHOR_WEIGHTED_EVENT 留痕，得 %d", count)
	}
	if payload == "" {
		t.Fatalf("留痕 payload 不应为空")
	}
}

// TestEmitAnchorWeightedEvent_NilSafe 验证 db 缺失/空 unitID 时静默 no-op（best-effort，不 panic）。
func TestEmitAnchorWeightedEvent_NilSafe(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1})
	// 空 unitID → no-op。
	r.emitAnchorWeightedEvent(ctx, "s1", "", "region", 0.9, 1)
	var n int
	_ = r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE reason_code = ?`,
		string(events.ReasonAnchorWeightedEvent)).Scan(&n)
	if n != 0 {
		t.Fatalf("空 unitID 不应留痕，得 %d", n)
	}
	// nil Runner → no-op（不 panic）。
	var nilRunner *Runner
	nilRunner.emitAnchorWeightedEvent(context.Background(), "s1", "u1", "region", 0.9, 1)
}
