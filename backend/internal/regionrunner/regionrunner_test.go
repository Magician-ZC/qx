package regionrunner

// 文件说明：region-runner 骨架的确定性测试（注入式固定时钟 + 真实 SQLite 队列）。
// 手动驱动 schedulePass/processOne 验证全机制；另跑一次真实短循环验证 Run 的优雅启停。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newRunner(t *testing.T, cfg Config) (*Runner, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "rr.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := New(db, cfg, nil).withClock(func() time.Time { return fixed })
	return r, context.Background()
}

func TestSchedulePassEnqueuesDueOnly(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1})
	tick := r.currentTick()

	// 到点的 u1（wake<=tick）、未到点的 u2（wake>tick）。
	_ = agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u1", SessionID: "s1", RegionID: "s1", WakeAtTick: tick - 1})
	_ = agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u2", SessionID: "s1", RegionID: "s1", WakeAtTick: tick + 100})

	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 1 {
		t.Fatalf("仅到点的 u1 应入队，得到 %d", enq)
	}
	// u1 的 wake 应被移除（防重复入队）；u2 仍在。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+1000, 100); len(due) != 1 || due[0].UnitID != "u2" {
		t.Fatalf("u1 wake 应已移除、u2 仍在，得到 %+v", due)
	}
	// 入队的 job 带 session_id（保留期清理键）。
	job, _ := agentqueue.ClaimNextJob(ctx, r.db)
	if job == nil || job.UnitID != "u1" || job.SessionID != "s1" {
		t.Fatalf("应入队 u1 作业且带 session_id，得到 %+v", job)
	}
}

func TestProcessOneShadowCycle(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1})

	// 空队列 → false。
	if worked, err := r.processOne(ctx); err != nil || worked {
		t.Fatalf("空队列应返回 false，得到 worked=%v err=%v", worked, err)
	}

	id, _ := agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: r.currentTick()})
	worked, err := r.processOne(ctx)
	if err != nil || !worked {
		t.Fatalf("应处理一条，得到 worked=%v err=%v", worked, err)
	}
	// 作业应已完成。
	if n, _ := agentqueue.CountJobsByStatus(ctx, r.db, agentqueue.StatusDone); n != 1 {
		t.Fatalf("作业应 done，得到 %d done", n)
	}
	if n, _ := agentqueue.CountJobsByStatus(ctx, r.db, agentqueue.StatusRunning); n != 0 {
		t.Fatalf("不应有 running，得到 %d", n)
	}
	// 单位下次唤醒被重排到未来 tick（WARM），故当前 tick 不到点、不会立即再处理。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", r.currentTick(), 100); len(due) != 0 {
		t.Fatalf("重排的 wake 应在未来 tick、当前不到点，得到 %+v", due)
	}
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", r.currentTick()+100, 100); len(due) != 1 || due[0].UnitID != "u1" {
		t.Fatalf("u1 应被重排到未来，得到 %+v", due)
	}
	_ = id
}

func TestSchedulePassBackpressure(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, MaxInFlight: 2})
	tick := r.currentTick()

	// 先把在途顶满到上限 2（入队 2 条并认领为 running）。
	for i := 0; i < 2; i++ {
		_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "busy", SessionID: "s1", RegionID: "s1"})
		_, _ = agentqueue.ClaimNextJob(ctx, r.db)
	}
	// 再来一个到点单位——背压应拒绝入队。
	_ = agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u1", SessionID: "s1", RegionID: "s1", WakeAtTick: tick})

	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 0 {
		t.Fatalf("在途达上限应背压、不入队，得到 %d", enq)
	}
	if r.Stats()["backpressured"].(int64) == 0 {
		t.Fatalf("应记一次背压")
	}
	// u1 的 wake 未被移除（背压时不该 RemoveWake），下个 tick 还能再试。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick, 100); len(due) != 1 || due[0].UnitID != "u1" {
		t.Fatalf("背压时不应移除 u1 的 wake，得到 %+v", due)
	}
}

func TestRunGracefulStop(t *testing.T) {
	// 真实短循环：启动 Run，很快取消 ctx，应在合理时间内优雅返回（不挂死）。
	r, _ := newRunner(t, Config{Enabled: true, TickSeconds: 1, TickInterval: 10 * time.Millisecond, Workers: 2, ReclaimEvery: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond) // 让 ticker/worker 转几圈
	cancel()
	select {
	case <-done:
		// 优雅退出。
	case <-time.After(3 * time.Second):
		t.Fatalf("Run 未在取消后优雅退出（疑似挂死）")
	}
	// 未 Enabled 时 Run 立即返回。
	r2, _ := newRunner(t, Config{Enabled: false})
	doneCh := make(chan struct{})
	go func() { r2.Run(context.Background()); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatalf("未启用时 Run 应立即返回")
	}
}
