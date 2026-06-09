package session

// 文件说明：阵营改换（F2 ③）的单元/集成测试。
// 守护：①隐藏条件——全满足才合格、缺一即不合格（亲和度差/连击/概率）；②flag 关恒不切（零行为）；
// ③概率确定性；④全链路切换落库 + FACTION_SWITCH 留痕 + 连击归零；⑤付费无关（不读 wallet）。
// test 文件被 statuslint 白名单豁免，可直接读写字段。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/unit"
)

// 构造一个「主导阵营明显背离当前阵营、亲和度差足够、连击足够」的道德轴（chaos 主导，当前 freedom）。
func defectingAlignment() faction.MoralAlignment {
	// chaos 80, freedom 50 → 差 30 ≥ margin 20。
	return faction.MoralAlignment{Freedom: 50, Order: 10, Chaos: 80}
}

// TestEvaluateFactionSwitch_AllConditions 验证三条隐藏条件全满足才合格。
func TestEvaluateFactionSwitch_AllConditions(t *testing.T) {
	m := defectingAlignment()
	// roll 过概率门（< switchProbability）。
	d := evaluateFactionSwitch(m, faction.IDFreedom, switchStreakTurns, switchProbability-0.01)
	if !d.Eligible {
		t.Fatalf("三条件全满足应合格：%+v", d)
	}
	if d.Dominant != faction.IDChaos {
		t.Fatalf("主导应为 chaos，得到 %s", d.Dominant)
	}
	if d.AffinityDelta < switchAffinityMargin {
		t.Fatalf("亲和度差应 ≥ margin，得到 %.1f", d.AffinityDelta)
	}
}

// TestEvaluateFactionSwitch_MissingEachCondition 验证缺任一条件即不合格。
func TestEvaluateFactionSwitch_MissingEachCondition(t *testing.T) {
	m := defectingAlignment()
	passRoll := switchProbability - 0.01

	// 缺①亲和度差：把 chaos 压到与 freedom 差 < margin。
	mNarrow := faction.MoralAlignment{Freedom: 50, Order: 10, Chaos: 55} // 差 5 < 20
	if d := evaluateFactionSwitch(mNarrow, faction.IDFreedom, switchStreakTurns, passRoll); d.Eligible {
		t.Fatalf("亲和度差不足应不合格：%+v", d)
	}

	// 缺②连击：streak < switchStreakTurns。
	if d := evaluateFactionSwitch(m, faction.IDFreedom, switchStreakTurns-1, passRoll); d.Eligible {
		t.Fatalf("连击不足应不合格：%+v", d)
	}

	// 缺③概率：roll ≥ switchProbability。
	if d := evaluateFactionSwitch(m, faction.IDFreedom, switchStreakTurns, switchProbability+0.01); d.Eligible {
		t.Fatalf("掷骰未过应不合格：%+v", d)
	}

	// 主导 == 当前（无背离）→ 不合格。
	mLoyal := faction.MoralAlignment{Freedom: 80, Order: 10, Chaos: 10}
	if d := evaluateFactionSwitch(mLoyal, faction.IDFreedom, 99, 0); d.Eligible {
		t.Fatalf("主导与当前一致应不合格：%+v", d)
	}

	// 无当前阵营 → 不合格。
	if d := evaluateFactionSwitch(m, "", switchStreakTurns, 0); d.Eligible {
		t.Fatalf("无当前阵营应不合格：%+v", d)
	}
}

// TestFactionSwitchRoll_Deterministic 验证概率掷骰确定性 + [0,1) 域 + actor 区分。
func TestFactionSwitchRoll_Deterministic(t *testing.T) {
	for turn := 0; turn < 50; turn++ {
		r := factionSwitchRoll("s", turn, "u")
		if r < 0 || r >= 1 {
			t.Fatalf("掷骰越域 %.4f", r)
		}
		if r != factionSwitchRoll("s", turn, "u") {
			t.Fatalf("掷骰非确定性 turn=%d", turn)
		}
	}
	same := 0
	for turn := 0; turn < 30; turn++ {
		if factionSwitchRoll("s", turn, "A") == factionSwitchRoll("s", turn, "B") {
			same++
		}
	}
	if same == 30 {
		t.Fatalf("不同 actor 掷骰全同，哈希未区分")
	}
}

