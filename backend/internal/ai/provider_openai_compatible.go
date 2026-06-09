package ai

// 文件说明：OpenAI 兼容 provider 适配层，处理多端点重试、结构化输出模式与响应清洗。

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	http429RetryCount       = 3
	http429RetryDelay       = 3 * time.Second
	failoverChainRetryCount = 1
	failoverChainRetryDelay = 3 * time.Second
)

var sensitiveJSONRedactionRules = []struct {
	pattern *regexp.Regexp
	replace string
}{
	{pattern: regexp.MustCompile(`(?i)[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}`), replace: "[email_redacted]"},
	{pattern: regexp.MustCompile(`(?:\+?86[- ]?)?1[3-9]\d{9}`), replace: "[phone_redacted]"},
	{pattern: regexp.MustCompile(`\b\d{17}[\dXx]\b`), replace: "[id_redacted]"},
	{pattern: regexp.MustCompile(`(?i)\b(?:how to make (?:a )?bomb|school shooting plan|terror attack guide)\b`), replace: "[sensitive_redacted]"},
	{pattern: regexp.MustCompile(`强奸|儿童色情|制毒教程|恐怖袭击教程`), replace: "[sensitive_redacted]"},
}

// StructuredOutputMode 类型定义用于统一该模块的数据表达。
type StructuredOutputMode string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	StructuredOutputJSONSchema StructuredOutputMode = "json_schema"
	StructuredOutputJSONObject StructuredOutputMode = "json_object"
)

// EndpointConfig 结构体用于承载该模块的核心数据。
type EndpointConfig struct {
	BaseURL         string
	WireAPI         string
	APIKey          string
	Model           string
	ReasoningEffort string
}

// providerEndpoint 结构体用于承载该模块的核心数据。
type providerEndpoint struct {
	role            string
	baseURL         string
	wireAPI         string
	apiKey          string
	model           string
	reasoningEffort string
}

// requestSpec 结构体用于承载该模块的核心数据。
type requestSpec struct {
	path   string
	body   []byte
	stream bool
}

// OpenAICompatibleProvider 结构体用于承载该模块的核心数据。
type OpenAICompatibleProvider struct {
	name        ProviderName
	mode        StructuredOutputMode
	httpClient  *http.Client
	validator   Validator
	endpoints   []providerEndpoint
	rotationSeq atomic.Uint64
}

// NewOpenAICompatibleProvider 创建兼容 OpenAI 协议的 provider，并装配主备端点链路。
func NewOpenAICompatibleProvider(
	name ProviderName,
	primary EndpointConfig,
	fallbacks []EndpointConfig,
	mode StructuredOutputMode,
	httpClient *http.Client,
	validator Validator,
) *OpenAICompatibleProvider {
	provider := &OpenAICompatibleProvider{
		name:       name,
		mode:       mode,
		httpClient: httpClient,
		validator:  validator,
	}
	if provider.httpClient == nil {
		provider.httpClient = http.DefaultClient
	}

	provider.appendEndpoint("primary", primary)
	for index, fallback := range fallbacks {
		provider.appendEndpoint(fmt.Sprintf("fallback_%d", index+1), fallback)
	}

	return provider
}

// Name 返回 provider 名称。
func (p *OpenAICompatibleProvider) Name() ProviderName {
	return p.name
}

// Available 判断是否至少存在一个可用端点配置。
func (p *OpenAICompatibleProvider) Available() bool {
	return len(p.endpoints) > 0
}

// Status 返回 provider 对外状态快照（主端点、默认模型、结构化模式）。
func (p *OpenAICompatibleProvider) Status() ProviderStatus {
	if len(p.endpoints) == 0 {
		return ProviderStatus{
			Name:                 p.name,
			Available:            false,
			StructuredOutputMode: string(p.mode),
		}
	}

	primary := p.endpoints[0]
	return ProviderStatus{
		Name:                 p.name,
		Available:            p.Available(),
		DefaultModel:         primary.model,
		BaseURL:              primary.baseURL,
		StructuredOutputMode: string(p.mode),
	}
}

