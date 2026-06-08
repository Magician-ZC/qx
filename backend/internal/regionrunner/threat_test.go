package regionrunner

// 文件说明：region-runner PvE 接入测试（PvE-1 shadow）。验证 rollThreat 确定性+分布、decision.Router 接入
// （HP 危急撤退 / 否则遭遇）、flag/HOT 门控、shadow 只计遥测不改单位、handler 注入调用、以及威胁短路日常 ambient。
// 复用 regionrunner_test.go 的 newRunner/seedActiveUnit/unitHunger helper。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/unit"
)

// firstTick 找首个对 (sid,uid) roll 命中(hit=true)或未命中(hit=false)的 tick（确定性，故可定位）。
func firstTick(sid, uid string, hit bool) int64 {
	for t := int64(1); t < 1_000_000; t++ {
		if rollThreat(sid, uid, t) == hit {
			return t
		}
	}
	return -1
}

func hotJob() *agentqueue.DecisionJob {
	return &agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1"}
}

func healthyRec() unit.Record {
	var rec unit.Record
	rec.ID = "u1"
	rec.Status.HP = 100
	rec.Status.Hunger = 80
	return rec
}

func TestRollThreatDeterministicAndDistribution(t *testing.T) {
	for _, tk := range []int64{1, 42, 9999} { // 确定性：同输入必同结果
		if rollThreat("s1", "u1", tk) != rollThreat("s1", "u1", tk) {
			t.Fatalf("rollThreat 应确定性，tick=%d 两次不一致", tk)
		}
	}
	hits := 0 // 分布：~3% 命中（粗区间，防概率常量误改一个数量级）
	const n = 20000
	for tk := int64(0); tk < n; tk++ {
		if rollThreat("sess", "unit", tk) {
			hits++
		}
	}
	if rate := float64(hits) / float64(n); rate < 0.015 || rate > 0.05 {
		t.Fatalf("命中率应 ~3%%（threatChancePerMille=30），实测 %.4f", rate)
	}
}

func TestSituationFromRecord(t *testing.T) {
	var rec unit.Record
	rec.ID = "u1"
	rec.Status.HP = 40
	rec.Status.Hunger = 70
	s := situationFromRecord(rec, 5)
	if !s.StrategicFork || s.HP != 40 || s.HPMax != hpMaxForThreat || s.Hunger != 70 || s.UnitID != "u1" {
		t.Fatalf("situationFromRecord 字段不符: %+v", s)
	}
}

func TestMaybeEncounterThreat_DisabledNoRoll(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1}) // Threats 默认 false
	hit := firstTick("s1", "u1", true)
	if handled, _ := r.maybeEncounterThreat(ctx, hotJob(), healthyRec(), scheduler.TierHot, hit); handled {
		t.Fatalf("Threats 关时不应触发威胁")
	}
	if r.Stats()["threats_rolled"].(int64) != 0 {
		t.Fatalf("关时不应 roll")
	}
}

func TestMaybeEncounterThreat_NonHotNoRoll(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	hit := firstTick("s1", "u1", true)
	for _, tier := range []scheduler.Tier{scheduler.TierWarm, scheduler.TierCold} {
		if handled, _ := r.maybeEncounterThreat(ctx, hotJob(), healthyRec(), tier, hit); handled {
			t.Fatalf("非 HOT(%s) 不应 roll 威胁", tier)
		}
	}
}

func TestMaybeEncounterThreat_MissNoEncounter(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	miss := firstTick("s1", "u1", false)
	if handled, _ := r.maybeEncounterThreat(ctx, hotJob(), healthyRec(), scheduler.TierHot, miss); handled {
		t.Fatalf("未命中 tick 不应触发遭遇")
	}
	if r.Stats()["threats_rolled"].(int64) != 0 {
		t.Fatalf("未命中不应计 rolled")
	}
}

