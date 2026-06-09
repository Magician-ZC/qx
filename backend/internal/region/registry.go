// Package region 是多世界模型里 region 实体的注册表与 per-region 逻辑时钟
// （设计文档 docs/大世界沙盘设计方案.md §8.1）。
//
// 在此之前 region 是「region_id == sessionID」的隐式约定：没有独立实体、没有区级活跃度
// （HOT/WARM/COLD）、没有 region.threat_level、也没有 per-region 逻辑时钟。本包把 region
// 扶正为 worlds 下的一等子实体：
//   - regions 表：每个 region 的活跃度档（调度分层用）、threat_level（威胁累积，供 PvE 威胁
//     「天然扎堆」结算用）、last_tick（最近一次推进到的逻辑时钟值）。
//   - world_ticks 表：per-region 逻辑时钟，AdvanceRegionTick 原子单调发号。worlds.tick 是世界级
//     「谁先动手」全局序，本表是每个 region 自己独立计时的发号器。
//
// 双驱动惯用法对齐 world.AdvanceTick / agentqueue：SQLite 走 UPDATE…RETURNING（写串行化原子），
// MySQL 走 SELECT…FOR UPDATE + UPDATE 包在内部事务里（FOR UPDATE 仅事务内加行锁、防并发双发号）。
// 时间戳一律由代码显式写入（MySQL 列默认 ”，与 agentqueue 同口径），双驱动行为一致、确定可测。
//
// 现阶段为纯新增地基（新包 + 两表），不改 regionrunner.go / world.go。
// ⚠️ 后续接线点：region-runner 接入 region.threat_level 做威胁扎堆结算（BumpThreatLevel 已就绪，
// 但 regionrunner.go 尚未消费），SetActivityTier 接入调度分层判定——均属 §8.2 调度层后续整合。
package region

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
)

// ErrNotFound 表示按 ID 找不到 region。
var ErrNotFound = errors.New("region not found")

// Tier 是 region 的活跃度档（三层活跃度，§8.2）：HOT 实时高频、WARM 规则推演、COLD 休眠懒推。
type Tier string

const (
	TierHot  Tier = "hot"  // 有玩家在场 / 高频事件，实时调度
	TierWarm Tier = "warm" // 规则推演，关键节点才升级 LLM
	TierCold Tier = "cold" // 休眠，lazy catch-up 时再追演
)

// validTier 归一化并校验活跃度档；空值视为 cold。
func validTier(t Tier) (Tier, bool) {
	switch Tier(strings.ToLower(strings.TrimSpace(string(t)))) {
	case TierHot:
		return TierHot, true
	case TierWarm:
		return TierWarm, true
	case TierCold, "":
		return TierCold, true
	default:
		return "", false
	}
}

// Region 是一个 region 实体（worlds 下的一等子实体）。
type Region struct {
	ID           string
	WorldID      string
	ActivityTier Tier
	ThreatLevel  int64
	LastTick     int64
	UpdatedAt    string
}

// tsLayout 定宽时间布局（与 agentqueue 同口径）：字典序==时间序，双驱动一致。
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func nowTS() string { return time.Now().UTC().Format(tsLayout) }

// Registry 是 region 注册表服务。它只持有 *sql.DB（方言由 dbdialect.For 推断），无其它状态。
type Registry struct {
	db *sql.DB
}

// New 构造一个 Registry。
func New(db *sql.DB) *Registry {
	return &Registry{db: db}
}

// UpsertRegion 幂等登记一个 region（按 id upsert）：首次插入按 world_id 建档（默认 cold 档、零威胁、tick 0），
// 重复调用只刷新 world_id 与 updated_at，**不覆盖** activity_tier / threat_level / last_tick（这些由各自的
// Set/Bump/Advance 专门维护）——避免重复登记把已积累的活跃度/威胁/时钟抹平。
func (r *Registry) UpsertRegion(ctx context.Context, id string, worldID string) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("region upsert: nil registry")
	}
	if id == "" {
		return fmt.Errorf("region upsert: empty id")
	}
	now := nowTS()
	query := `
		INSERT INTO regions (id, world_id, activity_tier, threat_level, last_tick, updated_at)
		VALUES (?, ?, 'cold', 0, 0, ?)
		ON CONFLICT(id) DO UPDATE SET world_id = excluded.world_id, updated_at = excluded.updated_at`
	if dbdialect.IsMySQL(r.db) {
		query = `
			INSERT INTO regions (id, world_id, activity_tier, threat_level, last_tick, updated_at)
			VALUES (?, ?, 'cold', 0, 0, ?)
			ON DUPLICATE KEY UPDATE world_id = VALUES(world_id), updated_at = VALUES(updated_at)`
	}
	if _, err := r.db.ExecContext(ctx, query, id, worldID, now); err != nil {
		return fmt.Errorf("region upsert: %w", err)
	}
	return nil
}

