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

// —— 设计闭环新增表（2026-06-09）：副本异步分段 / Live-Ops 赛季 / GM 审计。
// schema.sql 的 fresh 库已含这些表；以下供存量旧库经 EnsureTable 幂等补建（CREATE TABLE IF NOT EXISTS 双驱动安全）。

// DungeonSegmentsTable* 副本异步分段态（PvE威胁系统 §3）。列严格对齐 session/dungeon_segment.go 的 INSERT/SELECT。
const DungeonSegmentsTableSQLite = `
CREATE TABLE IF NOT EXISTS dungeon_segments (
  id TEXT PRIMARY KEY,
  dungeon_run_id TEXT NOT NULL,
  session_id TEXT NOT NULL,
  unit_ids_json TEXT NOT NULL DEFAULT '[]',
  floors INTEGER NOT NULL DEFAULT 1,
  floor INTEGER NOT NULL DEFAULT 1,
  entered_turn INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'in_progress',
  members_state_json TEXT NOT NULL DEFAULT '[]',
  boss_hp_remaining INTEGER NOT NULL DEFAULT 0,
  floor_round INTEGER NOT NULL DEFAULT 0,
  awards_accumulated_json TEXT NOT NULL DEFAULT '[]',
  pause_reason TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  left_at TEXT,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const DungeonSegmentsTableMySQL = `
