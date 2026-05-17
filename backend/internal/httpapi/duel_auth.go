package httpapi

// 文件说明：双人房鉴权与房间号管理，负责 room code 分配、角色令牌映射与持久化回补。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	duelRolePlayer = "player"
	duelRoleEnemy  = "enemy"
)

// duelSessionAuthStore 结构体用于承载该模块的核心数据。
type duelSessionAuthStore struct {
	mu            sync.RWMutex
	db            *sql.DB
	bySession     map[string]map[string]string
	byRoomCode    map[string]duelRoomState
	roomBySession map[string]string
}

// duelRoomState 结构体用于承载该模块的核心数据。
type duelRoomState struct {
	RoomCode    string
	SessionID   string
	PlayerToken string
	EnemyToken  string
	CreatedAt   time.Time
}

// newDuelSessionAuthStore 初始化内存态房间鉴权索引。
func newDuelSessionAuthStore() *duelSessionAuthStore {
	return &duelSessionAuthStore{
		bySession:     map[string]map[string]string{},
		byRoomCode:    map[string]duelRoomState{},
		roomBySession: map[string]string{},
	}
}

// newDuelSessionAuthStoreWithDB 初始化带数据库持久化能力的鉴权存储。
func newDuelSessionAuthStoreWithDB(db *sql.DB) *duelSessionAuthStore {
	store := newDuelSessionAuthStore()
	store.db = db
	if db != nil {
		_ = store.ensureSchema(context.Background())
	}
	return store
}

// ensureSchema 确保双人房房间码映射表存在。
func (store *duelSessionAuthStore) ensureSchema(ctx context.Context) error {
	if store == nil || store.db == nil {
		return nil
	}
	_, err := store.db.ExecContext(
		ctx,
		`
		CREATE TABLE IF NOT EXISTS duel_room_codes (
			room_code TEXT PRIMARY KEY,
			session_id TEXT NOT NULL UNIQUE,
			player_token TEXT NOT NULL,
			enemy_token TEXT NOT NULL,
			created_at TEXT NOT NULL
		)
		`,
	)
	return err
}

