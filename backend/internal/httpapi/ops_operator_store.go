package httpapi

// 文件说明：ops / GM 运营后台的「多操作者 + 角色 + 操作审计」数据库后端（双驱动 SQLite/MySQL）。
// 两张表：ops_operators（token_hash 主键 + name 唯一 + role）承载分级鉴权；ops_audit_log 承载操作留痕。
// token 一律只存 sha256 hex（不落明文），鉴权时把请求头 X-Ops-Token 同样 hash 后按 token_hash 命中。
// 与 RuntimeConfigStore 同构：开新连接前应已 dbdialect.Register，dialect 由 db 推断；upsert 走双驱动
// ON CONFLICT / ON DUPLICATE KEY。本文件还提供 opsRBACGuard 降级路径用的 env-token 读取 / 常量时间比对小工具。

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// OpsOperatorStore 把运营操作者 / 审计日志落库（双驱动），供 opsRBACGuard 分级鉴权与 auditOps 留痕。
type OpsOperatorStore struct {
	db      *sql.DB
	dialect dbdialect.Dialect
}

// NewOpsOperatorStore 构造持久化后端。dialect 由 db 推断（开新连接前应已 dbdialect.Register）。
func NewOpsOperatorStore(db *sql.DB) *OpsOperatorStore {
	return &OpsOperatorStore{db: db, dialect: dbdialect.For(db)}
}

// hashOpsToken 把明文 token 归一为 sha256 hex（落库 / 查表统一口径）。空 token 返回空串（不命中任何行）。
func hashOpsToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Resolve 按请求头 token 的 sha256 命中操作者。未命中返回 (零值, false, nil)；DB 错误返回 (零值, false, err)。
func (s *OpsOperatorStore) Resolve(ctx context.Context, token string) (OpsOperator, bool, error) {
	if s == nil || s.db == nil {
		return OpsOperator{}, false, fmt.Errorf("ops operator store resolve: nil store")
	}
	hash := hashOpsToken(token)
	if hash == "" {
		return OpsOperator{}, false, nil
	}
	var name, role string
	err := s.db.QueryRowContext(ctx,
		`SELECT name, role FROM ops_operators WHERE token_hash = ?`, hash,
	).Scan(&name, &role)
	if err == sql.ErrNoRows {
		return OpsOperator{}, false, nil
	}
	if err != nil {
		return OpsOperator{}, false, fmt.Errorf("ops operator store resolve: %w", err)
	}
	return OpsOperator{Name: name, Role: OpsRole(role)}, true, nil
}

