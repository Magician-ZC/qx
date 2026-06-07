package session

// 文件说明：命运相关性与命运收件箱（设计宪法 §4 M1 的 session 落地）。
// 把「世界事件」按相关性翻译成「我的角色命运的一段」：构造角色的相关性锚（当前模型=关系锚，
// geo/redline/goal 待世界化后接入），用 engine/relevance 评分并三档路由（自治不打扰/高光卡/待决策），
// 待决策经 events.EmitProcessEvent 写入命运收件箱（流程事件，不改状态）。

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"qunxiang/backend/internal/analytics"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/narration"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

const (
	relationIntensityNorm  = 20.0 // 关系强度（四轴绝对值之和，[-10,10] 量级）归一化分母
	relationAnchorHalfLife = 14.0 // 关系锚半衰期（天）
)

// FateEvent 是一条可能与某角色命运相关的世界事件。
type FateEvent struct {
	ActorID       string
	TargetID      string
	ReasonCode    events.ReasonCode
	Importance    int
	EmotionWeight float64
	Summary       string // 事件一句话，用于命运卡
}

// FateRouting 是一条世界事件对某角色的命运路由结果。
type FateRouting struct {
	Route      relevance.FateRoute
	Relevance  float64
	DecisionID string // pending 时的待决策 ID（用于后续 resolve）
	Card       string // 祖魂语气命运卡
}

// FateInboxItem 是命运收件箱里的一条未决待决策。
type FateInboxItem struct {
	DecisionID string
	Narrative  string
	OccurredAt string
}

// buildRelevanceAnchors 从角色的对外关系构造相关性锚（当前=关系锚；geo/redline/goal 待世界化接入）。
func (service *Service) buildRelevanceAnchors(ctx context.Context, unitID string) []relevance.Anchor {
	anchors := make([]relevance.Anchor, 0)
	if service == nil || service.db == nil {
		return anchors
	}
	for _, r := range service.loadTopOutgoingRelations(ctx, unitID, 16) {
		weight := relationIntensity(r.Trust, r.Fear, r.Affection, r.Rivalry) / relationIntensityNorm
		if weight <= 0 {
			continue
		}
		if weight > 1 {
			weight = 1
		}
		anchors = append(anchors, relevance.Anchor{
			Kind:         relevance.Relation,
			Ref:          r.TargetUnitID,
			Weight:       weight,
			HalfLifeDays: relationAnchorHalfLife,
		})
	}
	return anchors
}

// eventRelevance 计算一条世界事件对某角色（其锚集）的相关性。
func eventRelevance(anchors []relevance.Anchor, ev FateEvent) float64 {
	hits := make([]relevance.Hit, 0, len(anchors))
	for _, a := range anchors {
		if a.Kind == relevance.Relation && (a.Ref == ev.ActorID || a.Ref == ev.TargetID) {
			hits = append(hits, relevance.Hit{Anchor: a})
		}
	}
	return relevance.Score(hits, 1.0)
}

// SurfaceFateEvent 把一条世界事件按相关性路由进某角色的命运层。
// 自治不打扰：返回 RouteAutonomous，不写流程事件（底层事件已留痕）；高光卡/待决策：写入命运收件箱。
func (service *Service) SurfaceFateEvent(ctx context.Context, sessionID string, owner *unit.Record, ev FateEvent) (FateRouting, error) {
	if service == nil || service.db == nil || owner == nil {
		return FateRouting{}, fmt.Errorf("surface fate event: missing dependencies")
	}
	var rel float64
	if ev.ActorID == owner.ID || ev.TargetID == owner.ID {
		// 直接发生在她身上 → 命运分由重要度/情绪强度决定（她自己的事一定相关）。
		rel = float64(ev.Importance) / 10.0
		if e := absFloat(ev.EmotionWeight); e > rel {
			rel = e
		}
		if rel > 1 {
			rel = 1
		}
	} else {
		// 发生在别人身上 → 经她的关系锚翻译相关性。
		rel = eventRelevance(service.buildRelevanceAnchors(ctx, owner.ID), ev)
	}
	route := relevance.RouteFor(rel)
	out := FateRouting{Route: route, Relevance: rel}
	if route == relevance.RouteAutonomous {
		return out, nil
	}

	out.Card = fateCard(ev, route)
	code := events.ReasonInboxHighlight
	payload := map[string]any{
		"narrative":     out.Card,
		"relevance":     rel,
		"source_actor":  ev.ActorID,
		"source_target": ev.TargetID,
		"reason":        string(ev.ReasonCode),
	}
	if route == relevance.RoutePending {
		code = events.ReasonPendingDecision
		out.DecisionID = "fd_" + uuid.NewString()
		payload["decision_id"] = out.DecisionID
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:     sessionID,
		OwnerUnitID:   owner.ID,
		RelatedUnitID: ev.TargetID,
		Code:          code,
		Category:      events.CategoryFate,
		Payload:       payload,
	}); err != nil {
		return out, err
	}
	// 漏斗埋点（best-effort）：待决策入箱是北极星「D2 收件箱处理率」的分母。
	if route == relevance.RoutePending {
		_ = analytics.Emit(ctx, service.db, analytics.Event{
			Stage: analytics.StageRetention, Name: analytics.EventDecisionPending,
			SessionID: sessionID, UnitID: owner.ID,
			Props: map[string]any{"decision_id": out.DecisionID, "relevance": rel},
		})
	}
	return out, nil
}

