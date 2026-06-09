package session

// 文件说明：命运后果闸 M3 逐条门槛 + §8 OOC 双口径收紧 + 跨玩家 consent 字面 choices 的单元/集成测试。
// 覆盖：① 层3 三条 AND（缺任一降层2）、② 层2 care/天数门（含离乡/失盟更严 care≥50）、
// ③ 玩家 OOC 率高→一致性收紧态（含 flag 关零行为、样本不足不抖动、回落解除）、④ ReasonCrossConsentPending 字面 choices。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/analytics"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
)

// ---- ① §4.3 M3 层3 三条 AND（缺任一条即降层2）----

func TestFatePenaltyCapPrecise_Layer3ThreeAnd(t *testing.T) {
	const code = events.ReasonCombatDown // 不可逆类（非离乡/失盟，层2 走默认 care≥40 门）
	// 三条 AND 全满足 + consent 过 → 层3。
	if got := fatePenaltyCapPrecise(code, 70, 7, true, true); got != 3 {
		t.Fatalf("care≥70∧days≥7∧priorLayer2∧consent 全满足应到层3，得 %d", got)
	}
	// 缺 care（<70）→ 降层2。
	if got := fatePenaltyCapPrecise(code, 69, 7, true, true); got != 2 {
		t.Fatalf("care<70 应降层2，得 %d", got)
	}
	// 缺 days（<7）→ 降层2（care 仍≥40 故落层2，非层1）。
	if got := fatePenaltyCapPrecise(code, 70, 6, true, true); got != 2 {
		t.Fatalf("days<7 应降层2，得 %d", got)
	}
	// 缺 priorLayer2 → 降层2。
	if got := fatePenaltyCapPrecise(code, 70, 7, false, true); got != 2 {
		t.Fatalf("无 priorLayer2 应降层2，得 %d", got)
	}
	// 缺 consent（本地命运后果默认 false）→ 降层2。
	if got := fatePenaltyCapPrecise(code, 70, 7, true, false); got != 2 {
		t.Fatalf("未过 RequiresConsent 应降层2，得 %d", got)
	}
}

// ---- ② §4.3 M3 层2 care/天数门（含离乡/失盟更严 care≥50）----

func TestFatePenaltyCapPrecise_Layer2Gates(t *testing.T) {
	const code = events.ReasonCombatDown // 非离乡/失盟，层2 走默认 care≥40
	// care≥40 单独即解锁层2（days=0）。
	if got := fatePenaltyCapPrecise(code, 40, 0, false, false); got != 2 {
		t.Fatalf("care≥40 应解锁层2，得 %d", got)
	}
	// days≥3 单独即解锁层2（care=0）。
	if got := fatePenaltyCapPrecise(code, 0, 3, false, false); got != 2 {
		t.Fatalf("days≥3 应解锁层2，得 %d", got)
	}
	// 两条都不满足（care<40 且 days<3）→ 层1。
	if got := fatePenaltyCapPrecise(code, 39, 2, false, false); got != 1 {
		t.Fatalf("care<40∧days<3 应落层1，得 %d", got)
	}
	// 离乡/失盟类：care=40 不够（门抬到 50），days<3 → 层1。
	exile := events.ReasonRelationBetray // fateReasonIsExileOrAllianceLoss 命中
	if !fateReasonIsExileOrAllianceLoss(exile) {
		t.Fatalf("前置：ReasonRelationBetray 应判离乡/失盟类")
	}
	if got := fatePenaltyCapPrecise(exile, 49, 2, false, false); got != 1 {
		t.Fatalf("离乡/失盟类 care=49(<50)∧days<3 应落层1，得 %d", got)
	}
	// 离乡/失盟类：care≥50 才解锁层2。
	if got := fatePenaltyCapPrecise(exile, 50, 0, false, false); got != 2 {
		t.Fatalf("离乡/失盟类 care≥50 应解锁层2，得 %d", got)
	}
	// 离乡/失盟类即使 care 不够，days≥3 仍解锁层2（天数门不变）。
	if got := fatePenaltyCapPrecise(exile, 0, 3, false, false); got != 2 {
		t.Fatalf("离乡/失盟类 days≥3 应解锁层2，得 %d", got)
	}
}

