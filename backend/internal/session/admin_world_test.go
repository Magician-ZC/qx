package session

// 文件说明：GM 世界配置服务 admin_world.go 的集成测试（对真实 SQLite）。
// 覆盖：ListWorldsDetail 含 region/人口、SetRegionThreatLevel 绝对置位（含 region 不存在自动建档）、
// SeedWorldVillage 触发织村、FeatureFlagStore 双驱动持久化往返 + featureflags.LoadFromStore 回灌。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/region"
	"qunxiang/backend/internal/villageseed"
	"qunxiang/backend/internal/world"
)

func TestSetRegionThreatLevelAbsolute(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	// region 不存在时自动建档并置位到 50。
	got, err := service.SetRegionThreatLevel(ctx, "world_a", "r_north", 50)
	if err != nil {
		t.Fatalf("SetRegionThreatLevel 失败: %v", err)
	}
	if got != 50 {
		t.Fatalf("威胁度应置为 50，得 %d", got)
	}

	// 再降到 10：绝对置位（不是 +10）。
	got, err = service.SetRegionThreatLevel(ctx, "world_a", "r_north", 10)
	if err != nil {
		t.Fatalf("二次置位失败: %v", err)
	}
	if got != 10 {
		t.Fatalf("威胁度应绝对置为 10，得 %d", got)
	}

	// 负值夹到 0。
	got, err = service.SetRegionThreatLevel(ctx, "world_a", "r_north", -5)
	if err != nil {
		t.Fatalf("置零失败: %v", err)
	}
	if got != 0 {
		t.Fatalf("负值应夹到 0，得 %d", got)
	}

	// 直读 region 确认落库。
	reg, err := region.New(service.db).GetRegion(ctx, "r_north")
	if err != nil {
		t.Fatalf("读 region 失败: %v", err)
	}
	if reg.ThreatLevel != 0 || reg.WorldID != "world_a" {
		t.Fatalf("region 落库不符：threat=%d world=%q", reg.ThreatLevel, reg.WorldID)
	}
}

func TestListWorldsDetailWithRegionsAndPopulation(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	wid, err := world.Create(ctx, service.db, world.World{Name: "总览世界", MaxPopulation: 999})
	if err != nil {
		t.Fatalf("建世界失败: %v", err)
	}
	// 织一张村庄入世界，制造人口与成员。
	n, err := service.SeedWorldVillage(ctx, "s_overview", "player", wid, 11)
	if err != nil {
		t.Fatalf("SeedWorldVillage 失败: %v", err)
	}
	if n != villageseed.VillageSize {
		t.Fatalf("应织 %d 人，得 %d", villageseed.VillageSize, n)
	}
	// 登记一个 region 并置威胁度，验证 region 概览出现在 detail。
	if _, err := service.SetRegionThreatLevel(ctx, wid, "r_overview", 33); err != nil {
		t.Fatalf("置 region 威胁失败: %v", err)
	}

	details, err := service.ListWorldsDetail(ctx, 0)
	if err != nil {
		t.Fatalf("ListWorldsDetail 失败: %v", err)
	}
	var found *WorldDetail
	for i := range details {
		if details[i].ID == wid {
			found = &details[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("总览应含世界 %s", wid)
	}
	if found.Population != villageseed.VillageSize {
		t.Fatalf("人口应为 %d（已入世界成员），得 %d", villageseed.VillageSize, found.Population)
	}
	var regionFound bool
	for _, r := range found.Regions {
		if r.ID == "r_overview" {
			regionFound = true
			if r.ThreatLevel != 33 {
				t.Errorf("region 威胁度应为 33，得 %d", r.ThreatLevel)
			}
		}
	}
	if !regionFound {
		t.Fatalf("世界 detail 应含 region r_overview")
	}
}

func TestSeedWorldVillageDefaultsFaction(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	// factionID 留空 → 取 sessionID；worldID 留空 → 不入世界但仍落库村民。
	n, err := service.SeedWorldVillage(ctx, "s_seed", "", "", 3)
	if err != nil {
		t.Fatalf("SeedWorldVillage 失败: %v", err)
	}
	if n != villageseed.VillageSize {
		t.Fatalf("应织 %d 人，得 %d", villageseed.VillageSize, n)
	}
}

func TestFeatureFlagStoreRoundTrip(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	store := NewFeatureFlagStore(service.db)
	if err := store.Upsert(ctx, "QUNXIANG_DUNGEON", "true"); err != nil {
		t.Fatalf("Upsert 失败: %v", err)
	}
	// upsert 覆盖（同 name 二次写）。
	if err := store.Upsert(ctx, "QUNXIANG_AUTO_PVE", "on"); err != nil {
		t.Fatalf("Upsert2 失败: %v", err)
	}
	if err := store.Upsert(ctx, "QUNXIANG_DUNGEON", "1"); err != nil {
		t.Fatalf("Upsert 覆盖失败: %v", err)
	}

	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if loaded["QUNXIANG_DUNGEON"] != "1" {
		t.Fatalf("DUNGEON 应被覆盖为 1，得 %q", loaded["QUNXIANG_DUNGEON"])
	}
	if loaded["QUNXIANG_AUTO_PVE"] != "on" {
		t.Fatalf("AUTO_PVE 应为 on，得 %q", loaded["QUNXIANG_AUTO_PVE"])
	}

	// 删除一条后 Load 不再含之。
	if err := store.Delete(ctx, "QUNXIANG_AUTO_PVE"); err != nil {
		t.Fatalf("Delete 失败: %v", err)
	}
	loaded, err = store.Load(ctx)
	if err != nil {
		t.Fatalf("Load2 失败: %v", err)
	}
	if _, ok := loaded["QUNXIANG_AUTO_PVE"]; ok {
		t.Fatalf("AUTO_PVE 应已删除")
	}

	// 经 featureflags.SetStore + LoadFromStore 回灌内存，验证生效（重启存活路径）。
	featureflags.SetStore(store)
	t.Cleanup(func() {
		featureflags.SetStore(nil)
		_, _ = featureflags.ClearOverride("QUNXIANG_DUNGEON")
	})
	if _, err := featureflags.LoadFromStore(ctx); err != nil {
		t.Fatalf("LoadFromStore 失败: %v", err)
	}
	if got := featureflags.EnvOrOverride("QUNXIANG_DUNGEON"); got != "1" {
		t.Fatalf("回灌后 override 应生效，期望 1 得 %q", got)
	}
}
