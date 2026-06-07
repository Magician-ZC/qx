package arbitration

// 文件说明：零和仲裁原语的单元测试。核心是证明三条公平性质：确定性、频率/顺序无关、胜率与 Score 成正比。
// 这正是设计方案 §11.3 用来根治 P2W 的机制保证——付费买不到「保证赢」，只能买更高的 Score。

import (
	"math"
	"testing"
)

func TestResolveDeterministic(t *testing.T) {
	c := Contest{Key: "rg1:t100:trade", Contestants: []Contestant{
		{UnitID: "a", Score: 5}, {UnitID: "b", Score: 8}, {UnitID: "c", Score: 3},
	}}
	o1 := Resolve(c)
	o2 := Resolve(c)
	if o1.WinnerID != o2.WinnerID || !eqStrings(o1.Ranking, o2.Ranking) {
		t.Fatalf("同一竞争应得到相同结果：%+v vs %+v", o1, o2)
	}
}

func TestResolveOrderIndependent(t *testing.T) {
	a := Contest{Key: "k", Contestants: []Contestant{{UnitID: "a", Score: 5}, {UnitID: "b", Score: 8}, {UnitID: "c", Score: 3}}}
	b := Contest{Key: "k", Contestants: []Contestant{{UnitID: "c", Score: 3}, {UnitID: "a", Score: 5}, {UnitID: "b", Score: 8}}}
	if Resolve(a).WinnerID != Resolve(b).WinnerID || !eqStrings(Resolve(a).Ranking, Resolve(b).Ranking) {
		t.Fatalf("结果不应依赖入队顺序")
	}
}

// 频率无关：同一参与者无论入队多少次（模拟「付费更高频/抢先排队」），结果不变。
func TestResolveFrequencyInvariant(t *testing.T) {
	base := Contest{Key: "k", Contestants: []Contestant{{UnitID: "a", Score: 5}, {UnitID: "b", Score: 8}}}
	// b 入队 10 次，模拟它行动频率是 a 的 10 倍。
	spam := Contest{Key: "k", Contestants: []Contestant{{UnitID: "a", Score: 5}}}
	for i := 0; i < 10; i++ {
		spam.Contestants = append(spam.Contestants, Contestant{UnitID: "b", Score: 8})
	}
	if Resolve(base).WinnerID != Resolve(spam).WinnerID || !eqStrings(Resolve(base).Ranking, Resolve(spam).Ranking) {
		t.Fatalf("重复入队(高频)不应改变结果——这正是反 P2W 的关键性质")
	}
}

// 胜率与 Score 成正比：70/30 的两名参与者，跨大量竞争事件 A 胜率应≈0.70。
func TestResolveScoreProportional(t *testing.T) {
	const n = 4000
	winsA := 0
	for i := 0; i < n; i++ {
		c := Contest{Key: "evt:" + itoa(i), Contestants: []Contestant{{UnitID: "a", Score: 70}, {UnitID: "b", Score: 30}}}
		if Resolve(c).WinnerID == "a" {
			winsA++
		}
	}
	rate := float64(winsA) / float64(n)
	if math.Abs(rate-0.70) > 0.04 {
		t.Fatalf("A 胜率应≈0.70(±0.04)，实际 %.3f —— 胜率必须与 Score 成正比，而非偏向任何一方", rate)
	}
}

// 极端 Score 差：正分对几乎零分，正分几乎必胜（但零分不是绝对零概率，体现仍是随机）。
func TestResolveDominantScore(t *testing.T) {
	const n = 2000
	winsA := 0
	for i := 0; i < n; i++ {
		c := Contest{Key: "d:" + itoa(i), Contestants: []Contestant{{UnitID: "a", Score: 100}, {UnitID: "b", Score: 0}}}
		if Resolve(c).WinnerID == "a" {
			winsA++
		}
	}
	if float64(winsA)/float64(n) < 0.99 {
		t.Fatalf("压倒性 Score 应近乎必胜，实际胜率 %.3f", float64(winsA)/float64(n))
	}
}

func TestResolveEdgeCases(t *testing.T) {
	if Resolve(Contest{Key: "k"}).WinnerID != "" {
		t.Fatalf("空竞争应返回空胜者")
	}
	one := Resolve(Contest{Key: "k", Contestants: []Contestant{{UnitID: "solo", Score: 1}}})
	if one.WinnerID != "solo" || len(one.Ranking) != 1 {
		t.Fatalf("单一参与者应必胜，得到 %+v", one)
	}
	// 空 UnitID 应被忽略。
	filtered := Resolve(Contest{Key: "k", Contestants: []Contestant{{UnitID: "", Score: 99}, {UnitID: "x", Score: 1}}})
	if filtered.WinnerID != "x" {
		t.Fatalf("空 UnitID 应被剔除，得到 %+v", filtered)
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
