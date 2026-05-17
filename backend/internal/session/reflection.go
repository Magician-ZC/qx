package session

// 文件说明：组织单位反思与双人对话记录流程，并把气泡、记忆、关系变化同步进会话状态。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/unit"
)

// recordUnitReflectionBestEffort 以容错方式生成并记录单位反思文本。
func (service *Service) recordUnitReflectionBestEffort(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	record *unit.Record,
	eventSummary string,
	interactionKind string,
) (unitReflectionPayload, ai.CompletionResult, bool) {
	if state == nil || record == nil {
		return unitReflectionPayload{}, ai.CompletionResult{}, false
	}

	payload, result, interaction, err := service.generateUnitReflection(
		ctx,
		*state,
		byID,
		*record,
		eventSummary,
		interactionKind,
	)
	appendLLMInteraction(state, interaction)
	if err != nil {
		logKind := "reflection_error"
		if interactionKind == "unit_dialogue" {
			logKind = "dialogue_error"
		}
		appendLog(
			state,
			logKind,
			"我这回合没接上话。",
			record.ID,
			"",
		)
		return unitReflectionPayload{}, result, false
	}

	service.rememberUnitBestEffort(ctx, record, state.TurnState.Turn, payload.Memory)
	return payload, result, true
}

// recordTradeDialogueBestEffort 记录交易场景下的双单位对话反思。
func (service *Service) recordTradeDialogueBestEffort(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	leftEvent string,
	rightEvent string,
) {
	service.recordPairDialogueBestEffort(
		ctx,
		state,
		byID,
		left,
		right,
		leftEvent,
		rightEvent,
		"",
	)
}

// recordPairDialogueBestEffort 记录两单位对话、日志与关系变化。
func (service *Service) recordPairDialogueBestEffort(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	left *unit.Record,
	right *unit.Record,
	leftEvent string,
	rightEvent string,
	summary string,
) {
	if state == nil || left == nil || right == nil {
		return
	}

	leftPayload, leftResult, leftOK := service.recordUnitReflectionBestEffort(
		ctx,
		state,
		byID,
		left,
		leftEvent,
		"unit_dialogue",
	)
	rightPayload, rightResult, rightOK := service.recordUnitReflectionBestEffort(
		ctx,
		state,
		byID,
		right,
		rightEvent,
		"unit_dialogue",
	)

	if leftOK {
		appendAIDialogue(state, *left, leftPayload.Bubble, leftResult)
	}
	if rightOK {
		appendAIDialogue(state, *right, rightPayload.Bubble, rightResult)
	}
	summary = resolvePairDialogueSummary(summary, left, right, leftPayload, rightPayload, leftEvent, rightEvent)

	appendLog(
		state,
		"unit_dialogue",
		summary,
		left.ID,
		right.ID,
	)

	if left.FactionID == right.FactionID {
		delta := relationDelta{
			Trust:     0.35,
			Fear:      -0.06,
			Affection: 0.28,
			Rivalry:   -0.12,
		}
		service.applyMutualRelationShiftBestEffort(ctx, state, left, right, delta, delta, "行动间隙交谈")
		return
	}

	leftDelta := relationDelta{
		Trust:     0.12,
		Fear:      -0.04,
		Affection: 0.06,
		Rivalry:   -0.04,
	}
	rightDelta := leftDelta
	incrementCrossFactionInteraction(state, "cross_faction_dialogue", left, right)
	service.applyMutualRelationShiftBestEffort(ctx, state, left, right, leftDelta, rightDelta, "跨阵营短暂交谈")
}

// resolvePairDialogueSummary 生成双单位对话日志摘要文案。
func resolvePairDialogueSummary(
	summary string,
	left *unit.Record,
	right *unit.Record,
	leftPayload unitReflectionPayload,
	rightPayload unitReflectionPayload,
	leftEvent string,
	rightEvent string,
) string {
	summary = strings.TrimSpace(summary)
	if summary != "" {
		return summary
	}
	leftBubble := strings.TrimSpace(leftPayload.Bubble)
	rightBubble := strings.TrimSpace(rightPayload.Bubble)
	switch {
	case left != nil && right != nil && leftBubble != "" && rightBubble != "":
		return fmt.Sprintf("%s：%s；%s：%s", left.DisplayName(), leftBubble, right.DisplayName(), rightBubble)
	case left != nil && leftBubble != "":
		return fmt.Sprintf("%s：%s", left.DisplayName(), leftBubble)
	case right != nil && rightBubble != "":
		return fmt.Sprintf("%s：%s", right.DisplayName(), rightBubble)
	}

	leftEvent = strings.TrimSpace(leftEvent)
	rightEvent = strings.TrimSpace(rightEvent)
	switch {
	case left != nil && right != nil && leftEvent != "" && rightEvent != "":
		return fmt.Sprintf("%s：%s；%s：%s", left.DisplayName(), leftEvent, right.DisplayName(), rightEvent)
	case left != nil && leftEvent != "":
		return fmt.Sprintf("%s：%s", left.DisplayName(), leftEvent)
	case right != nil && rightEvent != "":
		return fmt.Sprintf("%s：%s", right.DisplayName(), rightEvent)
	case left != nil && right != nil:
		return fmt.Sprintf("我和 %s 在行动间隙聊了几句。", right.DisplayName())
	default:
		return "我们在行动间隙聊了几句。"
	}
}

// appendAIDialogue 把 AI 生成台词写入对话历史。
func appendAIDialogue(state *State, record unit.Record, message string, result ai.CompletionResult) {
	if state == nil {
		return
	}

	message = strings.TrimSpace(message)
	if message == "" {
		return
	}

	appendDialogue(state, DialogueMessage{
		ID:           uuid.NewString(),
		UnitID:       record.ID,
		Speaker:      record.DisplayName(),
		Message:      message,
		Turn:         state.TurnState.Turn,
		Phase:        state.TurnState.Phase,
		OccurredAt:   time.Now().UTC(),
		Provider:     result.Provider,
		Model:        result.Model,
		UsedFallback: result.UsedFallback,
	})
}
