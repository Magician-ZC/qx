package session

// 文件说明：跨玩家交互的三档异步同意闸（consent_gate，设计 §2.3 + relevance.ConsentTierFor）。高后果交互（联姻/复仇/开战/结盟/反目）
// 需对方角色自治同意：落 consent_requests(pending) 待目标方玩家/角色 resolve；accept 才应用关系效果，reject 不应用，
// 超时 expire 兜底（charter：不应用、避免无限挂起）。对齐 D1「能听见不能强迫」——同意是档而非覆盖。

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

// consent_state 列（cross_events）的同意档状态机取值（事件耦合 §3：consent_state 记同意档状态）。
// 全 best-effort 落库（旁路吞错，绝不阻断主结算）：层2 成立但等回应=contested_pending；层3 等 A 的命回应=consent_pending；
// A 自治接受/隐忍=accepted/declined；超时按档失效=timeout。
const (
	consentStateContestedPending = "contested_pending" // 层2(CONTESTED)：单方成立但 A 上线得一张回应卡
	consentStateConsentPending   = "consent_pending"   // 层3(REQUIRES_CONSENT)：A 同意前只挂 pending
	consentStateAccepted         = "accepted"          // A 的命自治回应=接受（关系效果已应用）
	consentStateDeclined         = "declined"          // A 的命自治回应=隐忍/保守拒绝（不应用）
	consentStateTimeout          = "timeout"           // 超时按档失效（层3 给 B 回响卡 / 层2 宪章兜底）
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
	// best-effort 旁路（§2.4/§2.5「知情权 + 被影响=她的命遇到另一缕命」）：交互成立的同一刻就给离线 A 投卡——
	// 层2(CONTESTED)投一张待决策回应卡（还手/求和/认账）；层3(REQUIRES_CONSENT)投一张高光「等她的命来回应」。
	// 同时把同意档状态记进 cross_events.consent_state，使「谁先动手 + 当前同意档」对争议可仲裁。任一步失败只吞错，
	// 绝不让投卡/状态写回滚掉已成立的 consent_request（投卡是旁路，consent_request 才是主结算）。
	req := ConsentRequest{ID: id, WorldID: worldID, ActorID: actorID, TargetID: targetID, Interaction: string(interaction), Tier: string(tier), Status: "pending", EventID: eventID}
	consentState := consentStateContestedPending
	if tier == relevance.RequiresConsent {
		consentState = consentStateConsentPending
	}
	service.bestEffortUpdateConsentState(ctx, eventID, consentState)
	service.surfaceConsentCardToTarget(ctx, req, tier)
	return id, nil
}

// bestEffortUpdateConsentState 把某 cross_event 的同意档状态写进 cross_events.consent_state（事件耦合 §3）。
// best-effort 旁路：eventID 空 / 列不存在 / 行不在本库（跨分片）→ 静默跳过，绝不阻断主结算（同意档只是可仲裁注记，
// 不是事实源；事实唯一回退 cross_events.occurred_at）。绝不直写他人 units/relations——只在 append-only 的 cross_events 注记同意档。
func (service *Service) bestEffortUpdateConsentState(ctx context.Context, eventID, state string) {
	if service == nil || service.db == nil || strings.TrimSpace(eventID) == "" {
		return
	}
	_, _ = service.db.ExecContext(ctx, `UPDATE cross_events SET consent_state = ? WHERE id = ?`, state, eventID)
}

