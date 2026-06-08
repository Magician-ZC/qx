package session

// 文件说明：单位自由恋爱、生育、野人归属与感化的轻量规则。

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

const (
	minRomanceDialogueTurns = 2
	pregnancyDurationTurns  = 5
)

type romanceConsentPayload struct {
	LeftAgree  bool   `json:"left_agree"`
	RightAgree bool   `json:"right_agree"`
	LeftLine   string `json:"left_line"`
	RightLine  string `json:"right_line"`
	Summary    string `json:"summary"`
	Reasoning  string `json:"reasoning"`
}

type romanceProposalPayload struct {
	Proposer         string `json:"proposer"`
	ProposalAccepted bool   `json:"proposal_accepted"`
	LeftLine         string `json:"left_line"`
	RightLine        string `json:"right_line"`
	Summary          string `json:"summary"`
	Reasoning        string `json:"reasoning"`
}

type childProfilePayload struct {
	Name        string           `json:"name"`
	Gender      string           `json:"gender"`
	Biography   string           `json:"biography"`
	Personality unit.Personality `json:"personality"`
	NameReason  string           `json:"name_reason"`
}

var romanceConsentSchema = []byte(`{
  "type":"object",
  "properties":{
    "left_agree":{"type":"boolean"},
    "right_agree":{"type":"boolean"},
    "left_line":{"type":"string","minLength":1,"maxLength":18},
    "right_line":{"type":"string","minLength":1,"maxLength":18},
    "summary":{"type":"string","minLength":1,"maxLength":60},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["left_agree","right_agree","left_line","right_line","summary","reasoning"],
  "additionalProperties":false
}`)

var romanceProposalSchema = []byte(`{
  "type":"object",
  "properties":{
    "proposer":{"type":"string","enum":["left","right","none"]},
    "proposal_accepted":{"type":"boolean"},
    "left_line":{"type":"string","maxLength":18},
    "right_line":{"type":"string","maxLength":18},
    "summary":{"type":"string","minLength":1,"maxLength":60},
    "reasoning":{"type":"string","minLength":1}
  },
  "required":["proposer","proposal_accepted","left_line","right_line","summary","reasoning"],
  "additionalProperties":false
}`)

var childProfileSchema = []byte(`{
  "type":"object",
  "properties":{
    "name":{"type":"string","minLength":1,"maxLength":16},
    "gender":{"type":"string","enum":["male","female","nonbinary"]},
    "biography":{"type":"string","minLength":1,"maxLength":180},
    "personality":{
      "type":"object",
      "properties":{
        "courage":{"type":"number","minimum":0.05,"maximum":0.95},
        "loyalty":{"type":"number","minimum":0.05,"maximum":0.95},
        "aggression":{"type":"number","minimum":0.05,"maximum":0.95},
        "prudence":{"type":"number","minimum":0.05,"maximum":0.95},
        "sociability":{"type":"number","minimum":0.05,"maximum":0.95},
        "integrity":{"type":"number","minimum":0.05,"maximum":0.95},
        "stability":{"type":"number","minimum":0.05,"maximum":0.95},
        "ambition":{"type":"number","minimum":0.05,"maximum":0.95}
      },
      "required":["courage","loyalty","aggression","prudence","sociability","integrity","stability","ambition"],
      "additionalProperties":false
    },
    "name_reason":{"type":"string","minLength":1,"maxLength":80}
  },
  "required":["name","gender","biography","personality","name_reason"],
  "additionalProperties":false
}`)

func (service *Service) maybeResolveRomanceAndFamily(ctx context.Context, state *State, byID map[string]*unit.Record, left *unit.Record, right *unit.Record) {
	if state == nil || left == nil || right == nil || left.ID == right.ID || !isBattleReady(*left) || !isBattleReady(*right) {
		return
	}
	if left.Social.LoverUnitID == "" &&
		right.Social.LoverUnitID == "" &&
		romanceScore(*state, *left, *right, "fall_in_love")%100 < 10 &&
		service.canAttemptRomanceProposal(ctx, *state, left, right) {
		proposal, result, interaction, ok := service.requestRomanceProposal(ctx, *state, byID, left, right)
		service.appendLLMInteractionWithSpend(ctx, state, interaction)
		if !ok || proposal.Proposer == "none" {
			return
		}
		if !proposal.ProposalAccepted {
			service.recordRomanceProposalDialogue(ctx, state, left, right, proposal, result)
			appendLog(state, "romance_hold", romanceProposalHoldMessage(proposal, left, right), left.ID, right.ID)
			return
		}
		left.Social.LoverUnitID = right.ID
		right.Social.LoverUnitID = left.ID
		left.Social.LastRomanceTurn = state.TurnState.Turn
		right.Social.LastRomanceTurn = state.TurnState.Turn
		service.recordRomanceProposalDialogue(ctx, state, left, right, proposal, result)
		appendLog(state, "romance", romanceProposalSuccessMessage(proposal, left, right), left.ID, right.ID)
		_ = service.units.Save(ctx, *left)
		_ = service.units.Save(ctx, *right)
		return
	}
	if canAttemptFamilyAction(*state, left, right) && romanceScore(*state, *left, *right, "child")%100 < 6 {
		consent, result, interaction, ok := service.requestRomanceConsent(ctx, *state, byID, left, right, "是否共同养育新生命")
		service.appendLLMInteractionWithSpend(ctx, state, interaction)
		if !ok || !consent.LeftAgree || !consent.RightAgree {
			service.recordRomanceConsentDialogue(ctx, state, left, right, consent, result)
			appendLog(state, "family_hold", romanceConsentHoldMessage(consent, left, right), left.ID, right.ID)
			return
		}
		service.recordRomanceConsentDialogue(ctx, state, left, right, consent, result)
		pregnancy := registerPregnancy(state, left, right)
		_ = service.units.Save(ctx, *left)
		_ = service.units.Save(ctx, *right)
		appendLog(state, "family", romanceConsentSuccessMessage(consent, left, right), left.ID, right.ID)
		appendLog(state, "pregnancy", pregnancyStartedMessage(*state, byID, pregnancy, left, right), left.ID, right.ID)
	}
	service.maybeConvertWildling(ctx, state, byID, left, right)
}