// gateFateConsequencesByPenaltyPrecise 把不可逆重击按 cap 降级，并报告是否落地层2。
func TestGateFateConsequencesByPenaltyPrecise_DegradeAndLandedFlag(t *testing.T) {
	neg := []fateConsequence{{Field: status.FieldMorale, Delta: -0.50, ReasonCode: events.ReasonEmotionTrauma}}
	// 低牵挂+陪伴浅（cap=1）：层3 候选(-0.50)降到层1 幅度，且未落地层2。
	gated, landed := gateFateConsequencesByPenaltyPrecise(neg, events.ReasonCombatDown, 0, 0, false, false)
	if gated[0].Delta != -fatePenaltyMagnitudeForLayer(1) {
		t.Fatalf("低牵挂应降到层1 幅度 %.3f，得 %.3f", -fatePenaltyMagnitudeForLayer(1), gated[0].Delta)
	}
	if landed {
		t.Fatalf("降到层1 不应报告落地层2")
	}
	// 高牵挂但缺 consent（cap=2）：层3 候选降到层2 幅度，且报告落地层2。
	gated2, landed2 := gateFateConsequencesByPenaltyPrecise(neg, events.ReasonCombatDown, 80, 8, true, false)
	if gated2[0].Delta != -fatePenaltyMagnitudeForLayer(2) {
		t.Fatalf("缺 consent 应降到层2 幅度 %.3f，得 %.3f", -fatePenaltyMagnitudeForLayer(2), gated2[0].Delta)
	}
	if !landed2 {
		t.Fatalf("落地层2 应报告 landedLayer2=true")
	}
	// 正向后果不进闸（原样、不报告落地层2）。
	pos := []fateConsequence{{Field: status.FieldMorale, Delta: +0.05, ReasonCode: events.ReasonEmotionReward}}
	gated3, landed3 := gateFateConsequencesByPenaltyPrecise(pos, events.ReasonCombatDown, 0, 0, false, false)
	if gated3[0].Delta != 0.05 || landed3 {
		t.Fatalf("正向后果应原样且不落地层2，得 delta=%.3f landed=%v", gated3[0].Delta, landed3)
	}
}

// ---- ③ §8 OOC 双口径：玩家高 OOC → 一致性收紧（flag 关零行为 / 样本不足不抖动 / 回落解除）----

func TestRefreshConsistencyTightening(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()
	// 测试隔离：起始解除收紧态（进程级 latch 跨测试共享）。
	SetConsistencyTightened(false)
	t.Cleanup(func() { SetConsistencyTightened(false) })

	emit := func(name string) {
		if err := analytics.Emit(ctx, db, analytics.Event{Stage: analytics.StageRetention, Name: name, SessionID: "s1"}); err != nil {
			t.Fatalf("emit %s: %v", name, err)
		}
	}

	// flag 关：即便堆高 OOC 也零行为（永不 latch）。
	for i := 0; i < 10; i++ {
		emit(analytics.EventFateReactOoc)
	}
	for i := 0; i < 10; i++ {
		emit(analytics.EventFateReactExpected)
	}
	if rate, n := service.RefreshConsistencyTightening(ctx); rate != 0 || n != 0 {
		t.Fatalf("flag 关时应零行为（rate=0,n=0），得 rate=%.3f n=%d", rate, n)
	}
	if ConsistencyTightened() {
		t.Fatalf("flag 关时绝不应 latch 收紧")
	}

	// 开 flag。
	t.Setenv("QUNXIANG_CONSISTENCY_TIGHTEN", "on")

	// 当前 20 条里 10 条 ooc → rate=0.5 > 0.15，样本=20≥20 → 应收紧。
	rate, n := service.RefreshConsistencyTightening(ctx)
	if n < playerOOCMinSample {
		t.Fatalf("样本应≥%d，得 %d", playerOOCMinSample, n)
	}
	if rate <= playerOOCTightenThreshold {
		t.Fatalf("OOC 率应 >%.2f，得 %.3f", playerOOCTightenThreshold, rate)
	}
	if !ConsistencyTightened() {
		t.Fatalf("高玩家 OOC 应 latch 一致性收紧")
	}
	// 收紧态下惊喜上限被压到 2、破圈暂缓。
	if TightenedSurpriseCap() != 2 {
		t.Fatalf("收紧态下 TightenedSurpriseCap 应为 2，得 %d", TightenedSurpriseCap())
	}
	if !serendipityPausedByTightening() {
		t.Fatalf("收紧态下破圈应暂缓")
	}

	// 回落：再灌大量「意料之中」把 OOC 率压到阈下 → 解除收紧。
	for i := 0; i < 200; i++ {
		emit(analytics.EventFateReactExpected)
	}
	rate2, _ := service.RefreshConsistencyTightening(ctx)
	if rate2 > playerOOCTightenThreshold {
		t.Fatalf("回落后 OOC 率应≤%.2f，得 %.3f", playerOOCTightenThreshold, rate2)
	}
	if ConsistencyTightened() {
		t.Fatalf("OOC 率回落后应解除收紧态")
	}
	if TightenedSurpriseCap() != 3 {
		t.Fatalf("解除收紧后 TightenedSurpriseCap 应恢复 3，得 %d", TightenedSurpriseCap())
	}
}

