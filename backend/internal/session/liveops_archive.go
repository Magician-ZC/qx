package session

// 文件说明：Live-Ops 赛季收尾的名人堂归档薄导出（liveops.HallArchiver 的 session 侧实现入口）。
// liveops.FinalizeSeason 对赛季世界的每个存活成员调一次 ArchiveCharacterToHall——这里复用既有名人堂归档
// 原语（hall_of_fame_entries 表 INSERT + upsertColdHallEntry 同步 L3），不重写结算、不碰受保护状态字段。
// best-effort 友好：导出错误供 liveops 端面（liveops 侧吞错只记 log，绝不阻断封存主链路）。

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
)

// ArchiveCharacterToHall 把单个角色（按 unitID）回流名人堂（hall_of_fame_entries）。
// 供 liveops.HallArchiver 适配器复用：赛季收尾时对世界每个存活成员调一次。
// 复用 persistHallOfFame 的同口径 INSERT（按 (source_session_id, source_unit_id) 幂等 upsert）+ upsertColdHallEntry 同步 L3。
// 与对局结束的 persistHallOfFame 不同：赛季收尾无 *State 上下文，故 biography 走规则 fallback（不调 LLM，省成本/确定性），
// source_session_id 用角色的 SessionID（无则用 worldID 兜底，保证幂等键非空）。不触碰任何受保护状态字段。
func (service *Service) ArchiveCharacterToHall(ctx context.Context, worldID, characterID string) error {
	if service == nil || service.db == nil || service.units == nil {
		return fmt.Errorf("archive character to hall: missing dependencies")
	}
	if characterID == "" {
		return fmt.Errorf("archive character to hall: empty character id")
	}
	record, err := service.units.GetByID(ctx, characterID)
	if err != nil {
		return fmt.Errorf("archive character to hall: load unit %s: %w", characterID, err)
	}
	// 死亡单位不入名人堂（与 persistHallOfFame 跳过 LifeStateDead 同口径）。
	if record.Status.LifeState == unit.LifeStateDead {
		return nil
	}

	// 收尾归档无对局 Outcome 上下文，按「赛季落幕」语义走规则 fallback 传记（不调 LLM）。
	topEvents := service.collectLegacyTopEvents(ctx, State{ID: record.SessionID}, record, 30)
	summary := fallbackHallBiography(record, OutcomeOngoing, topEvents)

	encodedTopEvents, err := json.Marshal(topEvents)
	if err != nil {
		return fmt.Errorf("archive character to hall: marshal top events: %w", err)
	}
	// 幂等键 source_session_id 必须非空：优先角色 SessionID，缺失则用 worldID 兜底（赛季世界级归档）。
	sourceSessionID := record.SessionID
	if sourceSessionID == "" {
		sourceSessionID = worldID
	}
	entryID := uuid.NewString()
	createdAt := time.Now().UTC()

	query := `
		INSERT INTO hall_of_fame_entries (
			id, source_session_id, source_unit_id, unit_name, unit_faction_id,
			outcome, biography_summary, top_events_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_session_id, source_unit_id) DO UPDATE SET
			biography_summary = excluded.biography_summary,
			top_events_json = excluded.top_events_json,
			outcome = excluded.outcome`
	if dbdialect.IsMySQL(service.db) {
		query = `
		INSERT INTO hall_of_fame_entries (
			id, source_session_id, source_unit_id, unit_name, unit_faction_id,
			outcome, biography_summary, top_events_json, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			biography_summary = VALUES(biography_summary),
			top_events_json = VALUES(top_events_json),
			outcome = VALUES(outcome)`
	}
	if _, err := service.db.ExecContext(ctx, query,
		entryID, sourceSessionID, record.ID, record.DisplayName(), record.FactionID,
		string(OutcomeOngoing), summary, string(encodedTopEvents), createdAt.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("archive character to hall: persist entry for %s: %w", record.DisplayName(), err)
	}
	// 同步 L3 冷存（配了 postgres 才生效；best-effort：失败不回滚已落的热存档）。
	if err := service.upsertColdHallEntry(ctx, coldHallEntry{
		ID:              entryID,
		SourceSessionID: sourceSessionID,
		SourceUnitID:    record.ID,
		UnitName:        record.DisplayName(),
		UnitFactionID:   record.FactionID,
		Outcome:         string(OutcomeOngoing),
		Biography:       summary,
		TopEventsJSON:   string(encodedTopEvents),
		CreatedAt:       createdAt,
	}); err != nil {
		// best-effort：L3 同步失败不影响赛季收尾归档主结果。
		_ = err
	}
	return nil
}