// executeRomance 执行 LLM 主动选择的表白候选；仍会二次请求双方同意，不能由玩家或单方强迫成立。
func (service *Service) executeRomance(ctx context.Context, state *State, byID map[string]*unit.Record, actor *unit.Record, decision unitDecisionPayload) error {
	target, err := resolveAdjacentInteractionTarget(byID, actor, decision.TargetUnitID)
	if err != nil {
		appendLog(state, "romance_hold", "我想把话说清，但眼前没有合适的人。", actor.ID, decision.TargetUnitID)
		return nil
	}
	if dialogueBlockedByPolicy(*state, actor, target) {
		appendLog(state, "romance_hold", "我想把话说清，但现在不适合越过阵营边界。", actor.ID, target.ID)
		return nil
	}
	if actor.Social.LoverUnitID != "" || target.Social.LoverUnitID != "" {
		appendLog(state, "romance_hold", "我想把话说清，但这段关系并不适合继续推进。", actor.ID, target.ID)
		return nil
	}
	if !service.canAttemptRomanceProposal(ctx, *state, actor, target) {
		appendLog(state, "romance_hold", "我想把话说清，但我们还需要多聊几轮，不能只按方针推进关系。", actor.ID, target.ID)
		return service.applyActionHungerCost(ctx, state, actor, "克制表白")
	}
	proposal, result, interaction, ok := service.requestRomanceProposal(ctx, *state, byID, actor, target)
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	if !ok || proposal.Proposer == "none" {
		appendLog(state, "romance_hold", "我把话咽了回去，还不是时候。", actor.ID, target.ID)
		return nil
	}
	if !proposal.ProposalAccepted {
		service.recordRomanceProposalDialogue(ctx, state, actor, target, proposal, result)
		appendLog(state, "romance_hold", romanceProposalHoldMessage(proposal, actor, target), actor.ID, target.ID)
		return service.applyActionHungerCost(ctx, state, actor, "表白")
	}
	actor.Social.LoverUnitID = target.ID
	target.Social.LoverUnitID = actor.ID
	actor.Social.LastRomanceTurn = state.TurnState.Turn
	target.Social.LastRomanceTurn = state.TurnState.Turn
	service.recordRomanceProposalDialogue(ctx, state, actor, target, proposal, result)
	appendLog(state, "romance", romanceProposalSuccessMessage(proposal, actor, target), actor.ID, target.ID)
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *target); err != nil {
		return err
	}
	return service.applyActionHungerCost(ctx, state, actor, "表白")
}

// executeFamily 执行 LLM 主动选择的共同养育候选；必须双方再次同意才会创建孩子单位。
func (service *Service) executeFamily(ctx context.Context, state *State, byID map[string]*unit.Record, actor *unit.Record, decision unitDecisionPayload) error {
	target, err := resolveAdjacentInteractionTarget(byID, actor, decision.TargetUnitID)
	if err != nil {
		appendLog(state, "family_hold", "我想谈谈未来，但眼前没有合适的人。", actor.ID, decision.TargetUnitID)
		return nil
	}
	if dialogueBlockedByPolicy(*state, actor, target) {
		appendLog(state, "family_hold", "我想谈谈未来，但现在不适合越过阵营边界。", actor.ID, target.ID)
		return nil
	}
	if !canAttemptFamilyAction(*state, actor, target) {
		appendLog(state, "family_hold", "我想谈谈未来，但现在还不到共同养育的时候。", actor.ID, target.ID)
		return nil
	}
	consent, result, interaction, ok := service.requestRomanceConsent(ctx, *state, byID, actor, target, "是否共同养育新生命")
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	service.recordRomanceConsentDialogue(ctx, state, actor, target, consent, result)
	if !ok || !consent.LeftAgree || !consent.RightAgree {
		appendLog(state, "family_hold", romanceConsentHoldMessage(consent, actor, target), actor.ID, target.ID)
		return service.applyActionHungerCost(ctx, state, actor, "商量家庭")
	}
	pregnancy := registerPregnancy(state, actor, target)
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *target); err != nil {
		return err
	}
	appendLog(state, "family", romanceConsentSuccessMessage(consent, actor, target), actor.ID, target.ID)
	appendLog(state, "pregnancy", pregnancyStartedMessage(*state, byID, pregnancy, actor, target), actor.ID, target.ID)
	return service.applyActionHungerCost(ctx, state, actor, "商量家庭")
}

func registerPregnancy(state *State, left *unit.Record, right *unit.Record) PregnancyState {
	if state == nil || left == nil || right == nil {
		return PregnancyState{}
	}
	pregnancy := PregnancyState{
		ID:             uuid.NewString(),
		ParentUnitIDs:  []string{left.ID, right.ID},
		PregnantUnitID: choosePregnantUnitID(left, right),
		StartedTurn:    state.TurnState.Turn,
		DueTurn:        state.TurnState.Turn + pregnancyDurationTurns,
	}
	state.Pregnancies = append(state.Pregnancies, pregnancy)
	left.Social.LastRomanceTurn = state.TurnState.Turn
	right.Social.LastRomanceTurn = state.TurnState.Turn
	return pregnancy
}

func choosePregnantUnitID(left *unit.Record, right *unit.Record) string {
	if left == nil {
		if right == nil {
			return ""
		}
		return right.ID
	}
	if right == nil {
		return left.ID
	}
	if isFemaleGender(left.Identity.Gender) {
		return left.ID
	}
	if isFemaleGender(right.Identity.Gender) {
		return right.ID
	}
	return left.ID
}

func isFemaleGender(gender string) bool {
	normalized := strings.ToLower(strings.TrimSpace(gender))
	return normalized == "female" || normalized == "f" || strings.Contains(normalized, "女")
}

func pendingPregnancyForPair(state State, leftID string, rightID string) *PregnancyState {
	for index := range state.Pregnancies {
		pregnancy := &state.Pregnancies[index]
		if pregnancyHasParents(*pregnancy, leftID, rightID) {
			return pregnancy
		}
	}
	return nil
}

func pregnancyHasParents(pregnancy PregnancyState, leftID string, rightID string) bool {
	if len(pregnancy.ParentUnitIDs) < 2 {
		return false
	}
	first := pregnancy.ParentUnitIDs[0]
	second := pregnancy.ParentUnitIDs[1]
	return (first == leftID && second == rightID) || (first == rightID && second == leftID)
}

func pregnancyHasUnit(pregnancy PregnancyState, unitID string) bool {
	for _, parentID := range pregnancy.ParentUnitIDs {
		if parentID == unitID {
			return true
		}
	}
	return false
}

func isUnitPregnant(state State, unitID string) bool {
	if strings.TrimSpace(unitID) == "" {
		return false
	}
	for _, pregnancy := range state.Pregnancies {
		if pregnancy.PregnantUnitID == unitID {
			return true
		}
	}
	return false
}

