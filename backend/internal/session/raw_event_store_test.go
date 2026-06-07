package session

// 文件说明：原始事件日志旁路表 cutover 测试（对真实 SQLite）：
//  - persist 幂等、可读回、空 ID 不写；
//  - cutover round-trip（blob 不再含 raw event、load 从表 hydrate 保序）；
//  - legacy 回填、hydrate merge 残留、定宽排序；
//  - 隐私擦除审计链路同步清表、保留期清理删表、GetAuditBundle 经 hydrate 非空。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPersistRawEventsIdempotentAndSkipsEmptyID(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	ev := RawEventEntry{ID: "e1", ActorUnitID: "u1", Source: "combat", Kind: "engage", Summary: "她出手了", PayloadJSON: `{"dmg":5}`}
	if err := persistRawEvents(ctx, service.db, false, "s1", []RawEventEntry{ev}); err != nil {
		t.Fatalf("写表失败: %v", err)
	}
	if err := persistRawEvents(ctx, service.db, false, "s1", []RawEventEntry{ev}); err != nil {
		t.Fatalf("幂等重写失败: %v", err)
	}
	list, err := service.ListRawEvents(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	if len(list) != 1 || list[0].ID != "e1" || list[0].PayloadJSON != `{"dmg":5}` {
		t.Fatalf("事件应可完整读回、幂等只 1 条，得到 %+v", list)
	}
	_ = persistRawEvents(ctx, service.db, false, "s1", []RawEventEntry{{ID: ""}})
	if again, _ := service.ListRawEvents(ctx, "s1", 10); len(again) != 1 {
		t.Fatalf("空 ID 不应写入，仍应 1 条，得到 %d", len(again))
	}
}

