// Package dbmigrate 提供幂等的「加列」迁移（大世界演进的最小迁移机制，沙盘 §8.7）。
//
// 背景：本项目 schema 用 CREATE TABLE IF NOT EXISTS 应用，对**已存在**的表不会加新列。
// 大世界需要给 events 等表加 world_id/region_id/tick 等列且不破坏现网库——本包检查现有列、只补缺的，
// 双驱动（SQLite / MySQL），可在每次 Open 安全重复执行。加列一律 nullable / 有默认值，绝不改既有语义。
package dbmigrate

import (
	"context"
	"database/sql"
	"fmt"

	"qunxiang/backend/internal/storage/dbdialect"
)

// Column 是一条待确保存在的列定义（双驱动类型分开给）。
type Column struct {
	Name       string
	SQLiteType string // 如 "TEXT" / "INTEGER NOT NULL DEFAULT 0"
	MySQLType  string // 如 "VARCHAR(191) NULL" / "BIGINT NOT NULL DEFAULT 0"
}

// EnsureColumns 确保 table 上存在给定列；缺哪个补哪个（ALTER TABLE ADD COLUMN），已存在的跳过。幂等。
func EnsureColumns(ctx context.Context, db *sql.DB, table string, cols []Column) error {
	if db == nil {
		return fmt.Errorf("ensure columns: nil db")
	}
	existing, err := existingColumns(ctx, db, table)
	if err != nil {
		return err
	}
	mysql := dbdialect.IsMySQL(db)
	for _, c := range cols {
		if existing[c.Name] {
			continue
		}
		typ := c.SQLiteType
		if mysql {
			typ = c.MySQLType
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, c.Name, typ)); err != nil {
			return fmt.Errorf("add column %s.%s: %w", table, c.Name, err)
		}
	}
	return nil
}

// existingColumns 返回某表已存在的列集合（双驱动）。
func existingColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	out := map[string]bool{}
	var rows *sql.Rows
	var err error
	if dbdialect.IsMySQL(db) {
		rows, err = db.QueryContext(ctx,
			`SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`, table)
	} else {
		rows, err = db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	}
	if err != nil {
		return nil, fmt.Errorf("inspect columns of %s: %w", table, err)
	}
	defer rows.Close()

	if dbdialect.IsMySQL(db) {
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			out[name] = true
		}
		return out, rows.Err()
	}
	// SQLite PRAGMA table_info: cid, name, type, notnull, dflt_value, pk
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// EnsureTable 幂等建表（双驱动：按方言选 DDL）：对**已存在**的表无副作用（CREATE TABLE IF NOT EXISTS），
// 对缺表的存量库补建。用于 schema.sql 后期新增、但现网旧库 applySchema 不会重跑全量的场景——
// 把建表 DDL 也纳入迁移，使老库与 fresh 库收敛到同一形态。可在每次 Open 安全重复执行。
func EnsureTable(ctx context.Context, db *sql.DB, sqliteDDL, mysqlDDL string) error {
	if db == nil {
		return fmt.Errorf("ensure table: nil db")
	}
	ddl := sqliteDDL
	if dbdialect.IsMySQL(db) {
		ddl = mysqlDDL
	}
	if _, err := db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("ensure table: %w", err)
	}
	return nil
}

// RelevanceAnchorsTableSQLite / RelevanceAnchorsTableMySQL 是相关性锚持久表的双驱动建表 DDL
// （列/主键严格对齐 session/anchors.go 的 UpsertAnchor INSERT 与 loadPersistentAnchors SELECT，
// 复合主键 (character_unit_id, anchor_kind, anchor_ref) 是 ON CONFLICT/ON DUPLICATE KEY 的依赖）。
// schema.sql 的 fresh 库已含本表，此处供存量旧库（升级前无此表）经迁移幂等补建，否则持久锚静默永不落库/加载。
const RelevanceAnchorsTableSQLite = `
CREATE TABLE IF NOT EXISTS relevance_anchors (
  character_unit_id TEXT NOT NULL,
  anchor_kind TEXT NOT NULL,
  anchor_ref TEXT NOT NULL,
  weight REAL NOT NULL DEFAULT 0,
  label TEXT NOT NULL DEFAULT '',
  half_life_days REAL NOT NULL DEFAULT 14,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (character_unit_id, anchor_kind, anchor_ref)
)`

