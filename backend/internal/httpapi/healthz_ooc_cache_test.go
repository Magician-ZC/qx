package httpapi

// 文件说明：/healthz 玩家口径 OOC 进程级 TTL 缓存（healthzPlayerOOCCache）的单元测试。
// 验证：缓存 miss 才打「扫描」、窗口内复用结果不重复扫、过 TTL 后重扫；并固化 TTL 取值在 [30s,60s] 区间。
// 该缓存是纯观测读（只缓存对 product_events 的只读 NorthStar 扫描结果），不写任何 session 的 units/relations/memory，
// 不进结算/latch，确定性不受影响——故本测试只需断言「命中不重复打 DB」即等价于「不放大负载且零行为侧效」。

import (
	"testing"
	"time"
)

// TestHealthzPlayerOOCCache_HitMissTTL 覆盖缓存的 miss/hit/过期三态：
//   - 首访（fetchedAt 零值）必 miss → 调一次 fetch；
//   - 同窗口内再访 → 命中，绝不再调 fetch（这正是「高频 /healthz 不重复打 product_events 扫描」的保证）；
//   - 推进时钟过 TTL 后再访 → 重新 miss → 再调一次 fetch。
func TestHealthzPlayerOOCCache_HitMissTTL(t *testing.T) {
	cache := &healthzPlayerOOCCache{}
	ttl := 30 * time.Second
	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	calls := 0
	fetch := func() (float64, int) {
		calls++
		// 返回随调用次数变化的值，便于断言「命中复用的是上次扫描结果」。
		return float64(calls) * 0.1, calls * 10
	}

	// 首访：miss，扫一次。
	rate, samples := cache.playerOOC(base, ttl, fetch)
	if calls != 1 {
		t.Fatalf("首访应触发 1 次扫描，得到 %d", calls)
	}
	if rate != 0.1 || samples != 10 {
		t.Fatalf("首访应返回扫描结果 (0.1,10)，得到 (%v,%d)", rate, samples)
	}

	// 窗口内多次复访：命中，绝不再扫；返回的恒是首访那次的值。
	for i := 0; i < 100; i++ {
		within := base.Add(time.Duration(i) * 200 * time.Millisecond) // 仍 < 30s
		r, s := cache.playerOOC(within, ttl, fetch)
		if calls != 1 {
			t.Fatalf("窗口内第 %d 次复访不应重新扫描，但 calls=%d", i, calls)
		}
		if r != 0.1 || s != 10 {
			t.Fatalf("窗口内复访应复用首访结果 (0.1,10)，得到 (%v,%d)", r, s)
		}
	}

	// 恰好等于 TTL 边界：now-fetchedAt == ttl，不满足 "< ttl"，应视为过期 → 重扫。
	rate, samples = cache.playerOOC(base.Add(ttl), ttl, fetch)
	if calls != 2 {
		t.Fatalf("到达 TTL 边界应重新扫描（第 2 次），得到 calls=%d", calls)
	}
	if rate != 0.2 || samples != 20 {
		t.Fatalf("过期重扫应返回新结果 (0.2,20)，得到 (%v,%d)", rate, samples)
	}

	// 边界之后立刻再访：在新的窗口内 → 命中，仍是第 2 次的值。
	r, s := cache.playerOOC(base.Add(ttl).Add(time.Second), ttl, fetch)
	if calls != 2 {
		t.Fatalf("重扫后新窗口内复访不应再扫，calls=%d", calls)
	}
	if r != 0.2 || s != 20 {
		t.Fatalf("新窗口内复访应复用第 2 次结果 (0.2,20)，得到 (%v,%d)", r, s)
	}
}

// TestHealthzPlayerOOCCache_FetchValuesCached 固化「fetch 返回 0 也照常缓存」：
// 失败/空查询返回 (0,0) 时，缓存仍记录该结果并在窗口内短路，避免热路径对失败查询反复打 DB。
func TestHealthzPlayerOOCCache_FetchValuesCached(t *testing.T) {
	cache := &healthzPlayerOOCCache{}
	ttl := 30 * time.Second
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	calls := 0
	zeroFetch := func() (float64, int) { calls++; return 0, 0 }

	if r, s := cache.playerOOC(now, ttl, zeroFetch); r != 0 || s != 0 {
		t.Fatalf("空结果应原样返回 (0,0)，得到 (%v,%d)", r, s)
	}
	// 窗口内再访：即便上次是 (0,0) 也必须命中、不重打 DB。
	if r, s := cache.playerOOC(now.Add(time.Second), ttl, zeroFetch); r != 0 || s != 0 {
		t.Fatalf("空结果窗口内复访应仍 (0,0)，得到 (%v,%d)", r, s)
	}
	if calls != 1 {
		t.Fatalf("空结果也应被缓存：窗口内应只扫 1 次，得到 %d", calls)
	}
}

// TestHealthzPlayerOOCTTL_InRange 固化 TTL 落在任务要求的 [30s,60s] 区间（防回归把缓存意外改没/改太长）。
func TestHealthzPlayerOOCTTL_InRange(t *testing.T) {
	if healthzPlayerOOCTTL < 30*time.Second || healthzPlayerOOCTTL > 60*time.Second {
		t.Fatalf("healthzPlayerOOCTTL 应在 [30s,60s]，得到 %v", healthzPlayerOOCTTL)
	}
}
