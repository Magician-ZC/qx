package session

// 文件说明：提供会话隐私擦除与保留期清理能力，覆盖对话、LLM 轨迹、审计日志、记忆与快照。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	defaultPrivacyRetentionDays = 30
	defaultPrivacyPurgeLimit    = 200
	maxPrivacyPurgeLimit        = 1000
)

// EraseSessionPrivateData 对单个会话执行按选项的数据擦除。
// 可选择清理对话、LLM 细节、审计链路、举报和单位记忆，并返回具体擦除统计。
func (service *Service) EraseSessionPrivateData(
	ctx context.Context,
	sessionID string,
	options PrivacyEraseOptions,
) (Snapshot, PrivacyEraseResult, error) {
	// 隐私红线 + 并发安全：擦除绝不能与后台异步执行的逐 actor Save 交错——否则擦除清空（blob/旁路表/相位快照）后，
	// 下一拍后台 Save 会把刚生成的完整 prompt 重新写回，使「不可逆擦除」失效。每个 HTTP 请求新建 Service 共享同一 *sql.DB，
	// per-instance 的 sessionSaveMu 跨请求实例不互斥，进程级 asyncExecutionRegistry 才是唯一有效护栏——故执行在飞时直接拒绝、要求重试。
	if isAsyncExecutionRunning(strings.TrimSpace(sessionID)) {
		return Snapshot{}, PrivacyEraseResult{}, fmt.Errorf("erase session %s: 本回合执行进行中，请待执行结束后重试擦除", strings.TrimSpace(sessionID))
	}

	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, PrivacyEraseResult{}, err
	}
	options = normalizePrivacyEraseOptions(options)

	result := PrivacyEraseResult{
		SessionID: state.ID,
	}

	if options.EraseDialogue {
		result.DialogueEntriesErased = len(state.DialogueHistory)
		state.DialogueHistory = []DialogueMessage{}
	}
	if options.EraseLLMDetails {
		for index := range state.LLMInteractions {
			changed := false
			if state.LLMInteractions[index].SystemPrompt != "" {
				state.LLMInteractions[index].SystemPrompt = ""
				changed = true
			}
			if state.LLMInteractions[index].UserPrompt != "" {
				state.LLMInteractions[index].UserPrompt = ""
				changed = true
			}
			if state.LLMInteractions[index].ParsedOutput != "" {
				state.LLMInteractions[index].ParsedOutput = ""
				changed = true
			}
			if state.LLMInteractions[index].RawOutput != "" {
				state.LLMInteractions[index].RawOutput = ""
				changed = true
			}
			if state.LLMInteractions[index].ErrorMessage != "" {
				state.LLMInteractions[index].ErrorMessage = ""
				changed = true
			}
			if state.LLMInteractions[index].FallbackCause != "" {
				state.LLMInteractions[index].FallbackCause = ""
				changed = true
			}
			if len(state.LLMInteractions[index].Attempts) > 0 {
				state.LLMInteractions[index].Attempts = nil
				changed = true
			}
			if changed {
				result.LLMInteractionsRedacted++
			}
		}
	}
	if options.EraseAuditTrail {
		result.AuditLogsErased = len(state.Logs)
		result.RawEventsErased = len(state.RawEventLog)
		state.Logs = []LogEntry{}
		state.RawEventLog = []RawEventEntry{}
	}
	// decision_traces（拆 state_json 第一片，cutover 后已是含 LLM 自由文本 Reasoning/Speak/Memory/Knowledge 的权威读源）
	// 既是 LLM 派生、又是决策审计链——擦除 LLM 细节或审计链路时一并清空内存，否则 Save 会把它回写表、下次 load 由 hydrate 读回，
	// 与刚补齐的 llm_interactions 擦除形成不对称缺口。表行本身在 Save 之后硬删（见下）。
	if options.EraseLLMDetails || options.EraseAuditTrail {
		state.DecisionTraces = nil
	}
	if options.EraseReports {
		result.ReportsErased = len(state.ModerationReports)
		state.ModerationReports = []ModerationReport{}
	}
	if options.EraseMemories {
		for index := range units {
			result.UnitHighlightsErased += len(units[index].Memory.Highlights) + len(units[index].Memory.RecentEventIDs)
			units[index].Memory.Highlights = []string{}
			units[index].Memory.RecentEventIDs = []string{}
			if err := service.units.Save(ctx, units[index]); err != nil {
				return Snapshot{}, PrivacyEraseResult{}, fmt.Errorf("save unit memory reset: %w", err)
			}

			memoryRows, ftsRows, err := service.deleteUnitMemoryRows(ctx, units[index].ID)
			if err != nil {
				return Snapshot{}, PrivacyEraseResult{}, err
			}
			result.MemoryRowsErased += memoryRows
			result.MemoryFTSRowsErased += ftsRows
		}
	}

	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, PrivacyEraseResult{}, err
	}
	// 隐私红线：LLM 交互旁路表（拆 state_json 第二片）含完整 prompt，擦除 LLM 细节时必须同步清本表，
	// 否则即不可逆擦除漏洞。须在 Save 之后——Save 会按 id 幂等回写（INSERT OR IGNORE 保留旧的完整行），
	// 故这里硬删整会话的旁路行，把 Save 回写的完整行一并抹掉；失败即整体擦除失败、绝不静默放过。
	if options.EraseLLMDetails {
		if _, err := service.db.ExecContext(ctx, `DELETE FROM llm_interactions WHERE session_id = ?`, state.ID); err != nil {
			return Snapshot{}, PrivacyEraseResult{}, fmt.Errorf("erase llm interactions side table: %w", err)
		}
	}
	// 对称清理 decision_traces 旁路表（权威读源，含 LLM 自由文本）。须在 Save 之后——Save 会把内存（已清空）幂等回写，
	// 但此前各 Save 累积的历史行仍在表里，这里硬删整会话；失败即整体擦除失败，绝不静默残留。
	if options.EraseLLMDetails || options.EraseAuditTrail {
		execResult, execErr := service.db.ExecContext(ctx, `DELETE FROM decision_traces WHERE session_id = ?`, state.ID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "erase decision traces side table"); rowsErr != nil {
			return Snapshot{}, PrivacyEraseResult{}, rowsErr
		} else {
			result.DecisionTracesErased = deleted
		}
	}
	// 对称清理 raw_event_log 旁路表（拆 state_json 第三片，审计链路读源）。擦除审计链路时一并硬删整会话；须在 Save 之后。
	// 计数仍用上面的内存工作集条数（result.RawEventsErased）——表里跨 Save 累积的行数（可远超 maxRawEventHistory）口径不同，不混入。
	if options.EraseAuditTrail {
		execResult, execErr := service.db.ExecContext(ctx, `DELETE FROM raw_event_log WHERE session_id = ?`, state.ID)
		if _, rowsErr := execRowsAffected(execResult, execErr, "erase raw event log side table"); rowsErr != nil {
			return Snapshot{}, PrivacyEraseResult{}, rowsErr
		}
	}
	if _, err := service.db.ExecContext(ctx, `DELETE FROM session_phase_snapshots WHERE session_id = ?`, state.ID); err != nil {
		return Snapshot{}, PrivacyEraseResult{}, fmt.Errorf("delete phase snapshots for privacy erase: %w", err)
	}
	if err := service.recordPhaseBoundarySnapshot(ctx, &state, units); err != nil {
		return Snapshot{}, PrivacyEraseResult{}, err
	}
	result.PhaseSnapshotsRegenerated = true

	return buildSnapshot(state, units), result, nil
}

