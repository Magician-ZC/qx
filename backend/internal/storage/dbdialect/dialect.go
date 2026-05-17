package dbdialect

// 文件说明：记录 database/sql 连接对应的 SQL 方言，供少量不兼容 SQL 分支使用。

import (
	"database/sql"
	"sync"
)

// Dialect 描述当前数据库方言。
type Dialect string

const (
	DialectSQLite Dialect = "sqlite"
	DialectMySQL  Dialect = "mysql"
)

var registry sync.Map

// Register 记录数据库连接对应的方言。
func Register(db *sql.DB, dialect Dialect) {
	if db == nil || dialect == "" {
		return
	}
	registry.Store(db, dialect)
}

// For 返回数据库连接方言，未注册时按 SQLite 兼容处理。
func For(db *sql.DB) Dialect {
	if db == nil {
		return DialectSQLite
	}
	if value, ok := registry.Load(db); ok {
		if dialect, ok := value.(Dialect); ok && dialect != "" {
			return dialect
		}
	}
	return DialectSQLite
}

// IsMySQL 判断数据库连接是否为 MySQL/MariaDB。
func IsMySQL(db *sql.DB) bool {
	return For(db) == DialectMySQL
}
