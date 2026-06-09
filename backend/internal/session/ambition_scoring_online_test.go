package session

// 文件说明：野心打分「在线决策消费侧」测试——验证 OnlineActionAmbitionTag 的在线 DecisionAction→标签映射、
// OnlineAmbitionActionWeight 的 flag 门控/确定性/各维度→标签引力/付费不进、PickAmbitionBiasedCandidate 的
// 「flag 关不重排（取首个）+ flag 开按野心引力挑最贴者 + 平手保留靠前者」确定性 tie-break。这条链补上此前
// 在线决策规则 fallback 零野心打分调用方的缺口（野心在线只进 prompt、不进确定性候选排序）。

import (
	"testing"

	"qunxiang/backend/internal/unit"
)

// TestOnlineActionAmbitionTag 校验在线 DecisionAction → 野心标签映射：带野心语义的进表、战术/生存/中性动作返回 ""。
func TestOnlineActionAmbitionTag(t *testing.T) {
	cases := map[DecisionAction]string{
		// conquer 攻伐扩张
		DecisionActionAttack:      "conquer",
		DecisionActionCharge:      "conquer",
		DecisionActionHeavyAttack: "conquer",
		// hoard 经商逐利
		DecisionActionTrade: "hoard",
		// train 精进钻研
		DecisionActionGather:  "train",
		DecisionActionForge:   "train",
		DecisionActionUpgrade: "train",
		DecisionActionEquip:   "train",
		DecisionActionBuild:   "train",
		// nurture 养育/成家
		DecisionActionRomance: "nurture",
		DecisionActionFamily:  "nurture",
		// bond 社交结盟
		DecisionActionSay:      "bond",
		DecisionActionDialogue: "bond",
		// 中性/战术/生存动作 → 不进表 → ""（绝不让野心污染战术/生存决策）
		DecisionActionHold:     "",
		DecisionActionObserve:  "",
		DecisionActionDefend:   "",
		DecisionActionMove:     "",
		DecisionActionEat:      "",
		DecisionActionPickup:   "",
		DecisionActionSkill:    "",
		DecisionActionAssist:   "",
		DecisionActionDemolish: "",
	}
	for action, want := range cases {
		if got := OnlineActionAmbitionTag(action); got != want {
			t.Fatalf("OnlineActionAmbitionTag(%q)=%q, want %q", action, got, want)
		}
	}
	// 未知动作 → 中性 ""（失败安全）。
	if got := OnlineActionAmbitionTag(DecisionAction("totally_unknown_action")); got != "" {
		t.Fatalf("未知动作应得中性 \"\"，得到 %q", got)
	}
}

// TestOnlineAmbitionActionWeight_FlagGating 校验 flag 门控：默认关恒 1.0、开时强野心放大、空标签恒中性、上界不越界。
func TestOnlineAmbitionActionWeight_FlagGating(t *testing.T) {
	var rec unit.Record
	rec.Ambition.Vengeance = 1.0 // 强复仇野心 → conquer/revenge 标签引力应被放大（flag 开时）

	t.Setenv(ambitionScoringFlagEnv, "") // flag 关 → 恒中性
	if w := OnlineAmbitionActionWeight(rec, "conquer"); w != ambitionFloor {
		t.Fatalf("flag 关时 OnlineAmbitionActionWeight 应恒中性 %.2f，得到 %.3f", ambitionFloor, w)
	}

	t.Setenv(ambitionScoringFlagEnv, "true") // flag 开
	w := OnlineAmbitionActionWeight(rec, "conquer")
	if w <= ambitionFloor {
		t.Fatalf("flag 开 + 强复仇野心，conquer 引力应 > %.2f，得到 %.3f", ambitionFloor, w)
	}
	if w > ambitionFloor+ambitionGain+1e-9 {
		t.Fatalf("引力应 ≤ %.2f，得到 %.3f", ambitionFloor+ambitionGain, w)
	}
	// 空标签（中性动作）恒中性，即便 flag 开。
	if w0 := OnlineAmbitionActionWeight(rec, ""); w0 != ambitionFloor {
		t.Fatalf("空标签应恒中性 %.2f，得到 %.3f", ambitionFloor, w0)
	}
}