// GenerateJSON 按端点链路执行请求，并在失败时进行跨端点/跨链路重试。
func (p *OpenAICompatibleProvider) GenerateJSON(
	ctx context.Context,
	request ProviderRequest,
) (ProviderResponse, error) {
	if !p.Available() {
		return ProviderResponse{}, fmt.Errorf("provider %s is not configured", p.name)
	}

	totalChainAttempts := failoverChainRetryCount + 1
	attempts := make([]CompletionAttempt, 0, len(p.endpoints)*totalChainAttempts)
	failures := make([]error, 0, len(p.endpoints)*totalChainAttempts)

	for chainAttempt := 0; chainAttempt < totalChainAttempts; chainAttempt++ {
		endpoints := p.orderedEndpointsForRequest()
		for index := 0; index < len(endpoints); index++ {
			endpoint := endpoints[index]
			if chainAttempt > 0 && !retryableEndpoint(endpoint) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return ProviderResponse{Attempts: attempts}, err
			}

			result, endpointAttempts, err := p.generateWithEndpoint(ctx, endpoint, request)
			attempts = append(attempts, endpointAttempts...)
			if err == nil {
				if validationErr := p.validateProviderResponse(endpoint, request, result); validationErr != nil {
					attempts = append(attempts, CompletionAttempt{
						Provider:  string(p.name),
						Endpoint:  endpoint.role + "_validation",
						BaseURL:   endpoint.baseURL,
						WireAPI:   endpoint.wireAPI,
						Model:     result.Model,
						Succeeded: false,
						Error:     validationErr.Error(),
					})
					failures = append(failures, fmt.Errorf(
						"chain attempt %d/%d on %s[%s]: returned invalid payload: %w",
						chainAttempt+1,
						totalChainAttempts,
						p.name,
						endpoint.role,
						validationErr,
					))
					if !shouldTryNextKeyForSameModel(endpointAttempts) {
						signature := endpointRotationSignature(endpoint)
						for index+1 < len(endpoints) && endpointRotationSignature(endpoints[index+1]) == signature {
							index++
						}
					}
					continue
				}
				result.Attempts = append([]CompletionAttempt{}, attempts...)
				return result, nil
			}

			failures = append(failures, fmt.Errorf(
				"chain attempt %d/%d on %s[%s]: %w",
				chainAttempt+1,
				totalChainAttempts,
				p.name,
				endpoint.role,
				err,
			))
			if !shouldTryNextKeyForSameModel(endpointAttempts) {
				signature := endpointRotationSignature(endpoint)
				for index+1 < len(endpoints) && endpointRotationSignature(endpoints[index+1]) == signature {
					index++
				}
			}
		}

		if chainAttempt+1 < totalChainAttempts {
			timer := time.NewTimer(failoverChainRetryDelay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ProviderResponse{Attempts: attempts}, ctx.Err()
			case <-timer.C:
			}
		}
	}

	return ProviderResponse{Attempts: attempts}, errors.Join(failures...)
}

func (p *OpenAICompatibleProvider) validateProviderResponse(
	endpoint providerEndpoint,
	request ProviderRequest,
	response ProviderResponse,
) error {
	if p.validator == nil {
		return nil
	}
	if err := p.validator.Validate(request.SchemaName, request.ResponseSchema, response.Output); err != nil {
		return fmt.Errorf("schema validation failed for %s[%s]: %w", p.name, endpoint.role, err)
	}
	return nil
}

func shouldTryNextKeyForSameModel(attempts []CompletionAttempt) bool {
	for index := len(attempts) - 1; index >= 0; index-- {
		if attempts[index].StatusCode == 0 {
			continue
		}
		return attempts[index].StatusCode == http.StatusTooManyRequests
	}
	return false
}

// generateWithEndpoint 在单个端点上执行请求，处理请求构造与 429 重试策略。
func (p *OpenAICompatibleProvider) generateWithEndpoint(
	ctx context.Context,
	endpoint providerEndpoint,
	request ProviderRequest,
) (ProviderResponse, []CompletionAttempt, error) {
	// LLM 全局 reasoning effort 热切（GM 运营，反 P2W：全局单值非付费分档）：请求级非空时覆盖端点构造期配置。
	// endpoint 是值拷贝，覆盖只作用于本次请求，绝不污染 provider 的端点配置；下游 buildRequestSpec/dashScope 检查均生效。
	if e := strings.TrimSpace(request.ReasoningEffort); e != "" {
		endpoint.reasoningEffort = e
	}
	spec, err := p.buildRequestSpec(endpoint, request)
	if err != nil {
		attempt := CompletionAttempt{
			Provider: string(p.name),
			Endpoint: endpoint.role,
			BaseURL:  endpoint.baseURL,
			WireAPI:  endpoint.wireAPI,
			Model:    endpoint.model,
			Error:    err.Error(),
		}
		return ProviderResponse{}, []CompletionAttempt{attempt}, err
	}

	retryCount := p.httpRetryCount(endpoint)
	attempts := make([]CompletionAttempt, 0, retryCount+1)
	for retry := 0; retry <= retryCount; retry++ {
		result, attempt, shouldRetry, err := p.sendOnce(ctx, endpoint, request, spec)
		attempts = append(attempts, attempt)
		if err == nil {
			return result, attempts, nil
		}
		if !shouldRetry || retry == retryCount {
			return ProviderResponse{}, attempts, err
		}

		timer := time.NewTimer(http429RetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ProviderResponse{}, attempts, ctx.Err()
		case <-timer.C:
		}
	}

	return ProviderResponse{}, attempts, fmt.Errorf("unreachable retry loop")
}