func TestMaybeEncounterThreat_FleeWhenCritical(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	hit := firstTick("s1", "u1", true)
	rec := healthyRec()
	rec.Status.HP = 10 // 10/100=0.1 < 0.25 → L1 反射撤退保命
	handled, tier := r.maybeEncounterThreat(ctx, hotJob(), rec, scheduler.TierHot, hit)
	if !handled || tier != scheduler.TierHot {
		t.Fatalf("HP 危急撞威胁应 handled+HOT，得到 handled=%v tier=%s", handled, tier)
	}
	if r.Stats()["threats_fled"].(int64) != 1 || r.Stats()["threats_encountered"].(int64) != 0 {
		t.Fatalf("HP 危急应撤退(fled=1,encountered=0)，得到 fled=%v enc=%v", r.Stats()["threats_fled"], r.Stats()["threats_encountered"])
	}
}

func TestMaybeEncounterThreat_ShadowEncounter(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true}) // handler nil → shadow
	hit := firstTick("s1", "u1", true)
	if handled, _ := r.maybeEncounterThreat(ctx, hotJob(), healthyRec(), scheduler.TierHot, hit); !handled {
		t.Fatalf("命中且 HP 健康应 handled（遭遇）")
	}
	if r.Stats()["threats_rolled"].(int64) != 1 || r.Stats()["threats_encountered"].(int64) != 1 {
		t.Fatalf("shadow 应计 rolled=1 encountered=1")
	}
	if r.Stats()["encounter_errors"].(int64) != 0 {
		t.Fatalf("shadow 不真触发，不应有 encounter_errors")
	}
}

func TestMaybeEncounterThreat_HandlerCalled(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	var gotSID, gotUID string
	var calls int32
	r.SetThreatHandler(func(_ context.Context, sid, uid string) error {
		atomic.AddInt32(&calls, 1)
		gotSID, gotUID = sid, uid
		return nil
	})
	hit := firstTick("s1", "u1", true)
	if handled, _ := r.maybeEncounterThreat(ctx, hotJob(), healthyRec(), scheduler.TierHot, hit); !handled {
		t.Fatalf("应触发遭遇")
	}
	if atomic.LoadInt32(&calls) != 1 || gotSID != "s1" || gotUID != "u1" {
		t.Fatalf("handler 应以 (s1,u1) 调一次，得到 calls=%d sid=%s uid=%s", calls, gotSID, gotUID)
	}
}

// TestApplyL1ThreatShortCircuitsAmbient 端到端：HOT 饿单位在命中威胁的 tick 被处理时，威胁短路日常 ambient——
// 不觅食（hunger 不变），证明 PvE 接入主链路、优先于 ambient。
func TestApplyL1ThreatShortCircuitsAmbient(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true, Threats: true})
	hit := firstTick("s1", "u1", true)
	r.withClock(func() time.Time { return time.Unix(hit, 0).UTC() }) // 时钟对齐命中 tick
	tick := r.currentTick()
	if tick != hit {
		t.Fatalf("时钟应对齐命中 tick：want %d got %d", hit, tick)
	}
	seedActiveUnit(t, r, ctx, "u1", 20, unit.LifeStateActive) // 饿 → 反射本会觅食（HP=100 健康，不撤退）
	if err := unit.NewRepository(r.db).TouchLastActiveTick(ctx, "u1", tick); err != nil {
		t.Fatalf("touch: %v", err)
	}
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	if _, err := r.processOne(ctx); err != nil {
		t.Fatalf("processOne: %v", err)
	}
	if h := unitHunger(t, r, ctx, "u1"); h != 20 {
		t.Fatalf("威胁应短路日常觅食，hunger 应仍为 20，得到 %d", h)
	}
	if r.Stats()["threats_rolled"].(int64) != 1 || r.Stats()["foraged"].(int64) != 0 {
		t.Fatalf("应计 threats_rolled=1 且未觅食，得到 rolled=%v foraged=%v", r.Stats()["threats_rolled"], r.Stats()["foraged"])
	}
}