// OpenFateInbox 返回某角色未被处理的待决策（命运收件箱）。
func (service *Service) OpenFateInbox(ctx context.Context, unitID string) ([]FateInboxItem, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("open fate inbox: missing db")
	}
	resolved, err := service.resolvedDecisionIDs(ctx, unitID)
	if err != nil {
		return nil, err
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT payload_json, occurred_at FROM events
		 WHERE actor_unit_id = ? AND reason_code = ?
		 ORDER BY occurred_at DESC`,
		unitID, string(events.ReasonPendingDecision),
	)
	if err != nil {
		return nil, fmt.Errorf("query fate inbox: %w", err)
	}
	defer rows.Close()

	items := make([]FateInboxItem, 0)
	for rows.Next() {
		var payloadJSON, occurredAt string
		if err := rows.Scan(&payloadJSON, &occurredAt); err != nil {
			return nil, fmt.Errorf("scan fate inbox: %w", err)
		}
		var payload struct {
			DecisionID string `json:"decision_id"`
			Narrative  string `json:"narrative"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID == "" || resolved[payload.DecisionID] {
			continue
		}
		items = append(items, FateInboxItem{DecisionID: payload.DecisionID, Narrative: payload.Narrative, OccurredAt: occurredAt})
	}
	return items, rows.Err()
}

// ResolveFateDecision 把一条待决策标记为已处理（写 DECISION_RESOLVED 留痕）。
func (service *Service) ResolveFateDecision(ctx context.Context, sessionID string, unitID string, decisionID string, resolveType string) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("resolve fate decision: missing db")
	}
	_, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		Code:        events.ReasonDecisionResolved,
		Category:    events.CategoryFate,
		Payload:     map[string]any{"decision_id": decisionID, "resolve_type": resolveType},
	})
	if err == nil {
		// 漏斗埋点（best-effort）：处理一条待决策 = 北极星留存动作。
		_ = analytics.Emit(ctx, service.db, analytics.Event{
			Stage: analytics.StageRetention, Name: analytics.EventDecisionResolved,
			SessionID: sessionID, UnitID: unitID,
			Props: map[string]any{"decision_id": decisionID, "resolve_type": resolveType},
		})
	}
	return err
}

func (service *Service) resolvedDecisionIDs(ctx context.Context, unitID string) (map[string]bool, error) {
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT payload_json FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		unitID, string(events.ReasonDecisionResolved),
	)
	if err != nil {
		return nil, fmt.Errorf("query resolved decisions: %w", err)
	}
	defer rows.Close()
	resolved := map[string]bool{}
	for rows.Next() {
		var payloadJSON string
		if err := rows.Scan(&payloadJSON); err != nil {
			return nil, err
		}
		var payload struct {
			DecisionID string `json:"decision_id"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID != "" {
			resolved[payload.DecisionID] = true
		}
	}
	return resolved, rows.Err()
}

// WorldizeDeath 把一个角色之死，按相关性路由进「在乎她的每个人」的命运收件箱（双向耦合）。
// 返回被实际惊动（进高光卡/待决策）的人数。这正是「她的密友死了→我的命运」的机制落地。
func (service *Service) WorldizeDeath(ctx context.Context, sessionID string, deceased unit.Record) (int, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("worldize death: missing db")
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT source_unit_id FROM relations
		 WHERE target_unit_id = ?
		 ORDER BY (ABS(trust) + ABS(fear) + ABS(affection) + ABS(rivalry)) DESC
		 LIMIT 64`,
		deceased.ID,
	)
	if err != nil {
		return 0, fmt.Errorf("query mourners: %w", err)
	}
	mourners := make([]string, 0)
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan mourner: %w", err)
		}
		if source != "" && source != deceased.ID {
			mourners = append(mourners, source)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	summary := deceased.DisplayName() + " 倒下了，再也没能起来。"
	surfaced := 0
	for _, source := range mourners {
		owner := unit.Record{ID: source}
		routing, err := service.SurfaceFateEvent(ctx, sessionID, &owner, FateEvent{
			ActorID:       deceased.ID,
			TargetID:      deceased.ID,
			ReasonCode:    events.ReasonCombatDown,
			Importance:    8,
			EmotionWeight: -0.6,
			Summary:       summary,
		})
		if err != nil {
			return surfaced, err
		}
		if routing.Route != relevance.RouteAutonomous {
			surfaced++
		}
	}
	return surfaced, nil
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// fateCard 把世界事件渲染成祖魂语气的命运卡（engine/narration，确定性、无 LLM、按事件打散变体）。
func fateCard(ev FateEvent, route relevance.FateRoute) string {
	return narration.Beat(
		string(ev.ReasonCode),
		ev.EmotionWeight,
		route == relevance.RoutePending,
		ev.Summary,
		0, // 种子按 reason-code+摘要派生，保证编年史不重复
	)
}
