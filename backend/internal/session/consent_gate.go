package session

// 文件说明：跨玩家交互的三档异步同意闸（consent_gate，设计 §2.3 + relevance.ConsentTierFor）。高后果交互（联姻/复仇/开战/结盟/反目）
// 需对方角色自治同意：落 consent_requests(pending) 待目标方玩家/角色 resolve；accept 才应用关系效果，reject 不应用，
// 超时 expire 兜底（charter：不应用、避免无限挂起）。对齐 D1「能听见不能强迫」——同意是档而非覆盖。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/relevance"
)

// consentTTL 是 pending 同意请求的存活上限：超过即超时兜底置 expired（不应用关系效果，避免无限挂起）。
// 取 72h（3 天）——给跨时区/离线玩家足够的人工处理窗口，又不至于让待决堆积太久。
// 与 nowConsentTS 同布局做字典序=时间序比较（见 cost_dashboard.go 同款窗口口径）。
const consentTTL = 72 * time.Hour

// consentTimeLayout 是 consent 时间列的统一布局（与 nowConsentTS 一致）。字典序即时间序，
// cutoff 必须用同一布局格式化才能在 `created_at < ?` 上正确比较（仿 cost_dashboard 的 traceTimeLayout 注意点）。
const consentTimeLayout = "2006-01-02 15:04:05"

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

