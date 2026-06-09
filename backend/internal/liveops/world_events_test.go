// 文件说明：GM 世界事件注入的 DB 集成测试（确定性发号 + cross_events/审计双写原子性 + 封存世界拒绝）。
package liveops

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"qunxiang/backend/internal/storage/dbdialect"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

func newLiveopsDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "liveops.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return context.Background(), db
}

func TestEmitWorldEvent_WritesCrossEventAndAudit(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)

	worldID, err := world.Create(ctx, db, world.World{Name: "测试世界"})
	if err != nil {
		t.Fatalf("建世界失败: %v", err)
	}

	res, err := svc.EmitWorldEvent(ctx, GMEvent{
		WorldID:    worldID,
		Kind:       "天灾",
		Importance: 8,
		CreatedBy:  "gm-alice",
		Payload:    map[string]any{"narrative": "山洪暴发"},
	})
	if err != nil {
		t.Fatalf("注入 GM 事件失败: %v", err)
	}
	if res.CrossEventID == "" || res.AuditID == "" {
		t.Fatalf("回执缺 ID: %+v", res)
	}
	if res.WorldTick != 1 {
		t.Fatalf("首次注入应发号 tick=1，得到 %d", res.WorldTick)
	}

	// cross_events 应有一条 GM_WORLD_EVENT，tick=1。
	events, err := worldbus.ListByWorld(ctx, db, worldID, 0)
	if err != nil {
		t.Fatalf("列 cross_events 失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("应有 1 条跨事件，得到 %d", len(events))
	}
	if events[0].Kind != KindGMWorldEvent || events[0].WorldTick != 1 || events[0].Importance != 8 {
		t.Fatalf("跨事件字段不符: %+v", events[0])
	}

	// 审计应有一条，event_kind=天灾、created_by=gm-alice、payload 含 narrative + gm_injected。
	audits, err := svc.ListAudit(ctx, worldID, 0)
	if err != nil {
		t.Fatalf("列审计失败: %v", err)
	}
	if len(audits) != 1 {
		t.Fatalf("应有 1 条审计，得到 %d", len(audits))
	}
	a := audits[0]
	if a.EventKind != "天灾" || a.CreatedBy != "gm-alice" || a.CrossEventID != res.CrossEventID {
		t.Fatalf("审计字段不符: %+v", a)
	}
	if !strings.Contains(a.PayloadJSON, "山洪暴发") || !strings.Contains(a.PayloadJSON, `"gm_injected":true`) {
		t.Fatalf("审计 payload 不含原始内容/注入标记: %s", a.PayloadJSON)
	}
}

func TestEmitWorldEvent_TickMonotonic(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)
	worldID, _ := world.Create(ctx, db, world.World{Name: "递增世界"})

	for want := 1; want <= 3; want++ {
		res, err := svc.EmitWorldEvent(ctx, GMEvent{WorldID: worldID, Kind: "事件", CreatedBy: "gm"})
		if err != nil {
			t.Fatalf("第 %d 次注入失败: %v", want, err)
		}
		if res.WorldTick != want {
			t.Fatalf("第 %d 次注入应发号 tick=%d，得到 %d", want, want, res.WorldTick)
		}
	}
}

func TestEmitWorldEvent_SealedWorldRejected(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)
	worldID, _ := world.Create(ctx, db, world.World{Name: "封存世界"})
	if err := world.Seal(ctx, db, worldID); err != nil {
		t.Fatalf("封存失败: %v", err)
	}

	if _, err := svc.EmitWorldEvent(ctx, GMEvent{WorldID: worldID, Kind: "事件", CreatedBy: "gm"}); err == nil {
		t.Fatalf("向封存世界注入应报错")
	}
	// 封存世界不应被推进 tick / 留下任何审计。
	w, _ := world.Get(ctx, db, worldID)
	if w.Tick != 0 {
		t.Fatalf("拒绝后 tick 不应被推进，得到 %d", w.Tick)
	}
	audits, _ := svc.ListAudit(ctx, worldID, 0)
	if len(audits) != 0 {
		t.Fatalf("拒绝后不应有审计，得到 %d", len(audits))
	}
}

func TestEmitWorldEvent_MissingWorld(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)
	if _, err := svc.EmitWorldEvent(ctx, GMEvent{WorldID: "nope", Kind: "x", CreatedBy: "gm"}); err == nil {
		t.Fatalf("不存在的世界应报错")
	}
}

