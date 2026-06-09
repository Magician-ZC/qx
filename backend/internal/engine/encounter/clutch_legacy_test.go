package encounter

// 文件说明：关键救场（Clutch）进 Score + 传家物升级判定的测试。
// 覆盖：Clutch 确定性进 Score、付费不进、终结一击/濒死反杀判定、传家物升级弹卡门控。

import "testing"

// TestMarkClutch_EntersScore 验证关键救场经 MarkClutch 真正进入 ContributionScore（不再恒 0）。
func TestMarkClutch_EntersScore(t *testing.T) {
	base := Contribution{Damage: 10}
	baseScore := ContributionScore(base)

	// 终结一击 → Clutch 进分（权重 1.2）。
	withClutch := MarkClutch(base, ClutchFinalBlow)
	if !HadClutch(withClutch) {
		t.Fatalf("MarkClutch 后应判有关键救场")
	}
	got := ContributionScore(withClutch)
	want := baseScore + ContributionWeights.Clutch*ClutchValues.FinalBlow
	if got != want {
		t.Fatalf("关键救场未正确进 Score：得到 %f 期望 %f", got, want)
	}
	if got <= baseScore {
		t.Fatalf("关键救场应抬高贡献分（之前恒为 0 的缺口）")
	}
}

// TestMarkClutch_Stacks 多次救场应叠加（既终结一击又救队友）。
func TestMarkClutch_Stacks(t *testing.T) {
	c := Contribution{}
	c = MarkClutch(c, ClutchFinalBlow)
	c = MarkClutch(c, ClutchRescueDown)
	c = MarkClutch(c, ClutchNearDeathReversal)
	want := ClutchValues.FinalBlow + ClutchValues.RescueDown + ClutchValues.NearDeathReversal
	if c.Clutch != want {
		t.Fatalf("多次救场应叠加：得到 %f 期望 %f", c.Clutch, want)
	}
}

// TestMarkClutch_Deterministic 同样的事件序列必得同样的 Clutch（确定性、可复算）。
func TestMarkClutch_Deterministic(t *testing.T) {
	seq := []ClutchKind{ClutchFinalBlow, ClutchRescueDown, ClutchFinalBlow}
	a, b := Contribution{}, Contribution{}
	for _, k := range seq {
		a = MarkClutch(a, k)
		b = MarkClutch(b, k)
	}
	if a.Clutch != b.Clutch {
		t.Fatalf("Clutch 应确定性可复算：%f vs %f", a.Clutch, b.Clutch)
	}
}

// TestClutchValue_Unknown 未知类型安全降级为 0（不污染 Score）。
func TestClutchValue_Unknown(t *testing.T) {
	if ClutchValue(ClutchKind("nonsense")) != 0 {
		t.Fatalf("未知救场类型应折算 0")
	}
	// MarkClutch 未知类型应 no-op，不抬分。
	c := MarkClutch(Contribution{}, ClutchKind("nonsense"))
	if HadClutch(c) {
		t.Fatalf("未知救场类型不应被记为有救场")
	}
}

// TestClutch_PayBlind 付费不进 Score——Clutch 只由战斗事件累加，无任何钱包/计费入参。
// 用「相同事件 → 相同贡献分」反向证明：付费方无法靠花钱凭空抬高 Clutch（无入口）。
func TestClutch_PayBlind(t *testing.T) {
	// 两名角色：a 真打出 2 次救场，b（假想付费方）0 次救场但伤害相同。
	a := MarkClutch(MarkClutch(Contribution{Damage: 30}, ClutchFinalBlow), ClutchRescueDown)
	b := Contribution{Damage: 30}
	if ContributionScore(a) <= ContributionScore(b) {
		t.Fatalf("付费无法凭空获得 Clutch，唯有真救场抬分")
	}
	// b 即便重复调用「无事件」也不会涨分（无付费旁路）。
	if ContributionScore(b) != ContributionScore(Contribution{Damage: 30}) {
		t.Fatalf("无救场事件不应有任何 Clutch 加成")
	}
}