// sendOnce 发送一次 HTTP 调用并解析响应，返回是否可重试的判定。
func (p *OpenAICompatibleProvider) sendOnce(
	ctx context.Context,
	endpoint providerEndpoint,
	request ProviderRequest,
	spec requestSpec,
) (result ProviderResponse, attempt CompletionAttempt, shouldRetry bool, err error) {
	startedAt := time.Now()
	attempt = CompletionAttempt{
		Provider:  string(p.name),
		Endpoint:  endpoint.role,
		BaseURL:   endpoint.baseURL,
		WireAPI:   endpoint.wireAPI,
		Model:     endpoint.model,
		StartedAt: startedAt.UTC().Format(time.RFC3339Nano),
	}
	defer func() {
		attempt.DurationMS = time.Since(startedAt).Milliseconds()
	}()

	requestCtx := ctx
	cancel := func() {}
	if request.Timeout > 0 {
		requestCtx, cancel = context.WithTimeout(ctx, request.Timeout)
	}
	defer cancel()

	httpRequest, err := http.NewRequestWithContext(
		requestCtx,
		http.MethodPost,
		endpoint.baseURL+spec.path,
		bytes.NewReader(spec.body),
	)
	if err != nil {
		attempt.Error = err.Error()
		return ProviderResponse{}, attempt, false, fmt.Errorf("build %s[%s] request: %w", p.name, endpoint.role, err)
	}

	httpRequest.Header.Set("Authorization", "Bearer "+endpoint.apiKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("User-Agent", "qunxiang-backend/0.1")

	response, err := p.httpClient.Do(httpRequest)
	if err != nil {
		attempt.Error = err.Error()
		return ProviderResponse{}, attempt, shouldRetryRequestError(endpoint, err), fmt.Errorf("send %s[%s] request: %w", p.name, endpoint.role, err)
	}
	defer response.Body.Close()

	attempt.StatusCode = response.StatusCode
	if response.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(response.Body)
		attempt.Error = strings.TrimSpace(string(body))
		return ProviderResponse{}, attempt, false, fmt.Errorf(
			"%s[%s] request failed with 429: %s",
			p.name,
			endpoint.role,
			strings.TrimSpace(string(body)),
		)
	}

	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		attempt.Error = strings.TrimSpace(string(body))
		return ProviderResponse{}, attempt, false, fmt.Errorf(
			"%s[%s] request failed with %d: %s",
			p.name,
			endpoint.role,
			response.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	switch {
	case endpoint.wireAPI == "responses" && spec.stream:
		result, err = parseStreamingResponsesResponse(string(p.name), endpoint, request, response.Body)
	case endpoint.wireAPI == "responses":
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			attempt.Error = readErr.Error()
			return ProviderResponse{}, attempt, false, fmt.Errorf("read %s[%s] response: %w", p.name, endpoint.role, readErr)
		}
		result, err = parseResponsesResponse(string(p.name), endpoint, request, body)
	case spec.stream:
		result, err = parseStreamingChatCompletionResponse(string(p.name), endpoint, request, response.Body)
	default:
		body, readErr := io.ReadAll(response.Body)
		if readErr != nil {
			attempt.Error = readErr.Error()
			return ProviderResponse{}, attempt, false, fmt.Errorf("read %s[%s] response: %w", p.name, endpoint.role, readErr)
		}
		result, err = parseChatCompletionResponse(string(p.name), endpoint, request, body)
	}
	if err != nil {
		attempt.Error = err.Error()
		return ProviderResponse{}, attempt, false, err
	}

	attempt.Succeeded = true
	return result, attempt, false, nil
}

// retryableEndpoint 判断端点是否允许重试（本地调试端点默认不重试）。
func retryableEndpoint(endpoint providerEndpoint) bool {
	return endpoint.baseURL != "http://localhost:8317/v1"
}

// shouldRetryRequestError 根据错误类型判定是否应重试网络请求。
func shouldRetryRequestError(endpoint providerEndpoint, err error) bool {
	if !retryableEndpoint(endpoint) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return !netErr.Timeout()
	}
	return false
}

// httpRetryCount 返回端点级 HTTP 重试次数上限。
func (p *OpenAICompatibleProvider) httpRetryCount(endpoint providerEndpoint) int {
	if !retryableEndpoint(endpoint) {
		return 0
	}
	return http429RetryCount
}

// appendEndpoint 构建并去重后追加端点到 provider 链路。APIKey 支持逗号/换行分隔的多 key。
func (p *OpenAICompatibleProvider) appendEndpoint(role string, config EndpointConfig) {
	keys := splitAPIKeys(config.APIKey)
	if len(keys) == 0 {
		return
	}
	for _, key := range keys {
		config.APIKey = key
		endpoint, ok := buildProviderEndpoint(role, config)
		if !ok {
			continue
		}

		exists := false
		for _, existing := range p.endpoints {
			if existing.baseURL == endpoint.baseURL &&
				existing.wireAPI == endpoint.wireAPI &&
				existing.apiKey == endpoint.apiKey &&
				existing.model == endpoint.model {
				exists = true
				break
			}
		}
		if exists {
			continue
		}

		p.endpoints = append(p.endpoints, endpoint)
	}
}

func (p *OpenAICompatibleProvider) orderedEndpointsForRequest() []providerEndpoint {
	if len(p.endpoints) <= 1 {
		return append([]providerEndpoint{}, p.endpoints...)
	}
	seq := int(p.rotationSeq.Add(1) - 1)
	ordered := make([]providerEndpoint, 0, len(p.endpoints))
	for index := 0; index < len(p.endpoints); {
		end := index + 1
		signature := endpointRotationSignature(p.endpoints[index])
		for end < len(p.endpoints) && endpointRotationSignature(p.endpoints[end]) == signature {
			end++
		}
		group := p.endpoints[index:end]
		offset := 0
		if len(group) > 1 {
			offset = seq % len(group)
		}
		ordered = append(ordered, group[offset:]...)
		ordered = append(ordered, group[:offset]...)
		index = end
	}
	return ordered
}

func endpointRotationSignature(endpoint providerEndpoint) string {
	return strings.Join([]string{
		endpoint.role,
		endpoint.baseURL,
		endpoint.wireAPI,
		endpoint.model,
	}, "\x00")
}

func splitAPIKeys(raw string) []string {
	normalized := strings.NewReplacer("\n", ",", "\r", ",", ";", ",").Replace(raw)
	parts := strings.Split(normalized, ",")
	keys := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		key := strings.TrimSpace(part)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	return keys
}

