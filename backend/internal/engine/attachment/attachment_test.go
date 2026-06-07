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
