package relevance

// 文件说明：相关性/传播原语的单元测试——时间衰减、跳数衰减、评分聚合、阈值、传播停止、同意档位、撮合分。

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestTimeDecay(t *testing.T) {
	if !approx(TimeDecay(0, 14), 1) {
		t.Fatalf("age=0 应不衰减")
	}
	if !approx(TimeDecay(14, 14), 0.5) {
		t.Fatalf("age=半衰期应=0.5，得到 %f", TimeDecay(14, 14))
	}
	if !approx(TimeDecay(28, 14), 0.25) {
		t.Fatalf("age=2×半衰期应=0.25，得到 %f", TimeDecay(28, 14))
	}
	if !approx(TimeDecay(100, 0), 1) {
		t.Fatalf("半衰期≤0(红线/传承)应不衰减")
	}
}

func TestHopFidelity(t *testing.T) {
	if !approx(HopFidelity(0), 1) {
		t.Fatalf("直击应可信度 1")
	}
	if !approx(HopFidelity(1), 0.6) {
		t.Fatalf("1 跳应 0.6，得到 %f", HopFidelity(1))
	}
	if !approx(HopFidelity(2), 0.36) {
		t.Fatalf("2 跳应 0.36，得到 %f", HopFidelity(2))
	}
}

func TestScoreAndGate(t *testing.T) {
	// 单根强关系锚直击：0.78 · 0.32 · 1 · 1 = 0.2496（不足以单独过阈）。
	rel := Hit{Anchor: Anchor{Kind: Relation, Ref: "u_wu", Weight: 0.78, HalfLifeDays: 14}, AgeDays: 0}
	s1 := Score([]Hit{rel}, 1.0)
	if !approx(s1, 0.78*0.32) {
		t.Fatalf("单关系锚评分错误：%f", s1)
	}
	if PassesGate(s1) {
		t.Fatalf("单根关系锚(0.25)不应过 0.35 阈")
	}
	// 多锚共振：关系 + 目标 + 红线 叠加越过阈值。
	goal := Hit{Anchor: Anchor{Kind: Goal, Ref: "g1", Weight: 0.6}, AgeDays: 0}
	redline := Hit{Anchor: Anchor{Kind: Redline, Ref: "rl3", Weight: 1.0, HalfLifeDays: 0}, AgeDays: 999}
	s2 := Score([]Hit{rel, goal, redline}, 1.0)
	want := 0.78*0.32 + 0.6*0.18 + 1.0*0.28 // 红线半衰期0→不衰减
	if !approx(s2, want) {
		t.Fatalf("多锚评分错误：得到 %f 期望 %f", s2, want)
	}
	if !PassesGate(s2) {
		t.Fatalf("多锚共振应过阈，得到 %f", s2)
	}
	// 传播衰减：同样的命中经 2 跳，相关性按 hopFidelity 缩水。
	if !approx(Score([]Hit{rel}, HopFidelity(2)), 0.78*0.32*0.36) {
		t.Fatalf("传播衰减后评分错误")
	}
}

func TestStopPropagation(t *testing.T) {
	if !StopPropagation(2, 0.9, 0.9) {
		t.Fatalf("达到最大跳数应停止")
	}
	if !StopPropagation(1, 0.10, 0.9) {
		t.Fatalf("传递重要度低于地板应停止")
	}
	if !StopPropagation(1, 0.9, 0.20) {
		t.Fatalf("可信度低于地板应停止")
	}
	if StopPropagation(1, 0.5, 0.5) {
		t.Fatalf("各项均在阈内不应停止")
	}
}

func TestConsentTierFor(t *testing.T) {
	if ConsentTierFor(1) != Unilateral {
		t.Fatalf("层1应 unilateral")
	}
	if ConsentTierFor(2) != Contested {
		t.Fatalf("层2应 contested")
	}
	if ConsentTierFor(3) != RequiresConsent {
		t.Fatalf("层3应 requires_consent")
	}
}

func TestMatchScore(t *testing.T) {
	if !approx(MatchScore(1, 1, 1, 1), 1.0) {
		t.Fatalf("四因子满分应=1.0，得到 %f", MatchScore(1, 1, 1, 1))
	}
	// 地理近+钩子契合，无关系交集、低密度调节。
	got := MatchScore(1, 1, 0, 0)
	if !approx(got, 0.65) {
		t.Fatalf("地理+钩子应=0.65，得到 %f", got)
	}
	if !PassesMatch(got) {
		t.Fatalf("0.65 应过撮合阈 0.45")
	}
	if PassesMatch(MatchScore(0.5, 0, 0, 0)) {
		t.Fatalf("仅微弱地理近(0.175)不应过阈")
	}
	// 入参越界被夹紧。
	if !approx(MatchScore(2, -1, 0, 0), 0.35) {
		t.Fatalf("越界入参应夹紧到[0,1]，得到 %f", MatchScore(2, -1, 0, 0))
	}
}