// buildProviderEndpoint 把外部端点配置规范化为运行时端点结构。
func buildProviderEndpoint(role string, config EndpointConfig) (providerEndpoint, bool) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	apiKey := strings.TrimSpace(config.APIKey)
	model := strings.TrimSpace(config.Model)
	if baseURL == "" || apiKey == "" || model == "" {
		return providerEndpoint{}, false
	}
	if strings.HasPrefix(model, "anti/gemini-") {
		return providerEndpoint{}, false
	}

	return providerEndpoint{
		role:            role,
		baseURL:         baseURL,
		wireAPI:         normalizeWireAPI(config.WireAPI),
		apiKey:          apiKey,
		model:           model,
		reasoningEffort: strings.TrimSpace(config.ReasoningEffort),
	}, true
}

// normalizeWireAPI 把多种写法归一成 chat_completions 或 responses。
func normalizeWireAPI(wireAPI string) string {
	normalized := strings.TrimSpace(strings.ToLower(wireAPI))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "responses", "response":
		return "responses"
	case "chat_completions", "chat.completions", "chat", "openai", "":
		return "chat_completions"
	default:
		return "chat_completions"
	}
}

// buildRequestSpec 依据 wire API 构建请求路径、payload 与是否流式。
// 同时处理不同平台的兼容参数与结构化输出约束。
func (p *OpenAICompatibleProvider) buildRequestSpec(endpoint providerEndpoint, request ProviderRequest) (requestSpec, error) {
	if len(request.ResponseSchema) == 0 {
		return requestSpec{}, errors.New("response schema is required")
	}

	systemPrompt := strings.TrimSpace(request.SystemPrompt)
	if systemPrompt == "" {
		systemPrompt = "You are a game simulation model. Return JSON only."
	}

	if endpoint.wireAPI == "responses" {
		systemPrompt = withSchemaInstructions(systemPrompt, request.ResponseSchema)
		payload := map[string]any{
			"model": firstNonEmpty(endpoint.model, request.Model),
			"input": buildResponsesInput(systemPrompt, request.UserPrompt),
		}
		if request.MaxTokens > 0 {
			payload["max_output_tokens"] = request.MaxTokens
		}
		if request.Temperature > 0 {
			payload["temperature"] = request.Temperature
		}
		if endpoint.reasoningEffort != "" {
			payload["reasoning"] = map[string]any{
				"effort": endpoint.reasoningEffort,
			}
		}

		spec := requestSpec{
			path: "/responses",
		}
		if shouldIncludeResponsesMessages(endpoint) {
			payload["messages"] = []map[string]string{
				{
					"role":    "system",
					"content": systemPrompt,
				},
				{
					"role":    "user",
					"content": request.UserPrompt,
				},
			}
		}

		body, err := json.Marshal(payload)
		if err != nil {
			return requestSpec{}, err
		}
		spec.body = body
		return spec, nil
	}

	responseFormat, systemPrompt, err := p.buildChatResponseFormat(endpoint, request.SchemaName, request.ResponseSchema, systemPrompt)
	if err != nil {
		return requestSpec{}, err
	}

	effectiveMaxTokens := request.MaxTokens
	payload := map[string]any{
		"model": firstNonEmpty(endpoint.model, request.Model),
		"messages": []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: request.UserPrompt},
		},
	}
	if responseFormat != nil {
		payload["response_format"] = responseFormat
	}
	if request.Temperature > 0 {
		payload["temperature"] = request.Temperature
	}
	if effectiveMaxTokens > 0 {
		payload["max_tokens"] = effectiveMaxTokens
	}
	if isDashScopeEndpoint(endpoint) {
		payload["enable_thinking"] = dashScopeThinkingEnabled(endpoint)
	}

	spec := requestSpec{
		path:   "/chat/completions",
		stream: shouldStreamChat(endpoint),
	}
	if spec.stream {
		payload["stream"] = true
	}
	if endpoint.baseURL == "https://integrate.api.nvidia.com/v1" && endpoint.model == "bytedance/seed-oss-36b-instruct" {
		payload["thinking_budget"] = -1
		payload["top_p"] = 0.95
		if effectiveMaxTokens < 4096 {
			payload["max_tokens"] = 4096
		}
	}
	if endpoint.baseURL == "https://integrate.api.nvidia.com/v1" && endpoint.model == "moonshotai/kimi-k2.5" {
		payload["temperature"] = 0.6
		payload["top_p"] = 0.95
		payload["extra_body"] = map[string]any{
			"thinking": map[string]any{
				"type": "disabled",
			},
		}
	}
	if endpoint.baseURL == "https://openrouter.ai/api/v1" && strings.HasPrefix(endpoint.model, "z-ai/glm-4.5") {
		payload["reasoning"] = map[string]any{
			"effort":  "none",
			"exclude": true,
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return requestSpec{}, err
	}
	spec.body = body
	return spec, nil
}

// shouldIncludeResponsesMessages 判断某些兼容端点是否需要额外注入 messages 字段。
func shouldIncludeResponsesMessages(endpoint providerEndpoint) bool {
	return endpoint.baseURL == "https://anyrouter.wuname.eu.org/v1" ||
		endpoint.baseURL == "https://a-ocnfniawgw.cn-shanghai.fcapp.run/v1"
}

