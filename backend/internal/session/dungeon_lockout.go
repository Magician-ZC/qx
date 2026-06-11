package session

// 文件说明：副本进入冷却/每日次数（设计模块3）。给副本「开始一次」加一道按 (world, unit, dungeon, 日窗) 的进入闸：
// 每日（UTC 自然日）至多进入 dungeonDailyEntryCap 次，跨日自动重置（window_key=UTC 日期串）。
// 落表 dungeon_lockouts（双驱动 upsert：SQLite ON CONFLICT DO UPDATE / MySQL ON DUPLICATE KEY UPDATE）。
// flag QUNXIANG_DUNGEON_LOCKOUT 默认关——关时整套逻辑零行为（恒放行、remaining=-1）。
// best-effort：DB 故障时为了不卡玩家一律放行（allowed=true），但把 err 上抛供上层记录，绝不让闸把玩家锁死。
// time.Now().UTC() 仅用于 window_key（每日重置）与 last_entered_at（落库时刻），符合铁律的真实时间语义豁免。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/storage/dbdialect"
)

// dungeonDailyEntryCap 是单个 (world, unit, dungeon) 每日（UTC）的最大进入次数。
const dungeonDailyEntryCap = 3

// ErrDungeonDailyCapReached 由异步副本入口（StartDungeonAsync）在当日进入次数已尽时返回——不落段、不开打。
// 同步副本不返此错，而是用 DungeonResult.Outcome="locked" 表达（与既有 cleared/fled/wiped 同口径）。
var ErrDungeonDailyCapReached = fmt.Errorf("dungeon: 今日进入次数已尽（设 QUNXIANG_DUNGEON_LOCKOUT 控制）")

// dungeonLockoutEnabled 读 QUNXIANG_DUNGEON_LOCKOUT，默认开（显式 false/0/no/off 可关）。
// 关时 checkAndConsumeDungeonEntry 恒放行、remaining=-1（零行为变化）。
func dungeonLockoutEnabled() bool {
	return featureflags.EnabledWithDefault("QUNXIANG_DUNGEON_LOCKOUT", true)
}

// dungeonWindowKey 返回当前的每日时间窗键（UTC 自然日，形如 "2026-06-11"）——跨 UTC 自然日即换窗，进入次数自动重置。
func dungeonWindowKey() string {
	return time.Now().UTC().Format("2006-01-02")
}

// checkAndConsumeDungeonEntry 在副本「真正开打前」校验并消费一次进入名额。
//   - flag 关：恒 allowed=true、remaining=-1（不限、零行为变化），err=nil。
//   - flag 开：「是否放行」与「+1 消费」收敛进**一条原子 SQL**（cap 由 DB 守，不靠应用层前置读）——
//     仅当当日窗 entered_count < cap 时才真 +1 并放行，已满则不写库、不放行。
//   - best-effort：DB 故障时为不卡玩家返回 allowed=true，但 err 非 nil（上层记录后照常放行）；remaining 在故障时返回 -1（未知）。
//
// 并发正确性（修审计 TOCTOU）：早先实现是「SELECT entered_count → if count>=cap 判定 → upsert +1」三步——
// SELECT 与 upsert 间存在 TOCTOU 窗口：N 个并发请求都读到同一旧 count、都过 cap 检查、都 +1，实际放行远超 cap
// （实测同 (w,u,d) 并发 50 次、cap=3 放行 12 次）。这正是 maybeRefreshWorldBoss 用单条原子 SQL 修掉的同类竞态。
// 现把 cap 守门下沉到 DB：
//   - SQLite：INSERT ... ON CONFLICT DO UPDATE entered_count=entered_count+1 **WHERE entered_count<cap** RETURNING entered_count。
//     首次进入走 INSERT（count=1，必返一行）；已存在且未满 → DO UPDATE 真 +1 并 RETURNING 新值；已满 → WHERE 挡下 UPDATE，
//     ON CONFLICT 整体 no-op、RETURNING 无行（sql.ErrNoRows）→ 即「已打满、不放行」。放行与否仅由「本次是否真 +1（拿到新值）」决定。
//   - MySQL（无 RETURNING）：事务内 SELECT ... FOR UPDATE 锁行，按锁后真值判 cap，未满才 +1（或首次 INSERT），把判定与写在行锁下原子完成。
//
// 注意：唯一键四列含 world_id，业务务必传非空 world_id（共享世界恒有），否则 MySQL/SQLite 唯一键里 NULL 各视相异、去重失效。
func (service *Service) checkAndConsumeDungeonEntry(ctx context.Context, worldID, unitID, dungeonID string) (allowed bool, remaining int, err error) {
	if !dungeonLockoutEnabled() {
		return true, -1, nil
	}
	if service == nil || service.db == nil {
		// 依赖缺失：不该把玩家锁死，放行但回 err 供上层记录。
		return true, -1, fmt.Errorf("dungeon lockout: missing db")
	}

	window := dungeonWindowKey()
	// created_at 仅首次插入时落，last_entered_at 每次真 +1 时刷新为本次进入时刻（UTC）。
	now := time.Now().UTC().Format(time.RFC3339)

	if dbdialect.IsMySQL(service.db) {
		return service.consumeDungeonEntryMySQL(ctx, worldID, unitID, dungeonID, window, now)
	}
	return service.consumeDungeonEntrySQLite(ctx, worldID, unitID, dungeonID, window, now)
}

