package session

// 文件说明：提供举报提交与审计数据打包接口，把监管链路统一落到报告、日志与原始事件。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
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

// 常量定义区：举报处置动作的取值（运营侧裁定一桩举报时的三种结论）。
const (
	moderationActionResolve = "resolve" // 仅标记已解决，不施加任何状态后果。
	moderationActionWarn    = "warn"    // 警告：对被举报单位小幅下调士气示警。
	moderationActionBan     = "ban"     // 封禁：对被举报单位重罚士气与忠诚。

	// 处置幅度（经 status.Mutator 落地，字段级 clamp 由 Mutator 负责）。
	moderationWarnMoraleDelta = -4.0  // 警告：小幅士气示警。
	moderationBanMoraleDelta  = -12.0 // 封禁：士气重挫。
	moderationBanLoyaltyDelta = -8.0  // 封禁：归属感重挫。
)

// ResolveModerationReport 裁定一桩举报并闭环：按 reportID 在 state.ModerationReports 标记 Resolved=true/ResolvedAt=now，
// 再按 action（resolve|warn|ban）对被举报单位（report.UnitID 非空时）经 **status.Mutator** 施加状态后果——
// warn 小幅下调士气示警，ban 重罚士气与忠诚；resolve 仅标记。
//
// 纪律与安全：① 受保护字段（Morale/Loyalty）一律经 Mutator.Apply（字段级 clamp + 标准化事件留痕 + 追加 RecentEventIDs），
// 绝不直接改 unit.Status；② Mutator 落地用的 reason code（MODERATION_WARNING / MODERATION_BAN）已在 events.Catalog 登记，
// 否则 Apply 会因「unknown reason code」报错；③ 整桩处置走 loadSession→改 state→appendLog/appendRawEvent 留痕→Save，
// 与 SubmitModerationReport 同范式；④ 返回更新后的 PublicSnapshot 与该 report，便于 router 复用渲染。
func (service *Service) ResolveModerationReport(
	ctx context.Context,
	sessionID string,
	reportID string,
	action string,
	note string,
) (Snapshot, ModerationReport, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, ModerationReport{}, err
	}

	reportID = strings.TrimSpace(reportID)
	if reportID == "" {
		return Snapshot{}, ModerationReport{}, fmt.Errorf("report id is required")
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = moderationActionResolve
	}
	switch action {
	case moderationActionResolve, moderationActionWarn, moderationActionBan:
	default:
		return Snapshot{}, ModerationReport{}, fmt.Errorf("unknown moderation action %q (want resolve|warn|ban)", action)
	}
	note = strings.TrimSpace(note)

	// 1) 按 reportID 定位举报；不存在即拒绝。
	idx := -1
	for i := range state.ModerationReports {
		if state.ModerationReports[i].ID == reportID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Snapshot{}, ModerationReport{}, fmt.Errorf("report %s was not found", reportID)
	}

	// 2) 标记已解决（幂等：重复处置只刷新 ResolvedAt 与状态后果由 Mutator clamp 自行收敛）。
	now := time.Now().UTC()
	state.ModerationReports[idx].Resolved = true
	state.ModerationReports[idx].ResolvedAt = now
	report := state.ModerationReports[idx]

	// 3) warn/ban：对被举报单位经 status.Mutator 施加状态后果（report.UnitID 非空时）。
	// 被举报单位可能已不在当前工作集（已死亡/移除），此时跳过状态后果但仍完成标记与留痕。
	if action != moderationActionResolve && report.UnitID != "" {
		if _, ok := mapRecordsByID(units)[report.UnitID]; ok && service.mutator != nil {
			for _, m := range moderationConsequences(action, report.UnitID, note) {
				if _, err := service.mutator.Apply(ctx, m); err != nil {
					return Snapshot{}, ModerationReport{}, fmt.Errorf("apply moderation consequence (%s/%s): %w", action, m.Field, err)
				}
			}
		}
	}

	// 4) 留痕：结构化日志 + 原始事件流，与 SubmitModerationReport 一致。
	appendLog(
		&state,
		"moderation_resolution",
		fmt.Sprintf("运营裁定举报：report=%s action=%s unit=%s note=%s", report.ID, action, report.UnitID, note),
		"",
		report.UnitID,
	)
	appendRawEvent(&state, rawEventSpec{
		source:       "moderation",
		kind:         "resolution",
		summary:      fmt.Sprintf("action=%s note=%s", action, note),
		targetUnitID: report.UnitID,
		payload: map[string]any{
			"report_id":   report.ID,
			"action":      action,
			"note":        note,
			"resolved_at": now,
			"unit_id":     report.UnitID,
			"category":    report.Category,
		},
	})

	// 5) 重新载入被 Mutator 改写后的单位用于快照（Mutator 直写 DB，未回灌内存 units 切片）；
	// state（含本次新写的日志/举报标记）仍以内存工作集为准、随后 Save，故只取刷新后的 units。
	if action != moderationActionResolve && report.UnitID != "" && service.mutator != nil {
		if _, freshUnits, loadErr := service.loadSession(ctx, sessionID); loadErr == nil {
			units = freshUnits
		}
	}

	if err := service.sessions.Save(ctx, &state); err != nil {
		return Snapshot{}, ModerationReport{}, err
	}
	return buildSnapshot(state, units), report, nil
}

// moderationConsequences 把一种处置动作翻译为一组经 status.Mutator 落地的状态变更（确定性、可复算）。
// warn → 小幅士气示警；ban → 士气重挫 + 忠诚重挫。所有 Delta 由 Mutator 字段级 clamp 收敛，reason code 已在 Catalog 登记。
// Turn 用 0：举报处置在回合循环外发生（运营侧裁定），无回合上下文。
func moderationConsequences(action string, unitID string, note string) []status.Mutation {
	// note 为空时 reasonText 留空，Mutator.Apply 会回填该 reason code 的 DefaultReasonText。
	reasonText := note
	switch action {
	case moderationActionWarn:
		return []status.Mutation{{
			UnitID:     unitID,
			Turn:       0,
			Field:      status.FieldMorale,
			Delta:      moderationWarnMoraleDelta,
			ReasonCode: events.ReasonModerationWarning,
			ReasonText: reasonText,
			Actors:     []string{unitID},
		}}
	case moderationActionBan:
		return []status.Mutation{
			{
				UnitID:     unitID,
				Turn:       0,
				Field:      status.FieldMorale,
				Delta:      moderationBanMoraleDelta,
				ReasonCode: events.ReasonModerationBan,
				ReasonText: reasonText,
				Actors:     []string{unitID},
			},
			{
				UnitID:     unitID,
				Turn:       0,
				Field:      status.FieldLoyalty,
				Delta:      moderationBanLoyaltyDelta,
				ReasonCode: events.ReasonModerationBan,
				ReasonText: reasonText,
				Actors:     []string{unitID},
			},
		}
	default:
		return nil
	}
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
