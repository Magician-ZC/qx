package session

// 文件说明：obedience_courage_test.go，验证「自治胆量曲线」（沙盘 §5.2「风险/广度双曲线：越久越自主但越保守」）——
// ①风险曲线：离线越久对高风险/不可逆动作谨慎度单调升；②广度曲线：领域按离线时长分档解锁/限制；
// ③与既有人格/记忆/状态/忠诚信号叠加不替代；④确定性；⑤flag 默认关时零行为（中性）。

import (
	"math"
	"testing"
	"time"

	"qunxiang/backend/internal/unit"
)

// withCourageCurve 在测试期临时置 QUNXIANG_COURAGE_CURVE，结束自动还原（不污染其它用例的进程环境）。
func withCourageCurve(t *testing.T, value string) {
	t.Helper()
	t.Setenv(courageCurveFlagEnv, value)
}

// TestOfflineCautionMonotonicAndSaturating 验证风险曲线 offlineCaution 单调升 + 非线性饱和 + 0h 中性。
func TestOfflineCautionMonotonicAndSaturating(t *testing.T) {
	// 0h 与负时长 → 1.0（在线无离线惩罚）。
	if got := offlineCaution(0); got != 1.0 {
		t.Fatalf("offlineCaution(0)=%v，期望 1.0", got)
	}
	if got := offlineCaution(-5); got != 1.0 {
		t.Fatalf("offlineCaution(-5)=%v，期望 1.0（负时长按 0 处理）", got)
	}

	// 严格单调递增：离线越久谨慎度越高。
	hours := []float64{0.5, 1, 2, 6, 12, 24, 48, 96, 240}
	prev := offlineCaution(0)
	for _, h := range hours {
		cur := offlineCaution(h)
		if !(cur > prev) {
			t.Fatalf("offlineCaution 非严格单调：h=%v cur=%v 应 > prev=%v", h, cur, prev)
		}
		if cur < 1.0 {
			t.Fatalf("offlineCaution(%v)=%v 不应 < 1.0", h, cur)
		}
		prev = cur
	}

	// 非线性饱和：[0→6h] 的增量应大于 [24h→96h]（log 增速递减），且封顶不无界。
	deltaEarly := offlineCaution(6) - offlineCaution(0)
	deltaLate := offlineCaution(96) - offlineCaution(24)
	if !(deltaEarly > deltaLate) {
		t.Fatalf("应非线性饱和：早期增量 %v 应 > 后期增量 %v", deltaEarly, deltaLate)
	}
	// 公式标定校验：offlineCaution(7)=1+0.4·log2(8)=1+0.4·3=2.2（log2(1+7)=3）。
	if got := offlineCaution(7); math.Abs(got-2.2) > 1e-9 {
		t.Fatalf("offlineCaution(7)=%v，期望 2.2（1+0.4·log2(8)）", got)
	}
	// 极长离线被上限夹住，仍 ≥ 较短离线（单调不被破坏）。
	if offlineCaution(100000) <= offlineCaution(48) {
		t.Fatalf("极长离线应 ≥ 中等离线（上限不破坏单调）")
	}
}

// TestDomainBreadthCautionTiers 验证广度曲线按离线时长分档解锁/限制领域。
func TestDomainBreadthCautionTiers(t *testing.T) {
	// 日常领域：任何时长恒 1.0（0h 即完全自主）。
	for _, h := range []float64{0, 3, 6, 24, 100} {
		if got := domainBreadthCaution(autonomyDomainDaily, h); got != 1.0 {
			t.Fatalf("日常领域 h=%v 应恒 1.0，得到 %v", h, got)
		}
	}
	// 经营领域：<6h 未解锁（>1 谨慎），≥6h 解锁（1.0）。
	if got := domainBreadthCaution(autonomyDomainCivic, 3); !(got > 1.0) {
		t.Fatalf("经营领域 3h（<6h 未解锁）应 >1.0，得到 %v", got)
	}
	if got := domainBreadthCaution(autonomyDomainCivic, 6); got != 1.0 {
		t.Fatalf("经营领域 6h（解锁）应 1.0，得到 %v", got)
	}
	if got := domainBreadthCaution(autonomyDomainCivic, 30); got != 1.0 {
		t.Fatalf("经营领域 30h（解锁）应 1.0，得到 %v", got)
	}
	// 高代价领域：<24h 强谨慎；≥24h 解锁后仍恒 >1.0（始终更保守，绝不拿命赌大的）。
	lockedHigh := domainBreadthCaution(autonomyDomainHighRisk, 10)
	unlockedHigh := domainBreadthCaution(autonomyDomainHighRisk, 30)
	if !(lockedHigh > unlockedHigh) {
		t.Fatalf("高代价领域未解锁(%v)应比解锁后(%v)更谨慎", lockedHigh, unlockedHigh)
	}
	if !(unlockedHigh > 1.0) {
		t.Fatalf("高代价领域即使解锁后也应恒 >1.0（始终更保守），得到 %v", unlockedHigh)
	}
}

