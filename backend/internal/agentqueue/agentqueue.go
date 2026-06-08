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
//  1. 保留期清理：两表已加 session_id 列、PurgeExpiredSessionData 按 session_id 删（real-0 已接，与其余旁路表口径一致）。
//     **enqueue 调用方必须把 WakeEntry/DecisionJob.SessionID 填成 ==sessionID**，否则 purge 漏删留孤儿。
//     仍待补（real-1+）：单位死亡时 RemoveWake + 失效其在途 job。（EraseSessionPrivateData 刻意不清——队列无 PII 且会话存续。）
//  2. stale-running 回收：worker 认领后崩溃/退出会让 job 永久卡 running，单调抬高 CountJobsByStatus(running)
//     背压计数、最终顶过 DefaultMaxInFlight 使 region-runner 饿死。接入时须补 claimed_at 超时 reclaim（置回 pending +
//     attempt 自增）+ attempt 上限（超限置 failed 不再 reclaim）。attempt 列已 plumb 但本片未自增。
//  3. 多世界作用域：ListDueWakes / 作业认领目前按 region_id 等值、未带 world_id。阶段-0 region_id==session_id 全局唯一
//     故无碰撞；多世界落地后须按 (world_id, region_id) 限定，避免跨世界同名 region 互相串唤醒。作业认领刻意全局（共享
//     worker 池 §9 全局在途上限），若需 region 亲和性再加 world/region 过滤。
//  4. **RegionID 必填不变量**：DistinctWakeRegions 带 `region_id IS NOT NULL`、ListDueWakes 按 region_id 等值匹配，
//     故空 RegionID 入队的 wake（存为 NULL）**永不会被 region-runner 发现 → 单位永久饿死**。real-4 seed 接入时
//     EnqueueWake 的 RegionID 必填（MVP ==sessionID）；切勿以空 region 入队。
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
// SessionID 是保留期清理键（PurgeExpiredSessionData 按它删），MVP 须由调用方填 ==sessionID；与 region 取值解耦。
type WakeEntry struct {
	UnitID     string
	SessionID  string
	WorldID    string
	RegionID   string
	WakeAtTick int64
	Tier       string
}

// DecisionJob 是一条决策作业。ID 留空时由 EnqueueJob 补全。
// SessionID 既是 worker 处理时定位会话的上下文，也是保留期清理键（须由调用方填 ==sessionID）。
type DecisionJob struct {
	ID        string
	UnitID    string
	SessionID string
	WorldID   string
	RegionID  string
	Status    string
	Tick      int64
	Attempt   int
}

// tsLayout 定宽时间布局（纳秒补零 + 显式时区），字典序=时间序——claimed_at/created_at 的字符串比较（stale-reclaim
// 的 claimed_at<cutoff、作业认领的 created_at 升序 FIFO）据此成立，双驱动一致。
const tsLayout = "2006-01-02T15:04:05.000000000Z07:00"

func nowTS() string { return time.Now().UTC().Format(tsLayout) }

