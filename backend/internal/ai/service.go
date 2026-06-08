package ai

// 文件说明：AI 编排服务，负责任务路由、主备 provider 调度、schema 校验与失败回退聚合。

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"qunxiang/backend/internal/config"
)

// Service 结构体用于承载该模块的核心数据。
type Service struct {
	logger    *slog.Logger
	validator Validator
	providers map[ProviderName]Provider
	profiles  map[TaskKind]TaskProfile
	cache     *promptCache // prompt 缓存（nil=未启用，QUNXIANG_PROMPT_CACHE 默认关）
}

var (
	activeLLMCallsMu sync.Mutex
	activeLLMCalls   = map[string]ActiveLLMCall{}
	activeLLMCallSeq atomic.Uint64
)

// NewService 创建 AI 编排服务并初始化 provider、路由策略与 JSON 校验器。
func NewService(cfg config.Config, logger *slog.Logger) *Service {
	httpClient := &http.Client{}
	validator := NewJSONSchemaValidator()

	deepSeekEndpoint := EndpointConfig{
		BaseURL:         cfg.DeepSeekBaseURL,
		WireAPI:         cfg.DeepSeekWireAPI,
		APIKey:          cfg.DeepSeekAPIKey,
		Model:           cfg.DeepSeekModel,
		ReasoningEffort: cfg.DeepSeekReasoningEffort,
	}
	openAIEndpoint := EndpointConfig{
		BaseURL:         cfg.OpenAIBaseURL,
		WireAPI:         cfg.OpenAIWireAPI,
		APIKey:          cfg.OpenAIAPIKey,
		Model:           cfg.OpenAIModel,
		ReasoningEffort: cfg.OpenAIReasoningEffort,
	}
	openAIFallbacks := make([]EndpointConfig, 0, len(cfg.OpenAIFallbacks))
	for _, endpoint := range cfg.OpenAIFallbacks {
		openAIFallbacks = append(openAIFallbacks, EndpointConfig{
			BaseURL:         endpoint.BaseURL,
			WireAPI:         endpoint.WireAPI,
			APIKey:          endpoint.APIKey,
			Model:           endpoint.Model,
			ReasoningEffort: endpoint.ReasoningEffort,
		})
	}
	openAIFallbackPrimary := EndpointConfig{}
	openAIFallbackRest := []EndpointConfig(nil)
	if len(openAIFallbacks) > 0 {
		openAIFallbackPrimary = openAIFallbacks[0]
		openAIFallbackRest = openAIFallbacks[1:]
	}
	providers := map[ProviderName]Provider{
		ProviderDeepSeek: NewOpenAICompatibleProvider(
			ProviderDeepSeek,
			deepSeekEndpoint,
			nil,
			StructuredOutputJSONObject,
			httpClient,
			validator,
		),
		ProviderOpenAI: NewOpenAICompatibleProvider(
			ProviderOpenAI,
			openAIEndpoint,
			nil,
			StructuredOutputJSONSchema,
			httpClient,
			validator,
		),
		ProviderOpenAIFallback: NewOpenAICompatibleProvider(
			ProviderOpenAIFallback,
			openAIFallbackPrimary,
			openAIFallbackRest,
			StructuredOutputJSONSchema,
			httpClient,
			validator,
		),
	}

	return &Service{
		logger:    logger.With("component", "ai"),
		validator: validator,
		providers: providers,
		profiles:  ConfiguredTaskProfiles(cfg.LLMTimeout),
		cache:     promptCacheFromEnv(),
	}
}

