package session

// 文件说明：命运收件箱/回响的 WS 实时推送测试——用假广播器捕获推送。

import (
	"context"
	"sync"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

type bcEvent struct {
	sessionID string
	eventType string
}

type fakeBroadcaster struct {
	mu     sync.Mutex
	events []bcEvent
}

func (f *fakeBroadcaster) BroadcastSessionEvent(sessionID string, eventType string, _ any) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, bcEvent{sessionID, eventType})
	return 1
}

func (f *fakeBroadcaster) count(eventType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, e := range f.events {
		if e.eventType == eventType {
			n++
		}
	}
	return n
}

func TestFateInboxAndEchoWSPush(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	fb := &fakeBroadcaster{}
	service.SetBroadcaster(fb)

	hero := unit.BootstrapRecord(6, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	// 高重要度自相关事件 → 待决策 → 应推一条 fate_inbox。
	routing, err := service.SurfaceFateEvent(ctx, "s1", &hero, FateEvent{
		ActorID: hero.ID, TargetID: hero.ID, ReasonCode: events.ReasonInboxHighlight,
		Importance: 8, EmotionWeight: -0.7, Summary: "她在峡谷口濒死了",
	})
	if err != nil {
		t.Fatalf("surface 失败: %v", err)
	}
	if routing.Relevance < 0.55 {
		t.Fatalf("高重要度应升待决策，rel=%v", routing.Relevance)
	}
	if fb.count("fate_inbox") != 1 {
		t.Fatalf("应推 1 条 fate_inbox，得到 %d", fb.count("fate_inbox"))
	}

	// 回响卡 → 应推一条 fate_echo。
	pid, err := service.RecordPlayerIntervention(ctx, "s1", hero.ID, "放过了那个逃兵")
	if err != nil {
		t.Fatalf("接管失败: %v", err)
	}
	if ok, err := service.SurfaceEcho(ctx, "s1", hero.ID, pid, "她却拔刀拦在了前面", 0.3); err != nil || !ok {
		t.Fatalf("回响应生成：ok=%v err=%v", ok, err)
	}
	if fb.count("fate_echo") != 1 {
		t.Fatalf("应推 1 条 fate_echo，得到 %d", fb.count("fate_echo"))
	}
}
