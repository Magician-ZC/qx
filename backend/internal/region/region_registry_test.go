// 文件说明：region 注册表（registry.go）的聚焦测试——起临时 SQLite，断言 upsert 幂等、活跃度档切换、
// 威胁累积、per-region 逻辑时钟单调发号、按 tier 列举。命名带 region 前缀避免与并行 agent 撞名。
package region

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/storage/dbdialect"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newRegionRegistry(t *testing.T) (context.Context, *Registry) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "region.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// 显式注册 SQLite 方言，确保走 SQLite 分支（与生产侧 Register 口径一致）。
	dbdialect.Register(db, dbdialect.DialectSQLite)
	return context.Background(), New(db)
}

func TestRegionUpsertIdempotentAndGet(t *testing.T) {
	ctx, reg := newRegionRegistry(t)
	if err := reg.UpsertRegion(ctx, "region-1", "world-A"); err != nil {
		t.Fatalf("首次 upsert 失败: %v", err)
	}
	got, err := reg.GetRegion(ctx, "region-1")
	if err != nil {
		t.Fatalf("取 region 失败: %v", err)
	}
	if got.WorldID != "world-A" || got.ActivityTier != TierCold || got.ThreatLevel != 0 || got.LastTick != 0 {
		t.Fatalf("新建 region 字段不符: %+v", got)
	}

	// 先改活跃度与威胁，再重复 upsert（换 world_id）：upsert 只刷 world_id，不抹活跃度/威胁。
	if err := reg.SetActivityTier(ctx, "region-1", TierHot); err != nil {
		t.Fatalf("置 tier 失败: %v", err)
	}
	if _, err := reg.BumpThreatLevel(ctx, "region-1", 5); err != nil {
		t.Fatalf("累积威胁失败: %v", err)
	}
	if err := reg.UpsertRegion(ctx, "region-1", "world-B"); err != nil {
		t.Fatalf("重复 upsert 失败: %v", err)
	}
	got, _ = reg.GetRegion(ctx, "region-1")
	if got.WorldID != "world-B" {
		t.Fatalf("重复 upsert 应刷新 world_id 为 world-B，得到 %q", got.WorldID)
	}
	if got.ActivityTier != TierHot {
		t.Fatalf("重复 upsert 不应抹掉已设活跃度，期望 hot，得到 %q", got.ActivityTier)
	}
	if got.ThreatLevel != 5 {
		t.Fatalf("重复 upsert 不应抹掉已积累威胁，期望 5，得到 %d", got.ThreatLevel)
	}
}