// shouldStreamChat 判断 chat-completions 请求是否走流式通道。
func shouldStreamChat(endpoint providerEndpoint) bool {
	if endpoint.wireAPI != "chat_completions" {
		return false
	}

	return endpoint.baseURL == "https://geek.tm2.xin/v1" ||
		(endpoint.baseURL == "https://integrate.api.nvidia.com/v1" &&
			(endpoint.model == "z-ai/glm5" || endpoint.model == "bytedance/seed-oss-36b-instruct"))
}

// buildChatResponseFormat 构建 chat-completions 的 response_format。
// 对不支持原生 schema 的端点会改为提示词注入约束。
func (p *OpenAICompatibleProvider) buildChatResponseFormat(
	endpoint providerEndpoint,
	schemaName string,
	responseSchema []byte,
	systemPrompt string,
) (any, string, error) {
	if !supportsNativeChatResponseFormat(endpoint) {
		return nil, withSchemaInstructions(systemPrompt, responseSchema), nil
	}

	useStrictSchema := p.mode == StructuredOutputJSONSchema && endpoint.role == "primary"
	if useStrictSchema {
		var schemaDocument map[string]any
		if err := json.Unmarshal(responseSchema, &schemaDocument); err != nil {
			return nil, "", fmt.Errorf("decode response schema: %w", err)
		}

		return map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   safeSchemaName(schemaName),
				"strict": true,
				"schema": schemaDocument,
			},
		}, systemPrompt, nil
	}

	switch p.mode {
	case StructuredOutputJSONSchema, StructuredOutputJSONObject:
		return map[string]any{
			"type": "json_object",
		}, withSchemaInstructions(systemPrompt, responseSchema), nil
	default:
		return nil, "", fmt.Errorf("unsupported structured output mode %q", p.mode)
	}
}

// supportsNativeChatResponseFormat 判断端点是否支持原生 response_format/json_schema。
func supportsNativeChatResponseFormat(endpoint providerEndpoint) bool {
	switch endpoint.baseURL {
	case "https://2c2ch1u11-share-api-0.hf.space/v1",
		"https://openrouter.ai/api/v1",
		"https://anyrouter.wuname.eu.org/v1",
		"https://dashscope.aliyuncs.com/compatible-mode/v1":
		return false
	default:
		return true
	}
}

func isDashScopeEndpoint(endpoint providerEndpoint) bool {
	return endpoint.baseURL == "https://dashscope.aliyuncs.com/compatible-mode/v1"
}

func dashScopeThinkingEnabled(endpoint providerEndpoint) bool {
	switch strings.ToLower(strings.TrimSpace(endpoint.reasoningEffort)) {
	case "0", "false", "off", "none", "disabled", "disable":
		return false
	case "1", "true", "on", "enabled", "enable", "minimal", "low", "medium", "high":
		// minimal=「最弱思考但仍思考」，与 low/medium/high 同向、与 responses 路径对任意非空 effort 透传的语义一致
		// （catalog llm.reasoning_effort 合法枚举含 minimal，GM 热切设 minimal 时不应在 dashScope 上静默关掉思考）。
		return true
	default:
		return false
	}
}

// withSchemaInstructions 把 JSON Schema 约束拼接到 system prompt 中。
func withSchemaInstructions(systemPrompt string, responseSchema []byte) string {
	return strings.TrimSpace(systemPrompt + "\nReturn only a JSON object that validates against this JSON Schema:\n" + string(responseSchema))
}

// buildResponsesInput 组装 Responses API 所需的多段输入结构。
func buildResponsesInput(systemPrompt string, userPrompt string) []map[string]any {
	return []map[string]any{
		{
			"role": "system",
			"content": []map[string]string{
				{
					"type": "input_text",
					"text": systemPrompt,
				},
			},
		},
		{
			"role": "user",
			"content": []map[string]string{
				{
					"type": "input_text",
					"text": userPrompt,
				},
			},
		},
	}
}

// parseChatCompletionResponse 解析非流式 chat-completions 响应并提取 JSON 输出。
func parseChatCompletionResponse(
	providerName string,
	endpoint providerEndpoint,
	request ProviderRequest,
	responseBody []byte,
) (ProviderResponse, error) {
	var decoded chatCompletionResponse
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return ProviderResponse{}, fmt.Errorf("decode %s[%s] response: %w", providerName, endpoint.role, err)
	}

	if len(decoded.Choices) == 0 {
		return ProviderResponse{}, fmt.Errorf(
			"%s[%s] response had no choices: %s",
			providerName,
			endpoint.role,
			describeUnexpectedChatResponse(responseBody),
		)
	}

	content, err := extractMessageContent(decoded.Choices[0].Message.Content)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("%s[%s] response content: %w", providerName, endpoint.role, err)
	}

	payload, err := sanitizeJSONPayload(content)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("%s[%s] response payload: %w", providerName, endpoint.role, err)
	}

	return ProviderResponse{
		Provider:  providerName,
		Model:     firstNonEmpty(decoded.Model, endpoint.model, request.Model),
		Output:    payload,
		RawOutput: content,
		Usage: Usage{
			PromptTokens:     decoded.Usage.PromptTokens,
			CompletionTokens: decoded.Usage.CompletionTokens,
			TotalTokens:      decoded.Usage.TotalTokens,
		},
	}, nil
}