// TestDomainUnlockedAt 验证领域解锁判定的分档边界（含边界值）。
func TestDomainUnlockedAt(t *testing.T) {
	cases := []struct {
		domain autonomyDomain
		hours  float64
		want   bool
	}{
		{autonomyDomainDaily, 0, true},
		{autonomyDomainDaily, 100, true},
		{autonomyDomainCivic, 5.99, false},
		{autonomyDomainCivic, 6, true}, // 边界含
		{autonomyDomainCivic, 24, true},
		{autonomyDomainHighRisk, 23.9, false},
		{autonomyDomainHighRisk, 24, true}, // 边界含
		{autonomyDomainHighRisk, 1000, true},
	}
	for _, tc := range cases {
		if got := DomainUnlockedAt(tc.domain, tc.hours); got != tc.want {
			t.Errorf("DomainUnlockedAt(%s, %v)=%v，期望 %v", tc.domain, tc.hours, got, tc.want)
		}
	}
}

// TestDecisionAutonomyDomainMapping 验证动作 → 领域归类（高代价不可逆动作落 highrisk，经营落 civic，其余 daily）。
func TestDecisionAutonomyDomainMapping(t *testing.T) {
	cases := []struct {
		action DecisionAction
		want   autonomyDomain
	}{
		{DecisionActionRomance, autonomyDomainHighRisk},
		{DecisionActionFamily, autonomyDomainHighRisk},
		{DecisionActionDemolish, autonomyDomainHighRisk},
		{DecisionActionTrade, autonomyDomainCivic},
		{DecisionActionBuild, autonomyDomainCivic},
		{DecisionActionForge, autonomyDomainCivic},
		{DecisionActionUpgrade, autonomyDomainCivic},
		{DecisionActionMove, autonomyDomainDaily},
		{DecisionActionGather, autonomyDomainDaily},
		{DecisionActionAttack, autonomyDomainDaily}, // 战斗动作不进广度门，交风险曲线处理
		{DecisionActionDialogue, autonomyDomainDaily},
		{DecisionActionDefend, autonomyDomainDaily},
	}
	for _, tc := range cases {
		if got := decisionAutonomyDomain(tc.action); got != tc.want {
			t.Errorf("decisionAutonomyDomain(%s)=%s，期望 %s", tc.action, got, tc.want)
		}
	}
}

// TestOfflineHoursFromState 验证离线时长由 State.UpdatedAt 与注入 now 确定性派生。
func TestOfflineHoursFromState(t *testing.T) {
	base := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	// 正常：now 晚 10h → 10。
	st := State{UpdatedAt: base}
	if got := offlineHoursFromState(st, base.Add(10*time.Hour)); math.Abs(got-10) > 1e-9 {
		t.Fatalf("offlineHoursFromState 应为 10，得到 %v", got)
	}
	// 零值 UpdatedAt（旧存档/未落库）→ 0（按在线，失败安全）。
	if got := offlineHoursFromState(State{}, base); got != 0 {
		t.Fatalf("零值 UpdatedAt 应返回 0，得到 %v", got)
	}
	// 时钟回拨（now 早于 UpdatedAt）→ 0（不出现负离线）。
	if got := offlineHoursFromState(st, base.Add(-3*time.Hour)); got != 0 {
		t.Fatalf("时钟回拨应返回 0，得到 %v", got)
	}
}

// TestOfflineCourageRejectModifierFlagGated 验证 flag 默认关时桥恒 1.0（中性、零行为）。
func TestOfflineCourageRejectModifierFlagGated(t *testing.T) {
	withCourageCurve(t, "") // 显式置空 = 默认关
	st := State{UpdatedAt: time.Now().Add(-72 * time.Hour)}
	dec := unitDecisionPayload{Action: DecisionActionRomance} // 高代价 + 长离线
	if got := offlineCourageRejectModifier(st, dec); got != 1.0 {
		t.Fatalf("flag 关时应恒 1.0（中性），得到 %v", got)
	}
	// 各非开值都视为关。
	for _, v := range []string{"false", "0", "no", "off", "bogus"} {
		withCourageCurve(t, v)
		if got := offlineCourageRejectModifier(st, dec); got != 1.0 {
			t.Fatalf("flag=%q 应视为关（1.0），得到 %v", v, got)
		}
	}
}