func TestRegionGetNotFound(t *testing.T) {
	ctx, reg := newRegionRegistry(t)
	if _, err := reg.GetRegion(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在 region 应返回 ErrNotFound，得到 %v", err)
	}
	if err := reg.SetActivityTier(ctx, "nope", TierWarm); !errors.Is(err, ErrNotFound) {
		t.Fatalf("对不存在 region 置 tier 应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := reg.BumpThreatLevel(ctx, "nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("对不存在 region 累积威胁应返回 ErrNotFound，得到 %v", err)
	}
}

func TestRegionSetActivityTierTransitions(t *testing.T) {
	ctx, reg := newRegionRegistry(t)
	if err := reg.UpsertRegion(ctx, "r", "w"); err != nil {
		t.Fatalf("upsert 失败: %v", err)
	}
	for _, tier := range []Tier{TierHot, TierWarm, TierCold} {
		if err := reg.SetActivityTier(ctx, "r", tier); err != nil {
			t.Fatalf("置 tier %q 失败: %v", tier, err)
		}
		got, _ := reg.GetRegion(ctx, "r")
		if got.ActivityTier != tier {
			t.Fatalf("活跃度档切换不符，期望 %q，得到 %q", tier, got.ActivityTier)
		}
	}
	// 大写/带空白应被归一化接受。
	if err := reg.SetActivityTier(ctx, "r", Tier(" HOT ")); err != nil {
		t.Fatalf("归一化 tier 应被接受: %v", err)
	}
	got, _ := reg.GetRegion(ctx, "r")
	if got.ActivityTier != TierHot {
		t.Fatalf("归一化后期望 hot，得到 %q", got.ActivityTier)
	}
	// 非法 tier 应被拒（不落库）。
	if err := reg.SetActivityTier(ctx, "r", Tier("blistering")); err == nil {
		t.Fatalf("非法 tier 应被拒")
	}
}

func TestRegionBumpThreatAccumulates(t *testing.T) {
	ctx, reg := newRegionRegistry(t)
	if err := reg.UpsertRegion(ctx, "r", "w"); err != nil {
		t.Fatalf("upsert 失败: %v", err)
	}
	level, err := reg.BumpThreatLevel(ctx, "r", 3)
	if err != nil || level != 3 {
		t.Fatalf("第一次累积期望 3，得到 %d (err=%v)", level, err)
	}
	level, err = reg.BumpThreatLevel(ctx, "r", 4)
	if err != nil || level != 7 {
		t.Fatalf("累积应叠加到 7，得到 %d (err=%v)", level, err)
	}
	// 负 delta 衰减。
	level, err = reg.BumpThreatLevel(ctx, "r", -2)
	if err != nil || level != 5 {
		t.Fatalf("负 delta 应衰减到 5，得到 %d (err=%v)", level, err)
	}
	got, _ := reg.GetRegion(ctx, "r")
	if got.ThreatLevel != 5 {
		t.Fatalf("持久化威胁应为 5，得到 %d", got.ThreatLevel)
	}
}

func TestRegionAdvanceTickMonotonicAndPerRegion(t *testing.T) {
	ctx, reg := newRegionRegistry(t)
	// 未登记 region 也能发号（world_ticks 自建档）；首发号为 1。
	for want := int64(1); want <= 3; want++ {
		got, err := reg.AdvanceRegionTick(ctx, "w", "region-A")
		if err != nil {
			t.Fatalf("推进 region-A 时钟失败: %v", err)
		}
		if got != want {
			t.Fatalf("region-A 时钟应单调发号到 %d，得到 %d", want, got)
		}
	}
	// 不同 region 各自独立计时，互不串扰。
	got, err := reg.AdvanceRegionTick(ctx, "w", "region-B")
	if err != nil {
		t.Fatalf("推进 region-B 时钟失败: %v", err)
	}
	if got != 1 {
		t.Fatalf("region-B 首发号应为 1（独立于 region-A），得到 %d", got)
	}
	// 同名 region 不同 world 也独立（复合主键）。
	got, err = reg.AdvanceRegionTick(ctx, "w2", "region-A")
	if err != nil {
		t.Fatalf("推进 w2/region-A 时钟失败: %v", err)
	}
	if got != 1 {
		t.Fatalf("w2/region-A 首发号应为 1（独立于 w/region-A），得到 %d", got)
	}

	// 已登记的 region：last_tick 应被同步到最新发号值。
	if err := reg.UpsertRegion(ctx, "region-C", "w"); err != nil {
		t.Fatalf("upsert region-C 失败: %v", err)
	}
	if _, err := reg.AdvanceRegionTick(ctx, "w", "region-C"); err != nil {
		t.Fatalf("推进 region-C 失败: %v", err)
	}
	if _, err := reg.AdvanceRegionTick(ctx, "w", "region-C"); err != nil {
		t.Fatalf("推进 region-C 失败: %v", err)
	}
	gotReg, _ := reg.GetRegion(ctx, "region-C")
	if gotReg.LastTick != 2 {
		t.Fatalf("region-C.last_tick 应同步到 2，得到 %d", gotReg.LastTick)
	}
}

func TestRegionListByTier(t *testing.T) {
	ctx, reg := newRegionRegistry(t)
	mustUpsert := func(id, world string, tier Tier) {
		if err := reg.UpsertRegion(ctx, id, world); err != nil {
			t.Fatalf("upsert %s 失败: %v", id, err)
		}
		if err := reg.SetActivityTier(ctx, id, tier); err != nil {
			t.Fatalf("置 %s tier 失败: %v", id, err)
		}
	}
	mustUpsert("h1", "world-A", TierHot)
	mustUpsert("h2", "world-A", TierHot)
	mustUpsert("c1", "world-A", TierCold)
	mustUpsert("h3", "world-B", TierHot)

	hotA, err := reg.ListByTier(ctx, "world-A", TierHot, 0)
	if err != nil {
		t.Fatalf("按 tier 列举失败: %v", err)
	}
	if len(hotA) != 2 {
		t.Fatalf("world-A 的 hot region 应有 2 个，得到 %d", len(hotA))
	}
	for _, rg := range hotA {
		if rg.ActivityTier != TierHot || rg.WorldID != "world-A" {
			t.Fatalf("列举结果含非预期 region: %+v", rg)
		}
	}

	coldA, _ := reg.ListByTier(ctx, "world-A", TierCold, 0)
	if len(coldA) != 1 || coldA[0].ID != "c1" {
		t.Fatalf("world-A 的 cold region 应只有 c1，得到 %+v", coldA)
	}

	// 跨世界列举该档全部（worldID 为空）。
	allHot, _ := reg.ListByTier(ctx, "", TierHot, 0)
	if len(allHot) != 3 {
		t.Fatalf("跨世界 hot region 应有 3 个，得到 %d", len(allHot))
	}
}
