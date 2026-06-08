package session

// 文件说明：驱动回合开始随机事件流程，负责模板选取、分支裁决、效果应用与叙事写回。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const minRandomEventTemplateCount = 20

// randomEventDefinition 定义一类随机事件模板及其可选分支。
type randomEventDefinition struct {
	ID       string
	Theme    string
	Title    string
	Prompt   string
	Branches []randomEventBranch
}

// randomEventBranch 定义随机事件单个分支的效果与文本素材。
type randomEventBranch struct {
	ID             string
	Label          string
	Outcome        string
	Bubble         string
	Memory         string
	WalletDelta    int
	HungerDelta    int
	MoraleDelta    float64
	LoyaltyDelta   float64
	GainItemID     string
	GainItemQty    int
	ConsumeItemID  string
	ConsumeItemQty int
}

// randomEventBranchDecision 记录分支选择中的掷骰、偏置和得分。
type randomEventBranchDecision struct {
	Branch randomEventBranch
	Roll   float64
	Bias   float64
	Score  float64
}

// randomEventNarrationPayload 表示事件叙述输出的结构化载荷。
type randomEventNarrationPayload struct {
	Outcome   string `json:"outcome"`
	Bubble    string `json:"bubble"`
	Memory    string `json:"memory"`
	Reasoning string `json:"reasoning"`
}

var randomEventNarrationSchema = []byte(`{
  "type":"object",
  "properties":{
    "outcome":{"type":"string","minLength":1},
    "bubble":{"type":"string","minLength":1},
    "memory":{"type":"string","minLength":1},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["outcome","bubble","memory","reasoning"],
  "additionalProperties":false
}`)

// resolveTurnRandomEvent 在部署阶段触发并结算一次随机事件。
func (service *Service) resolveTurnRandomEvent(ctx context.Context, state *State, units []unit.Record) error {
	if service == nil || state == nil || state.Outcome != OutcomeOngoing {
		return nil
	}
	if state.RandomEventsDisabled {
		return nil
	}
	if state.TurnState.Phase != turns.PhaseDeployment {
		return nil
	}

	events := randomEventCatalog()
	if len(events) == 0 {
		return nil
	}

	if units == nil {
		loaded, err := service.units.ListBySession(ctx, state.ID)
		if err != nil {
			return err
		}
		units = loaded
	}
	byID := mapRecordsByID(units)
	actors := randomEventActors(*state, byID)
	if len(actors) == 0 {
		return nil
	}

	event := selectTurnRandomEvent(*state, events)
	actor := selectTurnRandomEventActor(*state, actors, event)
	if actor == nil {
		return nil
	}

	decision := chooseRandomEventBranch(*state, *actor, event)
	branch := decision.Branch
	if strings.TrimSpace(branch.ID) == "" {
		return nil
	}

	effectSummary, err := service.applyRandomEventBranch(ctx, state, actor, branch)
	if err != nil {
		return err
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}

	rulePrompt := buildRandomEventRulePrompt(*state, *actor, event, branch, decision)
	selectorResult := ai.CompletionResult{
		Provider:     "autonomy_rules",
		Model:        "random_event_selector",
		UsedFallback: true,
		Debug: ai.CompletionDebug{
			FallbackCause: fmt.Sprintf(
				"rule-selected branch=%s roll=%.3f bias=%.3f score=%.3f",
				branch.ID,
				decision.Roll,
				decision.Bias,
				decision.Score,
			),
		},
	}
	service.appendLLMInteractionWithSpend(
		ctx,
		state,
		buildLLMInteraction(
			*state,
			actor.ID,
			"random_event_selector",
			fmt.Sprintf("%s -> %s", event.Title, branch.Label),
			"规则自主事件引擎：玩家只给全局自然语言方针，具体事件分支与文本由单位自决。",
			rulePrompt,
			selectorResult,
			"",
		),
	)

	narration, narrationResult, narrationInteraction := service.generateRandomEventNarration(
		ctx,
		*state,
		byID,
		*actor,
		event,
		branch,
		effectSummary,
		decision,
	)
	service.appendLLMInteractionWithSpend(ctx, state, narrationInteraction)

	appendAIDialogue(state, *actor, narration.Bubble, narrationResult)
	service.rememberUnitBestEffort(ctx, actor, state.TurnState.Turn, narration.Memory)

	message := strings.TrimSpace(firstNonEmptyText(narration.Outcome, narration.Bubble, narration.Memory))
	if message == "" {
		message = "我先按局势把这件事处理了。"
	}
	appendLog(state, "random_event", message, actor.ID, "")

	appendRawEvent(state, rawEventSpec{
		source:      "event",
		kind:        "random_event_applied",
		summary:     message,
		actorUnitID: actor.ID,
		payload: map[string]any{
			"event_id":       event.ID,
			"event_title":    event.Title,
			"event_theme":    event.Theme,
			"branch_id":      branch.ID,
			"branch_label":   branch.Label,
			"effect_summary": effectSummary,
			"narration": map[string]any{
				"outcome": narration.Outcome,
				"bubble":  narration.Bubble,
				"memory":  narration.Memory,
			},
			"narration_provider": narrationResult.Provider,
			"narration_model":    narrationResult.Model,
			"terrain":            terrainDisplayName(terrainAt(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR})),
			"weather":            state.Weather.DisplayName,
			"actor_unit_id":      actor.ID,
			"actor_unit_name":    actor.DisplayName(),
		},
	})
	return nil
}

