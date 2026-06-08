package regionrunner

// 文件说明：region 分片**接线**主循环的确定性测试（固定时钟 + 真实 SQLite）。
// 验证 schedulePass→AcquireLease→region-scoped 认领的端到端互斥：
//   - flag 开：两个不同 instanceID 的 runner 共用一张 DB、对同一 region 只有一个能 AcquireLease 进而处理，
//     另一个跳过（让位）；持租实例只认领自己持租 region 的作业；释放后区锁可转手给另一实例。
//   - flag 关（默认）：单实例照常处理所有 region，零行为变化（不跳过、走全局 ClaimNextJob）。
// 测试前缀 wire* 避免与既有 regionrunner/sharding 测试撞名。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// openWireDB 起一个临时 SQLite（含全部建表），供多个 runner 实例共享同一连接（模拟多实例分片同一世界库）。
func openWireDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "shard_wire.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newWireRunner 在给定（可共享的）db 上起一个固定时钟的 runner；每个实例自带独立 instanceID（New 生成 uuid）。
func newWireRunner(t *testing.T, db *sql.DB, cfg Config) *Runner {
	t.Helper()
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	return New(db, cfg, nil).withClock(func() time.Time { return fixed })
}

// seedWireUnit 在共享 db 建一个活跃单位（指定 hunger，会饿则觅食 → hunger 上升，便于断言「谁处理了它」）。
func seedWireUnit(t *testing.T, db *sql.DB, ctx context.Context, id, sessionID string, hunger int) {
	t.Helper()
	repo := unit.NewRepository(db)
	rec := unit.BootstrapRecord(1, sessionID, "player", "分片单位")
	rec.ID = id
	rec.Status.Hunger = hunger
	rec.Status.LifeState = unit.LifeStateActive
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
}

func wireUnitHunger(t *testing.T, db *sql.DB, ctx context.Context, id string) int {
	t.Helper()
	rec, err := unit.NewRepository(db).GetByID(ctx, id)
	if err != nil {
		t.Fatalf("读单位失败: %v", err)
	}
	return rec.Status.Hunger
}

// TestWireSchedulePassRegionLeaseMutualExclusion：flag 开时，两个共用 DB 的 runner 实例对同一 region
// 只有先调度的那个抢到租约并入队/处理其作业，另一个 schedulePass 时跳过该 region（让位、不入队、不处理）。
func TestWireSchedulePassRegionLeaseMutualExclusion(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "true")
	db := openWireDB(t)
	ctx := context.Background()

	// region == sessionID（MVP 阶段-0 口径）。两实例共享 db。
	const region = "s1"
	cfg := Config{TickSeconds: 1, Apply: true, LeaseTTL: 30 * time.Second}
	rA := newWireRunner(t, db, cfg)
	rB := newWireRunner(t, db, cfg)
	if rA.instanceID == rB.instanceID {
		t.Fatal("两实例应有不同 instanceID")
	}

	tick := rA.currentTick()
	seedWireUnit(t, db, ctx, "u1", region, 20) // 饿（<40）→ 处理它会觅食、hunger 升到 50

	// u1 到点。
	if err := agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "u1", SessionID: region, RegionID: region, WakeAtTick: tick}); err != nil {
		t.Fatalf("enqueue wake: %v", err)
	}

	// 让 A 先直接持租该 region（wake 仍在队列、未被消费）——模拟「A 正在处理该区」。
	// 注：若先跑 rA.schedulePass 会把 wake 提升为 job 消费掉，rB 再调度时 DistinctWakeRegions 已空、
	// 根本看不到该 region（也就无从触发跳过）；故直接预持租以确定性地复现「region 仍 due 但被他人持租」。
	heldA, err := rA.leases.AcquireLease(ctx, region, rA.instanceID, cfg.LeaseTTL)
	if err != nil || !heldA {
		t.Fatalf("rA 应先持有 region 租约: held=%v err=%v", heldA, err)
	}

	// 实例 B 调度 → 同 region 被 A 持租，B 看到 due region 但抢不到 → 跳过、不入队、不持租。
	enqB, err := rB.schedulePass(ctx)
	if err != nil {
		t.Fatalf("rB schedulePass: %v", err)
	}
	if enqB != 0 {
		t.Fatalf("rB 应因 region 被 A 持租而跳过、不入队，得到 %d", enqB)
	}
	if rB.Stats()["regions_skipped"].(int64) != 1 {
		t.Fatalf("rB 应记一次 region 跳过")
	}
	if rB.Stats()["regions_held"].(int) != 0 {
		t.Fatalf("rB 不应持租任何 region")
	}

	// 实例 A 调度 → 重入自己的租约（AcquireLease 的 holder=self 谓词命中）、入队 u1 作业。
	enqA, err := rA.schedulePass(ctx)
	if err != nil {
		t.Fatalf("rA schedulePass: %v", err)
	}
	if enqA != 1 {
		t.Fatalf("rA 应抢到 region 并入队 1 条，得到 %d", enqA)
	}
	if rA.Stats()["leases_acquired"].(int64) != 1 {
		t.Fatalf("rA 应记一次抢租")
	}
	if rA.Stats()["regions_held"].(int) != 1 {
		t.Fatalf("rA 应持租 1 个 region")
	}

	// 持租者 A 能 region-scoped 认领并处理它的作业（觅食 → hunger 升）。
	if worked, err := rA.processOne(ctx); err != nil || !worked {
		t.Fatalf("rA 应处理一条自己持租 region 的作业: worked=%v err=%v", worked, err)
	}
	if h := wireUnitHunger(t, db, ctx, "u1"); h != 20+forageGain {
		t.Fatalf("A 处理后 u1 应觅食到 %d，得到 %d", 20+forageGain, h)
	}

	// B 没持租任何 region → region-scoped 认领取不到作业（即便队列里有，也不属于 B 持租的区）。
	// 此处队列已被 A 认领空，断言 B 取不到（nil）即可：核心是 B 不会去碰 A 的 region。
	if worked, err := rB.processOne(ctx); err != nil || worked {
		t.Fatalf("rB 无持租 region 不应认领到作业: worked=%v err=%v", worked, err)
	}
}

