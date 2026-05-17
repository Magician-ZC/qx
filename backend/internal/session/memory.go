package session

// 文件说明：memory.go，单位记忆写入与提示词摘要工具，负责高亮回退、回合标签与环境记忆拼装。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// rememberUnit 以默认来源写入单位记忆摘要。
func (service *Service) rememberUnit(ctx context.Context, record *unit.Record, turn int, summary string) error {
	return service.rememberUnitWithSource(ctx, record, turn, summary, "unit_self", 0)
}

// rememberUnitWithSource 写入结构化记忆，并同步单位档案里的 highlights 兜底。
func (service *Service) rememberUnitWithSource(
	ctx context.Context,
	record *unit.Record,
	turn int,
	summary string,
	source string,
	importanceBoost int,
) error {
	if record == nil {
		return nil
	}

	summary = strings.TrimSpace(summary)
	if summary == "" {
		return nil
	}

	if err := service.storeMemoryAndSyncHighlights(ctx, record, turn, summary, source, importanceBoost); err != nil {
		// 结构化记忆写入失败时，退回到单位档案轻量记忆，保证 AI 仍有上下文。
		unit.Remember(record, fmt.Sprintf("T%d %s", turn, summary))
	}
	if len(record.Memory.Highlights) == 0 {
		unit.Remember(record, fmt.Sprintf("T%d %s", turn, summary))
	}
	return service.units.Save(ctx, *record)
}

// rememberUnitBestEffort 以容错方式写入记忆，失败不打断主流程。
func (service *Service) rememberUnitBestEffort(ctx context.Context, record *unit.Record, turn int, summary string) {
	if err := service.rememberUnit(ctx, record, turn, summary); err != nil {
		_ = err
	}
}

// summarizeUnitMemory 汇总单位最近记忆高亮文本。
func summarizeUnitMemory(record unit.Record, limit int) string {
	highlights := unit.RecentHighlights(record, limit)
	if len(highlights) == 0 {
		return "无"
	}
	return strings.Join(highlights, "\n")
}

// summarizeUnitMemoryWithTurn 汇总记忆并附相对回合时间标签。
func summarizeUnitMemoryWithTurn(record unit.Record, currentTurn int, limit int) string {
	highlights := unit.RecentHighlights(record, limit)
	if len(highlights) == 0 {
		return "无"
	}
	lines := make([]string, 0, len(highlights))
	for _, line := range highlights {
		lines = append(lines, formatMemoryHighlightWithTurn(line, currentTurn))
	}
	return strings.Join(lines, "\n")
}

// formatMemoryHighlightWithTurn 将 "Tn 文本" 规范化为 "文本（N回合前）" 供提示词使用。
func formatMemoryHighlightWithTurn(line string, currentTurn int) string {
	text := strings.TrimSpace(line)
	if text == "" {
		return ""
	}
	parts := strings.SplitN(text, " ", 2)
	if len(parts) < 2 {
		return text
	}
	turnPart := strings.TrimSpace(parts[0])
	content := strings.TrimSpace(parts[1])
	if content == "" || !strings.HasPrefix(strings.ToUpper(turnPart), "T") {
		return text
	}
	eventTurn, err := parseTurnNumber(turnPart[1:])
	if err != nil {
		return text
	}
	return fmt.Sprintf("%s（%s）", content, relativeTurnLabel(currentTurn, eventTurn))
}

// parseTurnNumber 解析 highlight 前缀里的回合号，格式必须是正整数。
func parseTurnNumber(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, fmt.Errorf("empty turn")
	}
	value := 0
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("invalid turn")
		}
		value = value*10 + int(char-'0')
	}
	if value <= 0 {
		return 0, fmt.Errorf("invalid turn")
	}
	return value, nil
}

// memoryContainsAny 判断最近记忆是否包含任一关键词。
func memoryContainsAny(record unit.Record, candidates ...string) bool {
	for _, highlight := range unit.RecentHighlights(record, 6) {
		text := strings.ToLower(highlight)
		if containsAny(text, candidates...) {
			return true
		}
	}
	return false
}

