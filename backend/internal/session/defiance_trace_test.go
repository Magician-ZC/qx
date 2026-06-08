package session

// 文件说明：defiance_trace_test.go，验证抗命溯源卡 + 固定成长旁白（宪法 §3.3）——
// 抗命决策的可见字段必含成长旁白原文与五来源溯源标签；服从路径零影响；确定性。

import (
	"strings"
	"testing"
)

// TestRefusedDecisionEmbedsGrowthNarration 验证抗命决策可见字段含固定成长旁白原文。
func TestRefusedDecisionEmbedsGrowthNarration(t *testing.T) {
	requested := unitDecisionPayload{
		Action:    DecisionActionAttack,
		Speak:     "我冲了",
		Reasoning: "这越过了她的红线，不肯杀。",
	}
	got := refusedDecision(requested, requested.Reasoning)

	if got.Action != DecisionActionHold {
		t.Fatalf("抗命应转 hold，得到 %s", got.Action)
	}
	// 成长旁白必须逐字出现在可见字段（Speak 直接承载，Reasoning 冗余携带）。
	if got.Speak != defianceGrowthNarration {
		t.Fatalf("Speak 应为固定成长旁白原文，得到 %q", got.Speak)
	}
	if !strings.Contains(got.Speak+got.Reasoning, "她第一次没有照你说的做。她在变成她自己。") {
		t.Fatalf("可见字段缺成长旁白原文：speak=%q reasoning=%q", got.Speak, got.Reasoning)
	}
}

// TestRefusedDecisionEmbedsTraceCard 验证抗命决策 Reasoning 含可解析的溯源卡与来源标签。
func TestRefusedDecisionEmbedsTraceCard(t *testing.T) {
	requested := unitDecisionPayload{Action: DecisionActionCharge}
	got := refusedDecision(requested, "上次在那里中了埋伏，她记得那个教训。")

	if !strings.Contains(got.Reasoning, defianceTraceMarker) {
		t.Fatalf("Reasoning 缺溯源卡标记：%q", got.Reasoning)
	}
	// 记忆类理由应归因到「记忆」来源标签。
	if !strings.Contains(got.Reasoning, "source="+string(defianceSourceMemory)) {
		t.Fatalf("溯源来源应为记忆，得到 %q", got.Reasoning)
	}
	if !strings.Contains(got.Reasoning, "phrase=") {
		t.Fatalf("溯源卡缺「她为什么没听你」短语：%q", got.Reasoning)
	}
	// 原拒绝理由应被保留在溯源卡之前。
	if !strings.HasPrefix(strings.TrimSpace(got.Reasoning), "上次在那里中了埋伏") {
		t.Fatalf("原拒绝理由应保留在前：%q", got.Reasoning)
	}
}

// TestDefianceSourceClassification 验证五来源关键词归因确定且覆盖五类。
func TestDefianceSourceClassification(t *testing.T) {
	cases := []struct {
		reason string
		want   defianceSourceLabel
	}{
		{"这越过了她不肯杀人的红线", defianceSourceRedline},
		{"上次照做中了埋伏，她记得教训", defianceSourceMemory},
		{"她舍不得丢下身边的战友", defianceSourceRelation},
		{"她太累了，士气崩了，扛不住", defianceSourcePressure},
		{"按风险评估拒绝执行", defianceSourcePersona},
		{"", defianceSourcePersona},
	}
	for _, tc := range cases {
		label, phrase := defianceSource(tc.reason)
		if label != tc.want {
			t.Errorf("defianceSource(%q) 来源=%s，期望=%s", tc.reason, label, tc.want)
		}
		if strings.TrimSpace(phrase) == "" {
			t.Errorf("defianceSource(%q) 短语不应为空", tc.reason)
		}
		// 确定性：同输入重复调用结果一致。
		if l2, _ := defianceSource(tc.reason); l2 != label {
			t.Errorf("defianceSource(%q) 非确定：%s vs %s", tc.reason, label, l2)
		}
	}
}

// TestObedientPathHasNoNarration 验证服从（steady）路径不触发溯源卡/成长旁白。
func TestObedientPathHasNoNarration(t *testing.T) {
	// steady：actor 非玩家阵营，resolveDirectiveCompliance 在最前早返回原决策。
	state := State{PlayerFactionID: "player"}
	requested := unitDecisionPayload{
		Action:    DecisionActionAttack,
		Speak:     "前进",
		Reasoning: "按方针推进",
	}
	res := resolveDirectiveComplianceWithRoll(state, nil, nil, requested, 0.0)
	if res.State != obedienceSteady {
		t.Fatalf("nil actor 应判 steady，得到 %s", res.State)
	}
	if res.Final.Speak == defianceGrowthNarration {
		t.Fatalf("服从路径不应注入成长旁白：%q", res.Final.Speak)
	}
	if strings.Contains(res.Final.Reasoning, defianceTraceMarker) {
		t.Fatalf("服从路径不应含溯源卡：%q", res.Final.Reasoning)
	}
	if strings.Contains(res.Final.Speak+res.Final.Reasoning, defianceGrowthNarration) {
		t.Fatalf("服从路径可见字段不应含成长旁白原文")
	}
}

// TestComposeDefianceTraceParsable 验证溯源卡编码可被前端按约定切分还原。
func TestComposeDefianceTraceParsable(t *testing.T) {
	line := composeDefianceTrace(defianceSourceRelation, "她放不下身边那个人。")
	if !strings.HasPrefix(line, defianceTraceMarker) {
		t.Fatalf("溯源卡应以标记起头：%q", line)
	}
	body := strings.TrimSpace(strings.TrimPrefix(line, defianceTraceMarker))
	fields := map[string]string{}
	for _, seg := range strings.Split(body, defianceTraceFieldSep) {
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) == 2 {
			fields[kv[0]] = kv[1]
		}
	}
	if fields["source"] != string(defianceSourceRelation) {
		t.Errorf("source 解析错误：%v", fields)
	}
	if fields["phrase"] != "她放不下身边那个人。" {
		t.Errorf("phrase 解析错误：%v", fields)
	}
	if fields["narration"] != defianceGrowthNarration {
		t.Errorf("narration 解析错误：%v", fields)
	}
}