// randomEventActors 收集可参与随机事件的在场单位。
func randomEventActors(state State, byID map[string]*unit.Record) []*unit.Record {
	actors := make([]*unit.Record, 0, len(state.PlayerUnitIDs)+len(state.EnemyUnitIDs))
	seen := map[string]struct{}{}
	appendActor := func(unitID string) {
		record := byID[unitID]
		if record == nil || !isBattleReady(*record) {
			return
		}
		if _, exists := seen[record.ID]; exists {
			return
		}
		seen[record.ID] = struct{}{}
		actors = append(actors, record)
	}

	for _, unitID := range state.PlayerUnitIDs {
		appendActor(unitID)
	}
	for _, unitID := range state.EnemyUnitIDs {
		appendActor(unitID)
	}
	return actors
}

// selectTurnRandomEvent 按种子与回合稳定选取事件模板。
func selectTurnRandomEvent(state State, events []randomEventDefinition) randomEventDefinition {
	if len(events) == 0 {
		return randomEventDefinition{}
	}
	roll := deterministicRandomEventRoll(state.RandomSeed, state.TurnState.Turn, "event")
	index := int(roll * float64(len(events)))
	if index < 0 {
		index = 0
	}
	if index >= len(events) {
		index = len(events) - 1
	}
	return events[index]
}

// selectTurnRandomEventActor 按种子稳定选取本回合事件主角。
func selectTurnRandomEventActor(state State, actors []*unit.Record, event randomEventDefinition) *unit.Record {
	if len(actors) == 0 {
		return nil
	}
	roll := deterministicRandomEventRoll(state.RandomSeed, state.TurnState.Turn, "actor", event.ID)
	index := int(roll * float64(len(actors)))
	if index < 0 {
		index = 0
	}
	if index >= len(actors) {
		index = len(actors) - 1
	}
	return actors[index]
}

// chooseRandomEventBranch 根据单位风险倾向在分支中做规则选择。
func chooseRandomEventBranch(state State, actor unit.Record, event randomEventDefinition) randomEventBranchDecision {
	if len(event.Branches) == 0 {
		return randomEventBranchDecision{}
	}
	if len(event.Branches) == 1 {
		return randomEventBranchDecision{
			Branch: event.Branches[0],
			Roll:   deterministicRandomEventRoll(state.RandomSeed, state.TurnState.Turn, "branch", actor.ID, event.ID),
			Bias:   0,
			Score:  0,
		}
	}

	roll := deterministicRandomEventRoll(state.RandomSeed, state.TurnState.Turn, "branch", actor.ID, event.ID)
	bias := randomEventRiskBias(state, actor)
	score := clampFloat(roll+bias, 0, 0.999)
	index := int(score * float64(len(event.Branches)))
	if index < 0 {
		index = 0
	}
	if index >= len(event.Branches) {
		index = len(event.Branches) - 1
	}
	return randomEventBranchDecision{
		Branch: event.Branches[index],
		Roll:   roll,
		Bias:   bias,
		Score:  score,
	}
}

// randomEventRiskBias 计算单位在随机事件中的风险偏置值。
func randomEventRiskBias(state State, actor unit.Record) float64 {
	bias := 0.0
	bias += (actor.Personality.Aggression - 0.5) * 0.22
	bias += (actor.Personality.Ambition - 0.5) * 0.20
	bias += (actor.Personality.Courage - 0.5) * 0.18
	bias -= (actor.Personality.Prudence - 0.5) * 0.22
	bias -= (actor.Personality.Integrity - 0.5) * 0.14
	bias -= (actor.Personality.Stability - 0.5) * 0.10

	if actor.Status.Hunger <= 35 {
		bias += 0.08
	}
	if actor.Status.Wallet < 40 {
		bias += 0.06
	}
	if actor.Status.Morale < 0.35 {
		bias -= 0.07
	}

	bias += memoryRiskTone(actor.Memory.Highlights)
	switch state.Weather.Type {
	case WeatherRainy, WeatherFoggy:
		bias -= 0.05
	case WeatherClear:
		bias += 0.02
	}
	return clampFloat(bias, -0.35, 0.35)
}

// memoryRiskTone 从近期记忆提取“保守/激进”倾向信号。
func memoryRiskTone(highlights []string) float64 {
	if len(highlights) == 0 {
		return 0
	}
	start := 0
	if len(highlights) > 4 {
		start = len(highlights) - 4
	}

	positive := []string{"守住", "成功", "救下", "奖励", "获胜", "丰收", "结盟"}
	negative := []string{"受伤", "倒下", "背叛", "饥饿", "创伤", "拦截", "失败"}
	score := 0.0
	for _, highlight := range highlights[start:] {
		text := strings.TrimSpace(highlight)
		if text == "" {
			continue
		}
		for _, token := range positive {
			if strings.Contains(text, token) {
				score += 0.03
			}
		}
		for _, token := range negative {
			if strings.Contains(text, token) {
				score -= 0.04
			}
		}
	}
	return clampFloat(score, -0.12, 0.12)
}

