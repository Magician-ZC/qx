package session

// 文件说明：在对局结束后归档幸存单位殿堂记录，生成生涯摘要并同步冷热存储。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
)

// hallBiographyPayload 结构体用于承载该模块的核心数据。
type hallBiographyPayload struct {
	Summary string `json:"summary"`
}

var hallBiographySchema = []byte(`{
  "type":"object",
  "properties":{
    "summary":{"type":"string","minLength":1}
  },
  "required":["summary"],
  "additionalProperties":false
}`)

// persistHallOfFame 在对局结束后把幸存单位写入名人堂档案。
// 档案包含摘要、生涯高光事件，并尝试同步到冷存储层。
func (service *Service) persistHallOfFame(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
) error {
	if service == nil || state == nil || state.Outcome == OutcomeOngoing {
		return nil
	}

	unitIDs := append([]string{}, state.PlayerUnitIDs...)
	unitIDs = append(unitIDs, state.EnemyUnitIDs...)
	for _, unitID := range unitIDs {
		record := byID[unitID]
		if record == nil || record.Status.LifeState == unit.LifeStateDead {
			continue
		}

		topEvents := service.collectLegacyTopEvents(ctx, *state, *record, 30)
		summary, result, interaction := service.generateHallBiography(ctx, *state, *record, topEvents)
		service.appendLLMInteractionWithSpend(ctx, state, interaction)
		if strings.TrimSpace(summary) == "" {
			summary = fallbackHallBiography(*record, state.Outcome, topEvents)
		}

		encodedTopEvents, err := json.Marshal(topEvents)
		if err != nil {
			return fmt.Errorf("marshal hall top events: %w", err)
		}
		entryID := uuid.NewString()
		createdAt := time.Now().UTC()
		state.HallArchiveEntries = upsertHallArchiveEntry(state.HallArchiveEntries, HallArchiveEntry{
			ID:           entryID,
			UnitID:       record.ID,
			UnitName:     record.DisplayName(),
			FactionID:    record.FactionID,
			Outcome:      state.Outcome,
			Biography:    summary,
			TopEvents:    append([]string{}, topEvents...),
			CreatedAt:    createdAt,
			Provider:     result.Provider,
			Model:        result.Model,
			UsedFallback: result.UsedFallback,
		})
		query := `
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
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source_session_id, source_unit_id) DO UPDATE SET
				biography_summary = excluded.biography_summary,
				top_events_json = excluded.top_events_json,
				outcome = excluded.outcome
			`
		if dbdialect.IsMySQL(service.db) {
			query = `
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
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				biography_summary = VALUES(biography_summary),
				top_events_json = VALUES(top_events_json),
				outcome = VALUES(outcome)
			`
		}
		if _, err := service.db.ExecContext(
			ctx,
			query,
			entryID,
			state.ID,
			record.ID,
			record.DisplayName(),
			record.FactionID,
			string(state.Outcome),
			summary,
			string(encodedTopEvents),
			createdAt.Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("persist hall entry for %s: %w", record.DisplayName(), err)
		}
		if err := service.upsertColdHallEntry(ctx, coldHallEntry{
			ID:              entryID,
			SourceSessionID: state.ID,
			SourceUnitID:    record.ID,
			UnitName:        record.DisplayName(),
			UnitFactionID:   record.FactionID,
			Outcome:         string(state.Outcome),
			Biography:       summary,
			TopEventsJSON:   string(encodedTopEvents),
			CreatedAt:       createdAt,
		}); err != nil {
			appendLog(
				state,
				"legacy_archive_l3_failed",
				fmt.Sprintf("%s 的殿堂档案写入 L3 失败：%v", record.DisplayName(), err),
				record.ID,
				"",
			)
		} else if service.coldStore != nil {
			appendLog(
				state,
				"legacy_archive_l3",
				fmt.Sprintf("%s 的殿堂档案已同步到 PostgreSQL L3。", record.DisplayName()),
				record.ID,
				"",
			)
		}

		appendLog(
			state,
			"legacy_archive",
			fmt.Sprintf("%s 的战后档案已写入殿堂。", record.DisplayName()),
			record.ID,
			"",
		)

		// Keep the provider/model trace for debugging archive quality.
		if strings.TrimSpace(result.Provider) != "" {
			appendLog(
				state,
				"legacy_archive_trace",
				fmt.Sprintf("%s 的殿堂摘要由 %s/%s 生成。", record.DisplayName(), result.Provider, result.Model),
				record.ID,
				"",
			)
		}
	}
	return nil
}

