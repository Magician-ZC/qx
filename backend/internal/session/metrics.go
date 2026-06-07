package session

// 文件说明：metrics.go，会话指标与成本统计工具，负责 token/费用估算与关键交互计数。

import (
	"fmt"
	"math"
	"strings"

	"qunxiang/backend/internal/unit"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	openAIPromptPerMillionUSD   = 0.35
	openAIOutputPerMillionUSD   = 1.20
	deepSeekPromptPerMillionUSD = 0.14
	deepSeekOutputPerMillionUSD = 0.28
	defaultPromptPerMillionUSD  = 0.30
	defaultOutputPerMillionUSD  = 0.90
)

// normalizeTokenUsage 规范化 token 统计字段，修正负值与不一致总量。
func normalizeTokenUsage(promptTokens int, outputTokens int, totalTokens int) (int, int, int) {
	if promptTokens < 0 {
		promptTokens = 0
	}
	if outputTokens < 0 {
		outputTokens = 0
	}
	if totalTokens <= 0 {
		totalTokens = promptTokens + outputTokens
	}
	if totalTokens < promptTokens+outputTokens {
		totalTokens = promptTokens + outputTokens
	}
	return promptTokens, outputTokens, totalTokens
}

// EstimateLLMCostUSD 是 estimateLLMCostUSD 的导出包装，供成本基准工具(cmd/costbench)复用同一套单价表。
func EstimateLLMCostUSD(provider string, model string, promptTokens int, outputTokens int) float64 {
	return estimateLLMCostUSD(provider, model, promptTokens, outputTokens)
}

// estimateLLMCostUSD 按 provider 单价估算一次调用成本（USD）。
func estimateLLMCostUSD(provider string, model string, promptTokens int, outputTokens int) float64 {
	provider = strings.ToLower(strings.TrimSpace(provider))
	model = strings.ToLower(strings.TrimSpace(model))
	if provider == "" {
		provider = "unknown"
	}
	if provider == "rules" || provider == "reaction_queue" || model == "fallback" {
		return 0
	}
	if promptTokens <= 0 && outputTokens <= 0 {
		return 0
	}

	promptRate := defaultPromptPerMillionUSD
	outputRate := defaultOutputPerMillionUSD
	switch provider {
	case "openai":
		promptRate = openAIPromptPerMillionUSD
		outputRate = openAIOutputPerMillionUSD
	case "deepseek":
		promptRate = deepSeekPromptPerMillionUSD
		outputRate = deepSeekOutputPerMillionUSD
	}

	cost := (float64(promptTokens)/1_000_000.0)*promptRate +
		(float64(outputTokens)/1_000_000.0)*outputRate
	return roundUSD(cost)
}

// roundUSD 把成本值裁到微美元精度。
func roundUSD(value float64) float64 {
	if value <= 0 {
		return 0
	}
	return math.Round(value*1_000_000) / 1_000_000
}

// accumulateLLMMetrics 累加会话级 LLM token 与成本指标。
func accumulateLLMMetrics(state *State, interaction LLMInteraction) {
	if state == nil {
		return
	}
	promptTokens, outputTokens, totalTokens := normalizeTokenUsage(
		interaction.PromptTokens,
		interaction.OutputTokens,
		interaction.TotalTokens,
	)
	state.Metrics.LLMPromptTokens += promptTokens
	state.Metrics.LLMOutputTokens += outputTokens
	state.Metrics.LLMTotalTokens += totalTokens
	state.Metrics.LLMEstimatedCostUSD = roundUSD(state.Metrics.LLMEstimatedCostUSD + interaction.EstimatedCost)
}

// incrementCrossFactionInteraction 在已知单位对象下统计跨阵营互动。
func incrementCrossFactionInteraction(
	state *State,
	kind string,
	actor *unit.Record,
	target *unit.Record,
) {
	if actor == nil || target == nil {
		return
	}
	incrementCrossFactionInteractionByFaction(
		state,
		kind,
		actor.FactionID,
		target.FactionID,
		actor.ID,
		target.ID,
	)
}

// incrementCrossFactionInteractionByFaction 按阵营信息统计跨阵营互动并写 raw event。
func incrementCrossFactionInteractionByFaction(
	state *State,
	kind string,
	leftFactionID string,
	rightFactionID string,
	actorUnitID string,
	targetUnitID string,
) {
	if state == nil {
		return
	}
	if !isPlayerEnemyFactionPair(*state, leftFactionID, rightFactionID) {
		return
	}
	if strings.TrimSpace(kind) == "" {
		kind = "unknown"
	}
	state.Metrics.CrossFactionInteractions++
	appendRawEvent(state, rawEventSpec{
		source:       "metric",
		kind:         "cross_faction_interaction",
		summary:      fmt.Sprintf("%s #%d", kind, state.Metrics.CrossFactionInteractions),
		actorUnitID:  strings.TrimSpace(actorUnitID),
		targetUnitID: strings.TrimSpace(targetUnitID),
		payload: map[string]any{
			"kind":                    kind,
			"count":                   state.Metrics.CrossFactionInteractions,
			"left_faction_id":         leftFactionID,
			"right_faction_id":        rightFactionID,
			"actor_unit_id":           strings.TrimSpace(actorUnitID),
			"target_unit_id":          strings.TrimSpace(targetUnitID),
			"llm_estimated_cost_usd":  state.Metrics.LLMEstimatedCostUSD,
			"llm_total_tokens_so_far": state.Metrics.LLMTotalTokens,
		},
	})
}
