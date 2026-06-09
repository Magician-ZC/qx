package session

// 文件说明：编年史物化（双驱动地基）——把散落的世界事件物化成可读的编年史条目落 chronicle_entries 表，
// 供传记 / 分享卡 / 命运 Copilot 取材。三件事：
//  1. recordChronicleEntry：append-only 落一条编年史（dbdialect 处理 SQLite ON CONFLICT vs MySQL INSERT IGNORE
//     方言差异），并 best-effort 旁路一条 CHRONICLE_RECORD 流程事件——这条 events 行就是「回到那一刻」的定位锚点
//     （编年史条目 ↔ 同 turn 的 event_id，供前端点击「回到那一刻」按 turn/event 跳转复盘）。
//  2. listChronicle：按 session（可叠加 unit 过滤）读回编年史，倒序（created_at 定宽布局，字典序即时间序）。
//  3. anchorMoment：「回到那一刻」定位逻辑——按一条编年史条目关联回 turn 上同主角的事件 id 列表。
// 全链路 best-effort 吞错：编年史是叙事派生物，绝不能因写表失败中断主模拟循环。
// 注意：CHRONICLE_RECORD 是流程事件码，不改任何受保护状态字段、不走 status.Mutator（纯流程旁路）。
// 实际写入触发点（战斗击杀 / 生育 / 血仇 / 命运结算等）由 Wire 阶段接入，本文件只提供导出原语。

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/storage/dbdialect"
)

// maxChronicleList 默认读取上限，避免传记/分享卡一次拉爆。
const maxChronicleList = 500

// ChronicleEntry 是一条物化后的编年史条目（与 chronicle_entries 表逐列对应）。
// CreatedAt 用 traceTimeLayout 定宽布局（复用 decision_trace_store.go 的 formatTraceTime），字符串字典序=时间序，双驱动一致。
type ChronicleEntry struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	UnitID    string `json:"unit_id,omitempty"`
	Turn      int    `json:"turn"`
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at,omitempty"`
}

// recordChronicleEntry 把一条编年史条目落 chronicle_entries 表（append-only、双驱动方言安全），并 best-effort
// 旁路一条 CHRONICLE_RECORD 流程事件作为「回到那一刻」的定位锚点。返回写入条目的 id（吞错场景返回空串）。
// best-effort：任何失败都不返回错误、不中断主循环——编年史是叙事派生物，写不进去最多丢一笔记述。
func (service *Service) recordChronicleEntry(ctx context.Context, sessionID, unitID string, turn int, kind, text string) string {
	if service == nil || service.db == nil || sessionID == "" || text == "" {
		return ""
	}
	id := uuid.NewString()
	// formatTraceTime 对零值 time.Time 会回退到 time.Now().UTC()，复用它免再 import time。
	createdAt := formatTraceTime(time.Time{})
	if err := insertChronicleEntry(ctx, service.db, dbdialect.IsMySQL(service.db), ChronicleEntry{
		ID:        id,
		SessionID: sessionID,
		UnitID:    unitID,
		Turn:      turn,
		Kind:      kind,
		Text:      text,
		CreatedAt: createdAt,
	}); err != nil {
		return "" // 吞错：写表失败不影响主循环
	}
	// best-effort 旁路一条流程事件作为「回到那一刻」锚点（同 turn、同主角，供前端 turn/event 定位复盘）。
	// 失败无所谓：编年史条目本身已落库，锚点只是导航增强。
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:     sessionID,
		OwnerUnitID:   unitID,
		RelatedUnitID: unitID,
		Code:          events.ReasonChronicleRecord,
		Category:      events.CategoryLifecycle,
		Payload: map[string]any{
			"chronicle_id": id,
			"kind":         kind,
			"turn":         turn,
		},
		Tick: turn,
	})
	return id
}

// insertChronicleEntry 单条幂等写入（按 id），SQLite 用 ON CONFLICT DO NOTHING、MySQL 用 INSERT IGNORE。
// 抽成独立函数以便测试直接驱动，且与 decision_trace_store.go 的 persist* 风格一致。
func insertChronicleEntry(ctx context.Context, db traceExecer, mysql bool, entry ChronicleEntry) error {
	if entry.ID == "" {
		return fmt.Errorf("chronicle entry: empty id")
	}
	query := `INSERT INTO chronicle_entries (id, session_id, unit_id, turn, kind, text, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`
	if mysql {
		query = `INSERT IGNORE INTO chronicle_entries (id, session_id, unit_id, turn, kind, text, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`
	}
	if _, err := db.ExecContext(ctx, query,
		entry.ID, entry.SessionID, nullableStr(entry.UnitID), entry.Turn, entry.Kind, entry.Text, entry.CreatedAt,
	); err != nil {
		return fmt.Errorf("insert chronicle entry %s: %w", entry.ID, err)
	}
	return nil
}