func pregnancyBlockedAction(action DecisionAction) bool {
	switch action {
	case DecisionActionAttack,
		DecisionActionCharge,
		DecisionActionHeavyAttack,
		DecisionActionSkill,
		DecisionActionDefend,
		DecisionActionObserve,
		DecisionActionAssist,
		DecisionActionBuild,
		DecisionActionDemolish:
		return true
	default:
		return false
	}
}

func pregnancyStartedMessage(state State, byID map[string]*unit.Record, pregnancy PregnancyState, left *unit.Record, right *unit.Record) string {
	pregnantName := pregnancy.PregnantUnitID
	if pregnant := byID[pregnancy.PregnantUnitID]; pregnant != nil {
		pregnantName = pregnant.DisplayName()
	}
	leftName := "一方"
	rightName := "另一方"
	if left != nil {
		leftName = left.DisplayName()
	}
	if right != nil {
		rightName = right.DisplayName()
	}
	return fmt.Sprintf("%s 和 %s 决定共同养育新生命；%s 进入孕期，预计第 %d 回合出生。", leftName, rightName, pregnantName, pregnancy.DueTurn)
}

func pregnancyStatusForPrompt(state State, byID map[string]*unit.Record, actor *unit.Record) string {
	if actor == nil {
		return "无"
	}
	for _, pregnancy := range state.Pregnancies {
		if pregnancy.PregnantUnitID != actor.ID && !pregnancyHasUnit(pregnancy, actor.ID) {
			continue
		}
		pregnantName := socialTieName(pregnancy.PregnantUnitID, byID)
		if pregnantName == "" {
			pregnantName = pregnancy.PregnantUnitID
		}
		remaining := pregnancy.DueTurn - state.TurnState.Turn
		if remaining < 0 {
			remaining = 0
		}
		if pregnancy.PregnantUnitID == actor.ID {
			return fmt.Sprintf("你正在怀孕，预产第 %d 回合，剩余 %d 回合；孕期不能参与战斗和建筑。", pregnancy.DueTurn, remaining)
		}
		return fmt.Sprintf("%s 正在怀孕，预产第 %d 回合，剩余 %d 回合。", pregnantName, pregnancy.DueTurn, remaining)
	}
	return "无"
}

func (service *Service) resolveDuePregnancies(ctx context.Context, state *State) error {
	if service == nil || state == nil || len(state.Pregnancies) == 0 {
		return nil
	}
	units, err := service.units.ListBySession(ctx, state.ID)
	if err != nil {
		return err
	}
	byID := mapRecordsByID(units)
	remaining := make([]PregnancyState, 0, len(state.Pregnancies))
	for _, pregnancy := range state.Pregnancies {
		if pregnancy.DueTurn > state.TurnState.Turn {
			remaining = append(remaining, pregnancy)
			continue
		}
		if len(pregnancy.ParentUnitIDs) < 2 {
			appendLog(state, "pregnancy_lost", "一段孕期记录不完整，没有顺利诞生。", pregnancy.PregnantUnitID, "")
			continue
		}
		left := byID[pregnancy.ParentUnitIDs[0]]
		right := byID[pregnancy.ParentUnitIDs[1]]
		pregnant := byID[pregnancy.PregnantUnitID]
		if left == nil || right == nil || pregnant == nil || !isBattleReady(*left) || !isBattleReady(*right) || !isBattleReady(*pregnant) {
			appendLog(state, "pregnancy_lost", "新生命没能等到出生。", pregnancy.PregnantUnitID, "")
			continue
		}
		birthCoord := findChildBirthCoord(state.Map, byID, *pregnant)
		profile, _, interaction := service.generateChildProfile(ctx, *state, byID, *left, *right, *pregnant)
		service.appendLLMInteractionWithSpend(ctx, state, interaction)
		child := createChildUnit(*state, *left, *right, birthCoord, profile)
		if err := service.units.Save(ctx, child); err != nil {
			appendLog(state, "romance_error", "新生命没能加入战场。", left.ID, right.ID)
			remaining = append(remaining, pregnancy)
			continue
		}
		byID[child.ID] = &child
		if child.FactionID == state.PlayerFactionID {
			state.PlayerUnitIDs = append(state.PlayerUnitIDs, child.ID)
		} else if child.FactionID == state.EnemyFactionID {
			state.EnemyUnitIDs = append(state.EnemyUnitIDs, child.ID)
		} else {
			state.WildUnitIDs = append(state.WildUnitIDs, child.ID)
		}
		// 中途出生的玩家子嗣也要进大世界离线调度（M7.3-real-4b，开关关时 no-op；仅玩家阵营）。
		service.seedAmbientForNewUnit(ctx, state, child)
		left.Social.ChildUnitIDs = append(left.Social.ChildUnitIDs, child.ID)
		right.Social.ChildUnitIDs = append(right.Social.ChildUnitIDs, child.ID)
		left.Social.LastRomanceTurn = state.TurnState.Turn
		right.Social.LastRomanceTurn = state.TurnState.Turn
		if err := service.units.Save(ctx, *left); err != nil {
			return err
		}
		if err := service.units.Save(ctx, *right); err != nil {
			return err
		}
		appendDialogue(state, DialogueMessage{
			ID:         uuid.NewString(),
			UnitID:     left.ID,
			Speaker:    "family_birth",
			Message:    fmt.Sprintf("我和%s给孩子取名%s。", right.DisplayName(), child.DisplayName()),
			Turn:       state.TurnState.Turn,
			Phase:      state.TurnState.Phase,
			OccurredAt: time.Now().UTC(),
		})
		appendDialogue(state, DialogueMessage{
			ID:         uuid.NewString(),
			UnitID:     right.ID,
			Speaker:    "family_birth",
			Message:    fmt.Sprintf("我和%s迎来了%s。", left.DisplayName(), child.DisplayName()),
			Turn:       state.TurnState.Turn,
			Phase:      state.TurnState.Phase,
			OccurredAt: time.Now().UTC(),
		})
		appendLog(state, "birth", fmt.Sprintf("%s 和 %s 的孩子 %s 出生在 %d,%d；归属：%s。", left.DisplayName(), right.DisplayName(), child.DisplayName(), child.Status.PositionQ, child.Status.PositionR, child.FactionID), left.ID, right.ID)
	}
	state.Pregnancies = remaining
	return nil
}

func (service *Service) canAttemptRomanceProposal(ctx context.Context, state State, left *unit.Record, right *unit.Record) bool {
	if service == nil || left == nil || right == nil {
		return false
	}
	if !hasEnoughPairDialogueTurns(state, left.ID, right.ID, minRomanceDialogueTurns) {
		return false
	}
	return service.hasFamiliarRelation(ctx, left.ID, right.ID) && service.hasFamiliarRelation(ctx, right.ID, left.ID)
}

