package regionrunner

// 文件说明：威胁刷新读 region.threat_level 真累积 + freshness 反扎堆的确定性测试。验证：
//   - 分片关 / registry==nil → 回退 threatBaseLevel 常量基线，与既有 threatSpawnPerMille 逐结果等价（零行为变化）；
//   - 分片开 + registry：region.threat_level 越高 → 阈值越高（威胁扎堆危险区）；
//   - freshness 反扎堆：同区刚命中后短期内阈值被压低、出窗口恢复，破圈下限恒保留；
//   - spawnPerMilleAtLevel/freshnessPerMilleFor 纯函数的单调/夹边界性质。
// 复用 regionrunner_test.go 的 newRunner helper；用 region.New(db)+UpsertRegion/BumpThreatLevel 造真实 region 行。

import (
	"testing"

	"qunxiang/backend/internal/region"
)

// TestSpawnPerMilleAtLevel_Properties 守护概率合成的单调/夹边界性质。
func TestSpawnPerMilleAtLevel_Properties(t *testing.T) {
	// base 单调：base 越高（freshness/anchor 固定满）阈值不降。
	lo := spawnPerMilleAtLevel(0, 0, 1000)
	hi := spawnPerMilleAtLevel(threatLevelMax, 0, 1000)
	if hi < lo {
		t.Fatalf("base 越高阈值应不降：base=0→%d base=max→%d", lo, hi)
	}
	// 破圈下限恒保留：base=0 + 锚 0 + freshness 压到 0 仍 ≥ floor。
	if v := spawnPerMilleAtLevel(0, 0, 0); v < threatFloorPerMille {
		t.Fatalf("破圈下限应恒保留，得 %d < %d", v, threatFloorPerMille)
	}
	// 上限封顶：base/anchor 拉满也不超 max。
	if v := spawnPerMilleAtLevel(threatLevelMax, 1, 1000); v > threatMaxPerMille {
		t.Fatalf("应夹上限 %d，得 %d", threatMaxPerMille, v)
	}
	// freshness 单调：同 base/anchor，freshness 千分比越小阈值越低（压制越强），但不破下限。
	full := spawnPerMilleAtLevel(threatLevelMax, 1, 1000)
	half := spawnPerMilleAtLevel(threatLevelMax, 1, 500)
	zero := spawnPerMilleAtLevel(threatLevelMax, 1, 0)
	if !(zero <= half && half <= full) {
		t.Fatalf("freshness 越小阈值应越低：zero=%d half=%d full=%d", zero, half, full)
	}
	if zero < threatFloorPerMille {
		t.Fatalf("freshness 压到 0 也应≥破圈下限，得 %d", zero)
	}
	// threat_level 超量/负值夹到 [0,max]。
	if spawnPerMilleAtLevel(threatLevelMax+999, 0, 1000) != spawnPerMilleAtLevel(threatLevelMax, 0, 1000) {
		t.Fatal("threat_level 超量应封顶到 max")
	}
	if spawnPerMilleAtLevel(-50, 0, 1000) != spawnPerMilleAtLevel(0, 0, 1000) {
		t.Fatal("threat_level 负值应视为 0")
	}
}

// TestThreatSpawnPerMille_FallbackEquivalence 守护回退版与常量基线等价（既有测试/触发链零行为变化）。
func TestThreatSpawnPerMille_FallbackEquivalence(t *testing.T) {
	for _, d := range []float64{0, 0.25, 0.5, 0.75, 1.0} {
		want := spawnPerMilleAtLevel(threatBaseLevel, d, 1000)
		if got := threatSpawnPerMille(d); got != want {
			t.Fatalf("density=%.2f：threatSpawnPerMille=%d 应等于常量基线合成 %d", d, got, want)
		}
	}
}

// TestFreshnessPerMilleFor_Refractory 守护 freshness refractory：Δ=0 最强压制、窗口内线性回升、出窗口恢复、无记录不衰减。
func TestFreshnessPerMilleFor_Refractory(t *testing.T) {
	if v := freshnessPerMilleFor(0, 0, false); v != 1000 {
		t.Fatalf("无最近命中应不衰减(1000)，得 %d", v)
	}
	if v := freshnessPerMilleFor(100, 100, true); v != threatFreshnessMinPerMille {
		t.Fatalf("Δtick=0 应最强压制(%d)，得 %d", threatFreshnessMinPerMille, v)
	}
	if v := freshnessPerMilleFor(100, 100+threatFreshnessWindowTicks, true); v != 1000 {
		t.Fatalf("出窗口应完全恢复(1000)，得 %d", v)
	}
	// 窗口内单调回升。
	prev := -1
	for d := int64(0); d <= threatFreshnessWindowTicks; d++ {
		v := freshnessPerMilleFor(100, 100+d, true)
		if v < prev {
			t.Fatalf("窗口内 freshness 应单调回升：Δ=%d v=%d prev=%d", d, v, prev)
		}
		prev = v
	}
	// 时钟回拨保护：tick < lastHit 视为刚命中（最强压制）。
	if v := freshnessPerMilleFor(100, 50, true); v != threatFreshnessMinPerMille {
		t.Fatalf("时钟回拨应视为刚命中(%d)，得 %d", threatFreshnessMinPerMille, v)
	}
}

// TestResolveSpawnThreshold_FallbackWhenShardingOff：分片关 → 回退常量基线（与 threatSpawnPerMille 等价），不读 registry。
func TestResolveSpawnThreshold_FallbackWhenShardingOff(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	// 即便注入了 registry，flag 关时也不消费 → 仍回退常量基线。
	r.SetRegistry(region.New(r.db))
	for _, d := range []float64{0, 0.5, 1.0} {
		if got := r.resolveSpawnThreshold(ctx, "s1", d, 1); got != threatSpawnPerMille(d) {
			t.Fatalf("分片关应回退常量基线：density=%.1f got=%d want=%d", d, got, threatSpawnPerMille(d))
		}
	}
}

