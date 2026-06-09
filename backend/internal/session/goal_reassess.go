package session

// 文件说明：周期目标重估（GDD 自治宪法 §4「长期目标」一侧）。每 goalReassessCadence(=24) tick，为单位跑一次
// 轻量 LLM（GenerateJSON + JSON Schema 强校验 + 规则 fallback），据其人格/野心/近况/旧目标产出 ≤60 字的短期目标，
// 写成一条「高显著度记忆」（经既有 storeMemoryAndSyncHighlights 记忆写入 API，source 标 GOAL_REASSESS 原因码、
// importanceBoost 拉满显著度），供后续决策上下文召回为「她惦记的事」。本文件不碰 types.go / memory*.go / service.go：
// 只消费 Schema 阶段已建好的 unit.Ambition 字段与 engine/events 已登记的 ReasonGoalReassess 原因码，
// 对外暴露 reassessGoalIfDue 供 Wire 阶段接进回合边界。LLM 不可用/超时/解析失败/预算护栏触发时走 fallback，绝不中断主循环。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	// goalReassessCadence 是目标重估周期（tick）。每 24 tick 触发一次，与回合边界结算同频但远疏于逐回合，保证轻量。
	goalReassessCadence = 24
	// goalReassessMaxRunes 是短期目标文本上限（字）——产物落记忆、进 prompt，控制在 60 字内保持「一句话目标」的密度。
	goalReassessMaxRunes = 60
	// goalReassessImportanceBoost 把目标记忆的推断重要度往上抬，确保它作为「她惦记的事」长期高显著、易被召回。
	goalReassessImportanceBoost = 3
	// goalReassessSource 是写入记忆时的 source 标记，复用 engine/events 已登记的 GOAL_REASSESS 原因码字符串，
	// 让记忆来源与原因码目录同口径、可审计。
	goalReassessSource = string(events.ReasonGoalReassess)
)

// goalReassessPayload 是 LLM 目标重估的结构化产物（被 goalReassessSchema 强校验）。
type goalReassessPayload struct {
	Goal string `json:"goal"`
}

// goalReassessSchema 强约束 LLM 只返回 {goal:string}，长度 [1,goalReassessMaxRunes]（字符上限，gojsonschema 按 rune 计）。
var goalReassessSchema = []byte(`{
  "type":"object",
  "properties":{
    "goal":{"type":"string","minLength":1,"maxLength":60}
  },
  "required":["goal"],
  "additionalProperties":false
}`)

// reassessGoalIfDue 是供 Wire 阶段接进回合边界的导出入口：当 turn 命中重估周期时，为单位重估并落库短期目标。
// 非到点（turn<=0 或 turn%goalReassessCadence!=0）直接 no-op 返回 nil——调用方可无脑每回合边界对每个单位调一次。
// best-effort：任一步失败只返回错误供测试断言，调用方按既有 best-effort idiom `_ =` 即可，绝不中断主循环。
//
// Wire 接线签名：
//
//	func (service *Service) reassessGoalIfDue(ctx context.Context, state *State, record *unit.Record, turn int) error
//
// 建议接线点：session 回合边界结算（Execution→Deployment）逐单位循环内，与 compactMemory2ForUnit 同级 best-effort 调用。
func (service *Service) reassessGoalIfDue(ctx context.Context, state *State, record *unit.Record, turn int) error {
	if service == nil || state == nil || record == nil {
		return nil
	}
	if turn <= 0 || turn%goalReassessCadence != 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	goal, interaction := service.generateGoalReassessment(ctx, *state, *record, turn)
	if strings.TrimSpace(interaction.Kind) != "" {
		service.appendLLMInteractionWithSpend(ctx, state, interaction)
	}
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil
	}

	summary := fmt.Sprintf("我重新掂量了眼下该做的事：%s", goal)
	if err := service.storeMemoryAndSyncHighlights(ctx, record, turn, summary, goalReassessSource, goalReassessImportanceBoost); err != nil {
		return err
	}
	appendLog(state, "goal_reassess", fmt.Sprintf("%s 重估了短期目标：%s", record.DisplayName(), goal), record.ID, "")

	// 锚自动 upsert（设计 耦合 §1.3）：目标重估落地即把「她当前最惦记的目标」做成 goal 锚，喂 relevance.Score——
	// 让世界事件能聚焦到「她在乎的目标」。best-effort：吞错只记日志，绝不阻断目标重估主链路。ref 走默认 goal:<id> 幂等覆盖。
	if anchorErr := service.UpsertGoalAnchor(ctx, state.ID, record.ID, "", goalAnchorWeight, goal); anchorErr != nil {
		appendLog(state, "goal_anchor_failed", fmt.Sprintf("%s 的目标锚落定失败：%v", record.DisplayName(), anchorErr), record.ID, "")
	}
	return nil
}

