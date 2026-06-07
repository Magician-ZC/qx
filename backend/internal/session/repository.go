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
	// 拆 state_json 第二片（沙盘 §11.2，影子双写）：把当回合 LLM 交互在 compactStateForStorage **抹除旧 prompt 之前**
	// best-effort 写入旁路表（含完整 prompt）。执行循环每个 actor 行动后即 Save，故 INSERT OR IGNORE 跨 Save 累积出全量。
	// 吞错——blob 仍裁剪仍为权威读源，写表失败仅旁路缺一条，零风险（读路径切换是后续过评审的步骤）。
	_ = persistLLMInteractions(ctx, repository.db, dbdialect.IsMySQL(repository.db), state.ID, state.LLMInteractions)

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
	encodedState, err := json.Marshal(state)
	state.DecisionTraces = traces
	if err != nil {
		return fmt.Errorf("marshal session state: %w", err)
	}

	query := `
		INSERT INTO single_player_sessions (id, state_json, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			state_json = excluded.state_json,
			updated_at = excluded.updated_at
		`
	if dbdialect.IsMySQL(repository.db) {
		query = `
		INSERT INTO single_player_sessions (id, state_json, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			state_json = VALUES(state_json),
			updated_at = VALUES(updated_at)
		`
	}
	if _, err := repository.db.ExecContext(
		ctx,
		query,
		state.ID,
		string(encodedState),
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
