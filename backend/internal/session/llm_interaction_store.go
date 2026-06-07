package session

// 文件说明：LLM 交互旁路表的写入/读取（拆 state_json 第二片，沙盘 §11.2，影子双写阶段）。
// 与 decision_traces 同一套不变量纪律，但因 appendLLMInteraction 有 ~25 个调用点、不便逐点影子写，
// 改在 Repository.Save 里于 blob 压缩**之前**把 state.LLMInteractions 持久化到表（含完整 prompt）。
// 执行循环每个 actor 行动后即 Save，故 INSERT OR IGNORE 跨 Save 累积出全量历史；blob 仍裁剪仍权威。
//
// 三条与 decision_traces 一致的约束：
//  1. 影子阶段最小风险：写表 best-effort、吞错，绝不影响 Save 主路径（blob 仍为权威读源）。
//  2. 排序：occurred_at 复用 traceTimeLayout 定宽布局（纳秒补零），字典序=时间序，双驱动一致。
//  3. 跳过不应留痕的记录：空 ID、以及 InProgress（流式快照派生项，从不经 appendLLMInteraction 落库——双保险）。
//
// 隐私红线：本表含完整 prompt，故隐私擦除/保留期清理必须同步清本表（见 privacy.go），否则即不可逆擦除漏洞。
//
// 读路径切换（cutover）前必须解决的两个已知前置项（影子阶段无害，因 blob 仍权威；评审 M6.2 第二片记录）：
//  A. 全量完整性依赖「单次 load→Save 窗口内 append 的新交互数 < maxLLMHistory(48)」——否则最旧者会在 append 时被切片淘汰、
//     从未活到 Save、永不入表。今日成立（开局花名册 ≤~24 < 48；执行主循环更是每 actor 即 Save）。一旦花名册放大到 ~48+
//     或回合边界批量结算链（advanceAfterAsyncExecution / resolvePigeonDispatches 等单 Save 多 append）规模增大即破。
//     cutover 把本表升为权威读源前，须改为 append 源头双写（像 decision_traces 那样逐条落库）或加窗口断言/遥测守住。
//  B. 下面 Save 处的写表失败当前被静默吞掉（best-effort）。影子期 blob 权威故无害，但 cutover 前须加可观测信号
//     （进程级计数 + /healthz）或「确认写表才瘦身」的自愈门，避免持续失败到 cutover 才暴露、届时 blob prompt 已压缩抹除。

import (
	"context"
	"encoding/json"
	"fmt"
)

// persistLLMInteractions 把若干 LLM 交互写入旁路表（按 id 幂等）。影子阶段供 Repository.Save 调用，
// best-effort：返回错误供调用方记录，但调用方吞错不影响 blob 权威写。跳过空 ID 与 InProgress 记录。
func persistLLMInteractions(ctx context.Context, db traceExecer, mysql bool, sessionID string, interactions []LLMInteraction) error {
	query := `INSERT INTO llm_interactions (id, session_id, unit_id, interaction_json, occurred_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`
	if mysql {
		query = `INSERT IGNORE INTO llm_interactions (id, session_id, unit_id, interaction_json, occurred_at) VALUES (?, ?, ?, ?, ?)`
	}
	for i := range interactions {
		it := interactions[i]
		if it.ID == "" || it.InProgress {
			continue
		}
		encoded, err := json.Marshal(it)
		if err != nil {
			continue
		}
		if _, err := db.ExecContext(ctx, query, it.ID, sessionID, nullableStr(it.UnitID), string(encoded), formatTraceTime(it.OccurredAt)); err != nil {
			return fmt.Errorf("persist llm interaction %s: %w", it.ID, err)
		}
	}
	return nil
}

// ListLLMInteractions 从旁路表按时间倒序读某会话最近的 LLM 交互（定宽 occurred_at，字典序即时间序）。
// 影子阶段仅供测试/审计；读路径切换后将成为 hydrate 的权威读源。
func (service *Service) ListLLMInteractions(ctx context.Context, sessionID string, limit int) ([]LLMInteraction, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("list llm interactions: missing db")
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT interaction_json FROM llm_interactions WHERE session_id = ?
		ORDER BY occurred_at DESC, id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, fmt.Errorf("query llm interactions: %w", err)
	}
	defer rows.Close()
	out := make([]LLMInteraction, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("scan llm interaction: %w", err)
		}
		var it LLMInteraction
		if err := json.Unmarshal([]byte(raw), &it); err != nil {
			continue
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