// TestIsFinalBlow 终结一击判定：把威胁从 >0 打到 ≤0 才算。
func TestIsFinalBlow(t *testing.T) {
	if !IsFinalBlow(5, 0) {
		t.Fatalf("打到 0 应是终结一击")
	}
	if !IsFinalBlow(3, -2) {
		t.Fatalf("打到负血应是终结一击")
	}
	if IsFinalBlow(10, 4) {
		t.Fatalf("还有血不算终结一击")
	}
	if IsFinalBlow(0, -3) {
		t.Fatalf("已经死了的再补刀不算终结一击")
	}
}

// TestIsNearDeathReversal 濒死反杀判定：自身血线已破撤退线却仍打出有效输出且存活。
func TestIsNearDeathReversal(t *testing.T) {
	if !IsNearDeathReversal(0.2, 0.25, 8, true) {
		t.Fatalf("血线 0.2<0.25、有输出、存活 → 应是濒死反杀")
	}
	if IsNearDeathReversal(0.5, 0.25, 8, true) {
		t.Fatalf("血线高于撤退线不算濒死反杀")
	}
	if IsNearDeathReversal(0.2, 0.25, 0, true) {
		t.Fatalf("没打出输出不算反杀（只是挨打）")
	}
	if IsNearDeathReversal(0.2, 0.25, 8, false) {
		t.Fatalf("当场倒下不算反杀")
	}
}

// TestQualifiesForLegacyUpgrade 传家物升级弹卡门控（设计 §5）。
func TestQualifiesForLegacyUpgrade(t *testing.T) {
	// Clutch 触发：有持有者、未升级、羁绊够、至少 1 次救场 → 弹卡。
	if !QualifiesForLegacyUpgrade(LegacyUpgradeQuery{Trigger: TriggerClutch, HasOwner: true, BondTurns: MinLegacyBondTurns, ClutchCount: 1}) {
		t.Fatalf("满足条件的 Clutch 应触发升级弹卡")
	}
	// 命运节点触发：本身够格，仍要求最小羁绊。
	if !QualifiesForLegacyUpgrade(LegacyUpgradeQuery{Trigger: TriggerFateNode, HasOwner: true, BondTurns: MinLegacyBondTurns}) {
		t.Fatalf("跨命运节点应触发升级弹卡")
	}
	// 羁绊不足（萍水相逢）→ 不弹卡。
	if QualifiesForLegacyUpgrade(LegacyUpgradeQuery{Trigger: TriggerClutch, HasOwner: true, BondTurns: MinLegacyBondTurns - 1, ClutchCount: 3}) {
		t.Fatalf("羁绊不足不应升级（防一捡到手就弹）")
	}
	// 已是传家物 → 幂等不弹。
	if QualifiesForLegacyUpgrade(LegacyUpgradeQuery{Trigger: TriggerClutch, AlreadyLegacy: true, HasOwner: true, BondTurns: 99, ClutchCount: 9}) {
		t.Fatalf("已是传家物不应重复弹卡")
	}
	// 无持有者 → 不弹。
	if QualifiesForLegacyUpgrade(LegacyUpgradeQuery{Trigger: TriggerFateNode, HasOwner: false, BondTurns: 99}) {
		t.Fatalf("无主装备不进升级流")
	}
	// Clutch 触发但 0 次救场 → 不弹。
	if QualifiesForLegacyUpgrade(LegacyUpgradeQuery{Trigger: TriggerClutch, HasOwner: true, BondTurns: 99, ClutchCount: 0}) {
		t.Fatalf("Clutch 触发须至少 1 次救场")
	}
	// 未知触发 → 不弹。
	if QualifiesForLegacyUpgrade(LegacyUpgradeQuery{Trigger: LegacyUpgradeTrigger("???"), HasOwner: true, BondTurns: 99, ClutchCount: 9}) {
		t.Fatalf("未知触发类型不应升级")
	}
}
