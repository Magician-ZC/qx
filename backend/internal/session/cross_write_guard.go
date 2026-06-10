package session

// 文件说明：共享世界 Phase 3「跨玩家写红线」的 storage 层强制 + 审计（设计宪法红线 + 事件耦合 §2.1/§5）。
//
// 设计宪法最高红线（必守）：**B 永远只能写一条 cross_event，改不了 A 的 units/relations。**
// 跨玩家交互 = 各自只写本侧 units + 一条共享 cross_event；绝不直写他人 units。
//
// 现有跨玩家路径已遵守该不变量（applyRelationShift 只写 source→target 的本侧 relations 行、insertContestCrossEvent 只 append
// cross_events、recordContestConsolation 有 loser.SessionID 守卫）。本文件补的是**「越界直写他人 units / 他人 outgoing relations」
// 在代码层被挡**的缺口（§5 风险 6「无 storage 层断言强制」）：
//
//   1) storage 层硬断言（units 整记录写）：unit.Repository.SaveOwnedBy(record, ownerSessionID)——只在「已落库 session_id ==
//      操作方 sessionID」时才落盘整记录 units 行，否则返回 unit.ErrCrossSessionWrite 拒绝写。让「直写他人 units」物理上写不进去。
//   2) session 层显式校验 + 审计（units 写）：assertOwnSideUnitWrite——若跨玩家结算路径**确需**整记录写一个单位，必须先调它，
//      不属本 session 即拒绝并落审计（CROSS_WRITE_DENIED）。saveOwnSideUnit 是把 1)+2) 合一的带审计写门面。
//   3) 关系写红线断言（outgoing relations 写，**已真实武装**）：assertOwnSideRelationWrite——consent accept 在写 `source→target`
//      关系前调它，钉死「relation source == 该次结算的接受方（consent.target_unit_id），绝不写反成发起方出边」，杜绝
//      「接受方 session 改写发起方一侧 outgoing relations」越界（2026-06-10 修复 major-1/major-2：consent accept 已改写
//      **接受方自己 owner 一侧**出边 source=接受方→发起方，并以此断言把关写、方向写反即拒 + 落审计）。
//
// 取向说明（避免「装好没接线」的误导）：跨玩家**绝大多数**路径根本不 units.Save 他人单位（只写本侧 relations + append cross_event），
// 故 1)/2)（units 整记录写断言）是**为 Phase 4「共享 NPC / 整记录写」预置的纵深护栏**，Phase 3 当前确无 units.Save(peer) 调用点
// （这是设计纪律的结果，不是缺口）；真正在 Phase 3 被武装、守住关系红线的是 3) assertOwnSideRelationWrite。三者都**只读/拒写**，
// 绝不放宽既有 best-effort 语义。中文注释、确定性、flag 无关（红线恒守，不受任何 flag 影响）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// assertOwnSideUnitWrite 判定「在 ownerSessionID 的结算上下文里，是否允许直写 unitID 这个单位的 units 行」。
// 返回 nil = 本侧单位（允许写）；返回非 nil = 越界（他人单位 / 跨分片 / 查不到归属），调用方必须**放弃该单位写**。
//
// 规则（与 unit.Repository.SaveOwnedBy 同口径，但在 session 层先校验，便于带审计 + 早返）：
//   - ownerSessionID 空 → 返回 error（跨玩家结算必须有明确操作方 session；无上下文不放行直写，保守）。
//   - unitID 的已落库 session_id == ownerSessionID → 本侧，放行（nil）。
//   - 不等 / 行不存在（跨分片远端）→ 越界，落审计（CROSS_WRITE_DENIED）+ 返回 ErrCrossSessionWriteDenied。
//
// best-effort 审计：审计写失败不影响「拒绝」判定（拒绝是硬的，审计是可观测增强）。只读 units.session_id，绝不写他人单位。
func (service *Service) assertOwnSideUnitWrite(ctx context.Context, ownerSessionID, unitID, reason string) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("assert own-side write: missing db")
	}
	ownerSessionID = strings.TrimSpace(ownerSessionID)
	unitID = strings.TrimSpace(unitID)
	if ownerSessionID == "" || unitID == "" {
		return fmt.Errorf("assert own-side write: empty owner session or unit id")
	}
	persisted := service.sessionIDForUnit(ctx, unitID) // 只读查 units.session_id（contest.go 既有 helper）
	if strings.TrimSpace(persisted) == ownerSessionID {
		return nil // 本侧单位：允许写。
	}
	// 越界（他人单位 / 跨分片远端 / 查不到）→ 审计 + 拒绝（跨玩家硬不变量）。
	service.auditCrossWriteDenied(ctx, ownerSessionID, unitID, persisted, reason)
	return fmt.Errorf("%w: owner session=%q tried to write unit %q (persisted session=%q, reason=%q)",
		ErrCrossSessionWriteDenied, ownerSessionID, unitID, persisted, reason)
}