// applyRandomEventBranch 应用分支效果并返回可读效果摘要。
func (service *Service) applyRandomEventBranch(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	branch randomEventBranch,
) (string, error) {
	if actor == nil {
		return "", nil
	}
	before := actor.Status

	// 随机事件分支最多对同一单位的 4 个独立字段（金币/饥饿/士气/忠诚）各变更一次。
	// 各字段目标值都基于原始 actor.Status 计算、彼此无数据依赖（一次变更只触及自身字段），
	// 故四档目标全部先行算出，再聚成一次 ApplyBatch 收敛 DB 往返；入参顺序与逐次路径一致
	// （金币→饥饿→士气→忠诚），事件/日志交错顺序不变。
	var branchMutations []pendingStatusMutation

	walletTarget := actor.Status.Wallet + branch.WalletDelta
	if walletTarget < 0 {
		walletTarget = 0
	}
	if walletTarget != actor.Status.Wallet {
		reasonCode := events.ReasonEconomyReward
		if walletTarget < actor.Status.Wallet {
			reasonCode = events.ReasonEconomyPurchase
		}
		branchMutations = append(branchMutations, pendingStatusMutation{
			record:     actor,
			field:      status.FieldWallet,
			delta:      float64(walletTarget - actor.Status.Wallet),
			reasonCode: reasonCode,
			reasonText: fmt.Sprintf("随机事件[%s]导致金币变化", branch.Label),
		})
	}
	hungerTarget := clampInt(actor.Status.Hunger+branch.HungerDelta, 0, 100)
	if hungerTarget != actor.Status.Hunger {
		branchMutations = append(branchMutations, pendingStatusMutation{
			record:     actor,
			field:      status.FieldHunger,
			delta:      float64(hungerTarget - actor.Status.Hunger),
			reasonCode: events.ReasonSurvivalHunger,
			reasonText: fmt.Sprintf("随机事件[%s]导致饥饿变化", branch.Label),
		})
	}
	moraleTarget := clampFloat(actor.Status.Morale+branch.MoraleDelta, 0.05, 1)
	if moraleTarget != actor.Status.Morale {
		reasonCode := events.ReasonEmotionReward
		if moraleTarget < actor.Status.Morale {
			reasonCode = events.ReasonEmotionTrauma
		}
		branchMutations = append(branchMutations, pendingStatusMutation{
			record:     actor,
			field:      status.FieldMorale,
			delta:      moraleTarget - actor.Status.Morale,
			reasonCode: reasonCode,
			reasonText: fmt.Sprintf("随机事件[%s]导致士气变化", branch.Label),
		})
	}
	loyaltyTarget := clampFloat(actor.Status.Loyalty+branch.LoyaltyDelta, 0.05, 1)
	if loyaltyTarget != actor.Status.Loyalty {
		reasonCode := events.ReasonRelationRescue
		if loyaltyTarget < actor.Status.Loyalty {
			reasonCode = events.ReasonRelationBetray
		}
		branchMutations = append(branchMutations, pendingStatusMutation{
			record:     actor,
			field:      status.FieldLoyalty,
			delta:      loyaltyTarget - actor.Status.Loyalty,
			reasonCode: reasonCode,
			reasonText: fmt.Sprintf("随机事件[%s]导致忠诚变化", branch.Label),
		})
	}
	if err := service.applyStatusMutationsBatch(ctx, state, branchMutations); err != nil {
		return "", err
	}

	consumeApplied := false
	if strings.TrimSpace(branch.ConsumeItemID) != "" {
		quantity := branch.ConsumeItemQty
		if quantity <= 0 {
			quantity = 1
		}
		if hasBackpackQuantity(*actor, branch.ConsumeItemID, quantity) {
			if err := unit.ConsumeBackpackItem(actor, branch.ConsumeItemID, quantity); err == nil {
				consumeApplied = true
			}
		}
	}

	gainApplied := false
	if strings.TrimSpace(branch.GainItemID) != "" {
		quantity := branch.GainItemQty
		if quantity <= 0 {
			quantity = 1
		}
		if err := unit.AddBackpackItem(actor, branch.GainItemID, quantity); err == nil {
			gainApplied = true
		}
	}

	details := make([]string, 0, 6)
	if before.Wallet != actor.Status.Wallet {
		details = append(details, fmt.Sprintf("金币%s", formatSignedInt(actor.Status.Wallet-before.Wallet)))
	}
	if before.Hunger != actor.Status.Hunger {
		details = append(details, fmt.Sprintf("饥饿%s", formatSignedInt(actor.Status.Hunger-before.Hunger)))
	}
	if before.Morale != actor.Status.Morale {
		details = append(details, fmt.Sprintf("士气%s", formatSignedFloat(actor.Status.Morale-before.Morale)))
	}
	if before.Loyalty != actor.Status.Loyalty {
		details = append(details, fmt.Sprintf("忠诚%s", formatSignedFloat(actor.Status.Loyalty-before.Loyalty)))
	}
	if consumeApplied {
		details = append(details, fmt.Sprintf("消耗%s", displayItemName(branch.ConsumeItemID)))
	}
	if gainApplied {
		details = append(details, fmt.Sprintf("获得%s", displayItemName(branch.GainItemID)))
	}
	return strings.Join(details, "，"), nil
}

