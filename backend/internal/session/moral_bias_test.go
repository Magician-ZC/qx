package session

// 文件说明：道德自治偏置（F2 ②）的单元测试。
// 守护：①偏置确定性（同输入同输出）；②阵营对齐（秩序→守序协作动作、混乱→攻伐、自由→游走获更高权）；
// ③零道德轴/中性动作恒 1.0（向后兼容）；④付费无关（不读 wallet）；⑤prompt 注入文案契合阵营/倾向；
// ⑥PickMoralBiasedCandidate 稳定 tie-break。

import (
	"strings"
	"testing"

	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/unit"
)

func orderUnit() unit.Record {
	return unit.Record{Faction: faction.IDOrder, MoralAlignment: faction.MoralAlignment{Order: 80, Freedom: 10, Chaos: 10}}
}
func chaosUnit() unit.Record {
	return unit.Record{Faction: faction.IDChaos, MoralAlignment: faction.MoralAlignment{Chaos: 80, Freedom: 10, Order: 10}}
}
func freedomUnit() unit.Record {
	return unit.Record{Faction: faction.IDFreedom, MoralAlignment: faction.MoralAlignment{Freedom: 80, Order: 10, Chaos: 10}}
}

// TestMoralActionBias_AxisAlignment 验证道德倾向放大契合动作：秩序→protect/build/bond、混乱→assault、自由→explore。
func TestMoralActionBias_AxisAlignment(t *testing.T) {
	order := orderUnit()
	chaos := chaosUnit()
	free := freedomUnit()

	// 秩序者：守序协作动作 > 1.0；攻伐动作 == 1.0（chaos 标签未被 order 驱动）。
	if w := MoralActionBias(order, "protect"); w <= moralBiasFloor {
		t.Fatalf("秩序者 protect 应被放大，得到 %.4f", w)
	}
	if w := MoralActionBias(order, "assault"); w != moralBiasFloor {
		t.Fatalf("秩序者 assault 不应被放大（chaos 标签），得到 %.4f", w)
	}
	// 混乱者：assault > 1.0；protect == 1.0。
	if w := MoralActionBias(chaos, "assault"); w <= moralBiasFloor {
		t.Fatalf("混乱者 assault 应被放大，得到 %.4f", w)
	}
	if w := MoralActionBias(chaos, "protect"); w != moralBiasFloor {
		t.Fatalf("混乱者 protect 不应被放大，得到 %.4f", w)
	}
	// 自由者：explore > 1.0。
	if w := MoralActionBias(free, "explore"); w <= moralBiasFloor {
		t.Fatalf("自由者 explore 应被放大，得到 %.4f", w)
	}
	// 上界：乘数恒 ≤ floor+gain。
	if w := MoralActionBias(chaos, "assault"); w > moralBiasFloor+moralBiasGain+1e-9 {
		t.Fatalf("乘数越上界 %.4f", w)
	}
}

// TestMoralActionBias_NeutralCases 验证零道德轴 / 空标签 / 未登记标签 → 恒 1.0（向后兼容、零行为）。
func TestMoralActionBias_NeutralCases(t *testing.T) {
	zero := unit.Record{} // 道德轴全零
	if w := MoralActionBias(zero, "assault"); w != moralBiasFloor {
		t.Fatalf("零道德轴应中性 1.0，得到 %.4f", w)
	}
	if w := MoralActionBias(chaosUnit(), ""); w != moralBiasFloor {
		t.Fatalf("空标签应中性 1.0，得到 %.4f", w)
	}
	if w := MoralActionBias(chaosUnit(), "no_such_tag"); w != moralBiasFloor {
		t.Fatalf("未登记标签应中性 1.0，得到 %.4f", w)
	}
}

