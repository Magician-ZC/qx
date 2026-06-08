package ai

// 文件说明：提供 LLM 结构化请求批处理能力，支持并发执行、请求去重与结果缓存标记。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	defaultBatchConcurrency = 4
	maxBatchConcurrency     = 16
)

// GenerateJSONBatch 并发执行一批结构化请求，并自动复用相同请求的结果。
// 相同 fingerprint 的请求只会真实调用一次，其余条目标记为 Cached。
func (s *Service) GenerateJSONBatch(
	ctx context.Context,
	requests []BatchRequest,
	options BatchOptions,
) []BatchResult {
	if len(requests) == 0 {
		return nil
	}

	maxConcurrency := options.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = defaultBatchConcurrency
	}
	if maxConcurrency > maxBatchConcurrency {
		maxConcurrency = maxBatchConcurrency
	}
	if maxConcurrency > len(requests) {
		maxConcurrency = len(requests)
	}

	type groupedRequest struct {
		request CompletionRequest
		indexes []int
	}
	groups := map[string]*groupedRequest{}
	orderedFingerprints := make([]string, 0, len(requests))
	for index, batchRequest := range requests {
		fingerprint := completionRequestFingerprint(batchRequest.Request)
		group := groups[fingerprint]
		if group == nil {
			group = &groupedRequest{
				request: batchRequest.Request,
				indexes: []int{},
			}
			groups[fingerprint] = group
			orderedFingerprints = append(orderedFingerprints, fingerprint)
		}
		group.indexes = append(group.indexes, index)
	}

	results := make([]BatchResult, len(requests))
	sem := make(chan struct{}, maxConcurrency)
	callbackMu := sync.Mutex{}

	var wait sync.WaitGroup
	for _, fingerprint := range orderedFingerprints {
		group := groups[fingerprint]
		if group == nil {
			continue
		}

		wait.Add(1)
		go func(request CompletionRequest, indexes []int) {
			defer wait.Done()

			result, err := s.generateOneBatchResult(ctx, sem, request)
			for offset, index := range indexes {
				key := strings.TrimSpace(requests[index].Key)
				if key == "" {
					key = strconv.Itoa(index)
				}
				item := BatchResult{
					Key:     key,
					Request: requests[index].Request,
					Result:  result,
					Err:     err,
					Cached:  offset > 0,
				}
				results[index] = item
				if options.OnComplete != nil {
					callbackMu.Lock()
					options.OnComplete(item)
					callbackMu.Unlock()
				}
			}
		}(group.request, append([]int(nil), group.indexes...))
	}
	wait.Wait()
	return results
}

// generateOneBatchResult 在并发信号量约束下执行单个请求。
// request 是 BatchRequest.Request 原样下发——其 Cacheable 字段一并透传给 GenerateJSON，
// 故 Cacheable=true 的批条目可命中/写入 prompt 缓存（与单发 GenerateJSON 行为一致）。
func (s *Service) generateOneBatchResult(
	ctx context.Context,
	sem chan struct{},
	request CompletionRequest,
) (CompletionResult, error) {
	select {
	case <-ctx.Done():
		return CompletionResult{}, ctx.Err()
	case sem <- struct{}{}:
	}
	defer func() {
		<-sem
	}()

	return s.GenerateJSON(ctx, request)
}

// completionRequestFingerprint 计算请求指纹，用于批处理去重与结果复用。
func completionRequestFingerprint(request CompletionRequest) string {
	var builder strings.Builder
	builder.WriteString("task=")
	builder.WriteString(string(request.Task))
	builder.WriteString("|schema=")
	builder.WriteString(strings.TrimSpace(request.SchemaName))
	builder.WriteString("|temperature=")
	builder.WriteString(fmt.Sprintf("%.6f", request.Temperature))
	builder.WriteString("|max_tokens=")
	builder.WriteString(strconv.Itoa(request.MaxTokens))
	builder.WriteString("|timeout_ns=")
	builder.WriteString(strconv.FormatInt(int64(request.Timeout), 10))
	builder.WriteString("|system=")
	builder.WriteString(strings.TrimSpace(request.SystemPrompt))
	builder.WriteString("|user=")
	builder.WriteString(strings.TrimSpace(request.UserPrompt))
	builder.WriteString("|response_schema=")
	builder.WriteString(string(request.ResponseSchema))

	if len(request.Metadata) > 0 {
		keys := make([]string, 0, len(request.Metadata))
		for key := range request.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		builder.WriteString("|metadata=")
		for _, key := range keys {
			builder.WriteString(key)
			builder.WriteString(":")
			builder.WriteString(request.Metadata[key])
			builder.WriteString(";")
		}
	}

	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}
