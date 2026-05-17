package config

// 文件说明：服务配置加载模块，负责环境变量解析、默认值回退与多端点 LLM 配置组装。

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultConfigFile = "config.local.json"

var (
	loadConfigOnce sync.Once
	loadEnvOnce    sync.Once
)

// LLMEndpoint 结构体用于承载该模块的核心数据。
type LLMEndpoint struct {
	BaseURL         string
	WireAPI         string
	APIKey          string
	Model           string
	ReasoningEffort string
}

// Config 结构体用于承载该模块的核心数据。
type Config struct {
	HTTPAddr                string
	DBDriver                string
	SQLitePath              string
	MySQLDSN                string
	PostgresDSN             string
	AuthTokenTTL            time.Duration
	ReadTimeout             time.Duration
	WriteTimeout            time.Duration
	LLMTimeout              time.Duration
	DeepSeekBaseURL         string
	DeepSeekWireAPI         string
	DeepSeekAPIKey          string
	DeepSeekModel           string
	DeepSeekReasoningEffort string
	OpenAIBaseURL           string
	OpenAIWireAPI           string
	OpenAIAPIKey            string
	OpenAIModel             string
	OpenAIReasoningEffort   string
	OpenAIFallbacks         []LLMEndpoint
}

// Load 从环境变量加载服务配置，并应用默认值与安全边界。
func Load() Config {
	loadExternalConfigFile()
	loadExternalEnvFile()

	openAIBaseURL := getAnyEnv([]string{"QUNXIANG_OPENAI_BASE_URL", "OPENAI_BASE_URL"}, "https://api.openai.com/v1")
	openAIWireAPI := getAnyEnv([]string{"QUNXIANG_OPENAI_WIRE_API", "OPENAI_WIRE_API"}, "chat_completions")
	openAIAPIKey := getAnyEnv([]string{"QUNXIANG_OPENAI_API_KEY", "OPENAI_API_KEY"}, "")
	openAIModel := getAnyEnv([]string{"QUNXIANG_OPENAI_MODEL", "OPENAI_MODEL"}, "gpt-4o-mini")
	openAIReasoningEffort := getAnyEnv([]string{"QUNXIANG_OPENAI_REASONING_EFFORT", "OPENAI_REASONING_EFFORT"}, "")
	if isOpenRouterBaseURL(openAIBaseURL) {
		openAIAPIKey = mergeKeyLists(openAIAPIKey, getAnyEnv([]string{"QUNXIANG_OPENROUTER_API_KEYS", "OPENROUTER_API_KEYS"}, ""))
	}
	llmTimeout := parseDurationSeconds(
		getAnyEnv([]string{"QUNXIANG_OPENAI_TIMEOUT_SECONDS", "OPENAI_TIMEOUT_SECONDS"}, "180"),
		180*time.Second,
	)

	return Config{
		HTTPAddr:                getAnyEnv([]string{"QUNXIANG_HTTP_ADDR"}, ":8080"),
		DBDriver:                strings.TrimSpace(getAnyEnv([]string{"QUNXIANG_DB_DRIVER"}, "sqlite")),
		SQLitePath:              getAnyEnv([]string{"QUNXIANG_SQLITE_PATH"}, "data/session.db"),
		MySQLDSN:                strings.TrimSpace(getAnyEnv([]string{"QUNXIANG_MYSQL_DSN", "MYSQL_DSN"}, "")),
		PostgresDSN:             strings.TrimSpace(getAnyEnv([]string{"QUNXIANG_POSTGRES_DSN", "POSTGRES_DSN"}, "")),
		AuthTokenTTL:            parseDurationHours(getAnyEnv([]string{"QUNXIANG_AUTH_TOKEN_TTL_HOURS"}, "72"), 72*time.Hour),
		ReadTimeout:             parseServerDurationSeconds(getAnyEnv([]string{"QUNXIANG_READ_TIMEOUT_SECONDS"}, "10"), 10*time.Second),
		WriteTimeout:            parseServerDurationSeconds(getAnyEnv([]string{"QUNXIANG_WRITE_TIMEOUT_SECONDS"}, "180"), 180*time.Second),
		LLMTimeout:              llmTimeout,
		DeepSeekBaseURL:         getAnyEnv([]string{"QUNXIANG_DEEPSEEK_BASE_URL", "OPENAI_BASE_URL"}, openAIBaseURL),
		DeepSeekWireAPI:         getAnyEnv([]string{"QUNXIANG_DEEPSEEK_WIRE_API", "OPENAI_WIRE_API"}, openAIWireAPI),
		DeepSeekAPIKey:          getAnyEnv([]string{"QUNXIANG_DEEPSEEK_API_KEY", "OPENAI_API_KEY"}, openAIAPIKey),
		DeepSeekModel:           getAnyEnv([]string{"QUNXIANG_DEEPSEEK_MODEL", "OPENAI_MODEL"}, openAIModel),
		DeepSeekReasoningEffort: getAnyEnv([]string{"QUNXIANG_DEEPSEEK_REASONING_EFFORT", "OPENAI_REASONING_EFFORT"}, openAIReasoningEffort),
		OpenAIBaseURL:           openAIBaseURL,
		OpenAIWireAPI:           openAIWireAPI,
		OpenAIAPIKey:            openAIAPIKey,
		OpenAIModel:             openAIModel,
		OpenAIReasoningEffort:   openAIReasoningEffort,
		OpenAIFallbacks:         collectOpenAIFallbacks(),
	}
}