// SetActivityTier 设置 region 活跃度档（HOT/WARM/COLD）。region 不存在时返回 ErrNotFound。
func (r *Registry) SetActivityTier(ctx context.Context, id string, tier Tier) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("region set tier: nil registry")
	}
	norm, ok := validTier(tier)
	if !ok {
		return fmt.Errorf("region set tier: invalid tier %q", tier)
	}
	res, err := r.db.ExecContext(ctx, `UPDATE regions SET activity_tier = ?, updated_at = ? WHERE id = ?`,
		string(norm), nowTS(), id)
	if err != nil {
		return fmt.Errorf("region set tier: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BumpThreatLevel 累积 region 威胁等级（delta 可正可负），供 PvE 威胁「天然扎堆」结算用，返回累积后的新值。
// 累加在 SQL 内完成（threat_level = threat_level + ?）以避免读改写竞态。region 不存在时返回 ErrNotFound。
//
// 双驱动：SQLite 走 UPDATE…RETURNING 一次拿到新值；MySQL 无 RETURNING，走内部事务 UPDATE + SELECT…FOR UPDATE
// 复读（事务内一致），避免并发 bump 读到串扰值。
func (r *Registry) BumpThreatLevel(ctx context.Context, id string, delta int64) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("region bump threat: nil registry")
	}
	now := nowTS()
	if dbdialect.IsMySQL(r.db) {
		return r.bumpThreatMySQL(ctx, id, delta, now)
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE regions SET threat_level = threat_level + ?, updated_at = ?
		WHERE id = ? RETURNING threat_level`, delta, now, id)
	var level int64
	if err := row.Scan(&level); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("region bump threat: %w", err)
	}
	return level, nil
}

// SetThreatLevelAbsolute 把 region 威胁等级**绝对置位**到 level（GM 人工拉高/清零某地威胁度做活动/演练）。
// 与 BumpThreatLevel 的「读当前+累加 delta」不同：本方法用单条原子 UPDATE 直写目标值，
// 杜绝「Get 当前值 → Bump(target−current)」两步之间的 TOCTOU 竞态（并发两次绝对置位不会互相串扰成错误终值）。
// level 入参前夹 MAX(0,level)（威胁度非负）。region 不存在时返回 ErrNotFound——调用方应先 UpsertRegion 建档。
// 返回置位后的实际威胁值（即夹钳后的 level）。
func (r *Registry) SetThreatLevelAbsolute(ctx context.Context, id string, level int64) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("region set threat absolute: nil registry")
	}
	if level < 0 {
		level = 0
	}
	res, err := r.db.ExecContext(ctx, `UPDATE regions SET threat_level = ?, updated_at = ? WHERE id = ?`,
		level, nowTS(), id)
	if err != nil {
		return 0, fmt.Errorf("region set threat absolute: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return level, nil
}

func (r *Registry) bumpThreatMySQL(ctx context.Context, id string, delta int64, now string) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("region bump threat (begin tx): %w", err)
	}
	defer tx.Rollback()
	var level int64
	if err := tx.QueryRowContext(ctx, `SELECT threat_level FROM regions WHERE id = ? FOR UPDATE`, id).Scan(&level); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("region bump threat (lock): %w", err)
	}
	level += delta
	if _, err := tx.ExecContext(ctx, `UPDATE regions SET threat_level = ?, updated_at = ? WHERE id = ?`, level, now, id); err != nil {
		return 0, fmt.Errorf("region bump threat (update): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("region bump threat (commit): %w", err)
	}
	return level, nil
}

// AdvanceRegionTick 原子推进某 region 的 per-region 逻辑时钟并返回新值（区级调度发号器）。
// world_ticks 表里 (world_id, region_id) 行不存在时自动建档为 tick=1（首次发号）；存在则单调 +1。
// 同时把 regions.last_tick 同步到新值（best-effort：regions 行不存在时不报错，因 world_ticks 自身已是权威）。
//
// 双驱动：均在内部事务内完成「确保行存在→原子取下一号→回写 regions.last_tick」，保证并发发号不撞号、
// 且 last_tick 与发号一致。SQLite 单连接写串行、MySQL 靠主键唯一 + FOR UPDATE 行锁。
func (r *Registry) AdvanceRegionTick(ctx context.Context, worldID string, regionID string) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("region advance tick: nil registry")
	}
	if regionID == "" {
		return 0, fmt.Errorf("region advance tick: empty region id")
	}
	now := nowTS()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("region advance tick (begin tx): %w", err)
	}
	defer tx.Rollback()

	tick, err := advanceTickTx(ctx, tx, worldID, regionID, now, dbdialect.For(r.db))
	if err != nil {
		return 0, err
	}
	// best-effort 同步 regions.last_tick（region 未登记则 0 行受影响，不报错）。
	if _, err := tx.ExecContext(ctx, `UPDATE regions SET last_tick = ?, updated_at = ? WHERE id = ?`, tick, now, regionID); err != nil {
		return 0, fmt.Errorf("region advance tick (sync last_tick): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("region advance tick (commit): %w", err)
	}
	return tick, nil
}

// advanceTickTx 在事务内确保 world_ticks 行存在，然后原子取下一个 tick。
func advanceTickTx(ctx context.Context, tx *sql.Tx, worldID, regionID, now string, dialect dbdialect.Dialect) (int64, error) {
	// 确保行存在：首次发号建档（tick 仍为 0，下面的 +1 把它推到 1）。冲突即已存在，DO NOTHING。
	ensure := `INSERT INTO world_ticks (world_id, region_id, tick, updated_at) VALUES (?, ?, 0, ?)
		ON CONFLICT(world_id, region_id) DO NOTHING`
	if dialect == dbdialect.DialectMySQL {
		ensure = `INSERT IGNORE INTO world_ticks (world_id, region_id, tick, updated_at) VALUES (?, ?, 0, ?)`
	}
	if _, err := tx.ExecContext(ctx, ensure, worldID, regionID, now); err != nil {
		return 0, fmt.Errorf("region advance tick (ensure row): %w", err)
	}
	if dialect == dbdialect.DialectMySQL {
		var tick int64
		if err := tx.QueryRowContext(ctx, `SELECT tick FROM world_ticks WHERE world_id = ? AND region_id = ? FOR UPDATE`, worldID, regionID).Scan(&tick); err != nil {
			return 0, fmt.Errorf("region advance tick (lock): %w", err)
		}
		tick++
		if _, err := tx.ExecContext(ctx, `UPDATE world_ticks SET tick = ?, updated_at = ? WHERE world_id = ? AND region_id = ?`, tick, now, worldID, regionID); err != nil {
			return 0, fmt.Errorf("region advance tick (update): %w", err)
		}
		return tick, nil
	}
	row := tx.QueryRowContext(ctx, `UPDATE world_ticks SET tick = tick + 1, updated_at = ? WHERE world_id = ? AND region_id = ? RETURNING tick`, now, worldID, regionID)
	var tick int64
	if err := row.Scan(&tick); err != nil {
		return 0, fmt.Errorf("region advance tick (bump): %w", err)
	}
	return tick, nil
}

// GetRegion 按 ID 取 region；不存在时返回 ErrNotFound。
func (r *Registry) GetRegion(ctx context.Context, id string) (Region, error) {
	if r == nil || r.db == nil {
		return Region{}, fmt.Errorf("region get: nil registry")
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, world_id, activity_tier, threat_level, last_tick, updated_at
		FROM regions WHERE id = ?`, id)
	reg, err := scanRegion(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Region{}, ErrNotFound
	}
	return reg, err
}