func formatTS(t time.Time) string { return t.UTC().Format(tsLayout) }

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
	query := `INSERT INTO agent_wake_queue (unit_id, session_id, world_id, region_id, wake_at_tick, tier, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(unit_id) DO UPDATE SET
			session_id = excluded.session_id, world_id = excluded.world_id, region_id = excluded.region_id,
			wake_at_tick = excluded.wake_at_tick, tier = excluded.tier, enqueued_at = excluded.enqueued_at`
	if dbdialect.IsMySQL(db) {
		query = `INSERT INTO agent_wake_queue (unit_id, session_id, world_id, region_id, wake_at_tick, tier, enqueued_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			session_id = VALUES(session_id), world_id = VALUES(world_id), region_id = VALUES(region_id),
			wake_at_tick = VALUES(wake_at_tick), tier = VALUES(tier), enqueued_at = VALUES(enqueued_at)`
	}
	if _, err := db.ExecContext(ctx, query, entry.UnitID, nullable(entry.SessionID), nullable(entry.WorldID), nullable(entry.RegionID), entry.WakeAtTick, entry.Tier, nowTS()); err != nil {
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

// WakeRegion 是一个有唤醒排期的 (world, region)（region-runner 遍历它们逐个推进）。
type WakeRegion struct {
	WorldID  string
	RegionID string
}

// DistinctWakeRegions 列出唤醒队列里出现过的不同 (world_id, region_id)，供 region-runner 枚举待推进的 region。
// 空队列返回空列表（runner 据此空转，零负载）。
func DistinctWakeRegions(ctx context.Context, db *sql.DB) ([]WakeRegion, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT world_id, region_id FROM agent_wake_queue WHERE region_id IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("list distinct wake regions: %w", err)
	}
	defer rows.Close()
	out := make([]WakeRegion, 0)
	for rows.Next() {
		var worldID, regionID sql.NullString
		if err := rows.Scan(&worldID, &regionID); err != nil {
			return nil, fmt.Errorf("scan wake region: %w", err)
		}
		out = append(out, WakeRegion{WorldID: worldID.String, RegionID: regionID.String})
	}
	return out, rows.Err()
}

// ListDueWakes 拉某 (world,region) 内到点（wake_at_tick <= currentTick）的唤醒排期，按 wake_at_tick 升序（最该醒的先）。
// worldID 为空时不按 world 过滤（MVP 单世界 / region_id 全局唯一）；非空时加 world_id 限定，避免跨世界同名 region 串唤醒。
func ListDueWakes(ctx context.Context, db *sql.DB, worldID string, regionID string, currentTick int64, limit int) ([]WakeEntry, error) {
	if limit <= 0 {
		limit = 256
	}
	query := `
		SELECT unit_id, session_id, world_id, region_id, wake_at_tick, tier
		FROM agent_wake_queue
		WHERE region_id = ? AND wake_at_tick <= ?
		ORDER BY wake_at_tick ASC, unit_id ASC LIMIT ?`
	args := []any{regionID, currentTick, limit}
	if worldID != "" {
		query = `
		SELECT unit_id, session_id, world_id, region_id, wake_at_tick, tier
		FROM agent_wake_queue
		WHERE world_id = ? AND region_id = ? AND wake_at_tick <= ?
		ORDER BY wake_at_tick ASC, unit_id ASC LIMIT ?`
		args = []any{worldID, regionID, currentTick, limit}
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list due wakes in region %s: %w", regionID, err)
	}
	defer rows.Close()
	out := make([]WakeEntry, 0)
	for rows.Next() {
		var e WakeEntry
		var sessionCol, worldCol, regionCol sql.NullString
		if err := rows.Scan(&e.UnitID, &sessionCol, &worldCol, &regionCol, &e.WakeAtTick, &e.Tier); err != nil {
			return nil, fmt.Errorf("scan due wake: %w", err)
		}
		e.SessionID = sessionCol.String
		e.WorldID = worldCol.String
		e.RegionID = regionCol.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// rowExecer 是 INSERT 决策作业所需的最小依赖（*sql.DB 或 *sql.Tx 均满足），让入队逻辑可在事务内复用。
type rowExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// insertPendingJob 在给定执行器上插一条 pending 作业，返回 ID。供 EnqueueJob（裸 db）与 PromoteWakeToJob（事务）复用。
func insertPendingJob(ctx context.Context, exec rowExecer, job DecisionJob) (string, error) {
	id := job.ID
	if id == "" {
		id = uuid.NewString()
	}
	if _, err := exec.ExecContext(ctx, `
		INSERT INTO agent_decision_jobs (id, unit_id, session_id, world_id, region_id, status, tick, attempt, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, job.UnitID, nullable(job.SessionID), nullable(job.WorldID), nullable(job.RegionID), StatusPending, job.Tick, job.Attempt, nowTS()); err != nil {
		return "", fmt.Errorf("enqueue decision job for %s: %w", job.UnitID, err)
	}
	return id, nil
}

// EnqueueJob 入队一条 pending 决策作业，返回作业 ID。
func EnqueueJob(ctx context.Context, db *sql.DB, job DecisionJob) (string, error) {
	if job.UnitID == "" {
		return "", fmt.Errorf("enqueue job: empty unit id")
	}
	return insertPendingJob(ctx, db, job)
}

// PromoteWakeToJob 把一条到点的唤醒**原子地**出队成决策作业：单事务内 DELETE wake + INSERT pending job。
// 杜绝「删了 wake 却没入 job」（两步非事务在崩溃/出错缺口下让单位永久丢失、再不被唤醒）。返回作业 ID。
// region-runner schedulePass 用它替代裸 RemoveWake+EnqueueJob 两步。双驱动：BeginTx 在 SQLite(单连接)/MySQL 均原子。
func PromoteWakeToJob(ctx context.Context, db *sql.DB, w WakeEntry, tick int64) (string, error) {
	if w.UnitID == "" {
		return "", fmt.Errorf("promote wake: empty unit id")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("promote wake (begin tx): %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM agent_wake_queue WHERE unit_id = ?`, w.UnitID); err != nil {
		return "", fmt.Errorf("promote wake (remove): %w", err)
	}
	id, err := insertPendingJob(ctx, tx, DecisionJob{UnitID: w.UnitID, SessionID: w.SessionID, WorldID: w.WorldID, RegionID: w.RegionID, Tick: tick})
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("promote wake (commit): %w", err)
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
		RETURNING id, unit_id, session_id, world_id, region_id, tick, attempt`,
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
	var sessionID, worldID, regionID sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT unit_id, session_id, world_id, region_id, tick, attempt FROM agent_decision_jobs WHERE id = ?`, id).
		Scan(&job.UnitID, &sessionID, &worldID, &regionID, &job.Tick, &job.Attempt); err != nil {
		return nil, fmt.Errorf("claim next job (reload): %w", err)
	}
	job.SessionID = sessionID.String
	job.WorldID = worldID.String
	job.RegionID = regionID.String
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim next job (commit): %w", err)
	}
	return job, nil
}

