package session

// 文件说明：维护断线重连快照机制，保存阶段边界快照并提供历史边界元信息查询。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	maxPhaseBoundarySnapshots    = 160
	defaultReconnectHistoryLimit = 12
)

// GetReconnectSnapshot 返回重连所需的完整快照包。
// 包含当前实时快照、最近阶段边界快照，以及一组历史边界元信息用于前端恢复时间轴。
func (service *Service) GetReconnectSnapshot(ctx context.Context, sessionID string) (ReconnectSnapshot, error) {
	liveSnapshot, err := service.GetSnapshot(ctx, sessionID)
	if err != nil {
		return ReconnectSnapshot{}, err
	}

	boundaryMeta, boundarySnapshot, found, err := service.loadLatestPhaseBoundarySnapshot(ctx, sessionID)
	if err != nil {
		return ReconnectSnapshot{}, err
	}
	if !found {
		now := time.Now().UTC()
		boundaryMeta = PhaseBoundarySnapshotMeta{
			ID:        "",
			SessionID: sessionID,
			Turn:      liveSnapshot.TurnState.Turn,
			Phase:     liveSnapshot.TurnState.Phase,
			CreatedAt: now,
		}
		boundarySnapshot = liveSnapshot
	}

	recent, err := service.listPhaseBoundarySnapshotMetas(ctx, sessionID, defaultReconnectHistoryLimit)
	if err != nil {
		return ReconnectSnapshot{}, err
	}
	if len(recent) == 0 {
		recent = append(recent, boundaryMeta)
	}

	return ReconnectSnapshot{
		Session:          liveSnapshot,
		Boundary:         boundaryMeta,
		BoundarySession:  boundarySnapshot,
		RecentBoundaries: recent,
	}, nil
}

// recordPhaseBoundarySnapshot 在阶段切换边界落盘一份可恢复快照。
// 写入采用 upsert，并按会话维度裁剪历史数量，防止快照表无限增长。
func (service *Service) recordPhaseBoundarySnapshot(
	ctx context.Context,
	state *State,
	units []unit.Record,
) error {
	if service == nil || service.db == nil || state == nil || strings.TrimSpace(state.ID) == "" {
		return nil
	}
	if units == nil {
		loaded, err := service.units.ListBySession(ctx, state.ID)
		if err != nil {
			return fmt.Errorf("list units for phase boundary snapshot: %w", err)
		}
		units = loaded
	}

	snapshot := buildSnapshot(*state, units)
	encodedSnapshot, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal phase boundary snapshot: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `
		INSERT INTO session_phase_snapshots (id, session_id, turn, phase, snapshot_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, turn, phase) DO UPDATE SET
			snapshot_json = excluded.snapshot_json,
			created_at = excluded.created_at
		`
	if dbdialect.IsMySQL(service.db) {
		query = `
		INSERT INTO session_phase_snapshots (id, session_id, turn, phase, snapshot_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			snapshot_json = VALUES(snapshot_json),
			created_at = VALUES(created_at)
		`
	}
	if _, err := service.db.ExecContext(
		ctx,
		query,
		uuid.NewString(),
		state.ID,
		state.TurnState.Turn,
		string(state.TurnState.Phase),
		string(encodedSnapshot),
		now,
	); err != nil {
		return fmt.Errorf("upsert phase boundary snapshot: %w", err)
	}

	trimQuery := `
		DELETE FROM session_phase_snapshots
		WHERE
			session_id = ? AND
			id NOT IN (
				SELECT id
				FROM session_phase_snapshots
				WHERE session_id = ?
				ORDER BY created_at DESC
				LIMIT ?
			)
		`
	if dbdialect.IsMySQL(service.db) {
		trimQuery = `
		DELETE FROM session_phase_snapshots
		WHERE
			session_id = ? AND
			id NOT IN (
				SELECT id FROM (
					SELECT id
					FROM session_phase_snapshots
					WHERE session_id = ?
					ORDER BY created_at DESC
					LIMIT ?
				) retained_phase_snapshots
			)
		`
	}
	if _, err := service.db.ExecContext(
		ctx,
		trimQuery,
		state.ID,
		state.ID,
		maxPhaseBoundarySnapshots,
	); err != nil {
		return fmt.Errorf("trim phase boundary snapshots: %w", err)
	}

	return nil
}

