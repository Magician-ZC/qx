package decision

// 文件说明：归因校验与门控意外的单元测试，验证「意外但合理」被代码强制——
// 无源戏剧性意外被判 OOC，突然恋爱/卖传家宝/叛变在缺前因时被门控挡住。

import "testing"

func baseSnapshot() Snapshot {
	return Snapshot{
		Traits: map[string]float64{"vengeance": 0.85, "freedom": 0.8, "calm": 0.52},
		Memories: map[string]MemoryMeta{
			"mem_8842": {Importance: 8, EmotionWeight: -0.7, Salience: 0.6, Summary: "童年在庄园被羞辱"},
			"mem_weak": {Importance: 3, EmotionWeight: 0.1, Salience: 0.5, Summary: "路过一个集市"},
			"mem_dead": {Importance: 9, EmotionWeight: -0.9, Salience: 0.10, Summary: "很久以前的旧事"},
		},
		Redlines:             map[string]string{"rl_3": "朋友有难，绝不旁观"},
		Relations:            map[string]RelationAxes{"u_amu": {Affection: 0.88}, "u_far": {Affection: 0.1}},
		Pressure:             PressureFlags{Hunger: true, Debt: true},
		PlayerActionEventIDs: map[string]struct{}{"evt_order_41": {}},
	}
}

func TestValidate_OK_StrongMemory(t *testing.T) {
	attr := Attribution{
		Primary:       CauseRef{Kind: CauseMemory, RefID: "mem_8842", Weight: 0.8, SnippetZH: "童年在庄园被羞辱"},
		SurpriseLevel: 2,
		NarrativeZH:   "她不肯向你举荐的领主低头，因为童年在庄园被羞辱过",
	}
	if v := ValidateAttribution(attr, baseSnapshot()); !v.OK {
		t.Fatalf("强记忆前因 + 合理意外应通过，得到 %s", v.Reason)
	}
}

func TestValidate_NarrativeEmpty(t *testing.T) {
	attr := Attribution{Primary: CauseRef{Kind: CausePressure, RefID: "hunger", Weight: 0.6}, NarrativeZH: "  "}
	if v := ValidateAttribution(attr, baseSnapshot()); v.OK || v.Reason != OOCNarrativeEmpty {
		t.Fatalf("空因果句应判 NARRATIVE_EMPTY，得到 %+v", v)
	}
}

func TestValidate_CauseNotFound(t *testing.T) {
	attr := Attribution{Primary: CauseRef{Kind: CauseMemory, RefID: "mem_does_not_exist", Weight: 0.8, SnippetZH: "x"}, NarrativeZH: "因为某事"}
	if v := ValidateAttribution(attr, baseSnapshot()); v.OK || v.Reason != OOCCauseNotFound {
		t.Fatalf("不存在的记忆 ID 应判 CAUSE_NOT_FOUND，得到 %+v", v)
	}
}

func TestValidate_CauseTooWeak(t *testing.T) {
	// persona calm=0.52，|0.52-0.5|=0.02 < 0.25，不显著。
	weakTrait := Attribution{Primary: CauseRef{Kind: CausePersonaTrait, RefID: "calm", Weight: 0.6}, NarrativeZH: "因为她性格平和", SurpriseLevel: 0}
	if v := ValidateAttribution(weakTrait, baseSnapshot()); v.OK || v.Reason != OOCCauseTooWeak {
		t.Fatalf("不显著人格维应判 CAUSE_TOO_WEAK，得到 %+v", v)
	}
	// 远距离弱关系 affection=0.1 < 0.3。
	weakRel := Attribution{Primary: CauseRef{Kind: CauseRelation, RefID: "u_far", Weight: 0.6}, NarrativeZH: "因为她和那人有点交情", SurpriseLevel: 0}
	if v := ValidateAttribution(weakRel, baseSnapshot()); v.OK || v.Reason != OOCCauseTooWeak {
		t.Fatalf("弱关系应判 CAUSE_TOO_WEAK，得到 %+v", v)
	}
	// salience 衰减到死的记忆。
	deadMem := Attribution{Primary: CauseRef{Kind: CauseMemory, RefID: "mem_dead", Weight: 0.8, SnippetZH: "很久以前的旧事"}, NarrativeZH: "因为旧事", SurpriseLevel: 0}
	if v := ValidateAttribution(deadMem, baseSnapshot()); v.OK || v.Reason != OOCCauseTooWeak {
		t.Fatalf("salience 死掉的记忆应判 CAUSE_TOO_WEAK，得到 %+v", v)
	}
}

func TestValidate_SurpriseUngrounded(t *testing.T) {
	// 明显意外(3)但 primary 只是显著人格维、无强 memory/relation/redline 支撑。
	attr := Attribution{
		Primary:       CauseRef{Kind: CausePersonaTrait, RefID: "freedom", Weight: 0.6},
		SurpriseLevel: 3,
		NarrativeZH:   "她突然抛下一切，因为她向往自由",
	}
	if v := ValidateAttribution(attr, baseSnapshot()); v.OK || v.Reason != OOCSurpriseUngrounded {
		t.Fatalf("无强前因的明显意外应判 SURPRISE_UNGROUNDED，得到 %+v", v)
	}
}

