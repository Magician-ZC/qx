package ai

// 文件说明：prompt 缓存单元测试——键剥身份、命中/未命中、TTL 过期、LRU 淘汰、Stats 计数。

import (
	"testing"
	"time"
)

func reqFixture(user string) CompletionRequest {
	return CompletionRequest{
		Task:           TaskUnitDecision,
		SchemaName:     "s",
		SystemPrompt:   "sys",
		UserPrompt:     user,
		ResponseSchema: []byte(`{"type":"object"}`),
	}
}

func TestCacheKey_IgnoresMetadataIdentity(t *testing.T) {
	a := reqFixture("同一情境")
	a.Metadata = map[string]string{"unit_id": "u1", "session_id": "s1"}
	b := reqFixture("同一情境")
	b.Metadata = map[string]string{"unit_id": "u2", "session_id": "s2"} // 身份不同
	if cacheKey(a) != cacheKey(b) {
		t.Fatalf("相同情境（仅 Metadata 身份不同）应同键")
	}
	if cacheKey(a) == cacheKey(reqFixture("不同情境")) {
		t.Fatalf("不同 prompt 应不同键")
	}
}

func TestPromptCache_HitMissStore(t *testing.T) {
	c := newPromptCache(8, time.Minute)
	k := cacheKey(reqFixture("x"))
	if _, hit := c.get(k); hit {
		t.Fatalf("空缓存应未命中")
	}
	c.put(k, CompletionResult{Provider: "p", Output: []byte("out")})
	got, hit := c.get(k)
	if !hit || string(got.Output) != "out" {
		t.Fatalf("put 后应命中且 Output 一致，得到 hit=%v out=%q", hit, got.Output)
	}
	st := c.Stats()
	if st["hits"].(int64) != 1 || st["misses"].(int64) != 1 || st["stores"].(int64) != 1 {
		t.Fatalf("Stats 计数不符: %+v", st)
	}
}

func TestPromptCache_TTLExpiry(t *testing.T) {
	c := newPromptCache(8, 2*time.Millisecond)
	k := cacheKey(reqFixture("x"))
	c.put(k, CompletionResult{Output: []byte("out")})
	time.Sleep(10 * time.Millisecond)
	if _, hit := c.get(k); hit {
		t.Fatalf("过期后应未命中")
	}
}

func TestPromptCache_LRUEviction(t *testing.T) {
	c := newPromptCache(2, time.Minute)
	k1, k2, k3 := cacheKey(reqFixture("1")), cacheKey(reqFixture("2")), cacheKey(reqFixture("3"))
	c.put(k1, CompletionResult{Output: []byte("1")})
	c.put(k2, CompletionResult{Output: []byte("2")})
	_, _ = c.get(k1)                                 // 触碰 k1 → k2 成最久未用
	c.put(k3, CompletionResult{Output: []byte("3")}) // 容量 2 → 淘汰 k2
	if _, hit := c.get(k2); hit {
		t.Fatalf("k2 应被 LRU 淘汰")
	}
	if _, hit := c.get(k1); !hit {
		t.Fatalf("k1 近期用过不应被淘汰")
	}
	if _, hit := c.get(k3); !hit {
		t.Fatalf("k3 应在缓存")
	}
	if c.Stats()["evictions"].(int64) != 1 {
		t.Fatalf("应记一次淘汰")
	}
}
