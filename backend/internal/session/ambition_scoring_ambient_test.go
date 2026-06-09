package session

// 文件说明：野心打分「ambient 消费侧」测试（M7.3-real 动机栈消费）——验证 ActionAmbitionTagForAmbient 映射、
// AmbientBaseScore 的需求强度分档与边界夹紧、AmbitionActionWeight 的 flag 门控与确定性、付费不进（只读 record 不读 wallet）。

import (
	"testing"

	"qunxiang/backend/internal/unit"
)

// rrThresholds 复用 regionrunner 侧的动机栈阈值（与 regionrunner.forageThreshold/moraleLow/moraleHigh 对齐），
// 测试内以字面量传入，避免跨包依赖。
const (
	tForageThreshold = 40
	tMoraleLow       = 0.4
	tMoraleHigh      = 0.8
)

func TestActionAmbitionTagForAmbient(t *testing.T) {
	cases := map[string]string{
		"forage":    "hoard",
		"socialize": "bond",
		"reflect":   "explore",
		"rest":      "",        // 中性：不进表
		"FORAGE":    "hoard",   // 大小写不敏感
		"  reflect": "explore", // 首尾空白不敏感
		"unknown":   "",        // 未知 → 中性
		"":          "",        // 空 → 中性
	}
	for action, want := range cases {
		if got := ActionAmbitionTagForAmbient(action); got != want {
			t.Fatalf("ActionAmbitionTagForAmbient(%q)=%q, want %q", action, got, want)
		}
	}
}

func TestAmbientBaseScore_Bounds(t *testing.T) {
	// 无论何种输入，基础分恒落在 [0.5, 1.0]。
	for _, hunger := range []int{0, 39, 40, 60, 100} {
		for _, morale := range []float64{0.0, 0.3, 0.5, 0.8, 1.0} {
			var rec unit.Record
			rec.Status.Hunger = hunger
			rec.Status.Morale = morale
			for _, act := range []string{"forage", "rest", "socialize", "reflect"} {
				s := AmbientBaseScore(rec, act, tForageThreshold, tMoraleLow, tMoraleHigh)
				if s < ambientBaseFloor-1e-9 || s > ambientBaseCeil+1e-9 {
					t.Fatalf("AmbientBaseScore(h=%d,m=%.1f,%s)=%.3f 越界 [%.1f,%.1f]", hunger, morale, act, s, ambientBaseFloor, ambientBaseCeil)
				}
			}
		}
	}
}

func TestAmbientBaseScore_NeedStrength(t *testing.T) {
	// 已饿（hunger<阈值）→ forage 拿强分；不饿（远超阈值）→ forage 拿底分。
	var hungry unit.Record
	hungry.Status.Hunger = 20
	if s := AmbientBaseScore(hungry, "forage", tForageThreshold, tMoraleLow, tMoraleHigh); s != ambientNeedStrong {
		t.Fatalf("已饿 forage 应得强分 %.2f，得到 %.3f", ambientNeedStrong, s)
	}
	var full unit.Record
	full.Status.Hunger = 90
	if s := AmbientBaseScore(full, "forage", tForageThreshold, tMoraleLow, tMoraleHigh); s != ambientBaseFloor {
		t.Fatalf("饱食 forage 应得底分 %.2f，得到 %.3f", ambientBaseFloor, s)
	}
	// 已低落 → socialize 强分；心满意足 → reflect 强分。
	var low unit.Record
	low.Status.Morale = 0.2
	if s := AmbientBaseScore(low, "socialize", tForageThreshold, tMoraleLow, tMoraleHigh); s != ambientNeedStrong {
		t.Fatalf("低落 socialize 应得强分，得到 %.3f", s)
	}
	var high unit.Record
	high.Status.Morale = 0.95
	if s := AmbientBaseScore(high, "reflect", tForageThreshold, tMoraleLow, tMoraleHigh); s != ambientNeedStrong {
		t.Fatalf("满足 reflect 应得强分，得到 %.3f", s)
	}
	// rest 恒中性兜底。
	if s := AmbientBaseScore(full, "rest", tForageThreshold, tMoraleLow, tMoraleHigh); s != ambientNeutralBase {
		t.Fatalf("rest 应得中性兜底 %.2f，得到 %.3f", ambientNeutralBase, s)
	}
}

func TestAmbientBaseScore_Deterministic(t *testing.T) {
	// 同输入恒同输出（纯函数、零随机/时间）。
	var rec unit.Record
	rec.Status.Hunger = 55
	rec.Status.Morale = 0.6
	first := AmbientBaseScore(rec, "socialize", tForageThreshold, tMoraleLow, tMoraleHigh)
	for i := 0; i < 50; i++ {
		if s := AmbientBaseScore(rec, "socialize", tForageThreshold, tMoraleLow, tMoraleHigh); s != first {
			t.Fatalf("AmbientBaseScore 非确定性：第 %d 次 %.6f != %.6f", i, s, first)
		}
	}
}

func TestAmbitionActionWeight_FlagGating(t *testing.T) {
	var rec unit.Record
	rec.Ambition.Wealth = 1.0 // 强敛财野心 → hoard 标签引力应被放大（flag 开时）

	t.Setenv(ambitionScoringFlagEnv, "") // flag 关
	if w := AmbitionActionWeight(rec, "hoard"); w != ambitionFloor {
		t.Fatalf("flag 关时 AmbitionActionWeight 应恒中性 %.2f，得到 %.3f", ambitionFloor, w)
	}

	t.Setenv(ambitionScoringFlagEnv, "true") // flag 开
	w := AmbitionActionWeight(rec, "hoard")
	if w <= ambitionFloor {
		t.Fatalf("flag 开 + 强敛财野心，hoard 引力应 > %.2f，得到 %.3f", ambitionFloor, w)
	}
	if w > ambitionFloor+ambitionGain+1e-9 {
		t.Fatalf("引力应 ≤ %.2f，得到 %.3f", ambitionFloor+ambitionGain, w)
	}
	// 空标签（中性动作）恒中性，即便 flag 开。
	if w0 := AmbitionActionWeight(rec, ""); w0 != ambitionFloor {
		t.Fatalf("空标签应恒中性 %.2f，得到 %.3f", ambitionFloor, w0)
	}
}

// TestAmbientScoring_PayToWinFree 守反 P2W 红线：野心/基础分打分**绝不**读 wallet/billing——
// 同一野心档下，钱包从 0 改到极大值，打分逐位不变。
func TestAmbientScoring_PayToWinFree(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "true")
	mk := func(wallet int) unit.Record {
		var rec unit.Record
		rec.Ambition.Wealth = 0.8
		rec.Status.Hunger = 50
		rec.Status.Morale = 0.5
		rec.Status.Wallet = wallet
		return rec
	}
	poor, rich := mk(0), mk(999999)
	if AmbitionActionWeight(poor, "hoard") != AmbitionActionWeight(rich, "hoard") {
		t.Fatalf("野心乘权随钱包变化 → 违反反 P2W 红线")
	}
	if AmbientBaseScore(poor, "forage", tForageThreshold, tMoraleLow, tMoraleHigh) !=
		AmbientBaseScore(rich, "forage", tForageThreshold, tMoraleLow, tMoraleHigh) {
		t.Fatalf("基础分随钱包变化 → 违反反 P2W 红线")
	}
}