// listChronicle 按 session 读回编年史条目，倒序（created_at 定宽布局，字典序即时间序）。
// unitID 非空则只读该单位的条目（传记 / 分享卡场景）；为空则读整局（编年史总览 / 命运 Copilot 取材）。
// best-effort：读表失败返回空切片 + 错误，由上层决定是否降级（绝不中断主循环）。
func (service *Service) listChronicle(ctx context.Context, sessionID, unitID string, limit int) ([]ChronicleEntry, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("list chronicle: missing db")
	}
	if sessionID == "" {
		return nil, fmt.Errorf("list chronicle: empty session id")
	}
	if limit <= 0 || limit > maxChronicleList {
		limit = maxChronicleList
	}
	var (
		rows *sql.Rows
		err  error
	)
	if unitID == "" {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id, session_id, unit_id, turn, kind, text, created_at
			FROM chronicle_entries WHERE session_id = ?
			ORDER BY created_at DESC, id DESC LIMIT ?`, sessionID, limit)
	} else {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id, session_id, unit_id, turn, kind, text, created_at
			FROM chronicle_entries WHERE session_id = ? AND unit_id = ?
			ORDER BY created_at DESC, id DESC LIMIT ?`, sessionID, unitID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query chronicle: %w", err)
	}
	defer rows.Close()
	return scanChronicleRows(rows)
}

// scanChronicleRows 把查询结果扫成 ChronicleEntry 切片（unit_id 可空，用 sql.NullString 兜底）。
func scanChronicleRows(rows *sql.Rows) ([]ChronicleEntry, error) {
	out := make([]ChronicleEntry, 0)
	for rows.Next() {
		var (
			entry  ChronicleEntry
			unitID sql.NullString
		)
		if err := rows.Scan(&entry.ID, &entry.SessionID, &unitID, &entry.Turn, &entry.Kind, &entry.Text, &entry.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan chronicle entry: %w", err)
		}
		if unitID.Valid {
			entry.UnitID = unitID.String
		}
		out = append(out, entry)
	}
	return out, rows.Err()
}

// MomentAnchor 是「回到那一刻」的定位结果：一条编年史条目 → 它发生的 turn + 同 turn/同主角的相关事件 id 列表。
// 前端据此把「回到那一刻」链接落到具体回合、并高亮那一刻的事件流（点击事件再展开复盘）。
type MomentAnchor struct {
	ChronicleID string   `json:"chronicle_id"`
	UnitID      string   `json:"unit_id,omitempty"`
	Turn        int      `json:"turn"`
	EventIDs    []string `json:"event_ids,omitempty"`
}

// anchorMoment 实现「回到那一刻」的定位逻辑：给定一条编年史条目，把它关联回它发生的 turn 上同主角的事件 id 列表。
// 定位口径：events 表里 tick=entry.Turn 且 actor/target=entry.UnitID 的事件（unitID 为空则取该 turn 全部事件）。
// best-effort：查不到关联事件不报错，返回只含 turn 的锚点（前端仍可跳到那一回合）；DB 出错才返回错误。
func (service *Service) anchorMoment(ctx context.Context, entry ChronicleEntry) (MomentAnchor, error) {
	anchor := MomentAnchor{ChronicleID: entry.ID, UnitID: entry.UnitID, Turn: entry.Turn}
	if service == nil || service.db == nil {
		return anchor, fmt.Errorf("anchor moment: missing db")
	}
	var (
		rows *sql.Rows
		err  error
	)
	if entry.UnitID == "" {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id FROM events WHERE session_id = ? AND tick = ?
			ORDER BY occurred_at ASC, id ASC LIMIT ?`, entry.SessionID, entry.Turn, maxChronicleList)
	} else {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id FROM events WHERE session_id = ? AND tick = ?
			AND (actor_unit_id = ? OR target_unit_id = ?)
			ORDER BY occurred_at ASC, id ASC LIMIT ?`, entry.SessionID, entry.Turn, entry.UnitID, entry.UnitID, maxChronicleList)
	}
	if err != nil {
		return anchor, fmt.Errorf("anchor moment query: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return anchor, fmt.Errorf("anchor moment scan: %w", err)
		}
		anchor.EventIDs = append(anchor.EventIDs, id)
	}
	return anchor, rows.Err()
}

