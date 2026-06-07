package session

// 文件说明：决策轨迹旁路表的写入/读取/hydrate（拆 state_json 第一片，沙盘 §11.2）。
// 经多智能体对抗评审加固，三条不变量：
//  1. 持久性：一条轨迹永远活在 {表, blob} 至少之一——Repository.Save **先确认写进表、成功才把它从 blob 摘除**；
//     写表失败则保留在 blob（瘦身放弃，但绝不丢轨迹），下次 load 自愈回填。
//  2. 排序：occurred_at 用**定宽**布局（纳秒补零），字典序=时间序，双驱动一致；hydrate 再按 OccurredAt 升序兜底。
//  3. hydrate 合并：表 ∪ blob（按 id 去重），任何回填失败而残留 blob 的轨迹也并入，绝不被表结果覆盖掉。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
)

// traceTimeLayout 定宽时间布局（纳秒固定 9 位 + 显式时区），保证字符串字典序与时间序一致。
const traceTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTraceTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(traceTimeLayout)
}

type traceExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// persistDecisionTraces 把若干轨迹写入旁路表（按 id 幂等）。任一写入出错即返回错误——
// 供 Repository.Save 判断「是否可安全把轨迹从 blob 摘除」：只有全部确认入表才摘。
func persistDecisionTraces(ctx context.Context, db traceExecer, mysql bool, sessionID string, traces []DecisionTrace) error {
	query := `INSERT INTO decision_traces (id, session_id, unit_id, trace_json, occurred_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`
	if mysql {
		query = `INSERT IGNORE INTO decision_traces (id, session_id, unit_id, trace_json, occurred_at) VALUES (?, ?, ?, ?, ?)`
	}
	for i := range traces {
		tr := traces[i]
		if tr.ID == "" {
			continue
		}
		encoded, err := json.Marshal(tr)
		if err != nil {
			continue
		}
		if _, err := db.ExecContext(ctx, query, tr.ID, sessionID, nullableStr(tr.UnitID), string(encoded), formatTraceTime(tr.OccurredAt)); err != nil {
			return fmt.Errorf("persist decision trace %s: %w", tr.ID, err)
		}
	}
	return nil
}

// shadowDecisionTrace 在 append 时 best-effort 写一条轨迹进表（捕获 >maxDecisionHistory 的全量历史）。
// 它的失败由 Repository.Save 的「确认写表才瘦身」兜底，故这里忽略错误是安全的。
func (service *Service) shadowDecisionTrace(ctx context.Context, sessionID string, trace DecisionTrace) {
	if service == nil || service.db == nil {
		return
	}
	_ = persistDecisionTraces(ctx, service.db, dbdialect.IsMySQL(service.db), sessionID, []DecisionTrace{trace})
}

// ListDecisionTraces 从旁路表按时间倒序读某会话最近的决策轨迹（定宽 occurred_at，字典序即时间序）。
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

// hydrateDecisionTraces 在加载会话后把决策轨迹读源切到旁路表：表 ∪ blob 残留（按 id 去重），
// 按 OccurredAt 升序、截到最近 maxDecisionHistory 条。任何 blob 残留（旧局/回填失败）都并入、绝不丢。
func (service *Service) hydrateDecisionTraces(ctx context.Context, state *State) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	recent, err := service.ListDecisionTraces(ctx, state.ID, maxDecisionHistory)
	if err != nil {
		return // 读表失败：保留 blob 原值（降级但不丢）
	}
	present := make(map[string]bool, len(recent))
	for i := range recent {
		present[recent[i].ID] = true
	}
	// blob 里不在表中的轨迹（现网旧局 / 影子写失败的）：回填进表 + 并入结果。
	residue := make([]DecisionTrace, 0)
	for i := range state.DecisionTraces {
		tr := state.DecisionTraces[i]
		if tr.ID == "" || present[tr.ID] {
			continue
		}
		service.shadowDecisionTrace(ctx, state.ID, tr)
		residue = append(residue, tr)
	}
	if len(recent) == 0 && len(residue) == 0 {
		return // 表空且 blob 无新增 → 保持原值
	}
	merged := make([]DecisionTrace, 0, len(recent)+len(residue))
	merged = append(merged, recent...) // 倒序
	merged = append(merged, residue...)
	sort.SliceStable(merged, func(a, b int) bool { return merged[a].OccurredAt.Before(merged[b].OccurredAt) })
	if len(merged) > maxDecisionHistory {
		merged = merged[len(merged)-maxDecisionHistory:]
	}
	state.DecisionTraces = merged
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
