package session

// 文件说明：封装部署协商、日常 upkeep、战斗动摇三类 LLM 辅助流程的 schema、prompt 与输出归一化。

import (
	"encoding/json"
	"fmt"
	"strings"

	"qunxiang/backend/internal/unit"
)

// deploymentCandidate 描述部署阶段可选的交易候选项。
type deploymentCandidate struct {
	ID           string
	Kind         string
	ActorUnitID  string
	TargetUnitID string
	ItemID       string
	OtherItemID  string
	Price        int
	GoldAmount   int
	Summary      string
}

// normalizeDeploymentChoice 对双人交易协商输出做长度和回填兜底，防止前端展示溢出。
func normalizeDeploymentChoice(choice deploymentChoicePayload) deploymentChoicePayload {
	choice.CandidateID = strings.TrimSpace(choice.CandidateID)
	choice.LeftLine = limitTextRunes(strings.TrimSpace(choice.LeftLine), 16)
	choice.RightLine = limitTextRunes(strings.TrimSpace(choice.RightLine), 16)
	choice.LeftMemory = limitTextRunes(strings.TrimSpace(choice.LeftMemory), llmMemoryRuneLimit)
	choice.RightMemory = limitTextRunes(strings.TrimSpace(choice.RightMemory), llmMemoryRuneLimit)
	choice.Summary = limitTextRunes(strings.TrimSpace(choice.Summary), 48)
	choice.Reasoning = strings.TrimSpace(choice.Reasoning)
	if choice.Summary == "" {
		choice.Summary = limitTextRunes(choice.Reasoning, 48)
	}
	if choice.LeftLine == "" {
		choice.LeftLine = limitTextRunes(choice.Summary, 16)
	}
	if choice.RightLine == "" {
		choice.RightLine = limitTextRunes(choice.Summary, 16)
	}
	if choice.LeftMemory == "" {
		switch {
		case choice.LeftLine != "":
			choice.LeftMemory = limitTextRunes(choice.LeftLine, llmMemoryRuneLimit)
		case choice.Summary != "":
			choice.LeftMemory = limitTextRunes(choice.Summary, llmMemoryRuneLimit)
		}
	}
	if choice.RightMemory == "" {
		switch {
		case choice.RightLine != "":
			choice.RightMemory = limitTextRunes(choice.RightLine, llmMemoryRuneLimit)
		case choice.Summary != "":
			choice.RightMemory = limitTextRunes(choice.Summary, llmMemoryRuneLimit)
		}
	}
	return choice
}

// normalizeUpkeepChoice 约束日常动作输出，避免模型返回超长文本。
func normalizeUpkeepChoice(choice upkeepChoicePayload) upkeepChoicePayload {
	choice.Bubble = limitTextRunes(strings.TrimSpace(choice.Bubble), 16)
	choice.Memory = limitTextRunes(strings.TrimSpace(choice.Memory), llmMemoryRuneLimit)
	choice.Reasoning = strings.TrimSpace(choice.Reasoning)
	if choice.Memory == "" {
		choice.Memory = limitTextRunes(choice.Bubble, llmMemoryRuneLimit)
	}
	return choice
}

// normalizeCombatShakeChoice 校验应激动作枚举，防止非法动作穿透到执行层。
func normalizeCombatShakeChoice(choice combatShakeChoicePayload) combatShakeChoicePayload {
	choice.Action = strings.TrimSpace(strings.ToLower(choice.Action))
	choice.Bubble = limitTextRunes(strings.TrimSpace(choice.Bubble), 16)
	choice.Memory = limitTextRunes(strings.TrimSpace(choice.Memory), llmMemoryRuneLimit)
	choice.Reasoning = strings.TrimSpace(choice.Reasoning)
	switch choice.Action {
	case "retreat", "surrender", "rage", "continue":
	default:
		choice.Action = "continue"
	}
	if choice.Memory == "" {
		choice.Memory = limitTextRunes(choice.Bubble, llmMemoryRuneLimit)
	}
	return choice
}

