package regionrunner

// 文件说明：region.Registry 接 region-runner schedulePass 的确定性接线测试（§11.3「多实例真分片」）。
// 验证 QUNXIANG_REGION_SHARDING 开时：
//   - schedulePass 按活跃度档（HOT/WARM）优先选区（COLD/未登记区经 DistinctWakeRegions 兜底也不饿死）；
//   - 抢到的 region 经 AdvanceRegionTick 推进 per-region 逻辑时钟（last_tick 单调上升）；
//   - 威胁命中经 BumpThreatLevel 累计到 region.threat_level（威胁扎堆）。
// 以及 flag 关时回归零行为变化（不消费 registry、不推 tick、region 集合仍来自 DistinctWakeRegions）。
// 复用 region_sharding_wire_test.go 的 openWireDB / newWireRunner / seedWireUnit 辅助。

import (
	"context"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/region"
)

// TestWireRegistrySharding_TierSelectionAndTickAdvance：flag 开 + 注入 registry → schedulePass 选 HOT/WARM 区、
// 推进其 per-region 逻辑时钟（AdvanceRegionTick），并入队/处理其作业。
func TestWireRegistrySharding_TierSelectionAndTickAdvance(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "true")
	t.Setenv(regionLeasesFlagEnv, "") // 分片与租约正交：本例只测分片，租约关（单实例不互斥）。
	db := openWireDB(t)
	ctx := context.Background()
	const world = "w1"
	const regionID = "w1#3" // 子区 id（worldID#shardN 形态）

	reg := region.New(db)
	if err := reg.UpsertRegion(ctx, regionID, world); err != nil {
		t.Fatalf("登记 region: %v", err)
	}
	if err := reg.SetActivityTier(ctx, regionID, region.TierHot); err != nil {
		t.Fatalf("标 HOT 档: %v", err)
	}

	cfg := Config{TickSeconds: 1, Apply: true, LeaseTTL: 30 * time.Second}
	r := newWireRunner(t, db, cfg)
	r.SetRegistry(reg)

	tick := r.currentTick()
	seedWireUnit(t, db, ctx, "u1", regionID, 20) // 饿（<40）→ 觅食、hunger 升
	if err := agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{
		UnitID: "u1", SessionID: "s1", WorldID: world, RegionID: regionID, WakeAtTick: tick,
	}); err != nil {
		t.Fatalf("enqueue wake: %v", err)
	}

	// selectRegions 应从 ListByTier(HOT) 取到该区。
	regions, err := r.selectRegions(ctx)
	if err != nil {
		t.Fatalf("selectRegions: %v", err)
	}
	found := false
	for _, rg := range regions {
		if rg.RegionID == regionID && rg.WorldID == world {
			found = true
		}
	}
	if !found {
		t.Fatalf("分片开时 selectRegions 应按 HOT 档选到 %s（世界 %s），得到 %+v", regionID, world, regions)
	}

	// schedulePass：抢区、推进 per-region tick、入队作业。
	tickBefore, err := reg.GetRegion(ctx, regionID)
	if err != nil {
		t.Fatalf("读 region: %v", err)
	}
	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 1 {
		t.Fatalf("应入队 1 条作业，得到 %d", enq)
	}
	tickAfter, err := reg.GetRegion(ctx, regionID)
	if err != nil {
		t.Fatalf("读 region: %v", err)
	}
	if tickAfter.LastTick <= tickBefore.LastTick {
		t.Fatalf("schedulePass 应推进 per-region 逻辑时钟（AdvanceRegionTick）：before=%d after=%d", tickBefore.LastTick, tickAfter.LastTick)
	}
	if r.Stats()["sharding_enabled"].(bool) != true {
		t.Fatal("flag 开 + registry 注入时 sharding_enabled 应为 true")
	}

	// 处理作业（觅食）。
	if worked, err := r.processOne(ctx); err != nil || !worked {
		t.Fatalf("应处理 1 条作业: worked=%v err=%v", worked, err)
	}
	if h := wireUnitHunger(t, db, ctx, "u1"); h != 20+forageGain {
		t.Fatalf("处理后 u1 应觅食到 %d，得到 %d", 20+forageGain, h)
	}
}

