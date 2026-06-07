package scheduler

import "testing"

func TestClassifyTier(t *testing.T) {
	cases := []struct {
		current, lastActive int64
		want                Tier
	}{
		{100, 100, TierHot},  // idle 0
		{100, 98, TierHot},   // idle 2 < 3
		{100, 97, TierWarm},  // idle 3
		{100, 80, TierWarm},  // idle 20 < 24
		{100, 76, TierCold},  // idle 24
		{100, 0, TierCold},   // idle 100
		{100, 110, TierHot},  // idle -10（时钟回拨）→ HOT 兜底
	}
	for _, c := range cases {
		if got := ClassifyTier(c.current, c.lastActive); got != c.want {
			t.Fatalf("ClassifyTier(%d,%d)=%s, want %s", c.current, c.lastActive, got, c.want)
		}
	}
}

func TestNextWakeTickStrictlyForward(t *testing.T) {
	for _, tier := range []Tier{TierHot, TierWarm, TierCold} {
		next := NextWakeTick(tier, 50)
		if next <= 50 {
			t.Fatalf("%s 下次唤醒应严格大于当前 tick，得到 %d", tier, next)
		}
	}
	if NextWakeTick(TierHot, 50) != 51 {
		t.Fatalf("HOT 应每 tick 唤醒（+1）")
	}
	if NextWakeTick(TierWarm, 50) != 54 {
		t.Fatalf("WARM 应 +4")
	}
	if NextWakeTick(TierCold, 50) != 66 {
		t.Fatalf("COLD 应 +16")
	}
}

func TestColderWakesLessOften(t *testing.T) {
	// 不变量：越冷的层唤醒间隔越大（成本越低）。
	if !(WakeInterval(TierHot) < WakeInterval(TierWarm) && WakeInterval(TierWarm) < WakeInterval(TierCold)) {
		t.Fatalf("唤醒间隔应随冷热单调：hot<warm<cold")
	}
}

func TestPlanNextWake(t *testing.T) {
	// 刚活跃过（idle 0）→ HOT → 下 tick 唤醒。
	tier, next := PlanNextWake(200, 200)
	if tier != TierHot || next != 201 {
		t.Fatalf("刚活跃应 HOT/+1，得到 %s/%d", tier, next)
	}
	// 久未活跃 → COLD → 远期唤醒。
	tier, next = PlanNextWake(200, 100)
	if tier != TierCold || next != 216 {
		t.Fatalf("久未活跃应 COLD/+16，得到 %s/%d", tier, next)
	}
}

func TestShouldWake(t *testing.T) {
	if !ShouldWake(10, 10) || !ShouldWake(10, 12) {
		t.Fatalf("到点/过点应唤醒")
	}
	if ShouldWake(10, 9) {
		t.Fatalf("未到点不应唤醒")
	}
}

func TestAdmitDecisionBackpressure(t *testing.T) {
	if !AdmitDecision(63, 64) {
		t.Fatalf("在途 63 < 64 应放行")
	}
	if AdmitDecision(64, 64) {
		t.Fatalf("在途达上限 64 应背压拒绝")
	}
	if AdmitDecision(70, 64) {
		t.Fatalf("超上限应拒绝")
	}
	// maxInFlight<=0 回退默认 64。
	if !AdmitDecision(63, 0) || AdmitDecision(64, 0) {
		t.Fatalf("maxInFlight<=0 应回退 DefaultMaxInFlight=64")
	}
}
