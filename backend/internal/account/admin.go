package account

// 文件说明：账户的 GM 客户管理只读/管控能力——列表/搜索/按 id 查 + 封禁开关。
// 供司命台「客户管理」面板使用（全程经后台 RBAC admin + 审计在 httpapi 层把关）。
// 封禁状态落 accounts_users.banned 列，Login/CurrentUser 据此拒绝（见 service.go 的 isBanned 拦截）。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// AdminUser 是 GM 客户管理视图的账户行（含封禁态；不含 password_hash）。
type AdminUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	Banned      bool   `json:"banned"`
	CreatedAt   string `json:"created_at"`
}

// ListUsers 列出/搜索账户（query 非空时按 username/display_name 模糊或 id 精确匹配）。limit<=0 取 100、>500 收口 500。
func (service *Service) ListUsers(ctx context.Context, query string, limit int) ([]AdminUser, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("account service is unavailable")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query = strings.TrimSpace(query)
	var rows *sql.Rows
	var err error
	if query == "" {
		rows, err = service.db.QueryContext(ctx,
			`SELECT id, username, display_name, banned, created_at FROM accounts_users ORDER BY created_at DESC LIMIT ?`, limit)
	} else {
		like := "%" + strings.ToLower(query) + "%"
		rows, err = service.db.QueryContext(ctx,
			`SELECT id, username, display_name, banned, created_at FROM accounts_users
			 WHERE LOWER(username) LIKE ? OR LOWER(display_name) LIKE ? OR id = ?
			 ORDER BY created_at DESC LIMIT ?`, like, like, query, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list account users: %w", err)
	}
	defer rows.Close()
	out := []AdminUser{}
	for rows.Next() {
		u, scanErr := scanAdminUser(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetByID 按账户 id 取单个账户（客户详情用）。不存在返回错误。
func (service *Service) GetByID(ctx context.Context, id string) (AdminUser, error) {
	if service == nil || service.db == nil {
		return AdminUser{}, fmt.Errorf("account service is unavailable")
	}
	row := service.db.QueryRowContext(ctx,
		`SELECT id, username, display_name, banned, created_at FROM accounts_users WHERE id = ?`, strings.TrimSpace(id))
	return scanAdminUser(row)
}

// SetBanned 设置/解除账户封禁。封禁后 Login 拒绝、既有 token 在 CurrentUser 处失效。
func (service *Service) SetBanned(ctx context.Context, id string, banned bool) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("account service is unavailable")
	}
	val := 0
	if banned {
		val = 1
	}
	res, err := service.db.ExecContext(ctx, `UPDATE accounts_users SET banned = ? WHERE id = ?`, val, strings.TrimSpace(id))
	if err != nil {
		return fmt.Errorf("set account banned: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("set account banned: account not found")
	}
	return nil
}

// scanRow 抽象 *sql.Row 与 *sql.Rows 的共同 Scan 能力（让 List/Get 共用扫描逻辑）。
type scanRow interface{ Scan(...any) error }

// scanAdminUser 把一行扫成 AdminUser（banned int→bool、created_at any→RFC3339 串）。
func scanAdminUser(r scanRow) (AdminUser, error) {
	var u AdminUser
	var banned int
	var createdAtRaw any
	if err := r.Scan(&u.ID, &u.Username, &u.DisplayName, &banned, &createdAtRaw); err != nil {
		return AdminUser{}, fmt.Errorf("scan account user: %w", err)
	}
	u.Banned = banned != 0
	if t, err := parseAccountTime(createdAtRaw); err == nil {
		u.CreatedAt = t.UTC().Format("2006-01-02 15:04:05")
	}
	return u, nil
}