// TestWireScopedClaimOnlyHeldRegion：flag 开时 region-scoped 认领严格只取本实例持租 region 的作业——
// 队列里有两个 region 的作业，实例只持租其一，则只认领该区的，另一区作业留给别的实例。
func TestWireScopedClaimOnlyHeldRegion(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "true")
	db := openWireDB(t)
	ctx := context.Background()
	cfg := Config{TickSeconds: 1, Apply: true, LeaseTTL: 30 * time.Second}

	rA := newWireRunner(t, db, cfg)
	rB := newWireRunner(t, db, cfg)
	tick := rA.currentTick()

	seedWireUnit(t, db, ctx, "uA", "rgA", 20)
	seedWireUnit(t, db, ctx, "uB", "rgB", 20)
	_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "uA", SessionID: "rgA", RegionID: "rgA", WakeAtTick: tick})
	_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "uB", SessionID: "rgB", RegionID: "rgB", WakeAtTick: tick})

	// A 先抢 rgA（DistinctWakeRegions 顺序不保证，故 A 先调度抢到它**遇到的**所有空闲 region）。
	if _, err := rA.schedulePass(ctx); err != nil {
		t.Fatalf("rA schedulePass: %v", err)
	}
	// B 再调度，只能抢到 A 没抢的 region。
	if _, err := rB.schedulePass(ctx); err != nil {
		t.Fatalf("rB schedulePass: %v", err)
	}

	// A 持租的 region 总和 + B 持租的 region 总和 == 2，且互不相交（同一 region 不会两实例都持租）。
	heldA := rA.Stats()["regions_held"].(int)
	heldB := rB.Stats()["regions_held"].(int)
	if heldA+heldB != 2 {
		t.Fatalf("两实例应合计持租 2 个 region（各自不相交），得到 A=%d B=%d", heldA, heldB)
	}
	// 确认无重叠：A 持租集合与 B 持租集合无交集。
	for _, ra := range rA.heldRegionList() {
		for _, rb := range rB.heldRegionList() {
			if ra.RegionID == rb.RegionID {
				t.Fatalf("region %s 被两实例同时持租，违反 per-region 单写者", ra.RegionID)
			}
		}
	}

	// 每个实例 processOne 只会处理自己持租 region 的单位。两实例各处理到上限后，两个单位都应被处理过一次（觅食）。
	for i := 0; i < 4; i++ {
		_, _ = rA.processOne(ctx)
		_, _ = rB.processOne(ctx)
	}
	if h := wireUnitHunger(t, db, ctx, "uA"); h != 20+forageGain {
		t.Fatalf("uA 应被其持租实例处理（觅食到 %d），得到 %d", 20+forageGain, h)
	}
	if h := wireUnitHunger(t, db, ctx, "uB"); h != 20+forageGain {
		t.Fatalf("uB 应被其持租实例处理（觅食到 %d），得到 %d", 20+forageGain, h)
	}
}

