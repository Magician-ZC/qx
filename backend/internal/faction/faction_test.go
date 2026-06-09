package faction

// 文件说明：阵营定义与数值道德轴的纯逻辑单测——验证道德基准、出生据点确定性落点、argmax 主导阵营、归一化与扰动。

import "testing"

// TestAll_ThreeFactionsWithBaselines 验证三阵营齐全、各带正确道德基准与 3 个出生据点 + 非空信条。
func TestAll_ThreeFactionsWithBaselines(t *testing.T) {
	all := All()
	if len(all) != 3 {
		t.Fatalf("应有 3 个阵营，得到 %d", len(all))
	}
	wantBaseline := map[string]MoralAlignment{
		IDFreedom: {Freedom: 70, Order: 15, Chaos: 15},
		IDOrder:   {Order: 70, Freedom: 15, Chaos: 15},
		IDChaos:   {Chaos: 70, Freedom: 15, Order: 15},
	}
	for _, def := range all {
		want, ok := wantBaseline[def.ID]
		if !ok {
			t.Fatalf("未预期的阵营 ID：%q", def.ID)
		}
		if def.Baseline != want {
			t.Fatalf("阵营 %s 道德基准应为 %+v，得到 %+v", def.ID, want, def.Baseline)
		}
		if len(def.SpawnPoints) != 3 {
			t.Fatalf("阵营 %s 应有 3 个出生据点，得到 %d", def.ID, len(def.SpawnPoints))
		}
		if def.NameZH == "" || def.MoralCreed == "" {
			t.Fatalf("阵营 %s 中文名/信条不应为空：%+v", def.ID, def)
		}
		// DominantFaction(baseline) 应回指本阵营（基准的主导维即本阵营维）。
		if got := DominantFaction(def.Baseline); got != def.ID {
			t.Fatalf("阵营 %s 基准的主导阵营应为自身，得到 %q", def.ID, got)
		}
	}
}

// TestGetAndNormalize 验证按 ID 取定义与归一化（含中文别名、空白容错）。
func TestGetAndNormalize(t *testing.T) {
	if _, ok := Get(" Freedom "); !ok {
		t.Fatalf("带空白/大小写的 freedom 应可取到")
	}
	if Normalize("秩序") != IDOrder {
		t.Fatalf("中文别名「秩序」应归一为 order")
	}
	if Normalize("nonsense") != "" {
		t.Fatalf("未知阵营应归一为空串")
	}
	if IsValid("混乱") != true || IsValid("") != false {
		t.Fatalf("IsValid 判定异常")
	}
	if _, ok := Get("unknown"); ok {
		t.Fatalf("未知阵营 Get 应返回 false")
	}
}

// TestBaselineFor 验证道德基准查询（已知阵营返基准、未知返零值）。
func TestBaselineFor(t *testing.T) {
	if BaselineFor(IDFreedom) != (MoralAlignment{Freedom: 70, Order: 15, Chaos: 15}) {
		t.Fatalf("freedom 基准不符")
	}
	if !BaselineFor("unknown").IsZero() {
		t.Fatalf("未知阵营基准应为零值")
	}
}

// TestDominantFaction 验证 argmax 主导阵营（含全零、平手稳定裁定）。
func TestDominantFaction(t *testing.T) {
	if DominantFaction(MoralAlignment{}) != "" {
		t.Fatalf("全零道德轴主导阵营应为空")
	}
	if got := DominantFaction(MoralAlignment{Freedom: 10, Order: 80, Chaos: 5}); got != IDOrder {
		t.Fatalf("order 维最高应主导 order，得到 %q", got)
	}
	if got := DominantFaction(MoralAlignment{Freedom: 5, Order: 10, Chaos: 90}); got != IDChaos {
		t.Fatalf("chaos 维最高应主导 chaos，得到 %q", got)
	}
	// 平手：freedom>order>chaos 稳定顺序。
	if got := DominantFaction(MoralAlignment{Freedom: 50, Order: 50, Chaos: 10}); got != IDFreedom {
		t.Fatalf("freedom/order 平手应稳定裁定 freedom，得到 %q", got)
	}
}