// Count 返回 ops_operators 表行数（opsRBACGuard 据此判定「以表为权威」还是降级 env）。
func (s *OpsOperatorStore) Count(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, fmt.Errorf("ops operator store count: nil store")
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops_operators`).Scan(&n); err != nil {
		return 0, fmt.Errorf("ops operator store count: %w", err)
	}
	return n, nil
}

// Upsert 幂等写一名操作者（按 token_hash 主键 upsert，双驱动 ON CONFLICT / ON DUPLICATE KEY）。
// createdBy 标记谁创建的（审计用，可空）。name 唯一冲突由表约束兜底（同 token 改 name/role 是更新路径）。
func (s *OpsOperatorStore) Upsert(ctx context.Context, name, role, token, createdBy string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ops operator store upsert: nil store")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("ops operator store upsert: empty name")
	}
	hash := hashOpsToken(token)
	if hash == "" {
		return fmt.Errorf("ops operator store upsert: empty token")
	}
	// role 归一小写 + 枚举校验：拒绝未知/大小写错误的角色——否则建首个 operator 时若拼错（→roleRank=0）
	// 会让全表无人具足够权限，叠加「表非空即以表为准」造成永久自锁。
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		role = string(RoleViewer)
	}
	if roleRank(OpsRole(role)) == 0 {
		return fmt.Errorf("ops operator store upsert: invalid role %q (want viewer|operator|admin)", role)
	}
	// 按 name upsert（name 上有 UNIQUE 索引）：同名换 token 即「令牌轮换」——更新该行 token_hash/role，
	// 而非 ON CONFLICT(token_hash)（新 token 不撞主键→走 INSERT→撞 name UNIQUE→失败，轮换路径被打死）。
	query := `
		INSERT INTO ops_operators (token_hash, name, role, created_at, created_by)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP, ?)
		ON CONFLICT(name) DO UPDATE SET token_hash = excluded.token_hash, role = excluded.role`
	if s.dialect == dbdialect.DialectMySQL {
		// MySQL ON DUPLICATE KEY 命中 name UNIQUE 冲突即更新 token_hash/role（轮换）。
		query = `
			INSERT INTO ops_operators (token_hash, name, role, created_at, created_by)
			VALUES (?, ?, ?, UTC_TIMESTAMP(), ?)
			ON DUPLICATE KEY UPDATE token_hash = VALUES(token_hash), role = VALUES(role)`
	}
	if _, err := s.db.ExecContext(ctx, query, hash, name, role, strings.TrimSpace(createdBy)); err != nil {
		return fmt.Errorf("ops operator store upsert: %w", err)
	}
	return nil
}

// Delete 按 name 删一名操作者（撤销访问）。
func (s *OpsOperatorStore) Delete(ctx context.Context, name string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ops operator store delete: nil store")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ops_operators WHERE name = ?`, strings.TrimSpace(name)); err != nil {
		return fmt.Errorf("ops operator store delete: %w", err)
	}
	return nil
}

// OpsOperatorRow 是 List 返回的脱敏行（绝不含 token_hash）。
type OpsOperatorRow struct {
	Name      string
	Role      string
	CreatedAt string
}

// List 读全部操作者（按创建时间倒序），脱敏——不返回 token_hash。
func (s *OpsOperatorStore) List(ctx context.Context) ([]OpsOperatorRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("ops operator store list: nil store")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, role, created_at FROM ops_operators ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("ops operator store list: %w", err)
	}
	defer rows.Close()
	out := []OpsOperatorRow{}
	for rows.Next() {
		var r OpsOperatorRow
		if err := rows.Scan(&r.Name, &r.Role, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("ops operator store list (scan): %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendAudit 追加一条操作审计行（append-only；id 用 uuid）。
func (s *OpsOperatorStore) AppendAudit(ctx context.Context, operator, role, action, target string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("ops operator store append audit: nil store")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO ops_audit_log (id, operator, role, action, target, created_at)
		 VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)`,
		uuid.NewString(), strings.TrimSpace(operator), strings.TrimSpace(role),
		strings.TrimSpace(action), strings.TrimSpace(target),
	); err != nil {
		return fmt.Errorf("ops operator store append audit: %w", err)
	}
	return nil
}

// auditRow 是 ListAudit 返回的一条审计记录。
type auditRow struct {
	ID        string
	Operator  string
	Role      string
	Action    string
	Target    string
	CreatedAt string
}

// ListAudit 读最近 limit 条审计（按时间倒序）。limit<=0 时按默认 100 收口。
func (s *OpsOperatorStore) ListAudit(ctx context.Context, limit int) ([]auditRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("ops operator store list audit: nil store")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, operator, role, action, target, created_at
		 FROM ops_audit_log ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ops operator store list audit: %w", err)
	}
	defer rows.Close()
	out := []auditRow{}
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.ID, &r.Operator, &r.Role, &r.Action, &r.Target, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("ops operator store list audit (scan): %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// opsEnvToken 读旧的单 token env（opsRBACGuard 表为空时的降级凭据）。复用 ops_auth.go 的 opsEnvVar 常量同口径。
func opsEnvToken() string {
	return strings.TrimSpace(os.Getenv(opsEnvVar))
}

// constantTimeEqual 常量时间比对两个 token，防时序侧信道（与 ops_auth.go 的 opsTokenGuard 同口径）。
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