// ListByTier 列出某世界下指定活跃度档的 region（worldID 为空时跨世界列举该档全部）。
// limit<=0 时取 500。按 updated_at DESC 排序（最近活跃优先），供调度层挑选要推进的区。
func (r *Registry) ListByTier(ctx context.Context, worldID string, tier Tier, limit int) ([]Region, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("region list: nil registry")
	}
	norm, ok := validTier(tier)
	if !ok {
		return nil, fmt.Errorf("region list: invalid tier %q", tier)
	}
	if limit <= 0 {
		limit = 500
	}
	var (
		rows *sql.Rows
		err  error
	)
	if worldID == "" {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, world_id, activity_tier, threat_level, last_tick, updated_at
			FROM regions WHERE activity_tier = ? ORDER BY updated_at DESC, id ASC LIMIT ?`, string(norm), limit)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT id, world_id, activity_tier, threat_level, last_tick, updated_at
			FROM regions WHERE world_id = ? AND activity_tier = ? ORDER BY updated_at DESC, id ASC LIMIT ?`, worldID, string(norm), limit)
	}
	if err != nil {
		return nil, fmt.Errorf("region list: %w", err)
	}
	defer rows.Close()
	var out []Region
	for rows.Next() {
		reg, err := scanRegion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, reg)
	}
	return out, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRegion(s scanner) (Region, error) {
	var reg Region
	var tier string
	if err := s.Scan(&reg.ID, &reg.WorldID, &tier, &reg.ThreatLevel, &reg.LastTick, &reg.UpdatedAt); err != nil {
		return Region{}, err
	}
	reg.ActivityTier = Tier(tier)
	return reg, nil
}