// register 为会话分配房间号，并写入 player/enemy 角色令牌。
func (store *duelSessionAuthStore) register(ctx context.Context, sessionID string, playerToken string, enemyToken string) (string, error) {
	if store == nil {
		return "", nil
	}
	sessionID = strings.TrimSpace(sessionID)
	playerToken = strings.TrimSpace(playerToken)
	enemyToken = strings.TrimSpace(enemyToken)
	if sessionID == "" || playerToken == "" || enemyToken == "" {
		return "", nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store.mu.Lock()
	if existing := strings.TrimSpace(store.roomBySession[sessionID]); existing != "" {
		store.mu.Unlock()
		return existing, nil
	}
	store.mu.Unlock()

	if store.db != nil {
		if err := store.ensureSchema(ctx); err != nil {
			return "", err
		}
		if persisted, ok, err := store.loadBySessionFromDB(ctx, sessionID); err != nil {
			return "", err
		} else if ok {
			store.hydrate(persisted)
			return persisted.RoomCode, nil
		}
	}

	for attempt := 0; attempt < 10; attempt++ {
		roomCode := store.nextAvailableRoomCode(ctx)
		if roomCode == "" {
			continue
		}
		room := duelRoomState{
			RoomCode:    roomCode,
			SessionID:   sessionID,
			PlayerToken: playerToken,
			EnemyToken:  enemyToken,
			CreatedAt:   time.Now().UTC(),
		}
		if err := store.persist(ctx, room); err != nil {
			if isRoomCodeConflict(err) {
				continue
			}
			return "", err
		}
		store.hydrate(room)
		return roomCode, nil
	}
	return "", fmt.Errorf("failed to allocate duel room code")
}

// roomCodeForSession 按 session_id 反查房间号（内存优先，数据库回补）。
func (store *duelSessionAuthStore) roomCodeForSession(ctx context.Context, sessionID string) string {
	if store == nil {
		return ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store.mu.RLock()
	existing := strings.TrimSpace(store.roomBySession[sessionID])
	store.mu.RUnlock()
	if existing != "" {
		return existing
	}

	if store.db == nil {
		return ""
	}
	if err := store.ensureSchema(ctx); err != nil {
		return ""
	}
	room, ok, err := store.loadBySessionFromDB(ctx, sessionID)
	if err != nil || !ok {
		return ""
	}
	store.hydrate(room)
	return room.RoomCode
}

// joinByRoomCode 按房间号加入并返回对应阵营 token。
func (store *duelSessionAuthStore) joinByRoomCode(ctx context.Context, roomCode string, preferredRole string) (string, string, string, bool) {
	if store == nil {
		return "", "", "", false
	}
	roomCode = normalizeRoomCode(roomCode)
	preferredRole = strings.ToLower(strings.TrimSpace(preferredRole))
	if roomCode == "" {
		return "", "", "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store.mu.RLock()
	room, ok := store.byRoomCode[roomCode]
	store.mu.RUnlock()
	if !ok && store.db != nil {
		if err := store.ensureSchema(ctx); err == nil {
			if persisted, found, err := store.loadByRoomCodeFromDB(ctx, roomCode); err == nil && found {
				room = persisted
				store.hydrate(room)
				ok = true
			}
		}
	}
	if !ok {
		return "", "", "", false
	}
	if store.db != nil {
		exists, err := store.sessionExists(ctx, room.SessionID)
		if err == nil && !exists {
			store.dropRoomCode(ctx, room)
			return "", "", "", false
		}
	}
	switch preferredRole {
	case duelRolePlayer:
		return room.SessionID, room.PlayerToken, duelRolePlayer, true
	default:
		return room.SessionID, room.EnemyToken, duelRoleEnemy, true
	}
}

// normalizeRoomCode 统一房间号格式（去空白并转大写）。
func normalizeRoomCode(roomCode string) string {
	return strings.ToUpper(strings.TrimSpace(roomCode))
}

// requiresToken 判断会话是否属于双人房并需要 token 鉴权。
func (store *duelSessionAuthStore) requiresToken(ctx context.Context, sessionID string) bool {
	if store == nil {
		return false
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store.mu.RLock()
	roles := store.bySession[sessionID]
	store.mu.RUnlock()
	if len(roles) > 0 {
		return true
	}

	if store.db == nil {
		return false
	}
	if err := store.ensureSchema(ctx); err != nil {
		return false
	}
	room, ok, err := store.loadBySessionFromDB(ctx, sessionID)
	if err != nil || !ok {
		return false
	}
	store.hydrate(room)
	return true
}

// resolveRole 校验 role token 并解析访问角色（player/enemy）。
func (store *duelSessionAuthStore) resolveRole(ctx context.Context, sessionID string, roleToken string) (string, bool) {
	if store == nil {
		return "", false
	}
	sessionID = strings.TrimSpace(sessionID)
	roleToken = strings.TrimSpace(roleToken)
	if sessionID == "" || roleToken == "" {
		return "", false
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store.mu.RLock()
	roles := store.bySession[sessionID]
	role, ok := roles[roleToken]
	store.mu.RUnlock()
	if ok {
		return role, true
	}

	if store.db == nil {
		return "", false
	}
	if err := store.ensureSchema(ctx); err != nil {
		return "", false
	}
	room, found, err := store.loadBySessionFromDB(ctx, sessionID)
	if err != nil || !found {
		return "", false
	}
	store.hydrate(room)
	store.mu.RLock()
	defer store.mu.RUnlock()
	role, ok = store.bySession[sessionID][roleToken]
	if !ok {
		return "", false
	}
	return role, true
}

// persist 把房间码与角色令牌持久化到数据库。
func (store *duelSessionAuthStore) persist(ctx context.Context, room duelRoomState) error {
	if store == nil || store.db == nil {
		return nil
	}
	_, err := store.db.ExecContext(
		ctx,
		`
		INSERT INTO duel_room_codes (room_code, session_id, player_token, enemy_token, created_at)
		VALUES (?, ?, ?, ?, ?)
		`,
		room.RoomCode,
		room.SessionID,
		room.PlayerToken,
		room.EnemyToken,
		room.CreatedAt.Format(time.RFC3339Nano),
	)
	return err
}

// loadBySessionFromDB 按 session_id 加载房间绑定记录。
func (store *duelSessionAuthStore) loadBySessionFromDB(ctx context.Context, sessionID string) (duelRoomState, bool, error) {
	if store == nil || store.db == nil {
		return duelRoomState{}, false, nil
	}
	var (
		room      duelRoomState
		createdAt string
	)
	err := store.db.QueryRowContext(
		ctx,
		`SELECT room_code, session_id, player_token, enemy_token, created_at FROM duel_room_codes WHERE session_id = ? LIMIT 1`,
		sessionID,
	).Scan(&room.RoomCode, &room.SessionID, &room.PlayerToken, &room.EnemyToken, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return duelRoomState{}, false, nil
		}
		return duelRoomState{}, false, err
	}
	if parsed, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(createdAt)); parseErr == nil {
		room.CreatedAt = parsed
	}
	return room, true, nil
}

// loadByRoomCodeFromDB 按 room_code 加载房间绑定记录。
func (store *duelSessionAuthStore) loadByRoomCodeFromDB(ctx context.Context, roomCode string) (duelRoomState, bool, error) {
	if store == nil || store.db == nil {
		return duelRoomState{}, false, nil
	}
	var (
		room      duelRoomState
		createdAt string
	)
	err := store.db.QueryRowContext(
		ctx,
		`SELECT room_code, session_id, player_token, enemy_token, created_at FROM duel_room_codes WHERE room_code = ? LIMIT 1`,
		roomCode,
	).Scan(&room.RoomCode, &room.SessionID, &room.PlayerToken, &room.EnemyToken, &createdAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return duelRoomState{}, false, nil
		}
		return duelRoomState{}, false, err
	}
	if parsed, parseErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(createdAt)); parseErr == nil {
		room.CreatedAt = parsed
	}
	return room, true, nil
}

// hydrate 把房间记录回填到内存索引，加速后续鉴权查询。
func (store *duelSessionAuthStore) hydrate(room duelRoomState) {
	if store == nil {
		return
	}
	if strings.TrimSpace(room.SessionID) == "" || strings.TrimSpace(room.RoomCode) == "" {
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	store.bySession[room.SessionID] = map[string]string{
		room.PlayerToken: duelRolePlayer,
		room.EnemyToken:  duelRoleEnemy,
	}
	store.byRoomCode[room.RoomCode] = room
	store.roomBySession[room.SessionID] = room.RoomCode
}

// sessionExists 判断会话是否仍存在，避免失效房间继续被加入。
func (store *duelSessionAuthStore) sessionExists(ctx context.Context, sessionID string) (bool, error) {
	if store == nil || store.db == nil {
		return true, nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false, nil
	}
	var found string
	err := store.db.QueryRowContext(
		ctx,
		`SELECT id FROM single_player_sessions WHERE id = ? LIMIT 1`,
		sessionID,
	).Scan(&found)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(found) != "", nil
}

// dropRoomCode 删除失效房间号及其内存/数据库映射。
func (store *duelSessionAuthStore) dropRoomCode(ctx context.Context, room duelRoomState) {
	if store == nil {
		return
	}
	roomCode := strings.TrimSpace(room.RoomCode)
	sessionID := strings.TrimSpace(room.SessionID)
	if roomCode == "" || sessionID == "" {
		return
	}
	store.mu.Lock()
	delete(store.byRoomCode, roomCode)
	delete(store.roomBySession, sessionID)
	delete(store.bySession, sessionID)
	store.mu.Unlock()

	if store.db == nil {
		return
	}
	_, _ = store.db.ExecContext(ctx, `DELETE FROM duel_room_codes WHERE room_code = ?`, roomCode)
}

// nextAvailableRoomCode 生成当前可用且不冲突的房间号。
func (store *duelSessionAuthStore) nextAvailableRoomCode(ctx context.Context) string {
	for attempt := 0; attempt < 20; attempt++ {
		code := randomRoomCode()
		if code == "" {
			continue
		}
		store.mu.RLock()
		_, inMemory := store.byRoomCode[code]
		store.mu.RUnlock()
		if inMemory {
			continue
		}
		if store.db != nil {
			var existed string
			err := store.db.QueryRowContext(
				ctx,
				`SELECT room_code FROM duel_room_codes WHERE room_code = ? LIMIT 1`,
				code,
			).Scan(&existed)
			if err == nil {
				continue
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				continue
			}
		}
		return code
	}
	return ""
}

// isRoomCodeConflict 判断数据库写入失败是否由唯一约束冲突引起。
func isRoomCodeConflict(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unique") || strings.Contains(text, "constraint")
}

// randomRoomCode 生成 6 位可读随机房间码。
func randomRoomCode() string {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	const length = 6
	var builder strings.Builder
	builder.Grow(length)
	max := big.NewInt(int64(len(alphabet)))
	for index := 0; index < length; index++ {
		value, err := rand.Int(rand.Reader, max)
		if err != nil {
			return ""
		}
		builder.WriteByte(alphabet[value.Int64()])
	}
	return builder.String()
}