// generateRandomEventNarration 生成事件叙述文本（优先 LLM，失败回落模板）。
func (service *Service) generateRandomEventNarration(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	actor unit.Record,
	event randomEventDefinition,
	branch randomEventBranch,
	effectSummary string,
	decision randomEventBranchDecision,
) (randomEventNarrationPayload, ai.CompletionResult, LLMInteraction) {
	systemPrompt := fmt.Sprintf(
		"你是《群像》的单位内心叙事器。玩家只会给全局自然语言方针；当前随机事件的分支已经由单位自主选择。请生成该单位这一瞬间的 outcome/bubble/memory/reasoning，全部使用中文。",
	)
	memorySummary := service.memorySummaryForPrompt(ctx, state, byID, actor, 8)
	relationSummary := service.relationSummaryForPrompt(ctx, byID, actor, 4)
	userPrompt := buildRandomEventNarrationPrompt(
		state,
		actor,
		event,
		branch,
		effectSummary,
		decision,
		memorySummary,
		relationSummary,
	)
	fallback := fallbackRandomEventNarrationPayload(event, branch, effectSummary)

	if llmBudgetGuardrailActive(state) {
		result := budgetGuardrailResult(state)
		return fallback, result, buildLLMInteraction(state, actor.ID, "random_event", fallback.Bubble, systemPrompt, userPrompt, result, "")
	}
	if service.llm == nil {
		result := ai.CompletionResult{
			Provider:     "autonomy_rules",
			Model:        "random_event_narration_rule",
			UsedFallback: true,
			Debug: ai.CompletionDebug{
				FallbackCause: "llm client is disabled",
			},
		}
		return fallback, result, buildLLMInteraction(
			state,
			actor.ID,
			"random_event",
			fallback.Bubble,
			systemPrompt,
			userPrompt,
			result,
			"",
		)
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskReflection,
		SchemaName:     "session_random_event_narration",
		ResponseSchema: randomEventNarrationSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.65,
		MaxTokens:      220,
	})
	if err != nil {
		fallbackResult := ai.CompletionResult{
			Provider:     "autonomy_rules",
			Model:        "random_event_narration_rule",
			UsedFallback: true,
			Debug: ai.CompletionDebug{
				FallbackCause: err.Error(),
				Attempts:      append([]ai.CompletionAttempt{}, result.Debug.Attempts...),
			},
		}
		return fallback, fallbackResult, buildLLMInteraction(
			state,
			actor.ID,
			"random_event",
			fallback.Bubble,
			systemPrompt,
			userPrompt,
			fallbackResult,
			"",
		)
	}

	var payload randomEventNarrationPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode random event narration payload: %v", err)
		fallbackResult := ai.CompletionResult{
			Provider:     "autonomy_rules",
			Model:        "random_event_narration_rule",
			UsedFallback: true,
			Debug: ai.CompletionDebug{
				FallbackCause: cause,
				Attempts:      append([]ai.CompletionAttempt{}, result.Debug.Attempts...),
			},
		}
		return fallback, fallbackResult, buildLLMInteraction(
			state,
			actor.ID,
			"random_event",
			fallback.Bubble,
			systemPrompt,
			userPrompt,
			fallbackResult,
			"",
		)
	}

	payload = normalizeRandomEventNarrationPayload(payload)
	if payload.Outcome == "" || payload.Bubble == "" || payload.Memory == "" || payload.Reasoning == "" {
		cause := "random event narration payload is incomplete"
		fallbackResult := ai.CompletionResult{
			Provider:     "autonomy_rules",
			Model:        "random_event_narration_rule",
			UsedFallback: true,
			Debug: ai.CompletionDebug{
				FallbackCause: cause,
				Attempts:      append([]ai.CompletionAttempt{}, result.Debug.Attempts...),
			},
		}
		return fallback, fallbackResult, buildLLMInteraction(
			state,
			actor.ID,
			"random_event",
			fallback.Bubble,
			systemPrompt,
			userPrompt,
			fallbackResult,
			"",
		)
	}

	return payload, result, buildLLMInteraction(
		state,
		actor.ID,
		"random_event",
		payload.Bubble,
		systemPrompt,
		userPrompt,
		result,
		"",
	)
}

// buildRandomEventNarrationPrompt 构建随机事件叙述任务提示词。
func buildRandomEventNarrationPrompt(
	state State,
	actor unit.Record,
	event randomEventDefinition,
	branch randomEventBranch,
	effectSummary string,
	decision randomEventBranchDecision,
	memorySummary string,
	relationSummary string,
) string {
	terrain := terrainDisplayName(terrainAt(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}))
	var builder strings.Builder
	fmt.Fprintf(&builder, "回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "事件: %s (%s)\n", event.Title, event.ID)
	fmt.Fprintf(&builder, "分支: %s (%s)\n", branch.Label, branch.ID)
	fmt.Fprintf(&builder, "事件原始描述: %s\n", strings.TrimSpace(branch.Outcome))
	fmt.Fprintf(&builder, "状态变化: %s\n", strings.TrimSpace(effectSummary))
	fmt.Fprintf(&builder, "环境: 天气=%s 地形=%s\n", state.Weather.DisplayName, terrain)
	fmt.Fprintf(&builder, "单位: %s\n", describeUnit(actor, nil))
	fmt.Fprintf(&builder, "人格: %s\n", summarizeActorPersonality(actor))
	if memorySummary == "" {
		memorySummary = "无"
	}
	if relationSummary == "" {
		relationSummary = "无"
	}
	fmt.Fprintf(&builder, "近期记忆:\n%s\n", memorySummary)
	fmt.Fprintf(&builder, "关系摘要:\n%s\n", relationSummary)
	fmt.Fprintf(&builder, "规则选择参数: roll=%.3f bias=%.3f score=%.3f\n", decision.Roll, decision.Bias, decision.Score)
	fmt.Fprintln(&builder, "输出约束:")
	fmt.Fprintln(&builder, "1) outcome: 18-44 字，第一人称，描述这次事件结果。")
	fmt.Fprintln(&builder, "2) bubble: 6-16 字，回合头顶短句。")
	fmt.Fprintln(&builder, "3) memory: 8-18 字，第一人称可记忆句。")
	fmt.Fprintln(&builder, "4) reasoning: 14-48 字，解释为何这样处理。")
	fmt.Fprintln(&builder, "5) 不能代替玩家发指令，必须体现单位自主判断。")
	return builder.String()
}