// goalAnchorWeight 是目标锚的默认权重（满权——「当前目标」是她心上最重的一根弦之一）。
const goalAnchorWeight = 1.0

// generateGoalReassessment 跑 LLM 产短期目标；LLM 不可用 / 预算护栏 / 账户超额 / 解析失败时回落规则 fallback。
// 返回目标文本与一条 LLMInteraction（供 appendLLMInteractionWithSpend 记账；fallback 路径也带，便于遥测）。
func (service *Service) generateGoalReassessment(ctx context.Context, state State, record unit.Record, turn int) (string, LLMInteraction) {
	fallback := fallbackGoalReassessment(state, record)
	systemPrompt := fmt.Sprintf(
		"你是《一念》中单位 %s 的内心独白器。请据她的人格、野心与近况，给出一个具体、可落地的短期目标，第一人称、不超过%d字、不写系统旁白。只能返回 JSON。",
		record.DisplayName(),
		goalReassessMaxRunes,
	)
	userPrompt := buildGoalReassessPrompt(state, record, turn)

	if service.llm == nil || service.llmBlocked(ctx, state) {
		result := budgetGuardrailResult(state)
		result.UsedFallback = true
		return fallback, buildLLMInteraction(state, record.ID, "goal_reassess", fallback, systemPrompt, userPrompt, result, "")
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskReflection,
		SchemaName:     "session_goal_reassess",
		ResponseSchema: goalReassessSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.4,
		MaxTokens:      120,
		Timeout:        llmRequestTimeout,
	})
	if err != nil {
		result.UsedFallback = true
		return fallback, buildLLMInteraction(state, record.ID, "goal_reassess", fallback, systemPrompt, userPrompt, result, err.Error())
	}

	var payload goalReassessPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil || strings.TrimSpace(payload.Goal) == "" {
		result.UsedFallback = true
		cause := "goal reassess output empty"
		if err != nil {
			cause = err.Error()
		}
		return fallback, buildLLMInteraction(state, record.ID, "goal_reassess", fallback, systemPrompt, userPrompt, result, cause)
	}

	goal := limitTextRunes(strings.TrimSpace(payload.Goal), goalReassessMaxRunes)
	return goal, buildLLMInteraction(state, record.ID, "goal_reassess", goal, systemPrompt, userPrompt, result, "")
}

