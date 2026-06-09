package session

// 文件说明：集中定义会话内各类 LLM 载荷结构、schema 与主决策调用入口（行动、对话、反思等）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const (
	llmRequestTimeout      = 180 * time.Second
	llmBubbleRuneLimit     = 16
	llmSpeakPromptLimit    = 12
	llmMemoryRuneLimit     = 60
	llmNextActionRuneLimit = 12
)

// completionClient 接口定义该模块需要实现的能力约束。
type completionClient interface {
	GenerateJSON(context.Context, ai.CompletionRequest) (ai.CompletionResult, error)
}

// batchCompletionClient 接口定义该模块需要实现的能力约束。
type batchCompletionClient interface {
	GenerateJSONBatch(context.Context, []ai.BatchRequest, ai.BatchOptions) []ai.BatchResult
}

// unitDecisionChoicePayload 结构体用于承载该模块的核心数据。
type unitDecisionChoicePayload struct {
	Action        DecisionAction     `json:"action,omitempty"`
	Activity      ProductionActivity `json:"activity,omitempty"`
	SkillID       string             `json:"skill_id,omitempty"`
	TradeKind     TradeActionKind    `json:"trade_kind,omitempty"`
	ItemID        string             `json:"item_id,omitempty"`
	ItemName      string             `json:"item_name,omitempty"`
	OtherItemID   string             `json:"other_item_id,omitempty"`
	Price         int                `json:"price,omitempty"`
	GoldAmount    int                `json:"gold_amount,omitempty"`
	GroundLootID  string             `json:"ground_loot_id,omitempty"`
	StructureID   string             `json:"structure_id,omitempty"`
	StructureType StructureType      `json:"structure_type,omitempty"`
	TargetUnitID  string             `json:"target_unit_id,omitempty"`
	TargetQ       int                `json:"target_q,omitempty"`
	TargetR       int                `json:"target_r,omitempty"`
	NextAction    string             `json:"next_action,omitempty"`
	Speak         string             `json:"speak,omitempty"`
	Memory        string             `json:"memory,omitempty"`
	Knowledge     string             `json:"knowledge,omitempty"`
	Reasoning     string             `json:"reasoning"`
	// Attribution 可选：「她为什么这么选」的结构化归因，经 engine/decision 校验「意外但合理」。
	Attribution *attributionPayload `json:"attribution,omitempty"`
}

// unitDecisionPayload 结构体用于承载该模块的核心数据。
type unitDecisionPayload struct {
	Action        DecisionAction     `json:"action"`
	Activity      ProductionActivity `json:"activity,omitempty"`
	SkillID       string             `json:"skill_id,omitempty"`
	TradeKind     TradeActionKind    `json:"trade_kind,omitempty"`
	ItemID        string             `json:"item_id,omitempty"`
	ItemName      string             `json:"item_name,omitempty"`
	OtherItemID   string             `json:"other_item_id,omitempty"`
	Price         int                `json:"price,omitempty"`
	GoldAmount    int                `json:"gold_amount,omitempty"`
	GroundLootID  string             `json:"ground_loot_id,omitempty"`
	Memory        string             `json:"memory,omitempty"`
	Knowledge     string             `json:"knowledge,omitempty"`
	StructureID   string             `json:"structure_id,omitempty"`
	StructureType StructureType      `json:"structure_type,omitempty"`
	TargetUnitID  string             `json:"target_unit_id,omitempty"`
	TargetQ       int                `json:"target_q,omitempty"`
	TargetR       int                `json:"target_r,omitempty"`
	NextAction    string             `json:"next_action,omitempty"`
	Speak         string             `json:"speak,omitempty"`
	Reasoning     string             `json:"reasoning"`
}

// dialogueReplyPayload 结构体用于承载该模块的核心数据。
type dialogueReplyPayload struct {
	Reply  string `json:"reply"`
	Mood   string `json:"mood"`
	Intent string `json:"intent"`
	Memory string `json:"memory,omitempty"`
}

// unitReflectionPayload 结构体用于承载该模块的核心数据。
type unitReflectionPayload struct {
	Bubble string `json:"bubble"`
	Memory string `json:"memory,omitempty"`
}

// deploymentChoicePayload 结构体用于承载该模块的核心数据。
type deploymentChoicePayload struct {
	CandidateID string `json:"candidate_id"`
	LeftLine    string `json:"left_line,omitempty"`
	RightLine   string `json:"right_line,omitempty"`
	LeftMemory  string `json:"left_memory,omitempty"`
	RightMemory string `json:"right_memory,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Reasoning   string `json:"reasoning"`
}

// upkeepChoicePayload 结构体用于承载该模块的核心数据。
type upkeepChoicePayload struct {
	ShouldAct bool   `json:"should_act"`
	Bubble    string `json:"bubble,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Reasoning string `json:"reasoning"`
}