// TestWireRegistrySharding_WarmAndFallback：HOT 区先选，WARM 区次之，未登记/COLD 区经 DistinctWakeRegions 兜底也被选到（不饿死）。
func TestWireRegistrySharding_WarmAndFallback(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "true")
	db := openWireDB(t)
	ctx := context.Background()
	const world = "w1"

	reg := region.New(db)
	// hotR：HOT 档；warmR：WARM 档；coldR：未登记（仅入唤醒队列，靠兜底被发现）。
	_ = reg.UpsertRegion(ctx, "w1#1", world)
	_ = reg.SetActivityTier(ctx, "w1#1", region.TierHot)
	_ = reg.UpsertRegion(ctx, "w1#2", world)
	_ = reg.SetActivityTier(ctx, "w1#2", region.TierWarm)

	r := newWireRunner(t, db, Config{TickSeconds: 1, Apply: true})
	r.SetRegistry(reg)
	tick := r.currentTick()

	for _, rg := range []string{"w1#1", "w1#2", "w1#9"} { // w1#9 未登记
		seedWireUnit(t, db, ctx, "u-"+rg, rg, 20)
		_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "u-" + rg, SessionID: "s1", WorldID: world, RegionID: rg, WakeAtTick: tick})
	}

	regions, err := r.selectRegions(ctx)
	if err != nil {
		t.Fatalf("selectRegions: %v", err)
	}
	if len(regions) < 2 {
		t.Fatalf("应至少选到 HOT+WARM 两区，得到 %d", len(regions))
	}
	// HOT 区应排在 WARM 之前（顺序断言：先 HOT 再 WARM）。
	if regions[0].RegionID != "w1#1" {
		t.Fatalf("HOT 区应优先排首位，得到 %s", regions[0].RegionID)
	}
	// 未登记区 w1#9 应经兜底被选到（不饿死）。
	got := map[string]bool{}
	for _, rg := range regions {
		got[rg.RegionID] = true
	}
	if !got["w1#9"] {
		t.Fatalf("未登记区 w1#9 应经 DistinctWakeRegions 兜底被选到，得到 %+v", regions)
	}
	// 无重复。
	if len(got) != len(regions) {
		t.Fatalf("selectRegions 不应有重复 region：%+v", regions)
	}
}

// TestWireRegistrySharding_DisabledZeroChange：flag 关（默认）→ 即使注入了 registry，schedulePass 仍走
// DistinctWakeRegions 旧路径、不推进 per-region tick，零行为变化。
func TestWireRegistrySharding_DisabledZeroChange(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "") // 显式关
	db := openWireDB(t)
	ctx := context.Background()
	const world = "w1"
	const regionID = "w1#3"

	reg := region.New(db)
	_ = reg.UpsertRegion(ctx, regionID, world)
	_ = reg.SetActivityTier(ctx, regionID, region.TierHot)

	r := newWireRunner(t, db, Config{TickSeconds: 1, Apply: true})
	r.SetRegistry(reg) // 注入但 flag 关 → 不消费

	tick := r.currentTick()
	// 注意：region_id 用 sessionID 口径（rgX），模拟分片关时的旧 region_id；HOT 档的 w1#3 没有 wake → 不应被选。
	seedWireUnit(t, db, ctx, "uX", "rgX", 20)
	_ = agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{UnitID: "uX", SessionID: "rgX", WorldID: world, RegionID: "rgX", WakeAtTick: tick})

	// selectRegions 应只来自 DistinctWakeRegions（=只有 rgX），不含 registry 标 HOT 的 w1#3。
	regions, err := r.selectRegions(ctx)
	if err != nil {
		t.Fatalf("selectRegions: %v", err)
	}
	if len(regions) != 1 || regions[0].RegionID != "rgX" {
		t.Fatalf("flag 关时 selectRegions 应只来自 DistinctWakeRegions（rgX），得到 %+v", regions)
	}

	before, _ := reg.GetRegion(ctx, regionID)
	if _, err := r.schedulePass(ctx); err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	after, _ := reg.GetRegion(ctx, regionID)
	if after.LastTick != before.LastTick {
		t.Fatalf("flag 关时不应推进任何 per-region tick：before=%d after=%d", before.LastTick, after.LastTick)
	}
	if r.Stats()["sharding_enabled"].(bool) != false {
		t.Fatal("flag 关时 sharding_enabled 应为 false")
	}
}