// consumeDungeonEntrySQLite 走 SQLite 的「条件 upsert + RETURNING」单条原子语句（见 checkAndConsumeDungeonEntry 注释）。
// 拿到新 entered_count → 本次真 +1（放行，remaining=cap-新值）；sql.ErrNoRows → WHERE 挡下（已满，不放行）；其余为真故障。
func (service *Service) consumeDungeonEntrySQLite(ctx context.Context, worldID, unitID, dungeonID, window, now string) (allowed bool, remaining int, err error) {
	var newCount int
	row := service.db.QueryRowContext(ctx, `
		INSERT INTO dungeon_lockouts (world_id, unit_id, dungeon_id, window_key, entered_count, last_entered_at, created_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON CONFLICT(world_id, unit_id, dungeon_id, window_key) DO UPDATE SET
			entered_count = entered_count + 1,
			last_entered_at = excluded.last_entered_at
		WHERE dungeon_lockouts.entered_count < ?
		RETURNING entered_count`,
		worldID, unitID, dungeonID, window, now, now, dungeonDailyEntryCap)
	if scanErr := row.Scan(&newCount); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			// ON CONFLICT 的 DO UPDATE 被 WHERE entered_count<cap 挡下、无行返回 → 当日已打满，不放行、不消耗。
			return false, 0, nil
		}
		// 真故障：放行但回 err（best-effort，不卡玩家）。
		return true, -1, fmt.Errorf("dungeon lockout: upsert: %w", scanErr)
	}
	return true, remainingFromCount(newCount), nil
}

// consumeDungeonEntryMySQL 走 MySQL 的「事务 + SELECT...FOR UPDATE 锁行 + 按锁后真值判 cap」原子消费
// （MySQL 无 RETURNING；与 strikeSharedBossCore 的 MySQL 分支同惯用法，避免 RowsAffected 歧义）。
func (service *Service) consumeDungeonEntryMySQL(ctx context.Context, worldID, unitID, dungeonID, window, now string) (allowed bool, remaining int, err error) {
	tx, txErr := service.db.BeginTx(ctx, nil)
	if txErr != nil {
		// 开事务失败：best-effort 放行（不卡玩家），回 err 供上层记录。
		return true, -1, fmt.Errorf("dungeon lockout: begin tx: %w", txErr)
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op

	// 锁住当日窗这一行（不存在则无行可锁，count 视作 0、走首次 INSERT）。
	var count int
	scanErr := tx.QueryRowContext(ctx, `
		SELECT entered_count FROM dungeon_lockouts
		WHERE world_id = ? AND unit_id = ? AND dungeon_id = ? AND window_key = ?
		FOR UPDATE`, worldID, unitID, dungeonID, window).Scan(&count)
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		return true, -1, fmt.Errorf("dungeon lockout: lock row: %w", scanErr)
	}
	if count >= dungeonDailyEntryCap {
		// 锁后真值已满：不放行、不写库（行锁下判定，无 TOCTOU）。
		return false, 0, nil
	}

	// 锁后真值未满（含尚无行）：原子 +1（首次 INSERT entered_count=1，否则 +1）。
	if _, execErr := tx.ExecContext(ctx, `
		INSERT INTO dungeon_lockouts (world_id, unit_id, dungeon_id, window_key, entered_count, last_entered_at, created_at)
		VALUES (?, ?, ?, ?, 1, ?, ?)
		ON DUPLICATE KEY UPDATE entered_count = entered_count + 1, last_entered_at = VALUES(last_entered_at)`,
		worldID, unitID, dungeonID, window, now, now); execErr != nil {
		return true, -1, fmt.Errorf("dungeon lockout: upsert: %w", execErr)
	}
	if commitErr := tx.Commit(); commitErr != nil {
		return true, -1, fmt.Errorf("dungeon lockout: commit: %w", commitErr)
	}
	// 提交成功 → 本次真 +1，新值 = 锁后旧值 + 1。
	return true, remainingFromCount(count + 1), nil
}

// remainingFromCount 由「+1 后的 entered_count 新值」算当日剩余名额，clamp 到 [0, cap-1]。
func remainingFromCount(newCount int) int {
	remaining := dungeonDailyEntryCap - newCount
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// dungeonEntriesRemaining 只读查当日窗某 (world, unit, dungeon) 的剩余进入名额。
//   - flag 关：恒返回 -1（不限）。
//   - flag 开：cap - 已进入次数，clamp 到 [0, cap]；DB 故障或依赖缺失同样返回 -1（未知，不阻拦）。
func (service *Service) dungeonEntriesRemaining(ctx context.Context, worldID, unitID, dungeonID string) int {
	if !dungeonLockoutEnabled() {
		return -1
	}
	if service == nil || service.db == nil {
		return -1
	}
	var count int
	row := service.db.QueryRowContext(ctx, `
		SELECT entered_count FROM dungeon_lockouts
		WHERE world_id = ? AND unit_id = ? AND dungeon_id = ? AND window_key = ?`,
		worldID, unitID, dungeonID, dungeonWindowKey())
	if scanErr := row.Scan(&count); scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return dungeonDailyEntryCap // 当日尚无进入，满额。
		}
		return -1 // 真故障：未知，不阻拦。
	}
	remaining := dungeonDailyEntryCap - count
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}