// ErrCrossSessionWriteDenied 是 session 层跨玩家写红线被触发的哨兵错误（与 unit.ErrCrossSessionWrite 同义，
// 分层各持一个以便调用方按层匹配）。
var ErrCrossSessionWriteDenied = errors.New("cross-session unit write denied (own-side only invariant)")

// processEventExecer 是 EmitProcessEvent 接受的最小写接口（*sql.DB 与 *sql.Tx 都满足）。
// 让审计可在「已开事务」时复用同一 tx 写——避免在 SQLite 单连接（SetMaxOpenConns(1)）下，持 tx 时再用 service.db
// 另起连接造成自死锁（repository.go:177 同款坑）。
type processEventExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// auditCrossWriteDenied 落一条「越界直写他人 units 被拒」的审计留痕（best-effort 旁路，吞错绝不阻断拒绝判定）。
// 用 service.db（无开放事务上下文时）。**OwnerUnitID 刻意留空（NULL）**：被试图改写的 unitID 是他人单位、
// 可能跨分片无本地 units 行，而 events.actor_unit_id 有 units FK——若把它落进 actor 列会触发 FK 失败。故 unitID 只入
// payload（denied_unit，纯审计文本，不受 FK 约束），SessionID 记操作方 session 供按 session 检索越界企图。
func (service *Service) auditCrossWriteDenied(ctx context.Context, ownerSessionID, unitID, persistedSession, reason string) {
	if service == nil || service.db == nil {
		return
	}
	service.auditCrossWriteDeniedOn(ctx, service.db, ownerSessionID, unitID, persistedSession, reason)
}

// auditCrossWriteDeniedOn 是 auditCrossWriteDenied 的「指定 execer」版：在已开事务里（如 consent accept 的 tx）必须传 tx，
// 否则 SQLite 单连接下持 tx 再用 service.db 写审计会自死锁（关键修复：assertOwnSideRelationWrite 在 tx 内调审计须走 tx）。
// best-effort：吞错，绝不阻断拒绝判定/主结算。
func (service *Service) auditCrossWriteDeniedOn(ctx context.Context, execer processEventExecer, ownerSessionID, unitID, persistedSession, reason string) {
	if service == nil || execer == nil {
		return
	}
	_, _ = events.EmitProcessEvent(ctx, execer, events.ProcessEvent{
		SessionID: ownerSessionID,
		// OwnerUnitID 留空 → events.actor_unit_id 落 NULL，避开他人/跨分片单位无本地 FK 行的插入失败。
		Code:     events.ReasonCrossWriteDenied,
		Category: events.CategoryGovernance,
		Payload: map[string]any{
			"denied_unit":       unitID,
			"owner_session":     ownerSessionID,
			"persisted_session": persistedSession,
			"reason":            reason,
			"invariant":         "own_side_unit_write_only",
		},
	})
}

