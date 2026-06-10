package session

// 文件说明：世界编年史入史规则与读侧的集成测试（分区大世界阶段4 §7）——
// boss 讨平入史 + 区域解锁入史 + 老死入史 + 旧档（WorldID 空）no-op + 纪元盖戳 + 读侧 feed。

import (
	"context"
	"strings"
	"testing"

	"qunxiang/backend/internal/world"
)

func TestChronicleBossSlain_RecordsWorldHistory(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := "w_chron"

	id := service.chronicleBossSlain(ctx, wid, 12, "u_hero", "晨曦剑客", "血牙魔王", "晨曦平原")
	if id == "" {
		t.Fatal("boss 讨平应写入一条世界编年史并返回 id")
	}

	feed, err := service.WorldChronicleFeedByWorldID(ctx, wid, 0)
	if err != nil {
		t.Fatalf("读世界编年史失败: %v", err)
	}
	if len(feed.Entries) != 1 {
		t.Fatalf("应有 1 条世界编年史，得 %d", len(feed.Entries))
	}
	e := feed.Entries[0]
	if e.Category != WorldChronicleBossSlain {
		t.Fatalf("category 应为 boss_slain，得 %q", e.Category)
	}
	if !strings.Contains(e.NarrativeZH, "血牙魔王") || !strings.Contains(e.NarrativeZH, "晨曦平原") {
		t.Fatalf("叙事应含 boss 名与区域名，得 %q", e.NarrativeZH)
	}
	if e.Era != worldEraNames[0] {
		t.Fatalf("首条应处首纪元 %q，得 %q", worldEraNames[0], e.Era)
	}
	if len(e.ActorRefs) != 1 || e.ActorRefs[0] != "u_hero" {
		t.Fatalf("actor_refs 应为 [u_hero]，得 %+v", e.ActorRefs)
	}
}

func TestChronicleZoneUnlocked_AndHeroDied(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := "w_mix"

	if id := service.chronicleZoneUnlocked(ctx, wid, 3, "u_h", "游侠", "zone_order", "铁律城郊"); id == "" {
		t.Fatal("区域解锁应入史")
	}
	if id := service.chronicleHeroDied(ctx, wid, 7, "u_old", "白发老者", "寿终正寝"); id == "" {
		t.Fatal("老死应入史")
	}

	feed, err := service.WorldChronicleFeedByWorldID(ctx, wid, 0)
	if err != nil {
		t.Fatalf("读世界编年史失败: %v", err)
	}
	if len(feed.Entries) != 2 {
		t.Fatalf("应有 2 条，得 %d", len(feed.Entries))
	}
	// 倒序：tick 7（hero_died）在前，tick 3（zone_unlocked）在后。
	if feed.Entries[0].Category != WorldChronicleHeroDied {
		t.Fatalf("倒序首条应为 hero_died，得 %q", feed.Entries[0].Category)
	}
	if feed.Entries[1].Category != WorldChronicleZoneUnlocked {
		t.Fatalf("第二条应为 zone_unlocked，得 %q", feed.Entries[1].Category)
	}
	if !strings.Contains(feed.Entries[0].NarrativeZH, "寿终正寝") {
		t.Fatalf("老死叙事应含死因，得 %q", feed.Entries[0].NarrativeZH)
	}
}

func TestRecordWorldChronicle_EmptyWorldIDNoOp(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	// 旧单图档（WorldID 空）：入史应 no-op、返回空 id、不写任何行。
	if id := service.chronicleBossSlain(ctx, "", 1, "u", "勇者", "魔王", "某地"); id != "" {
		t.Fatalf("空 worldID 应 no-op 返回空 id，得 %q", id)
	}
}

func TestEraThresholdParity(t *testing.T) {
	// session 层硬编的纪元常量必须与 world 包内部常量（worldEraEventThreshold=12 / worldEraMajorImportance=7）一致。
	// world 包常量未导出，这里以 world.EraShouldAdvance 的边界为锚反推阈值，守恒一致（改一处即此处报）。
	if !world.EraShouldAdvance(worldEraEventThresholdForSession()) {
		t.Fatalf("session 纪元阈值 %d 应正好够 world.EraShouldAdvance 推进纪元（阈值漂移）", worldEraEventThresholdForSession())
	}
	if world.EraShouldAdvance(worldEraEventThresholdForSession() - 1) {
		t.Fatalf("session 纪元阈值 %d 减 1 不应推进纪元（阈值漂移）", worldEraEventThresholdForSession())
	}
	if worldEraMajorImportanceForSession() != 7 {
		t.Fatalf("session 重大事件重要度门槛应为 7，得 %d", worldEraMajorImportanceForSession())
	}
}

func TestWorldChronicleProcessEventEmitted(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()

	before := eventCount(t, db)
	service.chronicleBossSlain(ctx, "w_ev", 5, "u_hero", "剑客", "魔王", "平原")
	after := eventCount(t, db)
	if after <= before {
		t.Fatalf("入史应旁路一条 WORLD_CHRONICLE_RECORD 流程事件，事件数应增长（%d→%d）", before, after)
	}
}