// preferKnownWorkingEndpoints 在候选端点中优先挑选“已验证可用”的组合。
// 该函数会重排 deepseek/openai 主备链路，并在必要时裁剪 fallback 列表。
func preferKnownWorkingEndpoints(
	deepSeekEndpoint EndpointConfig,
	openAIEndpoint EndpointConfig,
	openAIFallbacks []EndpointConfig,
) (EndpointConfig, EndpointConfig, []EndpointConfig) {
	candidates := make([]EndpointConfig, 0, len(openAIFallbacks)+2)
	candidates = append(candidates, deepSeekEndpoint, openAIEndpoint)
	candidates = append(candidates, openAIFallbacks...)

	deepSeekEndpoint = firstMatchingEndpoint(candidates, "https://2c2ch1u11-share-api-0.hf.space/v1", "deepseek-v4-pro", deepSeekEndpoint)
	anyRouterEndpoint := firstMatchingEndpoint(candidates, "https://anyrouter.wuname.eu.org/v1", "gpt-5.3-codex", EndpointConfig{})
	openRouterEndpoint := firstOpenRouterEndpoint(candidates, EndpointConfig{})
	if openRouterEndpoint.BaseURL != "" {
		openRouterEndpoint.Model = "openai/gpt-oss-120b:free"
	}
	foundKnownEndpoint := anyRouterEndpoint.BaseURL != "" || openRouterEndpoint.BaseURL != "" ||
		isEndpoint(deepSeekEndpoint, "https://2c2ch1u11-share-api-0.hf.space/v1", "deepseek-v4-pro")
	if !foundKnownEndpoint {
		return deepSeekEndpoint, openAIEndpoint, openAIFallbacks
	}
	if !isEndpoint(deepSeekEndpoint, "https://2c2ch1u11-share-api-0.hf.space/v1", "deepseek-v4-pro") {
		deepSeekEndpoint = EndpointConfig{}
	} else {
		deepSeekEndpoint.WireAPI = "chat_completions"
	}

	nextOpenAI := make([]EndpointConfig, 0, 2)
	if openRouterEndpoint.BaseURL != "" {
		openRouterEndpoint.WireAPI = "chat_completions"
		openAIEndpoint = openRouterEndpoint
		minimaxEndpoint := openRouterEndpoint
		minimaxEndpoint.Model = "minimax/minimax-m2.5:free"
		nextOpenAI = append(nextOpenAI, minimaxEndpoint)
		if anyRouterEndpoint.BaseURL != "" {
			anyRouterEndpoint.WireAPI = "responses"
			nextOpenAI = append(nextOpenAI, anyRouterEndpoint)
		}
	} else if anyRouterEndpoint.BaseURL != "" {
		anyRouterEndpoint.WireAPI = "responses"
		openAIEndpoint = anyRouterEndpoint
	}
	return deepSeekEndpoint, openAIEndpoint, nextOpenAI
}

// firstMatchingEndpoint 在候选端点中按 baseURL+model+apikey 匹配首个可用项。
func firstMatchingEndpoint(candidates []EndpointConfig, baseURL string, model string, fallback EndpointConfig) EndpointConfig {
	for _, candidate := range candidates {
		if strings.TrimRight(strings.TrimSpace(candidate.BaseURL), "/") == baseURL &&
			strings.TrimSpace(candidate.Model) == model &&
			strings.TrimSpace(candidate.APIKey) != "" {
			return candidate
		}
	}
	return fallback
}

// firstOpenRouterEndpoint 返回首个可用 OpenRouter 端点，并允许运行时替换模型。
func firstOpenRouterEndpoint(candidates []EndpointConfig, fallback EndpointConfig) EndpointConfig {
	for _, candidate := range candidates {
		if strings.TrimRight(strings.TrimSpace(candidate.BaseURL), "/") == "https://openrouter.ai/api/v1" &&
			strings.TrimSpace(candidate.APIKey) != "" {
			return candidate
		}
	}
	return fallback
}

// isEndpoint 判断端点是否与给定 baseURL/model 完全匹配且携带 APIKey。
func isEndpoint(endpoint EndpointConfig, baseURL string, model string) bool {
	return strings.TrimRight(strings.TrimSpace(endpoint.BaseURL), "/") == baseURL &&
		strings.TrimSpace(endpoint.Model) == model &&
		strings.TrimSpace(endpoint.APIKey) != ""
}