// fallbackRandomEventNarrationPayload 生成随机事件叙述的规则兜底文本。
func fallbackRandomEventNarrationPayload(
	event randomEventDefinition,
	branch randomEventBranch,
	effectSummary string,
) randomEventNarrationPayload {
	outcome := limitTextRunes(strings.TrimSpace(branch.Outcome), 44)
	if outcome == "" {
		outcome = limitTextRunes(fmt.Sprintf("我处理了%s。", event.Title), 44)
	}
	bubble := limitTextRunes(strings.TrimSpace(branch.Bubble), 16)
	if bubble == "" {
		bubble = limitTextRunes(fmt.Sprintf("我处理了%s。", event.Title), 16)
	}
	memory := limitTextRunes(strings.TrimSpace(branch.Memory), 18)
	if memory == "" {
		memory = limitTextRunes(fmt.Sprintf("我处理了%s。", event.Title), 18)
	}
	reasoning := strings.TrimSpace(effectSummary)
	if reasoning == "" {
		reasoning = "我按当前局势做了保守可行的处理。"
	}
	reasoning = limitTextRunes(reasoning, 48)
	return randomEventNarrationPayload{
		Outcome:   outcome,
		Bubble:    bubble,
		Memory:    memory,
		Reasoning: reasoning,
	}
}

// normalizeRandomEventNarrationPayload 规范化叙述字段长度并补齐缺省值。
func normalizeRandomEventNarrationPayload(payload randomEventNarrationPayload) randomEventNarrationPayload {
	payload.Outcome = limitTextRunes(strings.TrimSpace(payload.Outcome), 44)
	payload.Bubble = limitTextRunes(strings.TrimSpace(payload.Bubble), 16)
	payload.Memory = limitTextRunes(strings.TrimSpace(payload.Memory), 18)
	payload.Reasoning = limitTextRunes(strings.TrimSpace(payload.Reasoning), 48)
	return payload
}

// buildRandomEventRulePrompt 构建分支选择的规则提示词摘要。
func buildRandomEventRulePrompt(
	state State,
	actor unit.Record,
	event randomEventDefinition,
	branch randomEventBranch,
	decision randomEventBranchDecision,
) string {
	terrain := terrainDisplayName(terrainAt(state.Map, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}))
	return fmt.Sprintf(
		"事件=%s(%s)；分支=%s；天气=%s；地形=%s；人格[勇%.2f 忠%.2f 攻%.2f 谨%.2f 交%.2f 廉%.2f 稳%.2f 野%.2f]；状态[饥饿=%d 士气=%.2f 忠诚=%.2f 金币=%d]；规则选择 roll=%.3f bias=%.3f score=%.3f。所有动作由单位 AI 自主完成，玩家只发自然语言方针。",
		event.ID,
		event.Title,
		branch.ID,
		state.Weather.DisplayName,
		terrain,
		actor.Personality.Courage,
		actor.Personality.Loyalty,
		actor.Personality.Aggression,
		actor.Personality.Prudence,
		actor.Personality.Sociability,
		actor.Personality.Integrity,
		actor.Personality.Stability,
		actor.Personality.Ambition,
		actor.Status.Hunger,
		actor.Status.Morale,
		actor.Status.Loyalty,
		actor.Status.Wallet,
		decision.Roll,
		decision.Bias,
		decision.Score,
	)
}

// deterministicRandomEventRoll 生成与种子/回合绑定的稳定随机值。
func deterministicRandomEventRoll(seed int64, turn int, parts ...string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(fmt.Sprintf("seed=%d|turn=%d", seed, turn)))
	for _, part := range parts {
		_, _ = hasher.Write([]byte("|" + strings.TrimSpace(part)))
	}
	return float64(hasher.Sum32()%10000) / 10000
}