func (service *Service) requestRomanceProposal(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
) (romanceProposalPayload, ai.CompletionResult, LLMInteraction, bool) {
	leftTier := service.relationTier(ctx, left.ID, right.ID)
	rightTier := service.relationTier(ctx, right.ID, left.ID)
	systemPrompt := fmt.Sprintf(
		"你要同时扮演《群像》中的两个相邻 AI 单位：%s 和 %s。两人已经有过多轮真实交流，但玩家或阵营方针不能替他们决定亲密关系。请判断这段交谈里，是否会由其中一人主动提出确认亲密关系；只有先有人主动提出，另一人又真心接受，关系才会成立。若没人会主动提出，就把 proposer 设为 none。只能返回 JSON。",
		left.DisplayName(),
		right.DisplayName(),
	)
	userPrompt := buildRomanceProposalPrompt(state, byID, left, right, leftTier, rightTier)
	if service == nil || service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{Debug: ai.CompletionDebug{FallbackCause: err.Error()}}
		return romanceProposalPayload{}, result, buildLLMInteraction(state, left.ID, "romance_proposal", "", systemPrompt, userPrompt, result, err.Error()), false
	}
	if llmBudgetGuardrailActive(state) {
		result := budgetGuardrailResult(state)
		payload := romanceProposalPayload{
			Proposer:         "none",
			ProposalAccepted: false,
			LeftLine:         "",
			RightLine:        "",
			Summary:          "预算护栏生效，这段交谈没有推进到关系确认。",
			Reasoning:        "预算护栏生效时不能代替单位生成关系确认。",
		}
		return payload, result, buildLLMInteraction(state, left.ID, "romance_proposal", payload.Summary, systemPrompt, userPrompt, result, ""), true
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskDialogue,
		SchemaName:     "session_romance_proposal",
		ResponseSchema: romanceProposalSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.55,
		MaxTokens:      260,
		Timeout:        llmRequestTimeout,
	})
	if err != nil {
		return romanceProposalPayload{}, result, buildLLMInteraction(state, left.ID, "romance_proposal", "", systemPrompt, userPrompt, result, err.Error()), false
	}

	var payload romanceProposalPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode romance proposal payload: %v", err)
		return romanceProposalPayload{}, result, buildLLMInteraction(state, left.ID, "romance_proposal", "", systemPrompt, userPrompt, result, cause), false
	}
	payload = normalizeRomanceProposalPayload(payload)
	if payload.Summary == "" || payload.Reasoning == "" {
		cause := "romance proposal payload is incomplete"
		return payload, result, buildLLMInteraction(state, left.ID, "romance_proposal", "", systemPrompt, userPrompt, result, cause), false
	}
	return payload, result, buildLLMInteraction(state, left.ID, "romance_proposal", payload.Summary, systemPrompt, userPrompt, result, ""), true
}

func (service *Service) requestRomanceConsent(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	proposal string,
) (romanceConsentPayload, ai.CompletionResult, LLMInteraction, bool) {
	return service.requestPairConsent(ctx, state, byID, left, right, proposal, "恋爱、结婚、亲密关系或家庭选择", "romance_consent")
}

func (service *Service) requestPairConsent(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	proposal string,
	consentScope string,
	interactionKind string,
) (romanceConsentPayload, ai.CompletionResult, LLMInteraction, bool) {
	consentScope = strings.TrimSpace(consentScope)
	if consentScope == "" {
		consentScope = "需要双方自愿同意的关系或合作选择"
	}
	interactionKind = strings.TrimSpace(interactionKind)
	if interactionKind == "" {
		interactionKind = "unit_consent"
	}
	systemPrompt := fmt.Sprintf(
		"你要同时扮演《群像》中的两个相邻 AI 单位：%s 和 %s。玩家不能替任何单位决定%s。请分别判断两人是否自愿同意这个提议；只有双方都真心同意，才把 left_agree 和 right_agree 都设为 true。只能返回 JSON。",
		left.DisplayName(),
		right.DisplayName(),
		consentScope,
	)
	userPrompt := buildRomanceConsentPrompt(state, byID, left, right, proposal)
	if service == nil || service.llm == nil {
		err := fmt.Errorf("llm client is disabled")
		result := ai.CompletionResult{Debug: ai.CompletionDebug{FallbackCause: err.Error()}}
		return romanceConsentPayload{}, result, buildLLMInteraction(state, left.ID, interactionKind, "", systemPrompt, userPrompt, result, err.Error()), false
	}
	if llmBudgetGuardrailActive(state) {
		result := budgetGuardrailResult(state)
		payload := romanceConsentPayload{
			LeftAgree:  false,
			RightAgree: false,
			LeftLine:   "先别急。",
			RightLine:  "我也想想。",
			Summary:    "预算护栏生效，两人暂不推进这项选择。",
			Reasoning:  "预算护栏生效时不能代替双方确认同意。",
		}
		return payload, result, buildLLMInteraction(state, left.ID, interactionKind, payload.Summary, systemPrompt, userPrompt, result, ""), true
	}

	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskDialogue,
		SchemaName:     "session_pair_consent",
		ResponseSchema: romanceConsentSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.55,
		MaxTokens:      260,
		Timeout:        llmRequestTimeout,
	})
	if err != nil {
		return romanceConsentPayload{}, result, buildLLMInteraction(state, left.ID, interactionKind, "", systemPrompt, userPrompt, result, err.Error()), false
	}

	var payload romanceConsentPayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode romance consent payload: %v", err)
		return romanceConsentPayload{}, result, buildLLMInteraction(state, left.ID, interactionKind, "", systemPrompt, userPrompt, result, cause), false
	}
	payload = normalizeRomanceConsentPayload(payload)
	if payload.LeftLine == "" || payload.RightLine == "" || payload.Summary == "" || payload.Reasoning == "" {
		cause := "romance consent payload is incomplete"
		return payload, result, buildLLMInteraction(state, left.ID, interactionKind, "", systemPrompt, userPrompt, result, cause), false
	}

	return payload, result, buildLLMInteraction(state, left.ID, interactionKind, payload.Summary, systemPrompt, userPrompt, result, ""), true
}

