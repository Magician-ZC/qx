package session

// 文件说明：cold_storage.go，冷存储（L3）档案适配层，负责名人堂数据在 PostgreSQL 中的读写同步。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const coldStorageSchemaSQL = `
CREATE TABLE IF NOT EXISTS hall_of_fame_entries (
	id TEXT PRIMARY KEY,
	source_session_id TEXT NOT NULL,
	source_unit_id TEXT NOT NULL,
	unit_name TEXT NOT NULL,
	unit_faction_id TEXT NOT NULL,
	outcome TEXT NOT NULL,
	biography_summary TEXT NOT NULL,
	top_events_json TEXT NOT NULL DEFAULT '[]',
	created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(source_session_id, source_unit_id)
);

CREATE INDEX IF NOT EXISTS idx_hall_of_fame_entries_unit_name
	ON hall_of_fame_entries(unit_name, created_at DESC);
`

// coldHallEntry 表示写入冷存储荣誉榜的一条记录。
type coldHallEntry struct {
	ID              string
	SourceSessionID string
	SourceUnitID    string
	UnitName        string
	UnitFactionID   string
	Outcome         string
	Biography       string
	TopEventsJSON   string
	CreatedAt       time.Time
}

// EnsureColdStorageSchema 确保冷存储荣誉榜表结构已创建。
func EnsureColdStorageSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.ExecContext(ctx, coldStorageSchemaSQL); err != nil {
		return fmt.Errorf("ensure cold storage schema: %w", err)
	}
	return nil
}

// ensureColdStorageSchema 在 Service 内按需初始化冷存储表结构。
func (service *Service) ensureColdStorageSchema(ctx context.Context) error {
	if service == nil || service.coldStore == nil {
		return nil
	}

	service.coldSchemaMu.Lock()
	ready := service.coldSchemaReady
	service.coldSchemaMu.Unlock()
	if ready {
		return nil
	}

	if err := EnsureColdStorageSchema(ctx, service.coldStore); err != nil {
		return err
	}

	service.coldSchemaMu.Lock()
	service.coldSchemaReady = true
	service.coldSchemaMu.Unlock()
	return nil
}

// upsertColdHallEntry 向冷存储写入或更新一条荣誉榜记录。
func (service *Service) upsertColdHallEntry(ctx context.Context, entry coldHallEntry) error {
	if service == nil || service.coldStore == nil {
		return nil
	}
	if err := service.ensureColdStorageSchema(ctx); err != nil {
		return err
	}
	if strings.TrimSpace(entry.ID) == "" {
		return fmt.Errorf("cold hall entry id is required")
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}

	if _, err := service.coldStore.ExecContext(
		ctx,
		`
		INSERT INTO hall_of_fame_entries (
			id,
			source_session_id,
			source_unit_id,
			unit_name,
			unit_faction_id,
			outcome,
			biography_summary,
			top_events_json,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT(source_session_id, source_unit_id) DO UPDATE SET
			unit_name = EXCLUDED.unit_name,
			unit_faction_id = EXCLUDED.unit_faction_id,
			outcome = EXCLUDED.outcome,
			biography_summary = EXCLUDED.biography_summary,
			top_events_json = EXCLUDED.top_events_json,
			created_at = EXCLUDED.created_at
		`,
		entry.ID,
		entry.SourceSessionID,
		entry.SourceUnitID,
		entry.UnitName,
		entry.UnitFactionID,
		entry.Outcome,
		entry.Biography,
		entry.TopEventsJSON,
		entry.CreatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("upsert cold hall entry: %w", err)
	}
	return nil
}

// queryColdHallEntry 按单位名与阵营查询最近一条冷存储荣誉档案。
func (service *Service) queryColdHallEntry(
	ctx context.Context,
	unitName string,
	factionID string,
) (string, string, error) {
	if service == nil || service.coldStore == nil {
		return "", "", sql.ErrNoRows
	}
	if err := service.ensureColdStorageSchema(ctx); err != nil {
		return "", "", err
	}

	var biographySummary string
	var topEventsJSON string
	err := service.coldStore.QueryRowContext(
		ctx,
		`
		SELECT biography_summary, top_events_json
		FROM hall_of_fame_entries
		WHERE unit_name = $1 AND unit_faction_id = $2
		ORDER BY created_at DESC
		LIMIT 1
		`,
		strings.TrimSpace(unitName),
		strings.TrimSpace(factionID),
	).Scan(&biographySummary, &topEventsJSON)
	if err != nil {
		return "", "", err
	}
	return biographySummary, topEventsJSON, nil
}

// deleteColdHallEntriesBySession 删除指定会话写入的冷存储荣誉记录。
func (service *Service) deleteColdHallEntriesBySession(ctx context.Context, sessionID string) (int64, error) {
	if service == nil || service.coldStore == nil {
		return 0, nil
	}
	if err := service.ensureColdStorageSchema(ctx); err != nil {
		return 0, err
	}
	execResult, execErr := service.coldStore.ExecContext(
		ctx,
		`DELETE FROM hall_of_fame_entries WHERE source_session_id = $1`,
		strings.TrimSpace(sessionID),
	)
	deleted, err := execRowsAffected(execResult, execErr, "delete cold hall entries")
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
