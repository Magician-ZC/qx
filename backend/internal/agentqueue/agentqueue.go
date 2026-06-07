// Package agentqueue 是 region-runner 调度的**持久化队列**（沙盘 §8.2 / §9，M7.3 地基）：
//   - agent_wake_queue：每个单位「下次在哪个世界 tick 唤醒决策」，一单位一条（重排即 upsert）；
//   - agent_decision_jobs：到点单位生成的决策作业，worker 池按 status=pending **原子认领**、跑完置 done/failed。
//
// 纯调度判定（分层/算下次唤醒 tick/背压）在 internal/engine/scheduler（纯逻辑）；本包只管「持久化 + 原子认领」。
// 现阶段 shadow/additive：表与存取就绪，但未接执行主循环（接入是后续 M7.3-real）。
// 双驱动认领惯用法对齐 world.AdvanceTick：SQLite 走 UPDATE…RETURNING（写串行化原子），MySQL 走
// SELECT…FOR UPDATE + UPDATE 包在内部事务里（FOR UPDATE 仅事务内加行锁）。
//
// ⚠️ M7.3-real 接入执行主循环前**必须补齐**的三项（经评审登记，现阶段表空故无现网影响，但接入即生效）：
//   1. 保留期/隐私清理：本两表 key 在 unit_id/region_id（无 session_id 列），PurgeExpiredSessionData /
//      EraseSessionPrivateData 按 session_id 收敛、扫不到本表。接入时须把它们纳入这两条清理路径（阶段-0 region==session
//      可按 region_id 删；或给表加 session_id 列），并为单位死亡补 job 清理——否则会话过期/擦除后留永久孤儿，违保留期。
//   2. stale-running 回收：worker 认领后崩溃/退出会让 job 永久卡 running，单调抬高 CountJobsByStatus(running)
//      背压计数、最终顶过 DefaultMaxInFlight 使 region-runner 饿死。接入时须补 claimed_at 超时 reclaim（置回 pending +
//      attempt 自增）+ attempt 上限（超限置 failed 不再 reclaim）。attempt 列已 plumb 但本片未自增。
//   3. 多世界作用域：ListDueWakes / 作业认领目前按 region_id 等值、未带 world_id。阶段-0 region_id==session_id 全局唯一
//      故无碰撞；多世界落地后须按 (world_id, region_id) 限定，避免跨世界同名 region 互相串唤醒。作业认领刻意全局（共享
//      worker 池 §9 全局在途上限），若需 region 亲和性再加 world/region 过滤。
package agentqueue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// 作业状态。
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// WakeEntry 是一条唤醒排期（一单位一条）。
type WakeEntry struct {
	UnitID     string
	WorldID    string
	RegionID   string
	WakeAtTick int64
	Tier       string
}

// DecisionJob 是一条决策作业。ID 留空时由 EnqueueJob 补全。
type DecisionJob struct {
	ID       string
	UnitID   string
	WorldID  string
	RegionID string
	Status   string
	Tick     int64
	Attempt  int
}

func nowTS() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// EnqueueWake 排定/重排某单位的下次唤醒 tick（按 unit_id upsert）。region-runner 处理完一个唤醒后用它重新入队。
func EnqueueWake(ctx context.Context, db *sql.DB, entry WakeEntry) error {
	if entry.UnitID == "" {
		return fmt.Errorf("enqueue wake: empty unit id")
	}
	if entry.Tier == "" {
		entry.Tier = "hot"
	}
	query := `INSERT INTO agent_wake_queue (unit_id, world_id, region_id, wake_at_tick, tier, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(unit_id) DO UPDATE SET
			world_id = excluded.world_id, region_id = excluded.region_id,
			wake_at_tick = excluded.wake_at_tick, tier = excluded.tier, enqueued_at = excluded.enqueued_at`
	if dbdialect.IsMySQL(db) {
		query = `INSERT INTO agent_wake_queue (unit_id, world_id, region_id, wake_at_tick, tier, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			world_id = VALUES(world_id), region_id = VALUES(region_id),
			wake_at_tick = VALUES(wake_at_tick), tier = VALUES(tier), enqueued_at = VALUES(enqueued_at)`
	}
	if _, err := db.ExecContext(ctx, query, entry.UnitID, nullable(entry.WorldID), nullable(entry.RegionID), entry.WakeAtTick, entry.Tier, nowTS()); err != nil {
		return fmt.Errorf("enqueue wake %s: %w", entry.UnitID, err)
	}
	return nil
}