// TestMaybeSwitchFaction_FlagOffNoSwitch 验证 flag 关时绝不切（即便条件全满足）——零行为。
func TestMaybeSwitchFaction_FlagOffNoSwitch(t *testing.T) {
	t.Setenv(factionSwitchFlagEnv, "") // 默认关
	db, repo, service := newDriftTestService(t)
	ctx := context.Background()

	rec := unit.BootstrapRecord(1, "s-off", "player", "阿一")
	rec.Faction = faction.IDFreedom
	rec.MoralAlignment = defectingAlignment()
	rec.MoralDriftStreak = switchStreakTurns + 5 // 连击远超阈
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	switched := service.maybeSwitchFaction(ctx, "s-off", &rec, 1)
	if switched {
		t.Fatalf("flag 关时不应切换")
	}
	if rec.Faction != faction.IDFreedom {
		t.Fatalf("flag 关阵营不应变，得到 %s", rec.Faction)
	}
	if n := reasonEventCount(t, db, rec.ID, events.ReasonFactionSwitch); n != 0 {
		t.Fatalf("flag 关不应留痕，得到 %d", n)
	}
}

// TestMaybeSwitchFaction_FlagOnSwitchesWhenEligible 验证 flag 开 + 条件满足 + 掷骰过 → 真的切 + 留痕 + 连击归零。
func TestMaybeSwitchFaction_FlagOnSwitchesWhenEligible(t *testing.T) {
	t.Setenv(factionSwitchFlagEnv, "true")
	db, repo, service := newDriftTestService(t)
	ctx := context.Background()

	// 找一个掷骰能过概率门的 (unit, turn)，以稳定触发切换（确定性扫描，非随机）。
	rec := unit.BootstrapRecord(2, "s-on", "player", "阿二")
	rec.Faction = faction.IDFreedom
	rec.MoralAlignment = defectingAlignment()
	rec.MoralDriftStreak = switchStreakTurns + 1
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	turn := -1
	for tt := 0; tt < 200; tt++ {
		if factionSwitchRoll("s-on", tt, rec.ID) < switchProbability {
			turn = tt
			break
		}
	}
	if turn < 0 {
		t.Fatalf("200 回合内未找到过概率门的 turn（概率门标定异常）")
	}

	switched := service.maybeSwitchFaction(ctx, "s-on", &rec, turn)
	if !switched {
		t.Fatalf("条件满足 + 掷骰过应切换（turn=%d roll=%.4f）", turn, factionSwitchRoll("s-on", turn, rec.ID))
	}
	if rec.Faction != faction.IDChaos {
		t.Fatalf("应切到主导阵营 chaos，得到 %s", rec.Faction)
	}
	if rec.MoralDriftStreak != 0 {
		t.Fatalf("切换后连击应归 0，得到 %d", rec.MoralDriftStreak)
	}
	// 落库一致。
	after, err := repo.GetByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("reget: %v", err)
	}
	if after.Faction != faction.IDChaos {
		t.Fatalf("落库阵营应为 chaos，得到 %s", after.Faction)
	}
	// FACTION_SWITCH 留痕。
	if n := reasonEventCount(t, db, rec.ID, events.ReasonFactionSwitch); n < 1 {
		t.Fatalf("应留 FACTION_SWITCH 事件，得到 %d", n)
	}
}

// TestMaybeSwitchFaction_FlagOnNotEligibleNoSwitch 验证 flag 开但条件不足（连击为 0）→ 不切。
func TestMaybeSwitchFaction_FlagOnNotEligibleNoSwitch(t *testing.T) {
	t.Setenv(factionSwitchFlagEnv, "true")
	_, repo, service := newDriftTestService(t)
	ctx := context.Background()

	rec := unit.BootstrapRecord(4, "s-ne", "player", "阿四")
	rec.Faction = faction.IDFreedom
	rec.MoralAlignment = defectingAlignment()
	rec.MoralDriftStreak = 0 // 连击不足 → 永不合格
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	for tt := 0; tt < 50; tt++ {
		if service.maybeSwitchFaction(ctx, "s-ne", &rec, tt) {
			t.Fatalf("连击为 0 不应切换（turn=%d）", tt)
		}
	}
	if rec.Faction != faction.IDFreedom {
		t.Fatalf("阵营不应变")
	}
}