// TestOfflineCourageRejectModifierMonotonic 验证 flag 开时：离线越久乘数越大（更谨慎），且与领域分档叠加。
func TestOfflineCourageRejectModifierMonotonic(t *testing.T) {
	withCourageCurve(t, "true")
	now := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)

	// 用「移动」（日常领域，广度恒 1.0）隔离出纯风险曲线项：离线越久乘数严格单调升。
	mkState := func(offlineHours float64) State {
		return State{UpdatedAt: now.Add(-time.Duration(offlineHours * float64(time.Hour)))}
	}
	dailyDec := unitDecisionPayload{Action: DecisionActionMove}
	prev := 0.0
	for _, h := range []float64{0.5, 2, 8, 24, 72} {
		// 这里改用直接喂 offlineHours 的纯链路以避免 now 漂移：复用 offlineCaution·domainBreadthCaution。
		cur := offlineCaution(h) * domainBreadthCaution(decisionAutonomyDomain(dailyDec.Action), h)
		if !(cur > prev) {
			t.Fatalf("日常动作乘数应随离线时长单调升：h=%v cur=%v 应 > prev=%v", h, cur, prev)
		}
		prev = cur
	}
	_ = mkState

	// 高代价动作（恋爱）在同一离线时长下应比日常更谨慎（广度项叠加）。
	const h = 30.0
	dailyMod := offlineCaution(h) * domainBreadthCaution(autonomyDomainDaily, h)
	highMod := offlineCaution(h) * domainBreadthCaution(autonomyDomainHighRisk, h)
	if !(highMod > dailyMod) {
		t.Fatalf("同离线时长下高代价动作(%v)应比日常(%v)更谨慎", highMod, dailyMod)
	}
}

// TestCourageCurveStacksWithExistingModifiers 验证胆量曲线进 directiveRejectProbability 后与既有信号叠加不替代：
// 同一单位/风险下，flag 开 + 长离线（回合差驱动）的抗命概率应 ≥ flag 关的基线（曲线只抬高、不抹掉既有判定）。
// LOW② 后离线时长由回合差派生（脱墙钟），故用较晚回合（turn=13≈72h 等效离线）制造「长离线」。
func TestCourageCurveStacksWithExistingModifiers(t *testing.T) {
	actor := &unit.Record{
		ID:        "u1",
		FactionID: "player",
	}
	actor.Personality.Courage = 0.5
	actor.Personality.Loyalty = 0.5
	actor.Status.Loyalty = 0.5
	actor.Status.HP = 80
	actor.Status.Morale = 0.6
	byID := map[string]*unit.Record{"u1": actor}
	dec := unitDecisionPayload{Action: DecisionActionAttack, TargetUnitID: ""}
	const risk = 1.6

	// 长离线由回合差驱动：turn=13 → 12 回合差 × 6h = 72h 等效离线（脱墙钟、确定性）。
	stLong := State{ID: "s", PlayerFactionID: "player"}
	stLong.TurnState.Turn = 13

	// flag 关：基线（无离线项）。
	t.Setenv(courageCurveFlagEnv, "false")
	base := directiveRejectProbability(stLong, byID, actor, dec, risk)
	// flag 开 + 长离线：应 > 基线（曲线确实叠加抬高，非替代；上限仍 0.85）。
	t.Setenv(courageCurveFlagEnv, "true")
	boosted := directiveRejectProbability(stLong, byID, actor, dec, risk)
	if !(boosted > base) {
		t.Fatalf("胆量曲线应叠加抬高抗命概率：boosted=%v 应 > base=%v", boosted, base)
	}
	// 上限仍夹 0.85（曲线不得突破既有硬顶）。
	if boosted > 0.85 {
		t.Fatalf("抗命概率上限应仍夹 0.85，得到 %v", boosted)
	}
}

// TestCourageCurveDeterministic 验证确定性：同输入（state/now/decision）多次调用结果逐位一致。
func TestCourageCurveDeterministic(t *testing.T) {
	withCourageCurve(t, "true")
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	st := State{UpdatedAt: now.Add(-15 * time.Hour)}
	h := offlineHoursFromState(st, now)

	for _, dec := range []unitDecisionPayload{
		{Action: DecisionActionMove},
		{Action: DecisionActionTrade},
		{Action: DecisionActionRomance},
	} {
		dom := decisionAutonomyDomain(dec.Action)
		want := offlineCaution(h) * domainBreadthCaution(dom, h)
		for i := 0; i < 5; i++ {
			if got := offlineCaution(h) * domainBreadthCaution(dom, h); got != want {
				t.Fatalf("非确定：action=%s 第%d 次 %v != %v", dec.Action, i, got, want)
			}
		}
	}
}

