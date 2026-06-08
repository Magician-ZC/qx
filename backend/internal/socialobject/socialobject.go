// Package socialobject 是「社会客体」的持久层（设计 docs/事件耦合与跨玩家关联.md §2.2）：把一个可让多名角色被撮合进去的
// 共享对象/事件（组队、结盟、市集、血仇社会客体…）连同其成员存进 social_objects + social_object_members 两张表。
// 成员绑定按 (object_id, unit_id) 幂等；不设 units 外键（跨界角色可能在别分片/已离线，引用完整性由业务层负责，与 worldbus 同理）。
package socialobject

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// SocialObject 是一个可撮合多名角色进入的共享客体。
type SocialObject struct {
	ID        string `json:"id"`
	WorldID   string `json:"world_id"`
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// Member 是一名被撮合进社会客体的角色（带其撮合分数）。
type Member struct {
	ObjectID string  `json:"object_id"`
	UnitID   string  `json:"unit_id"`
	Score    float64 `json:"score"`
	JoinedAt string  `json:"joined_at"`
}

// Create 落库一个社会客体（id 空则生成）。occurred/created 留空交给 DB 默认 CURRENT_TIMESTAMP。
func Create(ctx context.Context, db *sql.DB, obj SocialObject) (string, error) {
	if obj.WorldID == "" || obj.Kind == "" {
		return "", fmt.Errorf("social object: world_id/kind required")
	}
	if obj.ID == "" {
		obj.ID = uuid.NewString()
	}
	if obj.Status == "" {
		obj.Status = "active"
	}
	if dbdialect.IsMySQL(db) {
		_, err := db.ExecContext(ctx,
			`INSERT INTO social_objects (id, world_id, kind, label, status) VALUES (?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE label=VALUES(label), status=VALUES(status)`,
			obj.ID, obj.WorldID, obj.Kind, obj.Label, obj.Status)
		return obj.ID, err
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO social_objects (id, world_id, kind, label, status) VALUES (?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET label=excluded.label, status=excluded.status`,
		obj.ID, obj.WorldID, obj.Kind, obj.Label, obj.Status)
	return obj.ID, err
}

// AddMember 幂等绑定一名成员（重复绑定更新分数）。
func AddMember(ctx context.Context, db *sql.DB, m Member) error {
	if m.ObjectID == "" || m.UnitID == "" {
		return fmt.Errorf("social object member: object_id/unit_id required")
	}
	if dbdialect.IsMySQL(db) {
		_, err := db.ExecContext(ctx,
			`INSERT INTO social_object_members (object_id, unit_id, score) VALUES (?,?,?)
			 ON DUPLICATE KEY UPDATE score=VALUES(score)`,
			m.ObjectID, m.UnitID, m.Score)
		return err
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO social_object_members (object_id, unit_id, score) VALUES (?,?,?)
		 ON CONFLICT(object_id, unit_id) DO UPDATE SET score=excluded.score`,
		m.ObjectID, m.UnitID, m.Score)
	return err
}

// Get 读一个社会客体。
func Get(ctx context.Context, db *sql.DB, objectID string) (SocialObject, error) {
	var o SocialObject
	err := db.QueryRowContext(ctx,
		`SELECT id, world_id, kind, label, status, created_at FROM social_objects WHERE id = ?`, objectID).
		Scan(&o.ID, &o.WorldID, &o.Kind, &o.Label, &o.Status, &o.CreatedAt)
	return o, err
}

// ListMembers 列出某客体的成员（按分数降序）。
func ListMembers(ctx context.Context, db *sql.DB, objectID string) ([]Member, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT object_id, unit_id, score, joined_at FROM social_object_members WHERE object_id = ? ORDER BY score DESC, unit_id ASC`, objectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Member, 0)
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ObjectID, &m.UnitID, &m.Score, &m.JoinedAt); err != nil {
			return out, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListByWorld 列出某世界的社会客体（最近优先）。
func ListByWorld(ctx context.Context, db *sql.DB, worldID string) ([]SocialObject, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, world_id, kind, label, status, created_at FROM social_objects WHERE world_id = ? ORDER BY created_at DESC, id ASC`, worldID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SocialObject, 0)
	for rows.Next() {
		var o SocialObject
		if err := rows.Scan(&o.ID, &o.WorldID, &o.Kind, &o.Label, &o.Status, &o.CreatedAt); err != nil {
			return out, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
