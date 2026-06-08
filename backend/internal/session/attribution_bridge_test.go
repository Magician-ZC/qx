package session

// 文件说明：归因接入桥的单元测试——快照构造、线上结构映射、以及 checkAttribution 的影子校验。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/unit"
)

func TestBuildAttributionSnapshot_TraitsAndPressure(t *testing.T) {
	actor := &unit.Record{
		Personality: unit.Personality{Courage: 0.8, Aggression: 0.9, Loyalty: 0.3},
		Status:      unit.Status{HP: 10, Hunger: 20, Fatigue: 80, InCombat: true, Wallet: 0},
	}
	snap := buildAttributionSnapshot(actor)
	if len(snap.Traits) != 8 {
		t.Fatalf("应有 8 个人格维，得到 %d", len(snap.Traits))
	}
	if snap.Traits["aggression"] != 0.9 || snap.Traits["courage"] != 0.8 {
		t.Fatalf("人格维映射错误：%+v", snap.Traits)
	}
	p := snap.Pressure
	if !p.Hunger || !p.Injury || !p.Fatigue || !p.Threat || !p.Debt {
		t.Fatalf("压力位应全部触发，得到 %+v", p)
	}
	// 健康单位无压力。
	healthy := buildAttributionSnapshot(&unit.Record{Status: unit.Status{HP: 100, Hunger: 100, Fatigue: 0, Wallet: 50}})
	if healthy.Pressure != (decision.PressureFlags{}) {
		t.Fatalf("健康单位不应有压力位，得到 %+v", healthy.Pressure)
	}
}

func TestToEngineAttribution(t *testing.T) {
	if _, ok := toEngineAttribution(nil); ok {
		t.Fatalf("nil 归因应返回 false")
	}
	payload := &attributionPayload{
		Primary:       causeRefPayload{Kind: "pressure", RefID: "hunger", Weight: 0.7, SnippetZH: "她饿了"},
		Supporting:    []causeRefPayload{{Kind: "persona_trait", RefID: "prudence", Weight: 0.4}},
		SurpriseLevel: 1,
		NarrativeZH:   "她饿得先去找吃的",
	}
	attr, ok := toEngineAttribution(payload)
	if !ok {
		t.Fatalf("非 nil 归因应返回 true")
	}
	if attr.Primary.Kind != decision.CausePressure || attr.Primary.RefID != "hunger" {
		t.Fatalf("primary 映射错误：%+v", attr.Primary)
	}
	if len(attr.Supporting) != 1 || attr.Supporting[0].Kind != decision.CausePersonaTrait {
		t.Fatalf("supporting 映射错误：%+v", attr.Supporting)
	}
	if attr.SurpriseLevel != 1 || attr.NarrativeZH != "她饿得先去找吃的" {
		t.Fatalf("标量字段映射错误：%+v", attr)
	}
}

func TestCheckAttribution(t *testing.T) {
	service := &Service{}

	// 无归因 → 跳过。
	if _, has := service.checkAttribution(&unit.Record{}, unitDecisionChoicePayload{}); has {
		t.Fatalf("无 attribution 应返回 has=false")
	}

	hungry := &unit.Record{Status: unit.Status{Hunger: 10, HP: 100}}
	aggressive := &unit.Record{Personality: unit.Personality{Aggression: 0.9}, Status: unit.Status{HP: 100, Hunger: 100}}

	// 压力归因（hunger 已激活）→ 通过。
	okChoice := unitDecisionChoicePayload{Attribution: &attributionPayload{
		Primary: causeRefPayload{Kind: "pressure", RefID: "hunger", Weight: 0.7}, NarrativeZH: "她饿了才去采集",
	}}
	if v, has := service.checkAttribution(hungry, okChoice); !has || !v.OK {
		t.Fatalf("已激活压力的归因应通过，得到 has=%v %+v", has, v)
	}

	// 显著人格归因 → 通过。
	traitChoice := unitDecisionChoicePayload{Attribution: &attributionPayload{
		Primary: causeRefPayload{Kind: "persona_trait", RefID: "aggression", Weight: 0.6}, NarrativeZH: "她生性好斗，主动出击",
	}}
	if v, has := service.checkAttribution(aggressive, traitChoice); !has || !v.OK {
		t.Fatalf("显著人格归因应通过，得到 has=%v %+v", has, v)
	}

	// 未激活压力（threat 未触发）→ 判 OOC。
	oocChoice := unitDecisionChoicePayload{Attribution: &attributionPayload{
		Primary: causeRefPayload{Kind: "pressure", RefID: "threat", Weight: 0.7}, NarrativeZH: "她感到威胁",
	}}
	if v, has := service.checkAttribution(hungry, oocChoice); !has || v.OK || v.Reason != decision.OOCCauseTooWeak {
		t.Fatalf("未激活压力应判 CAUSE_TOO_WEAK，得到 has=%v %+v", has, v)
	}
}

