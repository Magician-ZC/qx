package session

// 文件说明：data-driven 翻译层（translation.go）的纯函数测试——覆盖三类宏观事件（征召/欠债/投靠）命中正确模板、
// 通用兜底回落、未命中时 matched=false（向后兼容）、占位 {target}/{event} 渲染、确定性（同输入同输出）。
// 全部无 DB、无 LLM、无副作用。前缀统一 Translation* 避免与既有命运测试撞名。

import (
	"strings"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
)

// ---- 事件语义类判定 ----

func TestClassifyFateEvent(t *testing.T) {
	cases := []struct {
		name string
		ev   FateEvent
		want fateEventClass
	}{
		// ReasonCode 强信号：背叛/势力崩塌 → 投靠/倒戈语义。
		{"背叛 reason→defection", FateEvent{ReasonCode: events.ReasonRelationBetray}, classDefection},
		{"势力崩塌 reason→defection", FateEvent{ReasonCode: events.ReasonFactionCollapse}, classDefection},
		// 关键词扫 Summary。
		{"征召关键词→conscription", FateEvent{ReasonCode: events.ReasonInboxHighlight, Summary: "老吴被征召上了前线"}, classConscription},
		{"参军关键词→conscription", FateEvent{Summary: "他应征入伍，奔赴前线"}, classConscription},
		{"欠债关键词→debt", FateEvent{Summary: "她欠下了一身还不清的债"}, classDebt},
		{"借贷关键词→debt", FateEvent{Summary: "为了周转，他向债主借贷"}, classDebt},
		{"投靠关键词→defection", FateEvent{Summary: "他改换门庭，投靠了别处"}, classDefection},
		// 关键词也扫 AttributionZH（宏观语义有时落在归因句里）。
		{"AttributionZH 含征召", FateEvent{Summary: "一封信送到了门前", AttributionZH: "因为她被征召了"}, classConscription},
		// 未识别 → generic。
		{"无关键词→generic", FateEvent{Summary: "她在集市上买了些盐"}, classGeneric},
		{"空摘要无 reason→generic", FateEvent{}, classGeneric},
	}
	for _, c := range cases {
		if got := classifyFateEvent(c.ev); got != c.want {
			t.Fatalf("%s：classifyFateEvent=%q，期望 %q", c.name, got, c.want)
		}
	}
}

// ---- 三类宏观事件命中正确模板 ----

func TestTranslateFateBeat_ConscriptionHitsTemplate(t *testing.T) {
	ev := FateEvent{
		ReasonCode: events.ReasonInboxHighlight,
		ActorID:    "u_kin",
		TargetID:   "u_owner",
		Summary:    "老吴被征召上了前线",
	}
	beat, matched := translateFateBeat(ev, relevance.Relation)
	if !matched {
		t.Fatalf("征召×关系锚应命中模板")
	}
	want := fateTranslationTable[translationKey{class: classConscription, anchor: relevance.Relation}]
	if beat != renderFateTemplate(want, ev) {
		t.Fatalf("征召×关系锚 beat 不符：%q", beat)
	}
	// 命中的应是精确征召模板（含「征召」字样），而非通用兜底。
	if !strings.Contains(beat, "征召") {
		t.Fatalf("应命中精确征召模板，得到 %q", beat)
	}
	// 占位 {target} 应被 ActorID 替换。
	if !strings.Contains(beat, "u_kin") {
		t.Fatalf("{target} 应渲染为 ActorID，得到 %q", beat)
	}
}

func TestTranslateFateBeat_DebtHitsTemplate(t *testing.T) {
	ev := FateEvent{ActorID: "u_friend", Summary: "她欠下了一身还不清的债"}
	beat, matched := translateFateBeat(ev, relevance.Redline)
	if !matched {
		t.Fatalf("欠债×红线锚应命中模板")
	}
	want := renderFateTemplate(fateTranslationTable[translationKey{class: classDebt, anchor: relevance.Redline}], ev)
	if beat != want {
		t.Fatalf("欠债×红线锚 beat 不符：%q != %q", beat, want)
	}
	if !strings.Contains(beat, "u_friend") {
		t.Fatalf("{target} 应渲染为 ActorID，得到 %q", beat)
	}
}

func TestTranslateFateBeat_DefectionHitsTemplate(t *testing.T) {
	// reason-code 强信号路径（无关键词也应判 defection）。
	ev := FateEvent{ReasonCode: events.ReasonRelationBetray, ActorID: "u_traitor", Summary: "他在最关键的时候反水"}
	beat, matched := translateFateBeat(ev, relevance.DebtGrudgeLove)
	if !matched {
		t.Fatalf("投靠/倒戈×债仇爱锚应命中模板")
	}
	want := renderFateTemplate(fateTranslationTable[translationKey{class: classDefection, anchor: relevance.DebtGrudgeLove}], ev)
	if beat != want {
		t.Fatalf("投靠×债仇爱锚 beat 不符：%q != %q", beat, want)
	}
}

