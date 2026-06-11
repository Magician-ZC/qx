package session

// 文件说明：副本进入闸（每日次数/冷却）的集成测试（对真实 SQLite）：
// flag 关恒放行 remaining=-1 且不写库；flag 开当日打满 cap 后 allowed=false 不再写库；
// 跨 window_key（不同 UTC 日窗）后名额恢复；只读 dungeonEntriesRemaining 口径一致。
// 复用 threat_test.go 的 newThreatTestService(t)。

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

// withDungeonLockoutFlag 在测试期临时置 QUNXIANG_DUNGEON_LOCKOUT，结束自动复原（不污染其他测试的进程级环境）。
func withDungeonLockoutFlag(t *testing.T, value string) {
	t.Helper()
	orig, had := os.LookupEnv("QUNXIANG_DUNGEON_LOCKOUT")
	if err := os.Setenv("QUNXIANG_DUNGEON_LOCKOUT", value); err != nil {
		t.Fatalf("设置 flag 失败: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("QUNXIANG_DUNGEON_LOCKOUT", orig)
		} else {
			_ = os.Unsetenv("QUNXIANG_DUNGEON_LOCKOUT")
		}
	})
}

func dungeonLockoutRowCount(t *testing.T, service *Service) int {
	t.Helper()
	var n int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM dungeon_lockouts`).Scan(&n); err != nil {
		t.Fatalf("统计 dungeon_lockouts 失败: %v", err)
	}
	return n
}

// TestDungeonLockout_DisabledZeroBehavior 验证 flag 关时恒放行、remaining=-1，且不写任何 lockout 行。
func TestDungeonLockout_DisabledZeroBehavior(t *testing.T) {
	withDungeonLockoutFlag(t, "false") // 显式关（默认已开）
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	for i := 0; i < dungeonDailyEntryCap+5; i++ {
		allowed, remaining, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon")
		if err != nil {
			t.Fatalf("flag 关不应有 err: %v", err)
		}
		if !allowed {
			t.Fatalf("flag 关应恒放行，第 %d 次被拦", i)
		}
		if remaining != -1 {
			t.Fatalf("flag 关 remaining 应为 -1（不限），得到 %d", remaining)
		}
	}
	if got := dungeonLockoutRowCount(t, service); got != 0 {
		t.Fatalf("flag 关不应写任何 lockout 行，得到 %d 行", got)
	}
	if r := service.dungeonEntriesRemaining(ctx, "w1", "u1", "dungeon"); r != -1 {
		t.Fatalf("flag 关 dungeonEntriesRemaining 应为 -1，得到 %d", r)
	}
}

// TestDungeonLockout_CapEnforced 验证 flag 开时当日打满 cap 后 allowed=false，且超额后不再写库（entered_count 停在 cap）。
func TestDungeonLockout_CapEnforced(t *testing.T) {
	withDungeonLockoutFlag(t, "true")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	// 前 cap 次应放行，remaining 逐次递减到 0。
	for i := 0; i < dungeonDailyEntryCap; i++ {
		allowed, remaining, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon")
		if err != nil {
			t.Fatalf("第 %d 次进入不应 err: %v", i+1, err)
		}
		if !allowed {
			t.Fatalf("第 %d 次进入应放行（cap=%d）", i+1, dungeonDailyEntryCap)
		}
		wantRemaining := dungeonDailyEntryCap - (i + 1)
		if remaining != wantRemaining {
			t.Fatalf("第 %d 次进入剩余应为 %d，得到 %d", i+1, wantRemaining, remaining)
		}
	}

	// 第 cap+1 次应被拦，remaining=0，且不写库（行数仍 1、entered_count 仍 cap）。
	allowed, remaining, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon")
	if err != nil {
		t.Fatalf("超额一次不应 err: %v", err)
	}
	if allowed {
		t.Fatalf("打满 cap=%d 后第 %d 次应被拦", dungeonDailyEntryCap, dungeonDailyEntryCap+1)
	}
	if remaining != 0 {
		t.Fatalf("打满后 remaining 应为 0，得到 %d", remaining)
	}

	if got := dungeonLockoutRowCount(t, service); got != 1 {
		t.Fatalf("同 (world,unit,dungeon,日窗) 应仅一行，得到 %d 行", got)
	}
	var count int
	if err := service.db.QueryRow(`SELECT entered_count FROM dungeon_lockouts WHERE world_id=? AND unit_id=? AND dungeon_id=?`,
		"w1", "u1", "dungeon").Scan(&count); err != nil {
		t.Fatalf("读 entered_count 失败: %v", err)
	}
	if count != dungeonDailyEntryCap {
		t.Fatalf("超额不应再 +1，entered_count 应停在 %d，得到 %d", dungeonDailyEntryCap, count)
	}

	// 只读查剩余应为 0。
	if r := service.dungeonEntriesRemaining(ctx, "w1", "u1", "dungeon"); r != 0 {
		t.Fatalf("打满后只读剩余应为 0，得到 %d", r)
	}
}

// TestDungeonLockout_WindowResets 验证跨 window_key（不同 UTC 日窗）后名额恢复：
// 直接写一条「昨日已打满」的行，今日窗仍应满额放行（每日 UTC 重置）。
func TestDungeonLockout_WindowResets(t *testing.T) {
	withDungeonLockoutFlag(t, "true")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	// 手插一条「另一个日窗已打满」的行（模拟昨天打满）。
	if _, err := service.db.ExecContext(ctx, `
		INSERT INTO dungeon_lockouts (world_id, unit_id, dungeon_id, window_key, entered_count, last_entered_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"w1", "u1", "dungeon", "1999-12-31", dungeonDailyEntryCap, "1999-12-31T00:00:00Z", "1999-12-31T00:00:00Z"); err != nil {
		t.Fatalf("预置昨日满额行失败: %v", err)
	}

	// 今日窗（dungeonWindowKey 返回 time.Now().UTC 日期）应满额：第一次进入应放行、remaining=cap-1。
	allowed, remaining, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon")
	if err != nil {
		t.Fatalf("跨日后首次进入不应 err: %v", err)
	}
	if !allowed {
		t.Fatalf("跨 UTC 日窗后名额应重置、应放行（昨日满额不影响今日）")
	}
	if remaining != dungeonDailyEntryCap-1 {
		t.Fatalf("跨日后首次进入剩余应为 %d，得到 %d", dungeonDailyEntryCap-1, remaining)
	}

	// 应新写一行（今日窗），与昨日窗那行并存，共 2 行。
	if got := dungeonLockoutRowCount(t, service); got != 2 {
		t.Fatalf("今日窗应新增一行（与昨日窗并存），共应 2 行，得到 %d", got)
	}
}

