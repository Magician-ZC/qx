package relevance

// 文件说明：相关性/传播原语的单元测试——时间衰减、跳数衰减、评分聚合、阈值、传播停止、同意档位、撮合分。

import (
	"math"
	"math/rand"
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

func TestRelativeImportance(t *testing.T) {
	// 最重要类别(relation)归一化为 1.0，其余按比例。
	if !approx(RelativeImportance(Relation), 1.0) {
		t.Fatalf("relation 相对重要度应=1.0，得到 %f", RelativeImportance(Relation))
	}
	if !approx(RelativeImportance(Redline), 0.28/0.32) {
		t.Fatalf("redline 相对重要度错误：%f", RelativeImportance(Redline))
	}
	if RelativeImportance(Geo) >= RelativeImportance(Goal) {
		t.Fatalf("相对排序应保留：geo < goal")
	}
}

func TestScoreNoisyOR(t *testing.T) {
	// 修缺陷后：单根强关系锚(0.78)即可过阈——relevance = 0.78·1.0 = 0.78。
	rel := Hit{Anchor: Anchor{Kind: Relation, Ref: "u_wu", Weight: 0.78, HalfLifeDays: 14}, AgeDays: 0}
	s1 := Score([]Hit{rel}, 1.0)
	if !approx(s1, 0.78) {
		t.Fatalf("单关系锚评分应=0.78，得到 %f", s1)
	}
	if !PassesGate(s1) {
		t.Fatalf("强单锚(0.78)现在应过阈——这是修复的缺陷")
	}
	// 弱单锚仍过不了。
	weak := Hit{Anchor: Anchor{Kind: Relation, Weight: 0.2}, AgeDays: 0}
	if PassesGate(Score([]Hit{weak}, 1.0)) {
		t.Fatalf("弱锚(0.2)不应过阈")
	}
	// 多锚 noisy-OR：c_rel=0.78, c_goal=0.6·0.5625=0.3375, c_redline=1·0.875=0.875。
	goal := Hit{Anchor: Anchor{Kind: Goal, Weight: 0.6}, AgeDays: 0}
	redline := Hit{Anchor: Anchor{Kind: Redline, Weight: 1.0, HalfLifeDays: 0}, AgeDays: 999}
	want := 1 - (1-0.78)*(1-0.6*(0.18/0.32))*(1-1.0*(0.28/0.32))
	if !approx(Score([]Hit{rel, goal, redline}, 1.0), want) {
		t.Fatalf("多锚 noisy-OR 错误：得到 %f 期望 %f", Score([]Hit{rel, goal, redline}, 1.0), want)
	}
	// 两个中等信号共振过阈：c=0.25 each → 1-0.75² = 0.4375。
	g1 := Hit{Anchor: Anchor{Kind: Geo, Weight: 1.0, HalfLifeDays: 0}}             // 1·0.25=0.25
	d1 := Hit{Anchor: Anchor{Kind: DebtGrudgeLove, Weight: 0.25 / (0.14 / 0.32)}} // 调成 c=0.25
	r := Score([]Hit{g1, d1}, 1.0)
	if !PassesGate(r) {
		t.Fatalf("两个中等信号共振应过阈，得到 %f", r)
	}
	// 传播衰减：2 跳后强关系锚降到阈下（二手传闻被过滤）。
	if PassesGate(Score([]Hit{rel}, HopFidelity(2))) {
		t.Fatalf("密友消息经 2 跳传闻(0.78·0.36=0.28)应被过滤")
	}
}

// 单根强锚必须过阈——这正是修复的标定缺陷的回归守卫。
func TestSingleStrongAnchorPasses(t *testing.T) {
	for _, k := range []AnchorKind{Relation, Redline} {
		hl := 14.0
		if k == Redline {
			hl = 0
		}
		s := Score([]Hit{{Anchor: Anchor{Kind: k, Weight: 0.8, HalfLifeDays: hl}}}, 1.0)
		if !PassesGate(s) {
			t.Fatalf("%s 强锚(0.8)应过阈，得到 %f", k, s)
		}
	}
}

// 蒙特卡洛：随机事件群的相关性恒在 [0,1]，且过阈率落在合理标定带（非全死/全过）。
func TestRelevanceDistribution(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	kinds := []AnchorKind{Relation, Redline, Goal, DebtGrudgeLove, Geo, Legacy}
	const n = 30000
	passed := 0
	for i := 0; i < n; i++ {
		hits := randomHits(rng, kinds)
		r := Score(hits, HopFidelity(rng.Intn(3)))
		if r < 0 || r > 1 {
			t.Fatalf("相关性越界 [0,1]：%f", r)
		}
		if PassesGate(r) {
			passed++
		}
	}
	rate := float64(passed) / n
	if rate < 0.05 || rate > 0.85 {
		t.Fatalf("过阈率 %.3f 超出合理标定带 [0.05,0.85]（gate 过死或过松）", rate)
	}
}

// 蒙特卡洛：相关性对任一锚 weight 单调不减。
func TestRelevanceMonotonic(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	kinds := []AnchorKind{Relation, Redline, Goal, DebtGrudgeLove, Geo, Legacy}
	for i := 0; i < 10000; i++ {
		hits := randomHits(rng, kinds)
		base := Score(hits, 1.0)
		bumped := make([]Hit, len(hits))
		copy(bumped, hits)
		j := rng.Intn(len(bumped))
		bumped[j].Anchor.Weight = math.Min(1, bumped[j].Anchor.Weight+0.1)
		if got := Score(bumped, 1.0); got < base-1e-9 {
			t.Fatalf("单调性被破坏：base=%f bumped=%f", base, got)
		}
	}
}

func randomHits(rng *rand.Rand, kinds []AnchorKind) []Hit {
	n := 1 + rng.Intn(3)
	hits := make([]Hit, n)
	for j := range hits {
		k := kinds[rng.Intn(len(kinds))]
		hl := 14.0
		if k == Redline || k == Legacy {
			hl = 0
		}
		hits[j] = Hit{Anchor: Anchor{Kind: k, Weight: rng.Float64(), HalfLifeDays: hl}, AgeDays: rng.Float64() * 20}
	}
	return hits
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

func TestRouteFor(t *testing.T) {
	if RouteFor(0.1) != RouteAutonomous {
		t.Fatalf("低分应自治不打扰")
	}
	if RouteFor(0.4) != RouteHighlight {
		t.Fatalf("中分应进高光卡")
	}
	if RouteFor(0.7) != RoutePending {
		t.Fatalf("高分应升级待决策")
	}
	// 边界。
	if RouteFor(FatePendingGate) != RoutePending || RouteFor(FateHighlightGate) != RouteHighlight {
		t.Fatalf("阈值边界路由错误")
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