const RelevanceAnchorsTableMySQL = `
CREATE TABLE IF NOT EXISTS relevance_anchors (
  character_unit_id VARCHAR(191) NOT NULL,
  anchor_kind VARCHAR(32) NOT NULL,
  anchor_ref VARCHAR(191) NOT NULL,
  weight DOUBLE NOT NULL DEFAULT 0,
  label VARCHAR(255) NOT NULL DEFAULT '',
  half_life_days DOUBLE NOT NULL DEFAULT 14,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  PRIMARY KEY (character_unit_id, anchor_kind, anchor_ref),
  INDEX idx_relevance_anchors_char (character_unit_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// ProductEventAnalyticsColumns 给 product_events 补北极星/A-B 口径列（全可空 TEXT，与游戏状态解耦）：
// user_id（按用户聚合留存/北极星）、ab_bucket（A-B 实验分桶）、client_ts（客户端原始时间戳，校时漂移分析）、
// app_version（按版本切片）。nullable —— 兼容历史无这些维度的旧埋点；列已存在则 EnsureColumns 安全跳过。
var ProductEventAnalyticsColumns = []Column{
	{Name: "user_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "ab_bucket", SQLiteType: "TEXT", MySQLType: "VARCHAR(64) NULL"},
	{Name: "client_ts", SQLiteType: "TEXT", MySQLType: "VARCHAR(64) NULL"},
	{Name: "app_version", SQLiteType: "TEXT", MySQLType: "VARCHAR(64) NULL"},
}

// EventScopeColumns 是 events 表的世界作用域双键列（沙盘 §8.7：加列不改义，Mutator/流程事件可双写）。
var EventScopeColumns = []Column{
	{Name: "world_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "region_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "tick", SQLiteType: "INTEGER NOT NULL DEFAULT 0", MySQLType: "BIGINT NOT NULL DEFAULT 0"},
}

// SessionAccountColumn 给 single_player_sessions 补 account_id 列（账户成本闭环列级落地）：
// State.AccountID 仅塞 state_json 无法按账户聚合/风控，故去规范化为可查询列。
// nullable —— 兼容现网匿名旧局（建局前无账户的历史 session 留空），由迁移幂等补列、不改既有语义。
var SessionAccountColumn = []Column{
	{Name: "account_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
}

// AgentQueueSessionColumn 给 region-runner 调度队列补 session_id 列（M7.3-real-0）：保留期清理按 session_id 收敛
// （与其余旁路表口径一致、与 region 取值解耦）。两表（agent_wake_queue/agent_decision_jobs）共用此列定义。
// 现网若已建过 626af1e 的无 session_id 旧表，靠本迁移补列（schema.sql 的新表已含此列，故 fresh 库幂等跳过）。
var AgentQueueSessionColumn = []Column{
	{Name: "session_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
}

// UnitScopeColumns 是 units 表的世界作用域 + 生命态调度列（沙盘 §8.7：加列不改义，双写灰度）。
// life_state 是 status_json.LifeState 的去规范化可查询索引（Save 每次从 status 同步）；world_id/region_id/
// last_active_tick 由调度层赋值（region-runner / HOT-WARM-COLD 分层 / wake 队列），用于「按 region 查在世单位」。
var UnitScopeColumns = []Column{
	{Name: "world_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "region_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "life_state", SQLiteType: "TEXT NOT NULL DEFAULT 'active'", MySQLType: "VARCHAR(32) NOT NULL DEFAULT 'active'"},
	{Name: "last_active_tick", SQLiteType: "INTEGER NOT NULL DEFAULT 0", MySQLType: "BIGINT NOT NULL DEFAULT 0"},
	// version：乐观并发版本号（M7.3-real-3-0），Save 单调 +1，SaveOptimistic 据此检测并发修改。
	{Name: "version", SQLiteType: "INTEGER NOT NULL DEFAULT 0", MySQLType: "BIGINT NOT NULL DEFAULT 0"},
}