// ClaimNextJobInRegion 是 ClaimNextJob 的 **region 维度** 变体（沙盘 §8.2「per-region 唯一处理者」接线点）：
// 只原子认领 region_id == 给定 regionID 的下一条 pending 作业。region-runner 多实例分片时，每个实例仅认领自己
// 持租 region 的作业，避免「实例 A 持 region-1 租约却处理了 region-2 的单位」破坏 per-region 单写者不变量。
//
// 语义与 ClaimNextJob 完全一致（pending→running、FIFO created_at 升序、双驱动原子认领），仅多一个 region_id 等值谓词。
// regionID 为空时返回错误（空 region 作业不该被 region-scoped 认领；区分于全局 ClaimNextJob 的「认领任意作业」语义）。
// 不改既有 ClaimNextJob——后者仍是 flag 关（单实例）时 region-runner 的零行为变化默认路径。
func ClaimNextJobInRegion(ctx context.Context, db *sql.DB, regionID string) (*DecisionJob, error) {
	if regionID == "" {
		return nil, fmt.Errorf("claim next job in region: empty region id")
	}
	claimedAt := nowTS()
	if dbdialect.IsMySQL(db) {
		return claimNextJobInRegionMySQL(ctx, db, regionID, claimedAt)
	}
	row := db.QueryRowContext(ctx, `
		UPDATE agent_decision_jobs SET status = ?, claimed_at = ?
		WHERE id = (SELECT id FROM agent_decision_jobs WHERE status = ? AND region_id = ? ORDER BY created_at ASC, id ASC LIMIT 1)
		RETURNING id, unit_id, session_id, world_id, region_id, tick, attempt`,
		StatusRunning, claimedAt, StatusPending, regionID)
	job, err := scanClaimedJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim next job in region %s (sqlite): %w", regionID, err)
	}
	return job, nil
}