func buildRomanceProposalPrompt(
	state State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	leftTier string,
	rightTier string,
) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "交流门槛: 已满足至少 %d 个不同回合的真实单位对话；否则系统不会请求本判断。\n", minRomanceDialogueTurns)
	fmt.Fprintln(&builder, "触发前提: romance 不是按方针立即发生的动作；必须先经过多次真实交流、互相记住对方，并形成最低熟悉关系。")
	fmt.Fprintln(&builder, "刚发生的事情: 两人已经在行动间隙真实交谈过，并且关系已达到最低熟悉度。")
	fmt.Fprintf(&builder, "阵营方针背景: %s\n", state.GlobalDirective.Text)
	fmt.Fprintf(&builder, "%s 的资料: %s\n", left.DisplayName(), describeUnit(*left, nil))
	fmt.Fprintf(&builder, "%s 的家庭关系: %s\n", left.DisplayName(), summarizeSocialTiesForPrompt(*left, byID))
	fmt.Fprintf(&builder, "%s 的性格: %s\n", left.DisplayName(), summarizeActorPersonality(*left))
	fmt.Fprintf(&builder, "%s 对 %s 的关系层级: %s\n", left.DisplayName(), right.DisplayName(), firstNonEmptyText(leftTier, "陌生"))
	fmt.Fprintf(&builder, "%s 的环境: %s\n", left.DisplayName(), summarizeImmediateEnvironment(state, byID, left))
	fmt.Fprintf(&builder, "%s 的记忆:\n%s\n", left.DisplayName(), summarizeUnitMemoryWithTurn(*left, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "%s 的资料: %s\n", right.DisplayName(), describeUnit(*right, nil))
	fmt.Fprintf(&builder, "%s 的家庭关系: %s\n", right.DisplayName(), summarizeSocialTiesForPrompt(*right, byID))
	fmt.Fprintf(&builder, "%s 的性格: %s\n", right.DisplayName(), summarizeActorPersonality(*right))
	fmt.Fprintf(&builder, "%s 对 %s 的关系层级: %s\n", right.DisplayName(), left.DisplayName(), firstNonEmptyText(rightTier, "陌生"))
	fmt.Fprintf(&builder, "%s 的环境: %s\n", right.DisplayName(), summarizeImmediateEnvironment(state, byID, right))
	fmt.Fprintf(&builder, "%s 的记忆:\n%s\n", right.DisplayName(), summarizeUnitMemoryWithTurn(*right, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "最近相关对话:\n%s\n%s\n", summarizeDialogueHistory(state.DialogueHistory, left.ID, state.TurnState.Turn, 4), summarizeDialogueHistory(state.DialogueHistory, right.ID, state.TurnState.Turn, 4))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "生产与建筑边界: %s\n", unsupportedStructureRuleSummary())
	fmt.Fprintln(&builder, "规则:")
	fmt.Fprintln(&builder, "1. 只有在多轮交流、记忆、性格和关系层级共同支持时，proposer 才能是 left 或 right；否则必须是 none。")
	fmt.Fprintln(&builder, "2. proposer=left/right 时，left_line 和 right_line 要写成这次表露与回应的两句短话。")
	fmt.Fprintln(&builder, "3. proposal_accepted 只代表被表露的一方是否真心接受；陌生、犹豫、只是服从命令或只为完成方针，都必须是 false。")
	fmt.Fprintln(&builder, "4. 不要因为玩家方针、阵营方针、剧情方便或阵营利益而硬推关系。")
	fmt.Fprintln(&builder, "5. 如果当前更像战术协作、互相提醒、普通交谈，而不是表露关系，就输出 proposer=none。")
	fmt.Fprintln(&builder, "6. JSON 字符串内部不要使用英文双引号；引用词语时用中文引号。")
	return builder.String()
}

func buildRomanceConsentPrompt(state State, byID map[string]*unit.Record, left *unit.Record, right *unit.Record, proposal string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "当前阶段: %s\n", state.TurnState.Phase)
	fmt.Fprintf(&builder, "关系提议: %s\n", strings.TrimSpace(proposal))
	fmt.Fprintf(&builder, "阵营方针背景: %s\n", state.GlobalDirective.Text)
	fmt.Fprintf(&builder, "%s 的资料: %s\n", left.DisplayName(), describeUnit(*left, nil))
	fmt.Fprintf(&builder, "%s 的家庭关系: %s\n", left.DisplayName(), summarizeSocialTiesForPrompt(*left, byID))
	fmt.Fprintf(&builder, "%s 的性格: %s\n", left.DisplayName(), summarizeActorPersonality(*left))
	fmt.Fprintf(&builder, "%s 的环境: %s\n", left.DisplayName(), summarizeImmediateEnvironment(state, byID, left))
	fmt.Fprintf(&builder, "%s 的记忆:\n%s\n", left.DisplayName(), summarizeUnitMemoryWithTurn(*left, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "%s 的资料: %s\n", right.DisplayName(), describeUnit(*right, nil))
	fmt.Fprintf(&builder, "%s 的家庭关系: %s\n", right.DisplayName(), summarizeSocialTiesForPrompt(*right, byID))
	fmt.Fprintf(&builder, "%s 的性格: %s\n", right.DisplayName(), summarizeActorPersonality(*right))
	fmt.Fprintf(&builder, "%s 的环境: %s\n", right.DisplayName(), summarizeImmediateEnvironment(state, byID, right))
	fmt.Fprintf(&builder, "%s 的记忆:\n%s\n", right.DisplayName(), summarizeUnitMemoryWithTurn(*right, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "最近相关对话:\n%s\n%s\n", summarizeDialogueHistory(state.DialogueHistory, left.ID, state.TurnState.Turn, 4), summarizeDialogueHistory(state.DialogueHistory, right.ID, state.TurnState.Turn, 4))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "生产与建筑边界: %s\n", unsupportedStructureRuleSummary())
	fmt.Fprintln(&builder, "规则:")
	fmt.Fprintln(&builder, "1. left_agree 只代表左侧单位自己的真实意愿，right_agree 只代表右侧单位自己的真实意愿。")
	fmt.Fprintln(&builder, "2. 任何一方犹豫、抗拒、被命令裹挟、利益不清或状态不适，都应设为 false。")
	fmt.Fprintln(&builder, "3. 不要因为玩家要求、阵营方针、阵营利益或剧情方便而强迫同意。")
	fmt.Fprintln(&builder, "4. left_line/right_line 是两人当场会说的一句短话。")
	fmt.Fprintln(&builder, "5. summary 要说明双方是否达成一致。")
	fmt.Fprintln(&builder, "6. JSON 字符串内部不要使用英文双引号；引用词语时用中文引号。")
	return builder.String()
}

