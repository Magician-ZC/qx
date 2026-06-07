package session

// 文件说明：LLM 交互旁路表的写入/读取/hydrate（拆 state_json 第二片，沙盘 §11.2，**读路径已 cutover**）。
// 与 decision_traces 同一套不变量纪律，但因 appendLLMInteraction 有 ~25 个调用点、不便逐点影子写，
// 改在 Repository.Save 里于 blob 压缩**之前**把 state.LLMInteractions 持久化到表（含完整 prompt），
// 确认写表成功才从 blob 摘除；load 时 hydrateLLMInteractions 从表读回、收敛为与切表前一致的工作集。
//
// 四条不变量：
//  1. 持久性：一条交互永远活在 {表, blob} 至少之一——Save 先确认写进表、成功才从 blob 摘除；写表失败则保留在 blob
//     （瘦身放弃，但绝不丢交互），下次 load 由 hydrate 合并 blob 残留并回填表自愈。
//  2. 排序：occurred_at 复用 traceTimeLayout 定宽布局（纳秒补零），字典序=时间序，双驱动一致；hydrate 再按 OccurredAt 升序兜底。
//  3. hydrate 合并：表 ∪ blob 残留（按 id 去重），任何回填失败而残留 blob 的交互也并入、绝不被表结果覆盖掉。
//  4. 零行为变化：hydrate 后用 compactLLMInteractions 收敛为「最近 maxLLMHistory、仅最近 maxFullLLMHistory 留完整 prompt」，
//     使切表后内存/快照视图与切表前逐字节一致；表保有全量完整 prompt 仅供 ListLLMInteractions 审计（普通客户端经
//     publicLLMInteractions 本就脱敏，故全量 prompt 不外泄）。跳过空 ID / InProgress（流式快照派生项，从不经 appendLLMInteraction 落库）。
//
// 隐私红线：本表含完整 prompt，故隐私擦除/保留期清理必须同步清本表（见 privacy.go），否则即不可逆擦除漏洞。
//
// 已知前置项（仍待后续加固，非当前缺陷，评审 M6.2 记录）：
//  A. 全量完整性依赖「单次 load→Save 窗口内 append 的新交互数 < maxLLMHistory(48)」——否则最旧者会在 append 时被切片淘汰、
//     从未活到 Save、永不入表。今日成立（开局花名册 ≤~24 < 48；执行主循环更是每 actor 即 Save）。一旦花名册放大到 ~48+
//     或回合边界批量结算链（advanceAfterAsyncExecution / resolvePigeonDispatches 等单 Save 多 append）规模增大即破——
//     届时须改为 append 源头双写（像 decision_traces 那样逐条落库）或加窗口断言/遥测守住。注意：cutover 后这部分若超窗丢失，
//     blob 也已不再保有（已摘除），故仅影响**审计全量**，不影响工作集正确性（工作集恒 ≤ maxLLMHistory、由 hydrate 重建）。
//  B. 写表失败当前 best-effort 吞错但靠不变量 #1（确认写表才摘除 + hydrate 自愈）兜底，不丢交互；唯缺持续失败的可观测信号
//     （后续可加进程级计数 + /healthz，仿 AttributionStats）。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"qunxiang/backend/internal/storage/dbdialect"
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

// hydrateLLMInteractions 在加载会话后把 LLM 交互的读源切到旁路表（读路径 cutover，镜像 hydrateDecisionTraces）：
// 表 ∪ blob 残留（按 id 去重，跳过空 ID / InProgress），按 OccurredAt 升序，再用 compactLLMInteractions 收敛为
// 与切表前一致的工作集。任何 blob 残留（现网旧局 / 影子写失败回退留 blob 的）都并入并回填进表，绝不丢。
// 必须在任何 Save 之前调用——免旧局交互被 Save 的「确认写表才摘除」瘦身丢掉。
func (service *Service) hydrateLLMInteractions(ctx context.Context, state *State) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	recent, err := service.ListLLMInteractions(ctx, state.ID, maxLLMHistory)
	if err != nil {
		return // 读表失败：保留 blob 原值（降级但不丢）
	}
	present := make(map[string]bool, len(recent))
	for i := range recent {
		present[recent[i].ID] = true
	}
	residue := make([]LLMInteraction, 0)
	for i := range state.LLMInteractions {
		it := state.LLMInteractions[i]
		if it.ID == "" || it.InProgress || present[it.ID] {
			continue
		}
		_ = persistLLMInteractions(ctx, service.db, dbdialect.IsMySQL(service.db), state.ID, []LLMInteraction{it})
		residue = append(residue, it)
	}
	if len(recent) == 0 && len(residue) == 0 {
		return // 表空且 blob 无新增 → 保持原值
	}
	merged := make([]LLMInteraction, 0, len(recent)+len(residue))
	merged = append(merged, recent...) // 倒序
	merged = append(merged, residue...)
	sort.SliceStable(merged, func(a, b int) bool { return merged[a].OccurredAt.Before(merged[b].OccurredAt) })
	state.LLMInteractions = merged
	compactLLMInteractions(state) // 收敛为与切表前一致的工作集（最近 maxLLMHistory、仅最近 maxFullLLMHistory 留完整 prompt）
}
