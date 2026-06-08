package session

// 文件说明：combat_shake 高压情绪覆写接入决策主循环（resolveExecution）的聚焦测试。
// 主循环插桩本身依赖完整 DB/LLM，难以无副作用单测；此处验证插桩所依赖的纯逻辑契约：
// 触发时 OverrideDecision 替换原决策、applyCombatShakeOverlay 富化文本、Modifiers 乘性合流；
// 未触发时决策与倍率原样透传（零行为变化）。命名带 combatShakeLoop 前缀避免撞名。

import (
	"testing"

	"qunxiang/backend/internal/unit"
)

// combatShakeLoopApplyOverride 复刻 service.go 主循环插桩在 Triggered 时的决策改写顺序：
// 先用 OverrideDecision 替换，再用 applyCombatShakeOverlay 富化文本。
func combatShakeLoopApplyOverride(normal unitDecisionPayload, resolution combatShakeResolution) unitDecisionPayload {
	decision := normal
	if resolution.Triggered {
		if resolution.OverrideDecision != nil {
			decision = *resolution.OverrideDecision
		}
		decision = applyCombatShakeOverlay(decision, resolution)
	}
	return decision
}

// TestCombatShakeLoopOverridesDecisionWhenTriggered 断言触发且带 OverrideDecision 时，
// 主循环用覆写决策取代正常决策（例如撤退/狂暴），原 LLM 动作被丢弃。
func TestCombatShakeLoopOverridesDecisionWhenTriggered(t *testing.T) {
	normal := unitDecisionPayload{
		Action:     DecisionActionMove,
		TargetQ:    5,
		TargetR:    6,
		Speak:      "原计划前压",
		Memory:     "继续推进",
		Reasoning:  "敌人露出破绽",
		NextAction: "前压",
	}
	override := unitDecisionPayload{
		Action:       DecisionActionAttack,
		TargetUnitID: "enemy-1",
	}
	resolution := combatShakeResolution{
		Triggered:        true,
		OverrideDecision: &override,
		Choice: combatShakeChoicePayload{
			Bubble:    "为他报仇！",
			Memory:    "我看见队友倒下",
			Reasoning: "怒火盖过理智",
		},
		Modifiers: actionModifiers{MoveMultiplier: 0.95, AttackMultiplier: 1.15},
	}

	final := combatShakeLoopApplyOverride(normal, resolution)

	if final.Action != DecisionActionAttack {
		t.Fatalf("触发覆写后动作应为 attack，得到 %q", final.Action)
	}
	if final.TargetUnitID != "enemy-1" {
		t.Fatalf("覆写决策的目标应保留为 enemy-1，得到 %q", final.TargetUnitID)
	}
	// 原决策的移动目标必须被丢弃（不能泄漏回最终决策）。
	if final.TargetQ == 5 && final.TargetR == 6 {
		t.Fatalf("覆写后不应保留原 LLM 决策的移动坐标")
	}
	// applyCombatShakeOverlay 富化文本：override 自身无 Speak/Memory，应补入 Choice 文本。
	if final.Speak != "为他报仇！" {
		t.Fatalf("覆写决策的 Speak 应由 shake choice 补入，得到 %q", final.Speak)
	}
	if final.Memory != "我看见队友倒下" {
		t.Fatalf("覆写决策的 Memory 应由 shake choice 补入，得到 %q", final.Memory)
	}
	if final.Reasoning == "" {
		t.Fatalf("覆写决策的 Reasoning 应包含 shake choice 的推理")
	}
}

// TestCombatShakeLoopOverlayWithoutOverride 断言触发但无 OverrideDecision（例如英雄主义只给倍率）时，
// 正常决策的动作保留，仅文本被富化、空字段被补全。
func TestCombatShakeLoopOverlayWithoutOverride(t *testing.T) {
	normal := unitDecisionPayload{
		Action:    DecisionActionAttack,
		Speak:     "", // 留空以验证 overlay 补入
		Reasoning: "正面迎敌",
	}
	resolution := combatShakeResolution{
		Triggered:        true,
		OverrideDecision: nil,
		Choice: combatShakeChoicePayload{
			Bubble:    "挡在他们前面",
			Reasoning: "不能让友军白白牺牲",
		},
		Modifiers: actionModifiers{MoveMultiplier: 1.05, AttackMultiplier: 1.1},
	}

	final := combatShakeLoopApplyOverride(normal, resolution)

	if final.Action != DecisionActionAttack {
		t.Fatalf("无覆写时应保留原决策动作，得到 %q", final.Action)
	}
	if final.Speak != "挡在他们前面" {
		t.Fatalf("空 Speak 应由 overlay 补入，得到 %q", final.Speak)
	}
	if final.Reasoning == "正面迎敌" {
		t.Fatalf("overlay 应把 shake 推理拼接进 Reasoning")
	}
}

// TestCombatShakeLoopUntriggeredIsPassthrough 断言未触发时决策原样透传（零行为变化）。
func TestCombatShakeLoopUntriggeredIsPassthrough(t *testing.T) {
	normal := unitDecisionPayload{
		Action:    DecisionActionMove,
		TargetQ:   3,
		TargetR:   4,
		Speak:     "保持阵型",
		Memory:    "稳住",
		Reasoning: "无异常",
	}
	resolution := combatShakeResolution{
		Triggered: false,
		// 未触发时 Modifiers 默认中性。
		Modifiers: actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1},
	}

	final := combatShakeLoopApplyOverride(normal, resolution)

	if final != normal {
		t.Fatalf("未触发时决策必须原样透传，期望 %+v，得到 %+v", normal, final)
	}
}

// TestCombatShakeLoopModifiersFold 断言主循环把 compliance / 饥饿 / shake 三路倍率乘性合流。
// 复刻 service.go 的 combineActionModifiers(compliance.Modifiers, hungerActionModifiers, shakeResolution.Modifiers)。
func TestCombatShakeLoopModifiersFold(t *testing.T) {
	complianceMods := actionModifiers{MoveMultiplier: 0.9, AttackMultiplier: 1.0}
	// 构造饥饿单位（Hunger<30 → 0.8/0.8）。
	hungry := unit.Record{}
	hungry.Status.Hunger = 10
	shakeMods := actionModifiers{MoveMultiplier: 0.95, AttackMultiplier: 1.15}

	combined := combineActionModifiers(complianceMods, hungerActionModifiers(hungry), shakeMods)

	wantMove := 0.9 * 0.8 * 0.95
	wantAttack := 1.0 * 0.8 * 1.15
	if !floatNear(combined.MoveMultiplier, wantMove) {
		t.Fatalf("MoveMultiplier 合流错误，期望 %.4f，得到 %.4f", wantMove, combined.MoveMultiplier)
	}
	if !floatNear(combined.AttackMultiplier, wantAttack) {
		t.Fatalf("AttackMultiplier 合流错误，期望 %.4f，得到 %.4f", wantAttack, combined.AttackMultiplier)
	}

	// 未触发时 shake 倍率为中性，不应改变合流结果。
	neutralShake := actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1}
	withNeutral := combineActionModifiers(complianceMods, hungerActionModifiers(hungry), neutralShake)
	if !floatNear(withNeutral.MoveMultiplier, 0.9*0.8) || !floatNear(withNeutral.AttackMultiplier, 1.0*0.8) {
		t.Fatalf("中性 shake 倍率不应改变合流，得到 %+v", withNeutral)
	}
}

// floatNear 浮点近似比较，避免乘法精度抖动。
func floatNear(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}
