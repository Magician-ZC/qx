package ai

// 文件说明：定义 AI 子系统通用类型与任务配置，包括 provider、task profile、请求结果与批处理结构。

import (
	"context"
	"encoding/json"
	"time"
)

// ProviderName 类型定义用于统一该模块的数据表达。
type ProviderName string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	ProviderDeepSeek       ProviderName = "deepseek"
	ProviderOpenAI         ProviderName = "openai"
	ProviderOpenAIFallback ProviderName = "openai_fallback"
)

// TaskKind 类型定义用于统一该模块的数据表达。
type TaskKind string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	TaskIntentParse  TaskKind = "intent_parse"
	TaskUnitDecision TaskKind = "unit_decision"
	TaskDialogue     TaskKind = "dialogue"
	TaskReflection   TaskKind = "reflection"
	TaskDeployment   TaskKind = "deployment"
	TaskUpkeep       TaskKind = "upkeep"
	TaskBackstory    TaskKind = "backstory"
	TaskBattleReport TaskKind = "battle_report"
	TaskDowntime     TaskKind = "downtime"
	TaskStrategy     TaskKind = "strategy"
)

// TaskProfile 结构体用于承载该模块的核心数据。
type TaskProfile struct {
	Primary   ProviderName
	Secondary ProviderName
	Tertiary  ProviderName
	Timeout   time.Duration
}

// DefaultTaskProfiles 返回默认任务路由配置（统一 60 秒 LLM 超时）。
func DefaultTaskProfiles() map[TaskKind]TaskProfile {
	return ConfiguredTaskProfiles(60 * time.Second)
}

// ConfiguredTaskProfiles 基于基础超时生成各任务的主备 provider 与超时策略。
func ConfiguredTaskProfiles(baseTimeout time.Duration) map[TaskKind]TaskProfile {
	baseTimeout = normalizeBaseTimeout(baseTimeout)
	return map[TaskKind]TaskProfile{
		TaskIntentParse: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskUnitDecision: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskDialogue: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskReflection: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskDeployment: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskUpkeep: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskBackstory: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskBattleReport: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskDowntime: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
		TaskStrategy: {
			Primary:   ProviderOpenAI,
			Secondary: ProviderDeepSeek,
			Tertiary:  ProviderOpenAIFallback,
			Timeout:   baseTimeout,
		},
	}
}

// normalizeBaseTimeout 规范化基础超时，保证不低于系统最小值。
func normalizeBaseTimeout(timeout time.Duration) time.Duration {
	if timeout <= 0 {
		return 180 * time.Second
	}
	if timeout < 60*time.Second {
		return 60 * time.Second
	}
	if timeout > 180*time.Second {
		return 180 * time.Second
	}
	return timeout
}

// CompletionRequest 结构体用于承载该模块的核心数据。
type CompletionRequest struct {
	Task           TaskKind
	SystemPrompt   string
	UserPrompt     string
	SchemaName     string
	ResponseSchema []byte
	Temperature    float64
	MaxTokens      int
	Timeout        time.Duration
	Metadata       map[string]string
	Fallback       RuleFallback
	Cacheable      bool // 是否可进 prompt 缓存（高频重复情境的决策设 true；§11.2 降本，缓存全程 flag-gated 见 prompt_cache.go）
	// Importance 标注**任务重要度档**（非付费档——付费不得买更强 LLM，违反反 P2W 宪法）。
	// 取值：""=默认（不分档，走原 profile）/ "critical"（关键节点，可路由到更强/更长超时档）/ "cheap"（日常低权，可路由到便宜/短超时档）。
	// 仅当 QUNXIANG_TIER_ROUTING 开启且对应档配了 model/timeout 时才生效；否则该字段被完全忽略（零行为差异）。
	Importance string `json:"importance,omitempty"`
}

// 任务重要度档常量（CompletionRequest.Importance 取值；空串=不分档）。
const (
	ImportanceCritical = "critical" // 关键节点：命运抉择、首领遭遇、阵营战略等少数高价值决策
	ImportanceCheap    = "cheap"    // 日常低权：闲聊、待命、常规巡逻等可走便宜档的高频决策
)

// CompletionResult 结构体用于承载该模块的核心数据。
type CompletionResult struct {
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Output       json.RawMessage `json:"output"`
	UsedFallback bool            `json:"used_fallback"`
	Usage        Usage           `json:"usage"`
	Debug        CompletionDebug `json:"debug"`
	// CacheHit 标记该结果取自 prompt 缓存复用、无真实 LLM 花费；保留原始 Usage/Provider 供遥测，
	// 下游 buildLLMInteraction 据此把 EstimatedCost 计 0，避免幻影成本误触会话预算护栏与成本仪表盘。
	CacheHit bool `json:"cache_hit,omitempty"`
}

