package session

// 文件说明：repository.go，会话状态仓储实现，负责 State 的数据库持久化与读取。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
)

// Repository 负责会话状态在数据库中的读写。
type Repository struct {
	db *sql.DB
}

// NewRepository 创建会话状态仓库实例。
func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// Save 持久化会话状态（插入或覆盖更新）。
func (repository *Repository) Save(ctx context.Context, state *State) error {
	// 拆 state_json 第二片（沙盘 §11.2，读路径已切表）：把当回合 LLM 交互在 compactStateForStorage **抹除旧 prompt 之前**
	// 写入旁路表（含完整 prompt）。执行循环每个 actor 行动后即 Save，故 INSERT OR IGNORE 跨 Save 累积出全量。
	// 持久性不变量（与 decision_traces 同纪律）：**先确认写进表、成功才从 blob 摘除**；写表失败则保留在 blob（瘦身放弃，
	// 但绝不丢交互，下次 load 由 hydrateLLMInteractions 自愈回填）。注意须取压缩**前**的完整集做摘除判定与还原。
	fullLLM := state.LLMInteractions
	stripLLM := persistLLMInteractions(ctx, repository.db, dbdialect.IsMySQL(repository.db), state.ID, fullLLM) == nil

	compactStateForStorage(state)

	now := time.Now().UTC()
	if state.CreatedAt.IsZero() {
		state.CreatedAt = now
	}
	state.UpdatedAt = now

	// 拆 state_json（沙盘 §11.2）：决策轨迹外移到 decision_traces 表。持久性不变量——
	// **先确认写进表、成功才从 blob 摘除**；写表失败则保留在 blob（瘦身放弃，但绝不丢轨迹，下次 load 自愈）。
	traces := state.DecisionTraces
	stripTraces := persistDecisionTraces(ctx, repository.db, dbdialect.IsMySQL(repository.db), state.ID, traces) == nil
	if stripTraces {
		state.DecisionTraces = nil
	}
	// LLM 交互摘除：compactStateForStorage 已把 state.LLMInteractions 原地压缩，此处保存压缩后引用以便 marshal 后还原
	// （维持切换前「Save 后 state.LLMInteractions = 压缩态」的语义），确认写表成功才从 blob 摘除。
	compactedLLM := state.LLMInteractions
	if stripLLM {
		state.LLMInteractions = nil
	}
	// 拆 state_json 第三片：原始事件日志外移到 raw_event_log 表。与 LLM 不同——RawEventLog 无字段级脱敏要捕获，
	// 故在 compactStateForStorage **之后**持久化压缩态：appendRawEvent 已把 payload 限/裁、数组已裁到 maxRawEventHistory，
	// 压缩对它幂等（仅 limitTextRunes 的边界空格非幂等），post-compact 持久化使表与 blob 逐字节一致、hydrate 才能 cap-only。
	stripRaw := persistRawEvents(ctx, repository.db, dbdialect.IsMySQL(repository.db), state.ID, state.RawEventLog) == nil
	compactedRaw := state.RawEventLog
	if stripRaw {
		state.RawEventLog = nil
	}
	encodedState, err := json.Marshal(state)
	state.DecisionTraces = traces
	state.LLMInteractions = compactedLLM
	state.RawEventLog = compactedRaw
	if err != nil {
		return fmt.Errorf("marshal session state: %w", err)
	}

	// account_id / world_id 去规范化为可查询列（与 state_json 同步写回）：
	//   - account_id 支撑账户成本聚合/风控（成本闭环）。
	//   - world_id 支撑「账号在主世界 world_default 的角色」的 (account_id, world_id) 索引查询（大世界页游入口 resume）。
	// 二者空串时落 NULL（兼容匿名/未接入多世界的旧局；NULL 不进 (account_id, world_id) 索引匹配，天然不被 resume 误命中）。
	accountIDCol := nullableString(state.AccountID)
	worldIDCol := nullableString(state.WorldID)
	query := `
		INSERT INTO single_player_sessions (id, state_json, account_id, world_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			state_json = excluded.state_json,
			account_id = excluded.account_id,
			world_id = excluded.world_id,
			updated_at = excluded.updated_at
		`
	if dbdialect.IsMySQL(repository.db) {
		query = `
		INSERT INTO single_player_sessions (id, state_json, account_id, world_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			state_json = VALUES(state_json),
			account_id = VALUES(account_id),
			world_id = VALUES(world_id),
			updated_at = VALUES(updated_at)
		`
	}
	if _, err := repository.db.ExecContext(
		ctx,
		query,
		state.ID,
		string(encodedState),
		accountIDCol,
		worldIDCol,
		state.CreatedAt.Format(time.RFC3339Nano),
		state.UpdatedAt.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("save session %s: %w", state.ID, err)
	}

	return nil
}

