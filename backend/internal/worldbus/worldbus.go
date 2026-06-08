// Package worldbus 是跨玩家世界总线：把「谁对谁做了什么」记进一张 append-only、不可篡改的
// cross_events 表，作为跨用户、跨分片角色之间唯一可仲裁的事实源（设计文档 docs/事件耦合与跨玩家关联.md）。
//
// 三条铁律：
//  1. 只追加，永不改写——本包只暴露 Append + 只读查询，没有任何 UPDATE/DELETE 路径。
//  2. 权威排序 = (world_tick, occurred_at, id)，即「谁先动手算谁的」，可复算、可仲裁。
//  3. 不设 units 外键——跨界 actor/target 可能在别的分片或已离线，引用完整性由业务层而非 FK 负责。
package worldbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// EventKind 是跨玩家交互的类型。
type EventKind string

const (
	KindRescue          EventKind = "CROSS_RESCUE"        // 救了别家角色
	KindBetrayal        EventKind = "CROSS_BETRAYAL"      // 背叛/黑吃黑
	KindGift            EventKind = "CROSS_GIFT"          // 馈赠/交易
	KindAttack          EventKind = "CROSS_ATTACK"        // 攻击别家角色
	KindAlliance        EventKind = "CROSS_ALLIANCE"      // 结盟
	KindAcquaint        EventKind = "CROSS_ACQUAINT"      // 结识（七种交互，§2.3）
	KindMarriage        EventKind = "CROSS_MARRIAGE"      // 联姻（七种交互）
	KindVengeance       EventKind = "CROSS_VENGEANCE"     // 复仇（七种交互）
	KindWorldBossStrike EventKind = "WORLD_BOSS_STRIKE"   // 对世界Boss的一次出手
	KindWorldBossDown   EventKind = "WORLD_BOSS_DEFEATED" // 世界Boss被讨平（全服可见）
)

// CrossEvent 是一条跨玩家世界事件。ID/OccurredAt 留空时由存储层补全。
type CrossEvent struct {
	ID         string
	WorldID    string
	ActorID    string // 发起方角色（可跨用户/分片，不受 FK 约束）
	TargetID   string // 受体角色（可空）
	Kind       EventKind
	RegionID   string
	Importance int
	WorldTick  int    // 权威「谁先动手」排序键
	Payload    any    // 序列化进 payload_json
	OccurredAt string // RFC3339；为空则用 DB 的 CURRENT_TIMESTAMP
}

// Execer 是 Append 需要的最小依赖（*sql.DB 或 *sql.Tx 均满足）。
type Execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// Querier 是只读查询需要的最小依赖（*sql.DB 或 *sql.Tx 均满足）。
type Querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Append 把一条跨玩家事件追加进世界总线（append-only）。返回事件 ID。
func Append(ctx context.Context, execer Execer, event CrossEvent) (string, error) {
	if execer == nil {
		return "", fmt.Errorf("worldbus append: nil execer")
	}
	if event.WorldID == "" {
		return "", fmt.Errorf("worldbus append: empty world_id")
	}
	if event.Kind == "" {
		return "", fmt.Errorf("worldbus append: empty event_kind")
	}
	payload := event.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("worldbus marshal payload: %w", err)
	}
	id := event.ID
	if id == "" {
		id = uuid.NewString()
	}

	// occurred_at 留空时交给 DB 默认值（CURRENT_TIMESTAMP），保持「谁先动手」语义由写入顺序兜底。
	if event.OccurredAt == "" {
		_, err = execer.ExecContext(ctx, `
			INSERT INTO cross_events (id, world_id, actor_unit_id, target_unit_id, event_kind, region_id, importance, world_tick, payload_json)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, event.WorldID, nullable(event.ActorID), nullable(event.TargetID), string(event.Kind),
			nullable(event.RegionID), event.Importance, event.WorldTick, string(encoded))
	} else {
		_, err = execer.ExecContext(ctx, `
			INSERT INTO cross_events (id, world_id, actor_unit_id, target_unit_id, event_kind, region_id, importance, world_tick, payload_json, occurred_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, event.WorldID, nullable(event.ActorID), nullable(event.TargetID), string(event.Kind),
			nullable(event.RegionID), event.Importance, event.WorldTick, string(encoded), event.OccurredAt)
	}
	if err != nil {
		return "", fmt.Errorf("worldbus insert: %w", err)
	}
	return id, nil
}

// ListByWorld 按权威顺序（world_tick, occurred_at, id）返回某世界的跨玩家事件。limit<=0 时取 200。
func ListByWorld(ctx context.Context, q Querier, worldID string, limit int) ([]CrossEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, world_id, actor_unit_id, target_unit_id, event_kind, region_id, importance, world_tick, payload_json, occurred_at
		FROM cross_events WHERE world_id = ?
		ORDER BY world_tick ASC, occurred_at ASC, id ASC
		LIMIT ?`, worldID, limit)
	if err != nil {
		return nil, fmt.Errorf("worldbus list by world: %w", err)
	}
	defer rows.Close()
	return scanCrossEvents(rows)
}

// ListByWorldKind 返回某世界某类型的全部跨事件，按权威顺序，**不设默认上限**——
// 用于结算等必须读到完整账本的场景（worldboss 贡献账本若被 LIMIT 截断会少分赃）。
func ListByWorldKind(ctx context.Context, q Querier, worldID string, kind EventKind) ([]CrossEvent, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, world_id, actor_unit_id, target_unit_id, event_kind, region_id, importance, world_tick, payload_json, occurred_at
		FROM cross_events WHERE world_id = ? AND event_kind = ?
		ORDER BY world_tick ASC, occurred_at ASC, id ASC`, worldID, string(kind))
	if err != nil {
		return nil, fmt.Errorf("worldbus list by world kind: %w", err)
	}
	defer rows.Close()
	return scanCrossEvents(rows)
}

// ListForCharacter 返回某角色作为 actor 或 target 牵涉到的跨玩家事件（喂给命运收件箱的跨玩家关联）。
func ListForCharacter(ctx context.Context, q Querier, worldID string, characterID string, limit int) ([]CrossEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, world_id, actor_unit_id, target_unit_id, event_kind, region_id, importance, world_tick, payload_json, occurred_at
		FROM cross_events WHERE world_id = ? AND (actor_unit_id = ? OR target_unit_id = ?)
		ORDER BY world_tick ASC, occurred_at ASC, id ASC
		LIMIT ?`, worldID, characterID, characterID, limit)
	if err != nil {
		return nil, fmt.Errorf("worldbus list for character: %w", err)
	}
	defer rows.Close()
	return scanCrossEvents(rows)
}

func scanCrossEvents(rows *sql.Rows) ([]CrossEvent, error) {
	var out []CrossEvent
	for rows.Next() {
		var (
			ev               CrossEvent
			actor, target    sql.NullString
			region           sql.NullString
			kind, payloadStr string
		)
		if err := rows.Scan(&ev.ID, &ev.WorldID, &actor, &target, &kind, &region,
			&ev.Importance, &ev.WorldTick, &payloadStr, &ev.OccurredAt); err != nil {
			return nil, fmt.Errorf("worldbus scan: %w", err)
		}
		ev.ActorID = actor.String
		ev.TargetID = target.String
		ev.RegionID = region.String
		ev.Kind = EventKind(kind)
		ev.Payload = json.RawMessage(payloadStr)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// nullable 把空字符串映射为 SQL NULL（保持 actor/target 可空语义）。
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
