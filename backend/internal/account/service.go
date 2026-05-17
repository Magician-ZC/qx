package account

// 文件说明：账号服务实现，包含注册、登录、会话令牌签发、当前用户查询与表结构初始化。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"qunxiang/backend/internal/storage/dbdialect"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9_]{3,32}$`)

// User 结构体用于承载该模块的核心数据。
type User struct {
	ID          string    `json:"id"`
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// LoginResult 结构体用于承载该模块的核心数据。
type LoginResult struct {
	User      User      `json:"user"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
	TokenType string    `json:"token_type"`
	Provider  string    `json:"provider"`
}

// Service 结构体用于承载该模块的核心数据。
type Service struct {
	db       *sql.DB
	tokenTTL time.Duration
}

// NewService 创建账户服务并设置 token 过期时长（未配置时使用默认 72h）。
func NewService(db *sql.DB, tokenTTL time.Duration) *Service {
	if tokenTTL <= 0 {
		tokenTTL = 72 * time.Hour
	}
	return &Service{
		db:       db,
		tokenTTL: tokenTTL,
	}
}

// EnsureSchema 确保账号用户表与会话表存在，并建立常用查询索引。
func (service *Service) EnsureSchema(ctx context.Context) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("account db is not configured")
	}
	schema := `
	CREATE TABLE IF NOT EXISTS accounts_users (
		id TEXT PRIMARY KEY,
		username TEXT NOT NULL UNIQUE,
		display_name TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS accounts_sessions (
		token TEXT PRIMARY KEY,
		user_id TEXT NOT NULL REFERENCES accounts_users(id) ON DELETE CASCADE,
		expires_at TIMESTAMPTZ NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_accounts_sessions_user_id ON accounts_sessions(user_id);
	CREATE INDEX IF NOT EXISTS idx_accounts_sessions_expires_at ON accounts_sessions(expires_at);
	`
	if dbdialect.IsMySQL(service.db) {
		schema = `
		CREATE TABLE IF NOT EXISTS accounts_users (
			id VARCHAR(191) PRIMARY KEY,
			username VARCHAR(191) NOT NULL UNIQUE,
			display_name VARCHAR(191) NOT NULL,
			password_hash VARCHAR(191) NOT NULL,
			created_at VARCHAR(64) NOT NULL DEFAULT (CURRENT_TIMESTAMP),
			updated_at VARCHAR(64) NOT NULL DEFAULT (CURRENT_TIMESTAMP)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;

		CREATE TABLE IF NOT EXISTS accounts_sessions (
			token VARCHAR(191) PRIMARY KEY,
			user_id VARCHAR(191) NOT NULL,
			expires_at VARCHAR(64) NOT NULL,
			created_at VARCHAR(64) NOT NULL DEFAULT (CURRENT_TIMESTAMP),
			INDEX idx_accounts_sessions_user_id (user_id),
			INDEX idx_accounts_sessions_expires_at (expires_at),
			CONSTRAINT fk_accounts_sessions_user FOREIGN KEY (user_id) REFERENCES accounts_users(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
		`
	}
	for _, statement := range strings.Split(schema, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err := service.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure account schema: %w", err)
		}
	}
	return nil
}

// Register 注册新用户并写入账户表。
// 该流程包含输入校验、密码哈希、唯一用户名约束处理与时间字段解析。
func (service *Service) Register(ctx context.Context, username string, displayName string, password string) (User, error) {
	if service == nil || service.db == nil {
		return User{}, fmt.Errorf("account service is unavailable")
	}
	username, displayName, err := normalizeRegistrationInput(username, displayName, password)
	if err != nil {
		return User{}, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(strings.TrimSpace(password)), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	user := User{
		ID:          uuid.NewString(),
		Username:    username,
		DisplayName: displayName,
	}
	now := time.Now().UTC()
	_, err = service.db.ExecContext(
		ctx,
		`
		INSERT INTO accounts_users (id, username, display_name, password_hash)
		VALUES (?, ?, ?, ?)
		`,
		user.ID,
		user.Username,
		user.DisplayName,
		string(hash),
	)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, fmt.Errorf("username already exists")
		}
		return User{}, fmt.Errorf("create account user: %w", err)
	}
	user.CreatedAt = now
	user.UpdatedAt = now
	return user, nil
}

// Login 校验用户名与密码并签发会话 token。
func (service *Service) Login(ctx context.Context, username string, password string) (LoginResult, error) {
	if service == nil || service.db == nil {
		return LoginResult{}, fmt.Errorf("account service is unavailable")
	}
	username = strings.ToLower(strings.TrimSpace(username))
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return LoginResult{}, fmt.Errorf("username and password are required")
	}

	var (
		user         User
		passwordHash string
		createdAtRaw any
		updatedAtRaw any
	)
	err := service.db.QueryRowContext(
		ctx,
		`
		SELECT id, username, display_name, password_hash, created_at, updated_at
		FROM accounts_users
		WHERE username = ?
		`,
		username,
	).Scan(
		&user.ID,
		&user.Username,
		&user.DisplayName,
		&passwordHash,
		&createdAtRaw,
		&updatedAtRaw,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return LoginResult{}, fmt.Errorf("invalid username or password")
		}
		return LoginResult{}, fmt.Errorf("query account user: %w", err)
	}
	user.CreatedAt, err = parseAccountTime(createdAtRaw)
	if err != nil {
		return LoginResult{}, err
	}
	user.UpdatedAt, err = parseAccountTime(updatedAtRaw)
	if err != nil {
		return LoginResult{}, err
	}
	if compareErr := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)); compareErr != nil {
		return LoginResult{}, fmt.Errorf("invalid username or password")
	}

	token, err := generateToken()
	if err != nil {
		return LoginResult{}, err
	}
	expiresAt := time.Now().UTC().Add(service.tokenTTL)
	if _, err := service.db.ExecContext(
		ctx,
		`
		INSERT INTO accounts_sessions (token, user_id, expires_at)
		VALUES (?, ?, ?)
		`,
		token,
		user.ID,
		expiresAt,
	); err != nil {
		return LoginResult{}, fmt.Errorf("create account session: %w", err)
	}

	return LoginResult{
		User:      user,
		Token:     token,
		ExpiresAt: expiresAt,
		TokenType: "Bearer",
		Provider:  string(dbdialect.For(service.db)),
	}, nil
}

// CurrentUser 基于 bearer token 读取当前登录用户，并顺带清理过期会话。
func (service *Service) CurrentUser(ctx context.Context, token string) (User, error) {
	if service == nil || service.db == nil {
		return User{}, fmt.Errorf("account service is unavailable")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return User{}, fmt.Errorf("token is required")
	}

	if _, err := service.db.ExecContext(ctx, `DELETE FROM accounts_sessions WHERE expires_at <= CURRENT_TIMESTAMP`); err != nil {
		return User{}, fmt.Errorf("cleanup expired sessions: %w", err)
	}

	var user User
	var createdAtRaw any
	var updatedAtRaw any
	err := service.db.QueryRowContext(
		ctx,
		`
		SELECT u.id, u.username, u.display_name, u.created_at, u.updated_at
		FROM accounts_sessions s
		JOIN accounts_users u ON u.id = s.user_id
		WHERE s.token = ? AND s.expires_at > CURRENT_TIMESTAMP
		`,
		token,
	).Scan(
		&user.ID,
		&user.Username,
		&user.DisplayName,
		&createdAtRaw,
		&updatedAtRaw,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return User{}, fmt.Errorf("invalid or expired token")
		}
		return User{}, fmt.Errorf("query account session: %w", err)
	}
	user.CreatedAt, err = parseAccountTime(createdAtRaw)
	if err != nil {
		return User{}, err
	}
	user.UpdatedAt, err = parseAccountTime(updatedAtRaw)
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// Logout 注销当前 token 对应会话。
func (service *Service) Logout(ctx context.Context, token string) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("account service is unavailable")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("token is required")
	}
	if _, err := service.db.ExecContext(ctx, `DELETE FROM accounts_sessions WHERE token = ?`, token); err != nil {
		return fmt.Errorf("delete account session: %w", err)
	}
	return nil
}

// normalizeRegistrationInput 规范化并校验注册输入。
func normalizeRegistrationInput(username string, displayName string, password string) (string, string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	displayName = strings.TrimSpace(displayName)
	password = strings.TrimSpace(password)

	if username == "" {
		return "", "", fmt.Errorf("username is required")
	}
	if !usernamePattern.MatchString(username) {
		return "", "", fmt.Errorf("username must match ^[a-z0-9_]{3,32}$")
	}
	if len([]rune(password)) < 8 {
		return "", "", fmt.Errorf("password must be at least 8 characters")
	}
	if displayName == "" {
		displayName = username
	}
	if len([]rune(displayName)) > 32 {
		displayName = string([]rune(displayName)[:32])
	}
	return username, displayName, nil
}

// ExtractBearerToken 从 Authorization 头解析 Bearer token。
func ExtractBearerToken(authorization string) string {
	authorization = strings.TrimSpace(authorization)
	if authorization == "" {
		return ""
	}
	const bearerPrefix = "bearer "
	if len(authorization) <= len(bearerPrefix) || strings.ToLower(authorization[:len(bearerPrefix)]) != bearerPrefix {
		return ""
	}
	return strings.TrimSpace(authorization[len(bearerPrefix):])
}

// generateToken 生成随机会话令牌（24 字节随机数的十六进制编码）。
func generateToken() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// isUniqueViolation 判断数据库错误是否为唯一约束冲突。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "duplicate key") || strings.Contains(text, "unique constraint") || strings.Contains(text, "duplicate entry")
}

// parseAccountTime 解析数据库返回的时间字段（兼容 time/string/[]byte）。
func parseAccountTime(value any) (time.Time, error) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), nil
	case string:
		return parseAccountTimeString(typed)
	case []byte:
		return parseAccountTimeString(string(typed))
	default:
		return time.Time{}, fmt.Errorf("parse account timestamp: unsupported type %T", value)
	}
}

// parseAccountTimeString 使用多种候选格式解析时间字符串。
func parseAccountTimeString(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, fmt.Errorf("parse account timestamp: empty value")
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02T15:04:05",
	}
	for _, layout := range layouts {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("parse account timestamp %q: unsupported format", raw)
}