// TestMoralActionBias_Deterministic 验证同输入同输出（纯函数、付费无关——record 无 wallet 参与）。
func TestMoralActionBias_Deterministic(t *testing.T) {
	c := chaosUnit()
	for i := 0; i < 20; i++ {
		if MoralActionBias(c, "assault") != MoralActionBias(c, "assault") {
			t.Fatalf("非确定性")
		}
	}
	// 付费无关：改 wallet 不影响偏置。
	c.Status.Wallet = 999999
	if MoralActionBias(c, "assault") != MoralActionBias(chaosUnit(), "assault") {
		t.Fatalf("偏置不应随 wallet 变化（付费不进）")
	}
}

// TestOnlineActionMoralTag 验证动作→道德标签映射，中性动作返回空。
func TestOnlineActionMoralTag(t *testing.T) {
	cases := map[DecisionAction]string{
		DecisionActionAttack:   "assault",
		DecisionActionCharge:   "assault",
		DecisionActionMove:     "explore",
		DecisionActionAssist:   "protect",
		DecisionActionBuild:    "build",
		DecisionActionDialogue: "bond",
		DecisionActionEat:      "", // 中性生存动作
		DecisionActionObserve:  "",
	}
	for action, want := range cases {
		if got := OnlineActionMoralTag(action); got != want {
			t.Fatalf("动作 %s 标签应为 %q，得到 %q", action, want, got)
		}
	}
}

// TestMoralDecisionContext 验证 prompt 注入文案：有阵营给信条句、有倾向给「心更偏」句；零轴跳过倾向；无阵营空串。
func TestMoralDecisionContext(t *testing.T) {
	ctxLine := MoralDecisionContext(chaosUnit())
	if !strings.Contains(ctxLine, "混乱") {
		t.Fatalf("混乱阵营文案应含阵营名，得到 %q", ctxLine)
	}
	if !strings.Contains(ctxLine, "你认同") {
		t.Fatalf("应含阵营从属句，得到 %q", ctxLine)
	}
	if !strings.Contains(ctxLine, "心更偏") {
		t.Fatalf("有道德倾向应含倾向句，得到 %q", ctxLine)
	}
	// 有阵营但零道德轴 → 只有信条句、无倾向句。
	noLeaning := unit.Record{Faction: faction.IDOrder}
	line := MoralDecisionContext(noLeaning)
	if strings.Contains(line, "心更偏") {
		t.Fatalf("零道德轴不应有倾向句，得到 %q", line)
	}
	if !strings.Contains(line, "秩序") {
		t.Fatalf("有阵营应有信条句，得到 %q", line)
	}
	// 无阵营 + 零轴 → 空串。
	if line := MoralDecisionContext(unit.Record{}); line != "" {
		t.Fatalf("无阵营无倾向应空串，得到 %q", line)
	}
}

// TestPickMoralBiasedCandidate 验证道德契合 tie-break：混乱者在 {observe, attack} 间偏 attack；空候选/nil 安全；稳定平手。
func TestPickMoralBiasedCandidate(t *testing.T) {
	if _, ok := PickMoralBiasedCandidate(&unit.Record{}, nil); ok {
		t.Fatalf("空候选应返回 false")
	}
	candidates := []decisionCandidate{
		{Action: DecisionActionObserve}, // 中性 1.0
		{Action: DecisionActionAttack},  // assault → 混乱者放大
	}
	chaos := chaosUnit()
	got, ok := PickMoralBiasedCandidate(&chaos, candidates)
	if !ok || got.Action != DecisionActionAttack {
		t.Fatalf("混乱者应偏向 attack，得到 %v", got.Action)
	}
	// nil record → 取首个（向后兼容）。
	g2, _ := PickMoralBiasedCandidate(nil, candidates)
	if g2.Action != DecisionActionObserve {
		t.Fatalf("nil record 应取首个，得到 %v", g2.Action)
	}
	// 全中性候选 → 取首个（稳定）。
	neutral := []decisionCandidate{{Action: DecisionActionObserve}, {Action: DecisionActionEat}}
	g3, _ := PickMoralBiasedCandidate(&chaos, neutral)
	if g3.Action != DecisionActionObserve {
		t.Fatalf("全中性应取首个，得到 %v", g3.Action)
	}
}
