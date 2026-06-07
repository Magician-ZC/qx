package regionrunner

// 文件说明：region-runner HOT-LLM 决策测试（M7.3-real-3）。用 fake LLM 验证 HOT 门控、LLM 动作落地、
// 失败/非 HOT/未注入/预算耗尽四类回退反射，以及进程级成本闸 latch。复用 regionrunner_test.go 的 newRunner/seed helper。

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/unit"
)

// fakeAmbientLLM 是可控的离线 LLM 桩：返回固定 action 或固定 err，计调用次数，并记录传入 ctx 是否带 deadline（验证整体超时套用）。
type fakeAmbientLLM struct {
	action      string
	err         error
	calls       int32
	sawDeadline bool // 同步调用、单测内单线程读写，无竞态
}

func (f *fakeAmbientLLM) GenerateJSON(ctx context.Context, req ai.CompletionRequest) (ai.CompletionResult, error) {
	atomic.AddInt32(&f.calls, 1)
	_, f.sawDeadline = ctx.Deadline()
	if f.err != nil {
		return ai.CompletionResult{Provider: "fake", Model: "fake"}, f.err
	}
	out, _ := json.Marshal(map[string]string{"action": f.action})
	return ai.CompletionResult{Output: out, Provider: "fake", Model: "fake"}, nil
}

func freeCost(string, string, int, int) float64 { return 0 }

func TestParseAmbientAction(t *testing.T) {
	if act, err := parseAmbientAction([]byte(`{"action":"socialize"}`)); err != nil || act != actSocialize {
		t.Fatalf("合法动作应解析为 actSocialize，得到 %s err=%v", act, err)
	}
	if _, err := parseAmbientAction([]byte(`{"action":"betray"}`)); err == nil {
		t.Fatalf("未注册动作应报错（schema 外兜底）")
	}
	if _, err := parseAmbientAction([]byte(`not json`)); err == nil {
		t.Fatalf("非法 JSON 应报错")
	}
}

func TestChooseAmbientAction_HotUsesLLM(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	fake := &fakeAmbientLLM{action: "socialize"}
	r.SetLLMClient(fake, freeCost, 1000)

	var rec unit.Record
	rec.Status.Hunger = 80
	rec.Status.Morale = 0.7 // 反射会选 rest；LLM 强制 socialize → 用以区分走了哪条
	if act := r.chooseAmbientAction(ctx, rec, scheduler.TierHot); act != actSocialize {
		t.Fatalf("HOT+LLM 应采用 LLM 的 socialize，得到 %s", act)
	}
	if atomic.LoadInt32(&fake.calls) != 1 || r.Stats()["llm_calls"].(int64) != 1 {
		t.Fatalf("应调用并计一次 LLM 决策")
	}
}

func TestChooseAmbientAction_NonHotUsesReflex(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	fake := &fakeAmbientLLM{action: "socialize"}
	r.SetLLMClient(fake, freeCost, 1000)

	var rec unit.Record
	rec.Status.Hunger = 20 // 反射：饿 → forage
	for _, tier := range []scheduler.Tier{scheduler.TierWarm, scheduler.TierCold} {
		if act := r.chooseAmbientAction(ctx, rec, tier); act != actForage {
			t.Fatalf("非 HOT(%s) 应走反射 forage，得到 %s", tier, act)
		}
	}
	if atomic.LoadInt32(&fake.calls) != 0 {
		t.Fatalf("非 HOT 不应调 LLM，得到 calls=%d", fake.calls)
	}
}

func TestChooseAmbientAction_NoLLMUsesReflex(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true}) // 未 SetLLMClient → llm==nil
	var rec unit.Record
	rec.Status.Hunger = 20
	if act := r.chooseAmbientAction(ctx, rec, scheduler.TierHot); act != actForage {
		t.Fatalf("未注入 LLM 时 HOT 也应走反射，得到 %s", act)
	}
}

func TestChooseAmbientAction_LLMErrorFallsBackToReflex(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	fake := &fakeAmbientLLM{err: errors.New("llm down")}
	r.SetLLMClient(fake, freeCost, 1000)

	var rec unit.Record
	rec.Status.Hunger = 20 // 反射兜底 → forage
	if act := r.chooseAmbientAction(ctx, rec, scheduler.TierHot); act != actForage {
		t.Fatalf("LLM 失败应回退反射 forage，得到 %s", act)
	}
	if atomic.LoadInt32(&fake.calls) != 1 || r.Stats()["llm_fallbacks"].(int64) != 1 {
		t.Fatalf("LLM 失败应计一次 fallback")
	}
	if r.Stats()["llm_calls"].(int64) != 0 {
		t.Fatalf("失败不应计入 llm_calls")
	}
}

