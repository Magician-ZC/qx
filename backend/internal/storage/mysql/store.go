package mysql

// 文件说明：MySQL/MariaDB 存储入口，负责数据库打开、连接池配置与 schema 应用。

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
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
	// region-runner 调度队列迁移：给两表幂等补 session_id（保留期清理键，M7.3-real-0）。
	for _, table := range []string{"agent_wake_queue", "agent_decision_jobs"} {
		if err := dbmigrate.EnsureColumns(ctx, db, table, dbmigrate.AgentQueueSessionColumn); err != nil {
			_ = db.Close()
			return nil, err
		}
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