// ─────────────────────────────────────────────────────────────────────────────
// 读侧导出 API（供 HTTP 路由 / snapshot 消费）
//
// 写侧（recordChronicleEntry）已在实战路径真写（击杀/死亡/继承/升级），但 listChronicle/anchorMoment
// 是包内未导出的低层原语——router 拿不到、snapshot 进不去 → 玩家看不到编年史。本段补「读侧装配层」：
//   - ChronicleView：一条编年史条目 + 它的「回到那一刻」锚点（合并后即可直接渲染时间线 + 跳转链接）。
//   - ChronicleFeed：装配好的一页编年史（条目视图 + 分页游标元信息），供 GET 端点 / 可选并进 snapshot。
//   - Service.ChronicleFeed：导出读侧入口——倒序分页 + 逐条 best-effort 补锚点；这是 router/snapshot 唯一消费口。
//   - Service.ChronicleMomentByID：「回到那一刻」单条端点——按 chronicle_id 反查条目并定位锚点。
// 全部纯读、best-effort（锚点补不上不影响条目本身渲染），确定性（倒序口径与写侧定宽时间布局一致）。
// ─────────────────────────────────────────────────────────────────────────────

// ChronicleView 是一条编年史条目的完整可渲染视图：条目本身 + 它的「回到那一刻」锚点。
// 前端据 Entry 渲染时间线一行，据 Anchor.Turn / Anchor.EventIDs 渲染「回到那一刻」跳转链接 + 事件高亮。
type ChronicleView struct {
	Entry  ChronicleEntry `json:"entry"`
	Anchor MomentAnchor   `json:"anchor"`
}

// ChronicleFeed 是装配好的一页编年史（倒序），带分页游标元信息供前端无限滚动 / 翻页。
// HasMore 为本页是否填满 Limit（满即可能还有下一页）；NextOffset 为下一页起始 offset（HasMore 为假时无意义）。
type ChronicleFeed struct {
	SessionID  string          `json:"session_id"`
	UnitID     string          `json:"unit_id,omitempty"`
	Views      []ChronicleView `json:"views"`
	Limit      int             `json:"limit"`
	Offset     int             `json:"offset"`
	HasMore    bool            `json:"has_more"`
	NextOffset int             `json:"next_offset,omitempty"`
}