func TestChooseAmbientAction_BudgetLatch(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	fake := &fakeAmbientLLM{action: "forage"}
	// 每次 0.5 USD，上限 0.3 → 第一次调用后 spent 0.5>=0.3 即 latch。
	r.SetLLMClient(fake, func(string, string, int, int) float64 { return 0.5 }, 0.3)

	var rec unit.Record
	rec.Status.Hunger = 20
	_ = r.chooseAmbientAction(ctx, rec, scheduler.TierHot) // 第一次：latch 前 → 调 LLM
	if atomic.LoadInt32(&fake.calls) != 1 {
		t.Fatalf("第一次应调 LLM")
	}
	if !r.Stats()["llm_latched"].(bool) {
		t.Fatalf("0.5>=0.3 应已 latch")
	}
	_ = r.chooseAmbientAction(ctx, rec, scheduler.TierHot) // 第二次：latch 后 → 反射，不再调
	if atomic.LoadInt32(&fake.calls) != 1 {
		t.Fatalf("latch 后不应再调 LLM，得到 calls=%d", fake.calls)
	}
}

// TestApplyL1HotUnitUsesLLM 端到端：HOT 单位经 processOne 时用 LLM 选的动作落地（socialize 提升士气），
// 而非反射会选的 rest——证明 LLM 决策真正接入主链路（经饱和短路 + applyAction 落地）。
func TestApplyL1HotUnitUsesLLM(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	fake := &fakeAmbientLLM{action: "socialize"}
	r.SetLLMClient(fake, freeCost, 1000)
	tick := r.currentTick()

	seedUnitMorale(t, r, ctx, "u1", 80, 0.7) // 不饿、士气中 → 反射本会选 rest
	repo := unit.NewRepository(r.db)
	if err := repo.TouchLastActiveTick(ctx, "u1", tick); err != nil { // 设为 HOT（idle gap 0）
		t.Fatalf("touch: %v", err)
	}
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	if _, err := r.processOne(ctx); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	// LLM 选 socialize（morale+0.05）而非反射的 rest（仅动 hunger）。
	if m := unitMorale(t, r, ctx, "u1"); m <= 0.7 {
		t.Fatalf("HOT 单位应采用 LLM 的 socialize 提升士气，得到 %.3f（疑似走了反射 rest）", m)
	}
	if r.Stats()["llm_calls"].(int64) != 1 {
		t.Fatalf("应记一次 LLM 决策")
	}
}

// TestSetLLMClient_NonPositiveBudgetClampsNotUnlimited 守评审 load-bearing #1：非正预算（配置笔误 0/负）绝不能退化成
// 「无上限烧钱」。验证 budget=0 被夹成正默认（1.0 USD）→ 单次 2.0 USD 调用即能 latch（若退化成不限则永不 latch）。
func TestSetLLMClient_NonPositiveBudgetClampsNotUnlimited(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	fake := &fakeAmbientLLM{action: "forage"}
	r.SetLLMClient(fake, func(string, string, int, int) float64 { return 2.0 }, 0) // 0 应被夹到正默认，而非「不限」

	var rec unit.Record
	rec.Status.Hunger = 20
	_ = r.chooseAmbientAction(ctx, rec, scheduler.TierHot) // 1 次调用，成本 2.0 ≥ 夹后上限(1.0) → latch
	if !r.Stats()["llm_latched"].(bool) {
		t.Fatalf("非正预算应被夹为正上限并能 latch（证明未 fail-open 成无上限）")
	}
}

// TestAmbientLLMDecide_BoundsWholeDecisionWithDeadline 守评审 load-bearing #2：整个决策套了带 deadline 的派生 ctx，
// 把跨 provider/endpoint 故障链的总耗时夹在 ambientLLMTimeout 内（防单次决策累积数分钟长占 worker）。
func TestAmbientLLMDecide_BoundsWholeDecisionWithDeadline(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	fake := &fakeAmbientLLM{action: "rest"}
	r.SetLLMClient(fake, freeCost, 1000)

	var rec unit.Record
	rec.Status.Hunger = 80
	rec.Status.Morale = 0.6
	_ = r.chooseAmbientAction(ctx, rec, scheduler.TierHot)
	if !fake.sawDeadline {
		t.Fatalf("整个决策应在带 deadline 的派生 ctx 内执行（base ctx 无 deadline，必须由 ambientLLMDecide 套上）")
	}
}
