package session

// 文件说明：威胁刷新调度测试（对真实 SQLite）：确定性、按概率出没、至多刷一个。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

func TestThreatRollDeterministic(t *testing.T) {
	a := threatRoll("s1", 3, "u1")
	b := threatRoll("s1", 3, "u1")
	if a != b {
		t.Fatalf("掷骰应确定性：%v vs %v", a, b)
	}
	if a < 0 || a >= 1 {
		t.Fatalf("应落 [0,1)：%v", a)
	}
	if threatRoll("s1", 3, "u1") == threatRoll("s1", 4, "u1") {
		t.Fatalf("不同回合应不同骰值（极小概率撞巧，可重试）")
	}
}

func TestRefreshThreatsSurfacesAndDeterministic(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	u := unit.BootstrapRecord(2, "s1", "player", "她")
	u.FactionID = "player"
	u.Status.HP = 100
	u.Status.LivesRemaining = 1
	if err := repo.Save(ctx, u); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	countSightings := func() int {
		var n int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE reason_code = ? AND actor_unit_id = ?`,
			string(events.ReasonInboxHighlight), u.ID).Scan(&n)
		return n
	}

	// 扫一批回合：应至少刷出一些威胁，但不是每回合都刷（概率门控）。
	sightings := 0
	turns := 40
	for turn := 1; turn <= turns; turn++ {
		state := State{ID: "s1", PlayerFactionID: "player"}
		state.TurnState.Turn = turn
		service.refreshThreats(ctx, &state, []unit.Record{u})
	}
	sightings = countSightings()
	if sightings == 0 {
		t.Fatalf("%d 回合内应刷出至少一次威胁", turns)
	}
	if sightings >= turns {
		t.Fatalf("不应每回合都刷（应受概率门控），得到 %d/%d", sightings, turns)
	}

	// 确定性：同一回合重复刷应得到同一结果（这里验证 roll 决定是否出没的确定性）。
	state := State{ID: "s1", PlayerFactionID: "player"}
	state.TurnState.Turn = 1
	before := countSightings()
	service.refreshThreats(ctx, &state, []unit.Record{u})
	service.refreshThreats(ctx, &state, []unit.Record{u})
	delta := countSightings() - before
	// 回合1是否出没是确定的；两次重复刷，要么都出（+2）要么都不出（+0），不会出现 +1 的随机。
	if delta != 0 && delta != 2 {
		t.Fatalf("同回合重复刷应确定性一致（+0 或 +2），得到 +%d", delta)
	}
}