// buildDeploymentChoiceSchema 构建部署交易决策的 JSON schema。
func buildDeploymentChoiceSchema(candidates []deploymentCandidate) []byte {
	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}

	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"candidate_id": map[string]any{
				"type": "string",
				"enum": ids,
			},
			"left_line": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": 16,
			},
			"right_line": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": 16,
			},
			"left_memory": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": 18,
			},
			"right_memory": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": 18,
			},
			"summary": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": 48,
			},
			"reasoning": map[string]any{
				"type":      "string",
				"minLength": 1,
			},
		},
		"required":             []string{"candidate_id", "left_line", "right_line", "left_memory", "right_memory", "summary", "reasoning"},
		"additionalProperties": false,
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		return []byte(`{"type":"object","properties":{"candidate_id":{"type":"string","enum":["hold"]},"left_line":{"type":"string","minLength":1,"maxLength":16},"right_line":{"type":"string","minLength":1,"maxLength":16},"left_memory":{"type":"string","minLength":1,"maxLength":18},"right_memory":{"type":"string","minLength":1,"maxLength":18},"summary":{"type":"string","minLength":1,"maxLength":48},"reasoning":{"type":"string","minLength":1}},"required":["candidate_id","left_line","right_line","left_memory","right_memory","summary","reasoning"],"additionalProperties":false}`)
	}
	return encoded
}

// deploymentSystemPrompt 生成“双单位协商交易”任务的 system prompt。
func deploymentSystemPrompt(left unit.Record, right unit.Record) string {
	return fmt.Sprintf(
		"你要同时扮演《群像》中的两个相邻 AI 单位：%s 和 %s。玩家只能用自然语言表达意图，不能替他们点击交易。请根据两人的性格、记忆、钱包、背包、周围环境和玩家最近的方针/对话，从候选列表中决定他们这一刻是否自己谈成一笔交易，并给出两人各自会说的一句短话和各自会记住的一句短记忆。只能返回 JSON。",
		left.DisplayName(),
		right.DisplayName(),
	)
}

// upkeepSystemPrompt 生成日常生存动作判断任务的 system prompt。
func upkeepSystemPrompt(record unit.Record) string {
	return fmt.Sprintf(
		"你是《群像》中的单位 %s。现在轮到你自己判断要不要处理一件日常生存动作，比如吃口粮或吃药。玩家不会逐个指定你吃不吃。请根据你的性格、记忆、当前状态、周围环境和全局方针，自己决定是否行动；方针很重要，但你仍要结合自身处境、生存风险和个人判断。重要生存规则：饥饿度降到 0 会直接死亡；饥饿度高于 80 每回合自动恢复 3 HP；没有口粮且饥饿偏低时，后续要主动找食物、采集、狩猎、钓鱼、交易或求援，不能原地等到饿死；受伤时可以用治疗药剂恢复。行动时给出一句短气泡和一句短记忆。只能返回 JSON。",
		record.DisplayName(),
	)
}

// combatShakeSystemPrompt 生成战斗应激快决策任务的 system prompt。
func combatShakeSystemPrompt(record unit.Record) string {
	return fmt.Sprintf(
		"你是《群像》中的单位 %s，正在执行阶段战斗中。玩家不会逐项遥控你；玩家方针是重要强信号，但应激反应还要结合环境、性格、记忆、关系和当前压力。请在 200ms 内做一次应激快决策。牢记：饥饿度为 0 会直接死亡，饥饿度高于 80 每回合自动恢复 3 HP。你只能输出 JSON，且 action 只能是 continue/retreat/surrender/rage 之一。",
		record.DisplayName(),
	)
}