// RemoveWake 删除某单位的唤醒排期（单位死亡/离场时调用）。
func RemoveWake(ctx context.Context, db *sql.DB, unitID string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM agent_wake_queue WHERE unit_id = ?`, unitID); err != nil {
		return fmt.Errorf("remove wake %s: %w", unitID, err)
	}
	return nil
}

// ListDueWakes 拉某 region 内到点（wake_at_tick <= currentTick）的唤醒排期，按 wake_at_tick 升序（最该醒的先）。
func ListDueWakes(ctx context.Context, db *sql.DB, regionID string, currentTick int64, limit int) ([]WakeEntry, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := db.QueryContext(ctx, `
		SELECT unit_id, world_id, region_id, wake_at_tick, tier
		FROM agent_wake_queue
		WHERE region_id = ? AND wake_at_tick <= ?
		ORDER BY wake_at_tick ASC, unit_id ASC LIMIT ?`, regionID, currentTick, limit)
	if err != nil {
		return nil, fmt.Errorf("list due wakes in region %s: %w", regionID, err)
	}
	defer rows.Close()
	out := make([]WakeEntry, 0)
	for rows.Next() {
		var e WakeEntry
		var worldID, regionCol sql.NullString
		if err := rows.Scan(&e.UnitID, &worldID, &regionCol, &e.WakeAtTick, &e.Tier); err != nil {
			return nil, fmt.Errorf("scan due wake: %w", err)
		}
		e.WorldID = worldID.String
		e.RegionID = regionCol.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// EnqueueJob 入队一条 pending 决策作业，返回作业 ID。
func EnqueueJob(ctx context.Context, db *sql.DB, job DecisionJob) (string, error) {
	if job.UnitID == "" {
		return "", fmt.Errorf("enqueue job: empty unit id")
	}
	id := job.ID
	if id == "" {
		id = uuid.NewString()
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO agent_decision_jobs (id, unit_id, world_id, region_id, status, tick, attempt, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, job.UnitID, nullable(job.WorldID), nullable(job.RegionID), StatusPending, job.Tick, job.Attempt, nowTS()); err != nil {
		return "", fmt.Errorf("enqueue decision job for %s: %w", job.UnitID, err)
	}
	return id, nil
}

// ClaimNextJob 原子认领下一条 pending 作业（pending→running），返回该作业；无 pending 时返回 (nil, nil)。
// 双驱动：SQLite 用 UPDATE…RETURNING（写串行化原子）；MySQL 用内部事务 SELECT…FOR UPDATE + UPDATE（行锁防并发双认领）。
func ClaimNextJob(ctx context.Context, db *sql.DB) (*DecisionJob, error) {
	claimedAt := nowTS()
	if dbdialect.IsMySQL(db) {
		return claimNextJobMySQL(ctx, db, claimedAt)
	}
	row := db.QueryRowContext(ctx, `
		UPDATE agent_decision_jobs SET status = ?, claimed_at = ?
		WHERE id = (SELECT id FROM agent_decision_jobs WHERE status = ? ORDER BY created_at ASC, id ASC LIMIT 1)
		RETURNING id, unit_id, world_id, region_id, tick, attempt`,
		StatusRunning, claimedAt, StatusPending)
	job, err := scanClaimedJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim next job (sqlite): %w", err)
	}
	return job, nil
}

func claimNextJobMySQL(ctx context.Context, db *sql.DB, claimedAt string) (*DecisionJob, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim next job (begin tx): %w", err)
	}
	defer tx.Rollback()

	var id string
	row := tx.QueryRowContext(ctx, `SELECT id FROM agent_decision_jobs WHERE status = ? ORDER BY created_at ASC, id ASC LIMIT 1 FOR UPDATE`, StatusPending)
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim next job (select for update): %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_decision_jobs SET status = ?, claimed_at = ? WHERE id = ?`, StatusRunning, claimedAt, id); err != nil {
		return nil, fmt.Errorf("claim next job (update): %w", err)
	}
	job := &DecisionJob{ID: id, Status: StatusRunning}
	var worldID, regionID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT unit_id, world_id, region_id, tick, attempt FROM agent_decision_jobs WHERE id = ?`, id).
		Scan(&job.UnitID, &worldID, &regionID, &job.Tick, &job.Attempt); err != nil {
		return nil, fmt.Errorf("claim next job (reload): %w", err)
	}
	job.WorldID = worldID.String
	job.RegionID = regionID.String
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim next job (commit): %w", err)
	}
	return job, nil
}

func scanClaimedJob(row *sql.Row) (*DecisionJob, error) {
	job := &DecisionJob{Status: StatusRunning}
	var worldID, regionID sql.NullString
	if err := row.Scan(&job.ID, &job.UnitID, &worldID, &regionID, &job.Tick, &job.Attempt); err != nil {
		return nil, err
	}
	job.WorldID = worldID.String
	job.RegionID = regionID.String
	return job, nil
}

// ErrJobNotClaimable 表示要完成的作业不在 running 态（不存在 / 仍 pending / 已终态），完成被拒。
var ErrJobNotClaimable = errors.New("agentqueue: job not in running state (missing or already terminal)")

// CompleteJob 把一条 **running** 作业置为终态（done/failed）。
// 谓词限定 status=running + 校验 RowsAffected==0→ErrJobNotClaimable（对齐 world.Seal 惯用法）：
// 杜绝把 pending 直接推到 done、或对不存在/已终态 id 静默 no-op——running 计数是 AdmitDecision 背压源，状态机须可信。
func CompleteJob(ctx context.Context, db *sql.DB, jobID string, status string) error {
	if status != StatusDone && status != StatusFailed {
		return fmt.Errorf("complete job: invalid terminal status %q", status)
	}
	res, err := db.ExecContext(ctx, `UPDATE agent_decision_jobs SET status = ?, completed_at = ? WHERE id = ? AND status = ?`, status, nowTS(), jobID, StatusRunning)
	if err != nil {
		return fmt.Errorf("complete job %s: %w", jobID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete job %s rows affected: %w", jobID, err)
	}
	if affected == 0 {
		return ErrJobNotClaimable
	}
	return nil
}

// CountJobsByStatus 统计某状态的作业数（CountRunning 用于 worker 池在途背压判定 scheduler.AdmitDecision）。
func CountJobsByStatus(ctx context.Context, db *sql.DB, status string) (int, error) {
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_decision_jobs WHERE status = ?`, status).Scan(&count); err != nil {
		return 0, fmt.Errorf("count jobs status %s: %w", status, err)
	}
	return count, nil
}
