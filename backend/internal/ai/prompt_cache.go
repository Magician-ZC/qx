package ai

// 文件说明：进程内 TTL+LRU 的 LLM 决策缓存（§11.2 最高 ROI 成本项；沙盘 §8.3「决策缓存扩展 ai.batch fingerprint 思路做跨 tick LRU」）。
// 相同情境（task+schema+完整 prompt）的成功决策在 TTL 内直接复用、彻底跳过 LLM 调用。只缓存**成功的真 LLM 输出**
// （规则 fallback/降级结果不入缓存）；键不含身份 Metadata（unit/session 仅作遥测，不影响情境）。**默认开**
// （默认 LRU 2048 / TTL 5min 已是安全档；命中返回的 Output 仍由调用方对**当前**状态 re-validate，不符即优雅回退，故缓存安全），
// 仅当 QUNXIANG_PROMPT_CACHE=false/0/no/off 才显式禁用。注意：仅 Cacheable=true 的请求才会进/查缓存，故关 flag 与
// 「无任何请求标 Cacheable」二者效果一致——本变更不改变未标 Cacheable 请求的行为。

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type promptCacheEntry struct {
	key      string
	result   CompletionResult
	expireAt time.Time
	elem     *list.Element
}

type promptCache struct {
	mu        sync.Mutex
	cap       int
	ttl       time.Duration
	items     map[string]*promptCacheEntry
	lru       *list.List // 前=最近用；满则淘汰尾
	hits      atomic.Int64
	misses    atomic.Int64
	stores    atomic.Int64
	evictions atomic.Int64
}

func newPromptCache(capacity int, ttl time.Duration) *promptCache {
	if capacity <= 0 {
		capacity = 2048
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &promptCache{cap: capacity, ttl: ttl, items: make(map[string]*promptCacheEntry), lru: list.New()}
}

// cacheKey 由 task+schemaName+system+user+responseSchema 派生（**不含 Metadata 身份**）——相同情境同键。
func cacheKey(r CompletionRequest) string {
	h := sha256.New()
	_, _ = h.Write([]byte(string(r.Task)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(r.SchemaName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(r.SystemPrompt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(r.UserPrompt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(r.ResponseSchema)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *promptCache) get(key string) (CompletionResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return CompletionResult{}, false
	}
	if time.Now().After(e.expireAt) { // 过期：清掉并算未命中
		c.removeLocked(e)
		c.misses.Add(1)
		return CompletionResult{}, false
	}
	c.lru.MoveToFront(e.elem)
	c.hits.Add(1)
	return e.result, true
}

func (c *promptCache) put(key string, result CompletionResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		e.result = result
		e.expireAt = time.Now().Add(c.ttl)
		c.lru.MoveToFront(e.elem)
		return
	}
	e := &promptCacheEntry{key: key, result: result, expireAt: time.Now().Add(c.ttl)}
	e.elem = c.lru.PushFront(e)
	c.items[key] = e
	c.stores.Add(1)
	for c.lru.Len() > c.cap {
		back := c.lru.Back()
		if back == nil {
			break
		}
		c.removeLocked(back.Value.(*promptCacheEntry))
		c.evictions.Add(1)
	}
}

func (c *promptCache) removeLocked(e *promptCacheEntry) {
	c.lru.Remove(e.elem)
	delete(c.items, e.key)
}

// Stats 返回缓存遥测（/healthz 的 ai.prompt_cache 块）。
func (c *promptCache) Stats() map[string]any {
	c.mu.Lock()
	size := c.lru.Len()
	c.mu.Unlock()
	h, m := c.hits.Load(), c.misses.Load()
	rate := 0.0
	if h+m > 0 {
		rate = float64(h) / float64(h+m)
	}
	return map[string]any{
		"enabled": true, "size": size, "capacity": c.cap, "ttl_ms": c.ttl.Milliseconds(),
		"hits": h, "misses": m, "hit_rate": rate, "stores": c.stores.Load(), "evictions": c.evictions.Load(),
	}
}

// promptCacheFromEnv 构造 prompt 缓存：**默认开**（默认 LRU 2048 / TTL 5min 安全档），
// 仅当 QUNXIANG_PROMPT_CACHE=false/0/no/off 才返回 nil 禁用；未设/非法/其它真值一律启用。
// 容量与 TTL 可经 _SIZE / _TTL_SECONDS 覆盖（非法或 <=0 时退回默认）。
func promptCacheFromEnv() *promptCache {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_PROMPT_CACHE"))) {
	case "false", "0", "no", "off":
		return nil // 仅显式关才禁用
	}
	size := 2048
	if s, err := strconv.Atoi(os.Getenv("QUNXIANG_PROMPT_CACHE_SIZE")); err == nil && s > 0 {
		size = s
	}
	ttl := 5 * time.Minute
	if s, err := strconv.Atoi(os.Getenv("QUNXIANG_PROMPT_CACHE_TTL_SECONDS")); err == nil && s > 0 {
		ttl = time.Duration(s) * time.Second
	}
	return newPromptCache(size, ttl)
}
