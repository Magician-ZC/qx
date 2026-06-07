package session

// 文件说明：决策轨迹旁路表的影子双写 + 读取（拆 state_json 第一片，沙盘 §11.2）。
// 安全第一步：决策轨迹 append 时**额外**写一份到 decision_traces 表（best-effort，绝不影响决策循环），
// blob 仍按上限裁剪、仍是权威读源——本步零风险、零行为变更，只是把全量历史也留在旁路表。
// 待验证旁路表完整后，下一步再「load 时从表 hydrate + 不再 marshal 进 blob」，那才动读路径（届时过对抗评审）。

import (
	"context"
	"encoding/json"
	"fmt"

	"qunxiang/backend/internal/storage/dbdialect"
)

// shadowDecisionTrace 把一条决策轨迹影子写入旁路表（按 id 幂等，best-effort 忽略错误）。
func (service *Service) shadowDecisionTrace(ctx context.Context, sessionID string, trace DecisionTrace) {
	if service == nil || service.db == nil || trace.ID == "" {
		return
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		return
	}
	query := `INSERT INTO decision_traces (id, session_id, unit_id, trace_json) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`
	if dbdialect.IsMySQL(service.db) {
		query = `INSERT IGNORE INTO decision_traces (id, session_id, unit_id, trace_json) VALUES (?, ?, ?, ?)`
	}
	_, _ = service.db.ExecContext(ctx, query, trace.ID, sessionID, nullableStr(trace.UnitID), string(encoded))
}

// ListDecisionTraces 从旁路表按时间倒序读某会话的决策轨迹（供审计/未来 hydrate；blob 之外的全量历史）。
func (service *Service) ListDecisionTraces(ctx context.Context, sessionID string, limit int) ([]DecisionTrace, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("list decision traces: missing db")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT trace_json FROM decision_traces WHERE session_id = ?
		ORDER BY occurred_at DESC, id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query decision traces: %w", err)
	}
	defer rows.Close()
	out := make([]DecisionTrace, 0)
	for rows.Next() {
		var traceJSON string
		if err := rows.Scan(&traceJSON); err != nil {
			return nil, fmt.Errorf("scan decision trace: %w", err)
		}
		var trace DecisionTrace
		if err := json.Unmarshal([]byte(traceJSON), &trace); err != nil {
			continue
		}
		out = append(out, trace)
	}
	return out, rows.Err()
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
