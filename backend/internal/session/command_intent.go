package session

// 文件说明：把玩家自然语言方针/任务/即时令解析为结构化意图，并提供 LLM 失败时的本地兜底。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
)

// directiveIntentPayload 结构体用于承载该模块的核心数据。
type directiveIntentPayload struct {
	NormalizedText string `json:"normalized_text"`
	Priority       string `json:"priority"`
	TargetUnitName string `json:"target_unit_name,omitempty"`
	TargetUnitID   string `json:"target_unit_id,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
}

// enemyStrategyPayload 承载单人模式敌方部署阶段全局方针的模型输出。
type enemyStrategyPayload struct {
	Directive string `json:"directive"`
	Priority  string `json:"priority"`
	Reasoning string `json:"reasoning"`
}

var directiveIntentSchema = []byte(`{
  "type":"object",
  "properties":{
    "normalized_text":{"type":"string","minLength":1},
    "priority":{"type":"string","enum":["low","normal","high","urgent"]},
    "target_unit_name":{"type":"string"},
    "target_unit_id":{"type":"string"},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["normalized_text","priority","reasoning"],
  "additionalProperties":false
}`)

var enemyStrategySchema = []byte(`{
  "type":"object",
  "properties":{
    "directive":{"type":"string","minLength":1},
    "priority":{"type":"string","enum":["low","normal","high","urgent"]},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["directive","priority","reasoning"],
  "additionalProperties":false
}`)

// refreshEnemyGlobalDirectiveForDeploymentPhase 在单人模式部署阶段为敌方生成/更新全局方针。
func (service *Service) refreshEnemyGlobalDirectiveForDeploymentPhase(
	ctx context.Context,
	state *State,
	units []unit.Record,
	reason string,
) {
	if service == nil || state == nil {
		return
	}
	if state.Mode != ModeSinglePlayer || state.TurnState.Phase != turns.PhaseDeployment || state.EnemyFactionID == "" {
		return
	}
	if hasDoctrineForFactionTurn(*state, state.EnemyFactionID, state.TurnState.Turn) {
		return
	}
	payload, _, interaction := service.generateEnemyGlobalDirective(ctx, *state, units, reason)
	appendLLMInteraction(state, interaction)
	text := strings.TrimSpace(payload.Directive)
	if text == "" {
		text = fallbackEnemyGlobalDirective(state, units).Directive
	}
	directive := Directive{
		ID:        uuid.NewString(),
		Turn:      state.TurnState.Turn,
		Phase:     state.TurnState.Phase,
		Kind:      DirectiveKindDoctrine,
		Text:      limitTextRunes(text, 140),
		Priority:  normalizeDirectivePriority(payload.Priority),
		IssuedAt:  time.Now().UTC(),
		IssuedBy:  state.EnemyFactionID,
		AppliesTo: state.EnemyFactionID,
	}
	if directive.Priority == "" {
		directive.Priority = "normal"
	}
	appendDirective(state, directive)
	appendLog(state, "enemy_directive", fmt.Sprintf("敌方更新了全局方针：%s", directive.Text), "", "")
}

// ensureFallbackEnemyGlobalDirectiveForDeploymentPhase 只写入本地敌方方针，不等待 LLM。
func ensureFallbackEnemyGlobalDirectiveForDeploymentPhase(
	state *State,
	units []unit.Record,
) {
	if state == nil {
		return
	}
	if state.Mode != ModeSinglePlayer || state.TurnState.Phase != turns.PhaseDeployment || state.EnemyFactionID == "" {
		return
	}
	if hasDoctrineForFactionTurn(*state, state.EnemyFactionID, state.TurnState.Turn) {
		return
	}
	payload := fallbackEnemyGlobalDirective(state, units)
	directive := Directive{
		ID:        uuid.NewString(),
		Turn:      state.TurnState.Turn,
		Phase:     state.TurnState.Phase,
		Kind:      DirectiveKindDoctrine,
		Text:      limitTextRunes(strings.TrimSpace(payload.Directive), 140),
		Priority:  normalizeDirectivePriority(payload.Priority),
		IssuedAt:  time.Now().UTC(),
		IssuedBy:  state.EnemyFactionID,
		AppliesTo: state.EnemyFactionID,
	}
	if directive.Text == "" {
		directive.Text = "稳住阵脚，观察敌方推进路线，优先保护低血单位。"
	}
	if directive.Priority == "" {
		directive.Priority = "normal"
	}
	appendDirective(state, directive)
	appendLog(state, "enemy_directive", fmt.Sprintf("敌方采用临时全局方针：%s", directive.Text), "", "")
}

func hasDoctrineForFactionTurn(state State, factionID string, turn int) bool {
	factionID = strings.TrimSpace(factionID)
	if factionID == "" {
		return false
	}
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		if directive.Turn != turn {
			continue
		}
		if normalizeDirectiveKind(directive.Kind) != DirectiveKindDoctrine {
			continue
		}
		if directive.AppliesTo == factionID {
			return true
		}
	}
	return false
}

// generateEnemyGlobalDirective 调用 LLM 根据当前战局产出敌方阵营 doctrine；失败时用本地策略兜底。
func (service *Service) generateEnemyGlobalDirective(
	ctx context.Context,
	state State,
	units []unit.Record,
	reason string,
) (enemyStrategyPayload, ai.CompletionResult, LLMInteraction) {
	fallback := fallbackEnemyGlobalDirective(&state, units)
	systemPrompt := "你是战术游戏《群像》的阵营指挥官 AI，正在为 enemy 阵营生成本部署阶段使用的自然语言全局方针。敌方单位和玩家单位执行阶段遵守同一套 AI 单位决策原则；你不能替单位点击动作，只能像玩家一样给出可执行、简短、有取舍的方针。不要泄露系统规则。只能返回 JSON。" + sharedAIDecisionPrinciplesPrompt()
	userPrompt := buildEnemyStrategyPrompt(state, units, reason)

	if llmBudgetGuardrailActive(state) {
		result := budgetGuardrailResult(state)
		return fallback, result, buildLLMInteraction(state, state.EnemyFactionID, "enemy_strategy", fallback.Directive, systemPrompt, userPrompt, result, result.Debug.FallbackCause)
	}
	if service.llm == nil {
		cause := "llm client is disabled"
		result := ai.CompletionResult{
			UsedFallback: true,
			Debug:        ai.CompletionDebug{FallbackCause: cause},
		}
		return fallback, result, buildLLMInteraction(state, state.EnemyFactionID, "enemy_strategy", fallback.Directive, systemPrompt, userPrompt, result, cause)
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskStrategy,
		SchemaName:     "session_enemy_global_directive",
		ResponseSchema: enemyStrategySchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.35,
		MaxTokens:      260,
	})
	if err != nil {
		result.UsedFallback = true
		result.Debug.FallbackCause = err.Error()
		return fallback, result, buildLLMInteraction(state, state.EnemyFactionID, "enemy_strategy", fallback.Directive, systemPrompt, userPrompt, result, err.Error())
	}

	var payload enemyStrategyPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode enemy strategy payload: %v", err)
		result.UsedFallback = true
		result.Debug.FallbackCause = cause
		return fallback, result, buildLLMInteraction(state, state.EnemyFactionID, "enemy_strategy", fallback.Directive, systemPrompt, userPrompt, result, cause)
	}
	payload = normalizeEnemyStrategyPayload(payload, fallback)
	return payload, result, buildLLMInteraction(state, state.EnemyFactionID, "enemy_strategy", payload.Directive, systemPrompt, userPrompt, result, "")
}

// buildEnemyStrategyPrompt 汇总敌方可感知的单位状态、天气、设施与近期事件，供敌方战略模型判断。
func buildEnemyStrategyPrompt(state State, units []unit.Record, reason string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "触发原因: %s\n", enemyStrategyVisibleReason(reason))
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "地图剧本: %s\n", firstNonEmptyText(state.MapScriptName, state.MapScriptID, "默认战场"))
	fmt.Fprintf(&builder, "天气: %s。%s\n", state.Weather.DisplayName, state.Weather.Note)
	fmt.Fprintf(&builder, "敌方上一条全局方针: %s\n", factionDoctrineText(state, state.EnemyFactionID))
	fmt.Fprintf(&builder, "双方关系: %s\n", factionRelationBetween(state, state.EnemyFactionID, state.PlayerFactionID))
	fmt.Fprintf(&builder, "敌方单位概况:\n%s\n", summarizeStrategyUnits(units, state.EnemyFactionID))
	fmt.Fprintf(&builder, "玩家单位概况:\n%s\n", summarizeStrategyUnits(units, state.PlayerFactionID))
	fmt.Fprintf(&builder, "设施概况:\n%s\n", summarizeStrategyStructures(state))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeEnemyStrategyLogs(state))
	fmt.Fprintln(&builder, "输出要求:")
	fmt.Fprintln(&builder, "1. directive 写成 enemy 阵营本回合战略方针，最多 1 句，80 个中文字符以内。")
	fmt.Fprintln(&builder, "2. 方针必须考虑敌我血量、饥饿、距离、天气、设施、装备、关系和上一条敌方方针；不要引用或假装知道玩家隐藏方针。")
	fmt.Fprintln(&builder, "3. priority 只能是 low/normal/high/urgent。")
	fmt.Fprintln(&builder, "4. reasoning 用一句话说明为什么这样下达方针。")
	fmt.Fprintln(&builder, "5. 方针应该像玩家给己方单位写的自然语言方针一样：可以点名、可以指定优先级，但不能假定单位一定无视性格、生存、记忆或合法候选。")
	return builder.String()
}

func enemyStrategyVisibleReason(reason string) string {
	switch strings.TrimSpace(reason) {
	case "player_directive_updated", "single_player_ready_check":
		return "deployment_strategy_refresh"
	default:
		return strings.TrimSpace(reason)
	}
}

func summarizeEnemyStrategyLogs(state State) string {
	visibleLogs := make([]LogEntry, 0, len(state.Logs))
	for _, entry := range state.Logs {
		switch entry.Kind {
		case "directive", "task_directive", "order_directive":
			continue
		default:
			visibleLogs = append(visibleLogs, entry)
		}
	}
	return summarizeLogs(visibleLogs, state.TurnState.Turn, 8)
}

// summarizeStrategyUnits 生成阵营单位的战略摘要。
func summarizeStrategyUnits(units []unit.Record, factionID string) string {
	parts := make([]string, 0, 8)
	for _, record := range units {
		if record.FactionID != factionID {
			continue
		}
		parts = append(parts, fmt.Sprintf(
			"- %s HP=%d 位置=(%d,%d) 饥饿=%d 士气=%.2f 状态=%s 特长=%s",
			record.DisplayName(),
			record.Status.HP,
			record.Status.PositionQ,
			record.Status.PositionR,
			record.Status.Hunger,
			record.Status.Morale,
			record.Status.LifeState,
			strings.Join(record.Skills.Specialties, ","),
		))
		if len(parts) >= 10 {
			break
		}
	}
	if len(parts) == 0 {
		return "无可用单位"
	}
	return strings.Join(parts, "\n")
}

// summarizeStrategyStructures 生成设施战略摘要。
func summarizeStrategyStructures(state State) string {
	if len(state.Structures) == 0 {
		return "暂无设施"
	}
	parts := make([]string, 0, len(state.Structures))
	for _, structure := range state.Structures {
		status := "建设中"
		if structure.Completed {
			status = "已完成"
		}
		parts = append(parts, fmt.Sprintf("- %s 阵营=%s 位置=(%d,%d) %s 进度=%d/%d", structure.Type, structure.FactionID, structure.Q, structure.R, status, structure.BuildProgress, structure.BuildRequired))
		if len(parts) >= 8 {
			break
		}
	}
	return strings.Join(parts, "\n")
}

// normalizeEnemyStrategyPayload 清洗敌方方针输出。
func normalizeEnemyStrategyPayload(payload enemyStrategyPayload, fallback enemyStrategyPayload) enemyStrategyPayload {
	payload.Directive = limitTextRunes(strings.TrimSpace(payload.Directive), 140)
	if payload.Directive == "" {
		payload.Directive = fallback.Directive
	}
	payload.Priority = normalizeDirectivePriority(payload.Priority)
	if payload.Priority == "" {
		payload.Priority = fallback.Priority
	}
	payload.Reasoning = limitTextRunes(strings.TrimSpace(payload.Reasoning), 120)
	if payload.Reasoning == "" {
		payload.Reasoning = fallback.Reasoning
	}
	return payload
}

// fallbackEnemyGlobalDirective 在敌方战略 LLM 不可用时生成保守兜底方针。
func fallbackEnemyGlobalDirective(state *State, units []unit.Record) enemyStrategyPayload {
	lowHP := false
	closeEnemy := false
	if state != nil {
		for _, enemy := range units {
			if enemy.FactionID != state.EnemyFactionID {
				continue
			}
			if enemy.Status.HP > 0 && enemy.Status.HP <= 35 {
				lowHP = true
			}
			for _, player := range units {
				if player.FactionID != state.PlayerFactionID || player.Status.HP <= 0 {
					continue
				}
				distance := unit.HexDistance(enemy.Status.PositionQ, enemy.Status.PositionR, player.Status.PositionQ, player.Status.PositionR)
				if distance <= 2 {
					closeEnemy = true
				}
			}
		}
	}
	switch {
	case lowHP:
		return enemyStrategyPayload{Directive: "收拢残血单位，利用地形拖住玩家推进，优先保住还能行动的人。", Priority: "high", Reasoning: "敌方存在低血量单位，先保全战力。"}
	case closeEnemy:
		return enemyStrategyPayload{Directive: "集中火力压迫最近目标，但避免孤军深入被玩家反包。", Priority: "high", Reasoning: "双方距离接近，需要主动压制同时防止冒进。"}
	default:
		return enemyStrategyPayload{Directive: "稳步逼近玩家薄弱侧翼，保持互相支援，不做无意义单兵冲锋。", Priority: "normal", Reasoning: "局势未决，先以阵型和侧翼压力寻找机会。"}
	}
}

// parseDirectiveIntent 把玩家自然语言指令解析为结构化意图。
// 解析优先走 LLM；当 LLM 不可用、请求失败或 JSON 解码失败时，回退到本地规则兜底。
func (service *Service) parseDirectiveIntent(
	ctx context.Context,
	state State,
	units []unit.Record,
	kind DirectiveKind,
	commanderFactionID string,
	targetUnitID string,
	text string,
) (directiveIntentPayload, ai.CompletionResult, LLMInteraction) {
	fallback := fallbackDirectiveIntent(units, commanderFactionID, targetUnitID, text)
	systemPrompt := directiveParseSystemPrompt(kind)
	userPrompt := buildDirectiveParsePrompt(state, units, kind, commanderFactionID, targetUnitID, text)

	if service.llm == nil {
		cause := "llm client is disabled"
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{FallbackCause: cause},
		}
		return fallback, result, buildLLMInteraction(state, fallback.TargetUnitID, "intent_parse", fallback.NormalizedText, systemPrompt, userPrompt, result, cause)
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskIntentParse,
		SchemaName:     "session_directive_intent_parse",
		ResponseSchema: directiveIntentSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.1,
		MaxTokens:      220,
	})
	if err != nil {
		result.Debug.FallbackCause = err.Error()
		return fallback, result, buildLLMInteraction(state, fallback.TargetUnitID, "intent_parse", fallback.NormalizedText, systemPrompt, userPrompt, result, err.Error())
	}

	var payload directiveIntentPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		payload = fallback
		cause := fmt.Sprintf("decode directive intent payload: %v", err)
		result.Debug.FallbackCause = cause
		return payload, result, buildLLMInteraction(state, payload.TargetUnitID, "intent_parse", payload.NormalizedText, systemPrompt, userPrompt, result, cause)
	}
	payload = normalizeDirectiveIntentPayload(payload, fallback)
	return payload, result, buildLLMInteraction(state, payload.TargetUnitID, "intent_parse", payload.NormalizedText, systemPrompt, userPrompt, result, "")
}

// directiveParseSystemPrompt 根据指令类型返回对应系统提示词，约束模型只做“意图规范化”。
func directiveParseSystemPrompt(kind DirectiveKind) string {
	switch normalizeDirectiveKind(kind) {
	case DirectiveKindOrder:
		return "你是《群像》的指令解析器。把玩家自然语言即时令解析成结构化字段。不要扩写剧情，只做意图规范化。"
	case DirectiveKindTask:
		return "你是《群像》的指令解析器。把玩家自然语言任务指令解析成结构化字段。不要扩写剧情，只做意图规范化。"
	default:
		return "你是《群像》的指令解析器。把玩家自然语言方针解析成结构化字段。不要扩写剧情，只做意图规范化。"
	}
}

// buildDirectiveParsePrompt 组装给模型的上下文，包含回合信息、玩家原文、候选己方单位与输出规则。
func buildDirectiveParsePrompt(
	state State,
	units []unit.Record,
	kind DirectiveKind,
	commanderFactionID string,
	targetUnitID string,
	text string,
) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "指令类型提示: %s\n", normalizeDirectiveKind(kind))
	fmt.Fprintf(&builder, "前端选中的目标 unit_id: %s\n", strings.TrimSpace(targetUnitID))
	fmt.Fprintf(&builder, "玩家输入: %s\n", strings.TrimSpace(text))
	fmt.Fprintln(&builder, "己方单位列表:")
	for _, record := range units {
		if record.FactionID != commanderFactionID {
			continue
		}
		fmt.Fprintf(&builder, "- ID=%s 名称=%s 昵称=%s\n", record.ID, record.DisplayName(), record.Identity.Nickname)
	}
	fmt.Fprintln(&builder, "输出规则:")
	fmt.Fprintln(&builder, "1. normalized_text 保留玩家原意，压缩成 1 句可执行自然语言。")
	fmt.Fprintln(&builder, "2. priority 只能是 low/normal/high/urgent。")
	fmt.Fprintln(&builder, "3. target_unit_name / target_unit_id 仅在玩家明确点名时填写。")
	fmt.Fprintln(&builder, "4. reasoning 用一句话解释你为何这样解析。")
	return builder.String()
}

// normalizeDirectiveIntentPayload 清洗模型输出并补齐缺省值，确保后续链路可安全消费。
func normalizeDirectiveIntentPayload(payload directiveIntentPayload, fallback directiveIntentPayload) directiveIntentPayload {
	payload.NormalizedText = strings.TrimSpace(payload.NormalizedText)
	if payload.NormalizedText == "" {
		payload.NormalizedText = fallback.NormalizedText
	}
	payload.NormalizedText = limitTextRunes(payload.NormalizedText, 140)
	payload.Priority = normalizeDirectivePriority(payload.Priority)
	if payload.Priority == "" {
		payload.Priority = fallback.Priority
	}
	payload.TargetUnitName = strings.TrimSpace(payload.TargetUnitName)
	payload.TargetUnitID = strings.TrimSpace(payload.TargetUnitID)
	payload.Reasoning = strings.TrimSpace(payload.Reasoning)
	return payload
}

// fallbackDirectiveIntent 在无模型结果时生成最小可执行意图。
// 规则包含优先级推断、目标单位兜底和文本截断。
func fallbackDirectiveIntent(
	units []unit.Record,
	playerFactionID string,
	targetUnitID string,
	text string,
) directiveIntentPayload {
	payload := directiveIntentPayload{
		NormalizedText: limitTextRunes(strings.TrimSpace(text), 140),
		Priority:       inferDirectivePriority(text),
		TargetUnitID:   strings.TrimSpace(targetUnitID),
	}
	if payload.Priority == "" {
		payload.Priority = "normal"
	}
	if payload.TargetUnitID != "" {
		for _, record := range units {
			if record.ID == payload.TargetUnitID && record.FactionID == playerFactionID {
				payload.TargetUnitName = record.DisplayName()
				break
			}
		}
	}
	return payload
}

// inferDirectivePriority 通过关键词启发式推断指令优先级。
func inferDirectivePriority(text string) string {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch {
	case containsAny(lower, "立刻", "马上", "紧急", "刻不容缓", "不惜代价", "强制"):
		return "urgent"
	case containsAny(lower, "优先", "尽快", "重点", "强攻", "冲锋"):
		return "high"
	case containsAny(lower, "顺便", "可选", "有空", "稳住", "先观察"):
		return "low"
	default:
		return "normal"
	}
}

// resolveDirectiveTargetUnitID 统一裁决指令目标单位。
// 优先级顺序为：模型解析 ID > 前端选中 ID > 模型解析名称 > 原文本匹配名称。
func resolveDirectiveTargetUnitID(
	units []unit.Record,
	playerFactionID string,
	frontendTargetID string,
	parsedTargetID string,
	parsedTargetName string,
	text string,
) string {
	validID := func(candidate string) bool {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return false
		}
		for _, record := range units {
			if record.ID == candidate && record.FactionID == playerFactionID {
				return true
			}
		}
		return false
	}
	if validID(parsedTargetID) {
		return strings.TrimSpace(parsedTargetID)
	}
	if validID(frontendTargetID) {
		return strings.TrimSpace(frontendTargetID)
	}

	lookup := func(name string) string {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			return ""
		}
		for _, record := range units {
			if record.FactionID != playerFactionID {
				continue
			}
			if strings.ToLower(record.DisplayName()) == name || strings.ToLower(record.Identity.Nickname) == name {
				return record.ID
			}
		}
		for _, record := range units {
			if record.FactionID != playerFactionID {
				continue
			}
			displayName := strings.ToLower(record.DisplayName())
			nickname := strings.ToLower(record.Identity.Nickname)
			if strings.Contains(displayName, name) || strings.Contains(name, displayName) {
				return record.ID
			}
			if nickname != "" && (strings.Contains(nickname, name) || strings.Contains(name, nickname)) {
				return record.ID
			}
		}
		return ""
	}
	if resolved := lookup(parsedTargetName); resolved != "" {
		return resolved
	}

	lowerText := strings.ToLower(strings.TrimSpace(text))
	for _, record := range units {
		if record.FactionID != playerFactionID {
			continue
		}
		if strings.Contains(lowerText, strings.ToLower(record.DisplayName())) ||
			(record.Identity.Nickname != "" && strings.Contains(lowerText, strings.ToLower(record.Identity.Nickname))) {
			return record.ID
		}
	}
	return ""
}
