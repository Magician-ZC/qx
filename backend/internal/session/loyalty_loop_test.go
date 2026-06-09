package session

// 文件说明：loyalty_loop_test.go，验证忠诚负反馈闭环的纯函数映射（设计 §5.7「越按越不听」）——
// 强令违心扣忠诚且重复累积、违心顺从/抗命轻微离心、顺其本心归心、无玩家指令零结算；全部确定性。

import (
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// newLoyaltyTestActor 构造忠诚闭环纯函数测试用的最小单位记录（仅需 ID/阵营/存活态）。
func newLoyaltyTestActor(id string) *unit.Record {
	return &unit.Record{
		ID:        id,
		FactionID: "player",
		Status:    unit.Status{LifeState: unit.LifeStateActive, Loyalty: 0.6},
	}
}

// TestLoyaltyDeltaForcedDefianceStrains 验证「被强令做违心事」扣忠诚且随重复累积加重。
func TestLoyaltyDeltaForcedDefianceStrains(t *testing.T) {
	actor := newLoyaltyTestActor("u1")
	state := &State{
		PlayerFactionID: "player",
		DirectiveHistory: []Directive{
			{Kind: DirectiveKindOrder, TargetUnitID: "u1", AppliesTo: "u1", Text: "强攻"},
		},
	}
	compliance := obedienceResolution{
		HasPlayerDirective:     true,
		ForcedByImmediateOrder: true,
		WouldDefyUnderForce:    true,
	}

	delta, code, text := loyaltyDeltaFromCompliance(state, actor, compliance)
	if delta >= 0 {
		t.Fatalf("强令违心应扣忠诚（负 delta），得到 %.4f", delta)
	}
	if code != events.ReasonCommandForced {
		t.Fatalf("强令违心应用 ReasonCommandForced，得到 %s", code)
	}
	if text == "" {
		t.Fatalf("强令违心应带旁白")
	}
	firstStrain := -delta

	// 越按越不听：再追加一道点名即时令，扣得更狠（重复累积）。
	state.DirectiveHistory = append(state.DirectiveHistory, Directive{
		Kind: DirectiveKindOrder, TargetUnitID: "u1", AppliesTo: "u1", Text: "再压上",
	})
	delta2, _, _ := loyaltyDeltaFromCompliance(state, actor, compliance)
	if -delta2 <= firstStrain {
		t.Fatalf("重复强令应离心更重：首次 %.4f，再次 %.4f", firstStrain, -delta2)
	}
}

// TestLoyaltyDeltaForcedAlignedNoChange 验证「强令做的是她本来也愿意做的事」不扣忠诚。
func TestLoyaltyDeltaForcedAlignedNoChange(t *testing.T) {
	actor := newLoyaltyTestActor("u1")
	state := &State{
		PlayerFactionID: "player",
		DirectiveHistory: []Directive{
			{Kind: DirectiveKindOrder, TargetUnitID: "u1", AppliesTo: "u1", Text: "强攻"},
		},
	}
	compliance := obedienceResolution{
		HasPlayerDirective:     true,
		ForcedByImmediateOrder: true,
		WouldDefyUnderForce:    false, // 本来也想做
	}
	delta, _, _ := loyaltyDeltaFromCompliance(state, actor, compliance)
	if delta != 0 {
		t.Fatalf("强令做她本来也愿意的事不应改忠诚，得到 %.4f", delta)
	}
}

// TestLoyaltyDeltaReluctantStrains 验证「高风险方针下违心顺从/抗命」轻微离心。
func TestLoyaltyDeltaReluctantStrains(t *testing.T) {
	actor := newLoyaltyTestActor("u1")
	state := &State{PlayerFactionID: "player"}
	for _, st := range []obedienceState{obedienceConcerned, obedienceReluctant, obedienceRefused} {
		compliance := obedienceResolution{HasPlayerDirective: true, State: st}
		delta, code, _ := loyaltyDeltaFromCompliance(state, actor, compliance)
		if delta >= 0 {
			t.Fatalf("违心顺从/抗命(%s)应扣忠诚，得到 %.4f", st, delta)
		}
		if code != events.ReasonLoyaltyStrain {
			t.Fatalf("违心顺从(%s)应用 ReasonLoyaltyStrain，得到 %s", st, code)
		}
	}
}

// TestLoyaltyDeltaAlignedGains 验证「顺其本心服从有分量的玩家命令」归心。
func TestLoyaltyDeltaAlignedGains(t *testing.T) {
	actor := newLoyaltyTestActor("u1")
	state := &State{PlayerFactionID: "player"}
	compliance := obedienceResolution{
		HasPlayerDirective: true,
		State:              obedienceSteady,
		RiskScore:          1.2, // 有分量的命令（非 0.1 兜底）
	}
	delta, code, _ := loyaltyDeltaFromCompliance(state, actor, compliance)
	if delta <= 0 {
		t.Fatalf("顺其本心应加忠诚（正 delta），得到 %.4f", delta)
	}
	if code != events.ReasonLoyaltyGain {
		t.Fatalf("顺其本心应用 ReasonLoyaltyGain，得到 %s", code)
	}
}

// TestLoyaltyDeltaSteadyLowRiskNoChange 验证「无分量命令的安静 steady tick」不白嫖归心。
func TestLoyaltyDeltaSteadyLowRiskNoChange(t *testing.T) {
	actor := newLoyaltyTestActor("u1")
	state := &State{PlayerFactionID: "player"}
	compliance := obedienceResolution{
		HasPlayerDirective: true,
		State:              obedienceSteady,
		RiskScore:          0.1, // directiveOrderRisk 的「无进攻意图」兜底
	}
	delta, _, _ := loyaltyDeltaFromCompliance(state, actor, compliance)
	if delta != 0 {
		t.Fatalf("无分量 steady tick 不应改忠诚，得到 %.4f", delta)
	}
}

// TestLoyaltyDeltaNoDirectiveNoChange 验证「无玩家指令管辖」时纯自主行为零结算。
func TestLoyaltyDeltaNoDirectiveNoChange(t *testing.T) {
	actor := newLoyaltyTestActor("u1")
	state := &State{PlayerFactionID: "player"}
	compliance := obedienceResolution{
		HasPlayerDirective: false,
		State:              obedienceReluctant, // 即便状态非 steady，无指令也不结算
		RiskScore:          1.2,
	}
	delta, _, _ := loyaltyDeltaFromCompliance(state, actor, compliance)
	if delta != 0 {
		t.Fatalf("无玩家指令应零结算，得到 %.4f", delta)
	}
}

// TestLoyaltyDeltaDeterministic 验证同输入重复调用结果完全一致（确定性）。
func TestLoyaltyDeltaDeterministic(t *testing.T) {
	actor := newLoyaltyTestActor("u1")
	state := &State{
		PlayerFactionID: "player",
		DirectiveHistory: []Directive{
			{Kind: DirectiveKindOrder, TargetUnitID: "u1", AppliesTo: "u1"},
			{Kind: DirectiveKindOrder, TargetUnitID: "u1", AppliesTo: "u1"},
		},
	}
	compliance := obedienceResolution{
		HasPlayerDirective:     true,
		ForcedByImmediateOrder: true,
		WouldDefyUnderForce:    true,
	}
	d1, c1, t1 := loyaltyDeltaFromCompliance(state, actor, compliance)
	d2, c2, t2 := loyaltyDeltaFromCompliance(state, actor, compliance)
	if d1 != d2 || c1 != c2 || t1 != t2 {
		t.Fatalf("loyaltyDeltaFromCompliance 非确定：(%.4f,%s,%q) vs (%.4f,%s,%q)", d1, c1, t1, d2, c2, t2)
	}
}

// TestCountForcedOrdersForUnit 验证点名即时令计数只数本单位的 order、不数 task/doctrine/他人。
func TestCountForcedOrdersForUnit(t *testing.T) {
	state := State{
		DirectiveHistory: []Directive{
			{Kind: DirectiveKindOrder, TargetUnitID: "u1", AppliesTo: "u1"},
			{Kind: DirectiveKindTask, TargetUnitID: "u1", AppliesTo: "u1"},       // 非 order
			{Kind: DirectiveKindOrder, TargetUnitID: "u2", AppliesTo: "u2"},      // 他人
			{Kind: DirectiveKindOrder, TargetUnitID: "u1", AppliesTo: "u1"},      // 本单位第二条
			{Kind: DirectiveKindDoctrine, TargetUnitID: "", AppliesTo: "player"}, // 非 order
		},
	}
	if got := countForcedOrdersForUnit(state, "u1"); got != 2 {
		t.Fatalf("u1 的即时令计数应为 2，得到 %d", got)
	}
	if got := countForcedOrdersForUnit(state, ""); got != 0 {
		t.Fatalf("空 unitID 应计 0，得到 %d", got)
	}
}