// buildDeploymentPrompt 提供“两单位会话式交易”上下文，让 AI 自主决定是否成交及双方台词。
func buildDeploymentPrompt(
	state State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	candidates []deploymentCandidate,
	leftMemorySummary string,
	rightMemorySummary string,
	leftRelationSummary string,
	rightRelationSummary string,
) string {
	var builder strings.Builder
	policy := diplomacyPolicyForFaction(state, state.PlayerFactionID)

	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "玩家全局方针: %s\n", state.GlobalDirective.Text)
	fmt.Fprintln(&builder, "方针优先级: 默认优先满足玩家方针；只有会导致直接死亡或明显自杀时，才允许偏离。")
	if left.FactionID != right.FactionID && policy.ForbidCrossFactionTrade && isPlayerEnemyFactionPair(state, left.FactionID, right.FactionID) {
		fmt.Fprintf(&builder, "玩家严令: 当前禁止跨势力交易。来源：%s\n", policy.SourceText)
	}
	fmt.Fprintf(&builder, "%s 的资料: %s\n", left.DisplayName(), describeUnit(*left, nil))
	fmt.Fprintf(&builder, "%s 的性格: %s\n", left.DisplayName(), summarizeActorPersonality(*left))
	fmt.Fprintf(&builder, "%s 的环境: %s\n", left.DisplayName(), summarizeImmediateEnvironment(state, byID, left))
	if strings.TrimSpace(leftMemorySummary) == "" {
		leftMemorySummary = summarizeUnitMemoryWithTurn(*left, state.TurnState.Turn, 6)
	}
	fmt.Fprintf(&builder, "%s 的记忆:\n%s\n", left.DisplayName(), leftMemorySummary)
	if strings.TrimSpace(leftRelationSummary) == "" {
		leftRelationSummary = relationSummaryNoKnown
	}
	fmt.Fprintf(&builder, "%s 的关系网:\n%s\n", left.DisplayName(), leftRelationSummary)
	fmt.Fprintf(&builder, "%s 的资料: %s\n", right.DisplayName(), describeUnit(*right, nil))
	fmt.Fprintf(&builder, "%s 的性格: %s\n", right.DisplayName(), summarizeActorPersonality(*right))
	fmt.Fprintf(&builder, "%s 的环境: %s\n", right.DisplayName(), summarizeImmediateEnvironment(state, byID, right))
	if strings.TrimSpace(rightMemorySummary) == "" {
		rightMemorySummary = summarizeUnitMemoryWithTurn(*right, state.TurnState.Turn, 6)
	}
	fmt.Fprintf(&builder, "%s 的记忆:\n%s\n", right.DisplayName(), rightMemorySummary)
	if strings.TrimSpace(rightRelationSummary) == "" {
		rightRelationSummary = relationSummaryNoKnown
	}
	fmt.Fprintf(&builder, "%s 的关系网:\n%s\n", right.DisplayName(), rightRelationSummary)
	fmt.Fprintf(&builder, "与两人相关的最近对话:\n%s\n", summarizeDialogueHistory(state.DialogueHistory, left.ID, state.TurnState.Turn, 4))
	fmt.Fprintf(&builder, "%s\n", summarizeDialogueHistory(state.DialogueHistory, right.ID, state.TurnState.Turn, 4))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "候选交易:\n%s\n", formatDeploymentCandidates(candidates))
	fmt.Fprintln(&builder, "规则:")
	fmt.Fprintln(&builder, "1. 只能从 candidate_id 里选一个，不能自造动作。")
	fmt.Fprintln(&builder, "2. 如果没有必要成交，就选 hold。")
	fmt.Fprintln(&builder, "3. 如果选的不是 hold，left_line 和 right_line 都要是两人自己会说的短句，各不超过 16 个字。")
	fmt.Fprintln(&builder, "4. left_memory 和 right_memory 都要是一句第一人称短记忆，各不超过 18 个字。")
	fmt.Fprintln(&builder, "5. 所有文本都要像单位本人，不要写系统旁白。")
	fmt.Fprintln(&builder, "6. summary 要给一句 18-48 字的行动摘要（可用于日志），描述这次是否成交以及为什么。")

	return builder.String()
}

// buildUpkeepPrompt 驱动单位自行判断是否进食/补给等日常动作。
func buildUpkeepPrompt(
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	eventLabel string,
	memorySummary string,
	relationSummary string,
	knowledgeSummary string,
) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "当前要判断的事: %s\n", eventLabel)
	fmt.Fprintf(&builder, "玩家自然语言指令上下文: %s\n", directiveForUnit(state, record.ID, record.FactionID))
	fmt.Fprintln(&builder, "方针优先级: 默认优先满足玩家方针；只有会导致直接死亡或明显自杀时，才允许偏离。饥饿度为 0 会直接死亡，饥饿度高于 80 每回合自动恢复 3 HP，受伤时可以用治疗药剂恢复。")
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(record, nil))
	fmt.Fprintf(&builder, "你的性格: %s\n", summarizeActorPersonality(record))
	fmt.Fprintf(&builder, "你的环境摘要: %s\n", summarizeImmediateEnvironment(state, byID, &record))
	if strings.TrimSpace(memorySummary) == "" {
		memorySummary = summarizeUnitMemoryWithTurn(record, state.TurnState.Turn, 6)
	}
	fmt.Fprintf(&builder, "你记得的重点:\n%s\n", memorySummary)
	if strings.TrimSpace(relationSummary) == "" {
		relationSummary = relationSummaryNoKnown
	}
	fmt.Fprintf(&builder, "你的关系网:\n%s\n", relationSummary)
	if strings.TrimSpace(knowledgeSummary) == "" {
		knowledgeSummary = "无"
	}
	fmt.Fprintf(&builder, "你已掌握的世界规律:\n%s\n", knowledgeSummary)
	fmt.Fprintf(&builder, "你当前属性受影响说明:\n%s\n", summarizeCurrentAttributeInfluences(state, record))
	fmt.Fprintf(&builder, "最近对话:\n%s\n", summarizeDialogueHistory(state.DialogueHistory, record.ID, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 6))
	fmt.Fprintln(&builder, "请自己决定 should_act 是 true 还是 false。")
	fmt.Fprintln(&builder, "如果 should_act=true，再给出 bubble 和 memory。")
	fmt.Fprintln(&builder, "bubble 最多 10 个字；memory 可写到 60 个字，要具体记录对象、数值、位置、物品或发现。")
	fmt.Fprintln(&builder, "reasoning 需明确说明：你的决定受到了哪些环境/属性/记忆因素影响。")

	return builder.String()
}

