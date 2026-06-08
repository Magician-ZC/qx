package session

// 文件说明：实现结构化记忆存储、显著度衰减、分类裁剪、闪回召回与 highlights 同步全链路。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	memoryDecayTauTurns   = 120.0
	memoryDecayAlpha      = 2.5
	memoryRecallBoost     = 0.3
	memoryFlashbackBoost  = 0.3
	memoryHighlightBudget = 40
	memoryFlashbackMin    = 0.8
	memory2WindowTurns    = 10
	memory2LookbackTurns  = 20
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	memoryCategoryEntity    = "entity"
	memoryCategorySpatial   = "spatial"
	memoryCategoryRelation  = "relation"
	memoryCategoryEvent     = "event"
	memoryCategoryKnowledge = "knowledge"
	memoryCategoryMemory2   = "memory2"
)

var memoryCategoryCaps = map[string]int{
	memoryCategoryEntity:    300,
	memoryCategorySpatial:   200,
	memoryCategoryRelation:  150,
	memoryCategoryEvent:     500,
	memoryCategoryKnowledge: 180,
	memoryCategoryMemory2:   120,
}

type memory2CompactionPayload struct {
	Memory2 string `json:"memory2"`
}

var memory2CompactionSchema = []byte(`{
  "type":"object",
  "properties":{
    "memory2":{"type":"string","minLength":1,"maxLength":160}
  },
  "required":["memory2"],
  "additionalProperties":false
}`)

// memoryMetadata 结构体用于承载该模块的核心数据。
type memoryMetadata struct {
	Turn           int     `json:"turn"`
	Importance     int     `json:"importance"`
	BaseSalience   float64 `json:"base_salience"`
	Permanent      bool    `json:"permanent"`
	GroupResonance bool    `json:"group_resonance,omitempty"`
	Source         string  `json:"source,omitempty"`
	BucketStart    int     `json:"bucket_start,omitempty"`
	BucketEnd      int     `json:"bucket_end,omitempty"`
}

// memoryRow 结构体用于承载该模块的核心数据。
type memoryRow struct {
	ID            string
	UnitID        string
	Summary       string
	Category      string
	EmotionWeight float64
	Metadata      memoryMetadata
}

// flashbackFeatures 结构体用于承载该模块的核心数据。
type flashbackFeatures struct {
	Terrain    string
	Weather    string
	EnemyNames []string
}

// storeMemoryAndSyncHighlights 负责结构化记忆入库、分类裁剪与 highlights 同步。
func (service *Service) storeMemoryAndSyncHighlights(
	ctx context.Context,
	record *unit.Record,
	turn int,
	summary string,
	source string,
	importanceBoost int,
) error {
	if service == nil || record == nil {
		return nil
	}
	if turn <= 0 {
		turn = 1
	}
	if strings.TrimSpace(summary) == "" {
		return nil
	}

	// 避免同一动作重试时反复写入完全相同的句子。
	var latestSummary string
	_ = service.db.QueryRowContext(
		ctx,
		`SELECT summary FROM memories WHERE unit_id = ? ORDER BY created_at DESC LIMIT 1`,
		record.ID,
	).Scan(&latestSummary)
	insertedMemoryID := ""
	if strings.TrimSpace(latestSummary) != strings.TrimSpace(summary) {
		category := inferMemoryCategory(summary, source)
		importance := inferMemoryImportance(summary, category)
		if importanceBoost != 0 {
			importance = clampMemoryImportance(importance + importanceBoost)
		}
		emotionWeight := inferMemoryEmotionWeight(summary)
		memoryID := uuid.NewString()
		meta := memoryMetadata{
			Turn:         turn,
			Importance:   importance,
			BaseSalience: 1,
			Permanent:    importance >= 6 || emotionWeight >= 1.5,
			Source:       strings.TrimSpace(source),
		}
		metaJSON, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal memory metadata: %w", err)
		}
		salience := computeMemorySalience(turn, meta, emotionWeight)
		if _, err := service.db.ExecContext(
			ctx,
			`
			INSERT INTO memories (
				id,
				unit_id,
				category,
				summary,
				emotion_weight,
				salience,
				recall_count,
				metadata_json,
				created_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
			`,
			memoryID,
			record.ID,
			category,
			strings.TrimSpace(summary),
			emotionWeight,
			salience,
			string(metaJSON),
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return fmt.Errorf("insert memory: %w", err)
		}
		insertedMemoryID = memoryID
		if service.ensureMemoryFTS(ctx) == nil {
			_ = service.upsertMemoryFTS(ctx, insertedMemoryID, record.ID, strings.TrimSpace(summary))
		}
		if err := service.applyGroupResonance(ctx, record.SessionID, record.FactionID, turn, category, strings.TrimSpace(summary)); err != nil {
			return err
		}
	}

	if err := service.refreshUnitMemorySalience(ctx, record.ID, turn); err != nil {
		return err
	}
	service.markUnitMemoryRefreshedTurn(record.ID, turn)
	if err := service.enforceMemoryCaps(ctx, record.ID); err != nil {
		return err
	}
	if err := service.syncRecordHighlightsFromMemories(ctx, record, memoryHighlightBudget); err != nil {
		return err
	}
	return nil
}