func describeUnexpectedChatResponse(responseBody []byte) string {
	var decoded map[string]any
	if err := json.Unmarshal(responseBody, &decoded); err == nil {
		if errorValue, ok := decoded["error"]; ok {
			return "provider returned error payload: " + compactJSONValue(errorValue, 800)
		}
		if choices, ok := decoded["choices"].([]any); ok && len(choices) == 0 {
			return "provider returned empty choices; body=" + compactJSONBytes(responseBody, 800)
		}
		return "body=" + compactJSONBytes(responseBody, 800)
	}
	return "body=" + truncateString(strings.TrimSpace(string(responseBody)), 800)
}

func compactJSONValue(value any, limit int) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return truncateString(fmt.Sprint(value), limit)
	}
	return truncateString(string(encoded), limit)
}

func compactJSONBytes(raw []byte, limit int) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return truncateString(strings.TrimSpace(string(raw)), limit)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return truncateString(strings.TrimSpace(string(raw)), limit)
	}
	return truncateString(string(encoded), limit)
}

func truncateString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return value[:limit] + "...[truncated]"
}

// parseStreamingChatCompletionResponse 解析流式 chat-completions 的 SSE 增量文本。
func parseStreamingChatCompletionResponse(
	providerName string,
	endpoint providerEndpoint,
	request ProviderRequest,
	body io.Reader,
) (ProviderResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	var contentBuilder strings.Builder
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			break
		}

		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			continue
		}
		choices, _ := decoded["choices"].([]any)
		if len(choices) == 0 {
			continue
		}

		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		contentBuilder.WriteString(extractContentFragments(delta["content"]))
	}
	if err := scanner.Err(); err != nil {
		return ProviderResponse{}, fmt.Errorf("scan %s[%s] stream: %w", providerName, endpoint.role, err)
	}

	content := strings.TrimSpace(contentBuilder.String())
	if content == "" {
		return ProviderResponse{}, fmt.Errorf("%s[%s] stream did not include message.content", providerName, endpoint.role)
	}

	payload, err := sanitizeJSONPayload(content)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("%s[%s] stream payload: %w", providerName, endpoint.role, err)
	}

	return ProviderResponse{
		Provider:  providerName,
		Model:     firstNonEmpty(endpoint.model, request.Model),
		Output:    payload,
		RawOutput: content,
	}, nil
}

// parseResponsesResponse 解析非流式 responses API 输出并提取结构化文本。
func parseResponsesResponse(
	providerName string,
	endpoint providerEndpoint,
	request ProviderRequest,
	responseBody []byte,
) (ProviderResponse, error) {
	var decoded map[string]any
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return ProviderResponse{}, fmt.Errorf("decode %s[%s] responses payload: %w", providerName, endpoint.role, err)
	}

	content, err := extractResponsesOutputText(decoded)
	if err != nil {
		if shouldIncludeResponsesMessages(endpoint) {
			streamResult, streamErr := parseStreamingResponsesResponse(providerName, endpoint, request, bytes.NewReader(responseBody))
			if streamErr == nil {
				return streamResult, nil
			}
		}
		return ProviderResponse{}, fmt.Errorf("%s[%s] responses output: %w", providerName, endpoint.role, err)
	}

	payload, err := sanitizeJSONPayload(content)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("%s[%s] responses payload: %w", providerName, endpoint.role, err)
	}

	return ProviderResponse{
		Provider:  providerName,
		Model:     firstNonEmpty(stringValue(decoded["model"]), endpoint.model, request.Model),
		Output:    payload,
		RawOutput: content,
		Usage:     extractUsage(decoded),
	}, nil
}

// parseStreamingResponsesResponse 解析流式 responses 输出，兼容多种事件格式回退。
func parseStreamingResponsesResponse(
	providerName string,
	endpoint providerEndpoint,
	request ProviderRequest,
	body io.Reader,
) (ProviderResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024), 1024*1024)

	contentParts := make([]string, 0, 8)
	rawJSONLines := make([]string, 0, 8)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			rawJSONLines = append(rawJSONLines, line)
			continue
		}

		payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		if payload == "[DONE]" {
			break
		}

		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			continue
		}

		switch decoded["type"] {
		case "response.output_text.delta":
			if delta := strings.TrimSpace(stringValue(decoded["delta"])); delta != "" {
				contentParts = append(contentParts, delta)
			}
			continue
		case "response.output_text.done":
			if len(contentParts) == 0 {
				if text := strings.TrimSpace(stringValue(decoded["text"])); text != "" {
					contentParts = append(contentParts, text)
				}
			}
			continue
		}

		if text, err := extractResponsesOutputTextOrEmpty(decoded); err == nil && text != "" {
			contentParts = append(contentParts, text)
		}
	}
	if err := scanner.Err(); err != nil {
		return ProviderResponse{}, fmt.Errorf("scan %s[%s] responses stream: %w", providerName, endpoint.role, err)
	}

	content := strings.TrimSpace(strings.Join(contentParts, ""))
	if content == "" && len(rawJSONLines) > 0 {
		for _, line := range rawJSONLines {
			var decoded map[string]any
			if err := json.Unmarshal([]byte(line), &decoded); err != nil {
				continue
			}
			if text, err := extractResponsesOutputTextOrEmpty(decoded); err == nil && text != "" {
				content = text
				break
			}
		}
	}
	if content == "" && len(rawJSONLines) > 0 {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(strings.Join(rawJSONLines, "")), &decoded); err == nil {
			content, _ = extractResponsesOutputTextOrEmpty(decoded)
		}
	}
	if content == "" {
		return ProviderResponse{}, fmt.Errorf("%s[%s] responses stream did not include output text", providerName, endpoint.role)
	}

	payload, err := sanitizeJSONPayload(content)
	if err != nil {
		return ProviderResponse{}, fmt.Errorf("%s[%s] responses stream payload: %w", providerName, endpoint.role, err)
	}

	return ProviderResponse{
		Provider:  providerName,
		Model:     firstNonEmpty(endpoint.model, request.Model),
		Output:    payload,
		RawOutput: content,
	}, nil
}

