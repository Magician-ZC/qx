package regionrunner

// 文件说明：动机栈候选生成 + 确定性打分测试（M7.3-real 动机栈消费）。验证 L1 护栏 early-return、候选集组装、
// L3 记忆偏置、野心乘权（flag 门控）、降序排序的确定性，以及候选首选随野心/记忆变化（行为偏置真生效）。
// 与 ambient_llm_test.go 互补（后者测 HOT 门控/LLM 落地/预算）；本文件聚焦纯打分逻辑，零 DB、零 LLM。

import (
	"testing"

	"qunxiang/backend/internal/unit"
)

func mkRec(hunger int, morale float64) unit.Record {
	var rec unit.Record
	rec.Status.Hunger = hunger
	rec.Status.Morale = morale
	return rec
}

// TestDecideAmbientContextual_HungerGuardrail：饥饿 < 阈值 → L1 护栏强制 forage 且只返一个候选（early-return）。
func TestDecideAmbientContextual_HungerGuardrail(t *testing.T) {
	cands := decideAmbientContextual(mkRec(20, 0.9)) // 即便士气高，饿了也只 forage
	if len(cands) != 1 || cands[0].action != actForage {
		t.Fatalf("饿了应 L1 护栏强制单候选 forage，得到 %+v", cands)
	}
	if topAmbientAction(cands) != actForage {
		t.Fatalf("topAmbientAction 应为 forage")
	}
}

// TestDecideAmbientContextual_MoraleLowFavorsSocialize：不饿 + 士气低 → 候选含 socialize，且 socialize 需求分高于 rest。
func TestDecideAmbientContextual_MoraleLowFavorsSocialize(t *testing.T) {
	cands := decideAmbientContextual(mkRec(80, 0.2))
	if got := topAmbientAction(cands); got != actSocialize {
		t.Fatalf("不饿+低落应首选 socialize，得到 %s（候选 %+v）", got, cands)
	}
}

// TestDecideAmbientContextual_MoraleHighFavorsReflect：不饿 + 心满意足 → 首选 reflect。
func TestDecideAmbientContextual_MoraleHighFavorsReflect(t *testing.T) {
	cands := decideAmbientContextual(mkRec(80, 0.95))
	if got := topAmbientAction(cands); got != actReflect {
		t.Fatalf("不饿+满足应首选 reflect，得到 %s（候选 %+v）", got, cands)
	}
}

// TestDecideAmbientContextual_NeutralFallsToRest：不饿 + 士气中间（无强需求）→ flag 关时首选 rest（中性兜底，需求分最高）。
func TestDecideAmbientContextual_NeutralFallsToRest(t *testing.T) {
	t.Setenv("QUNXIANG_AMBITION_SCORING", "") // flag 关 → 纯需求强度序
	cands := decideAmbientContextual(mkRec(80, 0.6))
	// 中间态候选含 rest/socialize/reflect/forage；无强需求时 rest(0.6) 应压过弱倾向(0.5)。
	if got := topAmbientAction(cands); got != actRest {
		t.Fatalf("中间态 flag 关应首选中性 rest，得到 %s（候选 %+v）", got, cands)
	}
}

// TestDecideAmbientContextual_AmbitionShiftsTop：flag 开 + 强敛财野心 → 中间态首选从 rest 偏移到 forage（hoard 标签被放大）。
// 这是「野心打分真消费」的核心验证：同一状态，仅野心不同 → 候选排序改变。
func TestDecideAmbientContextual_AmbitionShiftsTop(t *testing.T) {
	t.Setenv("QUNXIANG_AMBITION_SCORING", "true")
	rec := mkRec(80, 0.6)     // 不饿、士气中间
	rec.Ambition.Wealth = 1.0 // 强敛财 → forage(hoard) 引力 ×1.6
	// forage 基础底分 0.5 × 1.6 = 0.8 > rest 中性 0.6 → 首选应变 forage。
	if got := topAmbientAction(decideAmbientContextual(rec)); got != actForage {
		t.Fatalf("flag 开 + 强敛财野心应把中间态首选偏移到 forage，得到 %s", got)
	}
	// 对照：无野心同状态 flag 开 → 仍 rest（野心乘权全 1.0）。
	if got := topAmbientAction(decideAmbientContextual(mkRec(80, 0.6))); got != actRest {
		t.Fatalf("无野心同状态应仍首选 rest，得到 %s", got)
	}
}

// TestDecideAmbientContextual_FlagOffNoAmbitionEffect：flag 关时，强野心单位的候选排序与无野心单位逐位一致
// （野心乘权恒 1.0，对既有行为零影响）。
func TestDecideAmbientContextual_FlagOffNoAmbitionEffect(t *testing.T) {
	t.Setenv("QUNXIANG_AMBITION_SCORING", "") // 关
	withAmb := mkRec(80, 0.6)
	withAmb.Ambition.Wealth = 1.0
	a := decideAmbientContextual(withAmb)
	b := decideAmbientContextual(mkRec(80, 0.6))
	if len(a) != len(b) {
		t.Fatalf("flag 关时候选数应一致：%d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].action != b[i].action || a[i].finalScore != b[i].finalScore {
			t.Fatalf("flag 关时野心不应改排序：位 %d %s(%.3f) vs %s(%.3f)", i, a[i].action, a[i].finalScore, b[i].action, b[i].finalScore)
		}
	}
}

