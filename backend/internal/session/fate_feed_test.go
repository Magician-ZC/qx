package session

// 文件说明：命运四槽首屏 feed 测试（对真实 SQLite）：高光/待决策/回响三类卡 + 已处理待决策剔除。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
)

func TestOpenFateFeed(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(8, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	// 一条高光（自相关、中等重要度 → highlight）。
	if _, err := service.SurfaceFateEvent(ctx, "s1", &hero, FateEvent{
		ActorID: hero.ID, TargetID: hero.ID, ReasonCode: "INBOX_HIGHLIGHT", Importance: 5, EmotionWeight: 0.3, Summary: "她今天过得不错",
	}); err != nil {
		t.Fatalf("surface highlight 失败: %v", err)
	}
	// 一条待决策（高重要度 → pending）。
	routing, err := service.SurfaceFateEvent(ctx, "s1", &hero, FateEvent{
		ActorID: hero.ID, TargetID: hero.ID, ReasonCode: "INBOX_HIGHLIGHT", Importance: 9, EmotionWeight: -0.8, Summary: "她濒死了，等你拿主意",
	})
	if err != nil {
		t.Fatalf("surface pending 失败: %v", err)
	}
	// 一条回响。
	pid, _ := service.RecordPlayerIntervention(ctx, "s1", hero.ID, "放过了逃兵")
	if _, err := service.SurfaceEcho(ctx, "s1", hero.ID, pid, "这次她拔刀拦了下来", 0.3); err != nil {
		t.Fatalf("surface echo 失败: %v", err)
	}

	feed, err := service.OpenFateFeed(ctx, hero.ID, 30)
	if err != nil {
		t.Fatalf("open feed 失败: %v", err)
	}
	kinds := map[string]int{}
	for _, it := range feed {
		kinds[it.Kind]++
	}
	if kinds["highlight"] < 1 || kinds["pending"] < 1 || kinds["echo"] < 1 {
		t.Fatalf("feed 应含三类卡，得到 %+v", kinds)
	}

	// 处理那条待决策后，feed 不应再含它。
	if err := service.ResolveFateDecision(ctx, "s1", hero.ID, routing.DecisionID, "acknowledge"); err != nil {
		t.Fatalf("处理待决策失败: %v", err)
	}
	feed2, _ := service.OpenFateFeed(ctx, hero.ID, 30)
	for _, it := range feed2 {
		if it.Kind == "pending" && it.DecisionID == routing.DecisionID {
			t.Fatalf("已处理的待决策不应再出现在 feed")
		}
	}
}
