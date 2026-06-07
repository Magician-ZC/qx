package session

// 文件说明：原始事件日志旁路表的写入/读取/hydrate（拆 state_json 第三片，沙盘 §11.2，读路径已 cutover）。
// 与 LLM 交互同一套不变量纪律，但更简单：RawEventEntry 的 PayloadJSON 在 appendRawEvent 落库时即已限/裁
// （llm/decision 源清空、其余截 1200 runes）、数组在 append 时即裁到 maxRawEventHistory，无 LLM 那样「压缩前后
// 字段不同、须在压缩前捕获完整 prompt」的问题——故 Repository.Save 在 compactStateForStorage **之后**持久化压缩态
// （limitTextRunes 边界空格非幂等，post-compact 持久化才能让表与 blob 逐字节一致），hydrate 也只需 cap、无需字段压缩。
//
// 四条不变量：
//  1. 持久性：一条事件永活于 {表, blob} 至少之一——Save 先确认写进表、成功才从 blob 摘除；写表失败保留 blob，
//     下次 load 由 hydrate 合并 blob 残留并回填表自愈。
//  2. 排序：occurred_at 复用 traceTimeLayout 定宽布局，字典序=时间序，双驱动一致；hydrate 再按 OccurredAt 升序兜底。
//  3. hydrate 合并：表 ∪ blob 残留（按 id 去重），任何回填失败而残留 blob 的事件也并入、绝不被表结果覆盖掉。
//  4. 零行为变化：hydrate 后 cap 到 maxRawEventHistory，与切表前工作集一致。跳过空 ID。
//
// 隐私红线：审计链路擦除（EraseAuditTrail）/ 保留期清理必须同步清本表（见 privacy.go），否则即审计残留漏洞。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"qunxiang/backend/internal/storage/dbdialect"
)

// persistRawEvents 把若干原始事件写入旁路表（按 id 幂等）。跳过空 ID。
func persistRawEvents(ctx context.Context, db traceExecer, mysql bool, sessionID string, events []RawEventEntry) error {
	query := `INSERT INTO raw_event_log (id, session_id, unit_id, event_json, occurred_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`
	if mysql {
		query = `INSERT IGNORE INTO raw_event_log (id, session_id, unit_id, event_json, occurred_at) VALUES (?, ?, ?, ?, ?)`
	}
	for i := range events {
		ev := events[i]
		if ev.ID == "" {
			continue
		}
		encoded, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		if _, err := db.ExecContext(ctx, query, ev.ID, sessionID, nullableStr(ev.ActorUnitID), string(encoded), formatTraceTime(ev.OccurredAt)); err != nil {
			return fmt.Errorf("persist raw event %s: %w", ev.ID, err)
		}
	}
	return nil
}

// ListRawEvents 从旁路表按时间倒序读某会话最近的原始事件（定宽 occurred_at，字典序即时间序）。
func (service *Service) ListRawEvents(ctx context.Context, sessionID string, limit int) ([]RawEventEntry, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("list raw events: missing db")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT event_json FROM raw_event_log WHERE session_id = ?
		ORDER BY occurred_at DESC, id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query raw events: %w", err)
	}
	defer rows.Close()
	out := make([]RawEventEntry, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan raw event: %w", err)
		}
		var ev RawEventEntry
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// hydrateRawEvents 在加载会话后把原始事件的读源切到旁路表（读路径 cutover，镜像 hydrateLLMInteractions）：
// 表 ∪ blob 残留（按 id 去重、跳过空 ID、回填进表）→ 按 OccurredAt 升序 → cap 到 maxRawEventHistory。
// 必须在任何 Save 之前调用——免旧局事件被 Save 的「确认写表才摘除」瘦身丢掉。
func (service *Service) hydrateRawEvents(ctx context.Context, state *State) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	recent, err := service.ListRawEvents(ctx, state.ID, maxRawEventHistory)
	if err != nil {
		return // 读表失败：保留 blob 原值（降级但不丢）
	}
	present := make(map[string]bool, len(recent))
	for i := range recent {
		present[recent[i].ID] = true
	}
	residue := make([]RawEventEntry, 0)
	for i := range state.RawEventLog {
		ev := state.RawEventLog[i]
		if ev.ID == "" || present[ev.ID] {
			continue
		}
		_ = persistRawEvents(ctx, service.db, dbdialect.IsMySQL(service.db), state.ID, []RawEventEntry{ev})
		residue = append(residue, ev)
	}
	if len(recent) == 0 && len(residue) == 0 {
		return // 表空且 blob 无新增 → 保持原值
	}
	merged := make([]RawEventEntry, 0, len(recent)+len(residue))
	merged = append(merged, recent...) // 倒序
	merged = append(merged, residue...)
	sort.SliceStable(merged, func(a, b int) bool { return merged[a].OccurredAt.Before(merged[b].OccurredAt) })
	if len(merged) > maxRawEventHistory {
		merged = merged[len(merged)-maxRawEventHistory:]
	}
	state.RawEventLog = merged
}
