package session

// 文件说明：跨玩家交互的三档异步同意闸（consent_gate，设计 §2.3 + relevance.ConsentTierFor）。高后果交互（联姻/复仇/开战/结盟/反目）
// 需对方角色自治同意：落 consent_requests(pending) 待目标方玩家/角色 resolve；accept 才应用关系效果，reject 不应用，
// 超时 expire 兜底（charter：不应用、避免无限挂起）。对齐 D1「能听见不能强迫」——同意是档而非覆盖。

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/relevance"
)

// ConsentRequest 是一条异步同意请求。
type ConsentRequest struct {
	ID          string `json:"id"`
	WorldID     string `json:"world_id"`
	ActorID     string `json:"actor_unit_id"`
	TargetID    string `json:"target_unit_id"`
	Interaction string `json:"interaction"`
	Tier        string `json:"tier"`
	Status      string `json:"status"` // pending/accepted/rejected/expired
	EventID     string `json:"event_id"`
	CreatedAt   string `json:"created_at"`
	ResolvedAt  string `json:"resolved_at,omitempty"`
}

func nowConsentTS() string { return time.Now().UTC().Format("2006-01-02 15:04:05") }

func (service *Service) createConsentRequest(ctx context.Context, worldID, actorID, targetID string, interaction SevenInteraction, tier relevance.ConsentTier, eventID string) (string, error) {
	id := uuid.NewString()
	if _, err := service.db.ExecContext(ctx,
		`INSERT INTO consent_requests (id, world_id, actor_unit_id, target_unit_id, interaction, tier, status, event_id, created_at)
		 VALUES (?,?,?,?,?,?, 'pending', ?, ?)`,
		id, worldID, actorID, targetID, string(interaction), string(tier), eventID, nowConsentTS()); err != nil {
		return "", fmt.Errorf("create consent request: %w", err)
	}
	return id, nil
}

func scanConsent(scan func(dest ...any) error) (ConsentRequest, error) {
	var r ConsentRequest
	var eventID, resolvedAt sql.NullString
	err := scan(&r.ID, &r.WorldID, &r.ActorID, &r.TargetID, &r.Interaction, &r.Tier, &r.Status, &eventID, &r.CreatedAt, &resolvedAt)
	r.EventID = eventID.String
	r.ResolvedAt = resolvedAt.String
	return r, err
}

const consentCols = `id, world_id, actor_unit_id, target_unit_id, interaction, tier, status, event_id, created_at, resolved_at`

// GetConsentRequest 读一条同意请求。
func (service *Service) GetConsentRequest(ctx context.Context, reqID string) (ConsentRequest, error) {
	return scanConsent(service.db.QueryRowContext(ctx, `SELECT `+consentCols+` FROM consent_requests WHERE id = ?`, reqID).Scan)
}

// ListPendingConsents 列出某 target 角色待处理的同意请求（其玩家可决定接受/拒绝）。
func (service *Service) ListPendingConsents(ctx context.Context, targetID string) ([]ConsentRequest, error) {
	rows, err := service.db.QueryContext(ctx,
		`SELECT `+consentCols+` FROM consent_requests WHERE target_unit_id = ? AND status = 'pending' ORDER BY created_at ASC`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ConsentRequest, 0)
	for rows.Next() {
		r, err := scanConsent(rows.Scan)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveConsentRequest 处理一条 pending 同意请求：accept→应用关系效果并置 accepted；reject→置 rejected（不应用）。
// 守门：原子 UPDATE … WHERE status='pending'（RowsAffected==0 即已被处理，防重复/竞态——对齐 CompleteJob 模式）。
func (service *Service) ResolveConsentRequest(ctx context.Context, reqID string, accept bool) (ConsentRequest, error) {
	req, err := service.GetConsentRequest(ctx, reqID)
	if err != nil {
		return req, err
	}
	if req.Status != "pending" {
		return req, fmt.Errorf("consent request %s not pending (status=%s)", reqID, req.Status)
	}
	newStatus := "rejected"
	if accept {
		newStatus = "accepted"
	}
	res, err := service.db.ExecContext(ctx,
		`UPDATE consent_requests SET status = ?, resolved_at = ? WHERE id = ? AND status = 'pending'`,
		newStatus, nowConsentTS(), reqID)
	if err != nil {
		return req, fmt.Errorf("resolve consent: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return req, fmt.Errorf("consent request %s already resolved", reqID)
	}
	if accept {
		if tmpl, ok := sevenTemplates[SevenInteraction(req.Interaction)]; ok {
			if err := service.applySevenEffect(ctx, req.ActorID, req.TargetID, tmpl); err != nil {
				return req, err // 效果应用失败：状态已置 accepted，关系未变（调用方可重试/告警）
			}
		}
	}
	req.Status = newStatus
	req.ResolvedAt = nowConsentTS()
	return req, nil
}

// ExpireStaleConsents 把创建早于 cutoff 仍 pending 的请求置 expired（charter 兜底：不应用效果、避免无限挂起）。返回置 expired 数。
func (service *Service) ExpireStaleConsents(ctx context.Context, cutoff string) (int64, error) {
	res, err := service.db.ExecContext(ctx,
		`UPDATE consent_requests SET status = 'expired', resolved_at = ? WHERE status = 'pending' AND created_at < ?`,
		nowConsentTS(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("expire consents: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