// GenerateJSON 按任务路由调用 provider 并返回结构化 JSON 结果。
// 流程包含：provider 主备切换、schema 校验、失败聚合和规则回退。
func (s *Service) GenerateJSON(ctx context.Context, request CompletionRequest) (CompletionResult, error) {
	if len(request.ResponseSchema) == 0 {
		return CompletionResult{}, errors.New("response schema is required")
	}

	profile, ok := s.profiles[request.Task]
	if !ok {
		return CompletionResult{}, fmt.Errorf("unsupported task kind %q", request.Task)
	}

	// prompt 缓存（§11.2 降本，flag-gated）：相同情境的成功决策直接复用、跳过 LLM。命中的 Output 仍由调用方对当前状态 re-validate。
	cacheable := request.Cacheable && s.cache != nil
	var ckey string
	if cacheable {
		ckey = cacheKey(request)
		if cached, hit := s.cache.get(ckey); hit {
			return cached, nil
		}
	}

	timeout := request.Timeout
	if timeout == 0 {
		timeout = profile.Timeout
	}

	order := providerOrder(profile.Primary, profile.Secondary, profile.Tertiary)
	activeProvider, activeModel := s.activeCallTarget(order)
	activeCallID := registerActiveLLMCall(request, activeProvider, activeModel)
	defer unregisterActiveLLMCall(activeCallID)

	failures := make([]error, 0, len(order))
	attempts := make([]CompletionAttempt, 0, len(order)*4)
	invalidRawOutput := ""

	for _, name := range order {
		provider := s.providers[name]
		if provider == nil {
			failures = append(failures, fmt.Errorf("provider %s is not registered", name))
			continue
		}

		if !provider.Available() {
			failures = append(failures, fmt.Errorf("provider %s is not configured", name))
			continue
		}

		response, err := provider.GenerateJSON(ctx, ProviderRequest{
			Task:           request.Task,
			SystemPrompt:   request.SystemPrompt,
			UserPrompt:     request.UserPrompt,
			Model:          provider.Status().DefaultModel,
			SchemaName:     request.SchemaName,
			ResponseSchema: request.ResponseSchema,
			Temperature:    request.Temperature,
			MaxTokens:      request.MaxTokens,
			Timeout:        timeout,
			Metadata:       request.Metadata,
		})
		attempts = append(attempts, response.Attempts...)

		if err != nil {
			failures = append(failures, fmt.Errorf("%s call failed: %w", name, err))
			continue
		}

		if err := s.validator.Validate(request.SchemaName, request.ResponseSchema, response.Output); err != nil {
			invalidRawOutput = appendInvalidRawOutput(invalidRawOutput, name, response)
			attempts = append(attempts, CompletionAttempt{
				Provider:  string(name),
				Endpoint:  "validation",
				Model:     response.Model,
				Succeeded: false,
				Error:     fmt.Sprintf("schema validation failed: %v", err),
			})
			failures = append(failures, fmt.Errorf("%s returned invalid payload: %w", name, err))
			continue
		}

		result := CompletionResult{
			Provider:     response.Provider,
			Model:        response.Model,
			Output:       response.Output,
			UsedFallback: false,
			Usage:        response.Usage,
			Debug: CompletionDebug{
				Attempts:  attempts,
				RawOutput: response.RawOutput,
			},
		}
		if cacheable { // 只缓存成功的真 LLM 输出（fallback 不入缓存）
			s.cache.put(ckey, result)
		}
		return result, nil
	}

	joined := errors.Join(failures...)
	if request.Fallback == nil {
		return CompletionResult{
			Debug: CompletionDebug{
				Attempts:      attempts,
				RawOutput:     invalidRawOutput,
				FallbackCause: joined.Error(),
			},
		}, joined
	}

	payload, err := request.Fallback.Fallback(ctx, request, joined)
	if err != nil {
		return CompletionResult{}, fmt.Errorf("rule fallback failed: %w", err)
	}

	if err := s.validator.Validate(request.SchemaName, request.ResponseSchema, payload); err != nil {
		return CompletionResult{}, fmt.Errorf("rule fallback returned invalid payload: %w", err)
	}

	s.logger.Warn("llm providers failed, used rule fallback", "task", request.Task, "cause", joined)

	return CompletionResult{
		Provider:     "rules",
		Model:        "fallback",
		Output:       payload,
		UsedFallback: true,
		Debug: CompletionDebug{
			Attempts:      attempts,
			RawOutput:     invalidRawOutput,
			FallbackCause: joined.Error(),
		},
	}, nil
}