func claimNextJobInRegionMySQL(ctx context.Context, db *sql.DB, regionID, claimedAt string) (*DecisionJob, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("claim next job in region (begin tx): %w", err)
	}
	defer tx.Rollback()

	var id string
	row := tx.QueryRowContext(ctx, `SELECT id FROM agent_decision_jobs WHERE status = ? AND region_id = ? ORDER BY created_at ASC, id ASC LIMIT 1 FOR UPDATE`, StatusPending, regionID)
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim next job in region %s (select for update): %w", regionID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_decision_jobs SET status = ?, claimed_at = ? WHERE id = ?`, StatusRunning, claimedAt, id); err != nil {
		return nil, fmt.Errorf("claim next job in region %s (update): %w", regionID, err)
	}
	job := &DecisionJob{ID: id, Status: StatusRunning}
	var sessionID, worldID, regionID2 sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT unit_id, session_id, world_id, region_id, tick, attempt FROM agent_decision_jobs WHERE id = ?`, id).
		Scan(&job.UnitID, &sessionID, &worldID, &regionID2, &job.Tick, &job.Attempt); err != nil {
		return nil, fmt.Errorf("claim next job in region %s (reload): %w", regionID, err)
	}
	job.SessionID = sessionID.String
	job.WorldID = worldID.String
	job.RegionID = regionID2.String
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("claim next job in region %s (commit): %w", regionID, err)
	}
	return job, nil
}

func scanClaimedJob(row *sql.Row) (*DecisionJob, error) {
	job := &DecisionJob{Status: StatusRunning}
	var sessionID, worldID, regionID sql.NullString
	if err := row.Scan(&job.ID, &job.UnitID, &sessionID, &worldID, &regionID, &job.Tick, &job.Attempt); err != nil {
		return nil, err
	}
	job.SessionID = sessionID.String
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

// ReclaimStaleJobs 回收认领后超时未完成（worker 崩溃/退出）的 running 作业：claimed_at 早于 now-olderThan 的，
// attempt < maxAttempt 者置回 pending 并 attempt+1（可被重新认领），attempt >= maxAttempt 者置 failed（不再重试）。
// 返回 (reclaimed, failed) 计数。这是 §9 防「孤儿 running 单调抬高背压计数致 region-runner 饿死」的回收闸——
// region-runner 须定期调它。依赖定宽 claimed_at（tsLayout）做字符串比较，双驱动一致。
func ReclaimStaleJobs(ctx context.Context, db *sql.DB, olderThan time.Duration, maxAttempt int) (reclaimed int, failed int, err error) {
	cutoff := formatTS(time.Now().Add(-olderThan))

	// 先把已达重试上限者置 failed（attempt >= maxAttempt：再 +1 就超限）。
	failRes, err := db.ExecContext(ctx, `
		UPDATE agent_decision_jobs SET status = ?, completed_at = ?
		WHERE status = ? AND claimed_at IS NOT NULL AND claimed_at < ? AND attempt >= ?`,
		StatusFailed, nowTS(), StatusRunning, cutoff, maxAttempt)
	if err != nil {
		return 0, 0, fmt.Errorf("reclaim stale jobs (fail exhausted): %w", err)
	}
	failedN, err := failRes.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("reclaim stale jobs (fail rows): %w", err)
	}

	// 其余（attempt < maxAttempt）置回 pending + attempt+1 + 清 claimed_at，可被重新认领。
	reRes, err := db.ExecContext(ctx, `
		UPDATE agent_decision_jobs SET status = ?, attempt = attempt + 1, claimed_at = NULL
		WHERE status = ? AND claimed_at IS NOT NULL AND claimed_at < ? AND attempt < ?`,
		StatusPending, StatusRunning, cutoff, maxAttempt)
	if err != nil {
		return 0, int(failedN), fmt.Errorf("reclaim stale jobs (requeue): %w", err)
	}
	reclaimedN, err := reRes.RowsAffected()
	if err != nil {
		return 0, int(failedN), fmt.Errorf("reclaim stale jobs (requeue rows): %w", err)
	}
	return int(reclaimedN), int(failedN), nil
}