// TestWireRegistrySharding_PromotesActiveRegionToHot 是 B-1 回归：生产侧把 region 升出 COLD 的唯一触发点。
// 一个未登记（COLD 语义）但有到点 wake 的 region，首个 schedulePass 经 DistinctWakeRegions 兜底被处理后，
// reconcileRegionTier 应把它 UpsertRegion + 标 HOT；于是下个 pass 的 selectRegions 的 tier 优先路径就能发现它。
// 没有这一步，ListByTier(HOT/WARM) 恒空、tier 优先调度全程死路（分片开等价于分片关）。
func TestWireRegistrySharding_PromotesActiveRegionToHot(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "true")
	t.Setenv(regionLeasesFlagEnv, "")
	db := openWireDB(t)
	ctx := context.Background()
	const world = "w1"
	const regionID = "w1#7" // 故意不预登记，模拟 seed 出来的、registry 尚未标档的活跃区

	reg := region.New(db)
	r := newWireRunner(t, db, Config{TickSeconds: 1, Apply: true, LeaseTTL: 30 * time.Second})
	r.SetRegistry(reg)

	tick := r.currentTick()
	seedWireUnit(t, db, ctx, "u7", regionID, 20)
	if err := agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{
		UnitID: "u7", SessionID: "s1", WorldID: world, RegionID: regionID, WakeAtTick: tick,
	}); err != nil {
		t.Fatalf("enqueue wake: %v", err)
	}

	// 处理前：region 未登记 → ListByTier(HOT) 不含它（tier 优先路径发现不了它，只能靠兜底）。
	hotBefore, err := reg.ListByTier(ctx, "", region.TierHot, 0)
	if err != nil {
		t.Fatalf("ListByTier(HOT) before: %v", err)
	}
	for _, rg := range hotBefore {
		if rg.ID == regionID {
			t.Fatalf("处理前 %s 不应已在 HOT 档", regionID)
		}
	}

	// 首个 pass：经 DistinctWakeRegions 兜底处理该区、入队作业，并把它升 HOT。
	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 1 {
		t.Fatalf("应入队 1 条作业，得到 %d", enq)
	}

	// 处理后：region 应已被 UpsertRegion + 标 HOT。
	got, err := reg.GetRegion(ctx, regionID)
	if err != nil {
		t.Fatalf("处理后 region 应已登记，GetRegion: %v", err)
	}
	if got.ActivityTier != region.TierHot {
		t.Fatalf("活跃区处理后应升 HOT，得到 %q", got.ActivityTier)
	}

	// 下个 pass 的 selectRegions 的 tier 优先路径现在能发现它（不再仅靠兜底）。
	hotAfter, err := reg.ListByTier(ctx, "", region.TierHot, 0)
	if err != nil {
		t.Fatalf("ListByTier(HOT) after: %v", err)
	}
	foundHot := false
	for _, rg := range hotAfter {
		if rg.ID == regionID {
			foundHot = true
		}
	}
	if !foundHot {
		t.Fatalf("升 HOT 后 ListByTier(HOT) 应含 %s，得到 %+v", regionID, hotAfter)
	}
}

// TestWireRegistrySharding_DemotesIdleRegionToCold 是 B-1 配套：空 pass（无到点 wake）的已登记 HOT 区应降回 COLD，
// 让 tier 焦点收敛到真活跃区（空区仍由 DistinctWakeRegions 兜底，不会丢失后续 wake 的处理机会）。
func TestWireRegistrySharding_DemotesIdleRegionToCold(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "true")
	t.Setenv(regionLeasesFlagEnv, "")
	db := openWireDB(t)
	ctx := context.Background()
	const world = "w1"
	const regionID = "w1#8"

	reg := region.New(db)
	if err := reg.UpsertRegion(ctx, regionID, world); err != nil {
		t.Fatalf("登记 region: %v", err)
	}
	if err := reg.SetActivityTier(ctx, regionID, region.TierHot); err != nil {
		t.Fatalf("标 HOT: %v", err)
	}

	r := newWireRunner(t, db, Config{TickSeconds: 1, Apply: true, LeaseTTL: 30 * time.Second})
	r.SetRegistry(reg)

	// 该 HOT 区无任何到点 wake → schedulePass 处理它时 regionEnqueued==0 → 应降 COLD。
	if _, err := r.schedulePass(ctx); err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	got, err := reg.GetRegion(ctx, regionID)
	if err != nil {
		t.Fatalf("GetRegion: %v", err)
	}
	if got.ActivityTier != region.TierCold {
		t.Fatalf("空 pass 的 HOT 区应降回 COLD，得到 %q", got.ActivityTier)
	}
}

// TestShardRegionIDDeterministic：ShardRegionID 确定性 + worldID 空回退 fallback。
func TestShardRegionIDDeterministic(t *testing.T) {
	// 同 (world, unit) 恒落同一子区。
	a := agentqueue.ShardRegionID("w1", "u1", "s1", agentqueue.DefaultShardCount)
	b := agentqueue.ShardRegionID("w1", "u1", "s1", agentqueue.DefaultShardCount)
	if a != b {
		t.Fatalf("ShardRegionID 应确定性：%s != %s", a, b)
	}
	if a == "s1" {
		t.Fatalf("有世界时不应回退 sessionID，得到 %s", a)
	}
	// worldID 空 → 回退 fallback（sessionID）。
	if got := agentqueue.ShardRegionID("", "u1", "s1", agentqueue.DefaultShardCount); got != "s1" {
		t.Fatalf("worldID 空应回退 fallback s1，得到 %s", got)
	}
	// 不同 world 前缀不同子区（不撞）。
	if agentqueue.ShardRegionID("w1", "u1", "s1", 16) == agentqueue.ShardRegionID("w2", "u1", "s1", 16) {
		t.Fatal("不同世界的同号子区不应撞 id")
	}
}