// listChroniclePaged 在 listChronicle 倒序口径上叠加 offset 分页（传记 / 分享卡无限滚动用）。
// 多读一条用于判定 HasMore（fetch limit+1，丢弃溢出条），避免再发一次 COUNT 查询。
// best-effort：读表失败返回空切片 + 错误（绝不中断主循环）。offset<0 归零、limit 归默认上限。
func (service *Service) listChroniclePaged(ctx context.Context, sessionID, unitID string, limit, offset int) (entries []ChronicleEntry, hasMore bool, err error) {
	if service == nil || service.db == nil {
		return nil, false, fmt.Errorf("list chronicle paged: missing db")
	}
	if sessionID == "" {
		return nil, false, fmt.Errorf("list chronicle paged: empty session id")
	}
	if limit <= 0 || limit > maxChronicleList {
		limit = maxChronicleList
	}
	if offset < 0 {
		offset = 0
	}
	// 多取一条探测「是否还有下一页」，再裁回 limit；offset 在倒序游标上滑动。
	probe := limit + 1
	var rows *sql.Rows
	if unitID == "" {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id, session_id, unit_id, turn, kind, text, created_at
			FROM chronicle_entries WHERE session_id = ?
			ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, sessionID, probe, offset)
	} else {
		rows, err = service.db.QueryContext(ctx, `
			SELECT id, session_id, unit_id, turn, kind, text, created_at
			FROM chronicle_entries WHERE session_id = ? AND unit_id = ?
			ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, sessionID, unitID, probe, offset)
	}
	if err != nil {
		return nil, false, fmt.Errorf("query chronicle paged: %w", err)
	}
	defer rows.Close()
	all, scanErr := scanChronicleRows(rows)
	if scanErr != nil {
		return nil, false, scanErr
	}
	if len(all) > limit {
		// 命中探测条：确有下一页，裁掉溢出的那一条。
		return all[:limit], true, nil
	}
	return all, false, nil
}

// ChronicleFeed 是读侧导出入口（router / snapshot 唯一消费口）：倒序分页读回一页编年史，并逐条
// best-effort 补「回到那一刻」锚点，装配成可直接渲染的 ChronicleView 列表 + 分页游标。
// unitID 非空 → 只读该单位（传记 / 分享卡）；为空 → 读整局（编年史总览 / 命运 Copilot 取材）。
// best-effort：条目读失败返回空 Feed + 错误（上层降级）；某条锚点补不上只是少了跳转增强，条目仍渲染。
// 确定性：倒序口径与写侧定宽时间布局一致，同输入同输出（无随机、time 仅作墙钟比较）。
func (service *Service) ChronicleFeed(ctx context.Context, sessionID, unitID string, limit, offset int) (ChronicleFeed, error) {
	feed := ChronicleFeed{SessionID: sessionID, UnitID: unitID, Offset: offset}
	if offset < 0 {
		feed.Offset = 0
		offset = 0
	}
	if limit <= 0 || limit > maxChronicleList {
		limit = maxChronicleList
	}
	feed.Limit = limit
	feed.Views = make([]ChronicleView, 0)
	entries, hasMore, err := service.listChroniclePaged(ctx, sessionID, unitID, limit, offset)
	if err != nil {
		return feed, err
	}
	for _, entry := range entries {
		view := ChronicleView{Entry: entry}
		// best-effort 补锚点：失败也保留只含 turn 的降级锚点（anchorMoment 出错时返回的 anchor 已含 turn）。
		anchor, anchorErr := service.anchorMoment(ctx, entry)
		if anchorErr != nil {
			anchor = MomentAnchor{ChronicleID: entry.ID, UnitID: entry.UnitID, Turn: entry.Turn}
		}
		view.Anchor = anchor
		feed.Views = append(feed.Views, view)
	}
	feed.HasMore = hasMore
	if hasMore {
		feed.NextOffset = offset + limit
	}
	return feed, nil
}

// ChronicleMomentByID 实现「回到那一刻」单条端点：按 chronicle_id 在指定 session 内反查条目并定位锚点。
// 供 GET …/chronicle/:chronicleId/moment 直接消费（前端点击某条编年史的「回到那一刻」时调）。
// 找不到该条目 → 返回 found=false（不报错，前端提示「那一刻已无从追溯」）；DB 出错才返回错误。
func (service *Service) ChronicleMomentByID(ctx context.Context, sessionID, chronicleID string) (view ChronicleView, found bool, err error) {
	if service == nil || service.db == nil {
		return ChronicleView{}, false, fmt.Errorf("chronicle moment: missing db")
	}
	if sessionID == "" || chronicleID == "" {
		return ChronicleView{}, false, fmt.Errorf("chronicle moment: empty session or chronicle id")
	}
	row := service.db.QueryRowContext(ctx, `
		SELECT id, session_id, unit_id, turn, kind, text, created_at
		FROM chronicle_entries WHERE session_id = ? AND id = ?`, sessionID, chronicleID)
	var (
		entry  ChronicleEntry
		unitID sql.NullString
	)
	switch scanErr := row.Scan(&entry.ID, &entry.SessionID, &unitID, &entry.Turn, &entry.Kind, &entry.Text, &entry.CreatedAt); {
	case scanErr == sql.ErrNoRows:
		return ChronicleView{}, false, nil // 条目不存在：not found，best-effort 不报错
	case scanErr != nil:
		return ChronicleView{}, false, fmt.Errorf("scan chronicle moment: %w", scanErr)
	}
	if unitID.Valid {
		entry.UnitID = unitID.String
	}
	anchor, anchorErr := service.anchorMoment(ctx, entry)
	if anchorErr != nil {
		anchor = MomentAnchor{ChronicleID: entry.ID, UnitID: entry.UnitID, Turn: entry.Turn}
	}
	return ChronicleView{Entry: entry, Anchor: anchor}, true, nil
}
