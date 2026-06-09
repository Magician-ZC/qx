// 文件说明：GM 世界事件注入（设计 docs/产品方案PRD.md §8 live-ops 手柄）。
// 运营在后台往某个活世界投一条权威跨事件——譬如「天灾」「外敌压境」「集市丰年」——它和玩家自发的跨事件
// 走完全相同的 append-only 总线（cross_events），用同一个世界时钟 tick 排序，因此能被命运层正常路由、被审计正常复算。
// 与玩家事件唯一的区别：每次 GM 注入额外写一条 gm_events_audit（谁、何时、注入了什么的全量 payload），可仲裁、可追责。
//
// 原子性：发号（AdvanceTick）+ 写 cross_events + 写审计必须在一个事务里，避免「发了号但事件没落库」或「事件落了但审计丢了」。
// MySQL 下 AdvanceTick 走 SELECT...FOR UPDATE 必须在事务内（否则并发注入会撞号），故这里一律包事务（双驱动统一）。

package liveops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

// KindGMWorldEvent 是 GM 注入的世界事件在 cross_events 里的统一 event_kind。
// 用单一 kind + payload.gm_kind 细分（天灾/外敌/丰年…），让命运层/审计无需枚举每种运营事件即可识别「这是 GM 注入的」。
const KindGMWorldEvent worldbus.EventKind = "GM_WORLD_EVENT"

// GMEvent 是一次 GM 世界事件注入的入参。
type GMEvent struct {
	WorldID    string         // 目标世界（必填，必须存在且为 active）
	Kind       string         // GM 事件细分（天灾/外敌/丰年…），落 payload.gm_kind；空则记 "generic"
	ActorID    string         // 可空：事件「发起方」角色（如某 NPC 势力的代表），多数 GM 事件无 actor
	TargetID   string         // 可空：事件「受体」角色/区域
	RegionID   string         // 可空：事件波及的区域
	Importance int            // 重要度（喂命运层路由阈值；夹到 [0,10]）
	CreatedBy  string         // GM 身份（运营账号/令牌指纹），写进审计，必填语义上但空则记 "unknown"
	Payload    map[string]any // 业务 payload（叙事文案/数值参数…），整块进 cross_events + 审计
}

// GMEventResult 是一次注入的结果回执。
type GMEventResult struct {
	CrossEventID string `json:"cross_event_id"`
	AuditID      string `json:"audit_id"`
	WorldTick    int    `json:"world_tick"`
}

// EmitWorldEvent 注入一条 GM 世界事件：原子发号 → 写 cross_events → 写 gm_events_audit。
// 失败时整事务回滚（不会留下「发了号没事件」或「有事件没审计」的半成品）。
// 守卫：服务未就绪 / 世界不存在或已封存 → 返回错误（运营端需可见，故不吞）。
func (s *LiveopsService) EmitWorldEvent(ctx context.Context, ev GMEvent) (GMEventResult, error) {
	if !s.ready() {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: service not ready")
	}
	if strings.TrimSpace(ev.WorldID) == "" {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: empty world_id")
	}

	// 校验世界存在且未封存（封存世界只读，不接受新事件）。
	w, err := world.Get(ctx, s.db, ev.WorldID)
	if err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: load world: %w", err)
	}
	if w.Status == world.StatusSealed {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: world %s is sealed (read-only)", ev.WorldID)
	}

	importance := clampImportance(ev.Importance)
	gmKind := strings.TrimSpace(ev.Kind)
	if gmKind == "" {
		gmKind = "generic"
	}
	createdBy := strings.TrimSpace(ev.CreatedBy)
	if createdBy == "" {
		createdBy = "unknown"
	}

	// 组装 payload：把 GM 细分 kind 与发起人嵌入 payload，让命运层/审计可见「这是谁注入的什么」。
	payload := map[string]any{}
	for k, v := range ev.Payload {
		payload[k] = v
	}
	payload["gm_kind"] = gmKind
	payload["gm_injected"] = true
	payload["created_by"] = createdBy
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: marshal payload: %w", err)
	}

	// 事务内：发号 + 写总线 + 写审计三者要么全成功要么全回滚。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// 事务内重检世界状态（堵 TOCTOU）：L59-65 的事务外预检只作 fast-fail，无法防住「预检通过后、发号前」
	// 被并发 FinalizeSeason 封存的竞态。这里在事务内对 worlds 行做带锁状态读——MySQL 用 SELECT ... FOR UPDATE
	// 与 AdvanceTick 的行锁对齐、阻塞并发封存；SQLite 单 writer，事务内 SELECT status 即拿到串行化后的最新态。
	// 若已 sealed 则回滚返回 sealed 错（与事务外预检同语义，运营端可见）。
	sealed, err := isWorldSealedTx(ctx, tx, ev.WorldID, s.dialect)
	if err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: recheck world status: %w", err)
	}
	if sealed {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: world %s is sealed (read-only)", ev.WorldID)
	}

	tick, err := world.AdvanceTick(ctx, tx, ev.WorldID, s.dialect)
	if err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: advance tick: %w", err)
	}

	crossID, err := worldbus.Append(ctx, tx, worldbus.CrossEvent{
		WorldID:    ev.WorldID,
		ActorID:    strings.TrimSpace(ev.ActorID),
		TargetID:   strings.TrimSpace(ev.TargetID),
		Kind:       KindGMWorldEvent,
		RegionID:   strings.TrimSpace(ev.RegionID),
		Importance: importance,
		WorldTick:  tick,
		Payload:    payload,
	})
	if err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: append cross event: %w", err)
	}

	auditID, err := writeAudit(ctx, tx, auditRecord{
		WorldID:      ev.WorldID,
		EventKind:    gmKind,
		CrossEventID: crossID,
		WorldTick:    tick,
		PayloadJSON:  string(payloadJSON),
		CreatedBy:    createdBy,
	})
	if err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: write audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return GMEventResult{}, fmt.Errorf("liveops emit world event: commit: %w", err)
	}
	committed = true

	// 提交后 best-effort 广播一条世界级轻通知（不携带敏感 payload，吞错）。
	if s.broadcaster != nil {
		s.broadcaster.BroadcastWorldNotice(ev.WorldID, "gm_world_event", fmt.Sprintf("一桩%s降临此世。", gmKind))
	}

	return GMEventResult{CrossEventID: crossID, AuditID: auditID, WorldTick: tick}, nil
}

