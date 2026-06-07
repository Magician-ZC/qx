package session

// 文件说明：回响 Echo 集成测试（对真实 SQLite）：玩家动作成事件→暴露给归因→以真实前因生成回响卡，无前因不生成。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

func TestEchoRefFromAttribution(t *testing.T) {
	ref, narr, ok := echoRefFromAttribution(&attributionPayload{
		Primary:     causeRefPayload{Kind: "order_echo", RefID: "e1"},
		NarrativeZH: "她这次拔了刀",
	})
	if !ok || ref != "e1" || narr != "她这次拔了刀" {
		t.Fatalf("应提取 order_echo 引用：ref=%q narr=%q ok=%v", ref, narr, ok)
	}
	if _, _, ok := echoRefFromAttribution(&attributionPayload{Primary: causeRefPayload{Kind: "memory", RefID: "m1"}}); ok {
		t.Fatalf("非 order_echo 不应判为回响")
	}
	if _, _, ok := echoRefFromAttribution(nil); ok {
		t.Fatalf("nil 归因不应判为回响")
	}
}

func TestPlayerInterventionExposedAndEcho(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	rec := unit.BootstrapRecord(5, "s1", "player", "她")
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	// 玩家接管一次 → 落成可引用事件。
	pid, err := service.RecordPlayerIntervention(ctx, "s1", rec.ID, "放过了那个跪地求饶的逃兵")
	if err != nil || pid == "" {
		t.Fatalf("记录接管失败: %v id=%q", err, pid)
	}

	// 该动作出现在玩家动作列表里。
	actions := service.loadRecentPlayerActions(ctx, rec.ID, 8)
	if len(actions) == 0 || actions[0].Summary != "放过了那个跪地求饶的逃兵" {
		t.Fatalf("玩家动作应可被取回：%+v", actions)
	}

	// 暴露给归因：PlayerActionEventIDs 含该事件 + prompt 提到它。
	snap, prompt := service.buildDecisionAttributionContext(ctx, State{ID: "s1"}, &rec)
	if _, has := snap.PlayerActionEventIDs[pid]; !has {
		t.Fatalf("PlayerActionEventIDs 应含接管事件 %q", pid)
	}
	if prompt == "" || !contains(prompt, pid) {
		t.Fatalf("prompt 应暴露玩家动作 event ID：%q", prompt)
	}

	// 以真实前因生成回响卡。
	ok, err := service.SurfaceEcho(ctx, "s1", rec.ID, pid, "她却拔刀拦在了那人身前", 0.3)
	if err != nil || !ok {
		t.Fatalf("真实前因应生成回响卡：ok=%v err=%v", ok, err)
	}
	var echoCount int
	var narrative string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`, rec.ID, string(events.ReasonEchoLink)).Scan(&echoCount); err != nil {
		t.Fatalf("统计回响事件失败: %v", err)
	}
	if echoCount != 1 {
		t.Fatalf("应恰有 1 条回响事件，得到 %d", echoCount)
	}
	_ = db.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE actor_unit_id = ? AND reason_code = ?`, rec.ID, string(events.ReasonEchoLink)).Scan(&narrative)
	if !contains(narrative, "因为你上次") || !contains(narrative, "放过了那个跪地求饶的逃兵") {
		t.Fatalf("回响卡应引用真实的上次选择：%q", narrative)
	}

	// 无真实前因 → 绝不编造回响（宪法 §6.2 硬约束）。
	fake, err := service.SurfaceEcho(ctx, "s1", rec.ID, "no_such_event", "随便编的", 0)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if fake {
		t.Fatalf("无真实前因不应生成回响卡")
	}
}