type localConfigFile struct {
	HTTPAddr          string                `json:"http_addr"`
	DBDriver          string                `json:"db_driver"`
	SQLitePath        string                `json:"sqlite_path"`
	MySQLDSN          string                `json:"mysql_dsn"`
	PostgresDSN       string                `json:"postgres_dsn"`
	AuthTokenTTLHours string                `json:"auth_token_ttl_hours"`
	ReadTimeoutSecs   string                `json:"read_timeout_seconds"`
	WriteTimeoutSecs  string                `json:"write_timeout_seconds"`
	OpenRouterAPIKeys string                `json:"openrouter_api_keys"`
	OpenAI            localEndpointConfig   `json:"openai"`
	DeepSeek          localEndpointConfig   `json:"deepseek"`
	OpenAIFallbacks   []localEndpointConfig `json:"openai_fallbacks"`
	Env               map[string]string     `json:"env"`
}

type localEndpointConfig struct {
	BaseURL         string `json:"base_url"`
	WireAPI         string `json:"wire_api"`
	APIKey          string `json:"api_key"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
	TimeoutSeconds  string `json:"timeout_seconds"`
}

// loadExternalConfigFile 从本地 JSON 配置补充环境变量（仅在缺失时注入）。
func loadExternalConfigFile() {
	loadConfigOnce.Do(func() {
		path := strings.TrimSpace(getAnyEnv([]string{"QUNXIANG_CONFIG_FILE"}, defaultConfigFile))
		if path == "" {
			return
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return
		}

		var file localConfigFile
		if err := json.Unmarshal(data, &file); err != nil {
			return
		}

		setEnvIfMissing("QUNXIANG_HTTP_ADDR", file.HTTPAddr)
		setEnvIfMissing("QUNXIANG_DB_DRIVER", file.DBDriver)
		setEnvIfMissing("QUNXIANG_SQLITE_PATH", file.SQLitePath)
		setEnvIfMissing("QUNXIANG_MYSQL_DSN", file.MySQLDSN)
		setEnvIfMissing("QUNXIANG_POSTGRES_DSN", file.PostgresDSN)
		setEnvIfMissing("QUNXIANG_AUTH_TOKEN_TTL_HOURS", file.AuthTokenTTLHours)
		setEnvIfMissing("QUNXIANG_READ_TIMEOUT_SECONDS", file.ReadTimeoutSecs)
		setEnvIfMissing("QUNXIANG_WRITE_TIMEOUT_SECONDS", file.WriteTimeoutSecs)
		setEnvIfMissing("QUNXIANG_OPENROUTER_API_KEYS", file.OpenRouterAPIKeys)

		applyLocalEndpoint("QUNXIANG_OPENAI", file.OpenAI)
		if strings.TrimSpace(file.OpenAI.TimeoutSeconds) != "" {
			setEnvIfMissing("QUNXIANG_OPENAI_TIMEOUT_SECONDS", file.OpenAI.TimeoutSeconds)
		}
		applyLocalEndpoint("QUNXIANG_DEEPSEEK", file.DeepSeek)

		fallbackPrefixes := []string{
			"QUNXIANG_OPENAI_FALLBACK",
			"QUNXIANG_OPENAI_SECOND_FALLBACK",
			"QUNXIANG_OPENAI_THIRD_FALLBACK",
			"QUNXIANG_OPENAI_FOURTH_FALLBACK",
			"QUNXIANG_OPENAI_FIFTH_FALLBACK",
			"QUNXIANG_OPENAI_SIXTH_FALLBACK",
			"QUNXIANG_OPENAI_SEVENTH_FALLBACK",
			"QUNXIANG_OPENAI_EIGHTH_FALLBACK",
		}
		for index, endpoint := range file.OpenAIFallbacks {
			if index >= len(fallbackPrefixes) {
				break
			}
			applyLocalEndpoint(fallbackPrefixes[index], endpoint)
		}

		for key, value := range file.Env {
			setEnvIfMissing(key, value)
		}
	})
}

func applyLocalEndpoint(prefix string, endpoint localEndpointConfig) {
	setEnvIfMissing(prefix+"_BASE_URL", endpoint.BaseURL)
	setEnvIfMissing(prefix+"_WIRE_API", endpoint.WireAPI)
	setEnvIfMissing(prefix+"_API_KEY", endpoint.APIKey)
	setEnvIfMissing(prefix+"_MODEL", endpoint.Model)
	setEnvIfMissing(prefix+"_REASONING_EFFORT", endpoint.ReasoningEffort)
}

func setEnvIfMissing(key string, value string) {
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if key == "" || value == "" {
		return
	}
	if _, exists := os.LookupEnv(key); !exists {
		_ = os.Setenv(key, value)
	}
}

// loadExternalEnvFile 从显式指定的外部 .env 文件补充环境变量（仅在缺失时注入）。
func loadExternalEnvFile() {
	loadEnvOnce.Do(func() {
		path := strings.TrimSpace(getAnyEnv([]string{"QUNXIANG_LLM_ENV_FILE"}, ""))
		if path == "" {
			return
		}
		file, err := os.Open(path)
		if err != nil {
			return
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}

			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}

			key = strings.TrimSpace(key)
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			if key == "" || value == "" {
				continue
			}

			setEnvIfMissing(key, value)
		}
	})
}

// getAnyEnv 按优先顺序读取多个环境变量键，全部缺失时返回回退值。
func getAnyEnv(keys []string, fallback string) string {
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok && value != "" {
			return value
		}
	}

	return fallback
}

// parseDurationSeconds 解析秒级时长，并限制在 LLM 超时允许区间内。
func parseDurationSeconds(raw string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(raw) + "s")
	if err != nil || value <= 0 {
		return fallback
	}
	if value < 60*time.Second {
		return 60 * time.Second
	}
	if value > 180*time.Second {
		return 180 * time.Second
	}
	return value
}

// parseServerDurationSeconds 解析服务端读写超时时长（秒）。
func parseServerDurationSeconds(raw string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(raw) + "s")
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// parseDurationHours 解析小时级时长（用于 token TTL 等配置）。
func parseDurationHours(raw string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(strings.TrimSpace(raw) + "h")
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// collectOpenAIFallbacks 收集多级 OpenAI 兼容后备端点配置。
func collectOpenAIFallbacks() []LLMEndpoint {
	descriptors := []struct {
		baseURLKeys         []string
		wireAPIKeys         []string
		apiKeyKeys          []string
		modelKeys           []string
		reasoningEffortKeys []string
		defaultWireAPI      string
	}{
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_FALLBACK_BASE_URL", "OPENAI_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_FALLBACK_WIRE_API", "OPENAI_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_FALLBACK_API_KEY", "OPENAI_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_FALLBACK_MODEL", "OPENAI_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_FALLBACK_REASONING_EFFORT", "OPENAI_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_SECOND_FALLBACK_BASE_URL", "OPENAI_SECOND_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_SECOND_FALLBACK_WIRE_API", "OPENAI_SECOND_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_SECOND_FALLBACK_API_KEY", "OPENAI_SECOND_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_SECOND_FALLBACK_MODEL", "OPENAI_SECOND_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_SECOND_FALLBACK_REASONING_EFFORT", "OPENAI_SECOND_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_THIRD_FALLBACK_BASE_URL", "OPENAI_THIRD_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_THIRD_FALLBACK_WIRE_API", "OPENAI_THIRD_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_THIRD_FALLBACK_API_KEY", "OPENAI_THIRD_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_THIRD_FALLBACK_MODEL", "OPENAI_THIRD_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_THIRD_FALLBACK_REASONING_EFFORT", "OPENAI_THIRD_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_FOURTH_FALLBACK_BASE_URL", "OPENAI_FOURTH_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_FOURTH_FALLBACK_WIRE_API", "OPENAI_FOURTH_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_FOURTH_FALLBACK_API_KEY", "OPENAI_FOURTH_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_FOURTH_FALLBACK_MODEL", "OPENAI_FOURTH_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_FOURTH_FALLBACK_REASONING_EFFORT", "OPENAI_FOURTH_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_FIFTH_FALLBACK_BASE_URL", "OPENAI_FIFTH_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_FIFTH_FALLBACK_WIRE_API", "OPENAI_FIFTH_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_FIFTH_FALLBACK_API_KEY", "OPENAI_FIFTH_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_FIFTH_FALLBACK_MODEL", "OPENAI_FIFTH_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_FIFTH_FALLBACK_REASONING_EFFORT", "OPENAI_FIFTH_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_SIXTH_FALLBACK_BASE_URL", "OPENAI_SIXTH_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_SIXTH_FALLBACK_WIRE_API", "OPENAI_SIXTH_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_SIXTH_FALLBACK_API_KEY", "OPENAI_SIXTH_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_SIXTH_FALLBACK_MODEL", "OPENAI_SIXTH_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_SIXTH_FALLBACK_REASONING_EFFORT", "OPENAI_SIXTH_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_SEVENTH_FALLBACK_BASE_URL", "OPENAI_SEVENTH_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_SEVENTH_FALLBACK_WIRE_API", "OPENAI_SEVENTH_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_SEVENTH_FALLBACK_API_KEY", "OPENAI_SEVENTH_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_SEVENTH_FALLBACK_MODEL", "OPENAI_SEVENTH_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_SEVENTH_FALLBACK_REASONING_EFFORT", "OPENAI_SEVENTH_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
		{
			baseURLKeys:         []string{"QUNXIANG_OPENAI_EIGHTH_FALLBACK_BASE_URL", "OPENAI_EIGHTH_FALLBACK_BASE_URL"},
			wireAPIKeys:         []string{"QUNXIANG_OPENAI_EIGHTH_FALLBACK_WIRE_API", "OPENAI_EIGHTH_FALLBACK_WIRE_API"},
			apiKeyKeys:          []string{"QUNXIANG_OPENAI_EIGHTH_FALLBACK_API_KEY", "OPENAI_EIGHTH_FALLBACK_API_KEY"},
			modelKeys:           []string{"QUNXIANG_OPENAI_EIGHTH_FALLBACK_MODEL", "OPENAI_EIGHTH_FALLBACK_MODEL"},
			reasoningEffortKeys: []string{"QUNXIANG_OPENAI_EIGHTH_FALLBACK_REASONING_EFFORT", "OPENAI_EIGHTH_FALLBACK_REASONING_EFFORT"},
			defaultWireAPI:      "chat_completions",
		},
	}

	fallbacks := make([]LLMEndpoint, 0, len(descriptors))
	seen := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		endpoint := LLMEndpoint{
			BaseURL:         strings.TrimSpace(getAnyEnv(descriptor.baseURLKeys, "")),
			WireAPI:         strings.TrimSpace(getAnyEnv(descriptor.wireAPIKeys, descriptor.defaultWireAPI)),
			APIKey:          strings.TrimSpace(getAnyEnv(descriptor.apiKeyKeys, "")),
			Model:           strings.TrimSpace(getAnyEnv(descriptor.modelKeys, "")),
			ReasoningEffort: strings.TrimSpace(getAnyEnv(descriptor.reasoningEffortKeys, "")),
		}
		if isOpenRouterBaseURL(endpoint.BaseURL) {
			endpoint.APIKey = mergeKeyLists(endpoint.APIKey, getAnyEnv([]string{"QUNXIANG_OPENROUTER_API_KEYS", "OPENROUTER_API_KEYS"}, ""))
		}
		if endpoint.BaseURL == "" || endpoint.APIKey == "" || endpoint.Model == "" {
			continue
		}

		signature := strings.Join([]string{
			endpoint.BaseURL,
			endpoint.WireAPI,
			endpoint.APIKey,
			endpoint.Model,
		}, "\x00")
		if _, exists := seen[signature]; exists {
			continue
		}

		seen[signature] = struct{}{}
		fallbacks = append(fallbacks, endpoint)
	}

	return fallbacks
}

func isOpenRouterBaseURL(baseURL string) bool {
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") == "https://openrouter.ai/api/v1"
}

func mergeKeyLists(primary string, extra string) string {
	keys := splitConfigKeyList(primary)
	keys = append(keys, splitConfigKeyList(extra)...)
	if len(keys) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(keys))
	merged := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		merged = append(merged, key)
	}
	return strings.Join(merged, ",")
}

func splitConfigKeyList(raw string) []string {
	normalized := strings.NewReplacer("\n", ",", "\r", ",", ";", ",").Replace(raw)
	parts := strings.Split(normalized, ",")
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		key := strings.TrimSpace(part)
		if key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}