// PurgeExpiredSessionData 批量清理超过保留期的历史会话及其关联数据。
// 清理范围覆盖会话、单位、事件、名人堂、快照、记忆索引和房间码等衍生表。
func (service *Service) PurgeExpiredSessionData(
	ctx context.Context,
	retentionDays int,
	limit int,
) (PrivacyPurgeResult, error) {
	retentionDays = normalizePrivacyRetentionDays(retentionDays)
	limit = normalizePrivacyPurgeLimit(limit)

	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	result := PrivacyPurgeResult{
		RetentionDays: retentionDays,
		CutoffUnix:    cutoff.Unix(),
	}

	query := `
		SELECT id
		FROM single_player_sessions
		WHERE julianday(updated_at) < julianday(?)
		ORDER BY updated_at ASC
		LIMIT ?
		`
	if dbdialect.IsMySQL(service.db) {
		query = `
		SELECT id
		FROM single_player_sessions
		WHERE updated_at < ?
		ORDER BY updated_at ASC
		LIMIT ?
		`
	}
	rows, err := service.db.QueryContext(
		ctx,
		query,
		cutoff.Format(time.RFC3339Nano),
		limit,
	)
	if err != nil {
		return PrivacyPurgeResult{}, fmt.Errorf("query expired sessions: %w", err)
	}
	defer rows.Close()

	sessionIDs := make([]string, 0, limit)
	for rows.Next() {
		var sessionID string
		if scanErr := rows.Scan(&sessionID); scanErr != nil {
			return PrivacyPurgeResult{}, fmt.Errorf("scan expired session id: %w", scanErr)
		}
		sessionIDs = append(sessionIDs, strings.TrimSpace(sessionID))
	}
	if err := rows.Err(); err != nil {
		return PrivacyPurgeResult{}, fmt.Errorf("iterate expired sessions: %w", err)
	}

	for _, sessionID := range sessionIDs {
		if sessionID == "" {
			continue
		}
		execResult, execErr := service.db.ExecContext(ctx, `DELETE FROM session_phase_snapshots WHERE session_id = ?`, sessionID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete phase snapshots"); rowsErr != nil {
			return PrivacyPurgeResult{}, rowsErr
		} else {
			result.PhaseSnapsDeleted += deleted
		}
		execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM duel_room_codes WHERE session_id = ?`, sessionID)
		if _, rowsErr := execRowsAffected(execResult, execErr, "delete duel room codes"); rowsErr != nil {
			if !isNoSuchTable(rowsErr, "duel_room_codes") {
				return PrivacyPurgeResult{}, rowsErr
			}
		}

		execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM events WHERE session_id = ?`, sessionID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete session events"); rowsErr != nil {
			return PrivacyPurgeResult{}, rowsErr
		} else {
			result.EventsDeleted += deleted
		}

		// 拆 state_json 的三张旁路表（沙盘 §11.2）：会话被清理时其旁路留痕也须一并删，否则永久孤儿、违保留期。
		// llm_interactions 含完整 prompt、raw_event_log 含事件载荷、decision_traces 含决策自由文本。
		execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM llm_interactions WHERE session_id = ?`, sessionID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete session llm interactions"); rowsErr != nil {
			return PrivacyPurgeResult{}, rowsErr
		} else {
			result.LLMInteractionsDeleted += deleted
		}
		execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM decision_traces WHERE session_id = ?`, sessionID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete session decision traces"); rowsErr != nil {
			return PrivacyPurgeResult{}, rowsErr
		} else {
			result.DecisionTracesDeleted += deleted
		}
		execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM raw_event_log WHERE session_id = ?`, sessionID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete session raw event log"); rowsErr != nil {
			return PrivacyPurgeResult{}, rowsErr
		} else {
			result.RawEventsDeleted += deleted
		}

		execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM hall_of_fame_entries WHERE source_session_id = ?`, sessionID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete hall entries"); rowsErr != nil {
			return PrivacyPurgeResult{}, rowsErr
		} else {
			result.HallEntriesDeleted += deleted
		}
		if deleted, coldErr := service.deleteColdHallEntriesBySession(ctx, sessionID); coldErr != nil {
			return PrivacyPurgeResult{}, coldErr
		} else {
			result.HallEntriesDeleted += deleted
		}

		execResult, execErr = service.db.ExecContext(
			ctx,
			`DELETE FROM memories_fts WHERE unit_id IN (SELECT id FROM units WHERE session_id = ?)`,
			sessionID,
		)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete memories_fts rows"); rowsErr != nil {
			if !isNoSuchTable(rowsErr, "memories_fts") {
				return PrivacyPurgeResult{}, rowsErr
			}
		} else {
			result.MemoriesFTSDeleted += deleted
		}

		unitsDeleted, unitErr := service.units.DeleteBySession(ctx, sessionID)
		if unitErr != nil {
			return PrivacyPurgeResult{}, unitErr
		}
		result.UnitsDeleted += unitsDeleted

		execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM single_player_sessions WHERE id = ?`, sessionID)
		if deleted, rowsErr := execRowsAffected(execResult, execErr, "delete expired session"); rowsErr != nil {
			return PrivacyPurgeResult{}, rowsErr
		} else {
			result.SessionsDeleted += deleted
		}
	}

	return result, nil
}

// normalizePrivacyEraseOptions 规范化擦除参数。
// 当调用方未显式指定任何开关时，默认执行全量隐私擦除。
func normalizePrivacyEraseOptions(options PrivacyEraseOptions) PrivacyEraseOptions {
	if options.EraseDialogue ||
		options.EraseLLMDetails ||
		options.EraseAuditTrail ||
		options.EraseMemories ||
		options.EraseReports {
		return options
	}
	return PrivacyEraseOptions{
		EraseDialogue:   true,
		EraseLLMDetails: true,
		EraseAuditTrail: true,
		EraseMemories:   true,
		EraseReports:    true,
	}
}

// normalizePrivacyRetentionDays 规范化数据保留天数，非法值回退到系统默认值。
func normalizePrivacyRetentionDays(days int) int {
	if days <= 0 {
		return defaultPrivacyRetentionDays
	}
	return days
}

// normalizePrivacyPurgeLimit 规范化单次清理会话数量上限，避免批处理过大。
func normalizePrivacyPurgeLimit(limit int) int {
	if limit <= 0 {
		return defaultPrivacyPurgeLimit
	}
	if limit > maxPrivacyPurgeLimit {
		return maxPrivacyPurgeLimit
	}
	return limit
}

// deleteUnitMemoryRows 删除单位记忆主表与全文索引表记录，并返回各自受影响行数。
func (service *Service) deleteUnitMemoryRows(ctx context.Context, unitID string) (int64, int64, error) {
	execResult, execErr := service.db.ExecContext(ctx, `DELETE FROM memories WHERE unit_id = ?`, unitID)
	memoryRows, err := execRowsAffected(execResult, execErr, "delete memory rows")
	if err != nil {
		return 0, 0, err
	}

	execResult, execErr = service.db.ExecContext(ctx, `DELETE FROM memories_fts WHERE unit_id = ?`, unitID)
	ftsRows, err := execRowsAffected(execResult, execErr, "delete memory fts rows")
	if err != nil {
		if isNoSuchTable(err, "memories_fts") {
			return memoryRows, 0, nil
		}
		return 0, 0, err
	}
	return memoryRows, ftsRows, nil
}

// execRowsAffected 统一处理 SQL 执行错误并提取受影响行数。
func execRowsAffected(result sql.Result, err error, operation string) (int64, error) {
	if err != nil {
		return 0, fmt.Errorf("%s: %w", operation, err)
	}
	affected, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return 0, fmt.Errorf("%s rows affected: %w", operation, rowsErr)
	}
	return affected, nil
}

// isNoSuchTable 判断错误是否为“指定表不存在”，用于兼容可选表清理流程。
func isNoSuchTable(err error, tableName string) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such table") && strings.Contains(text, strings.ToLower(tableName))
}
