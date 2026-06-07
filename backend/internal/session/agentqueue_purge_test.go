package session

// 文件说明：验证 PurgeExpiredSessionData 把 region-runner 调度队列（agent_wake_queue/agent_decision_jobs）
// 按 session_id 一并清理（M7.3-real-0 前置项）。**用真实 agentqueue.EnqueueWake/EnqueueJob 写入**（而非裸 SQL），
// 把「enqueue 必须填 SessionID==sessionID」的清理契约钉成 round-trip 测试，防 enqueue 接入时 region_id 取别的值致漏删孤儿。

import (
	"encoding/json"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
)

func TestPurgeDeletesAgentQueues(t *testing.T) {
	service, ctx := newCutoverService(t)

	// 注入早已过期的会话。
	oldTS := time.Now().UTC().Add(-90 * 24 * time.Hour).Format(time.RFC3339Nano)
	enc, _ := json.Marshal(&State{ID: "old"})
	if _, err := service.db.ExecContext(ctx, `INSERT INTO single_player_sessions (id, state_json, created_at, updated_at) VALUES ('old', ?, ?, ?)`, string(enc), oldTS, oldTS); err != nil {
		t.Fatalf("注入过期会话失败: %v", err)
	}

	// 真实 enqueue：SessionID 填成 sessionID（MVP 契约），region_id 故意填**别的值**（"someregion"）——
	// 验证 purge 按 session_id 而非 region_id 收敛，与 region 取值解耦。
	if err := agentqueue.EnqueueWake(ctx, service.db, agentqueue.WakeEntry{UnitID: "u1", SessionID: "old", RegionID: "someregion", WakeAtTick: 5}); err != nil {
		t.Fatalf("enqueue wake 失败: %v", err)
	}
	if _, err := agentqueue.EnqueueJob(ctx, service.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "old", RegionID: "someregion", Tick: 5}); err != nil {
		t.Fatalf("enqueue job 失败: %v", err)
	}

	res, err := service.PurgeExpiredSessionData(ctx, 30, 100)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if res.WakeQueueDeleted != 1 || res.DecisionJobsDeleted != 1 {
		t.Fatalf("应各删 1 条调度留痕，得到 wake=%d jobs=%d", res.WakeQueueDeleted, res.DecisionJobsDeleted)
	}

	var wakes, jobs int
	_ = service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_wake_queue WHERE session_id = 'old'`).Scan(&wakes)
	_ = service.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_decision_jobs WHERE session_id = 'old'`).Scan(&jobs)
	if wakes != 0 || jobs != 0 {
		t.Fatalf("会话过期清理后调度队列应清空，仍有 wakes=%d jobs=%d", wakes, jobs)
	}
}
