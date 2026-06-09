package sqlite

// 文件说明：SQLite 存储入口，负责数据库打开、schema 应用与本地文件目录初始化。

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/storage/dbmigrate"
)

//go:embed schema.sql
var schemaFS embed.FS

// Open 打开 SQLite 数据库并应用嵌入式 schema。
func Open(path string) (*sql.DB, error) {
	if err := ensureParentDir(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	dbdialect.Register(db, dbdialect.DialectSQLite)

	// SQLite 连接池：**保持单连接**（沙盘 §11.2「②连接硬顶」语境）。
	// SetMaxOpenConns(1) 不是性能保守，而是 WAL 模式单写者约束的必然：modernc.org/sqlite 同库并发写会 SQLITE_BUSY/锁竞争，
	// 单连接把写串行化交给 database/sql 池本身（最稳、最可预测），故此处**故意不调高**——多实例扩展靠切 MySQL，不靠多开 SQLite 连接。
	// 仅把 idle/lifetime 设为有限值便于连接健康回收（单连接下影响小，但对齐 MySQL 侧策略、避免长寿连接持锁假死）。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}

	if err := applySchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 大世界双键迁移：给 events 幂等补 world_id/region_id/tick（加列不改义，沙盘 §8.7）。
	if err := dbmigrate.EnsureColumns(ctx, db, "events", dbmigrate.EventScopeColumns); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 大世界单位作用域迁移：给 units 幂等补 world_id/region_id/life_state/last_active_tick（双写灰度，沙盘 §8.7）。
	if err := dbmigrate.EnsureColumns(ctx, db, "units", dbmigrate.UnitScopeColumns); err != nil {
		_ = db.Close()
		return nil, err
	}

	// region-runner 调度队列迁移：给两表幂等补 session_id（保留期清理键，M7.3-real-0）。
	for _, table := range []string{"agent_wake_queue", "agent_decision_jobs"} {
		if err := dbmigrate.EnsureColumns(ctx, db, table, dbmigrate.AgentQueueSessionColumn); err != nil {
			_ = db.Close()
			return nil, err
		}
	}

	// 账户成本闭环迁移：给 single_player_sessions 幂等补 account_id（nullable，兼容匿名旧局）。
	if err := dbmigrate.EnsureColumns(ctx, db, "single_player_sessions", dbmigrate.SessionAccountColumn); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 相关性锚持久表（存量旧库补建；fresh 库 schema.sql 已建）——否则持久锚 silently 永不落库/加载。
	if err := dbmigrate.EnsureTable(ctx, db, dbmigrate.RelevanceAnchorsTableSQLite, dbmigrate.RelevanceAnchorsTableMySQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	// product_events 北极星/A-B 维度列（user_id/ab_bucket/client_ts/app_version，幂等补列）。
	if err := dbmigrate.EnsureColumns(ctx, db, "product_events", dbmigrate.ProductEventAnalyticsColumns); err != nil {
		_ = db.Close()
		return nil, err
	}
	// 设计闭环新增表（副本异步分段 / Live-Ops 赛季 / GM 审计）：存量旧库幂等补建。
	for _, t := range dbmigrate.DesignClosureTables {
		if err := dbmigrate.EnsureTable(ctx, db, t.SQLite, t.MySQL); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	// 评审修复补列：dungeon_segments.entered_turn（L1 续跑确定性）、gm_events_audit.world_tick（L3 审计排序）。
	if err := dbmigrate.EnsureColumns(ctx, db, "dungeon_segments", dbmigrate.DungeonSegmentEnteredTurnColumn); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := dbmigrate.EnsureColumns(ctx, db, "gm_events_audit", dbmigrate.GmEventsAuditTickColumn); err != nil {
		_ = db.Close()
		return nil, err
	}
	// world_bosses 单世界至多一头 active 的硬兜底（L4，best-effort：存量重复 active 行致建索引失败时吞错，NOT EXISTS 仍守）。
	if _, err := db.ExecContext(ctx, dbmigrate.WorldBossActiveUniqueIndexSQLite); err != nil {
		log.Printf("ensure world_boss active unique index best-effort failed: %v", err)
	}
	// Wave2 跨玩家：cross_events 设计列 + social_objects 撮合列 + cross_event_echoes 视角层表。
	if err := dbmigrate.EnsureColumns(ctx, db, "cross_events", dbmigrate.CrossEventColumns); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := dbmigrate.EnsureColumns(ctx, db, "social_objects", dbmigrate.SocialObjectColumns); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := dbmigrate.EnsureTable(ctx, db, dbmigrate.CrossEventEchoesTableSQLite, dbmigrate.CrossEventEchoesTableMySQL); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Wave3 命运：M1 data-driven 翻译矩阵表。
	if err := dbmigrate.EnsureTable(ctx, db, dbmigrate.TranslationTemplatesTableSQLite, dbmigrate.TranslationTemplatesTableMySQL); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// ensureParentDir 确保数据库文件父目录存在。
func ensureParentDir(path string) error {
	parent := filepath.Dir(path)
	if parent == "." {
		return nil
	}

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create sqlite parent dir: %w", err)
	}

	return nil
}

// applySchema 逐条执行 schema.sql 中的 SQL 语句。
func applySchema(ctx context.Context, db *sql.DB) error {
	bytes, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read sqlite schema: %w", err)
	}

	for _, statement := range strings.Split(string(bytes), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}

		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply sqlite schema statement %q: %w", statement, err)
		}
	}

	return nil
}