// TestOnlineAmbitionActionWeight_DimensionToTag 逐维度校验：每一维野心拉满，对应行为标签引力应被放大到 floor+gain，
// 不相关标签恒中性。验证「六维野心 → 在线动作标签」的语义映射端到端正确（与 ambition.go 的 ambitionDimensionTags 对齐）。
func TestOnlineAmbitionActionWeight_DimensionToTag(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "true")
	const want = ambitionFloor + ambitionGain // 某维=1.0 时对应标签的满引力

	type dimCase struct {
		name     string
		set      func(*unit.Ambition)
		hotTags  []string // 该维应放大的标签
		coldTags []string // 该维不应影响的标签（应恒中性）
	}
	cases := []dimCase{
		{"power", func(a *unit.Ambition) { a.Power = 1.0 }, []string{"conquer", "bond"}, []string{"hoard", "nurture"}},
		{"vengeance", func(a *unit.Ambition) { a.Vengeance = 1.0 }, []string{"revenge", "conquer"}, []string{"hoard", "train"}},
		{"wealth", func(a *unit.Ambition) { a.Wealth = 1.0 }, []string{"hoard"}, []string{"conquer", "nurture", "train"}},
		{"lineage", func(a *unit.Ambition) { a.Lineage = 1.0 }, []string{"nurture", "bond"}, []string{"conquer", "hoard"}},
		{"mastery", func(a *unit.Ambition) { a.Mastery = 1.0 }, []string{"train", "explore"}, []string{"conquer", "hoard"}},
		{"freedom", func(a *unit.Ambition) { a.Freedom = 1.0 }, []string{"explore"}, []string{"conquer", "hoard", "nurture"}},
	}
	for _, c := range cases {
		var rec unit.Record
		c.set(&rec.Ambition)
		for _, tag := range c.hotTags {
			if w := OnlineAmbitionActionWeight(rec, tag); w < want-1e-9 || w > want+1e-9 {
				t.Fatalf("[%s] 标签 %q 满野心应得满引力 %.2f，得到 %.3f", c.name, tag, want, w)
			}
		}
		for _, tag := range c.coldTags {
			if w := OnlineAmbitionActionWeight(rec, tag); w != ambitionFloor {
				t.Fatalf("[%s] 无关标签 %q 应恒中性 %.2f，得到 %.3f", c.name, tag, ambitionFloor, w)
			}
		}
	}
}

// TestOnlineAmbitionActionWeight_Deterministic 守确定性：同输入恒同输出（纯函数、零随机/时间）。
func TestOnlineAmbitionActionWeight_Deterministic(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "true")
	var rec unit.Record
	rec.Ambition.Power = 0.7
	rec.Ambition.Wealth = 0.3
	first := OnlineAmbitionActionWeight(rec, "conquer")
	for i := 0; i < 50; i++ {
		if w := OnlineAmbitionActionWeight(rec, "conquer"); w != first {
			t.Fatalf("OnlineAmbitionActionWeight 非确定性：第 %d 次 %.6f != %.6f", i, w, first)
		}
	}
}

// TestOnlineAmbitionScoring_PayToWinFree 守反 P2W 红线：在线野心打分**绝不**读 wallet/billing——
// 同一野心档下，钱包从 0 改到极大值，打分逐位不变。
func TestOnlineAmbitionScoring_PayToWinFree(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "true")
	mk := func(wallet int) unit.Record {
		var rec unit.Record
		rec.Ambition.Vengeance = 0.8
		rec.Status.Wallet = wallet
		return rec
	}
	poor, rich := mk(0), mk(999999)
	if OnlineAmbitionActionWeight(poor, "conquer") != OnlineAmbitionActionWeight(rich, "conquer") {
		t.Fatalf("在线野心乘权随钱包变化 → 违反反 P2W 红线")
	}
}

// TestPickAmbitionBiasedCandidate_FlagOff 守向后兼容铁律：flag 关时绝不重排，恒返回 candidates[0]（取首个语义）。
func TestPickAmbitionBiasedCandidate_FlagOff(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "") // flag 关
	var rec unit.Record
	rec.Ambition.Vengeance = 1.0 // 即便强野心，flag 关也不重排
	// 首个是 observe（中性），后面有 attack（conquer）——flag 关时仍必须取首个 observe。
	cands := []decisionCandidate{
		{Action: DecisionActionObserve},
		{Action: DecisionActionAttack},
	}
	got, ok := PickAmbitionBiasedCandidate(&rec, cands)
	if !ok || got.Action != DecisionActionObserve {
		t.Fatalf("flag 关应取首个 observe，得到 %v ok=%v", got.Action, ok)
	}
	// 空候选集 → false。
	if _, ok := PickAmbitionBiasedCandidate(&rec, nil); ok {
		t.Fatalf("空候选集应返回 ok=false")
	}
	// nil actor → 取首个（失败安全），即便 flag 开。
	t.Setenv(ambitionScoringFlagEnv, "true")
	got2, ok2 := PickAmbitionBiasedCandidate(nil, cands)
	if !ok2 || got2.Action != DecisionActionObserve {
		t.Fatalf("nil actor 应失败安全取首个 observe，得到 %v ok=%v", got2.Action, ok2)
	}
}