// summarizeActorPersonality 汇总单位核心人格参数，供提示词使用。
func summarizeActorPersonality(record unit.Record) string {
	return fmt.Sprintf(
		"勇敢=%.2f 谨慎=%.2f 激进=%.2f 忠诚=%.2f 稳定=%.2f 社交=%.2f",
		record.Personality.Courage,
		record.Personality.Prudence,
		record.Personality.Aggression,
		record.Personality.Loyalty,
		record.Personality.Stability,
		record.Personality.Sociability,
	)
}

// summarizeImmediateEnvironment 汇总单位周边即时态势，作为决策提示词环境输入。
func summarizeImmediateEnvironment(state State, byID map[string]*unit.Record, actor *unit.Record) string {
	if actor == nil {
		return "无"
	}

	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrain := terrainDisplayName(terrainAt(state.Map, coord))
	structure := summarizeStructureAt(state.Structures, coord)

	allies := make([]string, 0, 3)
	for _, alliedID := range alliedIDs(state, actor.FactionID) {
		if alliedID == actor.ID {
			continue
		}
		ally := byID[alliedID]
		if ally == nil || !isBattleReady(*ally) {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) <= 1 {
			allies = append(allies, ally.DisplayName())
		}
	}
	if len(allies) == 0 {
		allies = append(allies, "无贴身友军")
	}

	threat := nearestBattleReady(visibleOpposingIDs(state, byID, actor), byID, actor)
	threatSummary := "附近无敌军"
	if threat != nil {
		threatSummary = fmt.Sprintf(
			"最近敌军=%s 距离=%d",
			threat.DisplayName(),
			unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, threat.Status.PositionQ, threat.Status.PositionR),
		)
	}

	return fmt.Sprintf(
		"天气=%s 脚下=%s 设施=%s 饥饿=%d HP=%d 贴身友军=%s %s 近期伤亡=%s",
		state.Weather.DisplayName,
		terrain,
		structure,
		actor.Status.Hunger,
		actor.Status.HP,
		strings.Join(allies, "、"),
		threatSummary,
		summarizeCasualtiesForPrompt(state, byID, actor, 3),
	)
}

// summarizeCasualtiesForPrompt 汇总单位能感知到的阵亡/倒下单位，显式放入 LLM 上下文。
func summarizeCasualtiesForPrompt(state State, byID map[string]*unit.Record, actor *unit.Record, limit int) string {
	if actor == nil || len(byID) == 0 || limit <= 0 {
		return "无"
	}
	orderedIDs := append([]string{}, state.PlayerUnitIDs...)
	orderedIDs = append(orderedIDs, state.EnemyUnitIDs...)
	orderedIDs = append(orderedIDs, state.WildUnitIDs...)
	seen := make(map[string]bool, len(orderedIDs))
	lines := make([]string, 0, limit)
	add := func(unitID string) {
		if len(lines) >= limit || seen[unitID] {
			return
		}
		seen[unitID] = true
		record := byID[unitID]
		if record == nil || record.ID == actor.ID || record.Status.HP > 0 && record.Status.LifeState == unit.LifeStateActive {
			return
		}
		if !casualtyVisibleToActor(state, actor, record) {
			return
		}
		factionLabel := "其他阵营"
		if record.FactionID == actor.FactionID {
			factionLabel = "友军"
		} else if isPlayerEnemyFactionPair(state, actor.FactionID, record.FactionID) {
			factionLabel = "敌方"
		}
		statusLabel := "倒下"
		if record.Status.LifeState == unit.LifeStateDead {
			statusLabel = "死亡"
		}
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, record.Status.PositionQ, record.Status.PositionR)
		cause := summarizeCasualtyCause(state, byID, record)
		lines = append(lines, fmt.Sprintf("%s%s在%d,%d%s（距你%d格，死因：%s）", factionLabel, record.DisplayName(), record.Status.PositionQ, record.Status.PositionR, statusLabel, distance, cause))
	}
	for _, unitID := range orderedIDs {
		add(unitID)
	}
	for unitID := range byID {
		add(unitID)
	}
	if len(lines) == 0 {
		return "无"
	}
	return strings.Join(lines, "；")
}

