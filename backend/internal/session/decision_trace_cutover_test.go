package session

// 文件说明：决策轨迹拆 state_json 读路径切换测试（对真实 SQLite）：
// save 后 blob 不再带轨迹、load 从表 hydrate 且保序；现网旧局（blob 里有轨迹）首次 load 自动回填。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newCutoverService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "cutover.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewServiceWithColdStore(db, nil, nil), context.Background()
}

func TestDecisionTraceCutoverRoundTrip(t *testing.T) {
	service, ctx := newCutoverService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	state := &State{ID: "s1"}
	for i := 0; i < 3; i++ {
		tr := DecisionTrace{ID: "t" + string(rune('0'+i)), UnitID: "u", NextAction: "act" + string(rune('0'+i)), OccurredAt: base.Add(time.Duration(i) * time.Second)}
		state.DecisionTraces = append(state.DecisionTraces, tr)
		service.shadowDecisionTrace(ctx, "s1", tr) // 真实流程：append 时影子写表
	}

	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// blob 不应再含轨迹。
	var blob string
	if err := service.db.QueryRowContext(ctx, `SELECT state_json FROM single_player_sessions WHERE id = 's1'`).Scan(&blob); err != nil {
		t.Fatalf("读 blob 失败: %v", err)
	}
	if strings.Contains(blob, `"t0"`) || strings.Contains(blob, "act0") {
		t.Fatalf("blob 不应再 marshal 决策轨迹（已瘦身），却含 t0")
	}

	// load 应从表 hydrate，且按时间正序。
	loaded, _, err := service.loadSession(ctx, "s1")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(loaded.DecisionTraces) != 3 {
		t.Fatalf("应从表 hydrate 出 3 条轨迹，得到 %d", len(loaded.DecisionTraces))
	}
	if loaded.DecisionTraces[0].ID != "t0" || loaded.DecisionTraces[2].ID != "t2" {
		t.Fatalf("hydrate 应按时间正序：%s..%s", loaded.DecisionTraces[0].ID, loaded.DecisionTraces[2].ID)
	}
}

func TestDecisionTraceOrderingFixedWidth(t *testing.T) {
	service, ctx := newCutoverService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 关键回归：A 在整秒（无小数）、B 在 :00.5。RFC3339Nano 变宽会把 A 排到 B 后面（'.'<'Z'）；定宽布局修正。
	a := DecisionTrace{ID: "a", UnitID: "u", OccurredAt: base}
	b := DecisionTrace{ID: "b", UnitID: "u", OccurredAt: base.Add(500 * time.Millisecond)}
	service.shadowDecisionTrace(ctx, "s1", b) // 故意乱序写
	service.shadowDecisionTrace(ctx, "s1", a)

	state := &State{ID: "s1"}
	service.hydrateDecisionTraces(ctx, state)
	if len(state.DecisionTraces) != 2 || state.DecisionTraces[0].ID != "a" || state.DecisionTraces[1].ID != "b" {
		t.Fatalf("整秒应排在小数秒之前（定宽时间序），得到 %+v", state.DecisionTraces)
	}
}

func TestHydrateMergeKeepsBlobResidue(t *testing.T) {
	service, ctx := newCutoverService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// 表里只有 t1；blob 里有 t1 + t2（t2 模拟影子写失败/旧局残留，只在 blob）。
	service.shadowDecisionTrace(ctx, "s1", DecisionTrace{ID: "t1", UnitID: "u", OccurredAt: base})
	state := &State{ID: "s1", DecisionTraces: []DecisionTrace{
		{ID: "t1", UnitID: "u", OccurredAt: base},
		{ID: "t2", UnitID: "u", OccurredAt: base.Add(time.Second)},
	}}
	service.hydrateDecisionTraces(ctx, state)
	ids := map[string]bool{}
	for _, tr := range state.DecisionTraces {
		ids[tr.ID] = true
	}
	if !ids["t1"] || !ids["t2"] {
		t.Fatalf("blob 残留轨迹 t2 应被并入、绝不丢，得到 %+v", state.DecisionTraces)
	}
	// t2 应已回填进表。
	list, _ := service.ListDecisionTraces(ctx, "s1", 10)
	found := false
	for _, tr := range list {
		if tr.ID == "t2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("blob 残留应被回填进表")
	}
}

func TestDecisionTraceLegacyBackfill(t *testing.T) {
	service, ctx := newCutoverService(t)

	// 模拟现网旧局：blob 里直接带轨迹，表为空（切换前存的）。
	legacy := &State{ID: "s2", DecisionTraces: []DecisionTrace{
		{ID: "old1", UnitID: "u", NextAction: "旧动作", OccurredAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
	}}
	enc, _ := json.Marshal(legacy)
	nowTS := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := service.db.ExecContext(ctx, `INSERT INTO single_player_sessions (id, state_json, created_at, updated_at) VALUES ('s2', ?, ?, ?)`, string(enc), nowTS, nowTS); err != nil {
		t.Fatalf("注入旧局失败: %v", err)
	}

	// 首次 load：应回填旧轨迹进表并 hydrate。
	loaded, _, err := service.loadSession(ctx, "s2")
	if err != nil {
		t.Fatalf("加载旧局失败: %v", err)
	}
	if len(loaded.DecisionTraces) != 1 || loaded.DecisionTraces[0].ID != "old1" {
		t.Fatalf("旧局轨迹应被回填+hydrate，得到 %+v", loaded.DecisionTraces)
	}
	// 表里现在应有这条（已回填）。
	list, _ := service.ListDecisionTraces(ctx, "s2", 10)
	if len(list) != 1 || list[0].ID != "old1" {
		t.Fatalf("旧轨迹应已回填进表，得到 %+v", list)
	}
}