// Get 读取会话状态并补齐兼容字段默认值。
func (repository *Repository) Get(ctx context.Context, sessionID string) (State, error) {
	var state State
	var encodedState string
	var createdAt string
	var updatedAt string

	if err := repository.db.QueryRowContext(
		ctx,
		`
		SELECT state_json, created_at, updated_at
		FROM single_player_sessions
		WHERE id = ?
		`,
		sessionID,
	).Scan(&encodedState, &createdAt, &updatedAt); err != nil {
		return State{}, fmt.Errorf("get session %s: %w", sessionID, err)
	}

	if err := json.Unmarshal([]byte(encodedState), &state); err != nil {
		return State{}, fmt.Errorf("decode session %s: %w", sessionID, err)
	}

	if state.CreatedAt.IsZero() {
		timestamp, err := parseTimestamp(createdAt)
		if err != nil {
			return State{}, err
		}
		state.CreatedAt = timestamp
	}
	if state.UpdatedAt.IsZero() {
		timestamp, err := parseTimestamp(updatedAt)
		if err != nil {
			return State{}, err
		}
		state.UpdatedAt = timestamp
	}

	if state.DirectiveHistory == nil {
		state.DirectiveHistory = []Directive{}
	}
	if state.DialogueHistory == nil {
		state.DialogueHistory = []DialogueMessage{}
	}
	if state.Structures == nil {
		state.Structures = []Structure{}
	}
	if state.DecisionTraces == nil {
		state.DecisionTraces = []DecisionTrace{}
	}
	if state.LLMInteractions == nil {
		state.LLMInteractions = []LLMInteraction{}
	}
	if state.PigeonQueue == nil {
		state.PigeonQueue = []PigeonDispatch{}
	}
	if state.FactionRelations == nil {
		state.FactionRelations = []FactionRelation{}
	}
	if state.BattleReports == nil {
		state.BattleReports = []BattleReport{}
	}
	if state.IntelAssets == nil {
		state.IntelAssets = []IntelAsset{}
	}
	if state.IntelReports == nil {
		state.IntelReports = []IntelReport{}
	}
	if state.ModerationReports == nil {
		state.ModerationReports = []ModerationReport{}
	}
	if state.Logs == nil {
		state.Logs = []LogEntry{}
	}
	if state.SetupPhase == "" {
		state.SetupPhase = SetupPhaseReady
	}
	if state.DraftRequiredPick <= 0 {
		state.DraftRequiredPick = 10
	}
	if state.PlayerDraftPool == nil {
		state.PlayerDraftPool = []unit.Record{}
	}
	if state.EnemyDraftPool == nil {
		state.EnemyDraftPool = []unit.Record{}
	}
	if state.WildUnitIDs == nil {
		state.WildUnitIDs = []string{}
	}
	if state.PhaseReady == nil {
		state.PhaseReady = map[string]bool{}
	}
	if state.RandomSeed == 0 {
		if state.Map.Seed != 0 {
			state.RandomSeed = state.Map.Seed
		} else {
			state.RandomSeed = seedFromSessionID(state.ID)
		}
	}
	ensureCommandPower(&state)
	state.GlobalDirective.Kind = normalizeDirectiveKind(state.GlobalDirective.Kind)
	if state.GlobalDirective.Priority == "" {
		state.GlobalDirective.Priority = "normal"
	}
	for index := range state.DirectiveHistory {
		state.DirectiveHistory[index].Kind = normalizeDirectiveKind(state.DirectiveHistory[index].Kind)
		if state.DirectiveHistory[index].Priority == "" {
			state.DirectiveHistory[index].Priority = "normal"
		}
	}
	if state.Weather.Type == "" {
		state.Weather = weatherForTurnBySeed(state.RandomSeed, state.TurnState.Turn)
	}
	if strings.TrimSpace(state.MapScriptID) == "" {
		state.MapScriptID = normalizeBattlefieldScriptID("", state.RandomSeed)
	}
	if strings.TrimSpace(state.MapScriptName) == "" {
		state.MapScriptName = battlefieldScriptDisplayName(state.MapScriptID)
	}
	ensureFactionRelations(&state)

	return state, nil
}

