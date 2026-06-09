package session

// 文件说明：runtimeconfig 的双驱动持久化后端——把 GM 运行时设的类型化配置 override 落
// runtime_config_overrides 表，使「不重启即实时调参」在进程重启后存活。
// 与 FeatureFlagStore（admin_world.go）同构、同一双驱动 upsert 范式；实现 runtimeconfig.Store 接口
// （在 runtimeconfig 包内声明，本类型结构上满足，刻意不让 runtimeconfig 依赖 session/storage）。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"qunxiang/backend/internal/runtimeconfig"
	"qunxiang/backend/internal/storage/dbdialect"
)

// RuntimeConfigStore 把 runtimeconfig 的 override 落 runtime_config_overrides 表（双驱动），重启回灌存活。
type RuntimeConfigStore struct {
	db      *sql.DB
	dialect dbdialect.Dialect
}

// NewRuntimeConfigStore 构造持久化后端。dialect 由 db 推断（开新连接前应已 dbdialect.Register）。
func NewRuntimeConfigStore(db *sql.DB) *RuntimeConfigStore {
	return &RuntimeConfigStore{db: db, dialect: dbdialect.For(db)}
}

// Upsert 幂等写一条 override（按 name 主键 upsert，双驱动 ON CONFLICT / ON DUPLICATE KEY）。
func (s *RuntimeConfigStore) Upsert(ctx context.Context, name, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("runtime config store upsert: nil store")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("runtime config store upsert: empty name")
	}
	query := `
		INSERT INTO runtime_config_overrides (name, value, updated_by, updated_at)
		VALUES (?, ?, 'gm', CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`
	if s.dialect == dbdialect.DialectMySQL {
		query = `
			INSERT INTO runtime_config_overrides (name, value, updated_by, updated_at)
			VALUES (?, ?, 'gm', UTC_TIMESTAMP())
			ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = UTC_TIMESTAMP()`
	}
	if _, err := s.db.ExecContext(ctx, query, name, value); err != nil {
		return fmt.Errorf("runtime config store upsert: %w", err)
	}
	return nil
}

// Delete 删一条 override 的持久记录（ClearOverride 时同步）。
func (s *RuntimeConfigStore) Delete(ctx context.Context, name string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("runtime config store delete: nil store")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runtime_config_overrides WHERE name = ?`, strings.TrimSpace(name)); err != nil {
		return fmt.Errorf("runtime config store delete: %w", err)
	}
	return nil
}

// Load 读全部已持久 override（启动期回灌；runtimeconfig 侧会丢弃已下线/已非法的历史条目）。
func (s *RuntimeConfigStore) Load(ctx context.Context) (map[string]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("runtime config store load: nil store")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name, value FROM runtime_config_overrides`)
	if err != nil {
		return nil, fmt.Errorf("runtime config store load: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("runtime config store load (scan): %w", err)
		}
		out[name] = value
	}
	return out, rows.Err()
}

// 编译期断言：RuntimeConfigStore 满足 runtimeconfig.Store 接口。
var _ runtimeconfig.Store = (*RuntimeConfigStore)(nil)