CREATE TABLE IF NOT EXISTS dungeon_segments (
  id VARCHAR(191) PRIMARY KEY,
  dungeon_run_id VARCHAR(191) NOT NULL,
  session_id VARCHAR(191) NOT NULL,
  unit_ids_json TEXT NOT NULL,
  floors INT NOT NULL DEFAULT 1,
  floor INT NOT NULL DEFAULT 1,
  entered_turn INT NOT NULL DEFAULT 0,
  state VARCHAR(48) NOT NULL DEFAULT 'in_progress',
  members_state_json MEDIUMTEXT NOT NULL,
  boss_hp_remaining INT NOT NULL DEFAULT 0,
  floor_round INT NOT NULL DEFAULT 0,
  awards_accumulated_json TEXT NOT NULL,
  pause_reason VARCHAR(64) NOT NULL DEFAULT '',
  started_at VARCHAR(64) NOT NULL DEFAULT '',
  left_at VARCHAR(64) NULL,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_dungeon_segments_session (session_id, state),
  INDEX idx_dungeon_segments_run (dungeon_run_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// SeasonsTable* Live-Ops 赛季（产品方案PRD §8）。
const SeasonsTableSQLite = `
CREATE TABLE IF NOT EXISTS seasons (
  id TEXT PRIMARY KEY,
  world_id TEXT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  started_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  ends_at TEXT NOT NULL DEFAULT '',
  burn_in_started_at TEXT NOT NULL DEFAULT '',
  burn_in_ended_at TEXT NOT NULL DEFAULT '',
  content_theme_id TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const SeasonsTableMySQL = `
CREATE TABLE IF NOT EXISTS seasons (
  id VARCHAR(191) PRIMARY KEY,
  world_id VARCHAR(191) NOT NULL,
  name VARCHAR(191) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  started_at VARCHAR(64) NOT NULL DEFAULT '',
  ends_at VARCHAR(64) NOT NULL DEFAULT '',
  burn_in_started_at VARCHAR(64) NOT NULL DEFAULT '',
  burn_in_ended_at VARCHAR(64) NOT NULL DEFAULT '',
  content_theme_id VARCHAR(191) NOT NULL DEFAULT '',
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_seasons_world (world_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// SeasonContentThemesTable* 赛季内容母题库。
const SeasonContentThemesTableSQLite = `
CREATE TABLE IF NOT EXISTS season_content_themes (
  id TEXT PRIMARY KEY,
  season_id TEXT NOT NULL,
  decisive_event_ids TEXT NOT NULL DEFAULT '[]',
  title_ids TEXT NOT NULL DEFAULT '[]',
  landmark_names TEXT NOT NULL DEFAULT '[]',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const SeasonContentThemesTableMySQL = `
CREATE TABLE IF NOT EXISTS season_content_themes (
  id VARCHAR(191) PRIMARY KEY,
  season_id VARCHAR(191) NOT NULL,
  decisive_event_ids TEXT NOT NULL,
  title_ids TEXT NOT NULL,
  landmark_names TEXT NOT NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_season_content_season (season_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// GmEventsAuditTable* GM 世界事件注入审计（append-only）。
const GmEventsAuditTableSQLite = `
CREATE TABLE IF NOT EXISTS gm_events_audit (
  id TEXT PRIMARY KEY,
  world_id TEXT NOT NULL,
  event_kind TEXT NOT NULL,
  cross_event_id TEXT NOT NULL DEFAULT '',
  world_tick INTEGER NOT NULL DEFAULT 0,
  payload_json TEXT NOT NULL DEFAULT '{}',
  created_by TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const GmEventsAuditTableMySQL = `
CREATE TABLE IF NOT EXISTS gm_events_audit (
  id VARCHAR(191) PRIMARY KEY,
  world_id VARCHAR(191) NOT NULL,
  event_kind VARCHAR(64) NOT NULL,
  cross_event_id VARCHAR(191) NOT NULL DEFAULT '',
  world_tick INT NOT NULL DEFAULT 0,
  payload_json TEXT NOT NULL,
  created_by VARCHAR(191) NOT NULL DEFAULT '',
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_gm_events_audit_world (world_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// DesignClosureTables 汇总本波新增表的双驱动 DDL，供 store.go 一次性幂等补建。
var DesignClosureTables = []struct{ SQLite, MySQL string }{
	{DungeonSegmentsTableSQLite, DungeonSegmentsTableMySQL},
	{SeasonsTableSQLite, SeasonsTableMySQL},
	{SeasonContentThemesTableSQLite, SeasonContentThemesTableMySQL},
	{GmEventsAuditTableSQLite, GmEventsAuditTableMySQL},
}

// DungeonSegmentEnteredTurnColumn 给 dungeon_segments 补 entered_turn（评审 L1：副本踏入回合钉死，
// 使 combat_roll 骰序与玩家何时回来无关——续跑确定性不随 live Turn 漂移）。供存量库（本波早些时候建过无此列）幂等补列。
var DungeonSegmentEnteredTurnColumn = []Column{
	{Name: "entered_turn", SQLiteType: "INTEGER NOT NULL DEFAULT 0", MySQLType: "INT NOT NULL DEFAULT 0"},
}

// GmEventsAuditTickColumn 给 gm_events_audit 补 world_tick（评审 L3：ListAudit 按权威单调 world_tick 排序，
// 取代不稳定的秒级/空串 created_at，使运营复核视图与 cross_events 注入序严格一致）。
var GmEventsAuditTickColumn = []Column{
	{Name: "world_tick", SQLiteType: "INTEGER NOT NULL DEFAULT 0", MySQLType: "INT NOT NULL DEFAULT 0"},
}

// CrossEventColumns 给 cross_events 补「跨玩家唯一事实源」的设计列（事件耦合 §3，全可空，append-only 语义不变）：
// prev_cross_event_id 复仇/证据链反指针；consent_state 同意档状态；interaction_type 七交互类型；
// social_object_id 所属社会客体；terms_json 交互条款；initiator/target_session_id 双方会话；score_* 零和裁决投入分。
var CrossEventColumns = []Column{
	{Name: "prev_cross_event_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "consent_state", SQLiteType: "TEXT", MySQLType: "VARCHAR(32) NULL"},
	{Name: "interaction_type", SQLiteType: "TEXT", MySQLType: "VARCHAR(32) NULL"},
	{Name: "social_object_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "terms_json", SQLiteType: "TEXT", MySQLType: "TEXT NULL"},
	{Name: "initiator_session_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "target_session_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "arbitration_key", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "score_initiator", SQLiteType: "REAL NOT NULL DEFAULT 0", MySQLType: "DOUBLE NOT NULL DEFAULT 0"},
	{Name: "score_target", SQLiteType: "REAL NOT NULL DEFAULT 0", MySQLType: "DOUBLE NOT NULL DEFAULT 0"},
}

// SocialObjectColumns 给 social_objects 补撮合所需列（事件耦合 §2.2，全可空/有默认）：
// region_id 地理就近择人；severity 严重度定 consent 档；expires_at 过期回收。
var SocialObjectColumns = []Column{
	{Name: "region_id", SQLiteType: "TEXT", MySQLType: "VARCHAR(191) NULL"},
	{Name: "severity", SQLiteType: "INTEGER NOT NULL DEFAULT 0", MySQLType: "INT NOT NULL DEFAULT 0"},
	{Name: "expires_at", SQLiteType: "TEXT", MySQLType: "VARCHAR(64) NULL"},
}

// CrossEventEchoesTable* 跨玩家事件的视角化叙事层（事件耦合 §2.7「echo 仅视角叙事，事实唯一回退 cross_events」）：
// 同一 cross_event_id 在多个 session 各有一条 echo（罗生门），但事实唯一——争议回退 cross_events 原表 occurred_at 仲裁。
const CrossEventEchoesTableSQLite = `
CREATE TABLE IF NOT EXISTS cross_event_echoes (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL,
  owner_unit_id TEXT NOT NULL,
  cross_event_id TEXT NOT NULL,
  relevance REAL NOT NULL DEFAULT 0,
  fate_score REAL NOT NULL DEFAULT 0,
  route TEXT NOT NULL DEFAULT '',
  narrative_zh TEXT NOT NULL DEFAULT '',
  valence REAL NOT NULL DEFAULT 0,
  hop INTEGER NOT NULL DEFAULT 0,
  read_at TEXT,
  created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const CrossEventEchoesTableMySQL = `
CREATE TABLE IF NOT EXISTS cross_event_echoes (
  id VARCHAR(191) PRIMARY KEY,
  session_id VARCHAR(191) NOT NULL,
  owner_unit_id VARCHAR(191) NOT NULL,
  cross_event_id VARCHAR(191) NOT NULL,
  relevance DOUBLE NOT NULL DEFAULT 0,
  fate_score DOUBLE NOT NULL DEFAULT 0,
  route VARCHAR(32) NOT NULL DEFAULT '',
  narrative_zh TEXT NOT NULL,
  valence DOUBLE NOT NULL DEFAULT 0,
  hop INT NOT NULL DEFAULT 0,
  read_at VARCHAR(64) NULL,
  created_at VARCHAR(64) NOT NULL DEFAULT '',
  INDEX idx_cross_echoes_owner (owner_unit_id, created_at),
  INDEX idx_cross_echoes_event (cross_event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// TranslationTemplatesTable* 是 M1 data-driven 翻译矩阵（事件耦合 §1.2「(reason_code × anchor_kind) → 命运 beat」）：
// 把宏观世界事件按命中锚类翻译成对她的个人命运 narrative，data-driven 可运营态补全，覆盖全 reason_code×anchor_kind 矩阵。
// force_pending=1 的组（如密友倒地 COMBAT_DOWN×relation）强制升级待决策；anchor_kind='' 为该 reason_code 的通用兜底模板。
const TranslationTemplatesTableSQLite = `
CREATE TABLE IF NOT EXISTS translation_templates (
  id TEXT PRIMARY KEY,
  reason_code TEXT NOT NULL,
  anchor_kind TEXT NOT NULL DEFAULT '',
  narrative_template TEXT NOT NULL,
  force_pending INTEGER NOT NULL DEFAULT 0,
  priority INTEGER NOT NULL DEFAULT 0,
  updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE (reason_code, anchor_kind)
)`

const TranslationTemplatesTableMySQL = `
CREATE TABLE IF NOT EXISTS translation_templates (
  id VARCHAR(191) PRIMARY KEY,
  reason_code VARCHAR(64) NOT NULL,
  anchor_kind VARCHAR(32) NOT NULL DEFAULT '',
  narrative_template TEXT NOT NULL,
  force_pending INT NOT NULL DEFAULT 0,
  priority INT NOT NULL DEFAULT 0,
  updated_at VARCHAR(64) NOT NULL DEFAULT '',
  UNIQUE KEY uq_translation_rc_ak (reason_code, anchor_kind)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`

// WorldBossActiveUniqueIndexSQLite 是「每世界至多一头 active boss」的 SQLite partial unique index
// （评审 L4：给默认驱动一道硬兜底，NOT EXISTS 之外再加唯一冲突拦截）。best-effort 执行：
// 存量库已有重复 active 行时建索引会失败——吞错即可（NOT EXISTS 仍是主护栏）。MySQL 无 partial index，
// 不在此补（gap-lock 理论竞态属 flag-off 低危 documented residual）。
const WorldBossActiveUniqueIndexSQLite = `CREATE UNIQUE INDEX IF NOT EXISTS uq_world_boss_active ON world_bosses(world_id) WHERE status='active'`

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