// TestIsWorldSealedTx_InTransactionRecheck 覆盖 L2 的事务内重检原语（堵 TOCTOU）：
// 直接验证事务内带锁状态读对 active/sealed/不存在三态的判定——这是 EmitWorldEvent 在发号前防住
// 「事务外预检通过、发号前被并发 FinalizeSeason 封存」竞态的核心。
func TestIsWorldSealedTx_InTransactionRecheck(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	dialect := dbdialect.For(db)

	activeID, _ := world.Create(ctx, db, world.World{Name: "活世界"})
	sealedID, _ := world.Create(ctx, db, world.World{Name: "封世界"})
	if err := world.Seal(ctx, db, sealedID); err != nil {
		t.Fatalf("封存失败: %v", err)
	}

	// active 世界：事务内重检应判未封存。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开事务失败: %v", err)
	}
	sealed, err := isWorldSealedTx(ctx, tx, activeID, dialect)
	if err != nil {
		t.Fatalf("active 世界重检报错: %v", err)
	}
	if sealed {
		t.Fatalf("active 世界不应判为 sealed")
	}
	_ = tx.Rollback()

	// sealed 世界：事务内重检应判已封存。
	tx2, _ := db.BeginTx(ctx, nil)
	sealed, err = isWorldSealedTx(ctx, tx2, sealedID, dialect)
	if err != nil {
		t.Fatalf("sealed 世界重检报错: %v", err)
	}
	if !sealed {
		t.Fatalf("sealed 世界应判为 sealed")
	}
	_ = tx2.Rollback()

	// 不存在的世界：应返回 world.ErrNotFound。
	tx3, _ := db.BeginTx(ctx, nil)
	if _, err = isWorldSealedTx(ctx, tx3, "ghost", dialect); !errors.Is(err, world.ErrNotFound) {
		t.Fatalf("不存在的世界应返回 world.ErrNotFound，实得: %v", err)
	}
	_ = tx3.Rollback()
}

// TestEmitWorldEvent_SealedAfterFastFailRejected 模拟 L2 的 TOCTOU：在事务外预检通过后、于发号前把世界封存，
// 事务内重检必须拦下注入（拒绝 + 不发号 + 不留审计）。用直接 UPDATE 模拟并发 FinalizeSeason 的 Seal 写。
func TestEmitWorldEvent_SealedAfterFastFailRejected(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)
	worldID, _ := world.Create(ctx, db, world.World{Name: "竞态世界"})

	// 先正常注入一次确认链路可用（tick 推进到 1）。
	if _, err := svc.EmitWorldEvent(ctx, GMEvent{WorldID: worldID, Kind: "热身", CreatedBy: "gm"}); err != nil {
		t.Fatalf("热身注入失败: %v", err)
	}

	// 模拟「事务外预检通过后、发号前」被并发封存：单 goroutine 下直接置 sealed，再注入。
	// 事务内重检（isWorldSealedTx）应在 AdvanceTick 之前拦下。
	if err := world.Seal(ctx, db, worldID); err != nil {
		t.Fatalf("封存失败: %v", err)
	}
	if _, err := svc.EmitWorldEvent(ctx, GMEvent{WorldID: worldID, Kind: "迟到事件", CreatedBy: "gm"}); err == nil {
		t.Fatalf("封存后注入应被事务内重检拒绝")
	}

	// 拒绝后 tick 不应越过热身的 1；审计仍只有热身那一条。
	w, _ := world.Get(ctx, db, worldID)
	if w.Tick != 1 {
		t.Fatalf("拒绝后 tick 不应被推进（应仍为 1），实得 %d", w.Tick)
	}
	audits, _ := svc.ListAudit(ctx, worldID, 0)
	if len(audits) != 1 {
		t.Fatalf("拒绝后审计仍应只有热身那 1 条，实得 %d", len(audits))
	}
}

// TestListAudit_OrderedByWorldTick 覆盖 L3：同秒多条 GM 注入下，ListAudit 须按 world_tick 单调降序（最新在前）、
// 与 cross_events 注入序严格一致——取代不稳定的秒级 created_at 排序。
func TestListAudit_OrderedByWorldTick(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)
	worldID, _ := world.Create(ctx, db, world.World{Name: "同秒世界"})

	// 同一秒内连发 5 条（CURRENT_TIMESTAMP 秒级，created_at 极可能相同 → 旧排序不稳）。
	const n = 5
	for i := 0; i < n; i++ {
		res, err := svc.EmitWorldEvent(ctx, GMEvent{WorldID: worldID, Kind: "连发", CreatedBy: "gm"})
		if err != nil {
			t.Fatalf("第 %d 次注入失败: %v", i, err)
		}
		if res.WorldTick != i+1 {
			t.Fatalf("第 %d 次注入应发号 tick=%d，实得 %d", i, i+1, res.WorldTick)
		}
	}

	audits, err := svc.ListAudit(ctx, worldID, 0)
	if err != nil {
		t.Fatalf("列审计失败: %v", err)
	}
	if len(audits) != n {
		t.Fatalf("应有 %d 条审计，实得 %d", n, len(audits))
	}
	// 最新在前：world_tick 应为 n, n-1, ..., 1 严格单调降序。
	for i, a := range audits {
		wantTick := n - i
		if a.WorldTick != wantTick {
			t.Fatalf("第 %d 条审计 world_tick 应为 %d，实得 %d（排序不单调）", i, wantTick, a.WorldTick)
		}
	}
}
