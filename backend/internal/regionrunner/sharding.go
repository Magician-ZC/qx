// 文件说明：region 租约管理器（大世界沙盘 §8.2「per-region 唯一处理者」/ §11.2「③单 region 单写者串行」吞吐地基切片）。
//
// 背景：region-runner 推进世界 tick 时，**一个 region 必须由唯一的 runner 实例/goroutine 独占处理**，否则两个实例
// 同时唤醒同一区单位 → 决策双发、状态写竞态、背压计数错乱。本文件提供 LeaseManager：用 SPEC 的 region_leases 表做
// **原子抢租**（DB 主键冲突 + expires_at 时间窗做互斥），让 runner 多实例/多 goroutine 安全 shard 同一张 DB。
//
// **本切片范围（务必看清边界）**：仅提供 LeaseManager 抢/续/释租原语，**不改 regionrunner.go 主循环**——真正的
// 多实例编排（leader 选举、region→实例 的稳定哈希分配、跨实例 rebalance、租约心跳 goroutine）是更大工程，留待后续。
// 接线点：未来 runner 在 advanceRegion 前 AcquireLease(regionID)，持租期间周期性 RenewLease，退出/让位时 ReleaseLease；
// 抢不到（false）则跳过该 region（别的实例在处理）。
//
// flag QUNXIANG_REGION_LEASES 默认关 → AcquireLease/RenewLease 恒返回 true（单实例向后兼容，零行为变化、零 DB 写）。
// 开启后才真正落库抢租。确定性：时间经注入式时钟（now），不调全局 time.Now（除默认构造），可固定时钟做确定性测试。
package regionrunner

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
)

// regionLeasesFlagEnv 是 region 租约的进程级开关。默认关 → 抢/续租恒成功（单实例兼容）。
const regionLeasesFlagEnv = "QUNXIANG_REGION_LEASES"

// leaseTSLayout 是定宽时间布局（纳秒补零 + 显式时区），字典序 == 时间序——expires_at 的字符串比较
// （抢过期租约用 expires_at < now）双驱动一致。对齐 agentqueue 的 tsLayout，避免 DATETIME 函数方言差异。
const leaseTSLayout = "2006-01-02T15:04:05.000000000Z07:00"

// LeaseManager 用 region_leases 表对 region 做独占租约抢占（沙盘 §8.2 per-region 唯一处理者）。
// 自包含、双驱动（SQLite/MySQL，dbdialect 分支）、确定性（注入式时钟）。
type LeaseManager struct {
	db  *sql.DB
	now func() time.Time // 注入式时钟（测试用固定时钟；默认 time.Now）
}

// NewLeaseManager 构造租约管理器（默认真实时钟）。db 为 nil 时仍可用（flag 关时不触 DB；flag 开时返回错误）。
func NewLeaseManager(db *sql.DB) *LeaseManager {
	return &LeaseManager{db: db, now: time.Now}
}

// withClock 返回固定时钟的副本（测试用，确定性抢租/过期窗口）。
func (m *LeaseManager) withClock(now func() time.Time) *LeaseManager {
	clone := *m
	if now != nil {
		clone.now = now
	}
	return &clone
}

// leasesEnabled 读 QUNXIANG_REGION_LEASES（true/1/yes/on 视为开，大小写不敏感、忽略首尾空白），默认关。
// 自包含解析，不引入外部依赖（对齐 billing/compliance/content_safety 的 flag 解析惯用法）。
func leasesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(regionLeasesFlagEnv))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// formatLeaseTS 把时刻格式化为定宽可字典序比较的 UTC 串。
func formatLeaseTS(t time.Time) string { return t.UTC().Format(leaseTSLayout) }