// extractContentFragments 从增量 content 字段中抽取文本片段。
func extractContentFragments(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, item := range typed {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			builder.WriteString(stringValue(entry["text"]))
		}
		return builder.String()
	default:
		return ""
	}
}

// extractResponsesOutputText 提取 responses 输出文本（为空时返回错误）。
func extractResponsesOutputText(payload map[string]any) (string, error) {
	return extractResponsesOutputTextWithOption(payload, true)
}

// extractResponsesOutputTextOrEmpty 提取 responses 输出文本（允许为空）。
func extractResponsesOutputTextOrEmpty(payload map[string]any) (string, error) {
	return extractResponsesOutputTextWithOption(payload, false)
}

// extractResponsesOutputTextWithOption 统一从多种 responses 响应形态提取 output text。
func extractResponsesOutputTextWithOption(payload map[string]any, raiseOnEmpty bool) (string, error) {
	if direct := strings.TrimSpace(stringValue(payload["output_text"])); direct != "" {
		return direct, nil
	}

	candidates := []map[string]any{}
	if wrapped, ok := payload["response"].(map[string]any); ok {
		candidates = append(candidates, wrapped)
	}
	candidates = append(candidates, payload)

	texts := make([]string, 0, 4)
	for _, candidate := range candidates {
		output, ok := candidate["output"].([]any)
		if !ok {
			continue
		}

		for _, item := range output {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}

			itemType := strings.TrimSpace(stringValue(entry["type"]))
			if itemType != "" && itemType != "message" && itemType != "output_text" && itemType != "text" {
				continue
			}

			contentBlocks, hasContentBlocks := entry["content"].([]any)
			if !hasContentBlocks {
				if text := strings.TrimSpace(stringValue(entry["text"])); text != "" {
					texts = append(texts, text)
				}
				continue
			}

			for _, block := range contentBlocks {
				content, ok := block.(map[string]any)
				if !ok {
					continue
				}

				contentType := strings.TrimSpace(stringValue(content["type"]))
				if contentType != "" && contentType != "output_text" && contentType != "text" {
					continue
				}

				if text := strings.TrimSpace(stringValue(content["text"])); text != "" {
					texts = append(texts, text)
				}
			}
		}
	}

	combined := strings.TrimSpace(strings.Join(texts, ""))
	if combined == "" && raiseOnEmpty {
		return "", errors.New("model response did not include output text")
	}

	return combined, nil
}

// extractUsage 从通用 usage 字段中提取 token 统计信息。
func extractUsage(payload map[string]any) Usage {
	usage, _ := payload["usage"].(map[string]any)
	return Usage{
		PromptTokens:     intValue(usage["prompt_tokens"], usage["input_tokens"]),
		CompletionTokens: intValue(usage["completion_tokens"], usage["output_tokens"]),
		TotalTokens:      intValue(usage["total_tokens"]),
	}
}

// stringValue 将任意值安全转换为字符串（非字符串返回空）。
func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return ""
	}
}

// intValue 从候选值列表中解析首个可用整数。
func intValue(values ...any) int {
	for _, value := range values {
		switch typed := value.(type) {
		case float64:
			return int(typed)
		case float32:
			return int(typed)
		case int:
			return typed
		case int64:
			return int(typed)
		case json.Number:
			if parsed, err := typed.Int64(); err == nil {
				return int(parsed)
			}
		case string:
			parsed, err := strconv.Atoi(strings.TrimSpace(typed))
			if err == nil {
				return parsed
			}
		}
	}

	return 0
}

// chatMessage 结构体用于承载该模块的核心数据。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionResponse 结构体用于承载该模块的核心数据。
type chatCompletionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content any `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// extractMessageContent 解析 chat message.content 的字符串或数组形态。
func extractMessageContent(content any) (string, error) {
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		var builder strings.Builder
		for _, item := range value {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}

			text := stringValue(entry["text"])
			if text == "" {
				text = stringValue(entry["content"])
			}
			builder.WriteString(text)
		}

		if builder.Len() == 0 {
			return "", errors.New("content array did not contain text parts")
		}

		return builder.String(), nil
	default:
		return "", fmt.Errorf("unsupported content type %T", content)
	}
}

// sanitizeJSONPayload 把模型原始输出清洗成合法 JSON，并做敏感信息脱敏。
func sanitizeJSONPayload(input string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, errors.New("empty response")
	}

	if strings.HasPrefix(trimmed, "```") {
		trimmed = stripCodeFence(trimmed)
	}

	if json.Valid([]byte(trimmed)) {
		return sanitizeSensitiveJSON(json.RawMessage(trimmed))
	}

	if repairedQuotes := repairUnescapedStringQuotes(trimmed); repairedQuotes != "" && json.Valid([]byte(repairedQuotes)) {
		return sanitizeSensitiveJSON(json.RawMessage(repairedQuotes))
	}

	if candidate := extractJSONCandidate(trimmed); candidate != "" && json.Valid([]byte(candidate)) {
		return sanitizeSensitiveJSON(json.RawMessage(candidate))
	}

	if candidate := extractJSONCandidate(repairUnescapedStringQuotes(trimmed)); candidate != "" && json.Valid([]byte(candidate)) {
		return sanitizeSensitiveJSON(json.RawMessage(candidate))
	}

	if repaired := repairJSONCandidate(trimmed); repaired != "" && json.Valid([]byte(repaired)) {
		return sanitizeSensitiveJSON(json.RawMessage(repaired))
	}

	return nil, fmt.Errorf("provider returned non-json content: %s", trimmed)
}