func nowConsentTS() string { return time.Now().UTC().Format(consentTimeLayout) }

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
// **事务原子**：在单个 tx 内 ① 原子 flip `status=? WHERE id=? AND status='pending'`（RowsAffected==0 即已被处理，
// 回滚 + 返回「已处理」错误——防重复/竞态，对齐 CompleteJob 模式）；② accept 时**在同一事务内**经
// applyRelationShiftTx 应用七类互动四轴关系增量，失败则**回滚**（status 回到 pending，可重试）；③ Commit。
// 这修了原先「先 flip 再应用、应用失败已 accepted 无法重试」的非原子缺口：现在关系效果与 accepted 翻转同生共死。
func (service *Service) ResolveConsentRequest(ctx context.Context, reqID string, accept bool) (ConsentRequest, error) {
	req, err := service.GetConsentRequest(ctx, reqID)
	if err != nil {
		return req, err
	}
	if req.Status != "pending" {
		return req, fmt.Errorf("consent request %s not pending (status=%s)", reqID, req.Status)
	}
	if service.db == nil {
		return req, fmt.Errorf("resolve consent: missing db")
	}
	newStatus := "rejected"
	if accept {
		newStatus = "accepted"
	}
	resolvedAt := nowConsentTS()

	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return req, fmt.Errorf("begin resolve consent tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op；任一早返回路径都回滚（关系/flip 整体不落库，可重试）。

	// ① 原子守门 flip：仅唯一胜出的 resolve 把 pending 翻成 accepted/rejected。
	res, err := tx.ExecContext(ctx,
		`UPDATE consent_requests SET status = ?, resolved_at = ? WHERE id = ? AND status = 'pending'`,
		newStatus, resolvedAt, reqID)
	if err != nil {
		return req, fmt.Errorf("resolve consent: %w", err)
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// 已被并发 resolve/expire 抢先处理：回滚（无副作用），返回「已处理」错误。
		return req, fmt.Errorf("consent request %s already resolved", reqID)
	}

	// ② accept 时在同一事务内应用关系效果；失败则随 defer 回滚（status 回到 pending），调用方可安全重试。
	if accept {
		if tmpl, ok := sevenTemplates[SevenInteraction(req.Interaction)]; ok {
			// best-effort 跨分片：actor/target 任一不在本库 → 跳过关系写、不报错（applyRelationShiftTx 内 SELECT 1 判存在）。
			if _, err := service.applyRelationShiftTx(ctx, tx, req.ActorID, req.TargetID, tmpl.Delta, "七种交互·"+tmpl.Reason); err != nil {
				return req, fmt.Errorf("apply consent relation effect: %w", err)
			}
		}
	}

	// ③ 提交：accepted 翻转与关系效果同时落库。
	if err := tx.Commit(); err != nil {
		return req, fmt.Errorf("commit resolve consent: %w", err)
	}
	req.Status = newStatus
	req.ResolvedAt = resolvedAt
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

// expireStaleConsents 是回合边界用的超时兜底入口：以 nowTS 为「现在」算出 cutoff = now - consentTTL，
// 把创建早于 cutoff 仍 pending 的请求批量置 expired。**确定性**：cutoff 仅由注入的 nowTS 派生（非进程墙钟），
// 同一 nowTS 恒得同一 cutoff，可复现。nowTS 解析失败则退回进程时钟（best-effort，不让格式异常吞掉兜底）。
// 双方言安全：参数化字符串比较，与 nowConsentTS 同布局做字典序=时间序比较（仿 cost_dashboard 窗口写法）。
func (service *Service) expireStaleConsents(ctx context.Context, nowTS string) (int64, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("expire stale consents: missing db")
	}
	now, err := time.Parse(consentTimeLayout, strings.TrimSpace(nowTS))
	if err != nil {
		now = time.Now().UTC() // nowTS 非法/缺失：退回进程时钟，仍把超时请求清掉（不因格式异常让 pending 永挂）。
	}
	cutoff := now.Add(-consentTTL).UTC().Format(consentTimeLayout)
	return service.ExpireStaleConsents(ctx, cutoff)
}

// autoResolveConsentsByCharter 基于目标单位的离线宪章 SocialMandates 做「自治同意决定」：玩家不在场时，
// 若目标单位被授权自行处理对应人际事（如宪章社交授权里含「结盟」「联姻」字样），则对该方向的 pending 同意请求
// best-effort 自动 accept（经 ResolveConsentRequest 在单事务内 flip + 应用关系效果）；未授权的留待玩家/超时兜底。
//
// **保守匹配**：只对 sevenTemplates 里登记的交互、且目标单位宪章 SocialMandates 文本「含该交互中文名」（tmpl.Reason，
// 如结盟/联姻/交易/结识）时才自动同意——授权是「白名单放行」而非「默认放行」，对齐 D1「能听见不能强迫」：
// 没写进授权的高后果交互（默认含复仇/开战，宪章一般不会写其名）继续走人工/超时。
// 全程 best-effort：单条失败只吞错跳过、绝不中断；返回自动同意条数。state 缺单位宪章/为 nil 时安全返回 0。
func (service *Service) autoResolveConsentsByCharter(ctx context.Context, state *State, targetID string) (int, error) {
	if service == nil || service.db == nil || state == nil || strings.TrimSpace(targetID) == "" {
		return 0, nil
	}
	charter, ok := GetUnitCharter(state, targetID)
	if !ok || len(charter.SocialMandates) == 0 {
		return 0, nil // 无授权 → 一律留待玩家/超时，自治绝不替玩家越权同意。
	}
	pending, err := service.ListPendingConsents(ctx, targetID)
	if err != nil {
		return 0, err
	}
	accepted := 0
	for _, req := range pending {
		tmpl, known := sevenTemplates[SevenInteraction(req.Interaction)]
		if !known {
			continue
		}
		if !mandateAllowsInteraction(charter.SocialMandates, tmpl.Reason) {
			continue // 该交互未被宪章社交授权点名 → 留待人工/超时兜底。
		}
		if _, err := service.ResolveConsentRequest(ctx, req.ID, true); err != nil {
			continue // best-effort：并发已处理/关系写失败只跳过，不中断其余请求。
		}
		accepted++
	}
	return accepted, nil
}

// mandateAllowsInteraction 判定一组社交授权文本里是否有任一条点名了该交互（按交互中文名子串匹配，去空白）。
// 确定性、零分配返回 bool；保守子串匹配——授权文本含「结盟」即放行 alliance，含「联姻」即放行 marriage。
func mandateAllowsInteraction(mandates []string, interactionName string) bool {
	name := strings.TrimSpace(interactionName)
	if name == "" {
		return false
	}
	for _, m := range mandates {
		if !strings.Contains(m, name) {
			continue
		}
		// 否定型授权是设计允许的合法写法（types.go：「勿与某派结仇」与「可代我结盟」并列）。
		// 含否定词的 mandate 即便点名该交互也绝不放行自治同意——否则「勿与某派结盟」因含「结盟」子串被误判放行（对抗评审 medium）。
		if mandateHasNegation(m) {
			return false // 显式否决：宁可不自动同意（留待玩家/超时），也不违背玩家禁令
		}
		return true
	}
	return false
}

// mandateHasNegation 判某条社交授权是否含否定/禁止语义（保守：含任一否定词即视为否定型，不参与自治放行）。
func mandateHasNegation(mandate string) bool {
	for _, neg := range []string{"勿", "不", "禁", "别", "拒", "莫", "毋"} {
		if strings.Contains(mandate, neg) {
			return true
		}
	}
	return false
}

// settleConsentsAtBoundary 是部署边界用的 consent 统一结算入口（供 service.go 在 settleAutonomy 附近调用）。
// 先做超时兜底 expireStaleConsents（必须：把超 TTL 的 pending 清成 expired，避免无限挂起），
// 再对本局活着单位做宪章授权驱动的自治同意 autoResolveConsentsByCharter（增强：玩家授权过的人际事自动放行）。
// 全程 best-effort：任一步失败只记日志、绝不中断回合推进（对齐自治结算「吞错不阻塞」约定）。
// nowTS 用注入的回合边界时刻（确定性可复现）；state 为 nil 时安全 no-op。
func (service *Service) settleConsentsAtBoundary(ctx context.Context, state *State) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	// ① 超时兜底（必须）：以回合边界时刻为「现在」清掉超 TTL 的 pending。
	if _, err := service.expireStaleConsents(ctx, nowConsentTS()); err != nil {
		appendLog(state, "consent", fmt.Sprintf("同意请求超时兜底失败（best-effort，已跳过）：%v", err), "", "")
	}
	// ② 自治同意（增强）：对登记了离线宪章社交授权的单位，按授权自动放行对应方向的 pending。
	//    仅遍历本局 UnitCharters 中有 SocialMandates 的单位（无授权单位本就不会自动同意，跳过省 DB）。
	for unitID, charter := range state.UnitCharters {
		if len(charter.SocialMandates) == 0 {
			continue
		}
		if _, err := service.autoResolveConsentsByCharter(ctx, state, unitID); err != nil {
			appendLog(state, "consent", fmt.Sprintf("单位 %s 自治同意结算失败（best-effort，已跳过）：%v", unitID, err), "", "")
		}
	}
}
