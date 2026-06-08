package session

// 文件说明：出生关系网落库集成测试（对真实 SQLite）：20 人 + 入世界 + 织关系 + 可复现。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/villageseed"
	"qunxiang/backend/internal/world"
)

func TestSeedVillagePersists(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid, err := world.Create(ctx, service.db, world.World{Name: "出生世界"})
	if err != nil {
		t.Fatalf("建世界失败: %v", err)
	}

	villagers, err := service.SeedVillage(ctx, "s1", "player", wid, 7)
	if err != nil {
		t.Fatalf("播种村庄失败: %v", err)
	}
	if len(villagers) != villageseed.VillageSize {
		t.Fatalf("应落库 %d 人，得到 %d", villageseed.VillageSize, len(villagers))
	}

	// 人确实落库，且人格/生平持久化。
	first := villagers[0]
	rec, err := repo.GetByID(ctx, first.UnitID)
	if err != nil {
		t.Fatalf("取村民失败: %v", err)
	}
	if rec.Identity.Name != first.Member.Name {
		t.Fatalf("姓名应一致：%q vs %q", rec.Identity.Name, first.Member.Name)
	}
	if rec.Personality.Courage != first.Member.Traits.Courage {
		t.Fatalf("人格应持久化：%v vs %v", rec.Personality.Courage, first.Member.Traits.Courage)
	}
	if rec.Identity.Biography == "" || rec.Identity.Lineage == "" {
		t.Fatalf("生平/出身应写入：%+v", rec.Identity)
	}

	// 全员入世界。
	members, _ := world.Members(ctx, service.db, wid, 0)
	if len(members) != villageseed.VillageSize {
		t.Fatalf("应有 %d 人入世界，得到 %d", villageseed.VillageSize, len(members))
	}

	// 关系网落库（relations 表非空）。
	var relCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations`).Scan(&relCount); err != nil {
		t.Fatalf("统计 relations 失败: %v", err)
	}
	if relCount == 0 {
		t.Fatalf("出生关系网应落 relations 行")
	}

	// 可复现：落库的人名序列应与纯生成器一致。
	gen := villageseed.Generate(wid, 7)
	for i, vv := range villagers {
		if vv.Member.Name != gen.Members[i].Name {
			t.Fatalf("第 %d 人应与生成器一致：%q vs %q", i, vv.Member.Name, gen.Members[i].Name)
		}
	}
}

// TestSeedVillageBestEffortReturnsCount 断言 onboarding 用的吞错包装在 worldID 为空（不入世界）时
// 仍落库满员 20 人并返回数量、织出关系网——这是 /api/units/bootstrap?with_village=1
// 兑现「身边二十人」的核心路径（worldID 空=不进世界，纯本局关系网）。
func TestSeedVillageBestEffortReturnsCount(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()

	n := service.SeedVillageBestEffort(ctx, "s-bootstrap", "player", "", 42)
	if n != villageseed.VillageSize {
		t.Fatalf("best-effort 应落库 %d 人，得到 %d", villageseed.VillageSize, n)
	}

	// worldID 为空时不入世界，但关系网仍落库（relations 表非空）。
	var relCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations`).Scan(&relCount); err != nil {
		t.Fatalf("统计 relations 失败: %v", err)
	}
	if relCount == 0 {
		t.Fatalf("出生关系网应落 relations 行")
	}
}

// TestSeedVillageBestEffortSwallowsError 断言依赖缺失时包装吞错返回 0 而非 panic，
// 保证 bootstrap 主路径（建主单位）永不被村庄附加体验拖垮。
func TestSeedVillageBestEffortSwallowsError(t *testing.T) {
	var nilService *Service
	if got := nilService.SeedVillageBestEffort(context.Background(), "s", "f", "", 1); got != 0 {
		t.Fatalf("依赖缺失应吞错返回 0，得到 %d", got)
	}
}
