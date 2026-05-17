package session

// 文件说明：管理“世界规律”知识记忆的归档、去重与 prompt 注入，并解释属性受环境影响的原因。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const knowledgeMemoryPrefix = "规律："

// rememberWorldKnowledgeBestEffort 将单位在本步总结的“世界规律”写入记忆层。
func (service *Service) rememberWorldKnowledgeBestEffort(
	ctx context.Context,
	state State,
	actor *unit.Record,
	decision unitDecisionPayload,
) (string, bool) {
	if service == nil || actor == nil {
		return "", false
	}
	if !isBattleReady(*actor) {
		return "", false
	}
	knowledge := strings.TrimSpace(decision.Knowledge)
	if knowledge == "" {
		return "", false
	}
	return service.rememberKnowledgeLineBestEffort(ctx, actor, state.TurnState.Turn, knowledge)
}

// rememberKnowledgeLineBestEffort 去重后写入知识记忆，避免同一句规则反复刷屏。
func (service *Service) rememberKnowledgeLineBestEffort(ctx context.Context, actor *unit.Record, turn int, summary string) (string, bool) {
	if service == nil || actor == nil {
		return "", false
	}
	summary = normalizeKnowledgeSummary(summary)
	if summary == "" {
		return "", false
	}
	known, err := service.hasKnowledgeMemory(ctx, actor.ID, summary)
	if err == nil && known {
		return summary, false
	}
	if err := service.rememberUnitWithSource(ctx, actor, turn, summary, "knowledge_world_rule", 1); err != nil {
		_ = err
		return summary, false
	}
	return summary, true
}

// hasKnowledgeMemory 判断单位是否已记录同一条知识记忆。
func (service *Service) hasKnowledgeMemory(ctx context.Context, unitID string, summary string) (bool, error) {
	if service == nil || service.db == nil {
		return false, nil
	}
	unitID = strings.TrimSpace(unitID)
	summary = strings.TrimSpace(summary)
	if unitID == "" || summary == "" {
		return false, nil
	}
	var exists int
	if err := service.db.QueryRowContext(
		ctx,
		`SELECT 1 FROM memories WHERE unit_id = ? AND category = ? AND summary = ? LIMIT 1`,
		unitID,
		memoryCategoryKnowledge,
		summary,
	).Scan(&exists); err != nil {
		if err == context.Canceled || err == context.DeadlineExceeded {
			return false, err
		}
		if strings.Contains(strings.ToLower(err.Error()), "no rows") {
			return false, nil
		}
		return false, nil
	}
	return exists == 1, nil
}

// knowledgeSummaryForPrompt 读取单位已掌握的高价值世界规律摘要。
func (service *Service) knowledgeSummaryForPrompt(ctx context.Context, unitID string, limit int) string {
	if service == nil || service.db == nil {
		return "无"
	}
	unitID = strings.TrimSpace(unitID)
	if unitID == "" {
		return "无"
	}
	if limit <= 0 {
		limit = 4
	}
	rows, err := service.db.QueryContext(
		ctx,
		`
		SELECT summary
		FROM memories
		WHERE unit_id = ? AND category = ?
		ORDER BY salience DESC, recall_count DESC, created_at DESC
		LIMIT ?
		`,
		unitID,
		memoryCategoryKnowledge,
		limit,
	)
	if err != nil {
		return "无"
	}
	defer rows.Close()

	lines := make([]string, 0, limit)
	seen := map[string]struct{}{}
	for rows.Next() {
		var summary string
		if err := rows.Scan(&summary); err != nil {
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
		lines = append(lines, summary)
	}
	if len(lines) == 0 {
		return "无"
	}
	return strings.Join(lines, "\n")
}

// summarizeCurrentAttributeInfluences 用自然语言解释“当前属性为何被天气/地形/设施影响”。
func summarizeCurrentAttributeInfluences(state State, actor unit.Record) string {
	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrainID := terrainAt(state.Map, coord)
	terrainName := terrainDisplayName(terrainID)
	weatherName := strings.TrimSpace(state.Weather.DisplayName)
	if weatherName == "" {
		weatherName = "当前天气"
	}
	lines := make([]string, 0, 8)

	baseReach := attackReach(state.Map, state.Structures, actor)
	reach := attackReachWithWeather(state, actor)
	if reach != baseReach {
		lines = append(lines, fmt.Sprintf("攻击距离受影响：基础 %d 格，在%s下变为 %d 格。", baseReach, weatherName, reach))
	} else {
		lines = append(lines, fmt.Sprintf("攻击距离：当前 %d 格（天气未改变基础射程）。", reach))
	}

	moveMultiplier := weatherAdjustedMoveMultiplier(state, actor)
	if moveMultiplier != 1 {
		lines = append(lines, fmt.Sprintf("机动效率受影响：在%s + %s下，移动效率为 %.0f%%。", weatherName, terrainName, moveMultiplier*100))
	} else {
		lines = append(lines, fmt.Sprintf("机动效率：在%s + %s下无额外修正。", weatherName, terrainName))
	}

	hungerPenalty := weatherActionHungerPenalty(state, actor)
	if hungerPenalty > 0 {
		lines = append(lines, fmt.Sprintf("行动饥饿消耗受影响：%s + %s 会额外 +%d。", weatherName, terrainName, hungerPenalty))
	} else {
		lines = append(lines, "行动饥饿消耗：天气与地形当前没有额外惩罚。")
	}
	if actor.Status.Hunger < 30 {
		lines = append(lines, "你当前处于低饥饿阈值(<30)：行动效率会下降（移速与攻击乘区 0.8）。")
	}

	defenseBonus := terrainDefenseBonus(state.Map, actor, actor)
	if defenseBonus > 0 {
		lines = append(lines, fmt.Sprintf("防御受地形影响：站在%s会得到防御修正 +%d。", terrainName, defenseBonus))
	}
	if defenseBonus < 0 {
		lines = append(lines, fmt.Sprintf("防御受地形影响：站在%s会得到防御修正 %d。", terrainName, defenseBonus))
	}

	if structure := structureAt(state.Structures, coord); structure != nil &&
		structureReady(*structure) &&
		structure.FactionID == actor.FactionID {
		switch structure.Type {
		case StructureTypeForge:
			lines = append(lines, "设施影响：你站在己方铁匠铺上，近战与防御都会得到额外修正（ATK+4 / DEF+3）。")
		case StructureTypeTurret:
			lines = append(lines, "设施影响：你站在己方炮台上，攻击修正 +8，且攻击距离至少 3 格。")
		case StructureTypeWatchtower:
			lines = append(lines, "设施影响：你站在己方瞭望塔上，攻击修正 +2，且攻击距离至少 2 格。")
		}
	}

	switch state.Weather.Type {
	case WeatherWindy:
		lines = append(lines, "天气影响补充：大风会降低远距攻击稳定性。")
	case WeatherRainy:
		lines = append(lines, "天气影响补充：阴雨会降低远距攻击稳定性，并使行动更耗体力。")
	case WeatherFoggy:
		lines = append(lines, "天气影响补充：浓雾会压缩攻击射程，远距更不稳定。")
	}

	return strings.Join(lines, "\n")
}

// normalizeKnowledgeSummary 规范化知识文本并统一前缀格式。
func normalizeKnowledgeSummary(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ""
	}
	summary = strings.TrimPrefix(summary, knowledgeMemoryPrefix)
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ""
	}
	return knowledgeMemoryPrefix + summary
}
