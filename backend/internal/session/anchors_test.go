package session

// 文件说明：相关性锚持久层集成测试（对真实 SQLite）：upsert/load、非关系锚命中事件、播种后落库。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

func TestAnchorUpsertAndHit(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(3, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	// upsert 一条「债仇爱」锚指向某位旧友（非关系锚，只有这张表能存）。
	if err := service.UpsertAnchor(ctx, hero.ID, relevance.DebtGrudgeLove, "old_friend", 0.8, "生死之交", 14); err != nil {
		t.Fatalf("upsert 锚失败: %v", err)
	}
	// 重复 upsert 应幂等更新（不报错、不重复行）。
	if err := service.UpsertAnchor(ctx, hero.ID, relevance.DebtGrudgeLove, "old_friend", 0.9, "生死之交", 14); err != nil {
		t.Fatalf("重复 upsert 失败: %v", err)
	}

	anchors := service.buildRelevanceAnchors(ctx, hero.ID)
	found := false
	for _, a := range anchors {
		if a.Kind == relevance.DebtGrudgeLove && a.Ref == "old_friend" {
			found = true
			if a.Weight != 0.9 {
				t.Fatalf("应取更新后的权重 0.9，得到 %v", a.Weight)
			}
		}
	}
	if !found {
		t.Fatalf("buildRelevanceAnchors 应包含持久锚")
	}

	// 关于这位旧友的一件世界事件，应因这条锚而变得相关（>0）。
	rel := eventRelevance(anchors, FateEvent{ActorID: "old_friend", TargetID: "old_friend"})
	if rel <= 0 {
		t.Fatalf("持久锚应让关于旧友的事件相关，得到 rel=%v", rel)
	}
	// 与她毫无关系的事件应不相关。
	if r2 := eventRelevance(anchors, FateEvent{ActorID: "stranger", TargetID: "nobody"}); r2 != 0 {
		t.Fatalf("无关事件应 0 相关，得到 %v", r2)
	}
}

func TestSeedVillagePopulatesAnchors(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := service.SeedVillage(ctx, "s1", "player", "", 7); err != nil {
		t.Fatalf("播种失败: %v", err)
	}
	var goalCount, debtCount int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relevance_anchors WHERE anchor_kind = 'goal'`).Scan(&goalCount)
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relevance_anchors WHERE anchor_kind = 'debt_grudge_love'`).Scan(&debtCount)
	if goalCount != 20 {
		t.Fatalf("应为 20 人各落 1 条目标锚，得到 %d", goalCount)
	}
	if debtCount == 0 {
		t.Fatalf("强关系应沉淀债仇爱锚，得到 0")
	}
}
