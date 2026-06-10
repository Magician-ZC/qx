package regionrunner

// 文件说明：共享世界 Phase 5「统一世界推进」在 region-runner 侧的端到端测试。
// 验证核心红线 ①：中心化 region-runner 把**共享世界主角（LifecycleProtagonist）**纳入按真·地理子区
// （复合 region_id=worldID#zoneID）的唤醒调度——共享主角离线也活，与同区单位在同一世界 tick 节拍 co-tick。
//
// region-runner 本就 session-agnostic、按 region_id 调度、对单位生命周期分级无感（任何 active 单位只要在某 region 有
// 到点 wake 就被唤醒、跑一拍 ambient 自治）。本测试构造 session 侧 Phase 5 接线后的 DB 事实：主角单位的 region_id 列
// 与 agent_wake_queue 的 region_id 都对齐到复合 region，然后驱动一次 schedulePass + processOne，断言：
//   - 调度 pass 在复合 region 下发现并入队该主角（DistinctWakeRegions→ListDueWakes(复合 region) 命中）；
//   - processOne 跑一拍 ambient 自治并经 Mutator 改了它的状态（离线也活）。
//
// 与 session 包测试的分工：session 侧（shared_world_phase5_test.go）验「降生即把 wake 对齐复合 region」这条接线；
// 本测试验「对齐后 region-runner 真把共享主角唤醒、跑活」这条引擎行为。两者合起来覆盖红线 ① 全链。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/unit"
)

// seedSharedProtagonist 在 runner 的 db 里建一个**共享世界主角**单位：LifecycleProtagonist、active、scope 到复合 region，
// 并按复合 region 入队一条到点 wake（模拟 session 侧 Phase 5 接线后的 DB 事实）。返回单位 ID。
func seedSharedProtagonist(t *testing.T, r *Runner, ctx context.Context, id, sessionID, worldID, regionID string, hunger int) {
	t.Helper()
	repo := unit.NewRepository(r.db)
	rec := unit.BootstrapRecord(1, sessionID, "player", "共享世界主角")
	rec.ID = id
	rec.Identity.LifecycleClass = unit.LifecycleProtagonist // 玩家控制角色（永生、跨时代）
	rec.Status.Hunger = hunger
	rec.Status.LifeState = unit.LifeStateActive
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存共享主角失败: %v", err)
	}
	// scope 到复合 region（与 session.scopeSharedWorldUnitsToZoneBestEffort 同口径）。
	if err := repo.SetUnitScope(ctx, id, worldID, regionID); err != nil {
		t.Fatalf("scope 共享主角失败: %v", err)
	}
	if err := repo.SetLifeState(ctx, id, unit.LifeStateActive); err != nil {
		t.Fatalf("置共享主角 life_state 失败: %v", err)
	}
	// 按复合 region 入队到点 wake（与 session.requeueSharedWorldWakeBestEffort 同口径：wake.region_id==复合 region）。
	if err := agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{
		UnitID: id, SessionID: sessionID, WorldID: worldID, RegionID: regionID, WakeAtTick: 0, Tier: "cold",
	}); err != nil {
		t.Fatalf("入队共享主角 wake 失败: %v", err)
	}
}

// TestSharedWorldProtagonistWokenAndAlive 验证红线 ①：region-runner 把对齐到复合 region 的共享主角唤醒、跑一拍自治。
func TestSharedWorldProtagonistWokenAndAlive(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()

	const (
		worldID  = "world_shared_v1"
		zoneID   = "zone_neutral_start"
		regionID = worldID + "#" + zoneID // 复合 region_id（session.sharedRegionID 同形）
		sessID   = "sess-A"
		unitID   = "hero-A"
	)
	seedSharedProtagonist(t, r, ctx, unitID, sessID, worldID, regionID, 20) // 饿（<40）→ 觅食可观测增益

	// 前置：调度的 region 枚举源应能发现复合 region（否则 region-runner 根本不会调度该区）。
	regions, err := agentqueue.DistinctWakeRegions(ctx, r.db)
	if err != nil {
		t.Fatalf("DistinctWakeRegions 失败: %v", err)
	}
	if len(regions) != 1 || regions[0].RegionID != regionID {
		t.Fatalf("调度源应只发现复合 region %q，得到 %+v", regionID, regions)
	}

	// 调度一拍：复合 region 下到点的共享主角应被入队。
	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 1 {
		t.Fatalf("共享主角应被入队 1 条，得到 %d", enq)
	}

	// 处理：跑一拍 ambient 自治（饿 → 觅食），经 Mutator 改 hunger（离线也活）。
	worked, err := r.processOne(ctx)
	if err != nil || !worked {
		t.Fatalf("应处理共享主角的一拍自治: worked=%v err=%v", worked, err)
	}
	got := unit.Record{}
	if got, err = unit.NewRepository(r.db).GetByID(ctx, unitID); err != nil {
		t.Fatalf("读共享主角失败: %v", err)
	}
	if got.Status.Hunger != 20+forageGain {
		t.Fatalf("共享主角饿则觅食应补口粮到 %d（离线也活），得到 %d", 20+forageGain, got.Status.Hunger)
	}
	if r.Stats()["foraged"].(int64) != 1 {
		t.Fatalf("应记一次共享主角觅食")
	}
	// 觅食是主动响应 → HOT 重排到复合 region（保持在地理子区下被持续调度、与同区 co-tick）。
	due, _ := agentqueue.ListDueWakes(ctx, r.db, worldID, regionID, tick+1, 10)
	if len(due) != 1 || due[0].UnitID != unitID || due[0].RegionID != regionID || due[0].Tier != "hot" {
		t.Fatalf("觅食后共享主角应在复合 region %q 下 HOT 重排，得到 %+v", regionID, due)
	}
}

// TestSharedWorldProtagonistColdRegionStillScheduled 验证：即便共享主角只是低频 COLD 单位（满意/不饿，跑反思/休息），
// region-runner 仍按复合 region 把它调度起来（离线也活不要求它一直 HOT）。这覆盖「共享主角日常多在 COLD 也持续被纳入」。
func TestSharedWorldProtagonistColdRegionStillScheduled(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})

	const (
		worldID  = "world_shared_v1"
		regionID = worldID + "#zone_freedom_capital"
		sessID   = "sess-B"
		unitID   = "hero-B"
	)
	seedSharedProtagonist(t, r, ctx, unitID, sessID, worldID, regionID, 80) // 不饿 → 休息（被动、降温 COLD）

	if _, err := r.schedulePass(ctx); err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	worked, err := r.processOne(ctx)
	if err != nil || !worked {
		t.Fatalf("不饿的共享主角也应被调度并跑一拍（休息），worked=%v err=%v", worked, err)
	}
	// 休息消耗少量 hunger（缓慢变饿，驱动下次觅食）——证明 COLD 单位也真被跑了一拍。
	got, err := unit.NewRepository(r.db).GetByID(ctx, unitID)
	if err != nil {
		t.Fatalf("读共享主角失败: %v", err)
	}
	if got.Status.Hunger != 80-restConsume {
		t.Fatalf("不饿的共享主角应休息消耗 hunger 到 %d，得到 %d", 80-restConsume, got.Status.Hunger)
	}
	if r.Stats()["rested"].(int64) != 1 {
		t.Fatalf("应记一次共享主角休息")
	}
}
