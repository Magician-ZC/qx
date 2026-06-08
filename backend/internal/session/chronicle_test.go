package session

// 文件说明：编年史物化（chronicle.go）的 DB 集成测试，对真实 SQLite 临时库跑通（参照 status 包/decision_trace 测试范式）：
// 落条目 → 读回（整局 / 按单位过滤）→ 「回到那一刻」锚点定位（关联回同 turn 同主角的事件）→ best-effort 吞错。

import (
	"context"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/events"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func newChronicleService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "chronicle.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewServiceWithColdStore(db, nil, nil), context.Background()
}

// seedChronicleUnit 落一个真实 unit 行——「回到那一刻」旁路的流程事件写 events 表，events 对 units(id) 有外键，
// 故测试同主角的锚点链路时需要单位先存在。返回其生成的 id（BootstrapRecord 自带 id）。
func seedChronicleUnit(t *testing.T, service *Service, ctx context.Context, seed int64, sessionID, name string) string {
	t.Helper()
	rec := unit.BootstrapRecord(seed, sessionID, "player", name)
	if err := service.units.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
	return rec.ID
}

// 落一条编年史 → 读回，且 best-effort 旁路了一条 CHRONICLE_RECORD 流程事件作为「回到那一刻」锚点。
func TestRecordAndListChronicle(t *testing.T) {
	service, ctx := newChronicleService(t)
	u1 := seedChronicleUnit(t, service, ctx, 2, "s1", "甲")

	id := service.recordChronicleEntry(ctx, "s1", u1, 7, "death", "她倒在了北境的雪原上")
	if id == "" {
		t.Fatalf("recordChronicleEntry 应返回非空 id")
	}

	entries, err := service.listChronicle(ctx, "s1", "", 0)
	if err != nil {
		t.Fatalf("listChronicle 出错: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("期望读回 1 条编年史，得到 %d", len(entries))
	}
	got := entries[0]
	if got.ID != id || got.SessionID != "s1" || got.UnitID != u1 || got.Turn != 7 ||
		got.Kind != "death" || got.Text != "她倒在了北境的雪原上" || got.CreatedAt == "" {
		t.Fatalf("读回条目字段不符: %+v", got)
	}

	// 旁路应落了一条 CHRONICLE_RECORD 流程事件（events 表），作为「回到那一刻」锚点。
	var n int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE session_id='s1' AND reason_code=?`,
		string(events.ReasonChronicleRecord)).Scan(&n); err != nil {
		t.Fatalf("统计 CHRONICLE_RECORD 事件失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("期望旁路 1 条 CHRONICLE_RECORD 流程事件，得到 %d", n)
	}
}

// 按单位过滤：listChronicle 传 unitID 只返回该单位的条目；空 unitID 返回整局。
func TestListChronicleByUnitFilter(t *testing.T) {
	service, ctx := newChronicleService(t)
	u1 := seedChronicleUnit(t, service, ctx, 2, "s1", "甲")
	u2 := seedChronicleUnit(t, service, ctx, 4, "s1", "乙")

	service.recordChronicleEntry(ctx, "s1", u1, 1, "birth", "u1 的第一笔")
	service.recordChronicleEntry(ctx, "s1", u1, 2, "battle", "u1 的第二笔")
	service.recordChronicleEntry(ctx, "s1", u2, 3, "birth", "u2 的一笔")
	service.recordChronicleEntry(ctx, "s2", u1, 4, "birth", "别局的一笔") // 不应混入 s1

	all, err := service.listChronicle(ctx, "s1", "", 0)
	if err != nil {
		t.Fatalf("listChronicle 整局出错: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("s1 整局期望 3 条，得到 %d", len(all))
	}

	onlyU1, err := service.listChronicle(ctx, "s1", u1, 0)
	if err != nil {
		t.Fatalf("listChronicle 过滤出错: %v", err)
	}
	if len(onlyU1) != 2 {
		t.Fatalf("s1/u1 期望 2 条，得到 %d", len(onlyU1))
	}
	for _, e := range onlyU1 {
		if e.UnitID != u1 {
			t.Fatalf("过滤后混入了非 u1 条目: %+v", e)
		}
	}
}

// 「回到那一刻」：anchorMoment 把一条编年史条目关联回它发生的 turn 上同主角的事件 id。
func TestAnchorMoment(t *testing.T) {
	service, ctx := newChronicleService(t)
	u1 := seedChronicleUnit(t, service, ctx, 2, "s1", "甲")
	u2 := seedChronicleUnit(t, service, ctx, 4, "s1", "乙")

	// 在 turn=5 给 u1 旁路两条流程事件（含 record 自身旁路的那条），turn=5 给 u2 一条、turn=6 给 u1 一条（不应命中）。
	id := service.recordChronicleEntry(ctx, "s1", u1, 5, "vengeance", "她了结了那桩旧怨") // 旁路 1 条 turn=5/u1 事件
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID: "s1", OwnerUnitID: u1, Code: events.ReasonChronicleRecord, Tick: 5,
	}); err != nil {
		t.Fatalf("旁路 turn5/u1 事件失败: %v", err)
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID: "s1", OwnerUnitID: u2, Code: events.ReasonChronicleRecord, Tick: 5,
	}); err != nil {
		t.Fatalf("旁路 turn5/u2 事件失败: %v", err)
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID: "s1", OwnerUnitID: u1, Code: events.ReasonChronicleRecord, Tick: 6,
	}); err != nil {
		t.Fatalf("旁路 turn6/u1 事件失败: %v", err)
	}

	entries, err := service.listChronicle(ctx, "s1", u1, 0)
	if err != nil {
		t.Fatalf("listChronicle 出错: %v", err)
	}
	var target ChronicleEntry
	for _, e := range entries {
		if e.ID == id {
			target = e
		}
	}
	if target.ID == "" {
		t.Fatalf("没读回刚落的编年史条目")
	}

	anchor, err := service.anchorMoment(ctx, target)
	if err != nil {
		t.Fatalf("anchorMoment 出错: %v", err)
	}
	if anchor.ChronicleID != id || anchor.Turn != 5 || anchor.UnitID != u1 {
		t.Fatalf("锚点头信息不符: %+v", anchor)
	}
	// turn=5 / 主角 u1 的事件应命中 2 条（record 自身旁路 + 手工旁路那条）；turn5/u2 与 turn6/u1 不命中。
	if len(anchor.EventIDs) != 2 {
		t.Fatalf("期望命中 2 条 turn5/u1 事件，得到 %d (%v)", len(anchor.EventIDs), anchor.EventIDs)
	}
}

// best-effort 吞错：缺 db / 空 sessionID / 空 text 都返回空串、不 panic、不报错。
func TestRecordChronicleBestEffortNoop(t *testing.T) {
	service, ctx := newChronicleService(t)
	if id := service.recordChronicleEntry(ctx, "", "u1", 1, "k", "t"); id != "" {
		t.Fatalf("空 sessionID 应返回空串")
	}
	if id := service.recordChronicleEntry(ctx, "s1", "u1", 1, "k", ""); id != "" {
		t.Fatalf("空 text 应返回空串")
	}
	var nilSvc *Service
	if id := nilSvc.recordChronicleEntry(ctx, "s1", "u1", 1, "k", "t"); id != "" {
		t.Fatalf("nil service 应返回空串、不 panic")
	}

	// 空局读回应是空切片、无错。
	got, err := service.listChronicle(ctx, "s1", "", 0)
	if err != nil {
		t.Fatalf("空局 listChronicle 不应报错: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("空局应读回 0 条，得到 %d", len(got))
	}
}
