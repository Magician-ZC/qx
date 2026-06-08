package attachment

import "testing"

func TestBounds(t *testing.T) {
	zero := Compute(Inputs{})
	full := Compute(Inputs{Resonance: 1, DaysAlive: 60, ReturnVisits: 30, CoCreations: 30})
	if zero < 0 || zero > 100 || full < 0 || full > 100 {
		t.Fatalf("应落在 [0,100]：zero=%v full=%v", zero, full)
	}
	if zero >= 25 {
		t.Fatalf("零输入应是低牵挂，得到 %v", zero)
	}
	if full <= 80 {
		t.Fatalf("满输入应接近 100，得到 %v", full)
	}
	if full <= zero {
		t.Fatalf("满输入应远高于零输入")
	}
}

func TestMonotonicInEachInput(t *testing.T) {
	base := Inputs{Resonance: 0.4, DaysAlive: 3, ReturnVisits: 1, CoCreations: 1}
	b := Compute(base)

	up := base
	up.Resonance = 0.9
	if Compute(up) <= b {
		t.Fatalf("共鸣↑应使牵挂↑")
	}
	up = base
	up.DaysAlive = 30
	if Compute(up) <= b {
		t.Fatalf("在世↑应使牵挂↑")
	}
	up = base
	up.ReturnVisits = 10
	if Compute(up) <= b {
		t.Fatalf("回访↑应使牵挂↑")
	}
	up = base
	up.CoCreations = 10
	if Compute(up) <= b {
		t.Fatalf("共创↑应使牵挂↑")
	}
}

// TestReturnVisitsMonotone 专测「回访越多牵挂越深」维度：固定其余三项，沿回访次数阶梯递增，
// 牵挂应严格单调不减（且 0→1 起步即抬升）；默认值 0 应等价于「该维度无贡献」。
func TestReturnVisitsMonotone(t *testing.T) {
	base := Inputs{Resonance: 0.5, DaysAlive: 5, CoCreations: 2}

	prev := -1.0
	for _, v := range []int{0, 1, 2, 3, 5, 8, 13, 30} {
		in := base
		in.ReturnVisits = v
		got := Compute(in)
		if got < prev {
			t.Fatalf("回访 %d 次牵挂 %v 低于上一档 %v，应单调不减", v, got, prev)
		}
		prev = got
	}

	// 0→1 次回访就应当带来可见抬升（该维度真实生效，而非被忽略）。
	atZero := Compute(base) // base.ReturnVisits 零值 = DefaultReturnVisits
	one := base
	one.ReturnVisits = 1
	if Compute(one) <= atZero {
		t.Fatalf("首次回访应抬升牵挂：0次=%v 1次=%v", atZero, Compute(one))
	}

	// DefaultReturnVisits 与显式传 0 同义，且与结构体零值一致。
	if Compute(base) != ComputeWithSignals(base.Resonance, base.DaysAlive, DefaultReturnVisits, base.CoCreations) {
		t.Fatalf("DefaultReturnVisits 应等价于该维度无贡献")
	}
}

// TestComputeWithSignalsEquivalence 保证位置参数入口与结构体入口数值完全一致。
func TestComputeWithSignalsEquivalence(t *testing.T) {
	cases := []Inputs{
		{},
		{Resonance: 0.5, DaysAlive: 5, ReturnVisits: 3, CoCreations: 2},
		{Resonance: 1, DaysAlive: 60, ReturnVisits: 30, CoCreations: 30},
		{Resonance: 0.2, DaysAlive: 0, ReturnVisits: 7, CoCreations: 0},
	}
	for _, c := range cases {
		want := Compute(c)
		got := ComputeWithSignals(c.Resonance, c.DaysAlive, c.ReturnVisits, c.CoCreations)
		if got != want {
			t.Fatalf("ComputeWithSignals 应与 Compute 一致：in=%+v want=%v got=%v", c, want, got)
		}
	}
}

func TestNonPurchasableShape(t *testing.T) {
	// 不可付费购买：函数签名里没有任何金钱/付费输入，牵挂只能靠互动养成。
	// 这里用「纯靠在世（挂机）刷不出高牵挂」来体现：只攒在世、零互动，牵挂上不去。
	idleLong := Compute(Inputs{DaysAlive: 365})
	if idleLong >= 60 {
		t.Fatalf("纯挂机（只在世、零互动）不应攒出高牵挂，得到 %v", idleLong)
	}
	engaged := Compute(Inputs{Resonance: 0.8, DaysAlive: 14, ReturnVisits: 6, CoCreations: 6})
	if engaged <= idleLong {
		t.Fatalf("真互动养成应高于纯挂机：engaged=%v idle=%v", engaged, idleLong)
	}
}
