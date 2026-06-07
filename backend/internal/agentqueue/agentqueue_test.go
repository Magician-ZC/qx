package agentqueue

// 文件说明：region-runner 持久化队列的集成测试（对真实 SQLite，schema.sql 建表）。
// 覆盖：唤醒队列 upsert/到点拉取/region 隔离/删除；作业队列入队/原子认领（不双认领/排空返回 nil）/完成/计数。

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newQueueDB(t *testing.T) (*sql.DB, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, context.Background()
}

func TestWakeQueueUpsertAndDue(t *testing.T) {
	db, ctx := newQueueDB(t)

	// 同一单位重排 → 仍只一条（upsert）。
	if err := EnqueueWake(ctx, db, WakeEntry{UnitID: "u1", RegionID: "r1", WakeAtTick: 5, Tier: "warm"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := EnqueueWake(ctx, db, WakeEntry{UnitID: "u1", RegionID: "r1", WakeAtTick: 12, Tier: "cold"}); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if err := EnqueueWake(ctx, db, WakeEntry{UnitID: "u2", RegionID: "r1", WakeAtTick: 8}); err != nil {
		t.Fatalf("enqueue u2: %v", err)
	}
	if err := EnqueueWake(ctx, db, WakeEntry{UnitID: "u3", RegionID: "r2", WakeAtTick: 1}); err != nil {
		t.Fatalf("enqueue u3 (other region): %v", err)
	}

	// tick=10 在 r1 到点的：u1(12,不到点 no)、u2(8,到点 yes)。u1 已被重排到 12 故不到点。
	due, err := ListDueWakes(ctx, db, "r1", 10, 100)
	if err != nil {
		t.Fatalf("list due: %v", err)
	}
	if len(due) != 1 || due[0].UnitID != "u2" {
		t.Fatalf("tick10 r1 到点应仅 u2（u1 已重排到 12），得到 %+v", due)
	}

	// tick=12：u1(12) 与 u2(8) 都到点，按 wake_at_tick 升序 u2 先。r2 的 u3 不混入。
	due, _ = ListDueWakes(ctx, db, "r1", 12, 100)
	if len(due) != 2 || due[0].UnitID != "u2" || due[1].UnitID != "u1" {
		t.Fatalf("tick12 r1 应 [u2,u1] 升序，得到 %+v", due)
	}

	// 删除后不再到点。
	if err := RemoveWake(ctx, db, "u2"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	due, _ = ListDueWakes(ctx, db, "r1", 12, 100)
	if len(due) != 1 || due[0].UnitID != "u1" {
		t.Fatalf("删 u2 后应仅 u1，得到 %+v", due)
	}
}

func TestDecisionJobClaimLifecycle(t *testing.T) {
	db, ctx := newQueueDB(t)

	ids := map[string]bool{}
	for _, u := range []string{"u1", "u2", "u3"} {
		id, err := EnqueueJob(ctx, db, DecisionJob{UnitID: u, RegionID: "r1", Tick: 7})
		if err != nil {
			t.Fatalf("enqueue job: %v", err)
		}
		ids[id] = true
	}
	if n, _ := CountJobsByStatus(ctx, db, StatusPending); n != 3 {
		t.Fatalf("应有 3 pending，得到 %d", n)
	}

	// 认领三次：每次拿到不同作业、状态 running、不双认领。
	claimedUnits := map[string]int{}
	claimedJobIDs := map[string]bool{}
	for i := 0; i < 3; i++ {
		job, err := ClaimNextJob(ctx, db)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if job == nil {
			t.Fatalf("第 %d 次认领不应为空", i+1)
		}
		if claimedJobIDs[job.ID] {
			t.Fatalf("作业 %s 被重复认领", job.ID)
		}
		claimedJobIDs[job.ID] = true
		claimedUnits[job.UnitID]++
		if job.Status != StatusRunning {
			t.Fatalf("认领后状态应 running，得到 %s", job.Status)
		}
		if job.Tick != 7 {
			t.Fatalf("作业 tick 应保留为 7，得到 %d", job.Tick)
		}
	}
	if len(claimedUnits) != 3 {
		t.Fatalf("三个单位应各被认领一次，得到 %+v", claimedUnits)
	}
	if n, _ := CountJobsByStatus(ctx, db, StatusRunning); n != 3 {
		t.Fatalf("应有 3 running（背压计数），得到 %d", n)
	}

	// 队列排空 → 认领返回 nil。
	job, err := ClaimNextJob(ctx, db)
	if err != nil {
		t.Fatalf("claim empty: %v", err)
	}
	if job != nil {
		t.Fatalf("排空后认领应返回 nil，得到 %+v", job)
	}

	// 完成一条 → running 减一。
	var oneID string
	for id := range claimedJobIDs {
		oneID = id
		break
	}
	if err := CompleteJob(ctx, db, oneID, StatusDone); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n, _ := CountJobsByStatus(ctx, db, StatusRunning); n != 2 {
		t.Fatalf("完成一条后应 2 running，得到 %d", n)
	}
	if n, _ := CountJobsByStatus(ctx, db, StatusDone); n != 1 {
		t.Fatalf("应有 1 done，得到 %d", n)
	}

	// 非法终态被拒。
	if err := CompleteJob(ctx, db, oneID, StatusPending); err == nil {
		t.Fatalf("非法终态应被拒")
	}
	// 重复完成同一条（已 done，非 running）应被拒——状态机守门。
	if err := CompleteJob(ctx, db, oneID, StatusDone); err != ErrJobNotClaimable {
		t.Fatalf("重复完成已终态作业应返回 ErrJobNotClaimable，得到 %v", err)
	}
}

func TestCompleteJobRequiresRunning(t *testing.T) {
	db, ctx := newQueueDB(t)
	// 完成一条仍 pending（未认领）的作业应被拒——不能跳过 running 阶段。
	id, err := EnqueueJob(ctx, db, DecisionJob{UnitID: "u1", RegionID: "r1"})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := CompleteJob(ctx, db, id, StatusDone); err != ErrJobNotClaimable {
		t.Fatalf("完成未认领的 pending 作业应返回 ErrJobNotClaimable，得到 %v", err)
	}
	if n, _ := CountJobsByStatus(ctx, db, StatusPending); n != 1 {
		t.Fatalf("被拒的完成不应改状态，仍应 1 pending，得到 %d", n)
	}
	// 不存在的 id 同样被拒（不静默 no-op）。
	if err := CompleteJob(ctx, db, "no-such-id", StatusFailed); err != ErrJobNotClaimable {
		t.Fatalf("完成不存在的作业应返回 ErrJobNotClaimable，得到 %v", err)
	}
}

func TestConcurrentClaimNoDoubleClaim(t *testing.T) {
	// 核心原子性保证：并发认领同一队列，每条作业只被一个 worker 拿到（无双认领、无丢失）。
	db, ctx := newQueueDB(t)
	const n = 40
	for i := 0; i < n; i++ {
		if _, err := EnqueueJob(ctx, db, DecisionJob{UnitID: "u", RegionID: "r1", Tick: 1}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := ClaimNextJob(ctx, db)
				if err != nil || job == nil {
					return
				}
				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != n {
		t.Fatalf("应认领全部 %d 条，得到 %d 条不同作业", n, len(claimed))
	}
	for id, c := range claimed {
		if c != 1 {
			t.Fatalf("作业 %s 被认领 %d 次（应恰 1 次，原子性破坏）", id, c)
		}
	}
	if remaining, _ := CountJobsByStatus(ctx, db, StatusPending); remaining != 0 {
		t.Fatalf("应无剩余 pending，得到 %d", remaining)
	}
}
