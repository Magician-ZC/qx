package mysql

// 文件说明：MySQL/MariaDB 存储入口，负责数据库打开、连接池配置与 schema 应用。

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/storage/dbmigrate"
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

	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(15 * time.Minute)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

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
	return db, nil
}

func applySchema(ctx context.Context, db *sql.DB) error {
	bytes, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read mysql schema: %w", err)
	}
	for _, statement := range strings.Split(string(bytes), ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" || strings.HasPrefix(statement, "--") {
			continue
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply mysql schema statement %q: %w", statement, err)
		}
	}
	return nil
}
