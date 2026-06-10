package mysql

// 文件说明：MySQL/MariaDB 存储入口，负责数据库打开、连接池配置与 schema 应用。

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/storage/dbmigrate"
)

// 连接池默认上限（沙盘 §11.2「②连接硬顶」「立即可做的高 ROI 改动③」：MySQL SetMaxOpenConns(8) 调到 64–128）。
// 原 8/8 在玩家扎堆同 region 时是第二瓶颈（仅次于巨型 state_json）；提到 64 open / 16 idle 给读/写/WS 留并发空间。
// open 可由 env QUNXIANG_MYSQL_MAX_OPEN 覆盖（如压测调参）；idle 取 open 的 1/4（夹在 [1, open]）。
const (
	defaultMySQLMaxOpenConns = 64
	mysqlMaxOpenConnsEnv     = "QUNXIANG_MYSQL_MAX_OPEN"
)

//go:embed schema.sql
var schemaFS embed.FS

// Open 打开 MySQL 数据库并应用嵌入式 schema。
func Open(dsn string) (*sql.DB, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("mysql dsn is required")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql database: %w", err)
	}
	dbdialect.Register(db, dbdialect.DialectMySQL)

	// 连接池调优（沙盘 §11.2 立即可做高 ROI③）：open 上限提到 64（env 可覆盖），idle 取 open 的 1/4，
	// 并设有限 lifetime（原 SetConnMaxLifetime(0) = 永不过期，长寿连接易踩中间件/MySQL wait_timeout 静默断连）。
	maxOpen := resolveMySQLMaxOpenConns()
	maxIdle := maxOpen / 4
	if maxIdle < 1 {
		maxIdle = 1
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(30 * time.Minute)
	db.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql database: %w", err)
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
	// 共享世界 Phase 2「玩家相遇」索引：按 (region_id, life_state) 查某区在世单位（ListActiveByRegion 跨 session 拉同区玩家）。
	// 列序前导 region_id：查询 WHERE 不含 world_id（复合 region_id=worldID#zoneID 已自带世界前缀，列冗余），
	// world_id 前导会按最左前缀规则使索引失效（退化全表扫描）；末列 last_active_tick 覆盖 ORDER BY。
	// 改名（旧名 idx_units_world_region 列序错失效）+先删旧索引：EnsureIndex 仅按名字查重，存量库若已建旧名索引，
	// 不改名只换列会被「名字已存在」静默跳过——故必须 DropIndex 清旧名 + 用新名建对（须在上方补列之后——索引列须先存在）。
	if err := dbmigrate.DropIndex(ctx, db, "units", "idx_units_world_region"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := dbmigrate.EnsureIndex(ctx, db, "units", "idx_units_region_active", "region_id", "life_state", "last_active_tick"); err != nil {
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
	// 大世界页游入口迁移：给 single_player_sessions 幂等补 world_id（nullable，兼容未接入多世界的旧局）。
	if err := dbmigrate.EnsureColumns(ctx, db, "single_player_sessions", dbmigrate.SessionWorldColumn); err != nil {
		_ = db.Close()
		return nil, err
	}
	// 复合查询索引：按 (account_id, world_id) 查「该账号在某世界的角色 session」（GET /api/me/character resume）。幂等。
	if err := dbmigrate.EnsureIndex(ctx, db, "single_player_sessions", "idx_single_player_sessions_account_world", "account_id", "world_id"); err != nil {
		_ = db.Close()
		return nil, err
	}
	// 账号绑定持久角色并发降生 TOCTOU 硬兜底（评审 W-E，best-effort：存量重复行致建唯一索引失败时吞错，query-first 仍守）。
	if err := dbmigrate.EnsureSinglePlayerAccountWorldUnique(ctx, db); err != nil {
		log.Printf("ensure single_player account-world unique index best-effort failed: %v", err)
	}
	// 共享世界 Phase4「共享进度」#1：world_bosses「每世界每区至多一头 active」唯一硬兜底（MySQL 侧）。
	// SQLite 用 partial unique index（sqlite/store.go），MySQL 无 partial index → 用 STORED 生成列 active_region_key
	// （active→region_id，否则→id）+ (world_id, active_region_key) 唯一键等价之。Phase4 把 zone boss 改成玩家直驱
	// get-or-create（每次挑战都可能并发首插），MySQL gap-lock 下两个 `WHERE NOT EXISTS` 都见 0→双插劈裂共享血池——
	// 此唯一键让第二头必触冲突，由 ensureSharedZoneBoss 的 isDupKeyErr 分支收敛兜底（再查既有行）。
	// best-effort：存量库若已有同区重复 active 行致建索引失败，吞错即可（NOT EXISTS 仍是主护栏）。
	if err := dbmigrate.EnsureWorldBossActiveUnique(ctx, db); err != nil {
		log.Printf("ensure world_boss active unique index best-effort failed: %v", err)
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

// resolveMySQLMaxOpenConns 解析连接池 open 上限：env QUNXIANG_MYSQL_MAX_OPEN 为正整数时覆盖，否则用默认 64。
// 非法/非正值（空、负、零、非数字）一律回退默认，保证永不把池压成不可用的 ≤0。
func resolveMySQLMaxOpenConns() int {
	raw := strings.TrimSpace(os.Getenv(mysqlMaxOpenConnsEnv))
	if raw == "" {
		return defaultMySQLMaxOpenConns
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultMySQLMaxOpenConns
	}
	return n
}

func applySchema(ctx context.Context, db *sql.DB) error {
	bytes, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read mysql schema: %w", err)
	}
	for _, statement := range strings.Split(string(bytes), ";") {
		statement = stripSQLLineComments(statement)
		if statement == "" {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply mysql schema statement %q: %w", statement, err)
		}
	}
	return nil
}

// stripSQLLineComments 逐行剥掉整行 `--` 注释后返回 trim 的语句。
// 修正原「整块 TrimSpace 后以 -- 开头即跳过」的缺陷：按 ; 切分后，「注释行 + CREATE TABLE」会成为同一块、块首是注释，
// 被整块跳过 → 该表在 MySQL 下从未建表（曾致 agent_wake_queue 缺失、region-runner 队列在 MySQL 部署失效，real-5 发现）。
// 逐行剥离后纯注释块自然变空被跳过，而「注释 + 语句」块保留语句正常执行。仅剥整行注释，不动行内 SQL（schema DDL 无字符串内 --）。
func stripSQLLineComments(statement string) string {
	lines := strings.Split(statement, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