func TestValidate_SurpriseGroundedBySupporting(t *testing.T) {
	// 同样是显著人格 primary，但加一条强记忆 supporting，则明显意外可成立。
	attr := Attribution{
		Primary:       CauseRef{Kind: CausePersonaTrait, RefID: "vengeance", Weight: 0.6},
		Supporting:    []CauseRef{{Kind: CauseMemory, RefID: "mem_8842", Weight: 0.7, SnippetZH: "童年在庄园被羞辱"}},
		SurpriseLevel: 2,
		NarrativeZH:   "她出乎意料地动了杀心，因为旧恨被勾起",
	}
	if v := ValidateAttribution(attr, baseSnapshot()); !v.OK {
		t.Fatalf("有强记忆 supporting 的明显意外应通过，得到 %s", v.Reason)
	}
}

func TestValidate_SnippetFabricated(t *testing.T) {
	// 记忆存在且显著，但 snippet 与真实 Summary 无重合 → 编造因果句。
	attr := Attribution{
		Primary:       CauseRef{Kind: CauseMemory, RefID: "mem_8842", Weight: 0.8, SnippetZH: "她曾在海上遇难漂流三月"},
		SurpriseLevel: 1,
		NarrativeZH:   "因为海难",
	}
	if v := ValidateAttribution(attr, baseSnapshot()); v.OK || v.Reason != OOCSnippetFabricated {
		t.Fatalf("编造的 snippet 应判 SNIPPET_FABRICATED，得到 %+v", v)
	}
}

func TestValidate_PressureAndRedlineAndEcho(t *testing.T) {
	snap := baseSnapshot()
	cases := []struct {
		name string
		attr Attribution
	}{
		{"pressure_debt", Attribution{Primary: CauseRef{Kind: CausePressure, RefID: "debt", Weight: 0.7}, NarrativeZH: "她欠债被逼，只能卖艺", SurpriseLevel: 0}},
		{"redline", Attribution{Primary: CauseRef{Kind: CauseRedline, RefID: "rl_3", Weight: 0.9, SnippetZH: "朋友有难，绝不旁观"}, NarrativeZH: "你嘱托过她朋友有难绝不旁观", SurpriseLevel: 1}},
		{"order_echo", Attribution{Primary: CauseRef{Kind: CauseOrderEcho, RefID: "evt_order_41", Weight: 0.6}, NarrativeZH: "因为你上次的叮嘱", SurpriseLevel: 0}},
	}
	for _, c := range cases {
		if v := ValidateAttribution(c.attr, snap); !v.OK {
			t.Fatalf("%s 应通过，得到 %s", c.name, v.Reason)
		}
	}
	// 压力位未激活（threat=false）应判 too weak。
	inactive := Attribution{Primary: CauseRef{Kind: CausePressure, RefID: "threat", Weight: 0.7}, NarrativeZH: "她受威胁", SurpriseLevel: 0}
	if v := ValidateAttribution(inactive, snap); v.OK || v.Reason != OOCCauseTooWeak {
		t.Fatalf("未激活压力位应判 CAUSE_TOO_WEAK，得到 %+v", v)
	}
}

func TestGateSurprise_Romance(t *testing.T) {
	if r := GateSurprise(ActionRomance, GateInput{TargetAffection: 0.1, RelationMemoryCount: 0, AccumulatedWindows: 0}); r.Decision != GateReject {
		t.Fatalf("一面之缘就告白应被剔除，得到 %+v", r)
	}
	if r := GateSurprise(ActionRomance, GateInput{TargetAffection: 0.5, RelationMemoryCount: 2, AccumulatedWindows: 3}); r.Decision != GateAllow {
		t.Fatalf("有充分前因的告白应放行，得到 %+v", r)
	}
}

func TestGateSurprise_SellPinned(t *testing.T) {
	if r := GateSurprise(ActionSellPinned, GateInput{ItemIsPermanentAnchor: true, HasDebtPressure: true}); r.Decision != GateReject {
		t.Fatalf("父辈遗志类绝不可卖，应剔除，得到 %+v", r)
	}
	if r := GateSurprise(ActionSellPinned, GateInput{HasDebtPressure: true}); r.Decision != GateAllow {
		t.Fatalf("有债务压力可自治变卖，应放行，得到 %+v", r)
	}
	if r := GateSurprise(ActionSellPinned, GateInput{}); r.Decision != GateFreeze {
		t.Fatalf("无压力卖传家宝应上交玩家(冻结)，得到 %+v", r)
	}
}

func TestGateSurprise_Defect(t *testing.T) {
	if r := GateSurprise(ActionDefect, GateInput{Loyalty: 0.2, HasNegativeMemory: true}); r.Decision != GateAllow {
		t.Fatalf("低忠诚+负面记忆的叛变应放行，得到 %+v", r)
	}
	if r := GateSurprise(ActionDefect, GateInput{Loyalty: 0.2}); r.Decision != GateReject {
		t.Fatalf("仅低忠诚无前因的叛变应剔除，得到 %+v", r)
	}
	if r := GateSurprise(ActionDefect, GateInput{Loyalty: 0.9, HasNegativeMemory: true}); r.Decision != GateReject {
		t.Fatalf("高忠诚不应叛变，得到 %+v", r)
	}
}

func TestGateSurprise_UngatedAllowed(t *testing.T) {
	if r := GateSurprise(GatedAction("move"), GateInput{}); r.Decision != GateAllow {
		t.Fatalf("非门控动作应一律放行，得到 %+v", r)
	}
}