func TestBuildDecisionAttributionContext_Redlines(t *testing.T) {
	service := &Service{} // 无 DB：仅验证宪章红线纯逻辑填充（早返回前已填）。
	actor := &unit.Record{ID: "u1", Personality: unit.Personality{Aggression: 0.9}}
	state := State{}
	SetUnitCharter(&state, "u1", OfflineCharter{
		Redlines: []CharterRedline{
			{ID: "rl_no_harm", Text: "绝不伤害平民"},
			{Text: "绝不背叛主公"}, // 缺 ID，应补确定性 ID
		},
	})

	snap, _ := service.buildDecisionAttributionContext(context.Background(), state, actor)
	if len(snap.Redlines) != 2 {
		t.Fatalf("应载入 2 条红线，得到 %d：%+v", len(snap.Redlines), snap.Redlines)
	}
	if snap.Redlines["rl_no_harm"] != "绝不伤害平民" {
		t.Fatalf("显式红线 ID 应保留原文，得到 %q", snap.Redlines["rl_no_harm"])
	}

	// 端到端：以红线为 primary 的归因应通过校验（snippet 与原文重合）。
	attr := decision.Attribution{
		Primary:     decision.CauseRef{Kind: decision.CauseRedline, RefID: "rl_no_harm", Weight: 0.8, SnippetZH: "绝不伤害平民"},
		NarrativeZH: "她守住底线，未对平民下手",
	}
	if v := decision.ValidateAttribution(attr, snap); !v.OK {
		t.Fatalf("红线归因应通过校验，得到 %+v", v)
	}

	// 无宪章单位：Redlines 退回空 map（向后兼容）。
	bare := &unit.Record{ID: "u2", Personality: unit.Personality{Aggression: 0.9}}
	bareSnap, _ := service.buildDecisionAttributionContext(context.Background(), state, bare)
	if len(bareSnap.Redlines) != 0 {
		t.Fatalf("无宪章单位 Redlines 应为空，得到 %d", len(bareSnap.Redlines))
	}
}

func TestAttributionAutoDegrade(t *testing.T) {
	attributionTotal.Store(0)
	attributionOOC.Store(0)
	attributionDegraded.Store(false)
	t.Cleanup(func() {
		attributionTotal.Store(0)
		attributionOOC.Store(0)
		attributionDegraded.Store(false)
	})

	// 全 OOC 但样本未达 minSample → 不降级。
	for i := 0; i < oocDegradeMinSample-1; i++ {
		recordAttributionResult(false)
	}
	if AttributionDegraded() {
		t.Fatalf("样本未达 %d 时不应降级", oocDegradeMinSample)
	}

	// 越过最小样本且 OOC 率超阈 → 自动降级（闩锁）。
	recordAttributionResult(false)
	recordAttributionResult(false)
	if !AttributionDegraded() {
		t.Fatalf("OOC 率超阈应自动降级")
	}

	// 降级后强制被抑制。
	enforced := &Service{attributionEnforced: true}
	if enforced.enforcementActive() {
		t.Fatalf("已降级时强制应被抑制")
	}

	// 可重新武装。
	ResetAttributionDegrade()
	if AttributionDegraded() || !enforced.enforcementActive() {
		t.Fatalf("ResetAttributionDegrade 后应恢复强制")
	}
}

func TestEnforcementActive(t *testing.T) {
	attributionDegraded.Store(false)
	t.Cleanup(func() { attributionDegraded.Store(false) })

	if (&Service{attributionEnforced: false}).enforcementActive() {
		t.Fatalf("未开启强制应非 active")
	}
	if !(&Service{attributionEnforced: true}).enforcementActive() {
		t.Fatalf("开启强制且未降级应 active")
	}
	attributionDegraded.Store(true)
	if (&Service{attributionEnforced: true}).enforcementActive() {
		t.Fatalf("全局降级时应非 active")
	}
}

func TestAttributionEnforcementToggle(t *testing.T) {
	service := &Service{}
	if service.attributionEnforced {
		t.Fatalf("强制模式默认应关闭")
	}
	service.SetAttributionEnforcement(true)
	if !service.attributionEnforced {
		t.Fatalf("SetAttributionEnforcement(true) 应开启强制")
	}
	total, ooc := service.AttributionStats()
	if total != 0 || ooc != 0 {
		t.Fatalf("初始计数应为 0，得到 %d/%d", total, ooc)
	}
}