// upsertHallArchiveEntry 把同一单位在当前对局内的战后档案保持为一条，避免重复推进时弹窗重复展示。
func upsertHallArchiveEntry(entries []HallArchiveEntry, entry HallArchiveEntry) []HallArchiveEntry {
	for index := range entries {
		if entries[index].UnitID == entry.UnitID {
			entries[index] = entry
			return entries
		}
	}
	return append(entries, entry)
}

// collectLegacyTopEvents 收集单位最具代表性的历史事件。
// 优先读取记忆表高显著性条目，不足时回退到 RawEventLog。
func (service *Service) collectLegacyTopEvents(
	ctx context.Context,
	state State,
	record unit.Record,
	limit int,
) []string {
	if limit <= 0 {
		limit = 30
	}
	summaries := make([]string, 0, limit)
	seen := map[string]struct{}{}

	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT summary
		FROM memories
		WHERE unit_id = ?
		ORDER BY salience DESC, recall_count DESC, created_at DESC
		LIMIT ?
		`,
		record.ID,
		limit,
	)
	if err == nil {
		for rows.Next() {
			var summary string
			if scanErr := rows.Scan(&summary); scanErr != nil {
				continue
			}
			summary = strings.TrimSpace(summary)
			if summary == "" {
				continue
			}
			if _, ok := seen[summary]; ok {
				continue
			}
			seen[summary] = struct{}{}
			summaries = append(summaries, summary)
		}
		rows.Close()
	}

	if len(summaries) < limit {
		for index := len(state.RawEventLog) - 1; index >= 0 && len(summaries) < limit; index-- {
			entry := state.RawEventLog[index]
			if entry.ActorUnitID != record.ID && entry.TargetUnitID != record.ID {
				continue
			}
			summary := strings.TrimSpace(entry.Summary)
			if summary == "" {
				continue
			}
			if _, ok := seen[summary]; ok {
				continue
			}
			seen[summary] = struct{}{}
			summaries = append(summaries, summary)
		}
	}

	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return summaries
}

// generateHallBiography 生成单位“战后人物档案摘要”。
// 模型失败或输出异常时自动回退本地模板。
func (service *Service) generateHallBiography(
	ctx context.Context,
	state State,
	record unit.Record,
	topEvents []string,
) (string, ai.CompletionResult, LLMInteraction) {
	systemPrompt := fmt.Sprintf(
		"你是《群像》的战后档案官。请基于单位 %s 的战斗经历，生成一段不超过 200 字的中文生平摘要。要突出其性格、行为倾向与关键经历。只返回 JSON。",
		record.DisplayName(),
	)
	userPrompt := buildHallBiographyPrompt(state, record, topEvents)

	if service.llm == nil {
		cause := "llm client is disabled"
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{FallbackCause: cause},
		}
		summary := fallbackHallBiography(record, state.Outcome, topEvents)
		return summary, result, buildLLMInteraction(state, record.ID, "legacy_summary", summary, systemPrompt, userPrompt, result, cause)
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskBackstory,
		Importance:     ai.ImportanceCheap, // 名人堂传记=低频低 stakes（分tier路由 flag 开时走廉价档；默认关零影响）
		SchemaName:     "session_hall_biography",
		ResponseSchema: hallBiographySchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.55,
		MaxTokens:      260,
	})
	if err != nil {
		summary := fallbackHallBiography(record, state.Outcome, topEvents)
		return summary, result, buildLLMInteraction(state, record.ID, "legacy_summary", summary, systemPrompt, userPrompt, result, err.Error())
	}

	var payload hallBiographyPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		summary := fallbackHallBiography(record, state.Outcome, topEvents)
		cause := fmt.Sprintf("decode hall biography payload: %v", err)
		return summary, result, buildLLMInteraction(state, record.ID, "legacy_summary", summary, systemPrompt, userPrompt, result, cause)
	}

	summary := limitTextRunes(strings.TrimSpace(payload.Summary), 200)
	if summary == "" {
		summary = fallbackHallBiography(record, state.Outcome, topEvents)
	}
	return summary, result, buildLLMInteraction(state, record.ID, "legacy_summary", summary, systemPrompt, userPrompt, result, "")
}

// buildHallBiographyPrompt 组装名人堂摘要提示词，注入对局结果、人格与关键事件。
func buildHallBiographyPrompt(state State, record unit.Record, topEvents []string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "对局结果: %s\n", state.Outcome)
	fmt.Fprintf(&builder, "单位: %s\n", describeUnit(record, nil))
	fmt.Fprintf(&builder, "性格: %s\n", summarizeActorPersonality(record))
	fmt.Fprintf(&builder, "关键事件(最多30):\n")
	if len(topEvents) == 0 {
		fmt.Fprintln(&builder, "无")
	} else {
		for index, event := range topEvents {
			fmt.Fprintf(&builder, "%d. %s\n", index+1, strings.TrimSpace(event))
		}
	}
	fmt.Fprintln(&builder, "输出规则: summary 不超过 200 字，必须像战后人物档案。")
	return builder.String()
}

// fallbackHallBiography 生成无需模型即可使用的名人堂摘要回退文本。
func fallbackHallBiography(record unit.Record, outcome Outcome, topEvents []string) string {
	outcomeText := "战局未定"
	switch outcome {
	case OutcomeVictory:
		outcomeText = "所在阵营取得胜利"
	case OutcomeDefeat:
		outcomeText = "所在阵营遭遇失利"
	case OutcomeDraw:
		outcomeText = "战局以平局收场"
	}
	summary := fmt.Sprintf(
		"%s 在本局中%s，作战风格受其性格与记忆驱动，并持续自行判断风险、补给与交战节奏。",
		record.DisplayName(),
		outcomeText,
	)
	if len(topEvents) > 0 {
		highlights := topEvents
		if len(highlights) > 3 {
			highlights = highlights[:3]
		}
		summary = fmt.Sprintf("%s 关键经历：%s。", summary, strings.Join(highlights, "；"))
	}
	return limitTextRunes(summary, 200)
}

// injectHallMemoriesForUnit 把历史名人堂档案注入到新局单位记忆中。
// 读取顺序为 L3 冷存储优先，缺失时回退本地表。
func (service *Service) injectHallMemoriesForUnit(
	ctx context.Context,
	state *State,
	record *unit.Record,
) error {
	if service == nil || record == nil {
		return nil
	}

	var biographySummary string
	var topEventsJSON string
	err := sql.ErrNoRows
	if summary, topEvents, coldErr := service.queryColdHallEntry(ctx, record.DisplayName(), record.FactionID); coldErr == nil {
		biographySummary = summary
		topEventsJSON = topEvents
		err = nil
	} else if coldErr != sql.ErrNoRows {
		if state != nil {
			appendLog(
				state,
				"legacy_return_l3_failed",
				fmt.Sprintf("%s 的 L3 殿堂读取失败，已回退本地档案。", record.DisplayName()),
				record.ID,
				"",
			)
		}
	}
	if err == sql.ErrNoRows {
		err = service.db.QueryRowContext(
			ctx,
			`
			SELECT biography_summary, top_events_json
			FROM hall_of_fame_entries
			WHERE unit_name = ? AND unit_faction_id = ?
			ORDER BY created_at DESC
			LIMIT 1
			`,
			record.DisplayName(),
			record.FactionID,
		).Scan(&biographySummary, &topEventsJSON)
	}
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("query hall entry for %s: %w", record.DisplayName(), err)
	}

	turn := 1
	if state != nil && state.TurnState.Turn > 0 {
		turn = state.TurnState.Turn
	}

	if strings.TrimSpace(biographySummary) != "" {
		if err := service.storeMemoryAndSyncHighlights(
			ctx,
			record,
			turn,
			fmt.Sprintf("我想起前尘：%s", strings.TrimSpace(biographySummary)),
			"hall_return_biography",
			0,
		); err != nil {
			return err
		}
	}

	var topEvents []string
	_ = json.Unmarshal([]byte(topEventsJSON), &topEvents)
	if len(topEvents) > 30 {
		topEvents = topEvents[:30]
	}
	for _, event := range topEvents {
		event = strings.TrimSpace(event)
		if event == "" {
			continue
		}
		if err := service.storeMemoryAndSyncHighlights(
			ctx,
			record,
			turn,
			fmt.Sprintf("我记得：%s", event),
			"hall_return_event",
			0,
		); err != nil {
			return err
		}
	}

	if err := service.units.Save(ctx, *record); err != nil {
		return err
	}
	if state != nil {
		appendLog(
			state,
			"legacy_return",
			fmt.Sprintf("%s 带着上一局的记忆重返战场。", record.DisplayName()),
			record.ID,
			"",
		)
	}
	return nil
}

func markAsyncHallMemoryRunning(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" {
		return false
	}
	asyncHallMemoryRegistry.Lock()
	defer asyncHallMemoryRegistry.Unlock()
	if _, exists := asyncHallMemoryRegistry.running[key]; exists {
		return false
	}
	asyncHallMemoryRegistry.running[key] = struct{}{}
	return true
}

func unmarkAsyncHallMemoryRunning(key string) {
	key = strings.TrimSpace(key)
	if key == "" {
		return
	}
	asyncHallMemoryRegistry.Lock()
	delete(asyncHallMemoryRegistry.running, key)
	asyncHallMemoryRegistry.Unlock()
}

func hallMemoryRefreshKey(sessionID string, unitIDs []string) string {
	filtered := make([]string, 0, len(unitIDs))
	for _, unitID := range unitIDs {
		unitID = strings.TrimSpace(unitID)
		if unitID == "" {
			continue
		}
		filtered = append(filtered, unitID)
	}
	if len(filtered) == 0 {
		return ""
	}
	return strings.TrimSpace(sessionID) + ":" + strings.Join(filtered, ",")
}

func (service *Service) refreshHallMemoriesAsync(sessionID string, unitIDs []string) {
	if service == nil || strings.TrimSpace(sessionID) == "" || len(unitIDs) == 0 {
		return
	}
	key := hallMemoryRefreshKey(sessionID, unitIDs)
	if !markAsyncHallMemoryRunning(key) {
		return
	}
	go func() {
		defer unmarkAsyncHallMemoryRunning(key)

		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		state, units, err := service.loadSession(ctx, sessionID)
		if err != nil {
			return
		}
		byID := mapRecordsByID(units)
		updated := false
		for _, unitID := range unitIDs {
			record := byID[strings.TrimSpace(unitID)]
			if record == nil {
				continue
			}
			beforeHighlights := len(record.Memory.Highlights)
			beforeRecent := len(record.Memory.RecentEventIDs)
			beforeLogs := len(state.Logs)
			if err := service.injectHallMemoriesForUnit(ctx, &state, record); err != nil {
				continue
			}
			if len(record.Memory.Highlights) != beforeHighlights || len(record.Memory.RecentEventIDs) != beforeRecent || len(state.Logs) != beforeLogs {
				updated = true
			}
		}
		if !updated {
			return
		}
		if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
			return
		}
		if service.progressReporter != nil {
			service.progressReporter("hall_memories_refreshed", buildSnapshot(state, snapshotUnitsFromByID(state, byID)), map[string]any{
				"unit_ids": append([]string{}, unitIDs...),
			})
		}
	}()
}