// refreshSessionMemoryDecay 在回合边界刷新全体单位记忆显著度并同步 highlights。
func (service *Service) refreshSessionMemoryDecay(ctx context.Context, state *State, units []unit.Record) error {
	if service == nil || state == nil {
		return nil
	}
	turn := state.TurnState.Turn
	if turn <= 0 {
		turn = 1
	}
	for index := range units {
		record := &units[index]
		if err := service.compactMemory2ForUnit(ctx, state, record, turn); err != nil {
			return err
		}
		if err := service.refreshUnitMemorySalience(ctx, record.ID, turn); err != nil {
			return err
		}
		service.markUnitMemoryRefreshedTurn(record.ID, turn)
		if err := service.enforceMemoryCaps(ctx, record.ID); err != nil {
			return err
		}
		if err := service.syncRecordHighlightsFromMemories(ctx, record, memoryHighlightBudget); err != nil {
			return err
		}
		if err := service.units.Save(ctx, *record); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) compactMemory2ForUnit(ctx context.Context, state *State, record *unit.Record, currentTurn int) error {
	if service == nil || state == nil || record == nil || currentTurn <= 0 || currentTurn%memory2WindowTurns != 0 {
		return nil
	}
	startTurn := currentTurn - memory2LookbackTurns + 1
	if startTurn < 1 {
		startTurn = 1
	}
	endTurn := currentTurn - memory2WindowTurns
	if endTurn < startTurn {
		return nil
	}
	exists, err := service.memory2BucketExists(ctx, record.ID, startTurn, endTurn)
	if err != nil || exists {
		return err
	}
	rows, err := service.loadMemoriesForMemory2Compaction(ctx, record.ID, startTurn, endTurn)
	if err != nil || len(rows) == 0 {
		return err
	}
	summary, result, interaction := service.generateMemory2Compaction(ctx, *state, *record, startTurn, endTurn, rows)
	if strings.TrimSpace(interaction.Kind) != "" {
		service.appendLLMInteractionWithSpend(ctx, state, interaction)
	}
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	if err := service.storeMemory2Summary(ctx, record.ID, currentTurn, startTurn, endTurn, summary); err != nil {
		return err
	}
	if result.Provider != "" || result.Model != "" || result.UsedFallback {
		appendLog(state, "memory2", fmt.Sprintf("%s 已将 T%d-T%d 的旧记忆压缩为 memory2。", record.DisplayName(), startTurn, endTurn), record.ID, "")
	}
	return nil
}

func (service *Service) memory2BucketExists(ctx context.Context, unitID string, startTurn int, endTurn int) (bool, error) {
	var count int
	err := service.db.QueryRowContext(
		ctx,
		`SELECT COUNT(1) FROM memories WHERE unit_id = ? AND category = ? AND summary LIKE ?`,
		unitID,
		memoryCategoryMemory2,
		fmt.Sprintf("memory2：T%d-T%d%%", startTurn, endTurn),
	).Scan(&count)
	return count > 0, err
}

func (service *Service) loadMemoriesForMemory2Compaction(ctx context.Context, unitID string, startTurn int, endTurn int) ([]memoryRow, error) {
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT id, unit_id, summary, category, emotion_weight, metadata_json
		FROM memories
		WHERE unit_id = ? AND category != ?
		ORDER BY created_at ASC
		`,
		unitID,
		memoryCategoryMemory2,
	)
	if err != nil {
		return nil, fmt.Errorf("query memory2 source memories: %w", err)
	}
	defer rows.Close()

	items := make([]memoryRow, 0, 32)
	for rows.Next() {
		var row memoryRow
		var metadataJSON string
		if err := rows.Scan(&row.ID, &row.UnitID, &row.Summary, &row.Category, &row.EmotionWeight, &metadataJSON); err != nil {
			return nil, fmt.Errorf("scan memory2 source memory: %w", err)
		}
		row.Metadata = decodeMemoryMetadata(metadataJSON)
		if row.Metadata.Turn >= startTurn && row.Metadata.Turn <= endTurn && strings.TrimSpace(row.Summary) != "" {
			items = append(items, row)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory2 source memories: %w", err)
	}
	return items, nil
}

func (service *Service) generateMemory2Compaction(ctx context.Context, state State, record unit.Record, startTurn int, endTurn int, rows []memoryRow) (string, ai.CompletionResult, LLMInteraction) {
	fallback := fallbackMemory2Summary(startTurn, endTurn, rows)
	systemPrompt := fmt.Sprintf("你是《群像》中单位 %s 的长期记忆整理器。请把较早的逐回合 memory 压缩成一条 memory2，保留关键人物、伤害、位置、物品、关系变化和发现，不要写系统旁白。只能返回 JSON。", record.DisplayName())
	userPrompt := buildMemory2CompactionPrompt(record, startTurn, endTurn, rows)
	if service.llm == nil || llmBudgetGuardrailActive(state) {
		result := budgetGuardrailResult(state)
		result.UsedFallback = true
		return fallback, result, buildLLMInteraction(state, record.ID, "memory2_compaction", fallback, systemPrompt, userPrompt, result, "")
	}
	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskReflection,
		SchemaName:     "session_memory2_compaction",
		ResponseSchema: memory2CompactionSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.2,
		MaxTokens:      220,
		Timeout:        llmRequestTimeout,
	})
	if err != nil {
		result.UsedFallback = true
		return fallback, result, buildLLMInteraction(state, record.ID, "memory2_compaction", fallback, systemPrompt, userPrompt, result, err.Error())
	}
	var payload memory2CompactionPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil || strings.TrimSpace(payload.Memory2) == "" {
		result.UsedFallback = true
		cause := "memory2 compaction output empty"
		if err != nil {
			cause = err.Error()
		}
		return fallback, result, buildLLMInteraction(state, record.ID, "memory2_compaction", fallback, systemPrompt, userPrompt, result, cause)
	}
	summary := limitTextRunes(strings.TrimSpace(payload.Memory2), 160)
	return summary, result, buildLLMInteraction(state, record.ID, "memory2_compaction", summary, systemPrompt, userPrompt, result, "")
}

func buildMemory2CompactionPrompt(record unit.Record, startTurn int, endTurn int, rows []memoryRow) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "单位: %s\n", record.DisplayName())
	fmt.Fprintf(&builder, "压缩范围: T%d-T%d，也就是当前视角下前10到前20回合之间的旧 memory。\n", startTurn, endTurn)
	fmt.Fprintln(&builder, "原始 memory:")
	for _, row := range rows {
		fmt.Fprintf(&builder, "- T%d %s\n", row.Metadata.Turn, strings.TrimSpace(row.Summary))
	}
	fmt.Fprintln(&builder, "请返回 JSON：memory2。要求：1）第一人称；2）不超过160字；3）浓缩人物、地点、伤害数字、武器/物品、关系和发现；4）不要逐条复述。")
	return builder.String()
}

func fallbackMemory2Summary(startTurn int, endTurn int, rows []memoryRow) string {
	parts := make([]string, 0, 5)
	for _, row := range rows {
		text := strings.TrimSpace(row.Summary)
		if text == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("T%d %s", row.Metadata.Turn, text))
		if len(parts) >= 5 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return limitTextRunes(fmt.Sprintf("T%d-T%d旧记忆：%s", startTurn, endTurn, strings.Join(parts, "；")), 160)
}

func (service *Service) storeMemory2Summary(ctx context.Context, unitID string, currentTurn int, startTurn int, endTurn int, summary string) error {
	meta := memoryMetadata{Turn: endTurn, Importance: 7, BaseSalience: 1.6, Permanent: true, Source: "memory2_compaction", BucketStart: startTurn, BucketEnd: endTurn}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal memory2 metadata: %w", err)
	}
	memoryID := uuid.NewString()
	text := fmt.Sprintf("memory2：T%d-T%d %s", startTurn, endTurn, strings.TrimSpace(summary))
	if _, err := service.db.ExecContext(
		ctx,
		`
		INSERT INTO memories (id, unit_id, category, summary, emotion_weight, salience, recall_count, metadata_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
		`,
		memoryID,
		unitID,
		memoryCategoryMemory2,
		text,
		1.2,
		computeMemorySalience(currentTurn, meta, 1.2),
		string(metaJSON),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("insert memory2 summary: %w", err)
	}
	if service.ensureMemoryFTS(ctx) == nil {
		_ = service.upsertMemoryFTS(ctx, memoryID, unitID, text)
	}
	return nil
}

// memorySummaryForPrompt 从结构化记忆 + 闪回特征中挑选高价值片段，拼成决策上下文。
func (service *Service) memorySummaryForPrompt(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	limit int,
) string {
	if limit <= 0 {
		limit = 6
	}
	turn := state.TurnState.Turn
	if turn <= 0 {
		turn = 1
	}

	if service.shouldRefreshUnitMemorySalience(record.ID, turn) {
		if err := service.refreshUnitMemorySalience(ctx, record.ID, turn); err != nil {
			return summarizeUnitMemoryWithTurn(record, turn, limit)
		}
		service.markUnitMemoryRefreshedTurn(record.ID, turn)
	}
	features := buildFlashbackFeatures(state, byID, record)
	memory2Rows, _ := service.loadMemory2Summaries(ctx, record.ID, memoryCategoryCaps[memoryCategoryMemory2])
	recentStartTurn := recentMemoryStartTurnAfterMemory2(memory2Rows)
	structuredRows, err := service.loadRecentMemoriesForPrompt(ctx, record.ID, recentStartTurn, turn)
	if err != nil || len(structuredRows)+len(memory2Rows) == 0 {
		return summarizeUnitMemoryWithTurn(record, turn, limit)
	}

	merged := make([]memoryRow, 0, limit+8)
	seen := map[string]struct{}{}
	for _, row := range memory2Rows {
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		merged = append(merged, row)
	}
	for _, row := range structuredRows {
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		merged = append(merged, row)
	}

	if query := buildMemoryFTSQuery(features); query != "" && service.ensureMemoryFTS(ctx) == nil {
		if ids, err := service.searchMemoryFTS(ctx, record.ID, query, limit+8); err == nil && len(ids) > 0 {
			if rows, err := service.loadMemoriesByIDs(ctx, ids); err == nil {
				for _, row := range rows {
					if _, ok := seen[row.ID]; ok {
						continue
					}
					seen[row.ID] = struct{}{}
					merged = append(merged, row)
				}
			}
		}
	}

	flashbacks := selectFlashbackMemories(merged, features, 2)
	if service.shouldReinforceUnitMemories(record.ID, turn) {
		if len(flashbacks) > 0 {
			_ = service.reinforceFlashbackMemories(ctx, flashbacks)
		}
		_ = service.reinforceMemories(ctx, merged)
		service.markUnitMemoryReinforcedTurn(record.ID, turn)
	}

	maxLines := len(memory2Rows) + len(structuredRows) + len(flashbacks)
	if maxLines < limit {
		maxLines = limit
	}
	lines := make([]string, 0, maxLines)
	flashbackIDs := map[string]struct{}{}
	for _, row := range flashbacks {
		flashbackIDs[row.ID] = struct{}{}
		lines = append(lines, fmt.Sprintf("闪回：%s", memorySummaryLineWithTurn(row.Summary, turn, row.Metadata.Turn)))
		if len(lines) >= maxLines {
			return strings.Join(lines[:maxLines], "\n")
		}
	}
	for _, row := range merged {
		if _, ok := flashbackIDs[row.ID]; ok {
			continue
		}
		lines = append(lines, memorySummaryLineWithTurn(row.Summary, turn, row.Metadata.Turn))
		if len(lines) >= maxLines {
			break
		}
	}
	if len(lines) == 0 {
		return summarizeUnitMemoryWithTurn(record, turn, limit)
	}
	return strings.Join(lines, "\n")
}

func recentMemoryStartTurnAfterMemory2(memory2Rows []memoryRow) int {
	latestEnd := 0
	for _, row := range memory2Rows {
		if row.Metadata.BucketEnd > latestEnd {
			latestEnd = row.Metadata.BucketEnd
			continue
		}
		if row.Metadata.BucketEnd == 0 && row.Metadata.Turn > latestEnd {
			latestEnd = row.Metadata.Turn
		}
	}
	if latestEnd <= 0 {
		return 1
	}
	return latestEnd + 1
}

// memorySummaryLineWithTurn 为记忆补充相对时序标签，降低模型对绝对回合号的理解负担。
func memorySummaryLineWithTurn(summary string, currentTurn int, memoryTurn int) string {
	text := strings.TrimSpace(summary)
	if text == "" {
		return ""
	}
	label := memoryRelativeTurnLabel(currentTurn, memoryTurn)
	if label == "" {
		return text
	}
	return fmt.Sprintf("%s（%s）", text, label)
}

// memoryRelativeTurnLabel 把记忆回合号转换为“本回合/N回合前”标签。
func memoryRelativeTurnLabel(currentTurn int, memoryTurn int) string {
	if memoryTurn <= 0 {
		return "时间未知"
	}
	if currentTurn <= 0 {
		return fmt.Sprintf("T%d", memoryTurn)
	}
	delta := currentTurn - memoryTurn
	if delta <= 0 {
		return "本回合"
	}
	return fmt.Sprintf("%d回合前", delta)
}

// buildFlashbackFeatures 提取地形、天气与邻近敌人等闪回检索特征。
func buildFlashbackFeatures(state State, byID map[string]*unit.Record, record unit.Record) flashbackFeatures {
	coord := worldCoordOf(record)
	features := flashbackFeatures{
		Terrain: strings.TrimSpace(terrainDisplayName(terrainAt(state.Map, coord))),
		Weather: strings.TrimSpace(state.Weather.DisplayName),
	}
	if byID == nil {
		return features
	}

	enemyIDs := opposingIDs(state, record.FactionID)
	type enemyPair struct {
		name     string
		distance int
	}
	enemies := make([]enemyPair, 0, len(enemyIDs))
	for _, enemyID := range enemyIDs {
		target := byID[enemyID]
		if target == nil || !isBattleReady(*target) {
			continue
		}
		enemies = append(enemies, enemyPair{
			name: target.DisplayName(),
			distance: unit.HexDistance(
				record.Status.PositionQ,
				record.Status.PositionR,
				target.Status.PositionQ,
				target.Status.PositionR,
			),
		})
	}
	sort.Slice(enemies, func(i, j int) bool {
		if enemies[i].distance != enemies[j].distance {
			return enemies[i].distance < enemies[j].distance
		}
		return enemies[i].name < enemies[j].name
	})
	for _, enemy := range enemies {
		if strings.TrimSpace(enemy.name) == "" || enemy.distance > 5 {
			continue
		}
		features.EnemyNames = append(features.EnemyNames, enemy.name)
		if len(features.EnemyNames) >= 2 {
			break
		}
	}
	return features
}

// worldCoordOf 读取单位当前世界坐标。
func worldCoordOf(record unit.Record) world.Coord {
	return world.Coord{Q: record.Status.PositionQ, R: record.Status.PositionR}
}

// shouldRefreshUnitMemorySalience 判断本回合是否需要刷新该单位记忆衰减。
func (service *Service) shouldRefreshUnitMemorySalience(unitID string, turn int) bool {
	if service == nil || strings.TrimSpace(unitID) == "" {
		return true
	}
	if turn <= 0 {
		turn = 1
	}
	service.memoryRefreshMu.Lock()
	defer service.memoryRefreshMu.Unlock()
	if service.memoryRefreshTurn == nil {
		service.memoryRefreshTurn = map[string]int{}
		return true
	}
	lastTurn, ok := service.memoryRefreshTurn[unitID]
	return !ok || lastTurn < turn
}

// markUnitMemoryRefreshedTurn 记录该单位最近一次完成显著度刷新的回合。
func (service *Service) markUnitMemoryRefreshedTurn(unitID string, turn int) {
	if service == nil || strings.TrimSpace(unitID) == "" {
		return
	}
	if turn <= 0 {
		turn = 1
	}
	service.memoryRefreshMu.Lock()
	defer service.memoryRefreshMu.Unlock()
	if service.memoryRefreshTurn == nil {
		service.memoryRefreshTurn = map[string]int{}
	}
	service.memoryRefreshTurn[unitID] = turn
	if len(service.memoryRefreshTurn) <= 4096 {
		return
	}
	threshold := turn - 3
	for id, refreshedTurn := range service.memoryRefreshTurn {
		if refreshedTurn < threshold {
			delete(service.memoryRefreshTurn, id)
		}
	}
}

// shouldReinforceUnitMemories 判断本回合是否允许对召回记忆做强化。
func (service *Service) shouldReinforceUnitMemories(unitID string, turn int) bool {
	if service == nil || strings.TrimSpace(unitID) == "" {
		return true
	}
	if turn <= 0 {
		turn = 1
	}
	service.memoryRecallMu.Lock()
	defer service.memoryRecallMu.Unlock()
	if service.memoryRecallTurn == nil {
		service.memoryRecallTurn = map[string]int{}
		return true
	}
	lastTurn, ok := service.memoryRecallTurn[unitID]
	return !ok || lastTurn < turn
}

// markUnitMemoryReinforcedTurn 记录该单位最近一次记忆强化回合。
func (service *Service) markUnitMemoryReinforcedTurn(unitID string, turn int) {
	if service == nil || strings.TrimSpace(unitID) == "" {
		return
	}
	if turn <= 0 {
		turn = 1
	}
	service.memoryRecallMu.Lock()
	defer service.memoryRecallMu.Unlock()
	if service.memoryRecallTurn == nil {
		service.memoryRecallTurn = map[string]int{}
	}
	service.memoryRecallTurn[unitID] = turn
	if len(service.memoryRecallTurn) <= 4096 {
		return
	}
	threshold := turn - 3
	for id, reinforcedTurn := range service.memoryRecallTurn {
		if reinforcedTurn < threshold {
			delete(service.memoryRecallTurn, id)
		}
	}
}

// memoryFlashbackScore 计算记忆与当前情境的闪回匹配分。
func memoryFlashbackScore(summary string, features flashbackFeatures) float64 {
	text := strings.ToLower(strings.TrimSpace(summary))
	if text == "" {
		return 0
	}

	score := 0.0
	terrain := strings.ToLower(strings.TrimSpace(features.Terrain))
	weather := strings.ToLower(strings.TrimSpace(features.Weather))
	if terrain != "" && strings.Contains(text, terrain) {
		score += 0.45
	}
	if weather != "" && strings.Contains(text, weather) {
		score += 0.25
	}
	for _, enemyName := range features.EnemyNames {
		name := strings.ToLower(strings.TrimSpace(enemyName))
		if name == "" {
			continue
		}
		if strings.Contains(text, name) {
			score += 0.30
			break
		}
	}
	if containsAny(text, "伏击", "埋伏", "阵亡", "濒死", "重创", "火攻", "冲锋") {
		score += 0.10
	}
	if score > 1 {
		score = 1
	}
	return score
}

// selectFlashbackMemories 从候选记忆中挑选最可能触发闪回的条目。
func selectFlashbackMemories(rows []memoryRow, features flashbackFeatures, limit int) []memoryRow {
	if limit <= 0 || len(rows) == 0 {
		return nil
	}
	type scoredRow struct {
		row   memoryRow
		score float64
	}
	scored := make([]scoredRow, 0, len(rows))
	for _, row := range rows {
		score := memoryFlashbackScore(row.Summary, features)
		if score < memoryFlashbackMin {
			continue
		}
		scored = append(scored, scoredRow{row: row, score: score})
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].row.Summary < scored[j].row.Summary
	})

	selected := make([]memoryRow, 0, limit)
	for _, item := range scored {
		selected = append(selected, item.row)
		if len(selected) >= limit {
			break
		}
	}
	return selected
}

// refreshUnitMemorySalience 按衰减模型刷新单位全部记忆显著度。
func (service *Service) refreshUnitMemorySalience(ctx context.Context, unitID string, currentTurn int) error {
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT id, emotion_weight, metadata_json FROM memories WHERE unit_id = ?`,
		unitID,
	)
	if err != nil {
		return fmt.Errorf("query memories for salience refresh: %w", err)
	}

	type update struct {
		id       string
		salience float64
	}
	updates := make([]update, 0, 64)
	for rows.Next() {
		var id string
		var emotionWeight float64
		var metadataJSON string
		if err := rows.Scan(&id, &emotionWeight, &metadataJSON); err != nil {
			rows.Close()
			return fmt.Errorf("scan memory row: %w", err)
		}
		meta := decodeMemoryMetadata(metadataJSON)
		updates = append(updates, update{
			id:       id,
			salience: computeMemorySalience(currentTurn, meta, emotionWeight),
		})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate memory rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close memory rows: %w", err)
	}

	for _, updateItem := range updates {
		if _, err := service.db.ExecContext(
			ctx,
			`UPDATE memories SET salience = ? WHERE id = ?`,
			updateItem.salience,
			updateItem.id,
		); err != nil {
			return fmt.Errorf("update memory salience: %w", err)
		}
	}
	return nil
}

// applyGroupResonance 把同阵营共鸣事件扩散到相关单位记忆中。
func (service *Service) applyGroupResonance(
	ctx context.Context,
	sessionID string,
	factionID string,
	turn int,
	category string,
	summary string,
) error {
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(factionID) == "" || strings.TrimSpace(summary) == "" {
		return nil
	}
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT m.id, m.unit_id, m.metadata_json
		FROM memories m
		INNER JOIN units u ON u.id = m.unit_id
		WHERE
			u.session_id = ? AND
			u.faction_id = ? AND
			m.category = ? AND
			m.summary = ?
		`,
		sessionID,
		factionID,
		category,
		summary,
	)
	if err != nil {
		return fmt.Errorf("query group resonance memories: %w", err)
	}

	type candidate struct {
		ID       string
		UnitID   string
		Metadata memoryMetadata
	}
	candidates := make([]candidate, 0, 8)
	unitSet := map[string]struct{}{}
	for rows.Next() {
		var id string
		var unitID string
		var metadataJSON string
		if err := rows.Scan(&id, &unitID, &metadataJSON); err != nil {
			rows.Close()
			return fmt.Errorf("scan group resonance row: %w", err)
		}
		meta := decodeMemoryMetadata(metadataJSON)
		if meta.Turn != turn {
			continue
		}
		candidates = append(candidates, candidate{
			ID:       id,
			UnitID:   unitID,
			Metadata: meta,
		})
		unitSet[unitID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate group resonance rows: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close group resonance rows: %w", err)
	}
	if len(unitSet) < 3 {
		return nil
	}

	for _, candidate := range candidates {
		meta := candidate.Metadata
		if meta.GroupResonance {
			continue
		}
		meta.GroupResonance = true
		meta.Permanent = true
		if meta.BaseSalience <= 0 {
			meta.BaseSalience = 1
		}
		meta.BaseSalience *= 1.2

		metaJSON, err := json.Marshal(meta)
		if err != nil {
			return fmt.Errorf("marshal group resonance metadata: %w", err)
		}
		if _, err := service.db.ExecContext(
			ctx,
			`
			UPDATE memories
			SET
				metadata_json = ?,
				salience = salience * 1.2
			WHERE id = ?
			`,
			string(metaJSON),
			candidate.ID,
		); err != nil {
			return fmt.Errorf("update group resonance memory: %w", err)
		}
	}
	return nil
}

// ensureMemoryFTS 确保记忆全文检索表及索引已就绪。
func (service *Service) ensureMemoryFTS(ctx context.Context) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("memory fts unavailable: missing db")
	}
	service.memoryFTSOnce.Do(func() {
		query := `
			CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts
			USING fts5(memory_id UNINDEXED, unit_id UNINDEXED, summary)
			`
		if dbdialect.IsMySQL(service.db) {
			query = `
			CREATE TABLE IF NOT EXISTS memories_fts (
				memory_id VARCHAR(191) PRIMARY KEY,
				unit_id VARCHAR(191) NOT NULL,
				summary TEXT NOT NULL,
				INDEX idx_memories_fts_unit_id (unit_id),
				FULLTEXT INDEX idx_memories_fts_summary (summary)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
			`
		}
		_, service.memoryFTSErr = service.db.ExecContext(ctx, query)
	})
	return service.memoryFTSErr
}

// upsertMemoryFTS 向 FTS 索引插入或更新一条记忆文本。
func (service *Service) upsertMemoryFTS(ctx context.Context, memoryID string, unitID string, summary string) error {
	if strings.TrimSpace(memoryID) == "" {
		return nil
	}
	if _, err := service.db.ExecContext(
		ctx,
		`DELETE FROM memories_fts WHERE memory_id = ?`,
		memoryID,
	); err != nil {
		return err
	}
	if _, err := service.db.ExecContext(
		ctx,
		`INSERT INTO memories_fts (memory_id, unit_id, summary) VALUES (?, ?, ?)`,
		memoryID,
		unitID,
		summary,
	); err != nil {
		return err
	}
	return nil
}

// deleteMemoryFTS 从 FTS 索引移除指定记忆。
func (service *Service) deleteMemoryFTS(ctx context.Context, memoryID string) error {
	if strings.TrimSpace(memoryID) == "" {
		return nil
	}
	if _, err := service.db.ExecContext(ctx, `DELETE FROM memories_fts WHERE memory_id = ?`, memoryID); err != nil {
		return err
	}
	return nil
}

// buildMemoryFTSQuery 按闪回特征拼装 FTS 查询语句。
func buildMemoryFTSQuery(features flashbackFeatures) string {
	terms := make([]string, 0, 4)
	if strings.TrimSpace(features.Terrain) != "" {
		terms = append(terms, strings.TrimSpace(features.Terrain))
	}
	if strings.TrimSpace(features.Weather) != "" {
		terms = append(terms, strings.TrimSpace(features.Weather))
	}
	for _, name := range features.EnemyNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		terms = append(terms, name)
	}
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(terms))
	seen := map[string]struct{}{}
	for _, term := range terms {
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		quoted = append(quoted, fmt.Sprintf("%q", term))
	}
	return strings.Join(quoted, " OR ")
}

// searchMemoryFTS 在指定单位记忆空间内执行全文检索。
func (service *Service) searchMemoryFTS(ctx context.Context, unitID string, query string, limit int) ([]string, error) {
	if strings.TrimSpace(query) == "" || limit <= 0 {
		return nil, nil
	}
	if dbdialect.IsMySQL(service.db) {
		return service.searchMemoryFTSLike(ctx, unitID, query, limit)
	}
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT memory_id
		FROM memories_fts
		WHERE unit_id = ? AND memories_fts MATCH ?
		ORDER BY bm25(memories_fts)
		LIMIT ?
		`,
		unitID,
		query,
		limit,
	)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(ids) > 0 {
		return ids, nil
	}

	return service.searchMemoryFTSLike(ctx, unitID, query, limit)
}

func (service *Service) searchMemoryFTSLike(ctx context.Context, unitID string, query string, limit int) ([]string, error) {
	ids := make([]string, 0, limit)
	likeNeedle := normalizeFTSLikeNeedle(query)
	if likeNeedle == "" {
		return ids, nil
	}
	fallbackRows, err := service.db.QueryContext(
		ctx,
		`
		SELECT memory_id
		FROM memories_fts
		WHERE unit_id = ? AND summary LIKE ?
		LIMIT ?
		`,
		unitID,
		"%"+likeNeedle+"%",
		limit,
	)
	if err != nil {
		return ids, nil
	}
	defer fallbackRows.Close()
	for fallbackRows.Next() {
		var id string
		if err := fallbackRows.Scan(&id); err != nil {
			return ids, nil
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// normalizeFTSLikeNeedle 规范化检索词，减少符号与大小写噪音。
func normalizeFTSLikeNeedle(query string) string {
	needle := strings.TrimSpace(query)
	needle = strings.ReplaceAll(needle, "\"", "")
	needle = strings.ReplaceAll(needle, "OR", " ")
	needle = strings.ReplaceAll(needle, "or", " ")
	needle = strings.Join(strings.Fields(needle), " ")
	parts := strings.Split(needle, " ")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

// enforceMemoryCaps 按分类上限裁剪低价值记忆，保留永久记忆优先。
func (service *Service) enforceMemoryCaps(ctx context.Context, unitID string) error {
	for category, capValue := range memoryCategoryCaps {
		rows, err := service.db.QueryContext(
			ctx,
			`
			SELECT id, metadata_json
			FROM memories
			WHERE unit_id = ? AND category = ?
			ORDER BY salience DESC, created_at DESC
			`,
			unitID,
			category,
		)
		if err != nil {
			return fmt.Errorf("query memories for cap enforcement: %w", err)
		}

		type candidate struct {
			id        string
			permanent bool
		}
		items := make([]candidate, 0, capValue+64)
		for rows.Next() {
			var id string
			var metadataJSON string
			if err := rows.Scan(&id, &metadataJSON); err != nil {
				rows.Close()
				return fmt.Errorf("scan cap memory row: %w", err)
			}
			meta := decodeMemoryMetadata(metadataJSON)
			items = append(items, candidate{id: id, permanent: meta.Permanent})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("iterate cap memory rows: %w", err)
		}
		rows.Close()

		overflow := len(items) - capValue
		if overflow <= 0 {
			continue
		}
		for index := len(items) - 1; index >= 0 && overflow > 0; index-- {
			if items[index].permanent {
				continue
			}
			if _, err := service.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, items[index].id); err != nil {
				return fmt.Errorf("delete overflow memory: %w", err)
			}
			if service.ensureMemoryFTS(ctx) == nil {
				_ = service.deleteMemoryFTS(ctx, items[index].id)
			}
			overflow--
		}
	}
	return nil
}

// syncRecordHighlightsFromMemories 把高显著记忆同步回单位档案 highlights。
func (service *Service) syncRecordHighlightsFromMemories(ctx context.Context, record *unit.Record, limit int) error {
	if record == nil {
		return nil
	}
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
	if err != nil {
		return fmt.Errorf("query top memories for highlights: %w", err)
	}
	defer rows.Close()

	summaries := make([]string, 0, limit)
	for rows.Next() {
		var summary string
		if err := rows.Scan(&summary); err != nil {
			return fmt.Errorf("scan highlight memory: %w", err)
		}
		summaries = append(summaries, strings.TrimSpace(summary))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate highlight memories: %w", err)
	}
	if len(summaries) == 0 {
		return nil
	}

	// 将高显著记忆放到切片末尾，便于 RecentHighlights(limit) 直接取得关键条目。
	for left, right := 0, len(summaries)-1; left < right; left, right = left+1, right-1 {
		summaries[left], summaries[right] = summaries[right], summaries[left]
	}
	record.Memory.Highlights = summaries
	return nil
}

// loadTopMemories 按显著度与召回次数加载单位高价值记忆。
func (service *Service) loadTopMemories(ctx context.Context, unitID string, limit int) ([]memoryRow, error) {
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT id, unit_id, summary, category, emotion_weight, metadata_json
		FROM memories
		WHERE unit_id = ?
		ORDER BY salience DESC, recall_count DESC, created_at DESC
		LIMIT ?
		`,
		unitID,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query top memories: %w", err)
	}
	defer rows.Close()

	memories := make([]memoryRow, 0, limit)
	for rows.Next() {
		var row memoryRow
		var metadataJSON string
		if err := rows.Scan(&row.ID, &row.UnitID, &row.Summary, &row.Category, &row.EmotionWeight, &metadataJSON); err != nil {
			return nil, fmt.Errorf("scan top memory row: %w", err)
		}
		row.Metadata = decodeMemoryMetadata(metadataJSON)
		memories = append(memories, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate top memories: %w", err)
	}
	return memories, nil
}

func (service *Service) loadRecentMemoriesForPrompt(ctx context.Context, unitID string, startTurn int, currentTurn int) ([]memoryRow, error) {
	if startTurn <= 0 {
		startTurn = 1
	}
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT id, unit_id, summary, category, emotion_weight, metadata_json
		FROM memories
		WHERE unit_id = ? AND category != ?
		ORDER BY created_at DESC
		LIMIT 512
		`,
		unitID,
		memoryCategoryMemory2,
	)
	if err != nil {
		return nil, fmt.Errorf("query recent prompt memories: %w", err)
	}
	defer rows.Close()

	items := make([]memoryRow, 0, 32)
	for rows.Next() {
		var row memoryRow
		var metadataJSON string
		if err := rows.Scan(&row.ID, &row.UnitID, &row.Summary, &row.Category, &row.EmotionWeight, &metadataJSON); err != nil {
			return nil, fmt.Errorf("scan recent prompt memory: %w", err)
		}
		row.Metadata = decodeMemoryMetadata(metadataJSON)
		if row.Category == memoryCategoryKnowledge {
			continue
		}
		if row.Metadata.Turn > 0 && row.Metadata.Turn < startTurn {
			continue
		}
		if currentTurn > 0 && row.Metadata.Turn > currentTurn {
			continue
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent prompt memories: %w", err)
	}
	sort.SliceStable(items, func(left, right int) bool {
		if items[left].Metadata.Turn == items[right].Metadata.Turn {
			return items[left].Summary < items[right].Summary
		}
		return items[left].Metadata.Turn < items[right].Metadata.Turn
	})
	return items, nil
}

func (service *Service) loadMemory2Summaries(ctx context.Context, unitID string, limit int) ([]memoryRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT id, unit_id, summary, category, emotion_weight, metadata_json
		FROM memories
		WHERE unit_id = ? AND category = ?
		ORDER BY created_at ASC
		LIMIT ?
		`,
		unitID,
		memoryCategoryMemory2,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query memory2 summaries: %w", err)
	}
	defer rows.Close()

	memories := make([]memoryRow, 0, limit)
	for rows.Next() {
		var row memoryRow
		var metadataJSON string
		if err := rows.Scan(&row.ID, &row.UnitID, &row.Summary, &row.Category, &row.EmotionWeight, &metadataJSON); err != nil {
			return nil, fmt.Errorf("scan memory2 summary: %w", err)
		}
		row.Metadata = decodeMemoryMetadata(metadataJSON)
		memories = append(memories, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory2 summaries: %w", err)
	}
	return memories, nil
}

// loadMemoriesByIDs 按给定 ID 集合加载记忆并保持输入顺序。
func (service *Service) loadMemoriesByIDs(ctx context.Context, ids []string) ([]memoryRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ordered := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	if len(ordered) == 0 {
		return nil, nil
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ordered)), ",")
	query := fmt.Sprintf(
		`
		SELECT id, unit_id, summary, category, emotion_weight, metadata_json
		FROM memories
		WHERE id IN (%s)
		`,
		placeholders,
	)
	args := make([]any, 0, len(ordered))
	for _, id := range ordered {
		args = append(args, id)
	}
	queryRows, err := service.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories by ids: %w", err)
	}
	defer queryRows.Close()

	byID := map[string]memoryRow{}
	for queryRows.Next() {
		var row memoryRow
		var metadataJSON string
		if err := queryRows.Scan(&row.ID, &row.UnitID, &row.Summary, &row.Category, &row.EmotionWeight, &metadataJSON); err != nil {
			return nil, fmt.Errorf("scan memory row by ids: %w", err)
		}
		row.Metadata = decodeMemoryMetadata(metadataJSON)
		byID[row.ID] = row
	}
	if err := queryRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memories by ids: %w", err)
	}

	rows := make([]memoryRow, 0, len(ordered))
	for _, id := range ordered {
		if row, ok := byID[id]; ok {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// reinforceMemories 对被召回的记忆增加 recall_count 与 salience。
func (service *Service) reinforceMemories(ctx context.Context, memories []memoryRow) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range memories {
		if strings.TrimSpace(row.ID) == "" {
			continue
		}
		if _, err := service.db.ExecContext(
			ctx,
			`
			UPDATE memories
			SET
				recall_count = recall_count + 1,
				last_recalled_at = ?,
				salience = salience + ?
			WHERE id = ?
			`,
			now,
			memoryRecallBoost,
			row.ID,
		); err != nil {
			return fmt.Errorf("reinforce memory: %w", err)
		}
	}
	return nil
}

// reinforceFlashbackMemories 对“闪回命中”的记忆做额外强化，提升后续再召回概率。
func (service *Service) reinforceFlashbackMemories(ctx context.Context, memories []memoryRow) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, row := range memories {
		if strings.TrimSpace(row.ID) == "" {
			continue
		}
		if _, err := service.db.ExecContext(
			ctx,
			`
			UPDATE memories
			SET
				recall_count = recall_count + 1,
				last_recalled_at = ?,
				salience = salience + ?
			WHERE id = ?
			`,
			now,
			memoryFlashbackBoost,
			row.ID,
		); err != nil {
			return fmt.Errorf("reinforce flashback memory: %w", err)
		}
	}
	return nil
}

// decodeMemoryMetadata 解码记忆元数据并补齐安全默认值。
func decodeMemoryMetadata(raw string) memoryMetadata {
	meta := memoryMetadata{
		Turn:         1,
		Importance:   4,
		BaseSalience: 1,
		Permanent:    false,
	}
	if strings.TrimSpace(raw) == "" {
		return meta
	}
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return meta
	}
	if meta.Turn <= 0 {
		meta.Turn = 1
	}
	if meta.Importance <= 0 {
		meta.Importance = 4
	}
	if meta.BaseSalience <= 0 {
		meta.BaseSalience = 1
	}
	return meta
}

// computeMemorySalience 根据重要度、情绪强度和时间衰减计算显著度。
func computeMemorySalience(currentTurn int, meta memoryMetadata, emotionWeight float64) float64 {
	turn := meta.Turn
	if turn <= 0 {
		turn = 1
	}
	importance := clampMemoryImportance(meta.Importance)
	base := meta.BaseSalience
	if base <= 0 {
		base = 1
	}
	if emotionWeight <= 0 {
		emotionWeight = 1
	}

	if meta.Permanent {
		score := base * emotionWeight
		if score < 0.8 {
			score = 0.8
		}
		return score
	}

	elapsed := float64(currentTurn - turn)
	if elapsed < 0 {
		elapsed = 0
	}
	denominator := memoryDecayTauTurns * math.Pow(float64(importance), memoryDecayAlpha)
	if denominator <= 0 {
		denominator = memoryDecayTauTurns
	}

	score := base * emotionWeight * math.Exp(-elapsed/denominator)
	if score < 0.01 {
		score = 0.01
	}
	return score
}

// inferMemoryCategory 依据文本语义自动归类，尽量减少人工规则配置成本。
func inferMemoryCategory(summary string, source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	if strings.Contains(source, "knowledge") || strings.Contains(source, "world_rule") {
		return memoryCategoryKnowledge
	}
	text := strings.ToLower(strings.TrimSpace(summary))
	switch {
	case containsAny(text, "规律", "法则", "我摸清了", "我发现", "会让", "会使", "更容易", "更难", "加成", "减益", "增伤", "减伤", "射程", "移速", "行动消耗", "天气", "地形"):
		return memoryCategoryKnowledge
	case containsAny(text, "队友", "同伴", "挚友", "朋友", "交易", "交换", "援助", "背叛", "信任", "营长", "玩家"):
		return memoryCategoryRelation
	case containsAny(text, "山", "林", "河", "草原", "沙漠", "沼泽", "村庄", "城市", "雪原", "道路", "坐标", "地形", "前线", "阵地"):
		return memoryCategorySpatial
	case containsAny(text, "敌军", "斥候", "前锋", "队长", "旗手", "铁匠", "医者", "弓手"):
		return memoryCategoryEntity
	default:
		return memoryCategoryEvent
	}
}

// inferMemoryEmotionWeight 从文本语义推断记忆情绪权重。
func inferMemoryEmotionWeight(summary string) float64 {
	text := strings.ToLower(strings.TrimSpace(summary))
	switch {
	case containsAny(text, "阵亡", "濒死", "崩溃", "背叛", "惨败", "绝望", "重创", "秒倒", "断粮"):
		return 2.0
	case containsAny(text, "愤怒", "暴怒", "恐惧", "害怕", "欢呼", "庆幸", "激动", "紧张", "惊慌"):
		return 1.5
	default:
		return 1.0
	}
}

// inferMemoryImportance 从文本与分类推断记忆重要度。
func inferMemoryImportance(summary string, category string) int {
	text := strings.ToLower(strings.TrimSpace(summary))
	importance := 4

	switch category {
	case memoryCategoryRelation:
		importance = 5
	case memoryCategorySpatial:
		importance = 4
	case memoryCategoryEntity:
		importance = 5
	case memoryCategoryKnowledge:
		importance = 6
	}

	if containsAny(text, "阵亡", "濒死", "背叛", "救命", "击倒", "秒倒", "崩溃") {
		importance = 8
	}
	if containsAny(text, "全军", "覆灭", "绝境") {
		importance = 9
	}
	if containsAny(text, "闲聊", "路过", "看看", "一般", "普通") && importance > 3 {
		importance = 3
	}

	return clampMemoryImportance(importance)
}

// clampMemoryImportance 把重要度限制在可接受区间。
func clampMemoryImportance(value int) int {
	if value < 1 {
		return 1
	}
	if value > 10 {
		return 10
	}
	return value
}

// loadMemoryCountByCategory 统计单位在指定分类下的记忆条数。
func loadMemoryCountByCategory(ctx context.Context, db *sql.DB, unitID string, category string) (int, error) {
	var count int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(1) FROM memories WHERE unit_id = ? AND category = ?`,
		unitID,
		category,
	).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
