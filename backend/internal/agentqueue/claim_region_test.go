package agentqueue

// 文件说明：region 维度作业认领（ClaimNextJobInRegion）的集成测试（真实 SQLite）。
// 断言：只认领指定 region 的作业、同区 FIFO（created_at 升序）、跨区不串认领、空 region 报错、排空返回 nil、
// 不双认领（认领即 pending→running）。测试前缀 ClaimRegion* 避免与既有 agentqueue 测试撞名。

import (
	"testing"
)

func TestClaimRegionScopedOnly(t *testing.T) {
	db, ctx := newQueueDB(t)

	// 两个 region 各一条 pending 作业。
	idA, err := EnqueueJob(ctx, db, DecisionJob{UnitID: "uA", SessionID: "rgA", RegionID: "rgA"})
	if err != nil {
		t.Fatalf("enqueue rgA: %v", err)
	}
	if _, err := EnqueueJob(ctx, db, DecisionJob{UnitID: "uB", SessionID: "rgB", RegionID: "rgB"}); err != nil {
		t.Fatalf("enqueue rgB: %v", err)
	}

	// 认领 rgA → 只拿到 rgA 的那条，绝不串到 rgB。
	job, err := ClaimNextJobInRegion(ctx, db, "rgA")
	if err != nil {
		t.Fatalf("claim rgA: %v", err)
	}
	if job == nil || job.ID != idA || job.RegionID != "rgA" {
		t.Fatalf("应认领 rgA 的作业 %s，得到 %+v", idA, job)
	}
	if job.Status != StatusRunning {
		t.Fatalf("认领后应 running，得到 %q", job.Status)
	}

	// rgA 已无 pending → 再认领 rgA 返回 nil（不会去拿 rgB 的）。
	job2, err := ClaimNextJobInRegion(ctx, db, "rgA")
	if err != nil {
		t.Fatalf("claim rgA again: %v", err)
	}
	if job2 != nil {
		t.Fatalf("rgA 已排空应返回 nil，得到 %+v", job2)
	}

	// rgB 的作业仍可被认领（未被 rgA 认领串走）。
	jobB, err := ClaimNextJobInRegion(ctx, db, "rgB")
	if err != nil {
		t.Fatalf("claim rgB: %v", err)
	}
	if jobB == nil || jobB.RegionID != "rgB" {
		t.Fatalf("应认领 rgB 的作业，得到 %+v", jobB)
	}
}

func TestClaimRegionFIFOWithinRegion(t *testing.T) {
	db, ctx := newQueueDB(t)

	// 同 region 三条，created_at 升序（nowTS 随插入时间递增；同一毫秒内靠 id ASC 兜底确定性）。
	id1, _ := EnqueueJob(ctx, db, DecisionJob{ID: "aaa", UnitID: "u1", RegionID: "r1"})
	id2, _ := EnqueueJob(ctx, db, DecisionJob{ID: "bbb", UnitID: "u2", RegionID: "r1"})
	// 另一区一条，不应被 r1 认领串走。
	_, _ = EnqueueJob(ctx, db, DecisionJob{ID: "zzz", UnitID: "u9", RegionID: "r2"})

	first, err := ClaimNextJobInRegion(ctx, db, "r1")
	if err != nil || first == nil {
		t.Fatalf("应认领 r1 第一条: err=%v job=%+v", err, first)
	}
	second, err := ClaimNextJobInRegion(ctx, db, "r1")
	if err != nil || second == nil {
		t.Fatalf("应认领 r1 第二条: err=%v job=%+v", err, second)
	}
	got := []string{first.ID, second.ID}
	want := []string{id1, id2}
	if got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("r1 应按 FIFO 认领 %v，得到 %v", want, got)
	}

	// r1 排空。
	if j, _ := ClaimNextJobInRegion(ctx, db, "r1"); j != nil {
		t.Fatalf("r1 排空应 nil，得到 %+v", j)
	}
	// r2 那条仍在（COUNT pending == 1）。
	if n, _ := CountJobsByStatus(ctx, db, StatusPending); n != 1 {
		t.Fatalf("r2 那条不应被 r1 认领串走，pending 应剩 1，得到 %d", n)
	}
}

func TestClaimRegionEmptyRegionErrors(t *testing.T) {
	db, ctx := newQueueDB(t)
	if _, err := ClaimNextJobInRegion(ctx, db, ""); err == nil {
		t.Fatal("空 region 认领应返回错误（区分于全局 ClaimNextJob 的认领任意作业语义）")
	}
}

func TestClaimRegionNoDoubleClaim(t *testing.T) {
	db, ctx := newQueueDB(t)
	if _, err := EnqueueJob(ctx, db, DecisionJob{UnitID: "u1", RegionID: "r1"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// 首次认领拿到；第二次认领（同区）应 nil（已 running，非 pending）。
	if j, err := ClaimNextJobInRegion(ctx, db, "r1"); err != nil || j == nil {
		t.Fatalf("首次认领应成功: err=%v job=%+v", err, j)
	}
	if j, err := ClaimNextJobInRegion(ctx, db, "r1"); err != nil || j != nil {
		t.Fatalf("已认领的作业不应被二次认领: err=%v job=%+v", err, j)
	}
}