// TestPickSpawnPoint_Deterministic 验证出生据点据 (faction, seed) 确定性落点、且落在该阵营据点集合内。
func TestPickSpawnPoint_Deterministic(t *testing.T) {
	for _, fid := range []string{IDFreedom, IDOrder, IDChaos} {
		def, _ := Get(fid)
		inSet := map[string]bool{}
		for _, sp := range def.SpawnPoints {
			inSet[sp] = true
		}
		// 确定性：同 (faction, seed) 反复调用一致。
		for _, seed := range []int64{1, 42, 1000, -7, 1 << 40} {
			first := PickSpawnPoint(fid, seed)
			if first == "" || !inSet[first] {
				t.Fatalf("阵营 %s seed=%d 应落在据点集合内，得到 %q", fid, seed, first)
			}
			for i := 0; i < 5; i++ {
				if again := PickSpawnPoint(fid, seed); again != first {
					t.Fatalf("阵营 %s seed=%d 落点应确定性一致：%q vs %q", fid, seed, first, again)
				}
			}
		}
		// 覆盖性：足够多 seed 应能落到至少 2 个不同据点（不退化为单点）。
		seen := map[string]bool{}
		for s := int64(0); s < 200; s++ {
			seen[PickSpawnPoint(fid, s)] = true
		}
		if len(seen) < 2 {
			t.Fatalf("阵营 %s 出生据点分布退化（只落 %d 个）", fid, len(seen))
		}
	}
	if PickSpawnPoint("unknown", 1) != "" {
		t.Fatalf("未知阵营应返回空据点")
	}
}

// TestFactionForSpawnPoint 验证据点反查阵营。
func TestFactionForSpawnPoint(t *testing.T) {
	if FactionForSpawnPoint("lawkeep_citadel") != IDOrder {
		t.Fatalf("lawkeep_citadel 应属 order")
	}
	if FactionForSpawnPoint("open_steppe") != IDFreedom {
		t.Fatalf("open_steppe 应属 freedom")
	}
	if FactionForSpawnPoint("nowhere") != "" {
		t.Fatalf("未知据点应返回空阵营")
	}
}

// TestPerturbBaseline 验证道德轴扰动：确定性、夹在 [0,100]、且≈baseline（在 maxDelta 内）。
func TestPerturbBaseline(t *testing.T) {
	const maxDelta = 5.0
	base := BaselineFor(IDFreedom)
	m1 := PerturbBaseline(IDFreedom, 99, "npc:3", maxDelta)
	m2 := PerturbBaseline(IDFreedom, 99, "npc:3", maxDelta)
	if m1 != m2 {
		t.Fatalf("同 (faction, seed, salt) 扰动应确定性一致：%+v vs %+v", m1, m2)
	}
	for _, v := range []float64{m1.Freedom, m1.Order, m1.Chaos} {
		if v < 0 || v > 100 {
			t.Fatalf("扰动后道德轴越界 [0,100]：%+v", m1)
		}
	}
	if absf(m1.Freedom-base.Freedom) > maxDelta+0.001 {
		t.Fatalf("扰动幅度应在 maxDelta 内，freedom 偏离 %.3f", absf(m1.Freedom-base.Freedom))
	}
	// maxDelta<=0 → 退化为纯基准（夹钳）。
	if got := PerturbBaseline(IDOrder, 1, "x", 0); got != base.Clamped() && got != BaselineFor(IDOrder) {
		t.Fatalf("maxDelta=0 应返回纯基准，得到 %+v", got)
	}
	// 不同 NPC（不同 salt）应有差异（个体差异非退化）。
	a := PerturbBaseline(IDChaos, 7, "npc:1", maxDelta)
	b := PerturbBaseline(IDChaos, 7, "npc:2", maxDelta)
	if a == b {
		t.Fatalf("不同 salt 的 NPC 道德轴不应全相同（退化）")
	}
}

// TestAlignmentDistance 验证道德轴欧氏距离非负、自身为 0。
func TestAlignmentDistance(t *testing.T) {
	m := MoralAlignment{Freedom: 70, Order: 15, Chaos: 15}
	if AlignmentDistance(m, m) != 0 {
		t.Fatalf("自身距离应为 0")
	}
	if d := AlignmentDistance(BaselineFor(IDFreedom), BaselineFor(IDOrder)); d <= 0 {
		t.Fatalf("不同阵营基准距离应为正，得到 %.3f", d)
	}
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