// assertOwnSideRelationWrite 是「关系写红线」的方向/归属断言：在跨玩家结算（consent accept / 兜底接受）写一条
// `source→target` 关系前，钉死 **写的必须是接受方自己 owner 一侧的出边**（source == 该次结算的接受方=consent.target_unit_id），
// 杜绝「接受方 session 改写发起方一侧 outgoing relations」这道与设计宪法 §2.1「各自 Mutator 只改自己 owner 一侧」相悖的越界
// （2026-06-10 修复 major-2）。
//
// 为何用「source==接受方」纯归属判定、而非「source 是否本库存在」：consent accept 的合法写恒是接受方自己的出边，接受方
// 可能本身在远端分片（target=远端角色）——此时本侧本就不该落任何关系行（由 applyRelationShiftTx 的 FK best-effort 静默跳过），
// 不是越界、不该报错。真正要挡死的是「方向被写反」——即把 source 误传成发起方（actor），那才是改写他人出边。故本断言只校验
// 「调用方把 source 钉成了接受方」这一不变量；远端接受方的 best-effort 跳过仍由下游 applyRelationShiftTx 处理。
//
// 判定：source/acceptor 空 → 拒绝（无明确出边拥有者，保守）；source != acceptor → **拒绝 + 落审计**（CROSS_WRITE_DENIED，
// 这正是「试图写发起方出边」的越界企图，须可追溯）；source == acceptor → 放行。
// **审计写必须走传入的 tx**（不是 service.db）：本断言被 consent accept 在已开事务内调用，SQLite 单连接（SetMaxOpenConns(1)）下
// 持 tx 时再用 service.db 另起连接写审计会自死锁（repository.go:177 同款坑）。tx 为 nil 时退回 service.db（无事务上下文，安全）。
// best-effort 审计：审计写失败不影响拒绝判定。绝不写他人 units/relations——纯归属判定 + 旁路审计。
func (service *Service) assertOwnSideRelationWrite(ctx context.Context, tx *sql.Tx, sourceID, acceptorID, reason string) error {
	if service == nil {
		return fmt.Errorf("assert own-side relation: nil service")
	}
	sourceID = strings.TrimSpace(sourceID)
	acceptorID = strings.TrimSpace(acceptorID)
	if sourceID == "" || acceptorID == "" {
		return fmt.Errorf("assert own-side relation: empty source or acceptor id")
	}
	if sourceID == acceptorID {
		return nil // 出边拥有者 == 接受方：合法本侧出边，放行。
	}
	// source 不是接受方（方向写反/试图改写发起方出边）→ 拒绝 + 审计（越界企图可追溯）。
	// 审计 execer 选择：在事务内（tx 非空）必须用 tx 写，否则 SQLite 单连接持 tx 再用 service.db 写审计会自死锁。
	var execer processEventExecer = service.db
	if tx != nil {
		execer = tx
	}
	service.auditCrossWriteDeniedOn(ctx, execer, "", sourceID, "", reason)
	return fmt.Errorf("%w: relation source %q is not the consent acceptor %q (reason=%q)",
		ErrCrossSessionWriteDenied, sourceID, acceptorID, reason)
}

// AccountOwnsUnit 判定某账号是否拥有某角色单位（玩家自处理 consent 的归属鉴权用，修复 major-4）。
// 归属链：units.session_id → single_player_sessions.account_id —— 一个单位属一个 session，一个 session 绑一个账号。
// 仅当该单位所属 session 的 account_id 严格等于 accountID（且二者非空）时返回 true；查不到/出错/匿名 → false（保守拒绝）。
// 纯只读单条 JOIN 查询、确定性、绝不写任何表。供 httpapi 玩家档 consent 路由「只能列/处理自己角色名下的 pending」用。
func (service *Service) AccountOwnsUnit(ctx context.Context, accountID, unitID string) bool {
	if service == nil || service.db == nil {
		return false
	}
	accountID = strings.TrimSpace(accountID)
	unitID = strings.TrimSpace(unitID)
	if accountID == "" || unitID == "" {
		return false // 匿名/空：保守拒绝（玩家档鉴权必须有明确账号 + 单位）。
	}
	var owner sql.NullString
	err := service.db.QueryRowContext(ctx,
		`SELECT s.account_id
		   FROM units u
		   JOIN single_player_sessions s ON s.id = u.session_id
		  WHERE u.id = ?`, unitID).Scan(&owner)
	if err != nil {
		return false // 查不到归属 session / 单位不存在 / 出错 → 保守拒绝。
	}
	return strings.TrimSpace(owner.String) == accountID
}

// saveOwnSideUnit 是「带本侧归属断言 + 审计」的单位整记录写门面：跨玩家结算路径若**确需**写一个单位 units 行
// （而非只写 relations / 只 append cross_event），必须走它而非裸 repo.Save——它先 assertOwnSideUnitWrite 校验归属、
// 落审计，再委托 unit.Repository.SaveOwnedBy 在 storage 层二次硬守（纵深）。任一层判定越界都拒绝写、返回错误。
//
// 注意：绝大多数跨玩家交互**不应**调用本方法——它们只写本侧 relations（applyRelationShift，源是本侧角色）+ append cross_event，
// 根本不写他人 units。本方法是给「合法的本侧单位整记录写」一道带断言的安全门，并把红线在 storage 显式化。
func (service *Service) saveOwnSideUnit(ctx context.Context, ownerSessionID string, record unit.Record, reason string) error {
	if service == nil || service.units == nil {
		return fmt.Errorf("save own-side unit: missing deps")
	}
	if err := service.assertOwnSideUnitWrite(ctx, ownerSessionID, record.ID, reason); err != nil {
		return err
	}
	return service.units.SaveOwnedBy(ctx, record, ownerSessionID)
}
