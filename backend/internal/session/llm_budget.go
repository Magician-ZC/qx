package session

// 文件说明：实现会话级 LLM 成本护栏，超预算时返回低成本兜底决策与对话模板。

import (
	"fmt"
	"strings"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/unit"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	sessionLLMHardCapUSD        = 3.0
	sessionLLMGuardrailFloorUSD = 2.85
)

// llmBudgetGuardrailActive 判断当前会话是否触发 LLM 成本护栏。
func llmBudgetGuardrailActive(state State) bool {
	return state.Metrics.LLMEstimatedCostUSD >= sessionLLMGuardrailFloorUSD
}

// budgetGuardrailResult 生成“预算护栏兜底”调用结果，统一标记 provider/model/debug 字段。
func budgetGuardrailResult(state State) ai.CompletionResult {
	cause := fmt.Sprintf(
		"llm budget guardrail active: estimated_cost=%.6f, guardrail=%.2f, hard_cap=%.2f",
		state.Metrics.LLMEstimatedCostUSD,
		sessionLLMGuardrailFloorUSD,
		sessionLLMHardCapUSD,
	)
	return ai.CompletionResult{
		Provider:     "budget_guardrail",
		Model:        "session_cap",
		UsedFallback: true,
		Debug: ai.CompletionDebug{
			FallbackCause: cause,
		},
	}
}

// budgetGuardrailDecision 在预算受限时返回保守行动决策，避免继续消耗昂贵推理。
func budgetGuardrailDecision(actor *unit.Record) unitDecisionPayload {
	line := "我先稳住。"
	if actor != nil && strings.TrimSpace(actor.DisplayName()) != "" {
		line = fmt.Sprintf("%s，我先稳住。", actor.DisplayName())
	}
	nextAction := limitTextRunes(line, 12)
	speak := limitTextRunes(line, 16)
	return unitDecisionPayload{
		Action:     DecisionActionHold,
		NextAction: nextAction,
		Speak:      speak,
		Memory:     "我先稳住等窗口。",
		Reasoning:  "我先稳住，等更明确的机会。",
	}
}

// budgetGuardrailDialogueReply 在预算护栏开启时返回固定对话应答模板。
func budgetGuardrailDialogueReply() dialogueReplyPayload {
	return dialogueReplyPayload{
		Reply:  "收到。我会按当前环境自己先稳住。",
		Mood:   "steady",
		Intent: "acknowledge",
		Memory: "我先按局势稳住。",
	}
}

// budgetGuardrailReflection 在预算护栏开启时返回最小反思输出，文本长度受限以控成本。
func budgetGuardrailReflection(eventSummary string) unitReflectionPayload {
	bubble := limitTextRunes(strings.TrimSpace(eventSummary), 16)
	if bubble == "" {
		bubble = "我先稳住。"
	}
	return unitReflectionPayload{
		Bubble: bubble,
		Memory: "我先稳住再观察。",
	}
}

// budgetGuardrailDeploymentChoice 在预算护栏开启时返回“观望”交易选择。
func budgetGuardrailDeploymentChoice() deploymentChoicePayload {
	return deploymentChoicePayload{
		CandidateID: "hold",
		LeftLine:    "我先观望。",
		RightLine:   "我先观望。",
		LeftMemory:  "我先观望局势。",
		RightMemory: "我先观望局势。",
		Summary:     "我先和对方继续观望，这回合先不急着成交。",
		Reasoning:   "我先和对方继续观望，这回合先不急着成交。",
	}
}

// budgetGuardrailUpkeepChoice 在预算护栏开启时返回保守补给策略。
func budgetGuardrailUpkeepChoice() upkeepChoicePayload {
	return upkeepChoicePayload{
		ShouldAct: false,
		Bubble:    "先稳住。",
		Memory:    "我先稳住体力。",
		Reasoning: "我先按保守节奏调整体力。",
	}
}

// budgetGuardrailCombatShakeChoice 在预算护栏开启时返回稳健的战斗情绪处置。
func budgetGuardrailCombatShakeChoice() combatShakeChoicePayload {
	return combatShakeChoicePayload{
		Action:    "continue",
		Bubble:    "先稳住。",
		Memory:    "我先稳住。",
		Reasoning: "我先稳住，不在这拍冒进。",
	}
}
