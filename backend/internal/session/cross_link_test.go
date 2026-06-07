package session

// 文件说明：世界总线→命运收件箱桥接的集成测试（对真实 SQLite）：
// 别家玩家角色的跨玩家事件（救援/背叛），经 worldbus 写入后，能被桥接成「她的命运」并进收件箱。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/worldbus"
)

func appendCross(t *testing.T, ctx context.Context, service *Service, ev worldbus.CrossEvent) {
	t.Helper()
	if _, err := worldbus.Append(ctx, service.db, ev); err != nil {
		t.Fatalf("append cross event 失败: %v", err)
	}
}

func TestSurfaceCrossEventsForCharacter(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	hero := unit.BootstrapRecord(31, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}

	// 别家玩家的角色与她发生了跨玩家交互（她是 target / 她是 actor），以及一条与她无关的事件。
	appendCross(t, ctx, service, worldbus.CrossEvent{
		WorldID: "w1", ActorID: "stranger_from_shard_3", TargetID: hero.ID,
		Kind: worldbus.KindRescue, Importance: 8, WorldTick: 1,
	})
	appendCross(t, ctx, service, worldbus.CrossEvent{
		WorldID: "w1", ActorID: hero.ID, TargetID: "rival_from_shard_9",
		Kind: worldbus.KindBetrayal, Importance: 8, WorldTick: 2,
	})
	appendCross(t, ctx, service, worldbus.CrossEvent{
		WorldID: "w1", ActorID: "x", TargetID: "y",
		Kind: worldbus.KindGift, Importance: 8, WorldTick: 3, // 与她无关，不应进她的收件箱
	})

	surfaced, err := service.SurfaceCrossEventsForCharacter(ctx, "s1", "w1", &hero, 0)
	if err != nil {
		t.Fatalf("桥接跨玩家事件失败: %v", err)
	}
	if surfaced != 2 {
		t.Fatalf("应有 2 条牵涉她的跨玩家事件被惊动，得到 %d", surfaced)
	}

	// 高重要度 → 升级待决策，落进命运收件箱。
	inbox, err := service.OpenFateInbox(ctx, hero.ID)
	if err != nil {
		t.Fatalf("打开命运收件箱失败: %v", err)
	}
	if len(inbox) != 2 {
		t.Fatalf("命运收件箱应有 2 条跨玩家待决策，得到 %d", len(inbox))
	}
	// 卡面应是祖魂语气（含「她」的措辞），且不会出现别家角色的原始 ID。
	for _, item := range inbox {
		if item.Narrative == "" || contains(item.Narrative, "shard") {
			t.Fatalf("命运卡应是祖魂语气、不泄露跨分片原始 ID：%q", item.Narrative)
		}
	}
}

func TestCrossEventToFateSelfPath(t *testing.T) {
	// 她是 target：映射后 TargetID 必为她，SurfaceFateEvent 会据此走自相关路径。
	fe := crossEventToFate(worldbus.CrossEvent{
		ActorID: "other", TargetID: "me", Kind: worldbus.KindAttack, Importance: 0,
	}, "me")
	if fe.TargetID != "me" {
		t.Fatalf("自相关映射应保留她为 target，得到 %q", fe.TargetID)
	}
	if fe.Importance != crossEventDefaultImportance {
		t.Fatalf("缺省重要度应回退默认值，得到 %d", fe.Importance)
	}
	if fe.EmotionWeight >= 0 {
		t.Fatalf("被攻击应为负效价，得到 %v", fe.EmotionWeight)
	}
}