// TestResolveSpawnThreshold_ReadsRealThreatLevel：分片开 + registry → 读 region.threat_level 真累积值，越高阈值越高。
func TestResolveSpawnThreshold_ReadsRealThreatLevel(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "true")
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	reg := region.New(r.db)
	r.SetRegistry(reg)
	const regionID = "w1#1"
	if err := reg.UpsertRegion(ctx, regionID, "w1"); err != nil {
		t.Fatalf("upsert region: %v", err)
	}

	// threat_level=0（刚建档）：应等于 base=0 的合成（且 ≥ 破圈下限）。无 freshness 记录 → 不衰减。
	zero := r.resolveSpawnThreshold(ctx, regionID, 0, 1)
	if want := spawnPerMilleAtLevel(0, 0, 1000); zero != want {
		t.Fatalf("threat_level=0 阈值应=%d，得 %d", want, zero)
	}

	// 累积 threat_level 到上限：阈值应升高（威胁扎堆危险区）。
	if _, err := reg.BumpThreatLevel(ctx, regionID, threatLevelMax); err != nil {
		t.Fatalf("bump: %v", err)
	}
	hot := r.resolveSpawnThreshold(ctx, regionID, 0, 1)
	if hot <= zero {
		t.Fatalf("threat_level 累积后阈值应升高（扎堆）：zero=%d hot=%d", zero, hot)
	}
	if want := spawnPerMilleAtLevel(threatLevelMax, 0, 1000); hot != want {
		t.Fatalf("threat_level=max 阈值应=%d，得 %d", want, hot)
	}

	// 未登记 region → 回退常量基线（不报错、不破触发链）。
	if got := r.resolveSpawnThreshold(ctx, "w1#unregistered", 0, 1); got != threatSpawnPerMille(0) {
		t.Fatalf("未登记 region 应回退常量基线 %d，得 %d", threatSpawnPerMille(0), got)
	}
}

// TestFreshnessAntiClustering：分片开下，同区刚命中（recordThreatHit）后短期内阈值被压低、出窗口恢复，破圈下限恒保留。
func TestFreshnessAntiClustering(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "true")
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	reg := region.New(r.db)
	r.SetRegistry(reg)
	const regionID = "w1#1"
	_ = reg.UpsertRegion(ctx, regionID, "w1")
	_, _ = reg.BumpThreatLevel(ctx, regionID, threatLevelMax) // 拉满 base，让 freshness 衰减空间最大、效果可见

	const hitTick = 100
	beforeHit := r.resolveSpawnThreshold(ctx, regionID, 1.0, hitTick) // 命中前（无 recency）→ 不衰减
	r.recordThreatHit(regionID, hitTick)

	justAfter := r.resolveSpawnThreshold(ctx, regionID, 1.0, hitTick) // 紧贴命中（Δ=0）→ 最强压制
	if justAfter >= beforeHit {
		t.Fatalf("同区刚命中应压低阈值（反扎堆）：before=%d justAfter=%d", beforeHit, justAfter)
	}
	if justAfter < threatFloorPerMille {
		t.Fatalf("freshness 压制也应≥破圈下限 %d，得 %d", threatFloorPerMille, justAfter)
	}

	// 出 refractory 窗口 → 完全恢复到命中前阈值。
	recovered := r.resolveSpawnThreshold(ctx, regionID, 1.0, hitTick+threatFreshnessWindowTicks)
	if recovered != beforeHit {
		t.Fatalf("出窗口应恢复到命中前阈值：before=%d recovered=%d", beforeHit, recovered)
	}

	// 窗口内单调回升。
	prev := justAfter
	for d := int64(1); d <= threatFreshnessWindowTicks; d++ {
		v := r.resolveSpawnThreshold(ctx, regionID, 1.0, hitTick+d)
		if v < prev {
			t.Fatalf("窗口内阈值应单调回升：Δ=%d v=%d prev=%d", d, v, prev)
		}
		prev = v
	}
}

// TestRecordThreatHit_MonotonicAndGated：recordThreatHit 单调取最大 tick；分片关时不写 recency（不污染、零行为变化）。
func TestRecordThreatHit_MonotonicAndGated(t *testing.T) {
	t.Setenv(regionShardingFlagEnv, "true")
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	r.SetRegistry(region.New(r.db))
	const regionID = "w1#1"
	_ = region.New(r.db).UpsertRegion(ctx, regionID, "w1")

	r.recordThreatHit(regionID, 100)
	r.recordThreatHit(regionID, 50) // 更旧 → 不应回拨
	if v, ok := r.threatRecency.Load(regionID); !ok || v.(int64) != 100 {
		t.Fatalf("recordThreatHit 应单调取最大 tick(100)，得 %v ok=%v", v, ok)
	}
	r.recordThreatHit(regionID, 200) // 更新 → 应推进
	if v, _ := r.threatRecency.Load(regionID); v.(int64) != 200 {
		t.Fatalf("更新 tick 应推进到 200，得 %v", v)
	}

	// 分片关：不写 recency。
	r2, _ := newRunner(t, Config{TickSeconds: 1, Threats: true}) // 无 t.Setenv 影响？同一测试内 env 已开，故显式关。
	t.Setenv(regionShardingFlagEnv, "")
	r2.SetRegistry(region.New(r2.db))
	r2.recordThreatHit("w1#x", 100)
	if _, ok := r2.threatRecency.Load("w1#x"); ok {
		t.Fatal("分片关时不应写 recency")
	}
}
