package villageseed

import "testing"

func TestDeterministicReproducible(t *testing.T) {
	a := Generate("world-1", 42)
	b := Generate("world-1", 42)
	if len(a.Members) != VillageSize || len(b.Members) != VillageSize {
		t.Fatalf("应有 %d 人，得到 %d/%d", VillageSize, len(a.Members), len(b.Members))
	}
	for i := range a.Members {
		if a.Members[i] != b.Members[i] {
			t.Fatalf("同 (worldID,seed) 第 %d 人应完全一致：%+v vs %+v", i, a.Members[i], b.Members[i])
		}
	}
	if len(a.Bonds) != len(b.Bonds) {
		t.Fatalf("关系网应一致，得到 %d vs %d", len(a.Bonds), len(b.Bonds))
	}
	for i := range a.Bonds {
		if a.Bonds[i] != b.Bonds[i] {
			t.Fatalf("第 %d 条关系应一致", i)
		}
	}
}

func TestDifferentSeedDiffers(t *testing.T) {
	a := Generate("world-1", 1)
	b := Generate("world-1", 2)
	same := 0
	for i := range a.Members {
		if a.Members[i].Name == b.Members[i].Name {
			same++
		}
	}
	if same == VillageSize {
		t.Fatalf("不同 seed 应生成不同的人，却全同名")
	}
	// 不同 worldID 也应不同。
	c := Generate("world-2", 1)
	if c.Members[0] == a.Members[0] {
		t.Fatalf("不同 worldID 应生成不同村庄")
	}
}

func TestWellFormed(t *testing.T) {
	v := Generate("w", 7)
	for _, m := range v.Members {
		if m.Name == "" || m.Archetype == "" || m.LifeGoal == "" || m.Secret == "" || m.SeedMemory == "" {
			t.Fatalf("成员字段不应为空：%+v", m)
		}
		for _, tr := range []float64{m.Traits.Courage, m.Traits.Loyalty, m.Traits.Aggression, m.Traits.Prudence, m.Traits.Sociability, m.Traits.Integrity, m.Traits.Stability, m.Traits.Ambition} {
			if tr < 0.05 || tr > 0.95 {
				t.Fatalf("人格值越界：%v in %+v", tr, m)
			}
		}
		if m.Age < 16 || m.Age > 50 {
			t.Fatalf("年龄越界：%d", m.Age)
		}
	}
	if len(v.Bonds) == 0 {
		t.Fatalf("应生成关系网")
	}
	for _, b := range v.Bonds {
		if b.From == b.To {
			t.Fatalf("不应有自环关系")
		}
		if b.From < 0 || b.From >= VillageSize || b.To < 0 || b.To >= VillageSize {
			t.Fatalf("关系索引越界：%+v", b)
		}
		for _, ax := range []float64{b.Trust, b.Fear, b.Affection, b.Rivalry} {
			if ax < -10 || ax > 10 {
				t.Fatalf("关系四轴越界：%v in %+v", ax, b)
			}
		}
	}
}