// summarizeCasualtyCause 从最近日志和原始事件流中提取单位倒下/死亡原因，供 LLM 判断风险来源。
func summarizeCasualtyCause(state State, byID map[string]*unit.Record, record *unit.Record) string {
	if record == nil {
		return "不明"
	}
	unitID := record.ID
	for index := len(state.Logs) - 1; index >= 0; index-- {
		entry := state.Logs[index]
		if entry.ActorUnitID != unitID && entry.TargetUnitID != unitID {
			continue
		}
		if cause := casualtyCauseFromLog(entry, byID, unitID); cause != "" {
			return cause
		}
	}
	for index := len(state.RawEventLog) - 1; index >= 0; index-- {
		entry := state.RawEventLog[index]
		if entry.ActorUnitID != unitID && entry.TargetUnitID != unitID {
			continue
		}
		if cause := casualtyCauseFromRawEvent(entry, byID, unitID); cause != "" {
			return cause
		}
	}
	return "不明"
}

func casualtyCauseFromLog(entry LogEntry, byID map[string]*unit.Record, unitID string) string {
	message := strings.TrimSpace(entry.Message)
	switch entry.Kind {
	case "starvation_down":
		if strings.Contains(message, "归零") || strings.Contains(message, "死亡") {
			return "饥饿归零"
		}
		return "极度饥饿"
	case "starvation":
		if strings.Contains(message, "死亡") || strings.Contains(message, "归零") || strings.Contains(message, "断粮") {
			return "断粮饥饿"
		}
	case "attack":
		if entry.TargetUnitID == unitID && (strings.Contains(message, "倒下") || strings.Contains(message, "死亡")) {
			attackerName := "敌人"
			if attacker := byID[entry.ActorUnitID]; attacker != nil {
				attackerName = attacker.DisplayName()
			}
			return fmt.Sprintf("被%s击倒", attackerName)
		}
	case "trap":
		if entry.ActorUnitID == unitID && (strings.Contains(message, "倒下") || strings.Contains(message, "死亡") || strings.Contains(message, "陷阱")) {
			return "触发陷阱"
		}
	case "gather_risk":
		if entry.ActorUnitID == unitID {
			return "打猎受伤"
		}
	}
	return ""
}

func casualtyCauseFromRawEvent(entry RawEventEntry, byID map[string]*unit.Record, unitID string) string {
	if entry.Source == "log" {
		logEntry := LogEntry{
			Kind:         entry.Kind,
			Message:      entry.Summary,
			ActorUnitID:  entry.ActorUnitID,
			TargetUnitID: entry.TargetUnitID,
		}
		return casualtyCauseFromLog(logEntry, byID, unitID)
	}
	if entry.Kind != "hp" || entry.PayloadJSON == "" {
		return ""
	}
	var payload struct {
		After      float64 `json:"after"`
		ReasonCode string  `json:"reason_code"`
		ReasonText string  `json:"reason_text"`
	}
	if err := json.Unmarshal([]byte(entry.PayloadJSON), &payload); err != nil || payload.After > 0 {
		return ""
	}
	switch payload.ReasonCode {
	case "COMBAT_DOWN":
		if entry.ActorUnitID != "" && entry.ActorUnitID != unitID {
			if attacker := byID[entry.ActorUnitID]; attacker != nil {
				return fmt.Sprintf("被%s击倒", attacker.DisplayName())
			}
		}
		return "战斗伤害"
	case "SURVIVAL_HUNGER":
		return "饥饿耗尽"
	case "COMBAT_HIT":
		if strings.Contains(payload.ReasonText, "陷阱") {
			return "触发陷阱"
		}
		if strings.Contains(payload.ReasonText, "野兽") || strings.Contains(payload.ReasonText, "打猎") {
			return "打猎受伤"
		}
		return "战斗伤害"
	}
	return strings.TrimSpace(payload.ReasonText)
}

func casualtyVisibleToActor(state State, actor *unit.Record, record *unit.Record) bool {
	if actor == nil || record == nil {
		return false
	}
	if actor.FactionID == record.FactionID {
		return true
	}
	if !state.FogOfWarEnabled {
		return true
	}
	vision := actor.Stats.Derived.Vision
	if vision <= 0 {
		vision = 5
	}
	visibleTiles, err := world.ComputeVisibleTiles(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}, vision)
	if err != nil {
		return unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, record.Status.PositionQ, record.Status.PositionR) <= vision
	}
	targetCoord := world.Coord{Q: record.Status.PositionQ, R: record.Status.PositionR}
	for _, coord := range visibleTiles {
		if coord == targetCoord {
			return true
		}
	}
	return false
}
