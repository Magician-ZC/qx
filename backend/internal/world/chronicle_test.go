package world

// 文件说明：世界编年史存储原语的 DB 集成测试（分区大世界阶段4 §7）——写入/倒序读/纪元计数/容量裁剪。

import (
	"testing"
)

func TestRecordAndListWorldChronicle(t *testing.T) {
	ctx, db := newWorldDB(t)
	wid := "w_test"

	// 写三条，world_tick 递增。倒序读应 tick 由近及远。
	for i, c := range []struct {
		tick int
		cat  string
		imp  int
	}{
		{10, "boss_slain", 7},
		{20, "zone_unlocked", 6},
		{30, "hero_died", 5},
	} {
		if _, err := RecordWorldChronicle(ctx, db, false, WorldChronicleEntry{
			WorldID: wid, WorldTick: c.tick, Era: "开拓纪元", Category: c.cat,
			TitleZH: "标题", NarrativeZH: "叙事", Importance: c.imp, ActorRefs: []string{"u1"},
		}); err != nil {
			t.Fatalf("写第 %d 条失败: %v", i, err)
		}
	}

	got, err := ListWorldChronicle(ctx, db, wid, 0)
	if err != nil {
		t.Fatalf("读世界史失败: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("应有 3 条，得 %d", len(got))
	}
	// 倒序：tick 30 → 20 → 10。
	if got[0].WorldTick != 30 || got[1].WorldTick != 20 || got[2].WorldTick != 10 {
		t.Fatalf("应按 world_tick 倒序，得 %d,%d,%d", got[0].WorldTick, got[1].WorldTick, got[2].WorldTick)
	}
	// actor_refs 往返。
	if len(got[0].ActorRefs) != 1 || got[0].ActorRefs[0] != "u1" {
		t.Fatalf("actor_refs 应往返 [u1]，得 %+v", got[0].ActorRefs)
	}
	// 跨世界隔离：另一个世界读不到本世界的条目。
	other, _ := ListWorldChronicle(ctx, db, "w_other", 0)
	if len(other) != 0 {
		t.Fatalf("跨世界应隔离，w_other 应为空，得 %d", len(other))
	}
}

func TestRecordWorldChronicle_EmptyWorldIDRejected(t *testing.T) {
	ctx, db := newWorldDB(t)
	if _, err := RecordWorldChronicle(ctx, db, false, WorldChronicleEntry{WorldTick: 1, Category: "x"}); err == nil {
		t.Fatal("空 world_id 应被拒")
	}
}

func TestCountMajorAndEraAdvance(t *testing.T) {
	ctx, db := newWorldDB(t)
	wid := "w_era"
	// 写 worldEraEventThreshold 个重大事件（importance≥worldEraMajorImportance）。
	for i := 0; i < worldEraEventThreshold; i++ {
		if _, err := RecordWorldChronicle(ctx, db, false, WorldChronicleEntry{
			WorldID: wid, WorldTick: i, Era: "开拓纪元", Category: "boss_slain",
			TitleZH: "x", NarrativeZH: "x", Importance: worldEraMajorImportance,
		}); err != nil {
			t.Fatalf("写第 %d 条失败: %v", i, err)
		}
	}
	// 再写一条非重大（importance 低）——不计入纪元推进。
	if _, err := RecordWorldChronicle(ctx, db, false, WorldChronicleEntry{
		WorldID: wid, WorldTick: 99, Era: "开拓纪元", Category: "minor", TitleZH: "x", NarrativeZH: "x", Importance: 2,
	}); err != nil {
		t.Fatalf("写非重大条失败: %v", err)
	}
	n, err := CountMajorWorldChronicle(ctx, db, wid, "开拓纪元")
	if err != nil {
		t.Fatalf("统计重大事件失败: %v", err)
	}
	if n != worldEraEventThreshold {
		t.Fatalf("重大事件应为 %d（非重大不计），得 %d", worldEraEventThreshold, n)
	}
	if !EraShouldAdvance(n) {
		t.Fatalf("累计 %d 重大事件应够开新纪元", n)
	}
	if EraShouldAdvance(worldEraEventThreshold - 1) {
		t.Fatal("少 1 个重大事件不应开新纪元")
	}
}

func TestEraShouldAdvanceBoundary(t *testing.T) {
	if EraShouldAdvance(0) {
		t.Fatal("0 不应推进纪元")
	}
	if !EraShouldAdvance(worldEraEventThreshold + 5) {
		t.Fatal("超阈值应推进纪元")
	}
}