// TestDungeonLockout_ConcurrentCapNotExceeded 是 TOCTOU 回归：N 个 goroutine 对同一 (world,unit,dungeon,日窗)
// 同时撞门，断言放行次数**恰为** cap、且 entered_count 落库不超 cap。
// 旧实现「SELECT count → if count>=cap → upsert +1」三步非原子：并发请求可读到同一旧 count、都过 cap 检查、都 +1，
// 放行远超 cap（审计实测 cap=3 放行 12）。现 cap 守门下沉单条原子 SQL（SQLite 条件 upsert+RETURNING / MySQL 行锁），
// 无论并发度多高放行都封顶 cap。注意：SQLite 测试库 SetMaxOpenConns(1) 把写串行化，本用例主要锁住「闸决策必须建立在
// 本次真 +1 而非过期前置读」这一语义不回退；真正多连接竞态在 MySQL 路径由 FOR UPDATE 行锁兜底。
func TestDungeonLockout_ConcurrentCapNotExceeded(t *testing.T) {
	withDungeonLockoutFlag(t, "true")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	const goroutines = 50
	var allowedCount int64
	var startBarrier sync.WaitGroup
	var doneBarrier sync.WaitGroup
	startBarrier.Add(1)
	doneBarrier.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer doneBarrier.Done()
			startBarrier.Wait() // 所有 goroutine 在此对齐，尽量逼出并发竞态
			allowed, _, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon")
			if err != nil {
				t.Errorf("并发进入不应 err: %v", err)
				return
			}
			if allowed {
				atomic.AddInt64(&allowedCount, 1)
			}
		}()
	}
	startBarrier.Done() // 放闸：N 个 goroutine 同时撞门
	doneBarrier.Wait()

	if got := atomic.LoadInt64(&allowedCount); got != int64(dungeonDailyEntryCap) {
		t.Fatalf("并发 %d 次撞门，放行次数应恰为 cap=%d，得到 %d（>cap 即 TOCTOU 越闸）", goroutines, dungeonDailyEntryCap, got)
	}

	// 落库 entered_count 必须停在 cap（绝不超）。
	var count int
	if err := service.db.QueryRow(`SELECT entered_count FROM dungeon_lockouts WHERE world_id=? AND unit_id=? AND dungeon_id=?`,
		"w1", "u1", "dungeon").Scan(&count); err != nil {
		t.Fatalf("读 entered_count 失败: %v", err)
	}
	if count != dungeonDailyEntryCap {
		t.Fatalf("并发后 entered_count 应恰为 cap=%d（不超不漏），得到 %d", dungeonDailyEntryCap, count)
	}

	// 闸已闭：再撞一次仍应被拦。
	if allowed, remaining, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon"); err != nil || allowed || remaining != 0 {
		t.Fatalf("并发打满后应恒被拦，得到 allowed=%v remaining=%d err=%v", allowed, remaining, err)
	}
}

// TestDungeonLockout_IndependentByDungeonAndUnit 验证不同 (unit) / (dungeon) 各自独立计数，互不串扰。
func TestDungeonLockout_IndependentByDungeonAndUnit(t *testing.T) {
	withDungeonLockoutFlag(t, "true")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	// u1 打满 dungeon。
	for i := 0; i < dungeonDailyEntryCap; i++ {
		if allowed, _, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon"); err != nil || !allowed {
			t.Fatalf("u1 第 %d 次进入异常 allowed=%v err=%v", i+1, allowed, err)
		}
	}
	if allowed, _, _ := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon"); allowed {
		t.Fatalf("u1 打满后应被拦")
	}

	// u2 应仍满额（不同 unit 独立计数）。
	if allowed, remaining, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u2", "dungeon"); err != nil || !allowed || remaining != dungeonDailyEntryCap-1 {
		t.Fatalf("u2 应独立满额 allowed=%v remaining=%d err=%v", allowed, remaining, err)
	}

	// u1 另一座副本（不同 dungeon_id）应仍满额。
	if allowed, remaining, err := service.checkAndConsumeDungeonEntry(ctx, "w1", "u1", "dungeon_b"); err != nil || !allowed || remaining != dungeonDailyEntryCap-1 {
		t.Fatalf("u1 的另一座副本应独立满额 allowed=%v remaining=%d err=%v", allowed, remaining, err)
	}
}