// surfaceConsentCardToTarget 给离线 A（target）经 SurfaceFateEvent 投一张卡（§2.4/§2.5）：
//   - 层2(CONTESTED)：一张「待决策」回应卡（ReasonCrossConsentPending），祖魂语气「她在桥头被信任的人推了一把——
//     另一缕命撞上了她的命」，choices = 还手/求和/认账（后果对称）。SurfaceFateEvent 内已含每日待决策预算/三档路由，
//     预算用尽会自动降级高光卡，不轰炸玩家。
//   - 层3(REQUIRES_CONSENT)：一张高光卡「有人对她做了件大事，等她的命来回应」——A 的角色之后按归因自治回应，
//     不是玩家点「同意」按钮（祖魂只能劝）。
//
// 全程 best-effort 旁路：A 不在本库（跨分片）/ sessions 未注入 / SurfaceFateEvent 失败 → 只吞错，绝不阻断已成立的 consent_request。
// 关联单位（FateEvent.ActorID=B/TargetID=A）走 SurfaceFateEvent 的「直接发生在她身上」自相关路径，落库 target 是 A 本人（本库真实行）。
func (service *Service) surfaceConsentCardToTarget(ctx context.Context, req ConsentRequest, tier relevance.ConsentTier) {
	if service == nil || service.db == nil || service.units == nil {
		return
	}
	target, err := service.units.GetByID(ctx, req.TargetID)
	if err != nil || target.ID == "" {
		return // A 跨分片/不在本库：投卡是 A 侧本地体验，远端 A 由其本库 surface（best-effort 跳过）
	}
	ev := consentCardFateEvent(req, tier)
	if _, err := service.SurfaceFateEvent(ctx, target.SessionID, &target, ev); err != nil {
		// 旁路吞错只 log（best-effort，绝不阻断主结算）。
		slog.Warn("surface consent card to target failed (best-effort)", "target", req.TargetID, "tier", string(tier), "err", err)
	}
}