func TestRawEventCutoverRoundTrip(t *testing.T) {
	service, ctx := newCutoverService(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	state := &State{ID: "s1"}
	for i := 0; i < 3; i++ {
		state.RawEventLog = append(state.RawEventLog, RawEventEntry{
			ID: "e" + string(rune('0'+i)), ActorUnitID: "u", Source: "combat", Summary: "act" + string(rune('0'+i)),
			OccurredAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}

	// cutover：blob 不应再含 raw event。
	var blob string
	if err := service.db.QueryRowContext(ctx, `SELECT state_json FROM single_player_sessions WHERE id = 's1'`).Scan(&blob); err != nil {
		t.Fatalf("读 blob 失败: %v", err)
	}
	if strings.Contains(blob, `"e0"`) || strings.Contains(blob, "act0") {
		t.Fatalf("cutover 后 blob 不应再 marshal 原始事件，却含 e0")
	}

	// load 应从表 hydrate，按时间正序。
	loaded, _, err := service.loadSession(ctx, "s1")
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(loaded.RawEventLog) != 3 || loaded.RawEventLog[0].ID != "e0" || loaded.RawEventLog[2].ID != "e2" {
		t.Fatalf("应从表 hydrate 出 3 条且正序，得到 %+v", loaded.RawEventLog)
	}
}

func TestRawEventLegacyBackfill(t *testing.T) {
	service, ctx := newCutoverService(t)
	legacy := &State{ID: "s2", RawEventLog: []RawEventEntry{
		{ID: "old1", ActorUnitID: "u", Source: "weather", Summary: "下雨了", OccurredAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)},
	}}
	enc, _ := json.Marshal(legacy)
	nowTS := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := service.db.ExecContext(ctx, `INSERT INTO single_player_sessions (id, state_json, created_at, updated_at) VALUES ('s2', ?, ?, ?)`, string(enc), nowTS, nowTS); err != nil {
		t.Fatalf("注入旧局失败: %v", err)
	}
	loaded, _, err := service.loadSession(ctx, "s2")
	if err != nil {
		t.Fatalf("加载旧局失败: %v", err)
	}
	if len(loaded.RawEventLog) != 1 || loaded.RawEventLog[0].ID != "old1" {
		t.Fatalf("旧局事件应被回填+hydrate，得到 %+v", loaded.RawEventLog)
	}
	if list, _ := service.ListRawEvents(ctx, "s2", 10); len(list) != 1 || list[0].ID != "old1" {
		t.Fatalf("旧事件应已回填进表，得到 %+v", list)
	}
}

func TestRawEventHydrateMergeKeepsBlobResidue(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := persistRawEvents(ctx, service.db, false, "s1", []RawEventEntry{{ID: "e1", ActorUnitID: "u", OccurredAt: base}}); err != nil {
		t.Fatalf("写表失败: %v", err)
	}
	state := &State{ID: "s1", RawEventLog: []RawEventEntry{
		{ID: "e1", ActorUnitID: "u", OccurredAt: base},
		{ID: "e2", ActorUnitID: "u", Summary: "残留", OccurredAt: base.Add(time.Second)},
	}}
	service.hydrateRawEvents(ctx, state)
	ids := map[string]bool{}
	for _, ev := range state.RawEventLog {
		ids[ev.ID] = true
	}
	if !ids["e1"] || !ids["e2"] {
		t.Fatalf("blob 残留 e2 应被并入、绝不丢，得到 %+v", state.RawEventLog)
	}
	list, _ := service.ListRawEvents(ctx, "s1", 10)
	found := false
	for _, ev := range list {
		if ev.ID == "e2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("blob 残留 e2 应被回填进表")
	}
}

func TestRawEventOrderingFixedWidth(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := RawEventEntry{ID: "a", ActorUnitID: "u", OccurredAt: base}
	b := RawEventEntry{ID: "b", ActorUnitID: "u", OccurredAt: base.Add(500 * time.Millisecond)}
	if err := persistRawEvents(ctx, service.db, false, "s1", []RawEventEntry{b, a}); err != nil {
		t.Fatalf("写表失败: %v", err)
	}
	list, err := service.ListRawEvents(ctx, "s1", 10)
	if err != nil {
		t.Fatalf("读表失败: %v", err)
	}
	if len(list) != 2 || list[0].ID != "b" || list[1].ID != "a" {
		t.Fatalf("定宽时间序应让整秒(a)排在小数秒(b)之后，得到 %+v", list)
	}
}

func TestPrivacyEraseClearsRawEventSideTable(t *testing.T) {
	service, ctx := newCutoverService(t)
	state := &State{ID: "s1", RawEventLog: []RawEventEntry{
		{ID: "e1", ActorUnitID: "u", Source: "combat", Summary: "敏感事件", PayloadJSON: `{"x":1}`, OccurredAt: time.Now().UTC()},
	}}
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if list, _ := service.ListRawEvents(ctx, "s1", 10); len(list) != 1 {
		t.Fatalf("前置：raw_event_log 应有 1 条，得到 %d", len(list))
	}
	// 擦除审计链路应同步清空 raw_event_log 表。
	if _, _, err := service.EraseSessionPrivateData(ctx, "s1", PrivacyEraseOptions{EraseAuditTrail: true}); err != nil {
		t.Fatalf("擦除失败: %v", err)
	}
	if list, _ := service.ListRawEvents(ctx, "s1", 10); len(list) != 0 {
		t.Fatalf("擦除审计链路后 raw_event_log 表应为空，仍有 %d 条", len(list))
	}
}

func TestPurgeDeletesRawEventSideTable(t *testing.T) {
	service, ctx := newCutoverService(t)
	oldTS := time.Now().UTC().Add(-90 * 24 * time.Hour).Format(time.RFC3339Nano)
	enc, _ := json.Marshal(&State{ID: "old"})
	if _, err := service.db.ExecContext(ctx, `INSERT INTO single_player_sessions (id, state_json, created_at, updated_at) VALUES ('old', ?, ?, ?)`, string(enc), oldTS, oldTS); err != nil {
		t.Fatalf("注入过期会话失败: %v", err)
	}
	if err := persistRawEvents(ctx, service.db, false, "old", []RawEventEntry{{ID: "e1", ActorUnitID: "u"}}); err != nil {
		t.Fatalf("注入 raw event 留痕失败: %v", err)
	}
	res, err := service.PurgeExpiredSessionData(ctx, 30, 100)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if res.RawEventsDeleted != 1 {
		t.Fatalf("应删 1 条 raw event 留痕，得到 %d", res.RawEventsDeleted)
	}
	if list, _ := service.ListRawEvents(ctx, "old", 10); len(list) != 0 {
		t.Fatalf("清理后 raw_event_log 表应为空，仍有 %d 条", len(list))
	}
}

func TestGetAuditBundleHydratesRawEventsAfterCutover(t *testing.T) {
	service, ctx := newCutoverService(t)
	state := &State{ID: "s1", RawEventLog: []RawEventEntry{
		{ID: "e1", ActorUnitID: "u", Source: "combat", Summary: "事件", OccurredAt: time.Now().UTC()},
	}}
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if got, err := service.sessions.Get(ctx, "s1"); err != nil || len(got.RawEventLog) != 0 {
		t.Fatalf("前置：cutover 后裸 Get 应读不到 raw event，得到 %d 条 (err=%v)", len(got.RawEventLog), err)
	}
	bundle, err := service.GetAuditBundle(ctx, "s1", 50)
	if err != nil {
		t.Fatalf("取审计包失败: %v", err)
	}
	if len(bundle.RawEventLog) != 1 || bundle.RawEventLog[0].ID != "e1" {
		t.Fatalf("审计包应经 hydrate 拿到 raw event，得到 %+v", bundle.RawEventLog)
	}
}