// 全锚类 × 三语义类的精确模板均存在且可渲染（防漏配/占位渲染不出错）。
func TestTranslateFateBeat_AllAnchorsCoveredForMacroClasses(t *testing.T) {
	anchors := []relevance.AnchorKind{
		relevance.Relation, relevance.Redline, relevance.Goal,
		relevance.DebtGrudgeLove, relevance.Geo, relevance.Legacy,
	}
	classes := map[fateEventClass]FateEvent{
		classConscription: {Summary: "他被征召上了前线", ActorID: "a"},
		classDebt:         {Summary: "他欠下了债", ActorID: "a"},
		classDefection:    {ReasonCode: events.ReasonRelationBetray, ActorID: "a"},
	}
	for class, ev := range classes {
		for _, anchor := range anchors {
			tmpl, ok := fateTranslationTable[translationKey{class: class, anchor: anchor}]
			if !ok {
				t.Fatalf("缺精确模板：(%s × %s)", class, anchor)
			}
			beat, matched := translateFateBeat(ev, anchor)
			if !matched || beat == "" {
				t.Fatalf("(%s × %s) 应命中且非空", class, anchor)
			}
			// 渲染后不应残留占位。
			if strings.Contains(beat, "{target}") || strings.Contains(beat, "{event}") {
				t.Fatalf("(%s × %s) 渲染后仍有占位：%q", class, anchor, beat)
			}
			_ = tmpl
		}
	}
}

// ---- 通用兜底（generic × 锚类）回落 ----

func TestTranslateFateBeat_GenericFallbackByAnchor(t *testing.T) {
	// 语义类未识别（普通经济奖励、无宏观关键词），但有锚类 → 回落「通用兜底 × 锚类」，仍命中。
	ev := FateEvent{ReasonCode: events.ReasonEconomyReward, Summary: "她在远方做成了一桩买卖"}
	beat, matched := translateFateBeat(ev, relevance.Goal)
	if !matched {
		t.Fatalf("未识别语义类但有锚类应命中通用兜底")
	}
	want := renderFateTemplate(fateGenericAnchorTemplate[relevance.Goal], ev)
	if beat != want {
		t.Fatalf("通用兜底×目标锚 beat 不符：%q != %q", beat, want)
	}
	// 通用兜底应内嵌 {event} 渲染出的摘要。
	if !strings.Contains(beat, "她在远方做成了一桩买卖") {
		t.Fatalf("通用兜底应嵌入事件摘要，得到 %q", beat)
	}
}

// ---- 未命中：matched=false（向后兼容，调用方零行为变化）----

func TestTranslateFateBeat_NoAnchorReturnsUnmatched(t *testing.T) {
	// 无锚类（自身事件）+ 未识别语义类 → matched=false，beat 为空。
	beat, matched := translateFateBeat(FateEvent{Summary: "她独自走完了那条路"}, "")
	if matched || beat != "" {
		t.Fatalf("无锚类自身事件应未命中（matched=false, beat=\"\"），得到 (%q,%v)", beat, matched)
	}
	// 即便是宏观语义类，缺锚类仍未命中（翻译层不强行翻译自身事件，交还调用方）。
	beat2, matched2 := translateFateBeat(FateEvent{ReasonCode: events.ReasonRelationBetray}, "")
	if matched2 || beat2 != "" {
		t.Fatalf("宏观语义但无锚类应未命中，得到 (%q,%v)", beat2, matched2)
	}
}

// ---- 占位渲染：缺省值与无摘要裁剪 ----

func TestRenderFateTemplate_Placeholders(t *testing.T) {
	// {target} 缺省：ActorID 空回落 TargetID。
	if got := renderFateTemplate("{target}来了", FateEvent{TargetID: "u_t"}); got != "u_t来了" {
		t.Fatalf("{target} 应回落 TargetID，得到 %q", got)
	}
	// {target} 全空 → 「那个人」。
	if got := renderFateTemplate("{target}来了", FateEvent{}); got != "那个人来了" {
		t.Fatalf("{target} 全空应为「那个人」，得到 %q", got)
	}
	// 无摘要：「，{event}」整段连引导标点被裁掉，无空尾巴。
	if got := renderFateTemplate("刀兵将至，{event}", FateEvent{}); got != "刀兵将至" {
		t.Fatalf("无摘要应裁掉「，{event}」，得到 %q", got)
	}
	if got := renderFateTemplate("红线在前：{event}", FateEvent{}); got != "红线在前" {
		t.Fatalf("无摘要应裁掉「：{event}」，得到 %q", got)
	}
	// 有摘要：{event} 被替换。
	if got := renderFateTemplate("缘由：{event}", FateEvent{Summary: "她倒下了"}); got != "缘由：她倒下了" {
		t.Fatalf("{event} 应替换为摘要，得到 %q", got)
	}
}

// ---- 确定性：同输入恒同输出 ----

func TestTranslateFateBeat_Deterministic(t *testing.T) {
	ev := FateEvent{ReasonCode: events.ReasonInboxHighlight, ActorID: "u_kin", Summary: "老吴被征召上了前线"}
	first, m1 := translateFateBeat(ev, relevance.Relation)
	for i := 0; i < 20; i++ {
		got, m := translateFateBeat(ev, relevance.Relation)
		if got != first || m != m1 {
			t.Fatalf("非确定性：第 %d 次得到 (%q,%v)，首次 (%q,%v)", i, got, m, first, m1)
		}
	}
}