func normalizeRomanceProposalPayload(payload romanceProposalPayload) romanceProposalPayload {
	payload.Proposer = strings.TrimSpace(strings.ToLower(payload.Proposer))
	switch payload.Proposer {
	case "left", "right", "none":
	default:
		payload.Proposer = "none"
	}
	payload.LeftLine = limitTextRunes(strings.TrimSpace(payload.LeftLine), 18)
	payload.RightLine = limitTextRunes(strings.TrimSpace(payload.RightLine), 18)
	payload.Summary = limitTextRunes(strings.TrimSpace(payload.Summary), 60)
	payload.Reasoning = strings.TrimSpace(payload.Reasoning)
	if payload.Proposer == "none" {
		payload.ProposalAccepted = false
	}
	if payload.Summary == "" {
		payload.Summary = limitTextRunes(payload.Reasoning, 60)
	}
	return payload
}

func normalizeRomanceConsentPayload(payload romanceConsentPayload) romanceConsentPayload {
	payload.LeftLine = limitTextRunes(strings.TrimSpace(payload.LeftLine), 18)
	payload.RightLine = limitTextRunes(strings.TrimSpace(payload.RightLine), 18)
	payload.Summary = limitTextRunes(strings.TrimSpace(payload.Summary), 60)
	payload.Reasoning = strings.TrimSpace(payload.Reasoning)
	if payload.LeftLine == "" {
		payload.LeftLine = "我需要想清楚。"
	}
	if payload.RightLine == "" {
		payload.RightLine = "我也要自己决定。"
	}
	if payload.Summary == "" {
		payload.Summary = limitTextRunes(payload.Reasoning, 60)
	}
	return payload
}

func (service *Service) recordRomanceConsentDialogue(ctx context.Context, state *State, left *unit.Record, right *unit.Record, consent romanceConsentPayload, result ai.CompletionResult) {
	if state == nil || left == nil || right == nil {
		return
	}
	appendAIDialogue(state, *left, consent.LeftLine, result)
	appendAIDialogue(state, *right, consent.RightLine, result)
	if service != nil {
		_ = service.rememberUnitWithSource(ctx, left, state.TurnState.Turn, consent.LeftLine, "romance_consent", 2)
		_ = service.rememberUnitWithSource(ctx, right, state.TurnState.Turn, consent.RightLine, "romance_consent", 2)
	}
}

func (service *Service) recordRomanceProposalDialogue(ctx context.Context, state *State, left *unit.Record, right *unit.Record, proposal romanceProposalPayload, result ai.CompletionResult) {
	if state == nil || left == nil || right == nil {
		return
	}
	leftLine := romanceProposalDialogueLine(proposal, "left")
	rightLine := romanceProposalDialogueLine(proposal, "right")
	appendAIDialogue(state, *left, leftLine, result)
	appendAIDialogue(state, *right, rightLine, result)
	if service != nil {
		_ = service.rememberUnitWithSource(ctx, left, state.TurnState.Turn, leftLine, "romance_proposal", 2)
		_ = service.rememberUnitWithSource(ctx, right, state.TurnState.Turn, rightLine, "romance_proposal", 2)
	}
	appendLog(
		state,
		"romance_proposal",
		fmt.Sprintf("【表白】%s：%s；%s：%s", left.DisplayName(), strings.TrimPrefix(leftLine, "【表白】"), right.DisplayName(), strings.TrimPrefix(rightLine, "【表白】")),
		left.ID,
		right.ID,
	)
}

func romanceProposalDialogueLine(proposal romanceProposalPayload, side string) string {
	line := ""
	if side == "left" {
		line = strings.TrimSpace(proposal.LeftLine)
	} else {
		line = strings.TrimSpace(proposal.RightLine)
	}
	if line == "" {
		switch {
		case proposal.Proposer == side:
			line = "我把心意说出口。"
		case proposal.ProposalAccepted:
			line = "我认真接受这份心意。"
		default:
			line = "我还需要想一想。"
		}
	}
	return "【表白】" + line
}

func romanceConsentSuccessMessage(consent romanceConsentPayload, left *unit.Record, right *unit.Record) string {
	if strings.TrimSpace(consent.Summary) != "" {
		return consent.Summary
	}
	return fmt.Sprintf("%s 和 %s 在战场间隙确认了心意。", left.DisplayName(), right.DisplayName())
}

func romanceConsentHoldMessage(consent romanceConsentPayload, left *unit.Record, right *unit.Record) string {
	if strings.TrimSpace(consent.Summary) != "" {
		return consent.Summary
	}
	return fmt.Sprintf("%s 和 %s 没有把关系继续推进。", left.DisplayName(), right.DisplayName())
}

func romanceProposalSuccessMessage(proposal romanceProposalPayload, left *unit.Record, right *unit.Record) string {
	if strings.TrimSpace(proposal.Summary) != "" {
		return proposal.Summary
	}
	initiator, responder := romanceProposalRoles(proposal, left, right)
	return fmt.Sprintf("%s 主动表露心意，%s 接受了这份关系。", initiator.DisplayName(), responder.DisplayName())
}

func romanceProposalHoldMessage(proposal romanceProposalPayload, left *unit.Record, right *unit.Record) string {
	if strings.TrimSpace(proposal.Summary) != "" {
		return proposal.Summary
	}
	initiator, responder := romanceProposalRoles(proposal, left, right)
	if initiator == nil || responder == nil {
		return fmt.Sprintf("%s 和 %s 没有把关系继续推进。", left.DisplayName(), right.DisplayName())
	}
	return fmt.Sprintf("%s 主动表露了心意，但 %s 还没有同意。", initiator.DisplayName(), responder.DisplayName())
}

func romanceProposalRoles(proposal romanceProposalPayload, left *unit.Record, right *unit.Record) (*unit.Record, *unit.Record) {
	switch proposal.Proposer {
	case "left":
		return left, right
	case "right":
		return right, left
	default:
		return nil, nil
	}
}

func hasRecentPairDialogue(state State, leftID string, rightID string) bool {
	return hasEnoughPairDialogueTurns(state, leftID, rightID, 1)
}

func hasEnoughPairDialogueTurns(state State, leftID string, rightID string, requiredTurns int) bool {
	if strings.TrimSpace(leftID) == "" || strings.TrimSpace(rightID) == "" || leftID == rightID {
		return false
	}
	if requiredTurns <= 0 {
		requiredTurns = 1
	}
	turns := make(map[int]struct{}, requiredTurns)
	for index := len(state.Logs) - 1; index >= 0; index-- {
		entry := state.Logs[index]
		if entry.Kind != "unit_dialogue" && entry.Kind != "say" {
			continue
		}
		if samePair(entry.ActorUnitID, entry.TargetUnitID, leftID, rightID) {
			turns[entry.Turn] = struct{}{}
			if len(turns) >= requiredTurns {
				return true
			}
		}
	}
	return false
}