// TestPickAmbitionBiasedCandidate_FlagOn 校验 flag 开时按野心引力挑最贴者。
func TestPickAmbitionBiasedCandidate_FlagOn(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "true")

	// 强复仇野心：候选 [observe(中性), attack(conquer)] → 应挑 attack（conquer 被复仇放大）。
	var vengeful unit.Record
	vengeful.Ambition.Vengeance = 1.0
	cands := []decisionCandidate{
		{Action: DecisionActionObserve}, // 中性 1.0
		{Action: DecisionActionAttack},  // conquer，被复仇放大 → 引力更高
	}
	got, ok := PickAmbitionBiasedCandidate(&vengeful, cands)
	if !ok || got.Action != DecisionActionAttack {
		t.Fatalf("强复仇野心应挑 attack，得到 %v ok=%v", got.Action, ok)
	}

	// 强敛财野心：同一候选集里 attack(conquer) 不被敛财放大、trade(hoard) 被放大 → 应挑 trade。
	var greedy unit.Record
	greedy.Ambition.Wealth = 1.0
	cands2 := []decisionCandidate{
		{Action: DecisionActionAttack}, // conquer，敛财不放大 → 中性 1.0
		{Action: DecisionActionTrade},  // hoard，被敛财放大 → 引力更高
	}
	got2, ok2 := PickAmbitionBiasedCandidate(&greedy, cands2)
	if !ok2 || got2.Action != DecisionActionTrade {
		t.Fatalf("强敛财野心应挑 trade，得到 %v ok=%v", got2.Action, ok2)
	}
}

// TestPickAmbitionBiasedCandidate_TiePrefersFirst 校验平手（含全中性）保留靠前者：仅当后者引力**严格更高**才改写首选。
func TestPickAmbitionBiasedCandidate_TiePrefersFirst(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "true")

	// 无野心单位：所有候选引力全中性 1.0 → 平手 → 取首个（与「取首个」逐位一致）。
	var blank unit.Record
	cands := []decisionCandidate{
		{Action: DecisionActionTrade, ID: "c0"},
		{Action: DecisionActionAttack, ID: "c1"},
		{Action: DecisionActionForge, ID: "c2"},
	}
	got, ok := PickAmbitionBiasedCandidate(&blank, cands)
	if !ok || got.ID != "c0" {
		t.Fatalf("无野心全平手应取首个 c0，得到 %v ok=%v", got.ID, ok)
	}

	// 两个同标签候选（都是 conquer），强复仇放大两者到同一引力 → 平手 → 取靠前者。
	var vengeful unit.Record
	vengeful.Ambition.Vengeance = 1.0
	cands2 := []decisionCandidate{
		{Action: DecisionActionAttack, ID: "atk"},      // conquer
		{Action: DecisionActionHeavyAttack, ID: "hvy"}, // conquer，同引力
	}
	got2, ok2 := PickAmbitionBiasedCandidate(&vengeful, cands2)
	if !ok2 || got2.ID != "atk" {
		t.Fatalf("同标签同引力应取靠前者 atk，得到 %v ok=%v", got2.ID, ok2)
	}
}

// TestPickAmbitionBiasedCandidate_Deterministic 守确定性：同输入恒同输出（不依赖 map 迭代序）。
func TestPickAmbitionBiasedCandidate_Deterministic(t *testing.T) {
	t.Setenv(ambitionScoringFlagEnv, "true")
	var rec unit.Record
	rec.Ambition.Power = 0.6
	rec.Ambition.Mastery = 0.9
	cands := []decisionCandidate{
		{Action: DecisionActionObserve, ID: "o"},
		{Action: DecisionActionBuild, ID: "b"},  // train，被 mastery 放大
		{Action: DecisionActionAttack, ID: "a"}, // conquer，被 power 放大（较弱）
	}
	first, _ := PickAmbitionBiasedCandidate(&rec, cands)
	for i := 0; i < 50; i++ {
		got, _ := PickAmbitionBiasedCandidate(&rec, cands)
		if got.ID != first.ID {
			t.Fatalf("PickAmbitionBiasedCandidate 非确定性：第 %d 次 %q != %q", i, got.ID, first.ID)
		}
	}
	// mastery(0.9) → train 引力 1.54 > power(0.6) → conquer 引力 1.36 → 应挑 build。
	if first.ID != "b" {
		t.Fatalf("mastery 强于 power，应挑 train(build)，得到 %q", first.ID)
	}
}
