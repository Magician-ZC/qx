package session

// 文件说明：实现单位关系图谱读写与增量更新，支持关系摘要、分数夹紧与审计备注记录。

import (
	"context"
	"database/sql"
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

// applyRelationShiftTx 是 applyRelationShift 的**事务版**：在调用方传入的 *sql.Tx 内做同一份四轴关系 UPSERT，
// 让「关系效果」与外层状态翻转（如 consent accept 的 status=accepted）原子提交——任一步失败整体回滚、可重试。
// 与 applyRelationShift 共享同一套 SQL/clamp(±10)/dbdialect 双分支语义；仅用 source.ID/target.ID + 四轴 delta + note，
// 不读单位其它字段。**best-effort 跨分片安全**：relations 表对 source/target 都有 units FK，故先在同一事务内
// `SELECT 1 FROM units WHERE id=?` 判存在，任一不在本库（跨分片/远端角色）→ 跳过关系写、返回 (false,nil)（不报错），
// 对齐 applySevenEffect 的 best-effort 语义；仅真实 DB 写失败才返错。dialect 仍以 service.db 判定（registry 按 *sql.DB 注册），
// 实际写落在 tx 上。
func (service *Service) applyRelationShiftTx(
	ctx context.Context,
	tx *sql.Tx,
	sourceID string,
	targetID string,
	delta relationDelta,
	reason string,
) (bool, error) {
	if service == nil || service.db == nil || tx == nil || sourceID == "" || targetID == "" || sourceID == targetID {
		return false, nil
	}
	if delta.isZero() {
		return false, nil
	}

	// 跨分片/远端角色 best-effort 跳过：relations 对 source/target 都有 units FK，任一不存在则不写关系（不报错）。
	if exists, err := unitExistsTx(ctx, tx, sourceID); err != nil {
		return false, err
	} else if !exists {
		return false, nil
	}
	if exists, err := unitExistsTx(ctx, tx, targetID); err != nil {
		return false, err
	} else if !exists {
		return false, nil
	}

	delta.Trust = clampRelationScore(delta.Trust)
	delta.Fear = clampRelationScore(delta.Fear)
	delta.Affection = clampRelationScore(delta.Affection)
	delta.Rivalry = clampRelationScore(delta.Rivalry)

	note := relationNote{
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
	if _, err := tx.ExecContext(
		ctx,
		query,
		sourceID,
		targetID,
		delta.Trust,
		delta.Fear,
		delta.Affection,
		delta.Rivalry,
		string(noteJSON),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return false, fmt.Errorf("upsert relation %s -> %s: %w", sourceID, targetID, err)
	}

	return true, nil
}

// unitExistsTx 在给定事务内判断某单位是否存在于本库（用于关系写前的 FK best-effort 跳过判断）。
func unitExistsTx(ctx context.Context, tx *sql.Tx, unitID string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM units WHERE id = ?`, unitID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check unit %s exists: %w", unitID, err)
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

// RelationView 是某角色对外四轴关系的公共视图（命运客户端「她身边的人」关系面板用）。
type RelationView struct {
	TargetUnitID string  `json:"target_unit_id"`
	TargetName   string  `json:"target_name"`
	Trust        float64 `json:"trust"`
	Fear         float64 `json:"fear"`
	Affection    float64 `json:"affection"`
	Rivalry      float64 `json:"rivalry"`
}

// ListRelations 列出某角色最强的对外四轴关系（按四轴绝对值和排序，**不带敌意过滤**——与只出世仇的 ListBloodFeuds 不同，
// 这是「她身边在乎/在意/提防的人」的全谱）。命运客户端关系面板用。
func (service *Service) ListRelations(ctx context.Context, unitID string, limit int) ([]RelationView, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("list relations: nil service or db")
	}
	rows := service.loadTopOutgoingRelations(ctx, unitID, limit)
	out := make([]RelationView, 0, len(rows))
	for _, r := range rows {
		out = append(out, RelationView{
			TargetUnitID: r.TargetUnitID,
			TargetName:   r.TargetName,
			Trust:        r.Trust,
			Fear:         r.Fear,
			Affection:    r.Affection,
			Rivalry:      r.Rivalry,
		})
	}
	return out, nil
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
