package session

// 文件说明：回响 Echo + 玩家动作事件（设计宪法 §6.2）。
// 三件事：
//  1. 把玩家的「接管/嘱咐」记成一条可被引用的真实事件（RecordPlayerIntervention）。
//  2. 把近期玩家动作事件 ID 暴露给决策归因（loadRecentPlayerActions），让 LLM 能合法引用 order_echo。
//  3. 当某次自治选择被归因到一条真实玩家动作时，生成「因为你上次…，所以这次…」的回响卡进收件箱。
//
// 硬约束（宪法 §6.2）：order_echo.ref 必须指向**真实存在**的玩家动作事件，否则不生成回响——
// 编造「因为你上次」本身就是 OOC。归因校验器(engine/decision)已据 PlayerActionEventIDs 强制这一点。

import (
	"context"
	"encoding/json"
	"fmt"

	"qunxiang/backend/internal/analytics"
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
)

// echoRefFromAttribution 若决策归因里引用了 order_echo（指向一条过往玩家动作），返回其 ref 与叙事。
func echoRefFromAttribution(p *attributionPayload) (refID string, narrative string, ok bool) {
	if p == nil {
		return "", "", false
	}
	if p.Primary.Kind == string(decision.CauseOrderEcho) && p.Primary.RefID != "" {
		return p.Primary.RefID, p.NarrativeZH, true
	}
	for _, c := range p.Supporting {
		if c.Kind == string(decision.CauseOrderEcho) && c.RefID != "" {
			return c.RefID, p.NarrativeZH, true
		}
	}
	return "", "", false
}

// playerActionRow 是一条可被 order_echo 引用的玩家动作。
type playerActionRow struct {
	EventID string
	Summary string
}

// RecordPlayerIntervention 把玩家的一次直接接管/嘱咐记成 append-only 事件，返回事件 ID（可被回响引用）。
// summary 是这次接管「做了什么」的一句话（如「你让她放过了那个逃兵」），回响卡会引用它。
func (service *Service) RecordPlayerIntervention(ctx context.Context, sessionID string, unitID string, summary string) (string, error) {
	if service == nil || service.db == nil {
		return "", fmt.Errorf("record intervention: missing db")
	}
	if unitID == "" {
		return "", fmt.Errorf("record intervention: empty unit")
	}
	id, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		Code:        events.ReasonPlayerIntervention,
		Category:    events.CategoryPlayer,
		Payload:     map[string]any{"summary": summary},
	})
	if err != nil {
		return "", err
	}
	_ = analytics.Emit(ctx, service.db, analytics.Event{
		Stage: analytics.StageRetention, Name: analytics.EventIntervention,
		SessionID: sessionID, UnitID: unitID, Props: map[string]any{"summary": summary},
	})
	return id, nil
}

// loadRecentPlayerActions 取某角色近期可被 order_echo 引用的玩家动作（接管 + 处理过的待决策）。
func (service *Service) loadRecentPlayerActions(ctx context.Context, unitID string, limit int) []playerActionRow {
	out := make([]playerActionRow, 0)
	if service == nil || service.db == nil || unitID == "" {
		return out
	}
	if limit <= 0 {
		limit = 8
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT id, reason_code, payload_json FROM events
		WHERE actor_unit_id = ? AND reason_code IN (?, ?)
		ORDER BY occurred_at DESC LIMIT ?`,
		unitID, string(events.ReasonPlayerIntervention), string(events.ReasonDecisionResolved), limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, code, payloadJSON string
		if err := rows.Scan(&id, &code, &payloadJSON); err != nil {
			return out
		}
		out = append(out, playerActionRow{EventID: id, Summary: playerActionSummary(code, payloadJSON)})
	}
	return out
}

// playerActionSummary 从事件 payload 还原「玩家那次做了什么」的一句话。
func playerActionSummary(code string, payloadJSON string) string {
	var p struct {
		Summary     string `json:"summary"`
		ResolveType string `json:"resolve_type"`
	}
	_ = json.Unmarshal([]byte(payloadJSON), &p)
	if p.Summary != "" {
		return p.Summary
	}
	switch code {
	case string(events.ReasonDecisionResolved):
		if p.ResolveType != "" {
			return "你为她拿了主意（" + p.ResolveType + "）"
		}
		return "你为她处理了一件待决策的事"
	case string(events.ReasonPlayerIntervention):
		return "你直接接管了她一次"
	default:
		return "你的一次干预"
	}
}

// SurfaceEcho 在某次自治选择被归因到一条真实玩家动作(priorEventID)时，生成回响卡进收件箱。
// priorEventID 必须能在 actor 的玩家动作里找到（否则返回 false，不生成——绝不编造「因为你上次」）。
// currentNarrative 是这次她做了什么的一句话。返回是否生成了回响卡。
func (service *Service) SurfaceEcho(ctx context.Context, sessionID string, unitID string, priorEventID string, currentNarrative string, valence float64) (bool, error) {
	if service == nil || service.db == nil || unitID == "" || priorEventID == "" {
		return false, nil
	}
	prior := ""
	for _, a := range service.loadRecentPlayerActions(ctx, unitID, 32) {
		if a.EventID == priorEventID {
			prior = a.Summary
			break
		}
	}
	if prior == "" {
		return false, nil // 无真实前因 → 不生成回响（宪法 §6.2 硬约束）
	}
	card := fmt.Sprintf("因为你上次%s，这一回，%s", prior, currentNarrative)
	payload := map[string]any{
		"narrative":      card,
		"prior_event_id": priorEventID,
		"prior_choice":   prior,
		"valence":        valence,
		"echo":           true,
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		Code:        events.ReasonEchoLink,
		Category:    events.CategoryPlayer,
		Payload:     payload,
	}); err != nil {
		return false, err
	}
	return true, nil
}
