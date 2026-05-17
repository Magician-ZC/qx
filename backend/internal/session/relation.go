package session

// 文件说明：实现单位关系图谱读写与增量更新，支持关系摘要、分数夹紧与审计备注记录。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	relationScoreMin       = -10.0
	relationScoreMax       = 10.0
	relationSummaryNoKnown = "我暂时还没和谁建立明确关系。"
)

// relationDelta 结构体用于承载该模块的核心数据。
type relationDelta struct {
	Trust     float64
	Fear      float64
	Affection float64
	Rivalry   float64
}

// isZero 判断关系变化量是否全部为 0。
func (delta relationDelta) isZero() bool {
	return delta.Trust == 0 && delta.Fear == 0 && delta.Affection == 0 && delta.Rivalry == 0
}

// relationPromptRow 结构体用于承载该模块的核心数据。
type relationPromptRow struct {
	TargetUnitID string
	TargetName   string
	Trust        float64
	Fear         float64
	Affection    float64
	Rivalry      float64
}

// relationNote 结构体用于承载该模块的核心数据。
type relationNote struct {
	Turn           int     `json:"turn,omitempty"`
	Phase          string  `json:"phase,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	TrustDelta     float64 `json:"trust_delta,omitempty"`
	FearDelta      float64 `json:"fear_delta,omitempty"`
	AffectionDelta float64 `json:"affection_delta,omitempty"`
	RivalryDelta   float64 `json:"rivalry_delta,omitempty"`
}

// applyRelationShift 对单向关系写入一次增量变化（source -> target）。
// 结果会被夹紧在 [-10,10] 区间，并记录带回合信息的 notes_json 供审计。
func (service *Service) applyRelationShift(
	ctx context.Context,
	state *State,
	source *unit.Record,
	target *unit.Record,
	delta relationDelta,
	reason string,
) (bool, error) {
	if service == nil || service.db == nil || source == nil || target == nil || source.ID == "" || target.ID == "" || source.ID == target.ID {
		return false, nil
	}
	if delta.isZero() {
		return false, nil
	}

	delta.Trust = clampRelationScore(delta.Trust)
	delta.Fear = clampRelationScore(delta.Fear)
	delta.Affection = clampRelationScore(delta.Affection)
	delta.Rivalry = clampRelationScore(delta.Rivalry)

	turn := 0
	phase := ""
	if state != nil {
		turn = state.TurnState.Turn
		phase = string(state.TurnState.Phase)
	}
	note := relationNote{
		Turn:           turn,
		Phase:          phase,
		Reason:         limitTextRunes(strings.TrimSpace(reason), 80),
		TrustDelta:     delta.Trust,
		FearDelta:      delta.Fear,
		AffectionDelta: delta.Affection,
		RivalryDelta:   delta.Rivalry,
	}
	noteJSON, err := json.Marshal(note)
	if err != nil {
		return false, fmt.Errorf("marshal relation note: %w", err)
	}

	query := `
		INSERT INTO relations (
			source_unit_id,
			target_unit_id,
			trust,
			fear,
			affection,
			rivalry,
			notes_json,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source_unit_id, target_unit_id) DO UPDATE SET
			trust = MIN(10.0, MAX(-10.0, relations.trust + excluded.trust)),
			fear = MIN(10.0, MAX(-10.0, relations.fear + excluded.fear)),
			affection = MIN(10.0, MAX(-10.0, relations.affection + excluded.affection)),
			rivalry = MIN(10.0, MAX(-10.0, relations.rivalry + excluded.rivalry)),
			notes_json = excluded.notes_json,
			updated_at = excluded.updated_at
		`
	if dbdialect.IsMySQL(service.db) {
		query = `
		INSERT INTO relations (
			source_unit_id,
			target_unit_id,
			trust,
			fear,
			affection,
			rivalry,
			notes_json,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			trust = LEAST(10.0, GREATEST(-10.0, relations.trust + VALUES(trust))),
			fear = LEAST(10.0, GREATEST(-10.0, relations.fear + VALUES(fear))),
			affection = LEAST(10.0, GREATEST(-10.0, relations.affection + VALUES(affection))),
			rivalry = LEAST(10.0, GREATEST(-10.0, relations.rivalry + VALUES(rivalry))),
			notes_json = VALUES(notes_json),
			updated_at = VALUES(updated_at)
		`
	}
	if _, err := service.db.ExecContext(
		ctx,
		query,
		source.ID,
		target.ID,
		delta.Trust,
		delta.Fear,
		delta.Affection,
		delta.Rivalry,
		string(noteJSON),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return false, fmt.Errorf("upsert relation %s -> %s: %w", source.ID, target.ID, err)
	}

	return true, nil
}

// applyMutualRelationShiftBestEffort 同时更新双方关系，任一方向失败都不会中断主流程。
// 若至少一侧成功，会追加一条关系变化日志。
func (service *Service) applyMutualRelationShiftBestEffort(
	ctx context.Context,
	state *State,
	left *unit.Record,
	right *unit.Record,
	leftDelta relationDelta,
	rightDelta relationDelta,
	reason string,
) {
	if service == nil || left == nil || right == nil || left.ID == right.ID {
		return
	}

	changed := false
	if applied, err := service.applyRelationShift(ctx, state, left, right, leftDelta, reason); err != nil {
		if state != nil {
			appendLog(state, "relation_error", fmt.Sprintf("我和 %s 这回合关系没记上。", right.DisplayName()), left.ID, right.ID)
		}
	} else if applied {
		changed = true
	}
	if applied, err := service.applyRelationShift(ctx, state, right, left, rightDelta, reason); err != nil {
		if state != nil {
			appendLog(state, "relation_error", fmt.Sprintf("我和 %s 这回合关系没记上。", left.DisplayName()), right.ID, left.ID)
		}
	} else if applied {
		changed = true
	}

	if !changed || state == nil {
		return
	}
	trimmedReason := strings.TrimSpace(reason)
	if trimmedReason == "" {
		trimmedReason = "发生了新的互动"
	}
	appendLog(
		state,
		"relation_shift",
		fmt.Sprintf("%s 与 %s 的关系变化：%s。", left.DisplayName(), right.DisplayName(), trimmedReason),
		left.ID,
		right.ID,
	)
}

// loadTopOutgoingRelations 按“关系强度”排序读取某单位最重要的对外关系。
func (service *Service) loadTopOutgoingRelations(
	ctx context.Context,
	sourceUnitID string,
	limit int,
) []relationPromptRow {
	if service == nil || service.db == nil || strings.TrimSpace(sourceUnitID) == "" {
		return nil
	}
	if limit <= 0 {
		limit = 6
	}

	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT
			r.target_unit_id,
			COALESCE(u.display_name, ''),
			r.trust,
			r.fear,
			r.affection,
			r.rivalry
		FROM relations r
		LEFT JOIN units u ON u.id = r.target_unit_id
		WHERE r.source_unit_id = ?
		ORDER BY (ABS(r.trust) + ABS(r.fear) + ABS(r.affection) + ABS(r.rivalry)) DESC, r.updated_at DESC
		LIMIT ?
		`,
		sourceUnitID,
		limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()

	result := make([]relationPromptRow, 0, limit)
	for rows.Next() {
		var row relationPromptRow
		if scanErr := rows.Scan(
			&row.TargetUnitID,
			&row.TargetName,
			&row.Trust,
			&row.Fear,
			&row.Affection,
			&row.Rivalry,
		); scanErr != nil {
			continue
		}
		result = append(result, row)
	}
	if rows.Err() != nil {
		return nil
	}
	return result
}

// loadOutgoingRelationMap 把对外关系列表重排为按 targetID 索引的 map，便于快速查询。
func (service *Service) loadOutgoingRelationMap(ctx context.Context, sourceUnitID string) map[string]relationPromptRow {
	rows := service.loadTopOutgoingRelations(ctx, sourceUnitID, 64)
	byTarget := make(map[string]relationPromptRow, len(rows))
	for _, row := range rows {
		byTarget[row.TargetUnitID] = row
	}
	return byTarget
}

// relationSummaryForPrompt 生成可注入提示词的人际关系摘要文本。
// 会过滤“陌生”关系，优先输出更强关系。
func (service *Service) relationSummaryForPrompt(
	ctx context.Context,
	byID map[string]*unit.Record,
	actor unit.Record,
	limit int,
) string {
	if limit <= 0 {
		limit = 4
	}
	rows := service.loadTopOutgoingRelations(ctx, actor.ID, limit*4)
	if len(rows) == 0 {
		return relationSummaryNoKnown
	}

	lines := make([]string, 0, limit)
	for _, row := range rows {
		tier := relationTierFromScores(row.Trust, row.Affection, row.Rivalry, row.Fear)
		if tier == "陌生" {
			continue
		}

		targetName := strings.TrimSpace(row.TargetName)
		if byID != nil {
			if target, ok := byID[row.TargetUnitID]; ok && target != nil {
				targetName = target.DisplayName()
			}
		}
		if targetName == "" {
			targetName = row.TargetUnitID
		}
		lines = append(
			lines,
			fmt.Sprintf(
				"%s：%s(信任%.1f 好感%.1f 竞争%.1f 戒备%.1f)",
				targetName,
				tier,
				row.Trust,
				row.Affection,
				row.Rivalry,
				row.Fear,
			),
		)
		if len(lines) >= limit {
			break
		}
	}
	if len(lines) == 0 {
		return relationSummaryNoKnown
	}
	return strings.Join(lines, "\n")
}

// relationTier 返回 source 对 target 的关系层级；无记录时返回空字符串。
func (service *Service) relationTier(ctx context.Context, sourceUnitID string, targetUnitID string) string {
	if service == nil || service.db == nil || sourceUnitID == "" || targetUnitID == "" || sourceUnitID == targetUnitID {
		return ""
	}

	var trust float64
	var fear float64
	var affection float64
	var rivalry float64
	err := service.db.QueryRowContext(
		ctx,
		`
		SELECT trust, fear, affection, rivalry
		FROM relations
		WHERE source_unit_id = ? AND target_unit_id = ?
		`,
		sourceUnitID,
		targetUnitID,
	).Scan(&trust, &fear, &affection, &rivalry)
	if err != nil {
		return ""
	}
	return relationTierFromScores(trust, affection, rivalry, fear)
}

// hasBondRelation 判断 source 对 target 是否达到“羁绊”关系层级。
func (service *Service) hasBondRelation(ctx context.Context, sourceUnitID string, targetUnitID string) bool {
	return service.relationTier(ctx, sourceUnitID, targetUnitID) == "羁绊"
}

// hasFamiliarRelation 判断 source 对 target 是否至少达到“熟人”层级。
func (service *Service) hasFamiliarRelation(ctx context.Context, sourceUnitID string, targetUnitID string) bool {
	tier := service.relationTier(ctx, sourceUnitID, targetUnitID)
	return tier == "熟人" || tier == "羁绊"
}

// relationTierFromScores 根据信任/好感/竞争/戒备分数计算关系层级标签。
func relationTierFromScores(trust float64, affection float64, rivalry float64, fear float64) string {
	closeness := trust + 0.9*affection - 0.6*rivalry - 0.5*fear
	hostility := 1.1*rivalry + fear - 0.5*trust - 0.4*affection

	switch {
	case closeness >= 6.2 && trust >= 3.5 && affection >= 2.8:
		return "羁绊"
	case hostility >= 5.8 || rivalry >= 5 || fear >= 5 || trust <= -3.8:
		return "敌视"
	case relationIntensity(trust, fear, affection, rivalry) >= 1.2:
		return "熟人"
	default:
		return "陌生"
	}
}

// relationAffinityFromScores 把多维关系分数压缩为单一亲和度值，便于参与决策打分。
func relationAffinityFromScores(trust float64, affection float64, rivalry float64, fear float64) float64 {
	affinity := trust*0.11 + affection*0.08 - rivalry*0.1 - fear*0.07
	return clampFloat(affinity, -0.9, 1.1)
}

// relationIntensity 计算关系强度（四个维度绝对值总和）。
func relationIntensity(trust float64, fear float64, affection float64, rivalry float64) float64 {
	return math.Abs(trust) + math.Abs(fear) + math.Abs(affection) + math.Abs(rivalry)
}

// clampRelationScore 把关系分数限制到系统支持区间。
func clampRelationScore(value float64) float64 {
	return clampFloat(value, relationScoreMin, relationScoreMax)
}

// clampFloat 通用浮点夹紧工具。
func clampFloat(value float64, low float64, high float64) float64 {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}