// buildCombatShakePrompt 用于高压瞬时决策，要求模型明确给出受哪些因素触发。
func buildCombatShakePrompt(
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	triggers []string,
	memorySummary string,
	relationSummary string,
	knowledgeSummary string,
) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "应激触发: %s\n", strings.Join(triggers, " / "))
	fmt.Fprintf(&builder, "玩家自然语言指令上下文: %s\n", directiveForUnit(state, record.ID, record.FactionID))
	fmt.Fprintln(&builder, "方针优先级: 默认优先满足玩家方针；只有会导致直接死亡或明显自杀时，才允许偏离。饥饿度为 0 会直接死亡，饥饿度高于 80 每回合自动恢复 3 HP。")
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(record, nil))
	fmt.Fprintf(&builder, "你的性格: %s\n", summarizeActorPersonality(record))
	fmt.Fprintf(&builder, "你的环境摘要: %s\n", summarizeImmediateEnvironment(state, byID, &record))
	if strings.TrimSpace(memorySummary) == "" {
		memorySummary = summarizeUnitMemoryWithTurn(record, state.TurnState.Turn, 6)
	}
	fmt.Fprintf(&builder, "你的最近记忆:\n%s\n", memorySummary)
	if strings.TrimSpace(relationSummary) == "" {
		relationSummary = relationSummaryNoKnown
	}
	fmt.Fprintf(&builder, "你的关系网:\n%s\n", relationSummary)
	if strings.TrimSpace(knowledgeSummary) == "" {
		knowledgeSummary = "无"
	}
	fmt.Fprintf(&builder, "你已掌握的世界规律:\n%s\n", knowledgeSummary)
	fmt.Fprintf(&builder, "你当前属性受影响说明:\n%s\n", summarizeCurrentAttributeInfluences(state, record))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 8))
	fmt.Fprintln(&builder, "输出规则:")
	fmt.Fprintln(&builder, "1. action 只能是 continue / retreat / surrender / rage。")
	fmt.Fprintln(&builder, "2. bubble 是第一人称短句，不超过 16 字；memory 可写到 60 字，要具体记录触发者、伤害/位置/目标/原因。")
	fmt.Fprintln(&builder, "3. reasoning 必须说明你为何这样做。")
	fmt.Fprintln(&builder, "4. retreat 表示优先撤离危险；surrender 表示停手保命；rage 表示暴怒抢攻。")
	fmt.Fprintln(&builder, "5. reasoning 需点明天气/地形/当前状态或记忆里哪条信息触发了这个应激动作。")

	return builder.String()
}

// formatDeploymentCandidates 把交易候选列表格式化为提示词文本。
func formatDeploymentCandidates(candidates []deploymentCandidate) string {
	lines := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		lines = append(lines, fmt.Sprintf("%s: %s", candidate.ID, candidate.Summary))
	}
	return strings.Join(lines, "\n")
}

// resolveDeploymentCandidate 根据 candidate_id 解析具体交易候选。
func resolveDeploymentCandidate(candidates []deploymentCandidate, candidateID string) (deploymentCandidate, bool) {
	for _, candidate := range candidates {
		if candidate.ID == candidateID {
			return candidate, true
		}
	}
	return deploymentCandidate{}, false
}