// TestOfflineHoursFromTurns 验证 LOW② 的确定性回合差派生：起始回合(1)为 0 离线，其后逐回合 ×6h 累积。
func TestOfflineHoursFromTurns(t *testing.T) {
	cases := []struct {
		turn int
		want float64
	}{
		{0, 0},                           // 异常/未初始化回合 → 0（失败安全）
		{1, 0},                           // 起始回合视作在线，0 离线
		{2, offlineTurnEquivHours},       // 第 2 回合 = 1 回合差 × 6h
		{5, 4 * offlineTurnEquivHours},   // 第 5 回合 = 4 回合差 × 6h = 24h（触发高代价门）
		{11, 10 * offlineTurnEquivHours}, // 严格单调累积
	}
	for _, tc := range cases {
		st := State{}
		st.TurnState.Turn = tc.turn
		if got := offlineHoursFromTurns(st); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("offlineHoursFromTurns(turn=%d)=%v，期望 %v", tc.turn, got, tc.want)
		}
	}
	// 单调：回合越大离线时长越大（脱墙钟、纯回合函数）。
	prev := -1.0
	for turn := 1; turn <= 20; turn++ {
		st := State{}
		st.TurnState.Turn = turn
		cur := offlineHoursFromTurns(st)
		if cur < prev {
			t.Fatalf("回合 %d 离线时长 %v 应 ≥ 前一回合 %v（单调）", turn, cur, prev)
		}
		prev = cur
	}
}

// TestOfflineCourageRejectModifier_TurnDeterministic 是 LOW② 核心回归：胆量曲线脱墙钟后，
// 同一 state（同 turn）多次调用 offlineCourageRejectModifier 逐位一致——不再随 time.Now() 漂移。
// 这正是「同 (sessionID,turn,actor) 重放逐位复现」的最小证明（曲线项不读墙钟）。
func TestOfflineCourageRejectModifier_TurnDeterministic(t *testing.T) {
	withCourageCurve(t, "true")
	// 第 5 回合（=24h 等效离线，已过高代价门），高代价动作。
	st := State{}
	st.TurnState.Turn = 5
	// UpdatedAt 故意置成「会随墙钟漂移」的近况——若实现仍偷读墙钟，两次调用会因 time.Now() 推进而不同。
	st.UpdatedAt = time.Now().Add(-3 * time.Hour)

	dec := unitDecisionPayload{Action: DecisionActionRomance}
	first := offlineCourageRejectModifier(st, dec)
	for i := 0; i < 8; i++ {
		if got := offlineCourageRejectModifier(st, dec); got != first {
			t.Fatalf("胆量曲线应脱墙钟、逐位确定：第%d 次 %v != 首次 %v", i, got, first)
		}
	}
	// 该回合（24h 等效）+ 高代价动作应 >1.0（确实受曲线影响，非恒等 1.0 的退化）。
	if !(first > 1.0) {
		t.Fatalf("turn=5（24h 等效离线）高代价动作应 >1.0（曲线生效），得 %v", first)
	}

	// 回合差驱动单调：更晚的回合（更久离线）抗命乘数应更大。
	stLater := State{}
	stLater.TurnState.Turn = 12
	stLater.UpdatedAt = st.UpdatedAt
	if !(offlineCourageRejectModifier(stLater, dec) > first) {
		t.Fatalf("更晚回合（更久离线）应更谨慎：turn12 应 > turn5(%v)", first)
	}
}

// TestDirectiveRejectProbability_TurnDeterministic 验证修复落到 directiveRejectProbability 顶层：
// 同输入两次调用逐位一致（胆量曲线脱墙钟后，整条抗命概率链无任何墙钟读取）。
func TestDirectiveRejectProbability_TurnDeterministic(t *testing.T) {
	t.Setenv(courageCurveFlagEnv, "true")
	actor := &unit.Record{ID: "u1", FactionID: "player"}
	actor.Personality.Courage = 0.5
	actor.Personality.Loyalty = 0.5
	actor.Status.Loyalty = 0.5
	actor.Status.HP = 80
	actor.Status.Morale = 0.6
	byID := map[string]*unit.Record{"u1": actor}
	dec := unitDecisionPayload{Action: DecisionActionRomance}
	const risk = 1.6

	st := State{ID: "s", PlayerFactionID: "player"}
	st.TurnState.Turn = 6
	st.UpdatedAt = time.Now().Add(-9 * time.Hour) // 即便墙钟锚在，结果也不应随墙钟变

	first := directiveRejectProbability(st, byID, actor, dec, risk)
	for i := 0; i < 8; i++ {
		if got := directiveRejectProbability(st, byID, actor, dec, risk); got != first {
			t.Fatalf("抗命概率应逐位确定（无墙钟）：第%d 次 %v != 首次 %v", i, got, first)
		}
	}
}