// 样本不足（<playerOOCMinSample）时即便 OOC 率高也不 latch（防小样本抖动）。
func TestRefreshConsistencyTightening_MinSample(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()
	SetConsistencyTightened(false)
	t.Cleanup(func() { SetConsistencyTightened(false) })
	t.Setenv("QUNXIANG_CONSISTENCY_TIGHTEN", "on")

	// 仅 3 条 ooc（rate=1.0 但样本=3<20）→ 不 latch。
	for i := 0; i < 3; i++ {
		if err := analytics.Emit(ctx, db, analytics.Event{Stage: analytics.StageRetention, Name: analytics.EventFateReactOoc, SessionID: "s1"}); err != nil {
			t.Fatalf("emit: %v", err)
		}
	}
	if _, n := service.RefreshConsistencyTightening(ctx); n >= playerOOCMinSample {
		t.Fatalf("样本应 <%d，得 %d", playerOOCMinSample, n)
	}
	if ConsistencyTightened() {
		t.Fatalf("样本不足时绝不应 latch 收紧")
	}
}

// FateOOCDualChannel 交叉暴露玩家主观与机器两口径。
func TestFateOOCDualChannel(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()
	SetConsistencyTightened(false)
	t.Cleanup(func() { SetConsistencyTightened(false) })

	// 玩家口径：2 ooc / 8 总 = 0.25。
	for i := 0; i < 2; i++ {
		_ = analytics.Emit(ctx, db, analytics.Event{Stage: analytics.StageRetention, Name: analytics.EventFateReactOoc, SessionID: "s1"})
	}
	for i := 0; i < 6; i++ {
		_ = analytics.Emit(ctx, db, analytics.Event{Stage: analytics.StageRetention, Name: analytics.EventFateReactExpected, SessionID: "s1"})
	}
	dc := service.FateOOCDualChannel(ctx)
	if dc.PlayerSamples != 8 {
		t.Fatalf("玩家样本应为 8，得 %d", dc.PlayerSamples)
	}
	if dc.PlayerOOCRate < 0.24 || dc.PlayerOOCRate > 0.26 {
		t.Fatalf("玩家 OOC 率应≈0.25，得 %.3f", dc.PlayerOOCRate)
	}
	// 机器口径恒可读（进程内存，不依赖本测试有无样本，仅断言被填充且范围合法）。
	if dc.MachineOOCRate < 0 || dc.MachineOOCRate > 1 {
		t.Fatalf("机器 OOC 率应在 [0,1]，得 %.3f", dc.MachineOOCRate)
	}
	if dc.ConsistencyTightened != ConsistencyTightened() {
		t.Fatalf("交叉暴露的收紧态应与全局一致")
	}
}

// ---- ④ §2.5 ReasonCrossConsentPending 字面 choices（还手/求和/认账）----

func TestBuildFateChoices_CrossConsentPending(t *testing.T) {
	choices := buildFateChoices(FateEvent{ReasonCode: events.ReasonCrossConsentPending}, "")
	if len(choices) != 3 {
		t.Fatalf("跨玩家回应卡应有 3 个字面选项，得 %d：%+v", len(choices), choices)
	}
	want := []struct{ id, class string }{
		{"strike_back", "urge"},    // 还手
		{"seek_peace", "let_her"},  // 求和
		{"swallow", "acknowledge"}, // 认账
	}
	for i, w := range want {
		if choices[i].ID != w.id || choices[i].ResolveClass != w.class {
			t.Fatalf("第 %d 个选项应为 %s(%s)，得 %s(%s)", i, w.id, w.class, choices[i].ID, choices[i].ResolveClass)
		}
		if choices[i].Label == "" {
			t.Fatalf("第 %d 个选项应有字面文案", i)
		}
	}
	// 折算：玩家传字面 id 应映射回基础后果类。
	if got := resolveFateChoiceClass("strike_back", choices); got != "urge" {
		t.Fatalf("strike_back 应折算为 urge，得 %s", got)
	}
	if got := resolveFateChoiceClass("seek_peace", choices); got != "let_her" {
		t.Fatalf("seek_peace 应折算为 let_her，得 %s", got)
	}
	// 跨玩家回应卡不应再回落通用三键（urge/let_her/acknowledge 的旧 id）。
	if choices[0].ID == "urge" {
		t.Fatalf("跨玩家回应卡不应回落通用三键，应为字面 strike_back/seek_peace/swallow")
	}
}
