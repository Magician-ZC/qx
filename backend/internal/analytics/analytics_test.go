package analytics

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return context.Background(), db
}

func TestEmitAndCount(t *testing.T) {
	ctx, db := newDB(t)
	if err := Emit(ctx, db, Event{Stage: StageRetention, Name: EventDecisionPending, SessionID: "s1", UnitID: "u1", Props: map[string]any{"decision_id": "d1"}}); err != nil {
		t.Fatalf("emit 失败: %v", err)
	}
	if err := Emit(ctx, db, Event{Stage: StageRetention, Name: EventDecisionResolved, SessionID: "s1", UnitID: "u1"}); err != nil {
		t.Fatalf("emit2 失败: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM product_events WHERE session_id = ?`, "s1").Scan(&n); err != nil {
		t.Fatalf("count 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("应有 2 条漏斗埋点，得到 %d", n)
	}
	// nil props 应落 '{}'，空 unit_id 应为 NULL。
	var props string
	var unitID sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT properties_json, unit_id FROM product_events WHERE event_name = ?`, EventDecisionResolved).Scan(&props, &unitID); err != nil {
		t.Fatalf("scan 失败: %v", err)
	}
	if props != "{}" {
		t.Fatalf("nil props 应序列化为 {}，得到 %q", props)
	}
}

func TestEmitValidation(t *testing.T) {
	ctx, db := newDB(t)
	if err := Emit(ctx, db, Event{Name: "x"}); err == nil {
		t.Fatalf("空 stage 应报错")
	}
	if err := Emit(ctx, db, Event{Stage: StageActivation}); err == nil {
		t.Fatalf("空 name 应报错")
	}
}