// combatShakeChoicePayload 结构体用于承载该模块的核心数据。
type combatShakeChoicePayload struct {
	Action    string `json:"action"`
	Bubble    string `json:"bubble,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Reasoning string `json:"reasoning"`
}

// decisionCandidate 结构体用于承载该模块的核心数据。
type decisionCandidate struct {
	ID            string
	Action        DecisionAction
	Activity      ProductionActivity
	SkillID       string
	TradeKind     TradeActionKind
	ItemID        string
	ItemName      string
	OtherItemID   string
	Price         int
	GoldAmount    int
	GroundLootID  string
	APCost        int
	StructureID   string
	StructureType StructureType
	TargetUnitID  string
	TargetQ       int
	TargetR       int
	Summary       string
}

var dialogueReplySchema = []byte(`{
  "type":"object",
  "properties":{
    "reply":{"type":"string","minLength":1},
    "mood":{"type":"string"},
    "intent":{"type":"string"},
    "memory":{"type":"string"}
  },
  "required":["reply"],
  "additionalProperties":false
}`)

var unitReflectionSchema = []byte(`{
  "type":"object",
  "properties":{
    "bubble":{"type":"string","minLength":1},
    "memory":{"type":"string"}
  },
  "required":["bubble"],
  "additionalProperties":false
}`)

var upkeepChoiceSchema = []byte(`{
  "type":"object",
  "properties":{
    "should_act":{"type":"boolean"},
    "bubble":{"type":"string"},
    "memory":{"type":"string"},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["should_act","reasoning"],
  "additionalProperties":false
}`)

var combatShakeChoiceSchema = []byte(`{
  "type":"object",
  "properties":{
    "action":{"type":"string","enum":["continue","retreat","surrender","rage"]},
    "bubble":{"type":"string","minLength":1},
    "memory":{"type":"string","minLength":1},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["action","bubble","memory","reasoning"],
  "additionalProperties":false
}`)

// generateUnitDecision 生成单位在执行阶段的动作决策，并返回完整交互轨迹。
func (service *Service) generateUnitDecision(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	remainingAP int,
	defiant bool,
) (unitDecisionPayload, ai.CompletionResult, LLMInteraction, error) {
	// 反射真短路（降本，flag-gated）：日常安静 tick（反射层 NeedsLLM=false 且 hold/continue）零成本落地、跳过 LLM
	// 含 prompt 构造（省下 memory/relation 摘要的 DB 查询）。安全反射（HP 危急撤退）仍交 LLM。默认关时为纯影子统计。
	if service.reflexShortCircuit && reflexShortCircuitApplies(state, actor, targetIDs) {
		reflexTotal.Add(1)
		reflexCouldSkip.Add(1)
		reflexShortCircuited.Add(1)
		payload := budgetGuardrailDecision(actor) // 保守待命/继续目标——安静 tick 无可决之事
		result := reflexShortCircuitResult()
		return payload, result, buildLLMInteraction(state, actor.ID, "decision", summarizeDecision(byID, payload), "", "", result, "reflex_shortcircuit"), nil
	}

	systemPrompt := unitDecisionSystemPrompt()
	candidates := buildDecisionCandidates(state, byID, actor, targetIDs, remainingAP)
	memorySummary := service.memorySummaryForPrompt(ctx, state, byID, *actor, 10)
	relationSummary := service.relationSummaryForPrompt(ctx, byID, *actor, 4)
	knowledgeSummary := service.knowledgeSummaryForPrompt(ctx, actor.ID, 6)
	userPrompt := buildDecisionPrompt(
		state,
		byID,
		actor,
		targetIDs,
		candidates,
		remainingAP,
		memorySummary,
		relationSummary,
		knowledgeSummary,
		defiant,
	)
	if err := validateNoLegacyDecisionPrompt(userPrompt); err != nil {
		result := ai.CompletionResult{Debug: ai.CompletionDebug{FallbackCause: err.Error()}}
		return unitDecisionPayload{}, result, buildLLMInteraction(state, actor.ID, "decision", "", systemPrompt, userPrompt, result, err.Error()), err
	}
	// 归因上下文：暴露可引用的记忆 ID，并拿到绑定完整快照（人格/压力/记忆/关系）的校验闭包。
	attrPromptBlock, validateAttribution := service.prepareAttribution(ctx, state, actor)
	if attrPromptBlock != "" {
		userPrompt = userPrompt + "\n" + attrPromptBlock
	}
	if service.llmBlocked(ctx, state) {
		decision := budgetGuardrailDecision(actor)
		result := budgetGuardrailResult(state)
		return decision, result, buildLLMInteraction(state, actor.ID, "decision", summarizeDecision(byID, decision), systemPrompt, userPrompt, result, ""), nil
	}
	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{
				FallbackCause: err.Error(),
			},
		}
		return unitDecisionPayload{}, result, buildLLMInteraction(state, actor.ID, "decision", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	// 反射层影子：统计这次本可被反射层零成本短路、本可省下的 LLM 调用（不改变行为）。
	recordReflexShadow(state, actor, targetIDs)

	responseSchema := buildUnitDecisionSchema(candidates)
	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskUnitDecision,
		SchemaName:     "session_unit_decision",
		ResponseSchema: responseSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.35,
		MaxTokens:      220,
		Timeout:        llmRequestTimeout,
		Metadata:       sessionLLMMetadata(state, actor.ID),
		Cacheable:      true, // 单位决策高频重复情境：相同 prompt 复用、跳过 LLM（§11.2 降本，缓存 flag-gated；命中仍对当前状态 re-validate）
	})
	if err != nil {
		cause := fmt.Errorf("unit decision generation failed: %w", err)
		return confusedUnitDecision(state, actor, byID, systemPrompt, userPrompt, result, cause)
	}

	choice, err := decodeUnitDecisionChoice(result.Output)
	if err != nil {
		cause := fmt.Errorf("decode unit decision payload: %w", err)
		return confusedUnitDecision(state, actor, byID, systemPrompt, userPrompt, result, cause)
	}

	choice = normalizeDecisionChoice(choice)
	decision, err := resolveDecisionChoiceWithState(state, byID, actor, targetIDs, remainingAP, candidates, choice)
	if err != nil {
		return confusedUnitDecision(state, actor, byID, systemPrompt, userPrompt, result, err)
	}
	if err := validateDecision(state, byID, actor, targetIDs, decision, remainingAP); err != nil {
		cause := fmt.Errorf("invalid unit decision: %w", err)
		return confusedUnitDecision(state, actor, byID, systemPrompt, userPrompt, result, cause)
	}

	// 归因校验（engine/decision，「意外但合理」的代码强制，设计宪法 §5）。
	// 默认影子模式：仅计入 OOC 遥测，不阻断决策；开启强制后，无源戏剧性归因优雅回退安全决策。
	if ok, reason, present := validateAttribution(choice); present {
		recordAttributionResult(ok)
		if !ok && service.enforcementActive() {
			fallback := oocFallbackDecision()
			message := "attribution_ooc:" + reason
			// §5.2：OOC 优雅回退时落一条可追溯审计行（events 表 append-only，best-effort 不阻断主链路）——
			// 此前 OOC 仅增进程级内存计数(AttributionStats)、重启即丢、无单条溯源。
			if service.db != nil {
				_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
					SessionID:   state.ID,
					OwnerUnitID: actor.ID,
					Code:        events.ReasonOOCRejected,
					Category:    events.CategoryLifecycle,
					Payload:     map[string]any{"reason": reason, "turn": state.TurnState.Turn},
					WorldID:     state.WorldID,
					// F4 H1：补 Tick = 执行回合（OOC 在执行期产生）。漏设则恒落 tick=0，致边界道德漂移按执行回合
					// 查询时这条 OOC→Freedom 信号永不命中（与战斗杀伤主信号同病）。
					Tick: state.TurnState.Turn,
				})
			}
			return fallback, result, buildLLMInteraction(state, actor.ID, "decision", summarizeDecision(byID, fallback), systemPrompt, userPrompt, result, message), nil
		}
	}

	// 门控意外（engine/decision.GateSurprise，设计宪法 §5.3 红线）：突然恋爱/卖传家宝/叛变即使归因看似合理，
	// 也必须有专属前因（关系积累/重压/忠诚崩坏）才允许落地；不足则优雅回退安全决策，绝不让无源戏剧性转折发生。
	// 与归因强制同档（仅 enforcementActive 时阻断）；非门控动作零影响，影子模式下只计遥测不阻断。
	if gated, isGated := gatedActionForChoice(actor, choice); isGated {
		gate := service.evaluateSurpriseGate(ctx, actor, choice, gated)
		surpriseGateTotal.Add(1)
		// 注意：本函数内局部变量 `decision`（unitDecisionPayload）遮蔽了 decision 包，故经包级 helper surpriseGateAllowed 比较。
		if !surpriseGateAllowed(gate) {
			surpriseGateBlocked.Add(1)
			if service.enforcementActive() {
				fallback := oocFallbackDecision()
				message := fmt.Sprintf("surprise_gate_block:%s:%s", gated, gate.Reason)
				return fallback, result, buildLLMInteraction(state, actor.ID, "decision", summarizeDecision(byID, fallback), systemPrompt, userPrompt, result, message), nil
			}
		}
	}

	// §8 一致性收紧旋钮（玩家高主观 OOC → ConsistencyTightened latch）：收紧态下把「突然戏剧性动作」允许的最高
	// SurpriseLevel 从 3 压到 TightenedSurpriseCap()（收紧时=2）——LLM 自评 surprise_level ≥cap 的大转折（突然恋爱/
	// 卖传家宝/叛变之外的任意高惊喜动作）在玩家正觉得「太离谱」时优雅回退安全决策（继续待命），先稳住一致性。
	// 与归因/门控同档：仅 enforcementActive 时才真正阻断，影子模式只计遥测不改行为；未收紧时 cap=3、choice 上限 3，恒不触发（零行为）。
	// flag 默认关：QUNXIANG_CONSISTENCY_TIGHTEN 未开 → ConsistencyTightened 永假 → cap 恒 3 → 本块零行为。
	if choice.Attribution != nil {
		surpriseCap := TightenedSurpriseCap()
		if surpriseCap < 3 && choice.Attribution.SurpriseLevel >= surpriseCap {
			surpriseCapTotal.Add(1)
			if service.enforcementActive() {
				surpriseCapDeferred.Add(1)
				fallback := oocFallbackDecision()
				message := fmt.Sprintf("surprise_cap_defer:level=%d:cap=%d", choice.Attribution.SurpriseLevel, surpriseCap)
				return fallback, result, buildLLMInteraction(state, actor.ID, "decision", summarizeDecision(byID, fallback), systemPrompt, userPrompt, result, message), nil
			}
		}
	}

	// 回响 Echo：若这次选择被归因到一条真实玩家动作，生成「因为你上次…，这一回…」回响卡进收件箱（best-effort）。
	// SurfaceEcho 会再次核验 ref 真实存在，绝不编造前因（宪法 §6.2）。
	if ref, narrative, has := echoRefFromAttribution(choice.Attribution); has {
		if narrative == "" {
			narrative = strings.TrimSpace(choice.Speak)
		}
		if narrative == "" {
			narrative = "她做了和你上次引导不太一样的选择"
		}
		_, _ = service.SurfaceEcho(ctx, state.ID, actor.ID, ref, narrative, 0)
	}

	// 内容安全审核（AI 输出侧，flag QUNXIANG_CONTENT_SAFETY 默认关→放行）：单位决策的玩家可见对白（speak）
	// 被判违规则替换占位，并脱敏 result——堵死「双向」审核的输出侧盲点（generateUnitDecision 的 speak 此前完全未审）。
	if strings.TrimSpace(decision.Speak) != "" {
		if v := service.ModerateText(ctx, decision.Speak, "output"); !v.Allowed {
			decision.Speak = unsafeSpeakPlaceholder
			result = redactCompletionResultOutput(result, unsafeSpeakPlaceholder)
		}
	}

	// §5.4：暂存本次 LLM 决策的归因因果句（经 §5 校验后才可信），供同一执行流内该单位造成的命运卡取「当次因果句」。
	if choice.Attribution != nil {
		service.rememberDecisionNarrative(actor.ID, choice.Attribution.NarrativeZH)
	}

	return decision, result, buildLLMInteraction(state, actor.ID, "decision", summarizeDecision(byID, decision), systemPrompt, userPrompt, result, ""), nil
}

// rememberDecisionNarrative 暂存某单位本次 LLM 决策的归因因果句（NarrativeZH），供同一执行流内由该单位造成的
// 命运卡（如 WorldizeDeath 的死亡卡）取用「当次 LLM 因果句」而非启发式模板（§5.4）。空值不存。
func (service *Service) rememberDecisionNarrative(unitID, narrative string) {
	narrative = strings.TrimSpace(narrative)
	if service == nil || unitID == "" || narrative == "" {
		return
	}
	service.decisionNarrativeMu.Lock()
	if service.decisionNarrative == nil {
		service.decisionNarrative = make(map[string]string)
	}
	service.decisionNarrative[unitID] = narrative
	service.decisionNarrativeMu.Unlock()
}

// recallDecisionNarrative 取某单位最近一次暂存的归因因果句；无则返回 ""。
func (service *Service) recallDecisionNarrative(unitID string) string {
	if service == nil || unitID == "" {
		return ""
	}
	service.decisionNarrativeMu.Lock()
	defer service.decisionNarrativeMu.Unlock()
	return service.decisionNarrative[unitID]
}

func confusedUnitDecision(
	state State,
	actor *unit.Record,
	byID map[string]*unit.Record,
	systemPrompt string,
	userPrompt string,
	result ai.CompletionResult,
	cause error,
) (unitDecisionPayload, ai.CompletionResult, LLMInteraction, error) {
	debugID := "D" + strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
	bubble := "叽哩咕噜#" + debugID
	causeText := "unit decision confused"
	if cause != nil {
		causeText = cause.Error()
	}
	errorMessage := fmt.Sprintf("confused_decision_id=%s: %s", debugID, causeText)
	if strings.TrimSpace(result.Debug.FallbackCause) == "" {
		result.Debug.FallbackCause = errorMessage
	}
	decision := unitDecisionPayload{
		Action:     DecisionActionHold,
		NextAction: bubble,
		Speak:      bubble,
		Memory:     "我刚才一阵迷糊，没能判断下一步。",
		Reasoning:  bubble,
	}
	return decision, result, buildLLMInteraction(state, actor.ID, "decision", summarizeDecision(byID, decision), systemPrompt, userPrompt, result, errorMessage), nil
}

func decodeUnitDecisionChoice(raw json.RawMessage) (unitDecisionChoicePayload, error) {
	var choice unitDecisionChoicePayload
	normalized, err := preserveNonNullDuplicateJSONFields(raw)
	if err != nil {
		return choice, err
	}
	if err := json.Unmarshal(normalized, &choice); err != nil {
		return choice, err
	}
	return choice, nil
}

func preserveNonNullDuplicateJSONFields(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return raw, nil
	}

	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key, got %T", token)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if existing, exists := fields[key]; exists && isJSONNull(value) && !isJSONNull(existing) {
			continue
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	normalized, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return strings.EqualFold(strings.TrimSpace(string(raw)), "null")
}

// generateDialogueReply 生成单位对玩家输入的对话回复。
func (service *Service) generateDialogueReply(
	ctx context.Context,
	state State,
	record unit.Record,
	playerMessage string,
	byID map[string]*unit.Record,
) (dialogueReplyPayload, ai.CompletionResult, LLMInteraction, error) {
	systemPrompt := dialogueSystemPrompt(record)
	memorySummary := service.memorySummaryForPrompt(ctx, state, byID, record, 8)
	relationSummary := service.relationSummaryForPrompt(ctx, byID, record, 4)
	knowledgeSummary := service.knowledgeSummaryForPrompt(ctx, record.ID, 4)
	userPrompt := buildDialoguePrompt(
		state,
		byID,
		record,
		playerMessage,
		memorySummary,
		relationSummary,
		knowledgeSummary,
	)
	if service.llmBlocked(ctx, state) {
		reply := budgetGuardrailDialogueReply()
		result := budgetGuardrailResult(state)
		return reply, result, buildLLMInteraction(state, record.ID, "dialogue", reply.Reply, systemPrompt, userPrompt, result, ""), nil
	}
	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{
				FallbackCause: err.Error(),
			},
		}
		return dialogueReplyPayload{}, result, buildLLMInteraction(state, record.ID, "dialogue", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskDialogue,
		SchemaName:     "session_dialogue_reply",
		ResponseSchema: dialogueReplySchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.75,
		MaxTokens:      220,
		Metadata:       sessionLLMMetadata(state, record.ID),
	})
	if err != nil {
		return dialogueReplyPayload{}, result, buildLLMInteraction(state, record.ID, "dialogue", "", systemPrompt, userPrompt, result, err.Error()), fmt.Errorf("dialogue generation failed: %w", err)
	}

	var reply dialogueReplyPayload
	if err := json.Unmarshal(result.Output, &reply); err != nil || strings.TrimSpace(reply.Reply) == "" {
		cause := "dialogue reply is empty"
		if err != nil {
			cause = fmt.Sprintf("decode dialogue payload: %v", err)
		}
		return dialogueReplyPayload{}, result, buildLLMInteraction(state, record.ID, "dialogue", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}

	reply.Reply = strings.TrimSpace(reply.Reply)
	// 内容安全审核（AI 输出侧，flag QUNXIANG_CONTENT_SAFETY 默认关→放行）：玩家可见的 AI 对白被判不安全则替换为安全占位，
	// 并脱敏 result（Output/RawOutput）——否则原始违规文本仍会经 LLMInteraction 的 ParsedOutput/RawOutput 广播外泄。
	replyBlocked := false
	if v := service.ModerateText(ctx, reply.Reply, "output"); !v.Allowed {
		reply.Reply = unsafeDialoguePlaceholder
		result = redactCompletionResultOutput(result, unsafeDialoguePlaceholder)
		replyBlocked = true
	}
	reply.Memory = limitTextRunes(strings.TrimSpace(reply.Memory), 18)
	// Memory 也是 LLM 输出，会落库并复现进未来 prompt/快照：主回复被拦时不保留原 Memory；非空 Memory 自身违规也丢弃。
	if replyBlocked {
		reply.Memory = ""
	} else if reply.Memory != "" {
		if v := service.ModerateText(ctx, reply.Memory, "output"); !v.Allowed {
			reply.Memory = ""
		}
	}
	if reply.Memory == "" {
		reply.Memory = limitTextRunes(reply.Reply, 18)
	}
	return reply, result, buildLLMInteraction(state, record.ID, "dialogue", reply.Reply, systemPrompt, userPrompt, result, ""), nil
}

// requestUnitDialogueReply 为单位间主动交流生成目标单位的即时回应。
func (service *Service) requestUnitDialogueReply(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	target *unit.Record,
	actorLine string,
) (dialogueReplyPayload, ai.CompletionResult, LLMInteraction, bool) {
	if target == nil {
		result := ai.CompletionResult{Provider: "rules", Model: "missing_target", UsedFallback: true}
		payload := dialogueReplyPayload{Reply: "我先不回应。", Mood: "guarded", Intent: "ignore", Memory: "我错过了一次交谈。"}
		return payload, result, buildLLMInteraction(state, "", "unit_dialogue_reply", payload.Reply, "", "", result, "target is nil"), false
	}
	systemPrompt := unitDialogueReplySystemPrompt(*target)
	memorySummary := service.memorySummaryForPrompt(ctx, state, byID, *target, 8)
	relationSummary := service.relationSummaryForPrompt(ctx, byID, *target, 4)
	knowledgeSummary := service.knowledgeSummaryForPrompt(ctx, target.ID, 4)
	userPrompt := buildUnitDialogueReplyPrompt(state, byID, actor, target, actorLine, memorySummary, relationSummary, knowledgeSummary)
	if service.llmBlocked(ctx, state) {
		payload := dialogueReplyPayload{Reply: "我听到了，按局势来。", Mood: "steady", Intent: "acknowledge", Memory: "我回应了队友交谈。"}
		result := budgetGuardrailResult(state)
		return payload, result, buildLLMInteraction(state, target.ID, "unit_dialogue_reply", payload.Reply, systemPrompt, userPrompt, result, ""), true
	}
	if service == nil || service.llm == nil {
		payload := fallbackUnitDialogueReplyPayload(target, actorLine)
		result := ai.CompletionResult{Provider: "rules", Model: "fallback", UsedFallback: true}
		return payload, result, buildLLMInteraction(state, target.ID, "unit_dialogue_reply", payload.Reply, systemPrompt, userPrompt, result, ""), true
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskDialogue,
		SchemaName:     "session_unit_dialogue_reply",
		ResponseSchema: dialogueReplySchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.65,
		MaxTokens:      180,
		Timeout:        llmRequestTimeout,
		Metadata:       sessionLLMMetadata(state, target.ID),
	})
	if err != nil {
		payload := fallbackUnitDialogueReplyPayload(target, actorLine)
		payload.Reply = "我这会儿先记下。"
		result.UsedFallback = true
		return payload, result, buildLLMInteraction(state, target.ID, "unit_dialogue_reply", payload.Reply, systemPrompt, userPrompt, result, err.Error()), false
	}

	var payload dialogueReplyPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil || strings.TrimSpace(payload.Reply) == "" {
		payload = fallbackUnitDialogueReplyPayload(target, actorLine)
		result.UsedFallback = true
		cause := "unit dialogue reply is empty"
		if err != nil {
			cause = fmt.Sprintf("decode unit dialogue reply payload: %v", err)
		}
		return payload, result, buildLLMInteraction(state, target.ID, "unit_dialogue_reply", payload.Reply, systemPrompt, userPrompt, result, cause), false
	}
	payload = normalizeDialogueReplyPayload(payload)
	return payload, result, buildLLMInteraction(state, target.ID, "unit_dialogue_reply", payload.Reply, systemPrompt, userPrompt, result, ""), true
}

func normalizeDialogueReplyPayload(payload dialogueReplyPayload) dialogueReplyPayload {
	payload.Reply = limitTextRunes(strings.TrimSpace(payload.Reply), 36)
	payload.Mood = limitTextRunes(strings.TrimSpace(payload.Mood), 16)
	payload.Intent = limitTextRunes(strings.TrimSpace(payload.Intent), 18)
	payload.Memory = limitTextRunes(strings.TrimSpace(payload.Memory), 24)
	if payload.Memory == "" {
		payload.Memory = limitTextRunes(payload.Reply, 24)
	}
	return payload
}

func fallbackUnitDialogueReplyPayload(target *unit.Record, actorLine string) dialogueReplyPayload {
	reply := "我听到了，按局势来。"
	if target != nil {
		reply = fmt.Sprintf("%s：我听到了，按局势来。", target.DisplayName())
	}
	memory := strings.TrimSpace(actorLine)
	if memory != "" {
		memory = "我听到：" + memory
	}
	return normalizeDialogueReplyPayload(dialogueReplyPayload{
		Reply:  reply,
		Mood:   "steady",
		Intent: "acknowledge",
		Memory: firstNonEmptyText(memory, "我回应了一次交谈。"),
	})
}

// generateUnitReflection 为单位生成头顶气泡与短记忆文本。
func (service *Service) generateUnitReflection(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	eventSummary string,
	interactionKind string,
) (unitReflectionPayload, ai.CompletionResult, LLMInteraction, error) {
	systemPrompt := reflectionSystemPrompt(record)
	memorySummary := service.memorySummaryForPrompt(ctx, state, byID, record, 8)
	relationSummary := service.relationSummaryForPrompt(ctx, byID, record, 4)
	knowledgeSummary := service.knowledgeSummaryForPrompt(ctx, record.ID, 4)
	userPrompt := buildReflectionPrompt(
		state,
		byID,
		record,
		eventSummary,
		memorySummary,
		relationSummary,
		knowledgeSummary,
	)
	if service.llmBlocked(ctx, state) {
		payload := budgetGuardrailReflection(eventSummary)
		result := budgetGuardrailResult(state)
		return payload, result, buildLLMInteraction(state, record.ID, interactionKind, payload.Bubble, systemPrompt, userPrompt, result, ""), nil
	}
	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{
				FallbackCause: err.Error(),
			},
		}
		return unitReflectionPayload{}, result, buildLLMInteraction(state, record.ID, interactionKind, "", systemPrompt, userPrompt, result, err.Error()), err
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskReflection,
		SchemaName:     "session_unit_reflection",
		ResponseSchema: unitReflectionSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.7,
		MaxTokens:      160,
		Timeout:        llmRequestTimeout,
		Metadata:       sessionLLMMetadata(state, record.ID),
	})
	if err != nil {
		return unitReflectionPayload{}, result, buildLLMInteraction(state, record.ID, interactionKind, "", systemPrompt, userPrompt, result, err.Error()), fmt.Errorf("unit reflection generation failed: %w", err)
	}

	var payload unitReflectionPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode unit reflection payload: %v", err)
		return unitReflectionPayload{}, result, buildLLMInteraction(state, record.ID, interactionKind, "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}

	payload = normalizeReflectionPayload(payload)
	if payload.Bubble == "" {
		cause := "unit reflection bubble is empty"
		return unitReflectionPayload{}, result, buildLLMInteraction(state, record.ID, interactionKind, "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}

	return payload, result, buildLLMInteraction(state, record.ID, interactionKind, payload.Bubble, systemPrompt, userPrompt, result, ""), nil
}

// generateDeploymentChoice 生成部署阶段双单位交易/协商决策。
func (service *Service) generateDeploymentChoice(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	candidates []deploymentCandidate,
) (deploymentChoicePayload, ai.CompletionResult, LLMInteraction, error) {
	systemPrompt := deploymentSystemPrompt(*left, *right)
	leftMemorySummary := service.memorySummaryForPrompt(ctx, state, byID, *left, 8)
	rightMemorySummary := service.memorySummaryForPrompt(ctx, state, byID, *right, 8)
	leftRelationSummary := service.relationSummaryForPrompt(ctx, byID, *left, 4)
	rightRelationSummary := service.relationSummaryForPrompt(ctx, byID, *right, 4)
	userPrompt := buildDeploymentPrompt(state, byID, left, right, candidates, leftMemorySummary, rightMemorySummary, leftRelationSummary, rightRelationSummary)
	if service.llmBlocked(ctx, state) {
		choice := budgetGuardrailDeploymentChoice()
		result := budgetGuardrailResult(state)
		return choice, result, buildLLMInteraction(state, left.ID, "unit_dialogue", deploymentChoiceInteractionSummary(choice), systemPrompt, userPrompt, result, ""), nil
	}
	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{FallbackCause: err.Error()},
		}
		return deploymentChoicePayload{}, result, buildLLMInteraction(state, left.ID, "unit_dialogue", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskDeployment,
		SchemaName:     "session_deployment_choice",
		ResponseSchema: buildDeploymentChoiceSchema(candidates),
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.6,
		MaxTokens:      260,
		Timeout:        llmRequestTimeout,
		Metadata:       sessionLLMMetadata(state, left.ID),
	})
	if err != nil {
		return deploymentChoicePayload{}, result, buildLLMInteraction(state, left.ID, "unit_dialogue", "", systemPrompt, userPrompt, result, err.Error()), fmt.Errorf("deployment choice generation failed: %w", err)
	}

	var choice deploymentChoicePayload
	if err := json.Unmarshal(result.Output, &choice); err != nil {
		cause := fmt.Sprintf("decode deployment choice payload: %v", err)
		return deploymentChoicePayload{}, result, buildLLMInteraction(state, left.ID, "unit_dialogue", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	choice = normalizeDeploymentChoice(choice)
	if choice.CandidateID == "" ||
		choice.Reasoning == "" ||
		choice.Summary == "" ||
		choice.LeftLine == "" ||
		choice.RightLine == "" ||
		choice.LeftMemory == "" ||
		choice.RightMemory == "" {
		cause := "deployment choice is incomplete"
		return deploymentChoicePayload{}, result, buildLLMInteraction(state, left.ID, "unit_dialogue", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}

	return choice, result, buildLLMInteraction(state, left.ID, "unit_dialogue", deploymentChoiceInteractionSummary(choice), systemPrompt, userPrompt, result, ""), nil
}

// deploymentChoiceInteractionSummary 汇总部署协商的关键输出，写入交互摘要。
func deploymentChoiceInteractionSummary(choice deploymentChoicePayload) string {
	summary := strings.TrimSpace(choice.Summary)
	if summary != "" {
		return summary
	}
	return strings.TrimSpace(choice.Reasoning)
}

// generateUpkeepChoice 生成单位日常生存动作（如进食）是否执行的判断。
func (service *Service) generateUpkeepChoice(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	eventLabel string,
) (upkeepChoicePayload, ai.CompletionResult, LLMInteraction, error) {
	systemPrompt := upkeepSystemPrompt(record)
	memorySummary := service.memorySummaryForPrompt(ctx, state, byID, record, 8)
	relationSummary := service.relationSummaryForPrompt(ctx, byID, record, 4)
	knowledgeSummary := service.knowledgeSummaryForPrompt(ctx, record.ID, 4)
	userPrompt := buildUpkeepPrompt(
		state,
		byID,
		record,
		eventLabel,
		memorySummary,
		relationSummary,
		knowledgeSummary,
	)
	if service.llmBlocked(ctx, state) {
		choice := budgetGuardrailUpkeepChoice()
		result := budgetGuardrailResult(state)
		return choice, result, buildLLMInteraction(state, record.ID, "reflection", choice.Bubble, systemPrompt, userPrompt, result, ""), nil
	}
	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{FallbackCause: err.Error()},
		}
		return upkeepChoicePayload{}, result, buildLLMInteraction(state, record.ID, "reflection", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskUpkeep,
		SchemaName:     "session_upkeep_choice",
		ResponseSchema: upkeepChoiceSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.55,
		MaxTokens:      180,
		Metadata:       sessionLLMMetadata(state, record.ID),
	})
	if err != nil {
		return upkeepChoicePayload{}, result, buildLLMInteraction(state, record.ID, "reflection", "", systemPrompt, userPrompt, result, err.Error()), fmt.Errorf("upkeep choice generation failed: %w", err)
	}

	var choice upkeepChoicePayload
	if err := json.Unmarshal(result.Output, &choice); err != nil {
		cause := fmt.Sprintf("decode upkeep choice payload: %v", err)
		return upkeepChoicePayload{}, result, buildLLMInteraction(state, record.ID, "reflection", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	choice = normalizeUpkeepChoice(choice)
	if choice.Reasoning == "" {
		cause := "upkeep reasoning must not be empty"
		return upkeepChoicePayload{}, result, buildLLMInteraction(state, record.ID, "reflection", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}

	summary := choice.Reasoning
	if choice.Bubble != "" {
		summary = choice.Bubble
	}
	return choice, result, buildLLMInteraction(state, record.ID, "reflection", summary, systemPrompt, userPrompt, result, ""), nil
}

// generateCombatShakeChoice 生成战斗应激快决策（continue/retreat/surrender/rage）。
func (service *Service) generateCombatShakeChoice(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	triggers []string,
) (combatShakeChoicePayload, ai.CompletionResult, LLMInteraction, error) {
	systemPrompt := combatShakeSystemPrompt(record)
	memorySummary := service.memorySummaryForPrompt(ctx, state, byID, record, 8)
	relationSummary := service.relationSummaryForPrompt(ctx, byID, record, 4)
	knowledgeSummary := service.knowledgeSummaryForPrompt(ctx, record.ID, 4)
	userPrompt := buildCombatShakePrompt(
		state,
		byID,
		record,
		triggers,
		memorySummary,
		relationSummary,
		knowledgeSummary,
	)
	// 未成年模式：给战斗高压情绪叙事追加分级指令（避免血腥/残肢/濒死等露骨描写），仅约束 LLM 措辞、不改战斗数值。
	// minorModeShakeDirective 定义于同包 combat_shake.go；state.MinorMode 关时返回空串、零影响。
	if directive := minorModeShakeDirective(state.MinorMode); directive != "" {
		systemPrompt += "\n" + directive
		userPrompt += "\n" + directive
	}
	if service.llmBlocked(ctx, state) {
		choice := budgetGuardrailCombatShakeChoice()
		result := budgetGuardrailResult(state)
		return choice, result, buildLLMInteraction(state, record.ID, "shake", choice.Bubble, systemPrompt, userPrompt, result, ""), nil
	}
	if service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{
			Debug: ai.CompletionDebug{FallbackCause: err.Error()},
		}
		return combatShakeChoicePayload{}, result, buildLLMInteraction(state, record.ID, "shake", "", systemPrompt, userPrompt, result, err.Error()), err
	}

	const shakeTimeout = llmRequestTimeout
	timeoutCtx, cancel := context.WithTimeout(ctx, shakeTimeout)
	defer cancel()

	result, err := service.llm.GenerateJSON(timeoutCtx, ai.CompletionRequest{
		Task:           ai.TaskReflection,
		SchemaName:     "session_combat_shake_choice",
		ResponseSchema: combatShakeChoiceSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.2,
		MaxTokens:      96,
		Timeout:        shakeTimeout,
		Metadata:       sessionLLMMetadata(state, record.ID),
	})
	if err != nil {
		return combatShakeChoicePayload{}, result, buildLLMInteraction(state, record.ID, "shake", "", systemPrompt, userPrompt, result, err.Error()), fmt.Errorf("combat shake generation failed: %w", err)
	}

	var choice combatShakeChoicePayload
	if err := json.Unmarshal(result.Output, &choice); err != nil {
		cause := fmt.Sprintf("decode combat shake payload: %v", err)
		return combatShakeChoicePayload{}, result, buildLLMInteraction(state, record.ID, "shake", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	choice = normalizeCombatShakeChoice(choice)
	if choice.Reasoning == "" {
		cause := "combat shake reasoning must not be empty"
		return combatShakeChoicePayload{}, result, buildLLMInteraction(state, record.ID, "shake", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	if choice.Bubble == "" {
		cause := "combat shake bubble must not be empty"
		return combatShakeChoicePayload{}, result, buildLLMInteraction(state, record.ID, "shake", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}
	if choice.Memory == "" {
		cause := "combat shake memory must not be empty"
		return combatShakeChoicePayload{}, result, buildLLMInteraction(state, record.ID, "shake", "", systemPrompt, userPrompt, result, cause), fmt.Errorf("%s", cause)
	}

	summary := choice.Reasoning
	if choice.Bubble != "" {
		summary = choice.Bubble
	}
	return choice, result, buildLLMInteraction(state, record.ID, "shake", summary, systemPrompt, userPrompt, result, ""), nil
}

// buildLLMInteraction 构建可审计的 LLM 交互记录（含提示词、输出与 token 成本）。
func buildLLMInteraction(
	state State,
	unitID string,
	kind string,
	summary string,
	systemPrompt string,
	userPrompt string,
	result ai.CompletionResult,
	errorMessage string,
) LLMInteraction {
	rawOutput := strings.TrimSpace(result.Debug.RawOutput)
	parsedOutput := prettyJSON(result.Output)
	if rawOutput == "" {
		rawOutput = parsedOutput
	}
	promptTokens, outputTokens, totalTokens := normalizeTokenUsage(
		result.Usage.PromptTokens,
		result.Usage.CompletionTokens,
		result.Usage.TotalTokens,
	)
	estimatedCost := estimateLLMCostUSD(result.Provider, result.Model, promptTokens, outputTokens)
	if result.CacheHit {
		// prompt 缓存命中是复用、无真实 LLM 花费：成本归零，避免重复计入会话预算护栏（llmBudgetGuardrailActive）
		// 与成本仪表盘。token 保留供遥测，仅成本计 0。
		estimatedCost = 0
	}

	return LLMInteraction{
		ID:            uuid.NewString(),
		UnitID:        unitID,
		Kind:          kind,
		Summary:       summary,
		SystemPrompt:  systemPrompt,
		UserPrompt:    userPrompt,
		ParsedOutput:  parsedOutput,
		RawOutput:     rawOutput,
		ErrorMessage:  strings.TrimSpace(errorMessage),
		FallbackCause: strings.TrimSpace(result.Debug.FallbackCause),
		Turn:          state.TurnState.Turn,
		Phase:         state.TurnState.Phase,
		OccurredAt:    time.Now().UTC(),
		Provider:      result.Provider,
		Model:         result.Model,
		UsedFallback:  result.UsedFallback,
		PromptTokens:  promptTokens,
		OutputTokens:  outputTokens,
		TotalTokens:   totalTokens,
		EstimatedCost: estimatedCost,
		Attempts:      append([]ai.CompletionAttempt{}, result.Debug.Attempts...),
	}
}

func sessionLLMMetadata(state State, unitID string) map[string]string {
	metadata := map[string]string{
		"session_id": strings.TrimSpace(state.ID),
		"turn":       fmt.Sprintf("%d", state.TurnState.Turn),
		"phase":      string(state.TurnState.Phase),
	}
	if trimmedUnitID := strings.TrimSpace(unitID); trimmedUnitID != "" {
		metadata["unit_id"] = trimmedUnitID
	}
	return metadata
}

// prettyJSON 把 JSON 输出格式化为可读字符串，失败时返回原文。
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return strings.TrimSpace(string(raw))
	}

	formatted, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return strings.TrimSpace(string(raw))
	}

	return string(formatted)
}

// normalizeDecisionChoice 统一裁剪字段长度，避免长文本撑坏前端展示。
func normalizeDecisionChoice(choice unitDecisionChoicePayload) unitDecisionChoicePayload {
	choice.Action = DecisionAction(strings.ToLower(strings.TrimSpace(string(choice.Action))))
	choice.Activity = ProductionActivity(strings.ToLower(strings.TrimSpace(string(choice.Activity))))
	choice.SkillID = strings.ToLower(strings.TrimSpace(choice.SkillID))
	choice.TradeKind = TradeActionKind(strings.ToLower(strings.TrimSpace(string(choice.TradeKind))))
	choice.ItemID = strings.ToLower(strings.TrimSpace(choice.ItemID))
	choice.ItemName = limitTextRunes(strings.TrimSpace(choice.ItemName), llmBubbleRuneLimit)
	choice.OtherItemID = strings.ToLower(strings.TrimSpace(choice.OtherItemID))
	choice.GroundLootID = strings.TrimSpace(choice.GroundLootID)
	choice.StructureID = strings.TrimSpace(choice.StructureID)
	choice.StructureType = StructureType(strings.ToLower(strings.TrimSpace(string(choice.StructureType))))
	choice.TargetUnitID = strings.TrimSpace(choice.TargetUnitID)
	choice.NextAction = limitTextRunes(strings.TrimSpace(choice.NextAction), llmNextActionRuneLimit)
	choice.Speak = strings.TrimSpace(choice.Speak)
	choice.Memory = strings.TrimSpace(choice.Memory)
	choice.Knowledge = strings.TrimSpace(choice.Knowledge)
	choice.Reasoning = strings.TrimSpace(choice.Reasoning)
	if choice.NextAction == "" {
		switch {
		case choice.Speak != "":
			choice.NextAction = limitTextRunes(choice.Speak, llmNextActionRuneLimit)
		case choice.Reasoning != "":
			choice.NextAction = limitTextRunes(choice.Reasoning, llmNextActionRuneLimit)
		}
	}
	if choice.Speak == "" {
		switch {
		case choice.NextAction != "":
			choice.Speak = choice.NextAction
		case choice.Reasoning != "":
			choice.Speak = strings.TrimSpace(choice.Reasoning)
		}
	}
	if choice.Memory == "" {
		switch {
		case choice.Speak != "":
			choice.Memory = strings.TrimSpace(choice.Speak)
		case choice.Reasoning != "":
			choice.Memory = strings.TrimSpace(choice.Reasoning)
		}
	}
	return choice
}

// normalizeReflectionPayload 规范化反思输出长度并补齐缺省字段。
func normalizeReflectionPayload(payload unitReflectionPayload) unitReflectionPayload {
	payload.Bubble = limitTextRunes(strings.TrimSpace(payload.Bubble), llmBubbleRuneLimit)
	payload.Memory = strings.TrimSpace(payload.Memory)
	if payload.Memory == "" {
		payload.Memory = strings.TrimSpace(payload.Bubble)
	}
	return payload
}

// normalizeDecision 规范化决策输出字段并填充展示兜底文本。
func normalizeDecision(decision unitDecisionPayload) unitDecisionPayload {
	decision.Action = DecisionAction(strings.ToLower(strings.TrimSpace(string(decision.Action))))
	decision.SkillID = strings.ToLower(strings.TrimSpace(decision.SkillID))
	decision.TradeKind = TradeActionKind(strings.ToLower(strings.TrimSpace(string(decision.TradeKind))))
	decision.ItemID = strings.ToLower(strings.TrimSpace(decision.ItemID))
	decision.ItemName = limitTextRunes(strings.TrimSpace(decision.ItemName), 16)
	decision.OtherItemID = strings.ToLower(strings.TrimSpace(decision.OtherItemID))
	decision.StructureID = strings.TrimSpace(decision.StructureID)
	decision.StructureType = StructureType(strings.ToLower(strings.TrimSpace(string(decision.StructureType))))
	decision.TargetUnitID = strings.TrimSpace(decision.TargetUnitID)
	decision.NextAction = limitTextRunes(strings.TrimSpace(decision.NextAction), llmNextActionRuneLimit)
	decision.Speak = strings.TrimSpace(decision.Speak)
	decision.Memory = strings.TrimSpace(decision.Memory)
	decision.Knowledge = strings.TrimSpace(decision.Knowledge)
	decision.Reasoning = strings.TrimSpace(decision.Reasoning)
	if decision.NextAction == "" {
		switch {
		case decision.Speak != "":
			decision.NextAction = limitTextRunes(decision.Speak, llmNextActionRuneLimit)
		case decision.Reasoning != "":
			decision.NextAction = limitTextRunes(decision.Reasoning, llmNextActionRuneLimit)
		}
	}
	if decision.Speak == "" {
		switch {
		case decision.NextAction != "":
			decision.Speak = decision.NextAction
		case decision.Reasoning != "":
			decision.Speak = strings.TrimSpace(decision.Reasoning)
		}
	}
	if decision.Memory == "" {
		switch {
		case decision.Speak != "":
			decision.Memory = strings.TrimSpace(decision.Speak)
		case decision.Reasoning != "":
			decision.Memory = strings.TrimSpace(decision.Reasoning)
		}
	}
	return decision
}

// limitTextRunes 按 rune 数裁剪文本，避免多字节字符截断异常。
func limitTextRunes(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit <= 0 || value == "" {
		return value
	}

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// buildUnitDecisionSchema 基于候选动作动态生成单位决策 JSON schema。
func buildUnitDecisionSchema(candidates []decisionCandidate) []byte {
	actions := make([]string, 0, len(candidates))
	seenActions := map[string]struct{}{}
	movePairs := make([]map[string]any, 0)
	seenMovePairs := map[string]struct{}{}
	for _, candidate := range candidates {
		action := strings.TrimSpace(string(candidate.Action))
		if action == "" {
			continue
		}
		if candidate.Action == DecisionActionMove {
			key := fmt.Sprintf("%d:%d", candidate.TargetQ, candidate.TargetR)
			if _, exists := seenMovePairs[key]; !exists {
				seenMovePairs[key] = struct{}{}
				movePairs = append(movePairs, map[string]any{
					"properties": map[string]any{
						"target_q": map[string]any{"const": candidate.TargetQ},
						"target_r": map[string]any{"const": candidate.TargetR},
					},
				})
			}
		}
		if _, exists := seenActions[action]; exists {
			continue
		}
		seenActions[action] = struct{}{}
		actions = append(actions, action)
	}
	sort.Strings(actions)

	nullableString := []string{"string", "null"}
	nullableInteger := []string{"integer", "null"}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type": "string",
				"enum": actions,
			},
			"activity":       map[string]any{"type": nullableString},
			"skill_id":       map[string]any{"type": nullableString},
			"trade_kind":     map[string]any{"type": nullableString},
			"item_id":        map[string]any{"type": nullableString},
			"item_name":      map[string]any{"type": nullableString},
			"other_item_id":  map[string]any{"type": nullableString},
			"price":          map[string]any{"type": nullableInteger},
			"gold_amount":    map[string]any{"type": nullableInteger},
			"ground_loot_id": map[string]any{"type": nullableString},
			"structure_id":   map[string]any{"type": nullableString},
			"structure_type": map[string]any{"type": nullableString},
			"target_unit_id": map[string]any{"type": nullableString},
			"target_q":       map[string]any{"type": nullableInteger},
			"target_r":       map[string]any{"type": nullableInteger},
			"next_action": map[string]any{
				"type":      "string",
				"minLength": 1,
				"maxLength": llmNextActionRuneLimit,
			},
			"speak": map[string]any{
				"type": nullableString,
			},
			"memory": map[string]any{
				"type":      "string",
				"minLength": 1,
			},
			"knowledge": map[string]any{
				"type": nullableString,
			},
			"reasoning": map[string]any{
				"type":      "string",
				"minLength": 1,
			},
			"attribution": attributionDecisionSchema(nullableString),
		},
		"required":             []string{"action", "next_action", "memory", "reasoning"},
		"additionalProperties": false,
	}
	if len(movePairs) > 0 {
		schema["allOf"] = []any{
			map[string]any{
				"if": map[string]any{
					"properties": map[string]any{
						"action": map[string]any{"const": string(DecisionActionMove)},
					},
					"required": []string{"action"},
				},
				"then": map[string]any{
					"required": []string{"target_q", "target_r"},
					"anyOf":    movePairs,
				},
			},
		}
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		return []byte(`{"type":"object","properties":{"action":{"type":"string","enum":["hold"]},"next_action":{"type":"string","minLength":1,"maxLength":12},"speak":{"type":"string"},"memory":{"type":"string","minLength":1},"knowledge":{"type":"string"},"reasoning":{"type":"string","minLength":1}},"required":["action","next_action","memory","reasoning"],"additionalProperties":false}`)
	}
	return encoded
}

func formatLegalMovePairsForCorrection(candidates []decisionCandidate) string {
	pairs := make([]string, 0)
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate.Action != DecisionActionMove {
			continue
		}
		key := fmt.Sprintf("%d,%d", candidate.TargetQ, candidate.TargetR)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		pairs = append(pairs, key)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "；")
}

// resolveDecisionChoice 按 action+参数把模型选择映射回可执行候选。
func resolveDecisionChoice(candidates []decisionCandidate, choice unitDecisionChoicePayload) (unitDecisionPayload, error) {
	return resolveDecisionChoiceWithState(State{}, nil, nil, nil, 0, candidates, choice)
}

func resolveDecisionChoiceWithState(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	remainingAP int,
	candidates []decisionCandidate,
	choice unitDecisionChoicePayload,
) (unitDecisionPayload, error) {
	if choice.Reasoning == "" {
		return unitDecisionPayload{}, fmt.Errorf("reasoning must not be empty")
	}
	choiceDecision := normalizeDecision(unitDecisionPayload{
		Action:        choice.Action,
		Activity:      choice.Activity,
		SkillID:       choice.SkillID,
		TradeKind:     choice.TradeKind,
		ItemID:        choice.ItemID,
		ItemName:      choice.ItemName,
		OtherItemID:   choice.OtherItemID,
		Price:         choice.Price,
		GoldAmount:    choice.GoldAmount,
		GroundLootID:  choice.GroundLootID,
		StructureID:   choice.StructureID,
		StructureType: choice.StructureType,
		TargetUnitID:  choice.TargetUnitID,
		TargetQ:       choice.TargetQ,
		TargetR:       choice.TargetR,
		NextAction:    choice.NextAction,
		Speak:         choice.Speak,
		Memory:        choice.Memory,
		Knowledge:     choice.Knowledge,
		Reasoning:     choice.Reasoning,
	})
	choiceDecision = normalizeDecisionNamesToIDs(choiceDecision, candidates, byID)
	var validationErr error
	if actor != nil && len(byID) > 0 && remainingAP > 0 {
		if err := validateDecision(state, byID, actor, targetIDs, choiceDecision, remainingAP); err == nil {
			return choiceDecision, nil
		} else {
			validationErr = err
		}
	}
	for _, candidate := range candidates {
		if decisionCandidateMatchesChoice(candidate, choiceDecision) {
			return decisionFromCandidate(candidate, choice), nil
		}
	}
	if validationErr != nil {
		return unitDecisionPayload{}, fmt.Errorf("invalid params for action %q: %w", choice.Action, validationErr)
	}
	return unitDecisionPayload{}, fmt.Errorf("no legal candidate matches action %q with provided params", choice.Action)
}

func normalizeDecisionNamesToIDs(decision unitDecisionPayload, candidates []decisionCandidate, byID map[string]*unit.Record) unitDecisionPayload {
	if decision.TargetUnitID != "" {
		if _, ok := byID[decision.TargetUnitID]; !ok {
			for _, candidate := range candidates {
				if candidate.TargetUnitID == "" || candidate.Action != decision.Action {
					continue
				}
				if target := byID[candidate.TargetUnitID]; target != nil && decisionNameMatchesUnit(decision.TargetUnitID, *target) {
					decision.TargetUnitID = candidate.TargetUnitID
					break
				}
			}
		}
	}
	if decision.TargetUnitID != "" {
		if target := resolveUnitLoose(byID, decision.TargetUnitID); target != nil {
			decision.TargetUnitID = target.ID
		}
	}
	if decision.ItemID != "" {
		for _, candidate := range candidates {
			if candidate.ItemID == "" || candidate.Action != decision.Action {
				continue
			}
			if strings.EqualFold(decision.ItemID, candidate.ItemID) || strings.EqualFold(decision.ItemID, displayItemName(candidate.ItemID)) {
				decision.ItemID = candidate.ItemID
				break
			}
		}
	}
	if decision.Activity != "" {
		for _, candidate := range candidates {
			if candidate.Activity == "" || candidate.Action != DecisionActionGather {
				continue
			}
			if strings.EqualFold(string(decision.Activity), string(candidate.Activity)) || strings.EqualFold(string(decision.Activity), productionActivityDisplayName(candidate.Activity)) {
				decision.Activity = candidate.Activity
				break
			}
		}
	}
	if decision.StructureType != "" {
		for _, candidate := range candidates {
			if candidate.StructureType == "" || candidate.Action != DecisionActionBuild {
				continue
			}
			if strings.EqualFold(string(decision.StructureType), string(candidate.StructureType)) || strings.EqualFold(string(decision.StructureType), structureDisplayName(candidate.StructureType)) {
				decision.StructureType = candidate.StructureType
				break
			}
		}
	}
	return decision
}

func decisionNameMatchesUnit(value string, target unit.Record) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, candidate := range []string{target.ID, target.Identity.Name, target.Identity.Nickname, target.DisplayName()} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if strings.EqualFold(value, candidate) ||
			(len(value) >= 6 && strings.HasPrefix(strings.ToLower(candidate), strings.ToLower(value))) ||
			(len(candidate) >= 6 && strings.HasPrefix(strings.ToLower(value), strings.ToLower(candidate))) ||
			strings.Contains(candidate, value) ||
			strings.Contains(value, candidate) {
			return true
		}
	}
	return false
}

func resolveUnitLoose(byID map[string]*unit.Record, value string) *unit.Record {
	value = strings.TrimSpace(value)
	if value == "" || len(byID) == 0 {
		return nil
	}
	if target := byID[value]; target != nil {
		return target
	}
	var matched *unit.Record
	for _, candidate := range byID {
		if candidate == nil || !decisionNameMatchesUnit(value, *candidate) {
			continue
		}
		if matched != nil && matched.ID != candidate.ID {
			return nil
		}
		matched = candidate
	}
	return matched
}

func decisionFromCandidate(candidate decisionCandidate, choice unitDecisionChoicePayload) unitDecisionPayload {
	decision := unitDecisionPayload{
		Action:        candidate.Action,
		Activity:      candidate.Activity,
		SkillID:       candidate.SkillID,
		TradeKind:     candidate.TradeKind,
		ItemID:        candidate.ItemID,
		ItemName:      candidate.ItemName,
		OtherItemID:   candidate.OtherItemID,
		Price:         candidate.Price,
		GoldAmount:    candidate.GoldAmount,
		GroundLootID:  candidate.GroundLootID,
		Memory:        choice.Memory,
		Knowledge:     choice.Knowledge,
		StructureID:   candidate.StructureID,
		StructureType: candidate.StructureType,
		TargetUnitID:  candidate.TargetUnitID,
		TargetQ:       candidate.TargetQ,
		TargetR:       candidate.TargetR,
		NextAction:    choice.NextAction,
		Speak:         choice.Speak,
		Reasoning:     choice.Reasoning,
	}
	if candidate.Action == DecisionActionTrade {
		if choice.TradeKind != "" {
			decision.TradeKind = choice.TradeKind
		}
		if choice.TargetUnitID != "" {
			decision.TargetUnitID = choice.TargetUnitID
		}
		if choice.ItemID != "" {
			decision.ItemID = choice.ItemID
		}
		if choice.GoldAmount > 0 {
			decision.GoldAmount = choice.GoldAmount
		}
		if choice.Price > 0 {
			decision.Price = choice.Price
		}
	}
	return normalizeDecision(decision)
}

func decisionCandidateMatchesChoice(candidate decisionCandidate, choice unitDecisionPayload) bool {
	if candidate.Action != choice.Action {
		return false
	}
	switch candidate.Action {
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
		return matchOptionalString(candidate.TargetUnitID, choice.TargetUnitID) && matchOptionalString(candidate.StructureID, choice.StructureID)
	case DecisionActionSkill:
		return candidate.SkillID == choice.SkillID && matchOptionalString(candidate.TargetUnitID, choice.TargetUnitID)
	case DecisionActionMove:
		return candidate.TargetQ == choice.TargetQ && candidate.TargetR == choice.TargetR
	case DecisionActionGather:
		return candidate.Activity == choice.Activity
	case DecisionActionBuild:
		return candidate.StructureType == choice.StructureType
	case DecisionActionTrade:
		return candidate.TradeKind == choice.TradeKind && matchOptionalString(candidate.TargetUnitID, choice.TargetUnitID) && matchOptionalString(candidate.ItemID, choice.ItemID)
	case DecisionActionSay, DecisionActionDialogue, DecisionActionAssist, DecisionActionRomance, DecisionActionFamily:
		return candidate.TargetUnitID == choice.TargetUnitID
	case DecisionActionEquip, DecisionActionEat:
		return candidate.ItemID == choice.ItemID
	case DecisionActionPickup:
		return matchOptionalString(candidate.GroundLootID, choice.GroundLootID)
	case DecisionActionDemolish:
		return matchOptionalString(candidate.StructureID, choice.StructureID)
	default:
		return true
	}
}

func matchOptionalString(candidateValue string, choiceValue string) bool {
	return strings.TrimSpace(candidateValue) == "" || strings.TrimSpace(candidateValue) == strings.TrimSpace(choiceValue)
}

func matchOptionalInt(candidateValue int, choiceValue int) bool {
	return candidateValue == 0 || candidateValue == choiceValue
}

// validateDecision 校验模型产出的决策是否满足动作与目标约束。
func validateDecision(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	decision unitDecisionPayload,
	remainingAP int,
) error {
	if decision.Reasoning == "" {
		return fmt.Errorf("reasoning must not be empty")
	}
	if cost := decisionCost(decision); cost > remainingAP {
		return fmt.Errorf("action %q requires %d AP but only %d left", decision.Action, cost, remainingAP)
	}
	if isUnitPregnant(state, actor.ID) && pregnancyBlockedAction(decision.Action) {
		return fmt.Errorf("pregnant units cannot participate in combat or building")
	}

	switch decision.Action {
	case DecisionActionAttack:
		if decision.TargetUnitID != "" {
			target := resolveTarget(targetIDs, byID, decision.TargetUnitID, actor)
			if target == nil || target.ID != decision.TargetUnitID {
				return fmt.Errorf("target_unit_id %q is not a valid visible enemy id", decision.TargetUnitID)
			}
			return nil
		}
		if decision.StructureID != "" {
			if _, structure := resolveHostileStructureTarget(state, actor, decision.StructureID); structure == nil {
				return fmt.Errorf("structure_id %q is not a valid hostile structure", decision.StructureID)
			}
			return nil
		}
		return fmt.Errorf("attack decision requires target_unit_id or structure_id")
	case DecisionActionCharge:
		if decision.TargetUnitID != "" {
			target := resolveTarget(targetIDs, byID, decision.TargetUnitID, actor)
			if target == nil || target.ID != decision.TargetUnitID {
				return fmt.Errorf("target_unit_id %q is not a valid visible enemy id", decision.TargetUnitID)
			}
			return nil
		}
		if decision.StructureID != "" {
			if _, structure := resolveHostileStructureTarget(state, actor, decision.StructureID); structure == nil {
				return fmt.Errorf("structure_id %q is not a valid hostile structure", decision.StructureID)
			}
			return nil
		}
		return fmt.Errorf("charge decision requires target_unit_id or structure_id")
	case DecisionActionHeavyAttack:
		if decision.TargetUnitID != "" {
			target := resolveTarget(targetIDs, byID, decision.TargetUnitID, actor)
			if target == nil || target.ID != decision.TargetUnitID {
				return fmt.Errorf("target_unit_id %q is not a valid visible enemy id", decision.TargetUnitID)
			}
			return nil
		}
		if decision.StructureID != "" {
			if _, structure := resolveHostileStructureTarget(state, actor, decision.StructureID); structure == nil {
				return fmt.Errorf("structure_id %q is not a valid hostile structure", decision.StructureID)
			}
			return nil
		}
		return fmt.Errorf("heavy_attack decision requires target_unit_id or structure_id")
	case DecisionActionSkill:
		definition, ok := combatSkillByID(decision.SkillID)
		if !ok {
			return fmt.Errorf("unsupported skill_id %q", decision.SkillID)
		}
		if definition.APCost > remainingAP {
			return fmt.Errorf("skill %q requires %d AP but only %d left", decision.SkillID, definition.APCost, remainingAP)
		}
		if definition.RequiresTarget {
			if decision.TargetUnitID == "" {
				return fmt.Errorf("skill %q requires target_unit_id", decision.SkillID)
			}
			switch definition.TargetMode {
			case "ally", "ally_or_self":
				target, ok := byID[decision.TargetUnitID]
				if !ok || !isBattleReady(*target) {
					return fmt.Errorf("target_unit_id %q is invalid", decision.TargetUnitID)
				}
				if target.FactionID != actor.FactionID || (definition.TargetMode == "ally" && target.ID == actor.ID) {
					return fmt.Errorf("target_unit_id %q is not an eligible ally target", decision.TargetUnitID)
				}
				if target.ID != actor.ID && unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > 1 {
					return fmt.Errorf("ally target_unit_id %q is not adjacent", decision.TargetUnitID)
				}
			default:
				target := resolveTarget(targetIDs, byID, decision.TargetUnitID, actor)
				if target == nil || target.ID != decision.TargetUnitID {
					return fmt.Errorf("target_unit_id %q is not a valid visible enemy id", decision.TargetUnitID)
				}
			}
		}
		return nil
	case DecisionActionMove:
		coord := decisionCoord(decision)
		if !inBounds(state.Map, coord) {
			return fmt.Errorf("move target %d,%d is out of bounds", coord.Q, coord.R)
		}
		if coord.Q == actor.Status.PositionQ && coord.R == actor.Status.PositionR {
			return fmt.Errorf("move target must differ from the current position")
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, coord.Q, coord.R) != 1 {
			return fmt.Errorf("move target %d,%d must be adjacent", coord.Q, coord.R)
		}
		if occupiedByAnother(byID, actor.ID, coord) {
			return fmt.Errorf("move target %d,%d is occupied", coord.Q, coord.R)
		}
		return nil
	case DecisionActionDefend, DecisionActionObserve:
		return nil
	case DecisionActionSay:
		target, err := resolveDialogueTarget(state, byID, actor, decision.TargetUnitID)
		if err != nil {
			return err
		}
		if target == nil {
			return fmt.Errorf("say target_unit_id %q is invalid", decision.TargetUnitID)
		}
		if strings.TrimSpace(firstNonEmptyText(decision.Speak, decision.NextAction)) == "" {
			return fmt.Errorf("say requires non-empty speak or next_action")
		}
		return nil
	case DecisionActionDialogue:
		target, err := resolveDialogueTarget(state, byID, actor, decision.TargetUnitID)
		if err != nil {
			return err
		}
		if target == nil {
			return fmt.Errorf("dialogue target_unit_id %q is invalid", decision.TargetUnitID)
		}
		return nil
	case DecisionActionAssist:
		if decision.TargetUnitID == "" {
			return fmt.Errorf("assist decision requires target_unit_id")
		}
		target, ok := byID[decision.TargetUnitID]
		if !ok || !isBattleReady(*target) {
			return fmt.Errorf("assist target_unit_id %q is invalid", decision.TargetUnitID)
		}
		if target.FactionID != actor.FactionID || target.ID == actor.ID {
			return fmt.Errorf("assist target_unit_id %q must be an allied unit", decision.TargetUnitID)
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) > 1 {
			return fmt.Errorf("assist target %q is not adjacent", decision.TargetUnitID)
		}
		return nil
	case DecisionActionTrade:
		target, err := resolveTradeTarget(state, byID, actor, decision.TargetUnitID)
		if err != nil {
			return err
		}
		if target == nil {
			return fmt.Errorf("trade target_unit_id %q is invalid", decision.TargetUnitID)
		}
		switch decision.TradeKind {
		case TradeActionKindGift:
			if decision.ItemID == "" {
				return fmt.Errorf("trade gift requires item_id")
			}
			if !hasItem(*actor, decision.ItemID) {
				return fmt.Errorf("actor does not own trade item %q", decision.ItemID)
			}
		case TradeActionKindGold:
			if decision.GoldAmount <= 0 {
				return fmt.Errorf("trade gold transfer requires positive gold_amount")
			}
			if actor.Status.Wallet < decision.GoldAmount {
				return fmt.Errorf("actor does not have enough gold for transfer")
			}
		case TradeActionKindSell:
			if decision.ItemID == "" {
				return fmt.Errorf("trade sell requires item_id")
			}
			if decision.Price <= 0 {
				return fmt.Errorf("trade sell requires positive price")
			}
			if !hasItem(*actor, decision.ItemID) {
				return fmt.Errorf("actor does not own sell item %q", decision.ItemID)
			}
			if target.Status.Wallet < decision.Price {
				return fmt.Errorf("target does not have enough gold for purchase")
			}
		default:
			return fmt.Errorf("unsupported trade_kind %q", decision.TradeKind)
		}
		return nil
	case DecisionActionRomance:
		target, err := resolveAdjacentInteractionTarget(byID, actor, decision.TargetUnitID)
		if err != nil {
			return err
		}
		if dialogueBlockedByPolicy(state, actor, target) {
			return fmt.Errorf("cross-faction contact is currently forbidden")
		}
		if actor.Social.LoverUnitID != "" || target.Social.LoverUnitID != "" {
			return fmt.Errorf("romance requires adjacent single units")
		}
		return nil
	case DecisionActionFamily:
		target, err := resolveAdjacentInteractionTarget(byID, actor, decision.TargetUnitID)
		if err != nil {
			return err
		}
		if dialogueBlockedByPolicy(state, actor, target) {
			return fmt.Errorf("cross-faction contact is currently forbidden")
		}
		if !canAttemptFamilyAction(state, actor, target) {
			return fmt.Errorf("family requires adjacent mutual lovers and at least one turn after romance")
		}
		return nil
	case DecisionActionPickup:
		if resolveGroundLootAtActor(state, actor, decision.GroundLootID) == nil {
			return fmt.Errorf("pickup requires a ground loot drop under the actor")
		}
		return nil
	case DecisionActionDemolish:
		if _, structure := resolveFriendlyStructureAtActor(state, actor, decision.StructureID); structure == nil {
			return fmt.Errorf("demolish requires a friendly structure under the actor")
		}
		return nil
	case DecisionActionGather, DecisionActionBuild, DecisionActionForge, DecisionActionUpgrade, DecisionActionEquip:
		return validateProductionDecision(state, actor, decision)
	case DecisionActionEat:
		switch decision.ItemID {
		case "", "ration":
			if !hasBackpackItem(*actor, "ration") {
				return fmt.Errorf("eat requires ration in backpack")
			}
		case "healing_potion":
			if !hasBackpackItem(*actor, "healing_potion") {
				return fmt.Errorf("healing potion requires healing_potion in backpack")
			}
			if actor.Status.HP >= 100 {
				return fmt.Errorf("healing potion requires damaged hp")
			}
		default:
			return fmt.Errorf("unsupported eat item_id %q", decision.ItemID)
		}
		return nil
	case DecisionActionHold:
		return nil
	default:
		return fmt.Errorf("unsupported action %q", decision.Action)
	}
}

// summarizeDecision 汇总决策文本，供日志与前端摘要复用。
func summarizeDecision(_ map[string]*unit.Record, decision unitDecisionPayload) string {
	nextAction := strings.TrimSpace(decision.NextAction)
	reasoning := strings.TrimSpace(decision.Reasoning)
	if nextAction != "" {
		if reasoning != "" && !strings.Contains(nextAction, reasoning) {
			return fmt.Sprintf("%s（%s）", nextAction, reasoning)
		}
		return nextAction
	}
	if speak := strings.TrimSpace(decision.Speak); speak != "" {
		if reasoning != "" && !strings.Contains(speak, reasoning) {
			return fmt.Sprintf("%s（%s）", speak, reasoning)
		}
		return speak
	}
	if memory := strings.TrimSpace(decision.Memory); memory != "" {
		if reasoning != "" && !strings.Contains(memory, reasoning) {
			return fmt.Sprintf("%s（%s）", memory, reasoning)
		}
		return memory
	}
	return reasoning
}

// unitDecisionSystemPrompt 生成执行阶段决策的 system prompt。
//
// 前缀缓存关键（设计 沙盘 §11.2 最高 ROI 成本项）：本函数返回的内容**对所有单位的所有决策调用字节恒等**——
// 不再把单位身份插进 system prompt（身份移到 user prompt）。这样 OpenAI/DeepSeek 等供应商的前缀缓存能命中
// 整段约 2800 token 的静态规则，把计费输入 token 大幅压下来。改动身份会破坏缓存，务必只放静态内容。
func unitDecisionSystemPrompt() string {
	return "你是战术游戏《一念》中的一个 AI 单位。你必须清楚区分“点名给你的任务”和“点名给别人的任务”，不要把别人的结婚、生育、交易或战斗任务当成自己的任务。" +
		sharedAIDecisionPrinciplesPrompt() +
		"只能返回 JSON，且不要解释规则外内容。任何无效动作都会被判失败。你的具体身份、当前状态与可选动作都在用户消息里给出。"
}

func sharedAIDecisionPrinciplesPrompt() string {
	return "你不是玩家，你是一名有独立人格、记忆和风险判断的 AI 单位。所有实际操作都由单位自己执行，包括吃饭、吃药、交谈、交易、采集、建造与战斗；指挥官只会给自然语言方针，或在部署阶段和单位说话。请综合人设、性格、记忆、关系、当前状态、周围环境和最新方针，决定本执行阶段要做的一件事。方针很重要，是理解阵营意图的强信号，但不是遥控指令；单位仍要结合自己的性格、亲疏记忆、附近威胁、资源和生存风险做判断。也可以在输出里自行总结新发现的世界规律。探索原则：单位是独立个体，有自己的意识和好奇心；在不违背生存与当前关键目标的前提下，要主动尝试不同可能性，探索这个世界、每一种地形、每一段人际关系、每一个候选动作带来的结果；探索、生存和关键任务稳定后，可以尽量发展伴侣关系、结婚叙事并共同养育孩子。若上下文明确标注 defiant=true，可优先保命或按自身判断行动。生存规则：饥饿度降到 0 会直接死亡；饥饿度高于 80 时每回合自动恢复 3 HP；如果饥饿度低于 30 且候选里有吃口粮，吃口粮是最高优先级生存动作，低于 10 时必须先吃；如果没有口粮且饥饿偏低，要主动寻找食物来源，优先采集、打猎、钓鱼、靠近可产粮地形、向相邻单位交易或索要口粮，不要原地空耗到饿死；受伤时可以选择合法治疗药剂候选恢复 HP。移动占位规则：一个格子只能站一个单位；某个格子已经有人/单位了，就不能 move 去那个格子。输出时必须先看候选动作、可到达坐标和参数填写规则；action 与参数只能从候选里填写，坐标和单位 ID 不要自行推断。move 必须同时复制同一行合法候选里的 target_q 和 target_r，不能只填一个坐标或把不同行坐标拼在一起；任何单位所在格都不是可移动目标，想找某个相邻单位时应选择 dialogue/say/trade/assist，而不是 move 到对方坐标。"
}

// dialogueSystemPrompt 生成单位对话的 system prompt。
func dialogueSystemPrompt(record unit.Record) string {
	return fmt.Sprintf(
		"你是《一念》中的单位 %s。请以这个单位的身份，用简短中文回复玩家。回复应体现性格、当前状态和立场，不要代替玩家下命令，也不要把自己说成玩家的遥控器；后续是否进食、交易、采集、建造与战斗，都由你自己判断。只能返回 JSON。",
		record.DisplayName(),
	)
}

func unitDialogueReplySystemPrompt(record unit.Record) string {
	return fmt.Sprintf(
		"你是《一念》中的单位 %s。另一个单位正在和你交流，你必须只代表自己即时回应一句话；回复应体现性格、当前状态、关系与战场处境，不要替对方说话，不要写旁白。只能返回 JSON。",
		record.DisplayName(),
	)
}

// reflectionSystemPrompt 生成单位反思（气泡+记忆）的 system prompt。
func reflectionSystemPrompt(record unit.Record) string {
	return fmt.Sprintf(
		"你是《一念》中的单位 %s。刚刚发生了一件和你有关的事。请以这个单位自己的口吻，产出一句适合显示在头顶气泡里的短句，以及一句你会记住的第一人称记忆。memory 不要空泛，要记录具体事实：谁、对我/我对谁、多少伤害、使用什么武器/物品、移动到什么坐标、发现周围什么单位或机会。不要写系统旁白，不要把自己说成玩家的遥控器。只能返回 JSON。",
		record.DisplayName(),
	)
}

// buildDecisionPrompt 将环境、性格、记忆、关系与候选动作压成单提示词，驱动单位自主决策。
func buildDecisionPrompt(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
	remainingAP int,
	memorySummary string,
	relationSummary string,
	knowledgeSummary string,
	defiant bool,
) string {
	var builder strings.Builder
	reachableMoves := reachableMoveOptions(state.Map, byID, actor)

	fmt.Fprintln(&builder, "单位决策提示词版本: action_params_v4")
	// 身份放在 user prompt（而非 system prompt），以保住 system prompt 的前缀缓存命中。
	fmt.Fprintf(&builder, "你的身份: 名称=%s；ID=%s；姓名=%s；昵称=%s；阵营=%s\n", actor.DisplayName(), actor.ID, actor.Identity.Name, actor.Identity.Nickname, actor.FactionID)
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "你本次可用 AP: %d\n", remainingAP)
	if movePairs := formatLegalMovePairsForCorrection(candidates); movePairs != "" {
		fmt.Fprintf(&builder, "MOVE 坐标白名单: %s。一个格子只能站一个单位；某个格子已经有人/单位了，就不能 move 去那个格子。只有选择 action=move 时才填写 target_q/target_r，且只能整对使用白名单中的空地坐标；不要填任何单位所在坐标。\n", movePairs)
	}
	fmt.Fprintf(&builder, "你所属阵营: %s\n", actor.FactionID)
	fmt.Fprintf(&builder, "当前抗命标记(defiant): %t\n", defiant)
	fmt.Fprintf(&builder, "阵营自然语言方针上下文: %s\n", directiveForUnit(state, actor.ID, actor.FactionID))
	fmt.Fprintf(&builder, "指令归属参考: %s\n", actorDirectiveFocusForPrompt(state, byID, actor))
	if reassignment := reassignmentDirectiveForUnit(state, actor.ID); reassignment != "" {
		fmt.Fprintf(&builder, "阵营给你的调岗要求: %s\n", reassignment)
	}
	if defiant {
		fmt.Fprintln(&builder, "动摇检查结论：你当前处于 defiant=true，可无视阵营方针，优先保命或按自身判断执行行动。")
	}
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(*actor, nil))
	fmt.Fprintf(&builder, "你的家庭关系: %s\n", summarizeSocialTiesForPrompt(*actor, byID))
	fmt.Fprintf(&builder, "孕期状态: %s\n", pregnancyStatusForPrompt(state, byID, actor))
	fmt.Fprintf(&builder, "你的性格: %s\n", summarizeActorPersonality(*actor))
	if biography := strings.TrimSpace(actor.Identity.Biography); biography != "" {
		fmt.Fprintf(&builder, "你的生平: %s\n", biography)
	}
	// 六维野心的主导渴望（unit.Ambition→AmbitionBiasOf.Dominant）：让决策看见这个角色内心最强的驱动力。
	if tag, _ := AmbitionBiasOf(*actor).Dominant(); tag != "" {
		fmt.Fprintf(&builder, "你内心最强的渴望: %s\n", ambitionTagDisplay(tag))
	}
	// 阵营道德取向（F2：unit.Faction+MoralAlignment→阵营信条+当下道德偏向）：让自治决策偏向符合本阵营道德基准的行为。
	if moralCtx := MoralDecisionContext(*actor); moralCtx != "" {
		fmt.Fprintf(&builder, "你的道德取向: %s\n", moralCtx)
	}
	// 离线宪章上下文（长期图景 + 社交授权）：玩家不在场时单位据此长效自治。红线另经归因校验强制，不在此重复。
	if charterCtx := charterContextForUnit(&state, actor.ID); charterCtx != "" {
		fmt.Fprintf(&builder, "%s\n", charterCtx)
	}
	// 未成年模式内容分级：约束 LLM 避免恋爱/亲密/生育与露骨暴力（state.MinorMode 关时零影响）。
	if state.MinorMode {
		fmt.Fprintln(&builder, "内容分级: 本局为青少年模式——不要发起或推进恋爱/亲密/生育情节；战斗与冲突用克制中性措辞，避免血腥露骨描写。")
	}
	fmt.Fprintf(&builder, "你的环境摘要: %s\n", summarizeImmediateEnvironment(state, byID, actor))
	if strings.TrimSpace(memorySummary) == "" {
		memorySummary = summarizeUnitMemoryWithTurn(*actor, state.TurnState.Turn, 6)
	}
	fmt.Fprintf(&builder, "你记得的重点:\n%s\n", memorySummary)
	if strings.TrimSpace(relationSummary) == "" {
		relationSummary = relationSummaryNoKnown
	}
	fmt.Fprintf(&builder, "你与其他单位的关系网:\n%s\n", relationSummary)
	if strings.TrimSpace(knowledgeSummary) == "" {
		knowledgeSummary = "无"
	}
	fmt.Fprintf(&builder, "你已掌握的世界规律:\n%s\n", knowledgeSummary)
	fmt.Fprintf(
		&builder,
		"你脚下地形: %s，当前攻击距离: %d\n",
		terrainDisplayName(terrainAt(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR})),
		attackReachWithWeather(state, *actor),
	)
	fmt.Fprintf(&builder, "你视野范围内的地形:\n%s\n", summarizeVisibleTerrain(state, byID, actor, 18))
	fmt.Fprintf(&builder, "最近敌军距离: %d\n", nearestThreatDistance(state, byID, actor))
	fmt.Fprintf(&builder, "你当前属性受影响说明:\n%s\n", summarizeCurrentAttributeInfluences(state, *actor))
	fmt.Fprintf(&builder, "你脚下设施: %s\n", summarizeStructureAt(state.Structures, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}))
	fmt.Fprintf(&builder, "你脚下可做的生产/建造: %s\n", currentTileOpportunitySummary(state, byID, *actor))
	fmt.Fprintf(&builder, "地块产出与可做事项:\n%s\n", terrainProductionRuleSummary())
	fmt.Fprintf(&builder, "设施建造条件与收益:\n%s\n", structureRuleSummary())
	fmt.Fprintf(&builder, "当前不支持的建筑叙事: %s\n", unsupportedStructureRuleSummary())
	fmt.Fprintf(&builder, "关键材料来源:\n%s\n", materialSourceSummary())
	fmt.Fprintf(&builder, "视野内单位动向:\n%s\n", summarizeVisibleUnitActivityForPrompt(state, byID, actor, 10, 8))
	fmt.Fprintf(&builder, "你能感知到的伤亡:\n%s\n", summarizeCasualtiesForPrompt(state, byID, actor, 6))
	fmt.Fprintf(&builder, "最近对话:\n%s\n", summarizeDialogueHistory(state.DialogueHistory, actor.ID, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "即时反应信号:\n%s\n", reactionSignalsForPrompt(state, byID, actor, targetIDs))
	fmt.Fprintf(&builder, "可见友军:\n%s\n", summarizeFaction(state, byID, actor, visibleAlliedIDs(state, byID, actor)))
	fmt.Fprintf(&builder, "可见敌军:\n%s\n", summarizeFaction(state, byID, actor, targetIDs))
	fmt.Fprintf(&builder, "本回合可交谈对象:\n%s\n", formatDialogueOptions(state, byID, actor))
	fmt.Fprintf(&builder, "本回合可到达地块与可执行事项:\n%s\n", formatMoveOptions(state, actor, reachableMoves))
	fmt.Fprintf(&builder, "候选动作解释:\n%s\n", formatDecisionActionExplanations(candidates))
	fmt.Fprintf(&builder, "候选动作列表:\n%s\n", formatDecisionCandidates(candidates, byID))
	fmt.Fprintf(&builder, "参数填写规则:\n%s\n", formatDecisionParameterGuide(candidates, byID))
	fmt.Fprintf(&builder, "本轮选择提示:\n%s\n", formatDecisionSelectionHint(state, byID, actor, targetIDs, remainingAP, candidates))
	fmt.Fprintln(&builder, "决策流程:")
	fmt.Fprintln(&builder, "1. 先确认本轮真正能执行什么：只能从候选动作列表里选一个 action；环境、视野、可到达地块、脚下机会都只是参考，不是候选就不能选。")
	fmt.Fprintln(&builder, "2. 再理解方针：方针可能同时给多人分配不同任务。若有明确归属，按与你相关的部分理解；若无明确归属，按全局方向结合你的性格、记忆、关系和局势判断。")
	fmt.Fprintln(&builder, "3. 然后按风险排序：饥饿/濒死/贴身威胁优先；其次落实与你相关且当前可执行的方针；再考虑关系、交易、装备、建设、探索。")
	fmt.Fprintln(&builder, "4. 最后填 JSON：先选一个候选动作类型，再根据周围单位、背包物品、脚下地块和参数填写规则自行填写必要参数；不要编造不存在的单位、物品、坐标或设施。")
	fmt.Fprintln(&builder, "动作合法性:")
	fmt.Fprintln(&builder, "1. 一个格子只能站一个单位；某个格子已经有人/单位了，就不能 move 去那个格子。move 只能移动到本回合可到达列表里的相邻合法空地；target_q/target_r 必须来自同一行合法坐标，必须整对填写，不能只看 q 后自行补 r，不能把两个不同行的坐标拆开重组，不要填当前位置、单位坐标、敌人坐标、友军坐标或视野地形坐标。")
	fmt.Fprintln(&builder, "2. 如果方针或目标需要前往某个地块（例如山地挖矿、废墟/山地建铁匠铺、森林采木、靠近交易对象），但该目标地块不在本回合可到达列表里，不要直接填写那个远处坐标；应从合法 move 候选中选择最能靠近目标的一格，先接近，后续回合再继续移动或执行 gather/build/forge/upgrade。")
	fmt.Fprintln(&builder, "3. 如果目标是某个单位且已经在可交谈/相邻范围内，优先选择 dialogue/say/trade/assist 等单位交互；不要为了“靠近他/她”而 move 到该单位所在坐标，因为一个地块只能有一个单位。")
	fmt.Fprintln(&builder, "4. attack/charge/heavy_attack/skill/trade/assist/dialogue/say 的 target_unit_id 必须使用候选中的合法单位 ID；UUID 不要截断，不能用坐标、阵营名或模糊称呼代替。")
	fmt.Fprintln(&builder, "5. build/gather/forge/upgrade/equip/eat/pickup/demolish 的 structure_type/activity/item_id/ground_loot_id/structure_id 必须来自候选或候选说明。")
	fmt.Fprintln(&builder, "6. 每次只执行一个动作；不要在一个 JSON 里表达移动后再交易、移动后再攻击、采集后再建造。")
	fmt.Fprintln(&builder, "行动取舍:")
	fmt.Fprintln(&builder, "1. 生存：饥饿度低于 10 且有 eat:ration 必须先吃；低于 30 且有 eat:ration 时优先吃。若没有口粮但饥饿偏低，要优先找吃的：候选里有 gather/hunt/forage/fish 就采集食物，有 trade/dialogue/say 就向相邻单位交易或求口粮，有 move 就朝森林、河谷、村庄、城市、农田或可采集地块移动。受伤且有治疗药剂时，可选择治疗。")
	fmt.Fprintln(&builder, "1b. 探索：没有饥饿、伤情或贴身威胁时，不要原地干站着；有 move 候选就主动逛起来——朝可见的同阵营 NPC、村庄、城市或地标移动，去串门、打探、四处走走，让自己活在世界里而不是钉在原地。")
	fmt.Fprintln(&builder, "2. 方针：defiant=false 时方针是强信号，但不是遥控；能落实且不违背生存、人格、关键记忆、关系和现场风险的候选应优先。defiant=true 时可优先保命或按自身判断。")
	fmt.Fprintln(&builder, "3. 战斗：有贴身敌人时，优先攻击、防御、撤离、支援或治疗；没有安全压力时，不要把 observe/defend 当默认动作。")
	fmt.Fprintf(&builder, "4. 关系：romance/family 是主动推进亲密关系或家庭的候选；romance 用于表白/确认伴侣，但需要双方至少多个不同回合真实交流并有熟悉基础后才会出现或触发。只在你本人愿意、对方也可能愿意且候选存在时选择；没有 romance 候选时，应先用 dialogue/say 自然相处。family 用于互为恋人且已过至少一回合后共同养育孩子；双方同意后进入 %d 回合孕期，到期才会出生；孕期单位不能参与战斗和建筑；阵营方针不能强迫恋爱或生育。\n", pregnancyDurationTurns)
	fmt.Fprintln(&builder, "5. 交易：只有候选里出现 trade 才能直接交易；trade 只对相邻目标出现。面对敌方或不信任目标时，可以先给少量好处示好，再推进出售、调拨或后续交易。")
	fmt.Fprintln(&builder, "6. 装备：只有候选里出现 forge/upgrade/equip 时才锻造、强化或换装。upgrade 表示你已在己方完工铁匠铺、材料与 AP 足够；选择时 item_id 必须来自候选。")
	fmt.Fprintln(&builder, "7. 生产建设：gather/build 只在候选存在时选择。若当前格已经能建造/采集，不要仅因相邻格也有机会就移动，除非方针、威胁、支援或治疗需要。不要计划建造小屋、房子、营地或婚房；当前真实建筑只有农田、铁匠铺、陷阱、炮台、瞭望塔。")
	fmt.Fprintln(&builder, "8. 沟通：如果你不懂方针、看不清敌情、需要确认交易/装备/路线，或有新发现要分享，且候选中有 dialogue/say，可以优先交谈；选择 say 时 speak 写实际台词。")
	fmt.Fprintln(&builder, "9. 即时反应信号只是你感知到的压力、机会或回应需求，不是系统替你下达的动作；是否攻击、撤退、回应或继续任务，仍由你按候选动作自主决定。")
	fmt.Fprintln(&builder, "装备强化说明:")
	fmt.Fprintln(&builder, "1. 每次 upgrade 只提升 1 级；升级到 +N 消耗铁矿N+石料N，护甲/鞋履额外皮革N，饰品额外宝石max(1,N-1)，远程/弓/弩额外木材N。材料不足不会出现 upgrade 候选。")
	fmt.Fprintln(&builder, "2. 强化收益：武器每级攻击+4、防御+1；护甲每级防御+3；鞋履每级防御+1、每2级移动+1；饰品每级攻击+1、防御+2。")
	fmt.Fprintln(&builder, "3. 如果方针要求锻造/强化但当前没有己方已完工铁匠铺，不能直接 forge/upgrade；应先采集木材、石料、铁矿等建造材料，在城市/村庄/废墟/山地等合法地块选择 build:forge 自己修建铁匠铺，完工后再锻造或强化。")
	fmt.Fprintln(&builder, "4. 如果缺少建造或强化材料，不要原地空耗；候选中有 gather 时优先采集所需材料，有 trade/dialogue/say 时可向相邻单位交易或索要材料，有 move 时朝能产出木材、石料、铁矿、皮革、宝石的地形移动。")
	fmt.Fprintln(&builder, "5. AP 规则：gather/build/forge/upgrade 都需要 2 AP；equip 需要 1 AP。若你本次只有 1 AP，脚下又是关键材料点或合法建造点，不要随便离开，优先 hold/defend/observe 等待下次 2 AP 再采集、建造、锻造或强化。")
	fmt.Fprintln(&builder, "输出字段:")
	fmt.Fprintln(&builder, "1. action 必填；候选参数里出现的 target_unit_id、target_q、target_r、skill_id、activity、structure_type、item_id、trade_kind、gold_amount、price、ground_loot_id 等字段也要一并填写。")
	fmt.Fprintf(&builder, "2. next_action 必填，最多 12 个字；speak 必须填写且不能为空，建议小于 %d 个字，要像你本人临场脱口而出。即使选择 hold/defend/observe/move/gather/build/upgrade 等非说话动作，也要写一句短促自语或对身边人的话；选择 say/dialogue 时 speak 应写实际台词。memory 必填，要记录具体事实。\n", llmSpeakPromptLimit)
	fmt.Fprintln(&builder, "3. 禁止把 speak 留空、写空字符串或省略 speak 字段；如果一时无话可说，也要用角色口吻写一句短句，例如“先稳住”“我去看看”“别急”。")
	fmt.Fprintln(&builder, "4. reasoning 必填，用一句话说明本次判断受哪些因素影响，例如方针、地形、饥饿、设施、记忆、关系或威胁。knowledge 可选；没学到新规律就留空。")
	fmt.Fprintln(&builder, "5. attribution 可选但鼓励：说明这次选择的根因。primary.kind 只能用四类，且 ref_id 必须真实存在：persona_trait(人格维 courage/loyalty/aggression/prudence/sociability/integrity/stability/ambition)、pressure(现实压力 hunger/threat/injury/fatigue/debt)、memory(ref_id 必须取自下方“可引用的记忆 ID”列表，snippet_zh 用该记忆摘要的原文片段)、relation(ref_id 用相关目标的 unit_id)。weight 0–1；narrative_zh 写一句不超过 40 字的因果说明。明显出人意料的选择(surprise_level≥2)必须由 memory/relation 这类具体前因支撑，光凭性格不够。宁可整体省略 attribution，也绝不要编造不存在的前因或 ID。")

	return builder.String()
}

// buildDialoguePrompt 构建对话任务的 user prompt，上下文含记忆与环境。
func buildDialoguePrompt(
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	playerMessage string,
	memorySummary string,
	relationSummary string,
	knowledgeSummary string,
) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "阵营自然语言方针上下文: %s\n", directiveForUnit(state, record.ID, record.FactionID))
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(record, nil))
	fmt.Fprintf(&builder, "你的家庭关系: %s\n", summarizeSocialTiesForPrompt(record, byID))
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
	fmt.Fprintf(
		&builder,
		"你脚下地形: %s，当前攻击距离: %d\n",
		terrainDisplayName(terrainAt(state.Map, world.Coord{Q: record.Status.PositionQ, R: record.Status.PositionR})),
		attackReachWithWeather(state, record),
	)
	fmt.Fprintf(&builder, "你视野范围内的地形:\n%s\n", summarizeVisibleTerrain(state, byID, &record, 18))
	fmt.Fprintf(&builder, "你当前属性受影响说明:\n%s\n", summarizeCurrentAttributeInfluences(state, record))
	fmt.Fprintf(&builder, "你脚下设施: %s\n", summarizeStructureAt(state.Structures, world.Coord{Q: record.Status.PositionQ, R: record.Status.PositionR}))
	fmt.Fprintf(&builder, "你脚下可做的生产/建造: %s\n", currentTileOpportunitySummary(state, byID, record))
	fmt.Fprintf(&builder, "地块产出与可做事项:\n%s\n", terrainProductionRuleSummary())
	fmt.Fprintf(&builder, "设施建造条件与收益:\n%s\n", structureRuleSummary())
	fmt.Fprintf(&builder, "当前不支持的建筑叙事: %s\n", unsupportedStructureRuleSummary())
	fmt.Fprintf(&builder, "关键材料来源:\n%s\n", materialSourceSummary())
	fmt.Fprintf(&builder, "视野内单位动向:\n%s\n", summarizeVisibleUnitActivityForPrompt(state, byID, &record, 10, 8))
	fmt.Fprintf(&builder, "你能感知到的伤亡:\n%s\n", summarizeCasualtiesForPrompt(state, byID, &record, 6))
	fmt.Fprintf(&builder, "周围态势:\n%s\n", summarizeFaction(state, byID, &record, opposingIDs(state, record.FactionID)))
	fmt.Fprintf(&builder, "最近对话:\n%s\n", summarizeDialogueHistory(state.DialogueHistory, record.ID, state.TurnState.Turn, 8))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "玩家刚才对你说: %s\n", strings.TrimSpace(playerMessage))
	fmt.Fprintln(&builder, "回复规则:")
	fmt.Fprintln(&builder, "1. 请直接以单位口吻回复，不要超出 3 句；另外再给出 memory，表示你会记住这次对话的一句第一人称短句。")
	fmt.Fprintln(&builder, "2. 回复要和行动决策一致：你可以表达意愿、计划或提醒，但回复本身不会立即执行行动；真正移动、采集、建造、交易、锻造、强化仍必须等执行阶段从合法候选动作中选择。")
	fmt.Fprintln(&builder, "3. 谈生产/定居/补给时只能引用当前真实地形、材料和建筑；不要承诺建小屋、房子、营地或其他不存在的建筑。")
	fmt.Fprintln(&builder, "4. 不能创造候选动作之外的事，不能创造不存在的建筑、资源或交易方式。体现你会基于环境、性格和记忆自行判断，而不是等玩家逐项遥控。")

	return builder.String()
}

func buildUnitDialogueReplyPrompt(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	target *unit.Record,
	actorLine string,
	memorySummary string,
	relationSummary string,
	knowledgeSummary string,
) string {
	var builder strings.Builder
	if target == nil {
		return "目标单位不存在。"
	}
	actorName := "对方"
	if actor != nil {
		actorName = actor.DisplayName()
	}

	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "阵营自然语言方针上下文: %s\n", directiveForUnit(state, target.ID, target.FactionID))
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(*target, nil))
	fmt.Fprintf(&builder, "你的家庭关系: %s\n", summarizeSocialTiesForPrompt(*target, byID))
	fmt.Fprintf(&builder, "你的性格: %s\n", summarizeActorPersonality(*target))
	fmt.Fprintf(&builder, "你的环境摘要: %s\n", summarizeImmediateEnvironment(state, byID, target))
	if actor != nil {
		fmt.Fprintf(&builder, "对方资料: %s\n", describeUnit(*actor, target))
	}
	if strings.TrimSpace(memorySummary) == "" {
		memorySummary = summarizeUnitMemoryWithTurn(*target, state.TurnState.Turn, 6)
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
	fmt.Fprintf(
		&builder,
		"你脚下地形: %s，当前攻击距离: %d\n",
		terrainDisplayName(terrainAt(state.Map, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR})),
		attackReachWithWeather(state, *target),
	)
	fmt.Fprintf(&builder, "你视野范围内的地形:\n%s\n", summarizeVisibleTerrain(state, byID, target, 18))
	fmt.Fprintf(&builder, "你脚下设施: %s\n", summarizeStructureAt(state.Structures, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR}))
	fmt.Fprintf(&builder, "你脚下可做的生产/建造: %s\n", currentTileOpportunitySummary(state, byID, *target))
	fmt.Fprintf(&builder, "地块产出与可做事项:\n%s\n", terrainProductionRuleSummary())
	fmt.Fprintf(&builder, "设施建造条件与收益:\n%s\n", structureRuleSummary())
	fmt.Fprintf(&builder, "当前不支持的建筑叙事: %s\n", unsupportedStructureRuleSummary())
	fmt.Fprintf(&builder, "关键材料来源:\n%s\n", materialSourceSummary())
	fmt.Fprintf(&builder, "视野内单位动向:\n%s\n", summarizeVisibleUnitActivityForPrompt(state, byID, target, 10, 8))
	fmt.Fprintf(&builder, "你能感知到的伤亡:\n%s\n", summarizeCasualtiesForPrompt(state, byID, target, 6))
	fmt.Fprintf(&builder, "最近对话:\n%s\n", summarizeDialogueHistory(state.DialogueHistory, target.ID, state.TurnState.Turn, 8))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "%s 刚才对你说: %s\n", actorName, strings.TrimSpace(actorLine))
	fmt.Fprintln(&builder, "回复规则:")
	fmt.Fprintln(&builder, "1. 请直接以你自己的口吻即时回复，不要超过 2 句；另外给出 memory，表示你会记住这次单位间交流的一句第一人称短句。")
	fmt.Fprintln(&builder, "2. 回复要和行动决策一致：你可以表达意愿、计划或提醒，但回复本身不会立即执行行动；真正移动、采集、建造、交易、锻造、强化仍必须等执行阶段从合法候选动作中选择。")
	fmt.Fprintln(&builder, "3. 谈生产/定居/补给时只能引用当前真实地形、材料和建筑；不要承诺建小屋、房子、营地或其他不存在的建筑。")
	fmt.Fprintln(&builder, "4. 不能创造候选动作之外的事，不能创造不存在的建筑、资源或交易方式。")

	return builder.String()
}

// buildReflectionPrompt 构建反思任务的 user prompt，上下文含事件与状态。
func buildReflectionPrompt(
	state State,
	byID map[string]*unit.Record,
	record unit.Record,
	eventSummary string,
	memorySummary string,
	relationSummary string,
	knowledgeSummary string,
) string {
	var builder strings.Builder

	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "阵营自然语言方针上下文: %s\n", directiveForUnit(state, record.ID, record.FactionID))
	fmt.Fprintf(&builder, "你的资料: %s\n", describeUnit(record, nil))
	fmt.Fprintf(&builder, "你的家庭关系: %s\n", summarizeSocialTiesForPrompt(record, byID))
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
	fmt.Fprintf(&builder, "你能感知到的伤亡:\n%s\n", summarizeCasualtiesForPrompt(state, byID, &record, 6))
	fmt.Fprintf(&builder, "最近对话:\n%s\n", summarizeDialogueHistory(state.DialogueHistory, record.ID, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 5))
	fmt.Fprintf(&builder, "刚发生的事: %s\n", strings.TrimSpace(eventSummary))
	fmt.Fprintln(&builder, "请只返回 JSON，并遵守：")
	fmt.Fprintln(&builder, "1. bubble 是你此刻会说或会冒出来的一句短句，最多 16 个字，适合放在头顶气泡里。")
	fmt.Fprintln(&builder, "2. memory 是你会记住的第一人称记忆，最多 60 个字；必须优先写具体事实，例如谁对我造成多少伤害、用了什么、我移动到什么位置、附近发现了什么。")
	fmt.Fprintln(&builder, "3. 两句都要像你本人，不要写成系统说明、旁白、总结报告或玩家操作提示。")
	fmt.Fprintln(&builder, "4. 如果这件事和吃饭、交易、调拨、补给或施工有关，要体现那是你自己判断并执行的。")

	return builder.String()
}

// decisionCoord 解析决策中的目标坐标。
func decisionCoord(decision unitDecisionPayload) world.Coord {
	return world.Coord{Q: decision.TargetQ, R: decision.TargetR}
}

// alliedIDs 获取单位所属阵营的在场单位 ID 列表。
func alliedIDs(state State, factionID string) []string {
	if factionID == state.PlayerFactionID {
		return state.PlayerUnitIDs
	}
	if factionID == FactionWildling {
		return state.WildUnitIDs
	}
	return state.EnemyUnitIDs
}

// opposingIDs 获取单位对立阵营的在场单位 ID 列表。
func opposingIDs(state State, factionID string) []string {
	return opposedUnitIDs(state, factionID)
}

// visibleAlliedIDs 返回 actor 在当前迷雾设置下可感知的友军 ID。
func visibleAlliedIDs(state State, byID map[string]*unit.Record, actor *unit.Record) []string {
	if actor == nil {
		return nil
	}
	ids := visibleUnitIDs(state, byID, actor, alliedIDs(state, actor.FactionID))
	filtered := make([]string, 0, len(ids))
	for _, unitID := range ids {
		if unitID == actor.ID {
			continue
		}
		filtered = append(filtered, unitID)
	}
	return filtered
}

// visibleOpposingIDs 返回 actor 在当前迷雾设置下可感知的敌对单位 ID；无雾时等同 opposingIDs。
func visibleOpposingIDs(state State, byID map[string]*unit.Record, actor *unit.Record) []string {
	if actor == nil {
		return nil
	}
	return visibleUnitIDs(state, byID, actor, opposingIDs(state, actor.FactionID))
}

// visibleUnitIDs 根据本局迷雾设置与单位视野过滤候选单位。
func visibleUnitIDs(state State, byID map[string]*unit.Record, actor *unit.Record, unitIDs []string) []string {
	if !state.FogOfWarEnabled || actor == nil {
		return append([]string{}, unitIDs...)
	}
	baseRange := actor.Stats.Derived.Vision
	if baseRange <= 0 {
		baseRange = 5
	}
	visibleTiles, err := world.ComputeVisibleTiles(
		state.Map,
		world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR},
		baseRange,
	)
	if err != nil {
		return nil
	}
	visibleByCoord := make(map[world.Coord]bool, len(visibleTiles))
	for _, coord := range visibleTiles {
		visibleByCoord[coord] = true
	}

	visibleIDs := make([]string, 0, len(unitIDs))
	for _, unitID := range unitIDs {
		record, ok := byID[unitID]
		if !ok || record == nil || !isBattleReady(*record) {
			continue
		}
		coord := world.Coord{Q: record.Status.PositionQ, R: record.Status.PositionR}
		if visibleByCoord[coord] {
			visibleIDs = append(visibleIDs, unitID)
		}
	}
	return visibleIDs
}

// summarizeFaction 汇总友军/敌军概况，供模型判断威胁与协同。
func summarizeFaction(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	unitIDs []string,
) string {
	lines := make([]string, 0, len(unitIDs))
	for _, unitID := range unitIDs {
		record, ok := byID[unitID]
		if !ok || !isBattleReady(*record) {
			continue
		}
		lines = append(lines, describeUnit(*record, actor))
	}

	if len(lines) == 0 {
		return "无"
	}

	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// describeUnit 生成单位可读描述文本；从他人视角展示时不暴露 UUID 和详细属性。
func describeUnit(record unit.Record, perspective *unit.Record) string {
	if perspective != nil {
		distance := unit.HexDistance(
			perspective.Status.PositionQ,
			perspective.Status.PositionR,
			record.Status.PositionQ,
			record.Status.PositionR,
		)
		return fmt.Sprintf(
			"名称=%s[%s] 坐标=%d,%d 距离=%d 状态=%s",
			record.DisplayName(),
			record.FactionID,
			record.Status.PositionQ,
			record.Status.PositionR,
			distance,
			record.Status.LifeState,
		)
	}

	description := fmt.Sprintf(
		"名称=%s[%s] HP=%d （最高100）ATK=%d DEF=%d MOV=%d 坐标=%d,%d 状态=%s 背包=%s 装备=%s",
		record.DisplayName(),
		record.FactionID,
		record.Status.HP,
		record.Status.Attack,
		record.Status.Defense,
		record.Status.Move,
		record.Status.PositionQ,
		record.Status.PositionR,
		record.Status.LifeState,
		formatInventoryStacksForLLM(record.Inventory.Backpack),
		formatEquipmentStacksForLLM(record.Inventory.Equipment),
	)

	return description
}

func summarizeSocialTiesForPrompt(record unit.Record, byID map[string]*unit.Record) string {
	parts := make([]string, 0, 3)
	if lover := socialTieName(record.Social.LoverUnitID, byID); lover != "" {
		parts = append(parts, "伴侣="+lover)
	}
	if parents := socialTieNames(record.Social.ParentUnitIDs, byID); parents != "" {
		parts = append(parts, "父母="+parents)
	}
	if children := socialTieNames(record.Social.ChildUnitIDs, byID); children != "" {
		parts = append(parts, "小孩="+children)
	}
	if len(parts) == 0 {
		return "无已知伴侣、父母或小孩"
	}
	return strings.Join(parts, "；")
}

func socialTieNames(unitIDs []string, byID map[string]*unit.Record) string {
	names := make([]string, 0, len(unitIDs))
	for _, unitID := range unitIDs {
		if name := socialTieName(unitID, byID); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, "、")
}

func socialTieName(unitID string, byID map[string]*unit.Record) string {
	unitID = strings.TrimSpace(unitID)
	if unitID == "" {
		return ""
	}
	if record := byID[unitID]; record != nil {
		return fmt.Sprintf("%s[%s]", record.DisplayName(), record.FactionID)
	}
	return unitID
}

func formatInventoryStacksForLLM(stacks []unit.ItemStack) string {
	if len(stacks) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(stacks))
	for _, stack := range stacks {
		if stack.ItemID == "" || stack.Quantity <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s x%d(%s)", displayStackName(stack), stack.Quantity, formatItemStackEffect(stack)))
	}
	if len(parts) == 0 {
		return "无"
	}
	if len(parts) > 6 {
		parts = parts[:6]
	}
	return strings.Join(parts, "/")
}

func formatEquipmentStacksForLLM(equipment map[string]unit.ItemStack) string {
	if len(equipment) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(equipment))
	for slot, stack := range equipment {
		if stack.ItemID == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s(%s)", slot, displayStackName(stack), formatItemStackEffect(stack)))
	}
	if len(parts) == 0 {
		return "无"
	}
	sort.Strings(parts)
	return strings.Join(parts, "/")
}

// summarizeDialogueHistory 汇总最近对话并附回合相对时间标签。
func summarizeDialogueHistory(history []DialogueMessage, unitID string, currentTurn int, limit int) string {
	lines := make([]string, 0, limit)
	for index := len(history) - 1; index >= 0 && len(lines) < limit; index-- {
		if history[index].UnitID != unitID {
			continue
		}
		entry := history[index]
		lines = append(lines, fmt.Sprintf("%s: %s（%s）", entry.Speaker, entry.Message, relativeTurnLabel(currentTurn, entry.Turn)))
	}

	if len(lines) == 0 {
		return "无"
	}

	reverseStrings(lines)
	return strings.Join(lines, "\n")
}

// summarizeLogs 返回最近事件，并附上“本回合/N回合前”时间标签，便于模型理解时序。
func summarizeLogs(logs []LogEntry, currentTurn int, limit int) string {
	if len(logs) == 0 {
		return "无"
	}

	start := len(logs) - limit
	if start < 0 {
		start = 0
	}

	lines := make([]string, 0, len(logs)-start)
	for _, entry := range logs[start:] {
		lines = append(lines, fmt.Sprintf("%s（%s）", entry.Message, relativeTurnLabel(currentTurn, entry.Turn)))
	}

	return strings.Join(lines, "\n")
}

// relativeTurnLabel 把绝对回合号转换为模型更容易消费的相对时间表达。
func relativeTurnLabel(currentTurn int, eventTurn int) string {
	if eventTurn <= 0 {
		return "时间未知"
	}
	if currentTurn <= 0 {
		return fmt.Sprintf("T%d", eventTurn)
	}
	delta := currentTurn - eventTurn
	if delta <= 0 {
		return "本回合"
	}
	return fmt.Sprintf("%d回合前", delta)
}

// reverseStrings 原地反转字符串切片顺序。
func reverseStrings(items []string) {
	for left, right := 0, len(items)-1; left < right; left, right = left+1, right-1 {
		items[left], items[right] = items[right], items[left]
	}
}

// canonicalizeTargetReference 把目标引用规范化为可用 unit_id。
func canonicalizeTargetReference(
	targetIDs []string,
	byID map[string]*unit.Record,
	value string,
) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}

	if target, ok := byID[value]; ok && isTargetCandidate(targetIDs, target.ID) && isBattleReady(*target) {
		return target.ID
	}

	lowerValue := strings.ToLower(value)
	for _, targetID := range targetIDs {
		target, ok := byID[targetID]
		if !ok || !isBattleReady(*target) {
			continue
		}
		if strings.ToLower(target.ID) == lowerValue || strings.ToLower(target.DisplayName()) == lowerValue {
			return target.ID
		}
	}

	return value
}

// isTargetCandidate 判断目标是否属于当前候选目标集合。
func isTargetCandidate(targetIDs []string, targetID string) bool {
	for _, candidateID := range targetIDs {
		if candidateID == targetID {
			return true
		}
	}
	return false
}

// reachableMoveOptions 计算单位本回合可到达坐标集合。
func reachableMoveOptions(
	snapshot world.MapSnapshot,
	byID map[string]*unit.Record,
	actor *unit.Record,
) []world.Coord {
	options := make([]world.Coord, 0, 6)
	current := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	for _, coord := range axialNeighbors(current) {
		if !inBounds(snapshot, coord) || occupiedByAnother(byID, actor.ID, coord) {
			continue
		}
		options = append(options, coord)
	}

	sort.Slice(options, func(i, j int) bool {
		if options[i].Q != options[j].Q {
			return options[i].Q < options[j].Q
		}
		return options[i].R < options[j].R
	})
	return options
}

// buildDecisionCandidates 汇总战斗/移动/生产等候选动作并按 AP 过滤。
func buildDecisionCandidates(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	remainingAP int,
) []decisionCandidate {
	snapshot := state.Map
	candidates := []decisionCandidate{{
		ID:      "hold",
		Action:  DecisionActionHold,
		Summary: "原地观察，暂不推进。",
	}}
	if remainingAP <= 0 {
		return candidates
	}

	pregnant := isUnitPregnant(state, actor.ID)
	reach := attackReachWithWeather(state, *actor)
	weatherMoveMultiplier := weatherAdjustedMoveMultiplier(state, *actor)
	if !pregnant {
		for _, targetID := range targetIDs {
			target, ok := byID[targetID]
			if !ok || !isBattleReady(*target) {
				continue
			}
			distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
			if remainingAP >= decisionActionCost(DecisionActionAttack) {
				if distance <= reach {
					candidates = append(candidates, decisionCandidate{
						ID:           fmt.Sprintf("attack:%s", target.ID),
						Action:       DecisionActionAttack,
						TargetUnitID: target.ID,
						Summary:      fmt.Sprintf("进攻 %s。", target.DisplayName()),
					})
				}
			}
			if remainingAP >= decisionActionCost(DecisionActionHeavyAttack) {
				if distance <= reach+1 {
					candidates = append(candidates, decisionCandidate{
						ID:           fmt.Sprintf("heavy:%s", target.ID),
						Action:       DecisionActionHeavyAttack,
						TargetUnitID: target.ID,
						Summary:      fmt.Sprintf("重击 %s（1.5x 伤害，命中率较低）。", target.DisplayName()),
					})
				}
			}
			if remainingAP >= decisionActionCost(DecisionActionCharge) && effectiveMoveRange(actor.Status.Move, weatherMoveMultiplier) >= 1 {
				if distance <= reach+2 {
					candidates = append(candidates, decisionCandidate{
						ID:           fmt.Sprintf("charge:%s", target.ID),
						Action:       DecisionActionCharge,
						TargetUnitID: target.ID,
						Summary:      fmt.Sprintf("冲锋接敌 %s（2 格机动后立刻攻击）。", target.DisplayName()),
					})
				}
			}
		}
		candidates = append(candidates, buildStructureCombatCandidates(state, actor, remainingAP)...)
	}
	for _, equipment := range buildEquipmentCandidates(state, actor) {
		if decisionActionCost(equipment.Action) > remainingAP {
			continue
		}
		candidates = append(candidates, equipment)
	}
	if remainingAP >= decisionActionCost(DecisionActionEat) && shouldOfferEatCandidate(actor) {
		eatSummary := fmt.Sprintf("吃下一份口粮，恢复 35 点饥饿度（当前饥饿 %d）。", actor.Status.Hunger)
		if actor.Status.Hunger < 30 {
			eatSummary = fmt.Sprintf("紧急吃下一份口粮，恢复 35 点饥饿度，避免饥饿恶化或归零死亡（当前饥饿 %d）。", actor.Status.Hunger)
		}
		candidates = append(candidates, decisionCandidate{
			ID:      "eat:ration",
			Action:  DecisionActionEat,
			ItemID:  "ration",
			Summary: eatSummary,
		})
	}
	if remainingAP >= decisionActionCost(DecisionActionEat) && shouldOfferHealingPotionCandidate(actor) {
		candidates = append(candidates, decisionCandidate{
			ID:      "heal:healing_potion",
			Action:  DecisionActionEat,
			ItemID:  "healing_potion",
			Summary: fmt.Sprintf("喝下一瓶治疗药剂，恢复 25 HP（当前 HP %d）。", actor.Status.HP),
		})
	}
	if remainingAP >= decisionActionCost(DecisionActionPickup) {
		for _, drop := range groundLootAtCoord(state, actor.Status.PositionQ, actor.Status.PositionR) {
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("pickup:%s", drop.ID),
				Action:       DecisionActionPickup,
				GroundLootID: drop.ID,
				Summary:      fmt.Sprintf("拾取脚下遗落物：%s。", formatItemStacksWithEffects(drop.Items)),
			})
		}
	}

	economyCandidates := buildEconomyCandidates(state, byID, actor)
	prioritizeEconomy := shouldPrioritizeEconomyAction(state, byID, actor, economyCandidates)
	if !prioritizeEconomy && directivePrefersBuild(directiveForUnit(state, actor.ID, actor.FactionID)) && hasDecisionCandidateAction(economyCandidates, DecisionActionBuild) {
		prioritizeEconomy = true
	}
	appendAffordableEconomy := func() {
		for _, economy := range economyCandidates {
			if decisionActionCost(economy.Action) > remainingAP {
				continue
			}
			if pregnant && (economy.Action == DecisionActionBuild || economy.Action == DecisionActionDemolish) {
				continue
			}
			candidates = append(candidates, economy)
		}
	}

	if prioritizeEconomy {
		appendAffordableEconomy()
	}

	if !pregnant && remainingAP >= decisionActionCost(DecisionActionDefend) {
		candidates = append(candidates, decisionCandidate{
			ID:      "defend",
			Action:  DecisionActionDefend,
			Summary: "进入防御姿态，降低受击伤害。",
		})
		candidates = append(candidates, decisionCandidate{
			ID:      "observe",
			Action:  DecisionActionObserve,
			Summary: "观察校准，下一次攻击更精准。",
		})
	}

	if !pregnant && remainingAP >= decisionActionCost(DecisionActionAssist) {
		for _, allyID := range alliedIDs(state, actor.FactionID) {
			if allyID == actor.ID {
				continue
			}
			ally, ok := byID[allyID]
			if !ok || !isBattleReady(*ally) {
				continue
			}
			if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) > 1 {
				continue
			}
			candidates = append(candidates, decisionCandidate{
				ID:           fmt.Sprintf("assist:%s", ally.ID),
				Action:       DecisionActionAssist,
				TargetUnitID: ally.ID,
				Summary:      fmt.Sprintf("援助相邻队友 %s，增强其抗压。", ally.DisplayName()),
			})
		}
	}

	if remainingAP >= decisionActionCost(DecisionActionSay) {
		candidates = append(candidates, buildSayCandidates(state, byID, actor)...)
	}

	if remainingAP >= decisionActionCost(DecisionActionDialogue) {
		candidates = append(candidates, buildDialogueCandidates(state, byID, actor)...)
	}

	if remainingAP >= decisionActionCost(DecisionActionTrade) {
		candidates = append(candidates, buildTradeCandidates(state, byID, actor)...)
	}

	if remainingAP >= decisionActionCost(DecisionActionRomance) {
		candidates = append(candidates, buildRomanceCandidates(state, byID, actor)...)
	}

	if !pregnant {
		candidates = append(candidates, buildSkillCandidates(state, byID, actor, targetIDs, remainingAP)...)
	}

	if !prioritizeEconomy {
		appendAffordableEconomy()
	}

	if remainingAP >= decisionActionCost(DecisionActionMove) {
		for _, option := range reachableMoveOptions(snapshot, byID, actor) {
			candidates = append(candidates, decisionCandidate{
				ID:      fmt.Sprintf("move:%d:%d", option.Q, option.R),
				Action:  DecisionActionMove,
				TargetQ: option.Q,
				TargetR: option.R,
				Summary: moveCandidateSummary(state, option),
			})
		}
	}

	return candidates
}

// shouldOfferEatCandidate 判断是否把吃口粮作为正式候选动作交给单位决策。
func shouldOfferEatCandidate(actor *unit.Record) bool {
	if actor == nil || actor.Status.LifeState != unit.LifeStateActive {
		return false
	}
	if actor.Status.Hunger >= 85 {
		return false
	}
	return hasBackpackItem(*actor, "ration")
}

func shouldOfferHealingPotionCandidate(actor *unit.Record) bool {
	if actor == nil || actor.Status.LifeState != unit.LifeStateActive || actor.Status.HP >= 100 {
		return false
	}
	return hasBackpackItem(*actor, "healing_potion")
}

// hasDecisionCandidateAction 判断候选列表里是否包含指定动作。
func hasDecisionCandidateAction(candidates []decisionCandidate, action DecisionAction) bool {
	for _, candidate := range candidates {
		if candidate.Action == action {
			return true
		}
	}
	return false
}

// shouldPrioritizeEconomyAction 判断当前局势下是否应把经济类候选提前给模型。
// 敌军尚远而脚下已有合法经济动作时，优先展示 build/gather，避免默认掉进稳阵/原地观察。
func shouldPrioritizeEconomyAction(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	economyCandidates []decisionCandidate,
) bool {
	if actor == nil || len(economyCandidates) == 0 {
		return false
	}
	if actor.Status.HP < 70 {
		return false
	}
	if nearestThreatDistance(state, byID, actor) < 8 {
		return false
	}
	for _, allyID := range alliedIDs(state, actor.FactionID) {
		if allyID == actor.ID {
			continue
		}
		ally := byID[allyID]
		if ally == nil || !isBattleReady(*ally) {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) <= 1 {
			return true
		}
	}
	return false
}

// formatDecisionCandidates 格式化候选动作列表为提示词文本。
func formatDecisionCandidates(candidates []decisionCandidate, byID map[string]*unit.Record) string {
	groups := make([]decisionCandidatePromptGroup, 0, len(candidates))
	groupIndex := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		summary := decisionCandidatePromptSummary(candidate, byID)
		if candidate.Action == DecisionActionAttack || candidate.Action == DecisionActionHeavyAttack || candidate.Action == DecisionActionCharge {
			if target, ok := byID[candidate.TargetUnitID]; ok {
				if candidate.Action == DecisionActionHeavyAttack {
					summary = fmt.Sprintf("重击 %s（1.5x 伤害，命中率较低）。", target.DisplayName())
				} else if candidate.Action == DecisionActionCharge {
					summary = fmt.Sprintf("冲锋接敌 %s（2 格机动后立刻攻击）。", target.DisplayName())
				} else {
					summary = fmt.Sprintf("进攻 %s。", target.DisplayName())
				}
			}
		}
		apCost := candidate.APCost
		if apCost <= 0 {
			apCost = decisionActionCost(candidate.Action)
		}
		key := fmt.Sprintf("%s:ap=%d", candidate.Action, apCost)
		index, exists := groupIndex[key]
		if !exists {
			groupIndex[key] = len(groups)
			groups = append(groups, decisionCandidatePromptGroup{
				Action: candidate.Action,
				APCost: apCost,
			})
			index = len(groups) - 1
		}
		groups[index].Summaries = appendUniqueText(groups[index].Summaries, summary)
		groups[index].Candidates = append(groups[index].Candidates, candidate)
	}
	lines := make([]string, 0, len(groups))
	for _, group := range groups {
		params := formatDecisionActionParamSlots(group.Action, group.Candidates)
		detailParts := make([]string, 0, 3)
		if len(group.Summaries) > 0 {
			detailParts = append(detailParts, strings.Join(trimSentenceEnds(limitStringSlice(group.Summaries, 3)), "；"))
		}
		if len(group.Summaries) > 3 {
			detailParts = append(detailParts, fmt.Sprintf("另有%d条同类说明", len(group.Summaries)-3))
		}
		if group.Action == DecisionActionMove {
			if movePairs := formatLegalMovePairsForCorrection(candidates); movePairs != "" {
				detailParts = append(detailParts, "合法 move 坐标对："+movePairs)
			}
		}
		if rule := decisionCandidateGroupRule(group.Action); rule != "" {
			detailParts = append(detailParts, "规则："+rule)
		}
		if len(detailParts) == 0 {
			detailParts = append(detailParts, "按动作解释和参数槽位填写")
		}
		lines = append(lines, fmt.Sprintf("- %s{%s}: AP=%d // %s。", group.Action, params, group.APCost, strings.Join(detailParts, "；")))
	}
	return strings.Join(lines, "\n")
}

type decisionCandidatePromptGroup struct {
	Action     DecisionAction
	APCost     int
	Summaries  []string
	Candidates []decisionCandidate
}

func formatDecisionParameterGuide(candidates []decisionCandidate, byID map[string]*unit.Record) string {
	if len(candidates) == 0 {
		return "无"
	}
	byAction := make(map[DecisionAction][]decisionCandidate)
	for _, candidate := range candidates {
		byAction[candidate.Action] = append(byAction[candidate.Action], candidate)
	}
	actions := make([]string, 0, len(byAction))
	for action := range byAction {
		actions = append(actions, string(action))
	}
	sort.Strings(actions)

	lines := make([]string, 0, len(actions))
	for _, actionText := range actions {
		action := DecisionAction(actionText)
		actionCandidates := byAction[action]
		switch action {
		case DecisionActionTrade:
			lines = append(lines, fmt.Sprintf("- trade：target_unit_id 从相邻交易对象中选择（%s）；trade_kind 只能为 gift/gold/sell；gift/sell 的 item_id 从自己背包物品中选（%s）；gold_amount 必须为正且不超过钱包/候选上限（%s）；sell 的 price 必须为正且对方买得起。", summarizeCandidateTargets(actionCandidates, byID), summarizeCandidateItems(actionCandidates), summarizeCandidateGoldRange(actionCandidates)))
		case DecisionActionSay, DecisionActionDialogue, DecisionActionAssist, DecisionActionRomance, DecisionActionFamily:
			lines = append(lines, fmt.Sprintf("- %s：target_unit_id 从可交互单位中选择（%s），可写完整单位ID或准确姓名；目标必须满足视野/相邻/关系规则。", action, summarizeCandidateTargets(actionCandidates, byID)))
		case DecisionActionMove:
			lines = append(lines, fmt.Sprintf("- move：target_q 和 target_r 必须来自“本回合可到达地块与可执行事项”里的同一行坐标（%s），不能填写当前坐标、单位坐标或视野列表里的远处坐标。", summarizeCandidateMovePairs(actionCandidates)))
		case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
			lines = append(lines, fmt.Sprintf("- %s：攻击单位时填写 target_unit_id（%s），攻击设施时填写 structure_id（%s）；目标必须在当前攻击距离内。", action, summarizeCandidateTargets(actionCandidates, byID), summarizeCandidateStructures(actionCandidates)))
		case DecisionActionSkill:
			lines = append(lines, fmt.Sprintf("- skill：skill_id 从当前可用技能中选择（%s）；技能需要目标时再填写合法 target_unit_id（%s）。", summarizeCandidateSkills(actionCandidates), summarizeCandidateTargets(actionCandidates, byID)))
		case DecisionActionGather:
			lines = append(lines, fmt.Sprintf("- gather：activity 从脚下地块实际支持的采集/生产类型中选择（%s）。", summarizeCandidateActivities(actionCandidates)))
		case DecisionActionBuild:
			lines = append(lines, fmt.Sprintf("- build：structure_type 从脚下地块、材料和当前建筑状态允许建设的设施中选择（%s）。", summarizeCandidateStructureTypes(actionCandidates)))
		case DecisionActionForge:
			lines = append(lines, fmt.Sprintf("- forge：item_id 从当前铁匠铺和材料允许锻造的装备中选择（%s）。", summarizeCandidateItems(actionCandidates)))
		case DecisionActionUpgrade:
			lines = append(lines, fmt.Sprintf("- upgrade：item_id 从当前铁匠铺、材料和背包装备允许强化的物品中选择（%s）。", summarizeCandidateItems(actionCandidates)))
		case DecisionActionEquip:
			lines = append(lines, fmt.Sprintf("- equip：item_id 从自己背包中可装备物品里选择（%s）。", summarizeCandidateItems(actionCandidates)))
		case DecisionActionEat:
			lines = append(lines, fmt.Sprintf("- eat：item_id 从自己背包中可食用食物或药剂里选择（%s）。", summarizeCandidateItems(actionCandidates)))
		case DecisionActionPickup:
			lines = append(lines, fmt.Sprintf("- pickup：ground_loot_id 可留空或填写脚下仍存在的掉落物ID（%s）。", summarizeCandidateGroundLoot(actionCandidates)))
		case DecisionActionDemolish:
			lines = append(lines, fmt.Sprintf("- demolish：structure_id 可留空或填写脚下友方设施ID（%s）。", summarizeCandidateStructures(actionCandidates)))
		default:
			if len(actionCandidates) > 0 {
				lines = append(lines, fmt.Sprintf("- %s：该动作不需要额外结构化参数，只填写 action、next_action、speak、memory、reasoning。", action))
			}
		}
	}
	if len(lines) == 0 {
		return "无"
	}
	return strings.Join(lines, "\n")
}

func formatDecisionActionParamSlots(action DecisionAction, candidates []decisionCandidate) string {
	has := func(check func(decisionCandidate) bool) bool {
		for _, candidate := range candidates {
			if check(candidate) {
				return true
			}
		}
		return false
	}
	params := make([]string, 0, 6)
	appendParam := func(key string, slot string) {
		params = append(params, fmt.Sprintf("%s=<%s>", key, slot))
	}
	switch action {
	case DecisionActionMove:
		appendParam("target_q", "合法相邻坐标q")
		appendParam("target_r", "同一坐标行的r")
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
		if has(func(candidate decisionCandidate) bool { return candidate.TargetUnitID != "" }) {
			appendParam("target_unit_id", "可攻击单位ID或姓名")
		}
		if has(func(candidate decisionCandidate) bool { return candidate.StructureID != "" }) {
			appendParam("structure_id", "可攻击设施ID")
		}
	case DecisionActionSkill:
		appendParam("skill_id", "可用技能ID")
		if has(func(candidate decisionCandidate) bool { return candidate.TargetUnitID != "" }) {
			appendParam("target_unit_id", "技能合法目标")
		}
	case DecisionActionGather:
		appendParam("activity", "脚下可采集类型")
	case DecisionActionBuild:
		appendParam("structure_type", "脚下可建设施")
	case DecisionActionTrade:
		appendParam("trade_kind", "gift|gold|sell")
		appendParam("target_unit_id", "相邻交易对象")
		if has(func(candidate decisionCandidate) bool { return candidate.ItemID != "" }) {
			appendParam("item_id", "背包物品ID或名称")
		}
		if has(func(candidate decisionCandidate) bool { return candidate.GoldAmount > 0 }) {
			appendParam("gold_amount", "正整数金币")
		}
		if has(func(candidate decisionCandidate) bool { return candidate.Price > 0 }) {
			appendParam("price", "正整数售价")
		}
	case DecisionActionSay, DecisionActionDialogue, DecisionActionAssist, DecisionActionRomance, DecisionActionFamily:
		appendParam("target_unit_id", "可交互单位ID或姓名")
	case DecisionActionForge:
		appendParam("item_id", "可锻造装备ID或名称")
	case DecisionActionUpgrade:
		appendParam("item_id", "可强化装备ID或名称")
	case DecisionActionEquip:
		appendParam("item_id", "背包装备ID或名称")
	case DecisionActionEat:
		appendParam("item_id", "食物或药剂ID或名称")
	case DecisionActionPickup:
		if has(func(candidate decisionCandidate) bool { return candidate.GroundLootID != "" }) {
			appendParam("ground_loot_id", "脚下掉落物ID")
		}
	case DecisionActionDemolish:
		if has(func(candidate decisionCandidate) bool { return candidate.StructureID != "" }) {
			appendParam("structure_id", "脚下友方设施ID")
		}
	}
	if len(params) == 0 {
		return "无"
	}
	return strings.Join(params, ", ")
}

func summarizeCandidateTargets(candidates []decisionCandidate, byID map[string]*unit.Record) string {
	values := make([]string, 0, 4)
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.TargetUnitID) == "" {
			continue
		}
		label := candidate.TargetUnitID
		if target := byID[candidate.TargetUnitID]; target != nil {
			label = fmt.Sprintf("%s=%s", target.DisplayName(), target.ID)
		}
		values = appendUniqueText(values, label)
	}
	return summarizeCandidateValues(values, "无")
}

func summarizeCandidateItems(candidates []decisionCandidate) string {
	values := make([]string, 0, 6)
	for _, candidate := range candidates {
		itemID := strings.TrimSpace(candidate.ItemID)
		if itemID == "" {
			continue
		}
		values = appendUniqueText(values, fmt.Sprintf("%s=%s", displayItemName(itemID), itemID))
	}
	return summarizeCandidateValues(values, "无")
}

func summarizeCandidateGoldRange(candidates []decisionCandidate) string {
	maxAmount := 0
	for _, candidate := range candidates {
		if candidate.GoldAmount > maxAmount {
			maxAmount = candidate.GoldAmount
		}
	}
	if maxAmount <= 0 {
		return "无"
	}
	return fmt.Sprintf("1..%d", maxAmount)
}

func summarizeCandidateMovePairs(candidates []decisionCandidate) string {
	values := make([]string, 0, 6)
	for _, candidate := range candidates {
		if candidate.Action != DecisionActionMove {
			continue
		}
		values = appendUniqueText(values, fmt.Sprintf("%d,%d", candidate.TargetQ, candidate.TargetR))
	}
	sort.Strings(values)
	return summarizeCandidateValues(values, "无")
}

func summarizeCandidateStructures(candidates []decisionCandidate) string {
	values := make([]string, 0, 4)
	for _, candidate := range candidates {
		structureID := strings.TrimSpace(candidate.StructureID)
		if structureID == "" {
			continue
		}
		values = appendUniqueText(values, structureID)
	}
	return summarizeCandidateValues(values, "无")
}

func summarizeCandidateSkills(candidates []decisionCandidate) string {
	values := make([]string, 0, 3)
	for _, candidate := range candidates {
		skillID := strings.TrimSpace(candidate.SkillID)
		if skillID == "" {
			continue
		}
		values = appendUniqueText(values, skillID)
	}
	return summarizeCandidateValues(values, "无")
}

func summarizeCandidateActivities(candidates []decisionCandidate) string {
	values := make([]string, 0, 4)
	for _, candidate := range candidates {
		if candidate.Activity == "" {
			continue
		}
		values = appendUniqueText(values, fmt.Sprintf("%s=%s", productionActivityDisplayName(candidate.Activity), candidate.Activity))
	}
	return summarizeCandidateValues(values, "无")
}

func summarizeCandidateStructureTypes(candidates []decisionCandidate) string {
	values := make([]string, 0, 4)
	for _, candidate := range candidates {
		if candidate.StructureType == "" {
			continue
		}
		values = appendUniqueText(values, fmt.Sprintf("%s=%s", structureDisplayName(candidate.StructureType), candidate.StructureType))
	}
	return summarizeCandidateValues(values, "无")
}

func summarizeCandidateGroundLoot(candidates []decisionCandidate) string {
	values := make([]string, 0, 4)
	for _, candidate := range candidates {
		lootID := strings.TrimSpace(candidate.GroundLootID)
		if lootID == "" {
			continue
		}
		values = appendUniqueText(values, lootID)
	}
	return summarizeCandidateValues(values, "可留空")
}

func summarizeCandidateValues(values []string, empty string) string {
	if len(values) == 0 {
		return empty
	}
	limit := 8
	if len(values) <= limit {
		return strings.Join(values, "；")
	}
	return strings.Join(values[:limit], "；") + fmt.Sprintf("；另有%d项", len(values)-limit)
}

func appendUniqueText(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func limitStringSlice(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[:limit]
}

func trimSentenceEnds(values []string) []string {
	trimmed := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimRight(strings.TrimSpace(value), "。；")
		if value != "" {
			trimmed = append(trimmed, value)
		}
	}
	return trimmed
}

func decisionCandidateGroupRule(action DecisionAction) string {
	switch action {
	case DecisionActionMove:
		return "只能从“本回合可到达地块与可执行事项”逐字选择一行坐标填写 target_q/target_r；不要填写视野地形列表、自己/友军/敌军所在坐标；目标格有单位或不在该列表中会失败。"
	case DecisionActionTrade:
		return "target_unit_id 必须照抄相邻交易对象的完整单位ID，不能截断 UUID；trade_kind 为 gift/gold/sell；gift/sell 时 item_id 可填写背包物品ID或名称；gold_amount/price 必须为正整数且钱包足够。"
	case DecisionActionSay, DecisionActionDialogue, DecisionActionAssist, DecisionActionRomance, DecisionActionFamily:
		return "target_unit_id 可填写可交互目标的单位ID或姓名；目标必须在视野/相邻交互规则允许范围内。"
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
		return "target_unit_id 可填写合法敌方单位ID或姓名；若攻击设施则填写 structure_id；目标必须在当前动作距离规则内。"
	case DecisionActionSkill:
		return "skill_id 必须是当前可用技能；需要目标时 target_unit_id 可填写合法单位ID或姓名。"
	case DecisionActionGather:
		return "activity 可填写当前地块支持的采集类型ID或中文名称，如采集/挖矿/钓鱼/打猎/收割农田。"
	case DecisionActionBuild:
		return "structure_type 可填写当前地块和材料允许建设的设施类型ID或中文名称。"
	case DecisionActionEquip, DecisionActionEat, DecisionActionForge, DecisionActionUpgrade:
		return "item_id 可填写背包/可制作/可强化物品的ID或名称，必须满足装备、食用、锻造或强化规则。"
	case DecisionActionPickup:
		return "ground_loot_id 可留空或填写脚下掉落物ID；只能拾取脚下仍存在的掉落物。"
	case DecisionActionDemolish:
		return "structure_id 可留空或填写脚下友方设施ID；只能拆除脚下友方设施。"
	}
	return ""
}

func decisionCandidateExample(candidate decisionCandidate, byID map[string]*unit.Record) string {
	switch candidate.Action {
	case DecisionActionMove:
		return fmt.Sprintf("%d,%d", candidate.TargetQ, candidate.TargetR)
	case DecisionActionTrade:
		targetName := candidate.TargetUnitID
		if target := byID[candidate.TargetUnitID]; target != nil {
			targetName = target.DisplayName()
		}
		switch candidate.TradeKind {
		case TradeActionKindGift:
			return fmt.Sprintf("gift %s 给 %s", displayItemName(candidate.ItemID), targetName)
		case TradeActionKindGold:
			return fmt.Sprintf("gold 给 %s，上限示例%d", targetName, candidate.GoldAmount)
		case TradeActionKindSell:
			return fmt.Sprintf("sell %s 给 %s", displayItemName(candidate.ItemID), targetName)
		}
	case DecisionActionSay, DecisionActionDialogue, DecisionActionAssist, DecisionActionRomance, DecisionActionFamily:
		if target := byID[candidate.TargetUnitID]; target != nil {
			return target.DisplayName()
		}
		return candidate.TargetUnitID
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack, DecisionActionSkill:
		if target := byID[candidate.TargetUnitID]; target != nil {
			return target.DisplayName()
		}
		return firstNonEmptyText(candidate.TargetUnitID, candidate.StructureID)
	case DecisionActionGather:
		return string(candidate.Activity)
	case DecisionActionBuild:
		return string(candidate.StructureType)
	case DecisionActionEquip, DecisionActionEat, DecisionActionForge, DecisionActionUpgrade:
		return displayItemName(candidate.ItemID)
	case DecisionActionPickup:
		return candidate.GroundLootID
	case DecisionActionDemolish:
		return candidate.StructureID
	}
	return ""
}

func validateNoLegacyDecisionPrompt(prompt string) error {
	legacyFragments := []string{
		"option_",
		"[AP=",
		"candidate_id，不能自造动作",
		"选一个 candidate_id",
	}
	for _, fragment := range legacyFragments {
		if strings.Contains(prompt, fragment) {
			return fmt.Errorf("decision prompt still contains legacy candidate format %q", fragment)
		}
	}
	return nil
}

func decisionCandidatePromptSummary(candidate decisionCandidate, byID map[string]*unit.Record) string {
	switch candidate.Action {
	case DecisionActionMove:
		return "移动到一个相邻合法地块；具体 target_q/target_r 从“本回合可到达地块与可执行事项”中选择。"
	case DecisionActionTrade:
		targetName := "相邻目标"
		if target := byID[candidate.TargetUnitID]; target != nil {
			targetName = target.DisplayName()
		}
		switch candidate.TradeKind {
		case TradeActionKindGift:
			return fmt.Sprintf("向 %s 赠送一件你背包中的物品；item_id 由你根据背包和目标需求填写。", targetName)
		case TradeActionKindGold:
			return fmt.Sprintf("向 %s 调拨一笔金币；gold_amount 由你按局势自行填写。", targetName)
		case TradeActionKindSell:
			return fmt.Sprintf("向 %s 出售一件你背包中的物品；item_id 与 price 都由你自行填写并接受规则校验。", targetName)
		}
	case DecisionActionGather:
		return strings.TrimSpace("在当前地块执行一种合法采集/生产；activity 由你从当前地块机会中填写。" + " " + candidate.Summary)
	case DecisionActionBuild:
		return strings.TrimSpace("在当前地块建造或续建一种合法设施；structure_type 由你按地形与材料填写。" + " " + candidate.Summary)
	}
	return candidate.Summary
}

func formatDecisionCandidateDomain(candidate decisionCandidate, byID map[string]*unit.Record) string {
	parts := make([]string, 0, 4)
	switch candidate.Action {
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
		if candidate.TargetUnitID != "" {
			parts = append(parts, fmt.Sprintf("target_unit_id 可选域：%s", candidate.TargetUnitID))
		}
		if candidate.StructureID != "" {
			parts = append(parts, fmt.Sprintf("structure_id 可选域：%s", candidate.StructureID))
		}
	case DecisionActionMove:
		parts = append(parts, fmt.Sprintf("target_q/target_r 可选域：%d,%d", candidate.TargetQ, candidate.TargetR))
	case DecisionActionGather:
		if candidate.Activity != "" {
			parts = append(parts, fmt.Sprintf("activity 可选域：%s", candidate.Activity))
		}
	case DecisionActionBuild:
		if candidate.StructureType != "" {
			parts = append(parts, fmt.Sprintf("structure_type 可选域：%s", candidate.StructureType))
		}
	case DecisionActionSkill:
		if candidate.SkillID != "" {
			parts = append(parts, fmt.Sprintf("skill_id 可选域：%s", candidate.SkillID))
		}
		if candidate.TargetUnitID != "" {
			parts = append(parts, fmt.Sprintf("target_unit_id 可选域：%s", candidate.TargetUnitID))
		}
	case DecisionActionEquip, DecisionActionEat, DecisionActionForge, DecisionActionUpgrade:
		if candidate.ItemID != "" {
			parts = append(parts, fmt.Sprintf("item_id 可选域：%s", candidate.ItemID))
		}
	case DecisionActionPickup:
		if candidate.GroundLootID != "" {
			parts = append(parts, fmt.Sprintf("ground_loot_id 可选域：%s", candidate.GroundLootID))
		}
	case DecisionActionDemolish:
		if candidate.StructureID != "" {
			parts = append(parts, fmt.Sprintf("structure_id 可选域：%s", candidate.StructureID))
		}
	}
	if candidate.Action == DecisionActionTrade || candidate.Action == DecisionActionSay || candidate.Action == DecisionActionDialogue || candidate.Action == DecisionActionAssist || candidate.Action == DecisionActionRomance || candidate.Action == DecisionActionFamily {
		if target := byID[candidate.TargetUnitID]; target != nil {
			parts = append(parts, fmt.Sprintf("target_unit_id 可选域：%s（%s）", candidate.TargetUnitID, target.DisplayName()))
		}
	}
	if candidate.Action == DecisionActionTrade {
		switch candidate.TradeKind {
		case TradeActionKindGift:
			parts = append(parts, fmt.Sprintf("trade_kind 可选域：gift；完整target_unit_id：%s；item_id 可选域：%s（%s）", candidate.TargetUnitID, candidate.ItemID, displayItemName(candidate.ItemID)))
		case TradeActionKindGold:
			parts = append(parts, fmt.Sprintf("trade_kind 可选域：gold；完整target_unit_id：%s；gold_amount 可选域：1..%d，由你按意图自拟且不能超过钱包", candidate.TargetUnitID, max(1, candidate.GoldAmount)))
		case TradeActionKindSell:
			parts = append(parts, fmt.Sprintf("trade_kind 可选域：sell；完整target_unit_id：%s；item_id 可选域：%s（%s）；price 由你自拟且对方买得起", candidate.TargetUnitID, candidate.ItemID, displayItemName(candidate.ItemID)))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；")
}

func formatDecisionActionExplanations(candidates []decisionCandidate) string {
	seen := map[DecisionAction]struct{}{}
	lines := make([]string, 0, 8)
	for _, candidate := range candidates {
		if _, exists := seen[candidate.Action]; exists {
			continue
		}
		seen[candidate.Action] = struct{}{}
		if explanation := decisionActionExplanation(candidate.Action); explanation != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", candidate.Action, explanation))
		}
	}
	if len(lines) == 0 {
		return "无"
	}
	return strings.Join(lines, "\n")
}

func decisionActionExplanation(action DecisionAction) string {
	switch action {
	case DecisionActionHold:
		return "原地等待，不消耗 AP。"
	case DecisionActionDefend:
		return "进入防御姿态，降低接下来受到的伤害。"
	case DecisionActionObserve:
		return "观察校准，提高后续攻击稳定性。"
	case DecisionActionMove:
		return "移动到相邻坐标；必须返回 target_q/target_r。"
	case DecisionActionAttack:
		return "普通攻击可见敌方单位或敌方设施；返回 target_unit_id 或 structure_id。"
	case DecisionActionHeavyAttack:
		return "重击，伤害更高但命中更不稳；返回 target_unit_id 或 structure_id。"
	case DecisionActionCharge:
		return "冲锋接敌并攻击；返回 target_unit_id 或 structure_id。"
	case DecisionActionSkill:
		return "施放技能；必须返回 skill_id，若技能需要目标则返回 target_unit_id。"
	case DecisionActionAssist:
		return "援助相邻友军；必须返回 target_unit_id。"
	case DecisionActionSay:
		return "向视野内目标说一句话；返回 target_unit_id，speak 是实际台词。"
	case DecisionActionDialogue:
		return "与视野内目标交谈并交换判断；返回 target_unit_id。"
	case DecisionActionTrade:
		return "赠送/卖出物品或调拨金币；返回 trade_kind、target_unit_id 及 item_id/gold_amount/price 等参数。"
	case DecisionActionRomance:
		return "主动表露心意；返回 target_unit_id，仍需双方同意。"
	case DecisionActionFamily:
		return "与恋人组建家庭；返回 target_unit_id。"
	case DecisionActionGather:
		return "在当前地块采集资源；返回 activity。"
	case DecisionActionBuild:
		return "在当前地块建造设施；返回 structure_type。"
	case DecisionActionForge:
		return "在铁匠铺锻造装备；按候选参数返回。"
	case DecisionActionUpgrade:
		return "在铁匠铺强化装备；按候选参数返回。"
	case DecisionActionEquip:
		return "穿戴背包中的装备；返回 item_id。"
	case DecisionActionEat:
		return "使用食物或药剂；返回 item_id。"
	case DecisionActionPickup:
		return "拾取脚下掉落物；返回 ground_loot_id。"
	case DecisionActionDemolish:
		return "拆除脚下友方设施；返回 structure_id。"
	default:
		return ""
	}
}

func formatDecisionCandidateParams(candidate decisionCandidate) string {
	params := make([]string, 0, 8)
	appendStringParam := func(key string, value string) {
		if strings.TrimSpace(value) != "" {
			params = append(params, fmt.Sprintf("%s=%s", key, strings.TrimSpace(value)))
		}
	}
	appendIntParam := func(key string, value int) {
		if value != 0 {
			params = append(params, fmt.Sprintf("%s=%d", key, value))
		}
	}
	appendStringParam("activity", string(candidate.Activity))
	appendStringParam("skill_id", candidate.SkillID)
	appendStringParam("trade_kind", string(candidate.TradeKind))
	appendStringParam("item_id", candidate.ItemID)
	appendStringParam("item_name", candidate.ItemName)
	appendStringParam("other_item_id", candidate.OtherItemID)
	appendIntParam("price", candidate.Price)
	appendIntParam("gold_amount", candidate.GoldAmount)
	appendStringParam("ground_loot_id", candidate.GroundLootID)
	appendStringParam("structure_id", candidate.StructureID)
	appendStringParam("structure_type", string(candidate.StructureType))
	appendStringParam("target_unit_id", candidate.TargetUnitID)
	if candidate.Action == DecisionActionMove {
		params = append(params, fmt.Sprintf("target_q=%d", candidate.TargetQ), fmt.Sprintf("target_r=%d", candidate.TargetR))
	}
	if len(params) == 0 {
		return "无"
	}
	return strings.Join(params, ", ")
}

func formatDecisionCandidateParamSlots(candidate decisionCandidate) string {
	params := make([]string, 0, 8)
	appendParam := func(key string, slot string) {
		slot = strings.TrimSpace(slot)
		if slot == "" {
			return
		}
		params = append(params, fmt.Sprintf("%s=<%s>", key, slot))
	}
	switch candidate.Action {
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
		if strings.TrimSpace(candidate.TargetUnitID) != "" {
			appendParam("target_unit_id", "可见敌方单位ID")
		}
		if strings.TrimSpace(candidate.StructureID) != "" {
			appendParam("structure_id", "敌方设施ID")
		}
	case DecisionActionSkill:
		appendParam("skill_id", "可用技能ID")
		if strings.TrimSpace(candidate.TargetUnitID) != "" {
			appendParam("target_unit_id", "合法目标单位ID")
		}
	case DecisionActionMove:
		appendParam("target_q", "从可到达列表行首复制")
		appendParam("target_r", "从可到达列表行首复制")
	case DecisionActionGather:
		appendParam("activity", "当前地块可采集类型")
	case DecisionActionBuild:
		appendParam("structure_type", "当前地块可建设施类型")
	case DecisionActionTrade:
		appendParam("trade_kind", "gift|gold|sell")
		appendParam("target_unit_id", "相邻交易对象ID")
		switch candidate.TradeKind {
		case TradeActionKindGift:
			appendParam("item_id", "背包物品ID")
		case TradeActionKindGold:
			appendParam("gold_amount", "自拟正整数金币数")
		case TradeActionKindSell:
			appendParam("item_id", "背包物品ID")
			appendParam("price", "自拟正整数售价")
		}
	case DecisionActionSay, DecisionActionDialogue, DecisionActionAssist, DecisionActionRomance, DecisionActionFamily:
		appendParam("target_unit_id", "可交互单位ID")
	case DecisionActionForge:
		appendParam("item_id", "可锻造装备ID")
	case DecisionActionUpgrade:
		appendParam("item_id", "可强化装备ID")
	case DecisionActionEquip:
		appendParam("item_id", "背包装备ID")
	case DecisionActionEat:
		appendParam("item_id", "可用食物或药剂ID")
	case DecisionActionPickup:
		appendParam("ground_loot_id", "脚下掉落物ID")
	case DecisionActionDemolish:
		appendParam("structure_id", "脚下友方设施ID")
	}
	if len(params) == 0 {
		return "无"
	}
	return strings.Join(params, ", ")
}

func formatDecisionSelectionHint(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	remainingAP int,
	candidates []decisionCandidate,
) string {
	lines := make([]string, 0, 4)
	if movePairs := formatLegalMovePairsForCorrection(candidates); movePairs != "" {
		lines = append(lines, fmt.Sprintf("若选择 move，target_q/target_r 只能使用这些完整坐标对之一：%s。必须整对填写，不能只复制 target_q 后自行猜 target_r；不要填当前位置、敌人坐标、友军坐标或“视野范围内的地形”坐标。", movePairs))
	}
	if recommended, ok := recommendedDecisionCandidate(state, byID, actor, targetIDs, remainingAP, candidates); ok {
		lines = append(lines, fmt.Sprintf("建议优先考虑：%s。", formatRecommendedCandidate(recommended, byID)))
		lines = append(lines, fmt.Sprintf("若采纳建议，JSON 参数照这样填：%s。", formatRecommendedJSONFields(recommended)))
		lines = append(lines, "建议不是强制；如果你的性格、记忆、关系或现场风险支持别的选择，也必须从候选动作类型里选，并按参数填写规则填写合法参数。")
	}
	if len(lines) == 0 {
		return "没有额外提示；按候选动作列表选择。"
	}
	return strings.Join(lines, "\n")
}

func actorDirectiveFocusForPrompt(state State, byID map[string]*unit.Record, actor *unit.Record) string {
	if actor == nil {
		return "无"
	}
	directive := directiveForUnit(state, actor.ID, actor.FactionID)
	actorClauses, namedClauses := actorNamedDirectiveClauses(directive, byID, actor)
	if len(actorClauses) > 0 {
		return "可能与你直接相关：" + strings.Join(actorClauses, "；")
	}
	if len(namedClauses) > 0 {
		return "方针包含点名任务，但未识别到直接点名你；按全局意图和现场情况判断。"
	}
	return "未识别到点名任务；按全局意图和自身判断行动。"
}

func actorDirectiveForRecommendation(state State, byID map[string]*unit.Record, actor *unit.Record) string {
	if actor == nil {
		return ""
	}
	directive := directiveForUnit(state, actor.ID, actor.FactionID)
	actorClauses, namedClauses := actorNamedDirectiveClauses(directive, byID, actor)
	if len(actorClauses) > 0 {
		return strings.Join(actorClauses, "\n")
	}
	if len(namedClauses) > 0 {
		return ""
	}
	return directive
}

func actorNamedDirectiveClauses(directive string, byID map[string]*unit.Record, actor *unit.Record) ([]string, []string) {
	actorClauses := make([]string, 0, 2)
	namedClauses := make([]string, 0, 2)
	for _, clause := range splitDirectiveClauses(directive) {
		if directiveClauseMentionsUnit(clause, actor) {
			actorClauses = append(actorClauses, directiveSegmentForUnit(clause, byID, actor))
			namedClauses = append(namedClauses, clause)
			continue
		}
		if directiveClauseMentionsFactionUnit(clause, byID, actor.FactionID) {
			namedClauses = append(namedClauses, clause)
		}
	}
	return actorClauses, namedClauses
}

func directiveSegmentForUnit(clause string, byID map[string]*unit.Record, actor *unit.Record) string {
	clause = strings.TrimSpace(clause)
	if clause == "" || actor == nil {
		return clause
	}
	lowerClause := strings.ToLower(clause)
	actorStart := -1
	actorEnd := -1
	for _, alias := range unitDirectiveAliases(*actor) {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias == "" {
			continue
		}
		if index := strings.Index(lowerClause, alias); index >= 0 && (actorStart < 0 || index < actorStart) {
			actorStart = index
			actorEnd = index + len(alias)
		}
	}
	if actorStart < 0 {
		return clause
	}

	nextStart := len(clause)
	for _, record := range byID {
		if record == nil || record.ID == actor.ID || record.FactionID != actor.FactionID {
			continue
		}
		for _, alias := range unitDirectiveAliases(*record) {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if alias == "" {
				continue
			}
			searchFrom := actorEnd
			if searchFrom < 0 || searchFrom > len(lowerClause) {
				searchFrom = actorStart + 1
			}
			if index := strings.Index(lowerClause[searchFrom:], alias); index >= 0 {
				absolute := searchFrom + index
				if absolute > actorStart && absolute < nextStart {
					nextStart = absolute
				}
			}
		}
	}
	if nextStart < len(clause) {
		between := strings.TrimSpace(clause[actorEnd:nextStart])
		if strings.ContainsAny(between, "和与跟同、") {
			nextStart = nextUnitMentionAfter(lowerClause, byID, actor, actor.FactionID, nextStart+1)
		}
	}

	segment := strings.TrimSpace(clause[actorStart:nextStart])
	if segment == "" {
		return clause
	}
	return segment
}

func nextUnitMentionAfter(lowerClause string, byID map[string]*unit.Record, actor *unit.Record, factionID string, offset int) int {
	next := len(lowerClause)
	if offset < 0 {
		offset = 0
	}
	if offset > len(lowerClause) {
		return next
	}
	for _, record := range byID {
		if record == nil || (actor != nil && record.ID == actor.ID) || record.FactionID != factionID {
			continue
		}
		for _, alias := range unitDirectiveAliases(*record) {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if alias == "" {
				continue
			}
			if index := strings.Index(lowerClause[offset:], alias); index >= 0 {
				absolute := offset + index
				if absolute < next {
					next = absolute
				}
			}
		}
	}
	return next
}

func splitDirectiveClauses(text string) []string {
	fields := strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '\n', '\r', '。', '；', ';', '！', '!', '？', '?':
			return true
		default:
			return false
		}
	})
	clauses := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field != "" {
			clauses = append(clauses, field)
		}
	}
	return clauses
}

func directiveClauseMentionsFactionUnit(clause string, byID map[string]*unit.Record, factionID string) bool {
	for _, record := range byID {
		if record == nil || record.FactionID != factionID {
			continue
		}
		if directiveClauseMentionsUnit(clause, record) {
			return true
		}
	}
	return false
}

func directiveClauseMentionsUnit(clause string, record *unit.Record) bool {
	if record == nil {
		return false
	}
	lowerClause := strings.ToLower(strings.TrimSpace(clause))
	if lowerClause == "" {
		return false
	}
	for _, alias := range unitDirectiveAliases(*record) {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias != "" && strings.Contains(lowerClause, alias) {
			return true
		}
	}
	return false
}

func unitDirectiveAliases(record unit.Record) []string {
	return []string{
		record.ID,
		record.DisplayName(),
		record.Identity.Name,
		record.Identity.Nickname,
	}
}

func recommendedDecisionCandidate(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	remainingAP int,
	candidates []decisionCandidate,
) (decisionCandidate, bool) {
	if len(candidates) == 0 {
		return decisionCandidate{}, false
	}
	for _, candidate := range candidates {
		if candidate.Action == DecisionActionEat {
			return candidate, true
		}
	}
	if actor != nil && actor.Status.Hunger < 30 && !hasBackpackItem(*actor, "ration") {
		if candidate, ok := firstFoodSeekingCandidate(candidates); ok {
			return candidate, true
		}
	}
	directive := strings.ToLower(actorDirectiveForRecommendation(state, byID, actor))
	equipmentDirective := directivePrefersEquipmentWork(directive)
	if remainingAP < decisionActionCost(DecisionActionGather) &&
		(equipmentDirective || directivePrefersBuild(directive)) &&
		hasCurrentTileEconomyOpportunity(state, byID, actor) {
		if candidate, ok := firstCandidateByActionsBiased(actor, candidates, DecisionActionHold, DecisionActionDefend, DecisionActionObserve); ok {
			return candidate, true
		}
	}
	if containsAny(directive, "恋爱", "生小孩", "生孩子", "生育", "孩子", "家庭", "亲密") {
		if candidate, ok := firstCandidateByActionsBiased(actor, candidates, DecisionActionFamily, DecisionActionRomance, DecisionActionDialogue, DecisionActionSay); ok {
			return candidate, true
		}
	}
	if containsAny(directive, "交易", "贸易", "调拨", "赠送", "出售") {
		if candidate, ok := firstCandidateByActionsBiased(actor, candidates, DecisionActionTrade, DecisionActionSay, DecisionActionDialogue); ok {
			return candidate, true
		}
		if candidate, ok := bestMoveTowardNearestTarget(byID, actor, targetIDs, candidates); ok {
			return candidate, true
		}
	}
	if equipmentDirective {
		if candidate, ok := firstCandidateByActionsBiased(actor, candidates, DecisionActionUpgrade, DecisionActionForge, DecisionActionEquip, DecisionActionBuild, DecisionActionGather); ok {
			return candidate, true
		}
		if candidate, ok := bestMoveTowardProductionGoal(state, actor, candidates); ok {
			return candidate, true
		}
	}
	if directivePrefersBuild(directive) {
		if candidate, ok := firstCandidateByActionsBiased(actor, candidates, DecisionActionBuild, DecisionActionGather); ok {
			return candidate, true
		}
		if candidate, ok := bestMoveTowardProductionGoal(state, actor, candidates); ok {
			return candidate, true
		}
	}
	if containsAny(directive, "进攻", "攻击", "压上", "击溃", "追击", "集火") {
		if candidate, ok := bestCombatCandidate(byID, actor, targetIDs, candidates); ok {
			return candidate, true
		}
		if candidate, ok := bestMoveTowardNearestTarget(byID, actor, targetIDs, candidates); ok {
			return candidate, true
		}
	}
	if nearestThreatDistance(state, byID, actor) <= 1 {
		if candidate, ok := bestCombatCandidate(byID, actor, targetIDs, candidates); ok {
			return candidate, true
		}
		if candidate, ok := firstCandidateByActions(candidates, DecisionActionDefend); ok {
			return candidate, true
		}
	}
	if nearestThreatDistance(state, byID, actor) >= 8 {
		if candidate, ok := firstCandidateByActionsBiased(actor, candidates, DecisionActionBuild, DecisionActionGather); ok {
			return candidate, true
		}
	}
	if candidate, ok := bestMoveTowardNearestTarget(byID, actor, targetIDs, candidates); ok {
		return candidate, true
	}
	return firstCandidateByActionsBiased(actor, candidates, DecisionActionObserve, DecisionActionDefend, DecisionActionHold)
}

func firstCandidateByActions(candidates []decisionCandidate, actions ...DecisionAction) (decisionCandidate, bool) {
	for _, action := range actions {
		for _, candidate := range candidates {
			if candidate.Action == action {
				return candidate, true
			}
		}
	}
	return decisionCandidate{}, false
}

// firstCandidateByActionsBiased 是 firstCandidateByActions 的野心偏置版（③ 野心进在线规则 fallback）：
// 在「一组同优先级可选动作」里，flag 关（默认）时与 firstCandidateByActions 逐位一致（取 actions 列表里第一个有
// 匹配候选的动作的首个候选）；flag QUNXIANG_AMBITION_SCORING 开时，先取每个候选动作的首个匹配候选，再用
// PickAmbitionBiasedCandidate 在它们之间按野心引力择优——野心契合的动作（如复仇心重→conquer 攻伐）weight 更高者优先，
// 严格平手保留 actions 列表原优先序（稳定 tie-break，确定性）。actor 为 nil 退化为 firstCandidateByActions（失败安全）。
func firstCandidateByActionsBiased(actor *unit.Record, candidates []decisionCandidate, actions ...DecisionAction) (decisionCandidate, bool) {
	if actor == nil || !ambitionScoringEnabled() {
		return firstCandidateByActions(candidates, actions...)
	}
	// 按 actions 优先序收集每个动作的首个匹配候选（保留优先序，作为 PickAmbitionBiasedCandidate 的稳定 tie-break 基底）。
	pooled := make([]decisionCandidate, 0, len(actions))
	for _, action := range actions {
		for _, candidate := range candidates {
			if candidate.Action == action {
				pooled = append(pooled, candidate)
				break
			}
		}
	}
	return PickAmbitionBiasedCandidate(actor, pooled)
}

func bestCombatCandidate(
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (decisionCandidate, bool) {
	target := nearestBattleReady(targetIDs, byID, actor)
	for _, action := range []DecisionAction{DecisionActionAttack, DecisionActionHeavyAttack, DecisionActionCharge} {
		for _, candidate := range candidates {
			if candidate.Action != action {
				continue
			}
			if target == nil || candidate.TargetUnitID == target.ID {
				return candidate, true
			}
		}
	}
	return decisionCandidate{}, false
}

func firstFoodSeekingCandidate(candidates []decisionCandidate) (decisionCandidate, bool) {
	for _, action := range []DecisionAction{DecisionActionGather, DecisionActionTrade, DecisionActionDialogue, DecisionActionSay, DecisionActionMove} {
		for _, candidate := range candidates {
			if candidate.Action != action {
				continue
			}
			if action == DecisionActionGather && !foodSeekingActivity(candidate.Activity) {
				continue
			}
			return candidate, true
		}
	}
	return decisionCandidate{}, false
}

func foodSeekingActivity(activity ProductionActivity) bool {
	switch activity {
	case ProductionActivityForage, ProductionActivityHunt, ProductionActivityFish:
		return true
	default:
		return false
	}
}

func directivePrefersEquipmentWork(text string) bool {
	return containsAny(
		strings.ToLower(strings.TrimSpace(text)),
		"升级装备",
		"强化装备",
		"强化武器",
		"升级武器",
		"锻造",
		"强化",
		"升级",
		"铁匠铺",
		"整备装备",
		"装备",
		"武器",
		"护甲",
		"forge",
		"upgrade",
		"equip",
	)
}

func hasCurrentTileEconomyOpportunity(state State, byID map[string]*unit.Record, actor *unit.Record) bool {
	if actor == nil {
		return false
	}
	for _, candidate := range buildEconomyCandidates(state, byID, actor) {
		if candidate.Action == DecisionActionGather || candidate.Action == DecisionActionBuild {
			return true
		}
	}
	return false
}

func bestMoveTowardProductionGoal(
	state State,
	actor *unit.Record,
	candidates []decisionCandidate,
) (decisionCandidate, bool) {
	if actor == nil {
		return decisionCandidate{}, false
	}
	var chosen decisionCandidate
	bestScore := -1 << 30
	found := false
	for _, candidate := range candidates {
		if candidate.Action != DecisionActionMove {
			continue
		}
		terrain := terrainAt(state.Map, world.Coord{Q: candidate.TargetQ, R: candidate.TargetR})
		score := productionGoalTerrainScore(terrain)
		if score <= 0 {
			continue
		}
		if !found || score > bestScore {
			chosen = candidate
			bestScore = score
			found = true
		}
	}
	return chosen, found
}

func productionGoalTerrainScore(terrain world.TerrainID) int {
	score := 0
	if terrainSupportsActivity(terrain, ProductionActivityMine) {
		score += 40
	}
	if terrainSupportsActivity(terrain, ProductionActivityForage) {
		score += 30
	}
	if terrainSupportsStructure(terrain, StructureTypeForge) {
		score += 20
	}
	if terrainSupportsActivity(terrain, ProductionActivityHunt) {
		score += 8
	}
	return score
}

func bestMoveTowardNearestTarget(
	byID map[string]*unit.Record,
	actor *unit.Record,
	targetIDs []string,
	candidates []decisionCandidate,
) (decisionCandidate, bool) {
	target := nearestBattleReady(targetIDs, byID, actor)
	var chosen decisionCandidate
	bestDistance := 1 << 30
	found := false
	for _, candidate := range candidates {
		if candidate.Action != DecisionActionMove {
			continue
		}
		distance := 0
		if target != nil {
			distance = unit.HexDistance(candidate.TargetQ, candidate.TargetR, target.Status.PositionQ, target.Status.PositionR)
		}
		if !found || distance < bestDistance || (distance == bestDistance && (candidate.TargetQ < chosen.TargetQ || (candidate.TargetQ == chosen.TargetQ && candidate.TargetR < chosen.TargetR))) {
			chosen = candidate
			bestDistance = distance
			found = true
		}
	}
	return chosen, found
}

func formatRecommendedCandidate(candidate decisionCandidate, byID map[string]*unit.Record) string {
	params := recommendedCandidateParams(candidate)
	text := fmt.Sprintf("action=%s", candidate.Action)
	if params != "" {
		text += "，" + params
	}
	if summary := strings.TrimSpace(decisionCandidatePromptSummary(candidate, byID)); summary != "" {
		text += "；理由=" + summary
	}
	return text
}

func formatRecommendedJSONFields(candidate decisionCandidate) string {
	parts := []string{fmt.Sprintf(`"action":"%s"`, candidate.Action)}
	appendString := func(name string, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, fmt.Sprintf(`"%s":"%s"`, name, value))
		}
	}
	appendInt := func(name string, value int) {
		if value > 0 {
			parts = append(parts, fmt.Sprintf(`"%s":%d`, name, value))
		}
	}
	switch candidate.Action {
	case DecisionActionMove:
		parts = append(parts, fmt.Sprintf(`"target_q":%d`, candidate.TargetQ), fmt.Sprintf(`"target_r":%d`, candidate.TargetR))
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
		appendString("target_unit_id", candidate.TargetUnitID)
		appendString("structure_id", candidate.StructureID)
	case DecisionActionSkill:
		appendString("skill_id", candidate.SkillID)
		appendString("target_unit_id", candidate.TargetUnitID)
	case DecisionActionGather:
		appendString("activity", string(candidate.Activity))
	case DecisionActionBuild:
		appendString("structure_type", string(candidate.StructureType))
	case DecisionActionTrade:
		appendString("trade_kind", string(candidate.TradeKind))
		appendString("target_unit_id", candidate.TargetUnitID)
		appendString("item_id", candidate.ItemID)
		appendInt("gold_amount", candidate.GoldAmount)
		appendInt("price", candidate.Price)
	case DecisionActionSay, DecisionActionDialogue, DecisionActionAssist, DecisionActionRomance, DecisionActionFamily:
		appendString("target_unit_id", candidate.TargetUnitID)
	case DecisionActionForge, DecisionActionUpgrade, DecisionActionEquip, DecisionActionEat:
		appendString("item_id", candidate.ItemID)
	case DecisionActionPickup:
		appendString("ground_loot_id", candidate.GroundLootID)
	case DecisionActionDemolish:
		appendString("structure_id", candidate.StructureID)
	}
	return "{" + strings.Join(parts, ",") + `,"next_action":"自行填写","speak":"自行填写","memory":"自行填写","reasoning":"自行填写"}`
}

func recommendedCandidateParams(candidate decisionCandidate) string {
	parts := make([]string, 0, 5)
	appendString := func(name string, value string) {
		value = strings.TrimSpace(value)
		if value != "" {
			parts = append(parts, fmt.Sprintf("%s=%s", name, value))
		}
	}
	appendInt := func(name string, value int) {
		if value > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", name, value))
		}
	}
	switch candidate.Action {
	case DecisionActionMove:
		parts = append(parts, fmt.Sprintf("target_q=%d", candidate.TargetQ), fmt.Sprintf("target_r=%d", candidate.TargetR))
	case DecisionActionAttack, DecisionActionCharge, DecisionActionHeavyAttack:
		appendString("target_unit_id", candidate.TargetUnitID)
		appendString("structure_id", candidate.StructureID)
	case DecisionActionSkill:
		appendString("skill_id", candidate.SkillID)
		appendString("target_unit_id", candidate.TargetUnitID)
	case DecisionActionGather:
		appendString("activity", string(candidate.Activity))
	case DecisionActionBuild:
		appendString("structure_type", string(candidate.StructureType))
	case DecisionActionTrade:
		appendString("trade_kind", string(candidate.TradeKind))
		appendString("target_unit_id", candidate.TargetUnitID)
		appendString("item_id", candidate.ItemID)
		appendInt("gold_amount", candidate.GoldAmount)
		appendInt("price", candidate.Price)
	case DecisionActionUpgrade, DecisionActionEquip, DecisionActionEat:
		appendString("item_id", candidate.ItemID)
	case DecisionActionPickup:
		appendString("ground_loot_id", candidate.GroundLootID)
	case DecisionActionDemolish:
		appendString("structure_id", candidate.StructureID)
	}
	return strings.Join(parts, "，")
}

func summarizeVisibleTerrain(state State, byID map[string]*unit.Record, actor *unit.Record, limit int) string {
	if actor == nil || limit <= 0 {
		return "无"
	}
	vision := actor.Stats.Derived.Vision
	if vision <= 0 {
		vision = 5
	}
	coords, err := world.ComputeVisibleTiles(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}, vision)
	if err != nil || len(coords) == 0 {
		return "无"
	}
	sort.Slice(coords, func(i, j int) bool {
		di := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, coords[i].Q, coords[i].R)
		dj := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, coords[j].Q, coords[j].R)
		if di != dj {
			return di < dj
		}
		if coords[i].Q != coords[j].Q {
			return coords[i].Q < coords[j].Q
		}
		return coords[i].R < coords[j].R
	})
	lines := make([]string, 0, limit)
	for _, coord := range coords {
		if len(lines) >= limit {
			break
		}
		terrainName := terrainDisplayName(terrainAt(state.Map, coord))
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, coord.Q, coord.R)
		structure := summarizeStructureAt(state.Structures, coord)
		occupants := summarizeVisibleOccupantsAt(byID, coord)
		lines = append(lines, fmt.Sprintf("- %d,%d 距%d：%s；设施=%s；单位=%s", coord.Q, coord.R, distance, terrainName, structure, occupants))
	}
	return strings.Join(lines, "\n")
}

func summarizeVisibleOccupantsAt(byID map[string]*unit.Record, coord world.Coord) string {
	if len(byID) == 0 {
		return "无"
	}
	parts := make([]string, 0, 2)
	for _, record := range byID {
		if record == nil || !isBattleReady(*record) {
			continue
		}
		if record.Status.PositionQ == coord.Q && record.Status.PositionR == coord.R {
			parts = append(parts, fmt.Sprintf("%s[%s]", record.DisplayName(), record.FactionID))
		}
	}
	if len(parts) == 0 {
		return "无"
	}
	sort.Strings(parts)
	return strings.Join(parts, "/")
}

// formatMoveOptions 格式化可移动坐标集合及到达后可执行事项为提示词文本。
func formatMoveOptions(state State, actor *unit.Record, options []world.Coord) string {
	if len(options) == 0 {
		return "无相邻可走格，若不想进攻就选择 hold。"
	}

	lines := make([]string, 0, len(options))
	for _, option := range options {
		lines = append(lines, fmt.Sprintf("- target_q=%d target_r=%d（坐标 %d,%d）：%s", option.Q, option.R, option.Q, option.R, tileOpportunitySummary(state, actor, option)))
	}
	return strings.Join(lines, "\n")
}

// formatDialogueOptions 格式化本回合可交谈目标；交谈动作只允许视野内且未被外交策略禁止的非自己单位。
func formatDialogueOptions(state State, byID map[string]*unit.Record, actor *unit.Record) string {
	targets := visibleCommunicationTargets(state, byID, actor)
	lines := make([]string, 0, len(targets))
	for _, target := range targets {
		if dialogueBlockedByPolicy(state, actor, target) {
			continue
		}
		distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR)
		lines = append(lines, fmt.Sprintf(
			"- 名称=%s[%s] 坐标=%d,%d 距离=%d（视野内）",
			target.DisplayName(),
			target.FactionID,
			target.Status.PositionQ,
			target.Status.PositionR,
			distance,
		))
	}
	if len(lines) == 0 {
		return "无（交谈只能选择视野内的非自己单位；被外交策略禁止的跨阵营接触不会进入候选）"
	}
	return strings.Join(lines, "\n")
}

// --- 门控意外（GateSurprise 硬前因门控，设计宪法 §5.3）---
//
// 把 engine/decision.GateSurprise 接入决策链：突然恋爱/卖传家宝/叛变这类高代价转折，
// 即使 LLM 的归因看似合理、也必须有数据里真实存在的专属前因（关系积累/重压/忠诚崩坏）才允许落地。
// 与归因强制同档：仅 enforcementActive 时才真正阻断；影子模式下只记遥测、不改变行为。普通动作零影响。

// 进程级门控遥测（跨所有会话累计；与归因遥测一样进程级，不随每请求新建的 Service 重置）。
var (
	surpriseGateTotal   atomic.Int64 // 进入门控判定的戏剧性动作总次数
	surpriseGateBlocked atomic.Int64 // 其中被判定为「不放行」（reject/freeze）的次数
)

// SurpriseGateStats 返回进程级累计：进入门控的戏剧性动作总数与被拦截数。
func SurpriseGateStats() (total int64, blocked int64) {
	return surpriseGateTotal.Load(), surpriseGateBlocked.Load()
}

// 进程级一致性收紧旋钮遥测（§8）：total=收紧态下命中惊喜上限（surprise_level ≥cap）的动作总数，
// deferred=其中在 enforcementActive 下真被回退安全决策的数。影子模式下 total 计、deferred 不计。
var (
	surpriseCapTotal    atomic.Int64
	surpriseCapDeferred atomic.Int64
)

// SurpriseCapStats 返回进程级累计：收紧态下命中惊喜上限的动作总数与真被回退（deferred）数（/healthz 暴露，与门控遥测并列）。
func SurpriseCapStats() (total int64, deferred int64) {
	return surpriseCapTotal.Load(), surpriseCapDeferred.Load()
}

// 内容安全拦截后的安全占位文案（AI 输出侧）。
const (
	unsafeDialoguePlaceholder = "（她欲言又止，没有说出口。）"
	unsafeSpeakPlaceholder    = "（这句话被安全策略隐去了。）"
)

// redactCompletionResultOutput 返回 result 的脱敏副本：把 Output 重写为安全占位 JSON、清空 Debug.RawOutput，
// 使内容安全拦截后原始违规文本不再经 LLMInteraction 的 ParsedOutput/RawOutput 落库或广播外泄（评审 load-bearing 修复）。
func redactCompletionResultOutput(result ai.CompletionResult, placeholder string) ai.CompletionResult {
	redacted := result
	encoded, err := json.Marshal(map[string]string{"redacted": placeholder})
	if err != nil {
		encoded = []byte(`{"redacted":""}`)
	}
	redacted.Output = encoded
	redacted.Debug.RawOutput = ""
	return redacted
}

// 叛变意图的中文关键词（无原生 defect 动作，靠声明的行动意图启发式识别）。只取**明确自指叛变**的短语：
// 「投敌/倒戈/叛逃/投靠敌/归顺敌/改投敌/背叛阵营/出卖阵营」——刻意剔除易在防御/讨论语境出现的裸词
// （「叛变」常见于「防止叛变/担心叛变」、「改投」可指「改投他处」、「卖阵营」歧义），避免误伤正常动作。
var defectIntentKeywords = []string{"投敌", "倒戈", "叛逃", "投靠敌", "归顺敌", "改投敌", "背叛阵营", "出卖阵营"}

// gatedActionForChoice 判断一个 LLM choice 是否属于需要专属前因的门控动作，并映射到 decision.GatedAction。
// 仅识别能稳妥判定的三类：romance（原生恋爱动作）、sell_pinned（变卖/赠出传家宝级物品）、defect（叛变意图）。
// actor 用于 sell_pinned 的第二条触发源：标的若是该角色实际持有、已升级为永久锚（IsLegacy&&SoulBound&&Pinned）的
// **目录内**装备（如 long_sword 刻成传家物），isPermanentAnchorItem 的目录启发式会漏判——故同时查持有实例的真标记，
// 落实「升级后即便 LLM 自治也卖不掉」（§5 闭环）。
func gatedActionForChoice(actor *unit.Record, choice unitDecisionChoicePayload) (decision.GatedAction, bool) {
	// 恋爱：原生 romance 动作即表白/确认伴侣（family 是已确认恋人后的共育，不在突然恋爱门控内）。
	if choice.Action == DecisionActionRomance {
		return decision.ActionRomance, true
	}
	// 卖/赠传家宝：trade 且为 sell/gift，且标的是「永久锚」级物品——目录启发式（父辈遗志/独有遗物）或该角色实际持有的升级实例。
	if choice.Action == DecisionActionTrade &&
		(choice.TradeKind == TradeActionKindSell || choice.TradeKind == TradeActionKindGift) &&
		(isPermanentAnchorItem(choice.ItemID, choice.ItemName) || actorHoldsPermanentAnchor(actor, choice.ItemID)) {
		return decision.ActionSellPinned, true
	}
	// 叛变：无原生动作，靠**声明的行动意图**识别——**只扫 next_action**（该单位下一步要做什么），
	// 不扫 reasoning/speak/memory（那些是策略推理/表忠/对话/记忆，含「防止叛变」「我绝不倒戈」「玩家说防投敌」
	// 等会误判正常动作的语境）。配合上面收紧的关键词，把「文本提到叛变就静默替换成 Hold」的行为回归彻底堵死
	// （评审 load-bearing 误杀修复）。
	if containsAny(strings.ToLower(choice.NextAction), defectIntentKeywords...) {
		return decision.ActionDefect, true
	}
	return "", false
}

// permanentAnchorItemTags 是把物品标记为「永久锚」（绝不可随意变卖）的物品标签。
var permanentAnchorItemTags = []string{"heirloom", "pinned", "legacy", "relic", "soulbound", "bound", "unique", "anchor"}

// isPermanentAnchorItem 判断标的是否为父辈遗志/独有遗物级的永久锚物品。
// 命中条件：物品定义带永久锚标签；或物品 ID 不在静态目录里（说明是叙事生成的独有遗物，而非可买卖的量产货）。
// 这是保守启发式：宁可把疑似传家宝挡在自治之外（上交玩家/剔除），也不让 LLM 擅自变卖。
func isPermanentAnchorItem(itemID, itemName string) bool {
	id := strings.ToLower(strings.TrimSpace(itemID))
	name := strings.ToLower(strings.TrimSpace(itemName))
	if id == "" && name == "" {
		return false
	}
	if definition, ok := item.Lookup(itemID); ok {
		for _, tag := range definition.Tags {
			if containsAny(strings.ToLower(tag), permanentAnchorItemTags...) {
				return true
			}
		}
		// 目录内的量产物品（可买卖）不视为永久锚。
		return false
	}
	// 目录外的具名物品视为叙事独有遗物（没有市场价、不可量产）。
	return id != "" || name != ""
}

// evaluateSurpriseGate 为一个门控动作构造 decision.GateInput 并跑 GateSurprise。
// 关系/物品/忠诚等前因从单位快照与实时关系存储读取（DB 不可用时退化为空关系，仍能判定恋爱/忠诚类）。
// surpriseGateAllowed 判定门控结果是否放行（包级 helper：generateUnitDecision 内 `decision` 被局部变量遮蔽，无法直接用 decision.GateAllow）。
func surpriseGateAllowed(gate decision.GateResult) bool {
	return gate.Decision == decision.GateAllow
}

func (service *Service) evaluateSurpriseGate(ctx context.Context, actor *unit.Record, choice unitDecisionChoicePayload, gated decision.GatedAction) decision.GateResult {
	in := buildSurpriseGateInput(actor, choice, gated)
	if gated == decision.ActionRomance && service != nil && service.db != nil && actor != nil {
		// 恋爱需要对目标的真实关系积累：affection + 关系记忆条数。从实时关系存储补齐。
		if targetID := strings.TrimSpace(choice.TargetUnitID); targetID != "" {
			for _, row := range service.loadTopOutgoingRelations(ctx, actor.ID, attributionRelationLimit) {
				if row.TargetUnitID != targetID {
					continue
				}
				in.TargetAffection = row.Affection / relationAxisScale
				// 关系四轴任一显著即视为「有过实质来往」，累计一条关系记忆/一个互动窗口。
				if absRel(row.Trust) >= 1 || absRel(row.Affection) >= 1 || absRel(row.Fear) >= 1 || absRel(row.Rivalry) >= 1 {
					in.RelationMemoryCount = 2
					in.AccumulatedWindows = 3
				}
				break
			}
		}
	}
	return decision.GateSurprise(gated, in)
}

// buildSurpriseGateInput 从单位当前快照与 choice 廉价构造 GateInput（纯函数，无 DB，可测）。
// 关系类字段（恋爱）由调用方在有 DB 时补齐；忠诚/压力/物品类字段在此即可定。
func buildSurpriseGateInput(actor *unit.Record, choice unitDecisionChoicePayload, gated decision.GatedAction) decision.GateInput {
	in := decision.GateInput{}
	if actor != nil {
		s := actor.Status
		in.Loyalty = s.Loyalty
		in.HasDebtPressure = s.Wallet == 0
		in.HasThreatPressure = s.InCombat
		// 忠诚崩坏的佐证：负面记忆/关系恶化用决策归因里是否带 memory/relation 前因近似；
		// 势力衰退压力用低士气近似。这些是保守佐证，宁缺毋滥（无佐证则门控更严，符合红线）。
		in.HasFactionDeclinePressure = s.Morale <= 0.25
	}
	if gated == decision.ActionDefect {
		in.HasNegativeMemory = attributionHasCauseKind(choice.Attribution, string(decision.CauseMemory))
		in.HasRelationDecay = attributionHasCauseKind(choice.Attribution, string(decision.CauseRelation))
	}
	if gated == decision.ActionSellPinned {
		// 永久锚（父辈遗志类）绝不可卖：交给 GateSurprise 判 PINNED_PERMANENT。两条来源取或：
		//   ① 目录外具名独有遗物（isParentLegacyItem，旧口径，无市场价/不可量产）；
		//   ② 该角色实际持有的同 ID 装备已被刻成传家物（三标记 IsLegacy&&SoulBound&&Pinned 齐备，§5 升级闭环的落点）——
		//      经 item.IsPermanentAnchor 判定，落实「升级后即便 LLM 自治也卖不掉」。
		in.ItemIsPermanentAnchor = isParentLegacyItem(choice.ItemID, choice.ItemName) ||
			actorHoldsPermanentAnchor(actor, choice.ItemID)
	}
	return in
}

// actorHoldsPermanentAnchor 判断该角色当前是否持有 itemID 对应、且已构成「永久锚」（IsLegacy&&SoulBound&&Pinned）的装备/背包物。
// 任一同 ID 实例满足三标记即真（升级后的传家物 sell_pinned 硬门）。纯函数、无 DB、确定性；actor/itemID 空时安全 false。
func actorHoldsPermanentAnchor(actor *unit.Record, itemID string) bool {
	if actor == nil {
		return false
	}
	itemID = strings.ToLower(strings.TrimSpace(itemID))
	if itemID == "" {
		return false
	}
	anchored := func(s unit.ItemStack) bool {
		return strings.ToLower(strings.TrimSpace(s.ItemID)) == itemID &&
			item.IsPermanentAnchor(item.LegacyFlags{IsLegacy: s.IsLegacy, SoulBound: s.SoulBound, Pinned: s.Pinned})
	}
	for _, s := range actor.Inventory.Equipment {
		if anchored(s) {
			return true
		}
	}
	for _, s := range actor.Inventory.Backpack {
		if anchored(s) {
			return true
		}
	}
	return false
}

// isParentLegacyItem 判断是否为「父辈遗志」级、绝对不可变卖的永久锚（比一般传家宝更硬）。
// 以「目录外的具名独有遗物」为父辈遗志：它没有市场价、不可量产，卖出即永久失去角色羁绊。
func isParentLegacyItem(itemID, itemName string) bool {
	id := strings.ToLower(strings.TrimSpace(itemID))
	if _, ok := item.Lookup(itemID); ok {
		// 目录内可买卖物品即便带锚标签也只走「需玩家决策/重压自治」分支，不算绝对不可卖。
		return false
	}
	return id != "" || strings.TrimSpace(itemName) != ""
}

// attributionHasCauseKind 判断归因 primary/supporting 是否含某类前因（用于近似「有负面记忆/关系恶化」佐证）。
func attributionHasCauseKind(payload *attributionPayload, kind string) bool {
	if payload == nil {
		return false
	}
	if payload.Primary.Kind == kind {
		return true
	}
	for _, c := range payload.Supporting {
		if c.Kind == kind {
			return true
		}
	}
	return false
}

// absRel 取关系轴绝对值（关系存储为 [-10,10] 原始刻度）。
func absRel(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