func repairUnescapedStringQuotes(input string) string {
	text := strings.TrimSpace(stripCodeFence(input))
	if text == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(text) + 8)
	inString := false
	escape := false
	for index := 0; index < len(text); index++ {
		char := text[index]
		if inString {
			if escape {
				builder.WriteByte(char)
				escape = false
				continue
			}
			if char == '\\' {
				builder.WriteByte(char)
				escape = true
				continue
			}
			if char == '"' {
				next := nextNonSpaceByte(text, index+1)
				if next == ':' || next == ',' || next == '}' || next == ']' || next == 0 {
					builder.WriteByte(char)
					inString = false
					continue
				}
				builder.WriteByte('\\')
				builder.WriteByte(char)
				continue
			}
			builder.WriteByte(char)
			continue
		}
		builder.WriteByte(char)
		if char == '"' {
			inString = true
		}
	}
	return builder.String()
}

func nextNonSpaceByte(text string, start int) byte {
	for index := start; index < len(text); index++ {
		switch text[index] {
		case ' ', '\n', '\r', '\t':
			continue
		default:
			return text[index]
		}
	}
	return 0
}

// sanitizeSensitiveJSON 对 JSON 树做递归脱敏后重新编码。
func sanitizeSensitiveJSON(payload json.RawMessage) (json.RawMessage, error) {
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, err
	}
	sanitized := sanitizeSensitiveJSONValue(root)
	encoded, err := json.Marshal(sanitized)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

// sanitizeSensitiveJSONValue 递归遍历 JSON 值并替换敏感字符串。
func sanitizeSensitiveJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			typed[key] = sanitizeSensitiveJSONValue(child)
		}
		return typed
	case []any:
		for index := range typed {
			typed[index] = sanitizeSensitiveJSONValue(typed[index])
		}
		return typed
	case string:
		return redactSensitiveString(typed)
	default:
		return value
	}
}

// redactSensitiveString 按预定义规则替换邮箱、手机号、身份证与敏感短语。
func redactSensitiveString(input string) string {
	output := input
	for _, rule := range sensitiveJSONRedactionRules {
		output = rule.pattern.ReplaceAllString(output, rule.replace)
	}
	return output
}

// stripCodeFence 去掉 Markdown ``` 包裹，提取内部正文。
func stripCodeFence(input string) string {
	lines := strings.Split(input, "\n")
	if len(lines) < 3 {
		return input
	}

	if strings.HasPrefix(lines[0], "```") && strings.HasPrefix(lines[len(lines)-1], "```") {
		return strings.TrimSpace(strings.Join(lines[1:len(lines)-1], "\n"))
	}

	return input
}

// extractJSONCandidate 从自由文本中提取首段平衡的 JSON 片段。
func extractJSONCandidate(content string) string {
	text := stripCodeFence(content)
	start := strings.IndexAny(text, "{[")
	if start < 0 {
		return ""
	}

	depth := 0
	inString := false
	escape := false
	for index := start; index < len(text); index++ {
		char := text[index]
		if inString {
			if escape {
				escape = false
				continue
			}
			if char == '\\' {
				escape = true
				continue
			}
			if char == '"' {
				inString = false
			}
			continue
		}
		if char == '"' {
			inString = true
			continue
		}
		switch char {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return text[start : index+1]
			}
		}
	}

	return ""
}

// repairJSONCandidate 尝试修复缺失闭合符导致的不完整 JSON 文本。
func repairJSONCandidate(content string) string {
	text := strings.TrimSpace(stripCodeFence(content))
	start := strings.IndexAny(text, "{[")
	if start < 0 {
		return ""
	}
	text = text[start:]

	var output strings.Builder
	stack := make([]rune, 0, 8)
	inString := false
	escape := false
	for _, char := range text {
		output.WriteRune(char)
		if inString {
			if escape {
				escape = false
				continue
			}
			if char == '\\' {
				escape = true
				continue
			}
			if char == '"' {
				inString = false
			}
			continue
		}

		switch char {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) == 0 || stack[len(stack)-1] != char {
				return ""
			}
			stack = stack[:len(stack)-1]
		}
	}

	repaired := strings.TrimRight(output.String(), ",:")
	if inString {
		repaired += `"`
	}
	for index := len(stack) - 1; index >= 0; index-- {
		repaired += string(stack[index])
	}
	return strings.TrimSpace(repaired)
}

// safeSchemaName 规范化 schema 名称，避免非法字符影响下游接口。
func safeSchemaName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "qunxiang_response"
	}

	replacer := strings.NewReplacer(" ", "_", "-", "_", "/", "_", ".", "_")
	return replacer.Replace(name)
}

// firstNonEmpty 返回参数列表中第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