// AcquireLease 尝试为 holderID 抢占 regionID 的租约，有效期 ttl。抢到返回 true，被别的 holder 持租返回 false。
//
// flag 关 → 恒返回 true（单实例兼容，不触 DB）。flag 开时双驱动**原子抢租**两步走：
//  1. 先尝试 INSERT region_leases（region 从未被租过 → 插入成功 → 抢到）；
//  2. 主键冲突（已有行）→ 仅当 expires_at IS NULL 或 expires_at < now（租约已过期/无主）时 UPDATE 抢过期租约；
//     谓词同时放行「自己已持租」(holder = holderID) 以支持重入式续租。RowsAffected == 1 才算抢到。
//
// 互斥保证：步骤 2 的 UPDATE 带 expires_at 时间窗谓词，未过期且属于他人的租约 RowsAffected == 0 → 抢租失败。
// 注：单步 INSERT...ON CONFLICT DO UPDATE 无法表达「仅当过期才更新」的条件谓词（DO UPDATE 总会执行），
// 故拆成 INSERT 失败再条件 UPDATE 两步；并发下两个实例同时走步骤 2 时，DB 行锁 + expires_at 谓词仍保证至多一个 RowsAffected==1。
func (m *LeaseManager) AcquireLease(ctx context.Context, regionID, holderID string, ttl time.Duration) (bool, error) {
	if !leasesEnabled() {
		return true, nil // flag 关 → 单实例兼容，恒抢到
	}
	if m.db == nil {
		return false, fmt.Errorf("acquire lease %s: region leases enabled but db is nil", regionID)
	}
	if strings.TrimSpace(regionID) == "" {
		return false, fmt.Errorf("acquire lease: empty region id")
	}
	now := m.now()
	nowStr := formatLeaseTS(now)
	expiresStr := formatLeaseTS(now.Add(ttl))

	// 步骤 1：尝试 INSERT 抢占空区。主键冲突走步骤 2，其它错误直接返回。
	insertOK, err := m.tryInsertLease(ctx, regionID, holderID, nowStr, expiresStr)
	if err != nil {
		return false, err
	}
	if insertOK {
		return true, nil
	}

	// 步骤 2：行已存在 → 仅当过期/无主/自己持租时条件 UPDATE 抢过来。RowsAffected==1 才算抢到。
	res, err := m.db.ExecContext(ctx, `
		UPDATE region_leases
		SET holder = ?, expires_at = ?, updated_at = ?
		WHERE region_id = ? AND (expires_at IS NULL OR expires_at = '' OR expires_at < ? OR holder = ?)`,
		holderID, expiresStr, nowStr, regionID, nowStr, holderID)
	if err != nil {
		return false, fmt.Errorf("acquire lease %s (steal expired): %w", regionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acquire lease %s rows affected: %w", regionID, err)
	}
	return affected == 1, nil
}

// tryInsertLease 尝试 INSERT 一条新租约。返回 (true,nil) 表示插入成功（抢到空区）；
// 返回 (false,nil) 表示主键冲突（行已存在，交步骤 2 处理）；返回 (false,err) 表示真实错误。
// 双驱动：用 INSERT ... ON CONFLICT/ON DUPLICATE ... DO NOTHING 把主键冲突收敛为 RowsAffected==0（不报错），
// 从而无需靠驱动相关的错误码识别唯一键冲突（modernc sqlite 与 go-sql-driver/mysql 错误形态不同）。
func (m *LeaseManager) tryInsertLease(ctx context.Context, regionID, holderID, nowStr, expiresStr string) (bool, error) {
	query := `INSERT INTO region_leases (region_id, holder, expires_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(region_id) DO NOTHING`
	if dbdialect.IsMySQL(m.db) {
		// MySQL 无 ON CONFLICT；INSERT IGNORE 把唯一键冲突降级为 0 行受影响的告警（不报错）。
		query = `INSERT IGNORE INTO region_leases (region_id, holder, expires_at, updated_at)
		VALUES (?, ?, ?, ?)`
	}
	res, err := m.db.ExecContext(ctx, query, regionID, holderID, expiresStr, nowStr)
	if err != nil {
		return false, fmt.Errorf("acquire lease %s (insert): %w", regionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("acquire lease %s insert rows: %w", regionID, err)
	}
	return affected == 1, nil
}

// RenewLease 续租：仅当 holderID 仍持有 regionID 的租约时，把 expires_at 顺延 ttl。续到返回 true，
// 否则（租约已被别人抢走 / 不存在）返回 false——runner 据此感知「已失去区锁」应停止处理该 region。
//
// flag 关 → 恒返回 true（不触 DB）。谓词 holder = holderID 保证只能续自己的租约（哪怕已过期但还没被别人抢，也允许续回）。
func (m *LeaseManager) RenewLease(ctx context.Context, regionID, holderID string, ttl time.Duration) (bool, error) {
	if !leasesEnabled() {
		return true, nil
	}
	if m.db == nil {
		return false, fmt.Errorf("renew lease %s: region leases enabled but db is nil", regionID)
	}
	if strings.TrimSpace(regionID) == "" {
		return false, fmt.Errorf("renew lease: empty region id")
	}
	now := m.now()
	res, err := m.db.ExecContext(ctx, `
		UPDATE region_leases
		SET expires_at = ?, updated_at = ?
		WHERE region_id = ? AND holder = ?`,
		formatLeaseTS(now.Add(ttl)), formatLeaseTS(now), regionID, holderID)
	if err != nil {
		return false, fmt.Errorf("renew lease %s: %w", regionID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("renew lease %s rows affected: %w", regionID, err)
	}
	return affected == 1, nil
}

// ReleaseLease 主动释放 regionID 的租约（runner 退出/让位时调用），仅释放 holderID 自己持有的那把。
// 实现为「把 expires_at 置空 + holder 清空」而非 DELETE：保留行便于审计/索引复用，且让别的实例立刻可抢
// （expires_at IS NULL 命中 AcquireLease 步骤 2 的过期谓词）。flag 关 → no-op（不触 DB）。
func (m *LeaseManager) ReleaseLease(ctx context.Context, regionID, holderID string) error {
	if !leasesEnabled() {
		return nil
	}
	if m.db == nil {
		return fmt.Errorf("release lease %s: region leases enabled but db is nil", regionID)
	}
	if strings.TrimSpace(regionID) == "" {
		return fmt.Errorf("release lease: empty region id")
	}
	if _, err := m.db.ExecContext(ctx, `
		UPDATE region_leases
		SET holder = '', expires_at = NULL, updated_at = ?
		WHERE region_id = ? AND holder = ?`,
		formatLeaseTS(m.now()), regionID, holderID); err != nil {
		return fmt.Errorf("release lease %s: %w", regionID, err)
	}
	return nil
}