// nullableString 把空白字符串映射为 SQL NULL，非空则原样落库。
// 用于 account_id / world_id 去规范化列：匿名/未接入多世界的局留 NULL，既不污染 (account_id, world_id) 索引、
// 也不会被 FindMainWorldSessionID 的「account_id=? AND world_id=?」精确匹配误命中。
func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

// FindMainWorldSessionID 查某账号在某世界绑定的角色 session ID（大世界页游入口 resume 的核心查询）。
// 走 (account_id, world_id) 复合索引，绝不扫描 state_json blob。一个账号在一个世界至多一个持久角色（由
// CreateMainWorldCharacter 的幂等守卫保证）；防御性地取 updated_at 最新一条，返回是否命中。
// accountID/worldID 任一为空直接返回未命中（NULL 列不参与精确匹配，匿名/单机局天然不被 resume）。
func (repository *Repository) FindMainWorldSessionID(ctx context.Context, accountID string, worldID string) (string, bool, error) {
	accountID = strings.TrimSpace(accountID)
	worldID = strings.TrimSpace(worldID)
	if repository == nil || repository.db == nil || accountID == "" || worldID == "" {
		return "", false, nil
	}
	var sessionID string
	err := repository.db.QueryRowContext(
		ctx,
		`
		SELECT id FROM single_player_sessions
		WHERE account_id = ? AND world_id = ?
		ORDER BY updated_at DESC
		LIMIT 1
		`,
		accountID, worldID,
	).Scan(&sessionID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("find main-world character (account=%s world=%s): %w", accountID, worldID, err)
	}
	return sessionID, true, nil
}

// ListMainWorldSessionIDs 列出某世界（world_default）下所有账号绑定的主世界 session ID（命运开盒后台 ticker 的扫描源）。
// 走 (account_id, world_id) 复合索引按 world_id 过滤、要求 account_id 非空（NULL/匿名/单机局不进列表）；绝不扫 state_json blob。
// worldID 为空直接返回空列表（不会误扫匿名局）。供 RunFateAutoTickLoop 在 flag 开启时逐 session 推一拍。
func (repository *Repository) ListMainWorldSessionIDs(ctx context.Context, worldID string) ([]string, error) {
	worldID = strings.TrimSpace(worldID)
	if repository == nil || repository.db == nil || worldID == "" {
		return nil, nil
	}
	rows, err := repository.db.QueryContext(
		ctx,
		`
		SELECT id FROM single_player_sessions
		WHERE world_id = ? AND account_id IS NOT NULL AND account_id <> ''
		ORDER BY updated_at DESC
		`,
		worldID,
	)
	if err != nil {
		return nil, fmt.Errorf("list main-world sessions (world=%s): %w", worldID, err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan main-world session id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// parseTimestamp 兼容解析 RFC3339Nano/RFC3339 时间字符串。
func parseTimestamp(value string) (time.Time, error) {
	timestamp, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return timestamp, nil
	}

	timestamp, err = time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", value, err)
	}

	return timestamp, nil
}