// TestWireReleaseTransfersRegion：持租实例释放后，另一实例下个 schedulePass 能接管该 region。
func TestWireReleaseTransfersRegion(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "true")
	db := openWireDB(t)
	ctx := context.Background()
	const region = "s1"
	cfg := Config{TickSeconds: 1, Apply: true, LeaseTTL: 30 * time.Second}
	rA := newWireRunner(t, db, cfg)
	rB := newWireRunner(t, db, cfg)
	tick := rA.currentTick()

	seedWireUnit(t, db, ctx, "u1", region, 20)
	_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "u1", SessionID: region, RegionID: region, WakeAtTick: tick})

	// A 抢到 region。
	if _, err := rA.schedulePass(ctx); err != nil {
		t.Fatalf("rA schedulePass: %v", err)
	}
	// B 此刻抢不到。
	if _, err := rB.schedulePass(ctx); err != nil {
		t.Fatalf("rB schedulePass: %v", err)
	}
	if rB.Stats()["regions_held"].(int) != 0 {
		t.Fatal("B 在 A 持租期间不应持租")
	}

	// A 释放它持租的所有 region。
	rA.releaseHeldRegions()
	if rA.Stats()["regions_held"].(int) != 0 {
		t.Fatal("A 释放后不应再持租")
	}

	// 给 B 一个新的到点单位（A 已把 u1 出队，需新 wake 才有可入队的活）。
	seedWireUnit(t, db, ctx, "u2", region, 20)
	_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "u2", SessionID: region, RegionID: region, WakeAtTick: tick})

	// B 再调度 → 现在能接管 region。
	enqB, err := rB.schedulePass(ctx)
	if err != nil {
		t.Fatalf("rB 接管 schedulePass: %v", err)
	}
	if enqB != 1 {
		t.Fatalf("A 释放后 B 应接管 region 并入队 1 条，得到 %d", enqB)
	}
	if rB.Stats()["regions_held"].(int) != 1 {
		t.Fatal("B 接管后应持租 1 个 region")
	}
}

// TestWireDisabledFlagSingleInstanceUnchanged：flag 关（默认）→ 单实例照常处理所有 region，零行为变化：
// 不跳过任何 region、走全局 ClaimNextJob 路径、leases_enabled=false、regions_skipped 恒 0。
func TestWireDisabledFlagSingleInstanceUnchanged(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "") // 显式关
	db := openWireDB(t)
	ctx := context.Background()
	cfg := Config{TickSeconds: 1, Apply: true}
	r := newWireRunner(t, db, cfg)
	tick := r.currentTick()

	// 两个不同 region 的到点单位。
	seedWireUnit(t, db, ctx, "uA", "rgA", 20)
	seedWireUnit(t, db, ctx, "uB", "rgB", 20)
	_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "uA", SessionID: "rgA", RegionID: "rgA", WakeAtTick: tick})
	_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "uB", SessionID: "rgB", RegionID: "rgB", WakeAtTick: tick})

	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 2 {
		t.Fatalf("flag 关时应处理所有 region、入队 2 条，得到 %d", enq)
	}
	if r.Stats()["leases_enabled"].(bool) {
		t.Fatal("flag 关时 leases_enabled 应为 false")
	}
	if r.Stats()["regions_skipped"].(int64) != 0 {
		t.Fatalf("flag 关时不应跳过任何 region，得到 %d", r.Stats()["regions_skipped"].(int64))
	}

	// 全局 ClaimNextJob 路径：两条作业都能被同一实例处理（不受 region 持租限制）。
	worked1, _ := r.processOne(ctx)
	worked2, _ := r.processOne(ctx)
	if !worked1 || !worked2 {
		t.Fatalf("flag 关时单实例应处理两条作业，得到 %v %v", worked1, worked2)
	}
	if h := wireUnitHunger(t, db, ctx, "uA"); h != 20+forageGain {
		t.Fatalf("uA 应被处理（觅食到 %d），得到 %d", 20+forageGain, h)
	}
	if h := wireUnitHunger(t, db, ctx, "uB"); h != 20+forageGain {
		t.Fatalf("uB 应被处理（觅食到 %d），得到 %d", 20+forageGain, h)
	}
}