// clampInt 把整数限制在给定闭区间。
func clampInt(value int, low int, high int) int {
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

// formatSignedInt 格式化带正负号的整数。
func formatSignedInt(value int) string {
	if value > 0 {
		return fmt.Sprintf("+%d", value)
	}
	return fmt.Sprintf("%d", value)
}

// formatSignedFloat 格式化带正负号的浮点数。
func formatSignedFloat(value float64) string {
	if value > 0 {
		return fmt.Sprintf("+%.2f", value)
	}
	return fmt.Sprintf("%.2f", value)
}

// randomEventCatalog 返回完整随机事件模板库。
func randomEventCatalog() []randomEventDefinition {
	return []randomEventDefinition{
		dualBranchEvent(
			"bandit_toll",
			"路口盗贼拦截",
			"一伙盗贼拦在要道，要求过路费。",
			randomEventBranch{ID: "pay_toll", Label: "交钱放行", Outcome: "你压住火气交了买路钱，队伍平安通过。", Bubble: "先破财免战。", Memory: "我给盗贼交了过路钱。", WalletDelta: -12, MoraleDelta: -0.02},
			randomEventBranch{ID: "counter_raid", Label: "反设伏击", Outcome: "你借地形反制盗贼，捞回了些战利。", Bubble: "反手把他们压住。", Memory: "我反制了拦路盗贼。", WalletDelta: 10, MoraleDelta: 0.03, GainItemID: "rope", GainItemQty: 1},
		),
		dualBranchEvent(
			"bandit_informant",
			"山道线人",
			"一个自称线人的盗贼来卖敌方行踪。",
			randomEventBranch{ID: "buy_tip", Label: "花钱买情报", Outcome: "你掏钱换来一条可用情报。", Bubble: "这情报值钱。", Memory: "我买了敌情线索。", WalletDelta: -8, LoyaltyDelta: 0.01},
			randomEventBranch{ID: "threaten_tip", Label: "施压逼供", Outcome: "你强硬施压，线人吐出消息后仓皇逃走。", Bubble: "别跟我绕。", Memory: "我逼出了线报。", MoraleDelta: 0.02, LoyaltyDelta: -0.01},
		),
		dualBranchEvent(
			"village_festival",
			"村镇节庆",
			"附近村庄举行节庆，愿意招待巡逻小队。",
			randomEventBranch{ID: "join_festival", Label: "参加庆典", Outcome: "你短暂休整，士气回暖。", Bubble: "今晚总算能笑。", Memory: "我在节庆里缓了口气。", WalletDelta: -4, HungerDelta: 3, MoraleDelta: 0.05},
			randomEventBranch{ID: "keep_patrol", Label: "继续巡逻", Outcome: "你拒绝停留，换来村民补给支持。", Bubble: "我继续看路。", Memory: "我放弃庆典继续巡逻。", GainItemID: "ration", GainItemQty: 1, MoraleDelta: 0.01},
		),
		dualBranchEvent(
			"harvest_fair",
			"丰收集市",
			"集市开张，商贩用低价兜售补给。",
			randomEventBranch{ID: "bulk_buy", Label: "低价囤货", Outcome: "你趁低价补齐了行军物资。", Bubble: "先把补给囤足。", Memory: "我在集市低价囤货。", WalletDelta: -9, GainItemID: "ration", GainItemQty: 2},
			randomEventBranch{ID: "quick_trade", Label: "倒手赚差价", Outcome: "你快进快出赚到一点差价。", Bubble: "这一手有赚。", Memory: "我在集市赚了差价。", WalletDelta: 7, MoraleDelta: 0.02},
		),
		dualBranchEvent(
			"plague_whisper",
			"疫病传闻",
			"营地传来疫病风声，队伍开始不安。",
			randomEventBranch{ID: "quarantine", Label: "先做隔离", Outcome: "你主动隔离可疑接触，风险下降。", Bubble: "先隔离，别乱走。", Memory: "我先做了隔离。", WalletDelta: -5, MoraleDelta: 0.01, ConsumeItemID: "herb_bundle", ConsumeItemQty: 1},
			randomEventBranch{ID: "press_forward", Label: "顶着前进", Outcome: "你强压焦虑继续推进，气氛更紧。", Bubble: "别停，继续压。", Memory: "我顶着疫病传闻前推。", MoraleDelta: -0.04, LoyaltyDelta: -0.02},
		),
		dualBranchEvent(
			"fever_outbreak",
			"营地发热",
			"有人发热倒下，周围补给被迅速消耗。",
			randomEventBranch{ID: "share_medicine", Label: "分发药材", Outcome: "你优先分发药材，局面稳住了。", Bubble: "药先给重症。", Memory: "我把药先分给重症。", ConsumeItemID: "herb_bundle", ConsumeItemQty: 1, MoraleDelta: 0.03, LoyaltyDelta: 0.02},
			randomEventBranch{ID: "tight_rationing", Label: "压缩配给", Outcome: "你选择硬性限配，短期撑住但怨气上升。", Bubble: "先按配给线。", Memory: "我压缩了配给。", HungerDelta: -3, MoraleDelta: -0.03},
		),
		dualBranchEvent(
			"stranger_plea",
			"陌生人求助",
			"路边陌生人请求护送一段危险路。",
			randomEventBranch{ID: "escort_help", Label: "出手护送", Outcome: "你护送成功，对方留下谢礼。", Bubble: "我送你过这段。", Memory: "我护送了陌生人。", WalletDelta: 6, MoraleDelta: 0.03, LoyaltyDelta: 0.01},
			randomEventBranch{ID: "refuse_help", Label: "谨慎拒绝", Outcome: "你拒绝冒险，节省了战力但心态稍沉。", Bubble: "我顾不上这单。", Memory: "我拒绝了陌生求助。", MoraleDelta: -0.02},
		),
		dualBranchEvent(
			"lost_child",
			"迷路孩童",
			"一个孩童在战区边缘迷路哭喊。",
			randomEventBranch{ID: "guide_home", Label: "送回村口", Outcome: "你花了些时间把孩童送回，村民很感激。", Bubble: "别怕，我带你回。", Memory: "我把孩童送回村口。", WalletDelta: 4, MoraleDelta: 0.03},
			randomEventBranch{ID: "signal_locals", Label: "放信号弹", Outcome: "你原地放信号弹呼叫村民接应。", Bubble: "原地等接应。", Memory: "我呼叫了本地接应。", ConsumeItemID: "torch", ConsumeItemQty: 1, LoyaltyDelta: 0.01},
		),
		dualBranchEvent(
			"caravan_dispute",
			"商队纠纷",
			"两支商队为过路权争吵不休。",
			randomEventBranch{ID: "mediate", Label: "出面调停", Outcome: "你调停成功，双方各给一点谢金。", Bubble: "先按规矩排队。", Memory: "我调停了商队纠纷。", WalletDelta: 8, LoyaltyDelta: 0.01},
			randomEventBranch{ID: "take_fee", Label: "强收通行费", Outcome: "你强势收取过路费，短期获利但口碑受损。", Bubble: "先交路费。", Memory: "我强收了商队过路费。", WalletDelta: 11, MoraleDelta: -0.02, LoyaltyDelta: -0.02},
		),
		dualBranchEvent(
			"shrine_omen",
			"古祠异兆",
			"古祠前忽然起风，队伍议论是否祭祀。",
			randomEventBranch{ID: "offer_respect", Label: "按俗祭祀", Outcome: "你顺应民俗祭祀，队伍心态趋稳。", Bubble: "照规矩行礼。", Memory: "我在古祠前行了礼。", WalletDelta: -3, MoraleDelta: 0.04},
			randomEventBranch{ID: "ignore_omen", Label: "无视异兆", Outcome: "你选择无视，推进速度不受影响。", Bubble: "别被风声带跑。", Memory: "我无视了异兆。", MoraleDelta: -0.01, WalletDelta: 2},
		),
		dualBranchEvent(
			"deserter_offer",
			"逃兵投靠",
			"一名逃兵带着零散物资请求投靠。",
			randomEventBranch{ID: "accept_deserter", Label: "收留并盘问", Outcome: "你收留逃兵并拿到一些可用物资。", Bubble: "先收留再盘问。", Memory: "我收留了投靠逃兵。", GainItemID: "herb_bundle", GainItemQty: 1, MoraleDelta: 0.01},
			randomEventBranch{ID: "drive_away", Label: "驱离营地", Outcome: "你拒绝收留，减少了潜在隐患。", Bubble: "离营地远点。", Memory: "我驱离了投靠逃兵。", LoyaltyDelta: 0.01},
		),
		dualBranchEvent(
			"bridge_damage",
			"桥段坍塌",
			"前方木桥受损，后勤线被迫减速。",
			randomEventBranch{ID: "repair_bridge", Label: "就地修桥", Outcome: "你组织修桥，通行恢复。", Bubble: "先把桥补上。", Memory: "我修复了坍塌木桥。", ConsumeItemID: "wood", ConsumeItemQty: 1, MoraleDelta: 0.02},
			randomEventBranch{ID: "detour_crossing", Label: "绕路涉水", Outcome: "你率队绕路，虽然慢但省下材料。", Bubble: "绕过去更稳。", Memory: "我选择绕路涉水。", HungerDelta: -2},
		),
		dualBranchEvent(
			"mine_collapse",
			"矿井塌方",
			"矿井入口塌方，附近工人请求救援。",
			randomEventBranch{ID: "rescue_workers", Label: "优先救人", Outcome: "你先清理通道救人，收获感谢和补给。", Bubble: "先把人救出来。", Memory: "我在塌方中优先救人。", GainItemID: "iron_ore", GainItemQty: 1, MoraleDelta: 0.03},
			randomEventBranch{ID: "secure_ore", Label: "先保矿料", Outcome: "你先抢救矿料，资源保住但人心受压。", Bubble: "先抢出矿料。", Memory: "我先保住了矿料。", GainItemID: "iron_ore", GainItemQty: 2, MoraleDelta: -0.03},
		),
		dualBranchEvent(
			"wolf_pack",
			"狼群逼近",
			"夜里狼群逼近外围哨点。",
			randomEventBranch{ID: "set_fireline", Label: "点火驱狼", Outcome: "你用火线驱散狼群，守住外围。", Bubble: "点火，逼退狼群。", Memory: "我用火线驱散狼群。", ConsumeItemID: "torch", ConsumeItemQty: 1, MoraleDelta: 0.02},
			randomEventBranch{ID: "bait_and_hunt", Label: "诱敌猎取", Outcome: "你诱敌反打，带回了皮革。", Bubble: "设饵，反打。", Memory: "我反猎了狼群。", GainItemID: "leather", GainItemQty: 1, HungerDelta: 2},
		),
		dualBranchEvent(
			"drought_notice",
			"旱情预警",
			"哨骑回报附近河沟即将断流。",
			randomEventBranch{ID: "store_water", Label: "优先储水", Outcome: "你提前储水，后勤压力下降。", Bubble: "先把水备够。", Memory: "我提前做了储水。", WalletDelta: -4, MoraleDelta: 0.02},
			randomEventBranch{ID: "rush_forage", Label: "抢收补给", Outcome: "你催促抢收，补给增加但疲惫加重。", Bubble: "先抢收这波。", Memory: "我催了抢收补给。", GainItemID: "ration", GainItemQty: 1, HungerDelta: -2},
		),
		dualBranchEvent(
			"bard_visit",
			"流浪吟游者",
			"一位吟游者来到营地，愿意用故事换食物。",
			randomEventBranch{ID: "host_bard", Label: "留宿听歌", Outcome: "你招待吟游者，队伍情绪明显回升。", Bubble: "今晚听他唱。", Memory: "我让吟游者留宿。", ConsumeItemID: "ration", ConsumeItemQty: 1, MoraleDelta: 0.05},
			randomEventBranch{ID: "hire_bard", Label: "雇来传声", Outcome: "你给了点钱让他替你放风传话。", Bubble: "替我把话放出去。", Memory: "我雇了吟游者传话。", WalletDelta: -6, GainItemID: "carrier_pigeon", GainItemQty: 1},
		),
		dualBranchEvent(
			"healer_patrol",
			"游医巡诊",
			"路过游医愿意低价诊治伤员。",
			randomEventBranch{ID: "buy_treatment", Label: "付费诊治", Outcome: "你花钱做了基础诊治，士气回升。", Bubble: "先把伤口处理。", Memory: "我给队伍做了诊治。", WalletDelta: -7, MoraleDelta: 0.04},
			randomEventBranch{ID: "buy_supplies", Label: "改买药包", Outcome: "你没停诊治，直接买下药包备用。", Bubble: "药包先囤着。", Memory: "我买了游医药包。", WalletDelta: -5, GainItemID: "healing_potion", GainItemQty: 1},
		),
		dualBranchEvent(
			"refugee_column",
			"难民队列",
			"一支难民队列经过防线请求补给。",
			randomEventBranch{ID: "share_ration", Label: "分发口粮", Outcome: "你分出部分口粮，士气和忠诚都更稳。", Bubble: "先给他们口粮。", Memory: "我给难民分了口粮。", ConsumeItemID: "ration", ConsumeItemQty: 1, MoraleDelta: 0.03, LoyaltyDelta: 0.02},
			randomEventBranch{ID: "strict_screen", Label: "严格盘查", Outcome: "你坚持严查放行，防线秩序稳定。", Bubble: "先盘查再放行。", Memory: "我对难民做了严查。", LoyaltyDelta: 0.01, MoraleDelta: -0.01},
		),
		dualBranchEvent(
			"relic_auction",
			"旧物竞拍",
			"废墟边出现一场临时竞拍会。",
			randomEventBranch{ID: "bid_relic", Label: "竞拍旧物", Outcome: "你拍下一件有价值的旧物。", Bubble: "这件我拿了。", Memory: "我拍下了一件旧物。", WalletDelta: -10, GainItemID: "gemstone", GainItemQty: 1},
			randomEventBranch{ID: "resell_parts", Label: "拆件转卖", Outcome: "你没参与竞拍，改做拆件转卖小赚一笔。", Bubble: "拆件更划算。", Memory: "我拆件转卖赚了点。", WalletDelta: 6},
		),
		dualBranchEvent(
			"tax_audit",
			"税吏抽查",
			"地方税吏突然抽查补给与财货。",
			randomEventBranch{ID: "pay_tax", Label: "按章纳税", Outcome: "你照章纳税，避免了后续纠缠。", Bubble: "按章交税。", Memory: "我按章缴了税。", WalletDelta: -9, LoyaltyDelta: 0.01},
			randomEventBranch{ID: "argue_tax", Label: "据理减免", Outcome: "你据理力争，成功减免一部分税额。", Bubble: "这税额得重算。", Memory: "我争取了税额减免。", WalletDelta: -4, MoraleDelta: 0.01},
		),
		dualBranchEvent(
			"night_fire",
			"夜间失火",
			"营地边缘突发小范围失火。",
			randomEventBranch{ID: "fire_control", Label: "先控火线", Outcome: "你迅速控火，损失被压到最低。", Bubble: "先控火线！", Memory: "我第一时间控住了火。", ConsumeItemID: "torch", ConsumeItemQty: 1, MoraleDelta: 0.02},
			randomEventBranch{ID: "salvage_supplies", Label: "先抢物资", Outcome: "你先抢救物资，资源保住但队伍心态受压。", Bubble: "先把物资拖出来。", Memory: "我先抢救了物资。", GainItemID: "wood", GainItemQty: 1, MoraleDelta: -0.02},
		),
		dualBranchEvent(
			"masked_trader",
			"蒙面行商",
			"一名蒙面行商在夜里兜售稀有货。",
			randomEventBranch{ID: "buy_rare", Label: "买稀有货", Outcome: "你花钱买下一件少见物资。", Bubble: "这件稀货我要。", Memory: "我向蒙面商买了稀货。", WalletDelta: -11, GainItemID: "antidote", GainItemQty: 1},
			randomEventBranch{ID: "probe_identity", Label: "试探来路", Outcome: "你没有下单，先试探了对方来路。", Bubble: "先说你的来路。", Memory: "我试探了蒙面商身份。", LoyaltyDelta: 0.01},
		),
	}
}

// dualBranchEvent 快速构造“稳妥/冒险”双分支事件模板。
func dualBranchEvent(
	id string,
	title string,
	prompt string,
	safeBranch randomEventBranch,
	riskyBranch randomEventBranch,
) randomEventDefinition {
	return randomEventDefinition{
		ID:       strings.TrimSpace(id),
		Title:    strings.TrimSpace(title),
		Theme:    randomEventThemeFromID(id),
		Prompt:   strings.TrimSpace(prompt),
		Branches: []randomEventBranch{safeBranch, riskyBranch},
	}
}

// randomEventThemeFromID 按事件 ID 推断主题标签。
func randomEventThemeFromID(id string) string {
	normalized := strings.TrimSpace(id)
	switch {
	case strings.HasPrefix(normalized, "bandit"):
		return "bandit"
	case strings.Contains(normalized, "plague") || strings.Contains(normalized, "fever"):
		return "plague"
	case strings.Contains(normalized, "festival") || strings.Contains(normalized, "harvest") || strings.Contains(normalized, "bard"):
		return "festival"
	case strings.Contains(normalized, "stranger") || strings.Contains(normalized, "lost_child"):
		return "stranger_help"
	default:
		return "world_incident"
	}
}