func (service *Service) generateChildProfile(
	ctx context.Context,
	state State,
	byID map[string]*unit.Record,
	left unit.Record,
	right unit.Record,
	pregnant unit.Record,
) (childProfilePayload, ai.CompletionResult, LLMInteraction) {
	fallback := fallbackChildProfile(state, left, right)
	systemPrompt := fmt.Sprintf(
		"你是《群像》的家族档案生成器。%s 与 %s 的孩子即将出生，请同时扮演两位父母共同为孩子取名，并生成孩子的性别、生平和人格向量。名字要像父母在战局中会认真取的名字，不要叫“崽”，不要使用系统旁白。只能返回 JSON。",
		left.DisplayName(),
		right.DisplayName(),
	)
	userPrompt := buildChildProfilePrompt(state, byID, left, right, pregnant)
	if service == nil || service.llm == nil {
		result := ai.CompletionResult{Debug: ai.CompletionDebug{FallbackCause: "llm client is disabled"}}
		return fallback, result, buildLLMInteraction(state, pregnant.ID, "child_profile", fallback.Name, systemPrompt, userPrompt, result, result.Debug.FallbackCause)
	}
	if llmBudgetGuardrailActive(state) {
		result := budgetGuardrailResult(state)
		return fallback, result, buildLLMInteraction(state, pregnant.ID, "child_profile", fallback.Name, systemPrompt, userPrompt, result, result.Debug.FallbackCause)
	}
	result, err := service.llm.GenerateJSON(ctx, ai.CompletionRequest{
		Task:           ai.TaskBackstory,
		SchemaName:     "session_child_profile",
		ResponseSchema: childProfileSchema,
		SystemPrompt:   systemPrompt,
		UserPrompt:     userPrompt,
		Temperature:    0.7,
		MaxTokens:      420,
		Timeout:        llmRequestTimeout,
	})
	if err != nil {
		return fallback, result, buildLLMInteraction(state, pregnant.ID, "child_profile", fallback.Name, systemPrompt, userPrompt, result, err.Error())
	}
	var payload childProfilePayload
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		cause := fmt.Sprintf("decode child profile payload: %v", err)
		return fallback, result, buildLLMInteraction(state, pregnant.ID, "child_profile", fallback.Name, systemPrompt, userPrompt, result, cause)
	}
	payload = normalizeChildProfile(payload, fallback)
	return payload, result, buildLLMInteraction(state, pregnant.ID, "child_profile", payload.Name, systemPrompt, userPrompt, result, "")
}

func buildChildProfilePrompt(state State, byID map[string]*unit.Record, left unit.Record, right unit.Record, pregnant unit.Record) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "当前回合: %d\n", state.TurnState.Turn)
	fmt.Fprintf(&builder, "出生背景: 怀孕单位=%s，孕期已满 %d 回合。\n", pregnant.DisplayName(), pregnancyDurationTurns)
	fmt.Fprintf(&builder, "父母A资料: %s\n", describeUnit(left, nil))
	fmt.Fprintf(&builder, "父母A家庭关系: %s\n", summarizeSocialTiesForPrompt(left, byID))
	fmt.Fprintf(&builder, "父母A性格: %s\n", summarizeActorPersonality(left))
	if bio := strings.TrimSpace(left.Identity.Biography); bio != "" {
		fmt.Fprintf(&builder, "父母A生平: %s\n", bio)
	}
	fmt.Fprintf(&builder, "父母B资料: %s\n", describeUnit(right, nil))
	fmt.Fprintf(&builder, "父母B家庭关系: %s\n", summarizeSocialTiesForPrompt(right, byID))
	fmt.Fprintf(&builder, "父母B性格: %s\n", summarizeActorPersonality(right))
	if bio := strings.TrimSpace(right.Identity.Biography); bio != "" {
		fmt.Fprintf(&builder, "父母B生平: %s\n", bio)
	}
	fmt.Fprintf(&builder, "最近相关对话:\n%s\n%s\n", summarizeDialogueHistory(state.DialogueHistory, left.ID, state.TurnState.Turn, 6), summarizeDialogueHistory(state.DialogueHistory, right.ID, state.TurnState.Turn, 6))
	fmt.Fprintf(&builder, "最近事件:\n%s\n", summarizeLogs(state.Logs, state.TurnState.Turn, 8))
	fmt.Fprintln(&builder, "生成规则:")
	fmt.Fprintln(&builder, "1. name 是父母共同认真取的中文名，1-8 个汉字，不要带“崽”“孩子”“宝宝”等占位词。")
	fmt.Fprintln(&builder, "2. biography 用第三人称写 60-140 字，要说明父母是谁、出生在战局中，以及孩子可能继承或反差于父母的性格。")
	fmt.Fprintln(&builder, "3. personality 八项数值必须在 0.05 到 0.95 之间，既参考父母性格，也允许有一点独立差异。")
	fmt.Fprintln(&builder, "4. name_reason 写父母为什么这样取名，最多 80 字。")
	return builder.String()
}

func normalizeChildProfile(payload childProfilePayload, fallback childProfilePayload) childProfilePayload {
	payload.Name = limitTextRunes(strings.TrimSpace(payload.Name), 16)
	if payload.Name == "" || strings.Contains(payload.Name, "崽") || strings.Contains(payload.Name, "宝宝") || strings.Contains(payload.Name, "孩子") {
		payload.Name = fallback.Name
	}
	if strings.TrimSpace(payload.Gender) == "" {
		payload.Gender = fallback.Gender
	} else {
		payload.Gender = normalizeGender(payload.Gender, 0)
	}
	payload.Biography = limitTextRunes(strings.TrimSpace(payload.Biography), 180)
	if payload.Biography == "" {
		payload.Biography = fallback.Biography
	}
	payload.NameReason = limitTextRunes(strings.TrimSpace(payload.NameReason), 80)
	if payload.NameReason == "" {
		payload.NameReason = fallback.NameReason
	}
	payload.Personality = normalizeCandidatePersonality(payload.Personality, int64(len(payload.Name)+len(payload.Biography)))
	return payload
}

func fallbackChildProfile(state State, left unit.Record, right unit.Record) childProfilePayload {
	seed := int64(romanceScore(state, left, right, "seed"))
	name := fmt.Sprintf("%s%s新", firstRuneText(left.DisplayName()), firstRuneText(right.DisplayName()))
	return childProfilePayload{
		Name:        name,
		Gender:      normalizeGender("", int(seed)),
		Biography:   fmt.Sprintf("%s 与 %s 在战局中诞下的孩子，出生便被双方家族记在心上。", left.DisplayName(), right.DisplayName()),
		Personality: blendChildPersonality(seed, left.Personality, right.Personality),
		NameReason:  fmt.Sprintf("取父母名中的字，记住%s与%s共同迎来的新生命。", left.DisplayName(), right.DisplayName()),
	}
}

