// Package world 是多世界模型的根：世界的注册、生命周期、权威时钟与角色归属
// （设计文档 docs/大世界沙盘设计方案.md §8）。
//
// 世界时钟 tick 是关键：它单调推进、由 AdvanceTick 原子递增，是 worldbus.cross_events.world_tick
// 「谁先动手」排序的唯一来源。这对应设计的「世界会等你，但不会假装暂停」——离线期间世界按 tick 继续。
//
// 与 units 刻意解耦：world_members 不设 FK，成员可以是跨分片/别用户的角色。
package world

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// ErrNotFound 表示按 ID 找不到世界。
var ErrNotFound = errors.New("world not found")

// Status 是世界的生命周期状态。
type Status string

const (
	StatusActive Status = "active" // 运行中，接受接入与推进
	StatusSealed Status = "sealed" // 封存（满员/归档/赛季结束），只读
)

// World 是一个持久世界。
type World struct {
	ID            string
	Name          string
	Status        Status
	Tick          int
	MaxPopulation int
	RegionSeed    string
	CreatedAt     string
}

// Member 是一条角色→世界归属。
type Member struct {
	WorldID     string
	CharacterID string
	Role        string
	JoinedAt    string
}

// DB 是本包所需的最小存储依赖（*sql.DB 与 *sql.Tx 均满足）。
type DB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Create 注册一个新世界。ID 为空时自动生成。返回世界 ID。
func Create(ctx context.Context, db DB, w World) (string, error) {
	if db == nil {
		return "", fmt.Errorf("world create: nil db")
	}
	if w.Name == "" {
		return "", fmt.Errorf("world create: empty name")
	}
	id := w.ID
	if id == "" {
		id = uuid.NewString()
	}
	status := w.Status
	if status == "" {
		status = StatusActive
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO worlds (id, name, status, tick, max_population, region_seed)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, w.Name, string(status), w.Tick, w.MaxPopulation, w.RegionSeed); err != nil {
		return "", fmt.Errorf("world insert: %w", err)
	}
	return id, nil
}

// Get 按 ID 取世界；不存在时返回 ErrNotFound。
func Get(ctx context.Context, db DB, id string) (World, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, name, status, tick, max_population, region_seed, created_at
		FROM worlds WHERE id = ?`, id)
	w, err := scanWorld(row)
	if errors.Is(err, sql.ErrNoRows) {
		return World{}, ErrNotFound
	}
	return w, err
}

// List 列出某状态的世界（status 为空时列全部）。limit<=0 时取 100。
func List(ctx context.Context, db DB, status Status, limit int) ([]World, error) {
	if limit <= 0 {
		limit = 100
	}
	var (
		rows *sql.Rows
		err  error
	)
	if status == "" {
		rows, err = db.QueryContext(ctx, `
			SELECT id, name, status, tick, max_population, region_seed, created_at
			FROM worlds ORDER BY created_at DESC LIMIT ?`, limit)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT id, name, status, tick, max_population, region_seed, created_at
			FROM worlds WHERE status = ? ORDER BY created_at DESC LIMIT ?`, string(status), limit)
	}
	if err != nil {
		return nil, fmt.Errorf("world list: %w", err)
	}
	defer rows.Close()
	var out []World
	for rows.Next() {
		w, err := scanWorld(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// AdvanceTick 原子推进世界时钟并返回新值（「谁先动手」排序键的发号器）。
// 世界不存在时返回 ErrNotFound。
func AdvanceTick(ctx context.Context, db DB, id string) (int, error) {
	row := db.QueryRowContext(ctx, `
		UPDATE worlds SET tick = tick + 1 WHERE id = ? RETURNING tick`, id)
	var tick int
	if err := row.Scan(&tick); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("world advance tick: %w", err)
	}
	return tick, nil
}

// Seal 把世界置为封存（只读）。
func Seal(ctx context.Context, db DB, id string) error {
	res, err := db.ExecContext(ctx, `UPDATE worlds SET status = ? WHERE id = ?`, string(StatusSealed), id)
	if err != nil {
		return fmt.Errorf("world seal: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Join 把一个角色接入世界（幂等：重复接入不报错、不重复计数）。role 为空时取 inhabitant。
func Join(ctx context.Context, db DB, worldID string, characterID string, role string) error {
	if worldID == "" || characterID == "" {
		return fmt.Errorf("world join: empty world_id or character_id")
	}
	if role == "" {
		role = "inhabitant"
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO world_members (world_id, character_unit_id, role)
		VALUES (?, ?, ?)
		ON CONFLICT(world_id, character_unit_id) DO NOTHING`,
		worldID, characterID, role); err != nil {
		return fmt.Errorf("world join: %w", err)
	}
	return nil
}

// Members 列出某世界的成员。limit<=0 时取 500。
func Members(ctx context.Context, db DB, worldID string, limit int) ([]Member, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := db.QueryContext(ctx, `
		SELECT world_id, character_unit_id, role, joined_at
		FROM world_members WHERE world_id = ? ORDER BY joined_at ASC LIMIT ?`, worldID, limit)
	if err != nil {
		return nil, fmt.Errorf("world members: %w", err)
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.WorldID, &m.CharacterID, &m.Role, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// WorldOf 返回某角色当前归属的世界 ID；未归属任何世界时 ok=false。
func WorldOf(ctx context.Context, db DB, characterID string) (string, bool, error) {
	row := db.QueryRowContext(ctx, `
		SELECT world_id FROM world_members WHERE character_unit_id = ? ORDER BY joined_at DESC LIMIT 1`, characterID)
	var worldID string
	if err := row.Scan(&worldID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("world of: %w", err)
	}
	return worldID, true, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanWorld(s scanner) (World, error) {
	var w World
	var status string
	if err := s.Scan(&w.ID, &w.Name, &status, &w.Tick, &w.MaxPopulation, &w.RegionSeed, &w.CreatedAt); err != nil {
		return World{}, err
	}
	w.Status = Status(status)
	return w, nil
}