// TestMemoryBias_ShiftsScore：近期记忆命中某动作关键词 → 该候选基础分被 ×1.15 抬升。
func TestMemoryBias_ShiftsScore(t *testing.T) {
	noMem := mkRec(80, 0.6)
	if mb := memoryBias(noMem, actReflect); mb != 1.0 {
		t.Fatalf("无记忆应 ×1.0 中性，得到 %.3f", mb)
	}
	withMem := mkRec(80, 0.6)
	withMem.Memory.Highlights = []string{"她独处时陷入了深深的反思"}
	if mb := memoryBias(withMem, actReflect); mb != memoryBiasMultiplier {
		t.Fatalf("命中记忆关键词应 ×%.2f，得到 %.3f", memoryBiasMultiplier, mb)
	}
	// 不相关记忆不抬升其它动作。
	if mb := memoryBias(withMem, actForage); mb != 1.0 {
		t.Fatalf("不相关记忆不应抬升 forage，得到 %.3f", mb)
	}
}

// TestDecideAmbientContextual_Deterministic：同输入恒同输出（顺序+分值），含平手字典序的确定性。
func TestDecideAmbientContextual_Deterministic(t *testing.T) {
	rec := mkRec(80, 0.6)
	rec.Memory.Highlights = []string{"与盟友的攀谈让她舒展"}
	first := decideAmbientContextual(rec)
	for n := 0; n < 30; n++ {
		got := decideAmbientContextual(rec)
		if len(got) != len(first) {
			t.Fatalf("非确定性：候选数变化 %d vs %d", len(got), len(first))
		}
		for i := range got {
			if got[i].action != first[i].action || got[i].finalScore != first[i].finalScore {
				t.Fatalf("非确定性：第 %d 次位 %d %s(%.6f) vs %s(%.6f)", n, i, got[i].action, got[i].finalScore, first[i].action, first[i].finalScore)
			}
		}
	}
}

// TestDecideAmbientContextual_AlwaysNonEmpty：任意输入至少留一候选（失败安全，topAmbientAction 永不空兜底）。
func TestDecideAmbientContextual_AlwaysNonEmpty(t *testing.T) {
	for _, h := range []int{0, 40, 100} {
		for _, m := range []float64{0.0, 0.5, 1.0} {
			if cands := decideAmbientContextual(mkRec(h, m)); len(cands) == 0 {
				t.Fatalf("decideAmbientContextual(h=%d,m=%.1f) 返回空候选", h, m)
			}
		}
	}
}

// TestBuildAmbientPrompt_InjectsMotivation：HOT-LLM prompt 注入主导渴望/候选倾向分；候选空时退化为极简 prompt（不 panic）。
func TestBuildAmbientPrompt_InjectsMotivation(t *testing.T) {
	t.Setenv("QUNXIANG_AMBITION_SCORING", "true")
	rec := mkRec(80, 0.6)
	rec.Identity.Name = "阿青"
	// 强敛财野心 → Dominant 标签 hoard → 主导渴望「敛财囤积」（用 wealth 维取唯一标签，避免 power/vengeance 的多标签平手歧义）。
	rec.Ambition.Wealth = 1.0
	rec.Memory.Highlights = []string{"她记起了那场背叛"}
	cands := decideAmbientContextual(rec)
	prompt := buildAmbientPrompt(rec, cands)
	if !containsAny(prompt, "敛财囤积") {
		t.Fatalf("prompt 应注入主导渴望「敛财囤积」，得到：%s", prompt)
	}
	if !containsAny(prompt, "候选日常与倾向分") {
		t.Fatalf("prompt 应注入候选倾向分，得到：%s", prompt)
	}
	if !containsAny(prompt, "最近触发的记忆") {
		t.Fatalf("prompt 应注入最近记忆，得到：%s", prompt)
	}
	// 候选空 → 退化极简 prompt，不 panic、仍含动作菜单。
	bare := buildAmbientPrompt(mkRec(80, 0.6), nil)
	if !containsAny(bare, "forage") {
		t.Fatalf("空候选 prompt 仍应含动作菜单，得到：%s", bare)
	}
}

func containsAny(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestContextualTopMatchesLegacyReflexWhenFlagOff 守向后兼容铁律：野心 flag 关时，动机栈候选首选必须与 legacy
// 反射层 decideAmbientReflex 在整个 (hunger×morale) 状态空间逐点一致——证明扩链对既有离线行为零影响（仅 flag 开才偏移）。
func TestContextualTopMatchesLegacyReflexWhenFlagOff(t *testing.T) {
	t.Setenv("QUNXIANG_AMBITION_SCORING", "") // flag 关
	for h := 0; h <= 100; h += 5 {
		for m := 0.0; m <= 1.0; m += 0.05 {
			rec := mkRec(h, m)
			legacy := decideAmbientReflex(rec)
			top := topAmbientAction(decideAmbientContextual(rec))
			if top != legacy {
				t.Fatalf("flag 关时首选应等于 legacy 反射：h=%d m=%.2f 动机栈=%s legacy=%s", h, m, top, legacy)
			}
		}
	}
}