func blendChildPersonality(seed int64, left unit.Personality, right unit.Personality) unit.Personality {
	fallback := unit.GeneratePersonality(seed)
	blend := func(a float64, b float64, f float64) float64 {
		return clampFloat((a+b)/2*0.78+f*0.22, 0.05, 0.95)
	}
	return unit.Personality{
		Courage:     blend(left.Courage, right.Courage, fallback.Courage),
		Loyalty:     blend(left.Loyalty, right.Loyalty, fallback.Loyalty),
		Aggression:  blend(left.Aggression, right.Aggression, fallback.Aggression),
		Prudence:    blend(left.Prudence, right.Prudence, fallback.Prudence),
		Sociability: blend(left.Sociability, right.Sociability, fallback.Sociability),
		Integrity:   blend(left.Integrity, right.Integrity, fallback.Integrity),
		Stability:   blend(left.Stability, right.Stability, fallback.Stability),
		Ambition:    blend(left.Ambition, right.Ambition, fallback.Ambition),
	}
}

func findChildBirthCoord(snapshot world.MapSnapshot, byID map[string]*unit.Record, pregnant unit.Record) world.Coord {
	origin := world.Coord{Q: pregnant.Status.PositionQ, R: pregnant.Status.PositionR}
	candidates := append([]world.Coord{}, axialNeighbors(origin)...)
	for _, coord := range candidates {
		if inBounds(snapshot, coord) && !occupiedByAnother(byID, "", coord) {
			return coord
		}
	}
	best := world.Coord{}
	found := false
	bestDistance := 1 << 30
	for _, tile := range snapshot.Tiles {
		if occupiedByAnother(byID, "", tile.Coord) {
			continue
		}
		distance := unit.HexDistance(origin.Q, origin.R, tile.Coord.Q, tile.Coord.R)
		if !found || distance < bestDistance {
			best = tile.Coord
			bestDistance = distance
			found = true
		}
	}
	if found {
		return best
	}
	return origin
}

func createChildUnit(state State, left unit.Record, right unit.Record, coord world.Coord, profile childProfilePayload) unit.Record {
	factionID := FactionWildling
	if left.FactionID == right.FactionID {
		factionID = left.FactionID
	}
	seed := int64(romanceScore(state, left, right, "seed"))
	child, err := bootstrapBattleUnit(seed, state.ID, factionID, profile.Name, coord)
	if err != nil {
		child = unit.BootstrapRecord(seed, state.ID, factionID, profile.Name)
		child.Status.PositionQ = coord.Q
		child.Status.PositionR = coord.R
	}
	child.Identity.Gender = normalizeGender(profile.Gender, int(seed))
	child.Identity.Lineage = "child"
	child.Identity.Age = 0
	child.Identity.Biography = profile.Biography
	child.Personality = normalizeCandidatePersonality(profile.Personality, seed)
	child.Social.ParentUnitIDs = []string{left.ID, right.ID}
	child.Social.ChildUnitIDs = []string{}
	child.Social.BornTurn = state.TurnState.Turn
	child.Social.Wildling = factionID == FactionWildling
	child.Status.HP = 60
	child.Status.Attack = 4
	child.Status.Defense = 2
	child.Status.Move = 3
	return child
}

func (service *Service) maybeConvertWildling(ctx context.Context, state *State, byID map[string]*unit.Record, left *unit.Record, right *unit.Record) {
	if state == nil || left == nil || right == nil {
		return
	}
	var wild *unit.Record
	var contact *unit.Record
	if left.FactionID == FactionWildling {
		wild, contact = left, right
	} else if right.FactionID == FactionWildling {
		wild, contact = right, left
	} else {
		return
	}
	if contact.FactionID != state.PlayerFactionID && contact.FactionID != state.EnemyFactionID {
		return
	}
	if romanceScore(*state, *wild, *contact, "convert")%100 >= 18 {
		return
	}
	if byID == nil {
		byID = map[string]*unit.Record{left.ID: left, right.ID: right}
	}
	proposal := fmt.Sprintf("是否由 %s 自愿接受 %s 的感化，并加入 %s。", wild.DisplayName(), contact.DisplayName(), contact.FactionID)
	consent, result, interaction, ok := service.requestPairConsent(ctx, *state, byID, wild, contact, proposal, "感化、招募、加入阵营或改变归属", "wildling_consent")
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	if !ok || !consent.LeftAgree || !consent.RightAgree {
		service.recordRomanceConsentDialogue(ctx, state, wild, contact, consent, result)
		appendLog(state, "wildling_convert_hold", romanceConsentHoldMessage(consent, wild, contact), contact.ID, wild.ID)
		return
	}
	service.recordRomanceConsentDialogue(ctx, state, wild, contact, consent, result)
	removeID := func(ids []string, id string) []string {
		result := ids[:0]
		for _, current := range ids {
			if current != id {
				result = append(result, current)
			}
		}
		return result
	}
	state.WildUnitIDs = removeID(state.WildUnitIDs, wild.ID)
	wild.FactionID = contact.FactionID
	wild.Social.Wildling = false
	if wild.FactionID == state.PlayerFactionID {
		state.PlayerUnitIDs = append(state.PlayerUnitIDs, wild.ID)
	} else {
		state.EnemyUnitIDs = append(state.EnemyUnitIDs, wild.ID)
	}
	_ = service.units.Save(ctx, *wild)
	// 归化入伙的玩家阵营野民也要进大世界离线调度（M7.3-real-4b，开关关时 no-op；仅玩家阵营）。
	service.seedAmbientForNewUnit(ctx, state, *wild)
	message := strings.TrimSpace(consent.Summary)
	if message == "" {
		message = fmt.Sprintf("%s 被 %s 感化，加入 %s。", wild.DisplayName(), contact.DisplayName(), contact.FactionID)
	}
	appendLog(state, "wildling_convert", message, contact.ID, wild.ID)
}

func romanceScore(state State, left unit.Record, right unit.Record, salt string) uint32 {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(fmt.Sprintf("%s|%d|%s|%s|%s", state.ID, state.TurnState.Turn, left.ID, right.ID, salt)))
	return hash.Sum32()
}

func firstRuneText(text string) string {
	text = strings.TrimSpace(text)
	for _, r := range text {
		return string(r)
	}
	return uuid.NewString()[:1]
}
