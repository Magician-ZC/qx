package session

// 文件说明：提供举报提交与审计数据打包接口，把监管链路统一落到报告、日志与原始事件。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	defaultAuditLimit = 80
	maxAuditLimit     = 512
)

// SubmitModerationReport 接收玩家举报并写入会话审计链路。
// 该方法会校验举报内容、补全默认字段、确认目标单位存在，
// 然后把举报同时写入 ModerationReports、结构化日志和 RawEventLog，确保后续可追溯。
func (service *Service) SubmitModerationReport(
	ctx context.Context,
	sessionID string,
	reporter string,
	unitID string,
	category string,
	detail string,
) (Snapshot, ModerationReport, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, ModerationReport{}, err
	}

	detail = strings.TrimSpace(detail)
	if detail == "" {
		return Snapshot{}, ModerationReport{}, fmt.Errorf("report detail is required")
	}
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		category = "other"
	}
	reporter = strings.TrimSpace(reporter)
	if reporter == "" {
		reporter = "player"
	}

	byID := mapRecordsByID(units)
	if unitID != "" {
		if _, ok := byID[unitID]; !ok {
			return Snapshot{}, ModerationReport{}, fmt.Errorf("unit %s was not found", unitID)
		}
	}

	report := ModerationReport{
		ID:        uuid.NewString(),
		SessionID: state.ID,
		Turn:      state.TurnState.Turn,
		Phase:     state.TurnState.Phase,
		Reporter:  reporter,
		UnitID:    strings.TrimSpace(unitID),
		Category:  category,
		Detail:    detail,
		CreatedAt: time.Now().UTC(),
	}
	state.ModerationReports = append(state.ModerationReports, report)
	if len(state.ModerationReports) > maxModerationReports {
		state.ModerationReports = state.ModerationReports[len(state.ModerationReports)-maxModerationReports:]
	}
	appendLog(
		&state,
		"moderation_report",
		fmt.Sprintf("玩家提交举报：category=%s unit=%s detail=%s", category, report.UnitID, detail),
		"",
		report.UnitID,
	)
	appendRawEvent(&state, rawEventSpec{
		source:       "moderation",
		kind:         category,
		summary:      detail,
		targetUnitID: report.UnitID,
		payload:      report,
	})

	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, ModerationReport{}, err
	}
	return buildSnapshot(state, units), report, nil
}

// GetAuditBundle 返回最近一段时间的审计数据快照。
// 它会按统一的 limit 裁剪举报、对话、LLM 交互、普通日志与原始事件，
// 供前端审计面板或外部监管流程一次性拉取。
func (service *Service) GetAuditBundle(ctx context.Context, sessionID string, limit int) (AuditBundle, error) {
	// 必须走 loadSession 而非裸 sessions.Get：拆 state_json 后 LLMInteractions（及后续 RawEventLog）已从 blob 摘除、
	// 读源切到旁路表，唯有 loadSession 会 hydrate 回内存工作集。裸 Get 只 unmarshal blob，审计的 llm_interactions 会恒空。
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return AuditBundle{}, err
	}
	limit = normalizeAuditLimit(limit)

	return AuditBundle{
		SessionID:       state.ID,
		Reports:         tailModerationReports(state.ModerationReports, limit),
		DialogueHistory: tailDialogues(state.DialogueHistory, limit),
		LLMInteractions: tailInteractions(state.LLMInteractions, limit),
		Logs:            tailLogs(state.Logs, limit),
		RawEventLog:     tailRawEvents(state.RawEventLog, limit),
	}, nil
}

// normalizeAuditLimit 将审计拉取条数限制在服务端允许范围内，避免一次查询过大。
func normalizeAuditLimit(limit int) int {
	if limit <= 0 {
		return defaultAuditLimit
	}
	if limit > maxAuditLimit {
		return maxAuditLimit
	}
	return limit
}

// tailModerationReports 返回举报列表的末尾 limit 条，并复制新切片避免外部修改内部状态。
func tailModerationReports(items []ModerationReport, limit int) []ModerationReport {
	if len(items) <= limit {
		return append([]ModerationReport{}, items...)
	}
	return append([]ModerationReport{}, items[len(items)-limit:]...)
}

// tailDialogues 返回对话历史的末尾 limit 条，保证审计输出与状态存储解耦。
func tailDialogues(items []DialogueMessage, limit int) []DialogueMessage {
	if len(items) <= limit {
		return append([]DialogueMessage{}, items...)
	}
	return append([]DialogueMessage{}, items[len(items)-limit:]...)
}

// tailInteractions 返回 LLM 交互记录的末尾 limit 条，用于排查决策链路。
func tailInteractions(items []LLMInteraction, limit int) []LLMInteraction {
	if len(items) <= limit {
		return append([]LLMInteraction{}, items...)
	}
	return append([]LLMInteraction{}, items[len(items)-limit:]...)
}

// tailLogs 返回普通运行日志的末尾 limit 条，便于快速定位最近行为。
func tailLogs(items []LogEntry, limit int) []LogEntry {
	if len(items) <= limit {
		return append([]LogEntry{}, items...)
	}
	return append([]LogEntry{}, items[len(items)-limit:]...)
}

// tailRawEvents 返回原始事件流的末尾 limit 条，保留最细粒度的调试上下文。
func tailRawEvents(items []RawEventEntry, limit int) []RawEventEntry {
	if len(items) <= limit {
		return append([]RawEventEntry{}, items...)
	}
	return append([]RawEventEntry{}, items[len(items)-limit:]...)
}