// auditRecord 是 gm_events_audit 的一行。
type auditRecord struct {
	WorldID      string
	EventKind    string
	CrossEventID string
	WorldTick    int // 注入时的权威世界时钟（与 cross_events.world_tick 同源；ListAudit 据此稳定排序）
	PayloadJSON  string
	CreatedBy    string
}

// writeAudit 把一条 GM 注入审计写进 gm_events_audit（append-only）。execer 可为 *sql.DB 或 *sql.Tx。
// 写入 world_tick（EmitWorldEvent 内 AdvanceTick 后已知），使 ListAudit 能按权威单调 tick 稳定排序。
func writeAudit(ctx context.Context, execer execer, rec auditRecord) (string, error) {
	id := uuid.NewString()
	if _, err := execer.ExecContext(ctx, `
		INSERT INTO gm_events_audit (id, world_id, event_kind, cross_event_id, world_tick, payload_json, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, rec.WorldID, rec.EventKind, rec.CrossEventID, rec.WorldTick, rec.PayloadJSON, rec.CreatedBy); err != nil {
		return "", fmt.Errorf("insert gm audit: %w", err)
	}
	return id, nil
}

// isWorldSealedTx 在事务内对 worlds 行做带锁状态读，返回该世界是否已封存。
// MySQL：SELECT status ... FOR UPDATE 加行锁，与并发 FinalizeSeason 的封存写互斥（堵 TOCTOU）。
// SQLite：单 writer，事务内裸 SELECT status 即拿串行化后的最新态（SQLite 不支持 FOR UPDATE，不能带该后缀）。
// 世界不存在时返回 world.ErrNotFound 的包装错（与事务外预检一致：注入不存在的世界应失败）。
func isWorldSealedTx(ctx context.Context, tx *sql.Tx, worldID string, dialect dbdialect.Dialect) (bool, error) {
	query := `SELECT status FROM worlds WHERE id = ?`
	if dialect == dbdialect.DialectMySQL {
		query = `SELECT status FROM worlds WHERE id = ? FOR UPDATE`
	}
	var status string
	if err := tx.QueryRowContext(ctx, query, worldID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("world %s not found: %w", worldID, world.ErrNotFound)
		}
		return false, fmt.Errorf("lock world status: %w", err)
	}
	return world.Status(status) == world.StatusSealed, nil
}

// AuditEntry 是 gm_events_audit 的一条只读记录（运营查询用）。
type AuditEntry struct {
	ID           string `json:"id"`
	WorldID      string `json:"world_id"`
	EventKind    string `json:"event_kind"`
	CrossEventID string `json:"cross_event_id"`
	WorldTick    int    `json:"world_tick"` // 注入时的权威世界时钟（排序键；与 cross_events 注入序严格一致）
	PayloadJSON  string `json:"payload_json"`
	CreatedBy    string `json:"created_by"`
	CreatedAt    string `json:"created_at"`
}

// ListAudit 列出某世界的 GM 注入审计，最新在前。limit<=0 取 100。
// 排序用权威单调的 world_tick（同源 cross_events.world_tick），取代秒级/空串的 created_at——
// 同秒多条注入下 created_at 排序不稳，会让运营复核视图与真实注入序错位；world_tick DESC, id DESC 保证严格一致。
func (s *LiveopsService) ListAudit(ctx context.Context, worldID string, limit int) ([]AuditEntry, error) {
	if !s.ready() {
		return nil, fmt.Errorf("liveops list audit: service not ready")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, world_id, event_kind, cross_event_id, world_tick, payload_json, created_by, created_at
		FROM gm_events_audit WHERE world_id = ?
		ORDER BY world_tick DESC, id DESC LIMIT ?`, worldID, limit)
	if err != nil {
		return nil, fmt.Errorf("liveops list audit: %w", err)
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.WorldID, &e.EventKind, &e.CrossEventID, &e.WorldTick, &e.PayloadJSON, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("liveops scan audit: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// execer 是写入所需的最小依赖（*sql.DB 或 *sql.Tx 均满足）。
type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// clampImportance 把重要度夹到 [0,10]。
func clampImportance(v int) int {
	if v < 0 {
		return 0
	}
	if v > 10 {
		return 10
	}
	return v
}