// appendInvalidRawOutput 保留 schema 校验失败时的模型原文，便于前端排查非法参数。
func appendInvalidRawOutput(existing string, provider ProviderName, response ProviderResponse) string {
	raw := strings.TrimSpace(response.RawOutput)
	if raw == "" && len(response.Output) > 0 {
		raw = strings.TrimSpace(string(response.Output))
	}
	if raw == "" {
		return existing
	}

	headerParts := []string{string(provider)}
	if strings.TrimSpace(response.Model) != "" {
		headerParts = append(headerParts, response.Model)
	}
	entry := fmt.Sprintf("[%s invalid schema raw_output]\n%s", strings.Join(headerParts, " · "), raw)
	if strings.TrimSpace(existing) == "" {
		return entry
	}
	return strings.TrimSpace(existing) + "\n\n" + entry
}

// Status 返回 AI 服务可观测状态（provider 就绪、任务路由与超时策略）。
func (s *Service) Status() map[string]any {
	providers := make([]ProviderStatus, 0, len(s.providers))
	ready := false
	for _, name := range []ProviderName{ProviderOpenAI, ProviderDeepSeek, ProviderOpenAIFallback} {
		provider := s.providers[name]
		if provider == nil {
			continue
		}

		status := provider.Status()
		if status.Available {
			ready = true
		}
		providers = append(providers, status)
	}

	routes := make(map[string]map[string]any, len(s.profiles))
	for _, task := range []TaskKind{
		TaskIntentParse,
		TaskUnitDecision,
		TaskDialogue,
		TaskBackstory,
		TaskBattleReport,
		TaskDowntime,
	} {
		profile := s.profiles[task]
		routes[string(task)] = map[string]any{
			"primary":    profile.Primary,
			"secondary":  profile.Secondary,
			"tertiary":   profile.Tertiary,
			"timeout_ms": profile.Timeout.Milliseconds(),
		}
	}

	promptCacheStatus := map[string]any{"enabled": false}
	if s.cache != nil {
		promptCacheStatus = s.cache.Stats()
	}

	return map[string]any{
		"ready":        ready,
		"providers":    providers,
		"routes":       routes,
		"prompt_cache": promptCacheStatus,
	}
}

// ActiveCalls 返回当前仍未结束的 LLM 调用快照，供会话调试面板展示。
func ActiveCalls() []ActiveLLMCall {
	activeLLMCallsMu.Lock()
	defer activeLLMCallsMu.Unlock()

	now := time.Now().UTC()
	calls := make([]ActiveLLMCall, 0, len(activeLLMCalls))
	for _, call := range activeLLMCalls {
		call.ElapsedMS = now.Sub(call.StartedAt).Milliseconds()
		calls = append(calls, call)
	}
	sort.Slice(calls, func(left int, right int) bool {
		return calls[left].StartedAt.After(calls[right].StartedAt)
	})
	return calls
}

func (s *Service) activeCallTarget(order []ProviderName) (string, string) {
	for _, name := range order {
		provider := s.providers[name]
		if provider == nil || !provider.Available() {
			continue
		}
		status := provider.Status()
		return string(name), status.DefaultModel
	}
	if len(order) == 0 {
		return "", ""
	}
	return string(order[0]), ""
}

// registerActiveLLMCall 记录一次调用开始，结束时必须 unregister。
func registerActiveLLMCall(request CompletionRequest, provider string, model string) string {
	now := time.Now().UTC()
	id := fmt.Sprintf("active-%d-%d", now.UnixNano(), activeLLMCallSeq.Add(1))
	activeLLMCallsMu.Lock()
	activeLLMCalls[id] = ActiveLLMCall{
		ID:           id,
		Task:         request.Task,
		SchemaName:   request.SchemaName,
		Summary:      "调用中",
		SystemPrompt: request.SystemPrompt,
		UserPrompt:   request.UserPrompt,
		Provider:     provider,
		Model:        model,
		Metadata:     cloneMetadata(request.Metadata),
		StartedAt:    now,
		ElapsedMS:    0,
	}
	activeLLMCallsMu.Unlock()
	return id
}

// unregisterActiveLLMCall 标记一次调用结束。
func unregisterActiveLLMCall(id string) {
	if id == "" {
		return
	}
	activeLLMCallsMu.Lock()
	delete(activeLLMCalls, id)
	activeLLMCallsMu.Unlock()
}

func cloneMetadata(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

// providerOrder 根据配置的 provider 优先级计算去重后的调用顺序。
func providerOrder(names ...ProviderName) []ProviderName {
	order := make([]ProviderName, 0, len(names))
	seen := map[ProviderName]struct{}{}
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		order = append(order, name)
	}
	return order
}