// consentCardFateEvent 据档构造投给 A 的命运事件（祖魂语气，§2.5「被影响=她的命遇到另一缕命」）。
// 层2→待决策档措辞（importance/emotion 偏高，配合 SurfaceFateEvent 路由进待决策）；层3→等回应的高光档。
// 纯函数、确定性。ActorID=B（对手，仅入 payload/措辞）、TargetID=A（她本人，走自相关路径落本库真实行）。
func consentCardFateEvent(req ConsentRequest, tier relevance.ConsentTier) FateEvent {
	tmpl, ok := sevenTemplates[SevenInteraction(req.Interaction)]
	deedZH := "一桩事"
	if ok {
		deedZH = tmpl.Reason
	}
	if tier == relevance.RequiresConsent {
		// 层3：不可逆且双向（联姻/血脉）——只挂 pending，先投高光「等她的命来回应」。
		return FateEvent{
			ActorID:       req.ActorID,
			TargetID:      req.TargetID,
			ReasonCode:    events.ReasonCrossConsentPending,
			Importance:    6,
			EmotionWeight: 0.4,
			Summary:       "有人对她做了件大事（" + deedZH + "），这事等她的命来回应。",
			AttributionZH: "另一缕命，正等着撞上她的命。",
		}
	}
	// 层2：高代价但可回应（结盟/复仇宣告/反目/偷袭背叛）——投待决策回应卡。
	return FateEvent{
		ActorID:       req.ActorID,
		TargetID:      req.TargetID,
		ReasonCode:    events.ReasonCrossConsentPending,
		Importance:    8,
		EmotionWeight: -0.6,
		Summary:       "她在桥头被信任的人推了一把（" + deedZH + "）——另一缕命撞上了她的命。",
		AttributionZH: "是另一缕命，撞上了她的命。",
	}
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
//
// **跨玩家硬不变量（设计宪法 §2.1/§2.3，2026-06-10 修复 major-2）**：接受方（target）的同意结算只写**接受方自己 owner 一侧**的
// 关系边——`source=target(接受方) → target=actor(发起方)`。这与「各自的 status.Mutator 只改自己 owner 一侧的 relations、
// 永不互相 UPDATE 对方」逐字对齐：B 接受 A 的联姻，落的是「B 对 A 的好感/信任」这条 B 的本侧出边，**绝不**替发起方 A 写
// A 的出边（A 的本侧增量由 A 自己 session 读 cross_event consent_state=accepted 后自行翻译应用——见 surfaceCrossEventsAtBoundary）。
// 写前经 assertOwnSideRelationWrite 钉死「relation source==接受方（绝不写反成发起方出边）」，把红线断言真正武装到
// 跨玩家结算路径（修复 major-1：断言不再休眠空转）。这同时消除了原先「接受方 session 直写发起方 outgoing relations」的张力。
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
			// 跨玩家硬不变量：写的是**接受方自己 owner 一侧**的出边（source=接受方=req.TargetID → target=发起方=req.ActorID）。
			// edgeSource 是关系写与红线断言**共用的同一变量**——若有人误把它改成 req.ActorID（写反成发起方出边），
			// assertOwnSideRelationWrite 立即拒绝（source != 接受方 req.TargetID）+ 落审计。这样断言真正绑住关系写方向，绝非装好没接线。
			edgeSource := req.TargetID
			if err := service.assertOwnSideRelationWrite(ctx, tx, edgeSource, req.TargetID, "consent_accept_own_side_edge"); err != nil {
				return req, fmt.Errorf("apply consent relation effect: %w", err)
			}
			// best-effort 跨分片：source/target 任一不在本库 → 跳过关系写、不报错（applyRelationShiftTx 内 SELECT 1 判存在）。
			if _, err := service.applyRelationShiftTx(ctx, tx, edgeSource, req.ActorID, tmpl.Delta, "七种交互·"+tmpl.Reason); err != nil {
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

// ExpireStaleConsents 把创建早于 cutoff 仍 pending 的请求置 expired，并在置 expired 前按档兜底回应（§2.5 超时分支）：
//   - 层3(REQUIRES_CONSENT) 失效 → 给**发起方 B** 投一张回响卡「她的命没等到回应」（ReasonCrossConsentTimeout）；
//   - 层2(CONTESTED) 失效 → 按 A 的离线宪章兜底自治回应（autonomousConsentAccepts：归因成立则接受应用效果，否则隐忍）。
//
// 先逐条读出将超时的 pending 行做按档兜底（best-effort 旁路，单条失败只吞错），再批量置 expired——兜底与置 expired 解耦，
// 即便某条兜底失败也仍会被置 expired（避免无限挂起）。返回置 expired 数（与原语义一致，调用方不感知兜底细节）。
func (service *Service) ExpireStaleConsents(ctx context.Context, cutoff string) (int64, error) {
	// 公开入口：无作用域（scopeSessionID=""）——层2 兜底自治接受不设 session 谓词（全局，保留原公开语义/ops 与现有用例）。
	// 边界结算请走 expireStaleConsentsScoped(传 state.ID) 以遵守跨玩家硬不变量（只替本 session 所辖 target 接受）。
	return service.expireStaleConsentsScoped(ctx, cutoff, "")
}

// expireStaleConsentsScoped 是 ExpireStaleConsents 的作用域版：把创建早于 cutoff 仍 pending 的请求按档兜底后批量置 expired。
// scopeSessionID 非空时（边界结算路径）：层2 兜底**自治接受**（走 ResolveConsentRequest→只写**接受方自己 owner 一侧**的出边
// source=target→actor）只对 **target 属本 session（units.session_id=scopeSessionID）** 的 pending 触发，绝不替别局离线 A 接受
// 而越界写他人 session 的关系（跨玩家硬不变量，HIGH）。scopeSessionID 空时（公开/ops 路径）无 session 谓词，保留原全局语义。
// ② 批量置 expired 保持全局（仅改本表 consent_requests.status，不写他人 units/relations/memory，不违反不变量），
// 避免别局超 TTL 的 pending 永挂（无限堆积）。返回置 expired 数（与原语义一致）。
func (service *Service) expireStaleConsentsScoped(ctx context.Context, cutoff, scopeSessionID string) (int64, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("expire consents: missing db")
	}
	// ① 先读出将超时的 pending 行，逐条按档兜底回应（在置 expired 之前——层2 兜底接受需 status 仍 pending 才能 flip）。
	stale, err := service.listStalePendingConsents(ctx, cutoff)
	if err == nil {
		for _, req := range stale {
			service.fallbackOnConsentTimeout(ctx, req, scopeSessionID)
		}
	} else {
		slog.Warn("list stale consents for timeout fallback failed (best-effort)", "err", err)
	}
	// ② 批量置 expired（剩余仍 pending 的——层2 已被兜底接受的此时不再 pending，自然不被覆盖）。全局：仅改本表 status。
	res, err := service.db.ExecContext(ctx,
		`UPDATE consent_requests SET status = 'expired', resolved_at = ? WHERE status = 'pending' AND created_at < ?`,
		nowConsentTS(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("expire consents: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// sweepStaleConsentsTimeoutOnly 是「全局安全」的超时清理：把超 TTL 的 pending 置 expired，并对**层3** 给发起方 B 投回响卡，
// 但**绝不**跑层2 的自治接受（那会写关系，须由 target 自己 session 边界驱动）。这道清理本身**只 UPDATE consent_requests.status +
// 经 SurfaceFateEvent 写发起方自己收件箱**，绝不写任何他人 units/relations——故无 session 作用域、可全局低频跑（修复 major-3）。
//
// 解决的缺口：层2/3 pending 的「自治回应/超时兜底」原本只在 **B 自己的部署边界** 触发（settleConsentsAtBoundary，作用域=B.state.ID）。
// 一个**真离线、从不推进 session** 的 B，其 pending 在默认 flag（FATE_AUTOTICK 关）下既不被自治回应、也不被超时清理，无限挂起。
// 本 sweep 由独立后台 loop 驱动（不依赖 FATE_AUTOTICK / 不依赖 B 上线），保证即便 B 永不上线，层3 pending 也按 72h 失效 +
// 给发起方投回响卡，层2/3 pending 不会永挂。层2 的「据归因自治接受」仍留给 target 自己 session 边界（写关系须本侧驱动）。
//
// nowTS 注入「现在」（确定性可复现）；cutoff = now - consentTTL。返回置 expired 数。best-effort：单条投卡失败只吞错。
func (service *Service) sweepStaleConsentsTimeoutOnly(ctx context.Context, nowTS string) (int64, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("sweep consents: missing db")
	}
	now, err := time.Parse(consentTimeLayout, strings.TrimSpace(nowTS))
	if err != nil {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-consentTTL).UTC().Format(consentTimeLayout)
	// ① 先对将超时的层3 pending 给发起方投回响卡（SurfaceFateEvent 落发起方本库收件箱，不写他人 relations）。层2 不在此处理。
	if stale, lerr := service.listStalePendingConsents(ctx, cutoff); lerr == nil {
		for _, req := range stale {
			if relevance.ConsentTier(req.Tier) == relevance.RequiresConsent {
				service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateTimeout)
				service.surfaceConsentTimeoutEchoToInitiator(ctx, req)
			}
		}
	} else {
		slog.Warn("sweep: list stale consents failed (best-effort)", "err", lerr)
	}
	// ② 全局置 expired（仅改本表 status，绝不写他人 units/relations）：层2/3 超 TTL 的剩余 pending 一律失效，避免永挂。
	res, err := service.db.ExecContext(ctx,
		`UPDATE consent_requests SET status = 'expired', resolved_at = ? WHERE status = 'pending' AND created_at < ?`,
		nowConsentTS(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("sweep expire consents: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RunConsentExpirySweepLoop 是**独立于 FATE_AUTOTICK** 的后台低频 sweep：周期跑 sweepStaleConsentsTimeoutOnly，
// 保证「真离线、从不上线」的 B 的层2/3 pending 也按 72h TTL 失效 + 层3 给发起方投回响卡（修复 major-3，§2.5 超时兜底落地）。
// 与 RunFateAutoTickLoop 的区别：本 loop **无 flag 门控、恒开**（默认即跑），因为它只做全局安全操作（置 expired + 发起方自己收件箱投卡），
// 绝不写任何他人 units/relations、绝不跑 LLM——成本极低，可常驻。层2「据归因自治接受」仍由 target 自己 session 边界驱动（不在此 loop）。
// 随 ctx 取消优雅退出（与 region-runner / fate-autotick 同模式）。interval ≤0 时取默认 10min（低频，TTL=72h 远大于此，无精度损失）。
func (service *Service) RunConsentExpirySweepLoop(ctx context.Context, interval time.Duration) {
	if service == nil {
		return
	}
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() { _ = recover() }() // best-effort：异常不拖垮 sweep loop
				if _, err := service.sweepStaleConsentsTimeoutOnly(ctx, nowConsentTS()); err != nil {
					slog.Warn("consent expiry sweep pass failed (best-effort)", "err", err)
				}
			}()
		}
	}
}

// listStalePendingConsents 读出创建早于 cutoff 仍 pending 的同意请求（供超时按档兜底逐条处理）。双方言安全：参数化字典序比较。
func (service *Service) listStalePendingConsents(ctx context.Context, cutoff string) ([]ConsentRequest, error) {
	rows, err := service.db.QueryContext(ctx,
		`SELECT `+consentCols+` FROM consent_requests WHERE status = 'pending' AND created_at < ? ORDER BY created_at ASC`, cutoff)
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

// targetInSession 判某 consent target 是否属指定 session（units.session_id=scopeSessionID）。供层2 兜底自治接受的作用域门用：
// 只对本 session 所辖 target 跑兜底接受（绝不越界直写他人 session 的 B 侧 relations，HIGH）。best-effort：查不到/跨分片=不属本 session。
func (service *Service) targetInSession(ctx context.Context, targetID, scopeSessionID string) bool {
	if service == nil || service.db == nil || strings.TrimSpace(targetID) == "" || strings.TrimSpace(scopeSessionID) == "" {
		return false
	}
	var n int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM units WHERE id = ? AND session_id = ?`, targetID, scopeSessionID).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// fallbackOnConsentTimeout 对一条即将超时失效的 pending 同意请求按档兜底（§2.5 超时分支）。best-effort 旁路：单条失败只吞错。
//   - 层3(REQUIRES_CONSENT)：A 长期不上线 → 自动失效 + 给**发起方 B** 投回响卡「她的命没等到回应」；consent_state→timeout。
//     回响卡走 B 自己的命运层（SurfaceFateEvent 落 B 本库的 append-only 事件，不写他人 relations），无作用域之虞、永远跑。
//   - 层2(CONTESTED)：按 A 的离线宪章兜底**自治接受**——接受会经 ResolveConsentRequest→applyRelationShiftTx 写**接受方自己
//     owner 一侧**的出边（source=target=接受方 → target=actor=发起方），故须作用域门（HIGH）：scopeSessionID 非空时，只对
//     **target 属本 session(units.session_id=scopeSessionID)** 的 pending 跑兜底接受；不属本 session（别局/跨分片）→ 隐忍
//     （留待该 target 自己的 session 边界结算/超时），绝不替别局 target 写其本侧出边。
//     scopeSessionID 空（公开/ops 路径）= 无 session 谓词，保留原全局兜底语义。归因成立则接受应用效果，否则隐忍；
//     接受=consent_state accepted，隐忍=declined（仍由 ② 批量置 expired）。
func (service *Service) fallbackOnConsentTimeout(ctx context.Context, req ConsentRequest, scopeSessionID string) {
	if service == nil {
		return
	}
	switch relevance.ConsentTier(req.Tier) {
	case relevance.RequiresConsent:
		service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateTimeout)
		service.surfaceConsentTimeoutEchoToInitiator(ctx, req)
	case relevance.Contested:
		// 作用域门（HIGH）：有作用域且 target 不属本 session（别局/跨分片）→ 不替它兜底接受（绝不越界写他人 B 侧 relations），
		// 留 pending 待其本库 session 结算（仍会被 ② 全局置 expired 清理，不会永挂）。scopeSessionID 空保留原全局语义。
		if strings.TrimSpace(scopeSessionID) != "" && !service.targetInSession(ctx, req.TargetID, scopeSessionID) {
			return
		}
		// 层2 兜底自治回应：A 不在本库或归因不成立 → 隐忍（留待 ② 置 expired）。
		if service.units == nil {
			service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateDeclined)
			return
		}
		target, err := service.units.GetByID(ctx, req.TargetID)
		if err != nil || target.ID == "" {
			service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateDeclined)
			return
		}
		if !service.autonomousConsentAccepts(ctx, &target, req) {
			service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateDeclined)
			return
		}
		if _, err := service.ResolveConsentRequest(ctx, req.ID, true); err != nil {
			service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateDeclined)
			return
		}
		service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateAccepted)
	default:
		// 层1 不应进 consent_requests（直接成立），保守不处理。
	}
}

// surfaceConsentTimeoutEchoToInitiator 给发起方 B（req.ActorID）投一张回响卡「她的命没等到回应」（ReasonCrossConsentTimeout，§2.5）。
// 经 SurfaceFateEvent 走 B 的命运层（祖魂语气，克制）。best-effort 旁路：B 跨分片/不在本库/SurfaceFateEvent 失败→只吞错。
// 关联单位 ActorID=A（对方，仅入 payload/措辞）、TargetID=B（她本人，走自相关路径落 B 本库真实行）。
func (service *Service) surfaceConsentTimeoutEchoToInitiator(ctx context.Context, req ConsentRequest) {
	if service == nil || service.db == nil || service.units == nil {
		return
	}
	initiator, err := service.units.GetByID(ctx, req.ActorID)
	if err != nil || initiator.ID == "" {
		return // B 跨分片/不在本库：回响是 B 侧本地体验，best-effort 跳过
	}
	tmpl, ok := sevenTemplates[SevenInteraction(req.Interaction)]
	deedZH := "一桩事"
	if ok {
		deedZH = tmpl.Reason
	}
	ev := FateEvent{
		ActorID:       req.TargetID, // 对方=没回应的 A
		TargetID:      req.ActorID,  // 她本人=发起方 B
		ReasonCode:    events.ReasonCrossConsentTimeout,
		Importance:    5,
		EmotionWeight: -0.3,
		Summary:       "他朝那人递出的那桩事（" + deedZH + "），终究没等到回应。",
		AttributionZH: "她的命，没等到回应。",
	}
	if _, err := service.SurfaceFateEvent(ctx, initiator.SessionID, &initiator, ev); err != nil {
		slog.Warn("surface consent timeout echo to initiator failed (best-effort)", "initiator", req.ActorID, "err", err)
	}
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

// autoResolveConsentsByCharter 让 A 的角色「上线/回合边界由她的命按归因自治回应」（§2.5 升级）：玩家不在场时，
// 对每条 pending 同意请求经 **decision+attribution 管线** 自治决定是否接受——归因必须落到「对 B 的 relation 恶化/亲近」
// 或现实 pressure（饥饿/威胁/债务/受伤/疲劳）这类真实前因，否则 OOC 回退保守隐忍（不接受、留待人工/超时）。
// 这取代了原先「纯中文子串匹配宪章」的放行：substring 命中只作 fast-path 显式授权（玩家明写「可代我结盟」仍直接接受），
// 但即便无显式 mandate，A 的角色也能在归因成立时自治回应——「B 改变的是处境，A 的角色仍有自己的命、自己的怕、自己的选」。
//
// 接受经 ResolveConsentRequest 在单事务内 flip + 应用**接受方自己 owner 一侧**关系效果（source=target→actor，绝不替发起方
// 写其 outgoing relations、绝不直写发起方的 units）；隐忍把 consent_state 记 declined（留 pending 待超时按档兜底）。
// 全程 best-effort：单条失败只吞错跳过、绝不中断；返回自动「接受」的条数。
// state 为 nil 或无该单位时安全返回 0（无 state 锚/宪章/记忆上下文，归因无从解析，一律保守隐忍）。
func (service *Service) autoResolveConsentsByCharter(ctx context.Context, state *State, targetID string) (int, error) {
	if service == nil || service.db == nil || state == nil || strings.TrimSpace(targetID) == "" {
		return 0, nil
	}
	pending, err := service.ListPendingConsents(ctx, targetID)
	if err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}
	charter, _ := GetUnitCharter(state, targetID) // 无宪章=空，mandate fast-path 自然不命中
	// 载入 A 的角色记录构造归因快照（人格+压力+对 B 关系四轴）。不在本库（跨分片）→ 无从自治，全部保守隐忍。
	target, err := service.units.GetByID(ctx, targetID)
	if err != nil || target.ID == "" {
		return 0, nil
	}
	accepted := 0
	for _, req := range pending {
		tmpl, known := sevenTemplates[SevenInteraction(req.Interaction)]
		if !known {
			continue
		}
		// fast-path：玩家显式授权过该交互（「可代我结盟」），尊重玩家意志直接接受（仍排除否定型禁令）。
		if mandateAllowsInteraction(charter.SocialMandates, tmpl.Reason) {
			if _, err := service.ResolveConsentRequest(ctx, req.ID, true); err != nil {
				continue
			}
			service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateAccepted)
			accepted++
			continue
		}
		// 无显式授权 → 过 decision+attribution 管线自治回应：归因成立才接受，否则保守隐忍。
		if !service.autonomousConsentAccepts(ctx, &target, req) {
			// 隐忍：留 pending（待超时按档兜底），把同意档记 declined（可仲裁注记，不应用任何效果）。
			service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateDeclined)
			continue
		}
		if _, err := service.ResolveConsentRequest(ctx, req.ID, true); err != nil {
			continue // best-effort：并发已处理/关系写失败只跳过，不中断其余请求。
		}
		service.bestEffortUpdateConsentState(ctx, req.EventID, consentStateAccepted)
		accepted++
	}
	return accepted, nil
}

// autonomousConsentAccepts 用 decision+attribution 管线判定 A 的角色是否自治接受一条 pending 同意请求（§2.5）。
// 接受的前因必须真实可解析（与 attribution.ValidateAttribution 同口径）：要么对 B 有显著关系四轴（亲近/敌视都「有关」，
// 牵动她回应），要么她正背负现实压力（pressure），否则判 OOC → 返回 false（保守隐忍，不替无源的她做不可逆决定）。
// 纯逻辑 + 一次关系读：构造 A 的归因快照（人格/压力/对 B 关系），合成一条「她为何回应这缕命」的候选归因交给
// decision.ValidateAttribution 硬校验。校验通过=有源回应=接受；不过=无源=隐忍。确定性、可测、best-effort（出错=隐忍）。
func (service *Service) autonomousConsentAccepts(ctx context.Context, target *unit.Record, req ConsentRequest) bool {
	if service == nil || target == nil || target.ID == "" {
		return false
	}
	snap := buildAttributionSnapshot(target)
	// 把 A 对发起方 B 的关系四轴填入快照（归一到 [-1,1]，与 withRelations 同口径），使 relation 类前因可解析。
	snap = withRelations(ctx, service, target.ID, snap)
	attr, ok := consentResponseAttribution(req.ActorID, snap)
	if !ok {
		return false // 既无显著关系、也无现实压力 → 无源 → 隐忍
	}
	verdict := decision.ValidateAttribution(attr, snap)
	return verdict.OK
}

// consentResponseAttribution 为「A 自治回应 B 的跨玩家交互」合成一条候选归因（§2.5：归因落 relation 恶化或 pressure）。
// 优先用 A 对 B 的显著关系四轴（relation 类前因，最贴合「另一缕命撞上她的命」）；无显著关系则退而用现实压力位（pressure 类）。
// 两者皆无 → 返回 false（无源，调用方据此判隐忍）。SurpriseLevel 取 1（回应一桩落到自己头上的事并不算「无前因的戏剧性意外」，
// 不触发 ValidateAttribution 规则3 的强支撑要求）。纯函数、确定性、可测。
func consentResponseAttribution(initiatorID string, snap decision.Snapshot) (decision.Attribution, bool) {
	if axes, ok := snap.Relations[initiatorID]; ok {
		if absf64(axes.Trust) >= consentRelAxisMin || absf64(axes.Affection) >= consentRelAxisMin ||
			absf64(axes.Fear) >= consentRelAxisMin || absf64(axes.Rivalry) >= consentRelAxisMin {
			return decision.Attribution{
				Primary:       decision.CauseRef{Kind: decision.CauseRelation, RefID: initiatorID, Weight: 0.7},
				SurpriseLevel: 1,
				NarrativeZH:   "她的命撞上了另一缕命，她照自己与那人的恩怨回应。",
			}, true
		}
	}
	// 关系不显著 → 看现实压力位（饥饿/威胁/债务/受伤/疲劳任一在线即可作为回应的前因）。
	for _, name := range []string{"threat", "debt", "injury", "hunger", "fatigue"} {
		if pressureFlagActive(name, snap.Pressure) {
			return decision.Attribution{
				Primary:       decision.CauseRef{Kind: decision.CausePressure, RefID: name, Weight: 0.6},
				SurpriseLevel: 1,
				NarrativeZH:   "她正背着自己的难处，这桩事她得照自己的处境回应。",
			}, true
		}
	}
	return decision.Attribution{}, false
}

// consentRelAxisMin 是「A 对 B 关系是否显著到足以作为回应前因」的门槛（与 attribution.relAxisMin 同口径 0.3，归一后量级）。
const consentRelAxisMin = 0.3

// pressureFlagActive 判某个压力位是否在线（与 attribution 包的 pressureActive 同义，本包内自包含一份，避免跨包导出）。
func pressureFlagActive(name string, p decision.PressureFlags) bool {
	switch name {
	case "hunger":
		return p.Hunger
	case "threat":
		return p.Threat
	case "debt":
		return p.Debt
	case "injury":
		return p.Injury
	case "fatigue":
		return p.Fatigue
	default:
		return false
	}
}

// absf64 是本包内的浮点绝对值（避免与同包其它文件的 absFloat 撞名/跨包导出）。
func absf64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
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
// 先做超时兜底 expireStaleConsents（必须：把超 TTL 的 pending 清成 expired，避免无限挂起 + 按档投回响/宪章兜底），
// 再对每个有 pending 同意请求的离线 A「由她的角色按归因自治回应」（§2.5 升级：过 decision+attribution 管线，
// 不再仅限有宪章 mandate 的单位——substring 命中只作显式授权 fast-path，无 mandate 的 A 也能在归因成立时自治回应）。
// 全程 best-effort：任一步失败只记日志、绝不中断回合推进（对齐自治结算「吞错不阻塞」约定）。
// nowTS 用注入的回合边界时刻（确定性可复现）；state 为 nil 时安全 no-op。
func (service *Service) settleConsentsAtBoundary(ctx context.Context, state *State) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	// ① 超时兜底（必须）：以回合边界时刻为「现在」清掉超 TTL 的 pending（内含层3 回响卡 / 层2 宪章兜底）。
	//    传本 session 作用域 state.ID——层2 兜底自治接受（走 ResolveConsentRequest→只写**接受方自己 owner 一侧**出边
	//    source=target→actor）只对 **本 session 所辖 target** 触发，绝不替别局离线 A 接受而越界写他人 session 的关系
	//    （跨玩家硬不变量，HIGH）。expire 清理本身（不写他人 relations）保持全局，避免别局 pending 永挂。
	if _, err := service.expireStaleConsentsScoped(ctx, nowConsentTS(), state.ID); err != nil {
		appendLog(state, "consent", fmt.Sprintf("同意请求超时兜底失败（best-effort，已跳过）：%v", err), "", "")
	}
	// ② 自治回应（§2.5 升级）：遍历**本 session 所辖**且仍有 pending 的目标 A（不限有无宪章），逐个过归因管线自治回应——
	//    归因成立（对 B 显著关系/现实压力）→ 接受应用本侧效果；无源 → 保守隐忍（留待人工/超时）。
	//    作用域门（listPendingConsentTargets JOIN units.session_id=state.ID）确保：别局 session 推进边界绝不替本局离线 A
	//    自治接受、绝不越界直写 B 侧 relations（与 surfaceCrossEventsAtBoundary 的 WorldID 空早返 + 本阵营过滤同源对齐）。
	for _, targetID := range service.listPendingConsentTargets(ctx, state.ID) {
		if _, err := service.autoResolveConsentsByCharter(ctx, state, targetID); err != nil {
			appendLog(state, "consent", fmt.Sprintf("单位 %s 自治回应结算失败（best-effort，已跳过）：%v", targetID, err), "", "")
		}
	}
}

// listPendingConsentTargets 列出**本 session(scopeSessionID) 所辖**且仍有 pending 同意请求的去重目标单位（A 方）。
// 作用域门（跨玩家硬不变量，HIGH）：JOIN units 限 units.session_id=scopeSessionID——只让本 session 边界结算替**自己所辖**的
// 离线 A 自治回应（接受走 ResolveConsentRequest→只写**接受方自己 owner 一侧**出边 source=target→actor），绝不替别局 A
// 越界写他人 session 的关系。
// scopeSessionID 空 → 返回空切片（无作用域=不替任何人自治回应，保守，与 surfaceCrossEventsAtBoundary 的 WorldID 空早返同源）。
// target 跨分片（不在本库 units 行）自然被 JOIN 排除（其本库 session 才是合法结算方）。best-effort：出错返回空切片（不阻断边界结算）。
func (service *Service) listPendingConsentTargets(ctx context.Context, scopeSessionID string) []string {
	if service == nil || service.db == nil || strings.TrimSpace(scopeSessionID) == "" {
		return nil
	}
	rows, err := service.db.QueryContext(ctx,
		`SELECT DISTINCT cr.target_unit_id
		   FROM consent_requests cr
		   JOIN units u ON u.id = cr.target_unit_id
		  WHERE cr.status = 'pending' AND u.session_id = ?`, scopeSessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return out
		}
		if strings.TrimSpace(id) != "" {
			out = append(out, id)
		}
	}
	return out
}