// BatchRequest 结构体用于承载该模块的核心数据。
// 注意：批处理的可缓存性（Cacheable）经内嵌的 Request.Cacheable 透传——batch.go 把整条 Request 原样下发给
// GenerateJSON，故无需在此重复字段；Cacheable=true 的批条目命中 prompt 缓存时同样跳过真实 LLM 调用。
type BatchRequest struct {
	Key     string
	Request CompletionRequest
}

// BatchResult 结构体用于承载该模块的核心数据。
type BatchResult struct {
	Key     string
	Request CompletionRequest
	Result  CompletionResult
	Err     error
	Cached  bool
}

// BatchOptions 结构体用于承载该模块的核心数据。
type BatchOptions struct {
	MaxConcurrency int
	OnComplete     func(BatchResult)
}

// Usage 结构体用于承载该模块的核心数据。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CompletionAttempt 结构体用于承载该模块的核心数据。
type CompletionAttempt struct {
	Provider   string `json:"provider"`
	Endpoint   string `json:"endpoint"`
	BaseURL    string `json:"base_url,omitempty"`
	WireAPI    string `json:"wire_api,omitempty"`
	Model      string `json:"model,omitempty"`
	StartedAt  string `json:"started_at,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	Succeeded  bool   `json:"succeeded"`
	Error      string `json:"error,omitempty"`
}

// CompletionDebug 结构体用于承载该模块的核心数据。
type CompletionDebug struct {
	Attempts      []CompletionAttempt `json:"attempts,omitempty"`
	RawOutput     string              `json:"raw_output,omitempty"`
	FallbackCause string              `json:"fallback_cause,omitempty"`
}

// ActiveLLMCall 表示仍在执行中的一次 LLM 调用，用于调试面板展示未完成请求。
type ActiveLLMCall struct {
	ID           string            `json:"id"`
	Task         TaskKind          `json:"task"`
	SchemaName   string            `json:"schema_name,omitempty"`
	Summary      string            `json:"summary"`
	SystemPrompt string            `json:"system_prompt"`
	UserPrompt   string            `json:"user_prompt"`
	Provider     string            `json:"provider,omitempty"`
	Model        string            `json:"model,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	StartedAt    time.Time         `json:"started_at"`
	ElapsedMS    int64             `json:"elapsed_ms"`
}

// ProviderRequest 结构体用于承载该模块的核心数据。
type ProviderRequest struct {
	Task            TaskKind
	Model           string
	SystemPrompt    string
	UserPrompt      string
	SchemaName      string
	ResponseSchema  []byte
	Temperature     float64
	MaxTokens       int
	Timeout         time.Duration
	ReasoningEffort string // 请求级 reasoning effort（GM 全局热切覆盖端点构造期配置；空=不覆盖、沿用端点配置）
	Metadata        map[string]string
}

// ProviderResponse 结构体用于承载该模块的核心数据。
type ProviderResponse struct {
	Provider  string
	Model     string
	Output    json.RawMessage
	Usage     Usage
	RawOutput string
	Attempts  []CompletionAttempt
}

// RuleFallback 接口定义该模块需要实现的能力约束。
type RuleFallback interface {
	Fallback(context.Context, CompletionRequest, error) (json.RawMessage, error)
}

// RuleFallbackFunc 类型定义用于统一该模块的数据表达。
type RuleFallbackFunc func(context.Context, CompletionRequest, error) (json.RawMessage, error)

// Fallback 让函数类型 RuleFallbackFunc 满足 RuleFallback 接口。
func (f RuleFallbackFunc) Fallback(
	ctx context.Context,
	request CompletionRequest,
	cause error,
) (json.RawMessage, error) {
	return f(ctx, request, cause)
}

// ProviderStatus 结构体用于承载该模块的核心数据。
type ProviderStatus struct {
	Name                 ProviderName `json:"name"`
	Available            bool         `json:"available"`
	DefaultModel         string       `json:"default_model"`
	BaseURL              string       `json:"base_url"`
	StructuredOutputMode string       `json:"structured_output_mode"`
}

// Provider 接口定义该模块需要实现的能力约束。
type Provider interface {
	Name() ProviderName
	Available() bool
	Status() ProviderStatus
	GenerateJSON(context.Context, ProviderRequest) (ProviderResponse, error)
}