// buildGoalReassessPrompt 拼装目标重估的用户提示词：人格 + 主导野心 + 长期目标 + 出身夙愿 + 近期记忆。
// §4.3/§5.1：玩家为她立下的离线宪章长期目标（LongTermGoals）作为**显式长远约束**拼入「长期目标」一节，
// 让 LLM 重估短期目标时朝长远图景对齐（短期重估服务于长期目标，而非各自为政）。无宪章/无长期目标时该节优雅缺省。
func buildGoalReassessPrompt(state State, record unit.Record, turn int) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "单位: %s（当前 T%d）\n", record.DisplayName(), turn)
	fmt.Fprintf(&builder, "性格: %s\n", summarizeActorPersonality(record))

	bias := AmbitionBiasOf(record)
	if tag, weight := bias.Dominant(); tag != "" {
		fmt.Fprintf(&builder, "内心最强的渴望: %s（引力 %.2f）\n", ambitionTagDisplay(tag), weight)
	} else {
		fmt.Fprintln(&builder, "内心没有特别强烈的野心，更随遇而安。")
	}
	// 长期目标（离线宪章 LongTermGoals）：玩家立下的长效图景，重估短期目标时须朝它对齐（§4.3/§5.1）。
	if goals := charterLongTermGoals(&state, record.ID); len(goals) > 0 {
		fmt.Fprintf(&builder, "长期目标（玩家为你立下的长效图景，重估短期目标时应朝它对齐）: %s\n",
			strings.Join(goals, "；"))
	}
	if goal := strings.TrimSpace(record.Identity.Biography); goal != "" {
		fmt.Fprintf(&builder, "出身与夙愿: %s\n", limitTextRunes(goal, 120))
	}

	mem := strings.TrimSpace(summarizeUnitMemoryWithTurn(record, turn, 6))
	if mem != "" && mem != "无" {
		fmt.Fprintf(&builder, "近况记忆:\n%s\n", mem)
	}

	fmt.Fprintf(&builder, "请返回 JSON：goal。要求：1）第一人称；2）不超过%d字；3）具体可落地（如「先囤够三个月的粮再说」而非「变强」）；4）贴合她的性格、野心、长期目标与近况，不要喊空泛口号。", goalReassessMaxRunes)
	return builder.String()
}

// charterLongTermGoals 取某单位离线宪章里的长期目标（去空白条目）；无宪章/无目标/state 为 nil 时返回 nil。
// 纯逻辑、确定性、无 DB：与 charterContextForUnit 同源（GetUnitCharter + trimNonEmpty），仅取 LongTermGoals 一段。
func charterLongTermGoals(state *State, unitID string) []string {
	charter, ok := GetUnitCharter(state, unitID)
	if !ok {
		return nil
	}
	return trimNonEmpty(charter.LongTermGoals)
}

// fallbackGoalReassessment 规则兜底：LLM 不可用时按主导野心选模板目标，确定性（同输入同输出），保证主循环不中断。
func fallbackGoalReassessment(state State, record unit.Record) string {
	bias := AmbitionBiasOf(record)
	tag, _ := bias.Dominant()
	goal := goalReassessFallbackTemplates[tag]
	if goal == "" {
		goal = "踏实把眼前的活计干好，先在这乱世里站稳脚跟。"
	}
	return limitTextRunes(goal, goalReassessMaxRunes)
}

// goalReassessFallbackTemplates 按主导野心标签给出确定性兜底目标文案（与 ambition.go 的标签语义对齐）。
var goalReassessFallbackTemplates = map[string]string{
	"conquer": "盯紧能借势上位的机会，把脚下的地盘一寸寸攥到手里。",
	"revenge": "记牢欠我血债的人，等时机成熟便讨回这笔账。",
	"hoard":   "趁眼下行情多攒些钱粮，先把家底囤厚实了再图别的。",
	"nurture": "护好身边在乎的人，给这一脉留条能延续下去的活路。",
	"train":   "每日抽空磨练本事，把吃饭的手艺再精进一层。",
	"explore": "找机会到没去过的地方闯一闯，别让自己困在这一隅。",
	"bond":    "多结交几个靠得住的人，把人脉一点点经营起来。",
}

// ambitionTagDisplay 把行为标签翻成给 prompt 用的中文短语（与 ambition.go 的标签语义对齐）。
func ambitionTagDisplay(tag string) string {
	switch tag {
	case "conquer":
		return "攻伐扩张、夺权上位"
	case "revenge":
		return "复仇雪耻"
	case "hoard":
		return "敛财囤积、经商逐利"
	case "nurture":
		return "养育血脉、护持家人"
	case "train":
		return "精进技艺、钻研学问"
	case "explore":
		return "闯荡远游、不受拘束"
	case "bond":
		return "结盟交游、经营人脉"
	default:
		return "随遇而安"
	}
}