// loadLatestPhaseBoundarySnapshot 读取某会话最近一条阶段边界快照及其元信息。
func (service *Service) loadLatestPhaseBoundarySnapshot(
	ctx context.Context,
	sessionID string,
) (PhaseBoundarySnapshotMeta, Snapshot, bool, error) {
	if service == nil || service.db == nil || strings.TrimSpace(sessionID) == "" {
		return PhaseBoundarySnapshotMeta{}, Snapshot{}, false, nil
	}

	var (
		id           string
		turn         int
		phaseValue   string
		snapshotJSON string
		createdAtRaw string
	)
	err := service.db.QueryRowContext(
		ctx,
		`
		SELECT id, turn, phase, snapshot_json, created_at
		FROM session_phase_snapshots
		WHERE session_id = ?
		ORDER BY created_at DESC
		LIMIT 1
		`,
		sessionID,
	).Scan(&id, &turn, &phaseValue, &snapshotJSON, &createdAtRaw)
	if err != nil {
		if err == sql.ErrNoRows {
			return PhaseBoundarySnapshotMeta{}, Snapshot{}, false, nil
		}
		return PhaseBoundarySnapshotMeta{}, Snapshot{}, false, fmt.Errorf("query latest phase boundary snapshot: %w", err)
	}

	createdAt, err := parseTimestamp(createdAtRaw)
	if err != nil {
		createdAt = time.Now().UTC()
	}

	var snapshot Snapshot
	if err := json.Unmarshal([]byte(snapshotJSON), &snapshot); err != nil {
		return PhaseBoundarySnapshotMeta{}, Snapshot{}, false, fmt.Errorf("decode latest phase boundary snapshot: %w", err)
	}
	meta := PhaseBoundarySnapshotMeta{
		ID:        id,
		SessionID: sessionID,
		Turn:      turn,
		Phase:     turns.Phase(phaseValue),
		CreatedAt: createdAt,
	}
	return meta, snapshot, true, nil
}

// listPhaseBoundarySnapshotMetas 按时间倒序列出阶段边界元信息，不加载完整快照正文。
func (service *Service) listPhaseBoundarySnapshotMetas(
	ctx context.Context,
	sessionID string,
	limit int,
) ([]PhaseBoundarySnapshotMeta, error) {
	if service == nil || service.db == nil || strings.TrimSpace(sessionID) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = defaultReconnectHistoryLimit
	}
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT id, turn, phase, created_at
		FROM session_phase_snapshots
		WHERE session_id = ?
		ORDER BY created_at DESC
		LIMIT ?
		`,
		sessionID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query phase boundary history: %w", err)
	}
	defer rows.Close()

	result := make([]PhaseBoundarySnapshotMeta, 0, limit)
	for rows.Next() {
		var (
			meta         PhaseBoundarySnapshotMeta
			phaseValue   string
			createdAtRaw string
		)
		if err := rows.Scan(&meta.ID, &meta.Turn, &phaseValue, &createdAtRaw); err != nil {
			return nil, fmt.Errorf("scan phase boundary row: %w", err)
		}
		createdAt, parseErr := parseTimestamp(createdAtRaw)
		if parseErr != nil {
			createdAt = time.Now().UTC()
		}
		meta.SessionID = sessionID
		meta.Phase = turns.Phase(phaseValue)
		meta.CreatedAt = createdAt
		result = append(result, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate phase boundary rows: %w", err)
	}
	return result, nil
}
