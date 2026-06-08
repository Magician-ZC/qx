package session

// 文件说明：运营成本/单位经济仪表盘（只读聚合，产品验证）。跨会话聚合真实 LLM 成本（沿用 estimateLLMCostUSD 同口径，
// 从 llm_interactions 旁路表的 interaction_json 流式解析）+ MAU 代理（窗口内活跃 session 数）+ 单位经济（按生命态计数）。
// MVP 按需解析 JSON（仪表盘是低频 ops 查询，O(N) 可接受）；扁平成本列做 SQL SUM 是规模化后续（登记）。双驱动 ? 占位通用。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ProviderCost 是单个 provider 的成本与调用聚合。
type ProviderCost struct {
	Provider     string  `json:"provider"`
	Calls        int     `json:"calls"`
	CostUSD      float64 `json:"cost_usd"`
	TotalTokens  int     `json:"total_tokens"`
	FallbackHits int     `json:"fallback_hits"`
}

// CostDashboardData 是仪表盘聚合结果（对外只读）。
type CostDashboardData struct {
	SinceDays         int                     `json:"since_days"`
	TotalInteractions int                     `json:"total_interactions"`
	TotalCostUSD      float64                 `json:"total_cost_usd"`
	TotalTokens       int                     `json:"total_tokens"`
	FallbackCount     int                     `json:"fallback_count"`
	FallbackRate      float64                 `json:"fallback_rate"`
	DistinctSessions  int                     `json:"distinct_sessions"` // MAU 代理：窗口内有 LLM 交互的会话数
	CostPerSessionUSD float64                 `json:"cost_per_session_usd"`
	ByProvider        map[string]ProviderCost `json:"by_provider"`
	UnitsTotal        int                     `json:"units_total"`
	UnitsByLifeState  map[string]int          `json:"units_by_life_state"`
	GeneratedAt       string                  `json:"generated_at"`
}

// CostDashboard 聚合最近 sinceDays 天的 LLM 成本与单位经济。sinceDays<=0 视为全量（不设下界）。now 为注入时钟（测试用）。
func (service *Service) CostDashboard(ctx context.Context, sinceDays int, now time.Time) (CostDashboardData, error) {
	data := CostDashboardData{
		SinceDays:        sinceDays,
		ByProvider:       map[string]ProviderCost{},
		UnitsByLifeState: map[string]int{},
		GeneratedAt:      now.UTC().Format(time.RFC3339),
	}
	if service == nil || service.db == nil {
		return data, fmt.Errorf("cost dashboard: missing db")
	}

	// 时间窗口：llm_interactions.occurred_at 由 persistLLMInteractions 用 traceTimeLayout（定宽纳秒 RFC3339）写入，
	// 字典序=时间序。cutoff 必须用**同一布局**才能正确 > 比较（否则 'T' vs ' ' 错位致窗口尾部多算约 1 天，评审 load-bearing）。
	cutoff := ""
	if sinceDays > 0 {
		cutoff = now.UTC().AddDate(0, 0, -sinceDays).Format(traceTimeLayout)
	}

	rows, err := service.queryInteractionRows(ctx, cutoff)
	if err != nil {
		return data, err
	}
	defer rows.Close()
	sessions := map[string]struct{}{}
	for rows.Next() {
		var raw, sessionID string
		if err := rows.Scan(&raw, &sessionID); err != nil {
			return data, fmt.Errorf("cost dashboard scan: %w", err)
		}
		sessions[sessionID] = struct{}{}
		var it LLMInteraction
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			continue // 坏行跳过，不阻断仪表盘
		}
		data.TotalInteractions++
		data.TotalCostUSD += it.EstimatedCost
		data.TotalTokens += it.TotalTokens
		pc := data.ByProvider[it.Provider]
		pc.Provider = it.Provider
		pc.Calls++
		pc.CostUSD += it.EstimatedCost
		pc.TotalTokens += it.TotalTokens
		if it.UsedFallback {
			data.FallbackCount++
			pc.FallbackHits++
		}
		data.ByProvider[it.Provider] = pc
	}
	if err := rows.Err(); err != nil {
		return data, fmt.Errorf("cost dashboard rows: %w", err)
	}
	data.DistinctSessions = len(sessions)
	if data.TotalInteractions > 0 {
		data.FallbackRate = float64(data.FallbackCount) / float64(data.TotalInteractions)
	}
	if data.DistinctSessions > 0 {
		data.CostPerSessionUSD = data.TotalCostUSD / float64(data.DistinctSessions)
	}

	// 单位经济（MVP：按生命态计数；钱包/产出 SUM 需扁平列，登记后续）。
	if err := service.aggregateUnitsByLifeState(ctx, &data); err != nil {
		return data, err
	}
	return data, nil
}

func (service *Service) queryInteractionRows(ctx context.Context, cutoff string) (*sql.Rows, error) {
	if cutoff == "" {
		return service.db.QueryContext(ctx, `SELECT interaction_json, session_id FROM llm_interactions`)
	}
	return service.db.QueryContext(ctx, `SELECT interaction_json, session_id FROM llm_interactions WHERE occurred_at > ?`, cutoff)
}

func (service *Service) aggregateUnitsByLifeState(ctx context.Context, data *CostDashboardData) error {
	rows, err := service.db.QueryContext(ctx, `SELECT life_state, COUNT(*) FROM units GROUP BY life_state`)
	if err != nil {
		return fmt.Errorf("cost dashboard units: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var lifeState string
		var n int
		if err := rows.Scan(&lifeState, &n); err != nil {
			return fmt.Errorf("cost dashboard units scan: %w", err)
		}
		if lifeState == "" {
			lifeState = "unknown"
		}
		data.UnitsByLifeState[lifeState] = n
		data.UnitsTotal += n
	}
	return rows.Err()
}
