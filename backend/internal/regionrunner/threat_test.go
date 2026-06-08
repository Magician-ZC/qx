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

// firstTick 找首个对 (sid,uid) 在零锚(密度0)阈值下命中(hit=true)/未命中(hit=false)的 tick（确定性，故可定位）。
// 零锚阈值 = threatSpawnPerMille(0)，即无 provider（density 0）时 maybeEncounterThreat 用的阈值。
func firstTick(sid, uid string, hit bool) int64 {
	pm := threatSpawnPerMille(0)
	for t := int64(1); t < 1_000_000; t++ {
		if (threatRoll1000(sid, uid, t) < pm) == hit {
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

func TestThreatRoll1000DeterministicAndUniform(t *testing.T) {
	for _, tk := range []int64{1, 42, 9999} { // 确定性：同输入必同结果
		if threatRoll1000("s1", "u1", tk) != threatRoll1000("s1", "u1", tk) {
			t.Fatalf("threatRoll1000 应确定性，tick=%d 两次不一致", tk)
		}
	}
	const n = 20000 // 均匀性：取值落 [0,1000)、均值 ~500（粗检防哈希/取模错）
	sum := 0
	for tk := int64(0); tk < n; tk++ {
		v := threatRoll1000("sess", "unit", tk)
		if v < 0 || v >= 1000 {
			t.Fatalf("threatRoll1000 应落 [0,1000)，得 %d", v)
		}
		sum += v
	}
	if mean := float64(sum) / n; mean < 450 || mean > 550 {
		t.Fatalf("均匀抽样均值应 ~500，实测 %.1f", mean)
	}
}

func TestThreatSpawnPerMille_AnchorWeighted(t *testing.T) {
	zero := threatSpawnPerMille(0)
	full := threatSpawnPerMille(1)
	if zero < threatFloorPerMille {
		t.Fatalf("零锚应≥破圈下限 %d，得 %d", threatFloorPerMille, zero)
	}
	if full <= zero {
		t.Fatalf("满锚应比零锚更易撞威胁（扎堆她在乎处）：full=%d zero=%d", full, zero)
	}
	if full > threatMaxPerMille {
		t.Fatalf("应夹上限 %d，得 %d", threatMaxPerMille, full)
	}
	if mid := threatSpawnPerMille(0.5); mid < zero || mid > full {
		t.Fatalf("应随锚密度单调：mid=%d 不在 [%d,%d]", mid, zero, full)
	}
	if threatSpawnPerMille(2) != full || threatSpawnPerMille(-1) != zero {
		t.Fatalf("密度应夹 [0,1]")
	}
}

// TestMaybeEncounterThreat_AnchorClustering 是 PvE-4 核心：同一 tick（draw 落在零锚阈值之上、满锚阈值之下），
// 高密度（在乎得多）单位撞威胁、零锚单位不撞——证「威胁天然扎堆她在乎的地方」。
func TestMaybeEncounterThreat_AnchorClustering(t *testing.T) {
	lo, hi := threatSpawnPerMille(0), threatSpawnPerMille(1)
	var tick int64 = -1
	for tk := int64(1); tk < 1_000_000; tk++ {
		if d := threatRoll1000("s1", "u1", tk); d >= lo && d < hi {
			tick = tk
			break
		}
	}
	if tick < 0 {
		t.Fatal("找不到落在 [零锚阈值, 满锚阈值) 区间的 tick")
	}

	rHi, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
	rHi.SetAnchorDensityProvider(func(context.Context, string) float64 { return 1.0 }) // 在乎得多
	if handled, _ := rHi.maybeEncounterThreat(ctx, hotJob(), healthyRec(), scheduler.TierHot, tick); !handled {
		t.Fatalf("高锚密度单位在该 tick 应撞威胁（draw<%d）", hi)
	}

	rLo, ctx2 := newRunner(t, Config{TickSeconds: 1, Threats: true}) // 无 provider → 密度 0
	if handled, _ := rLo.maybeEncounterThreat(ctx2, hotJob(), healthyRec(), scheduler.TierHot, tick); handled {
		t.Fatalf("零锚单位在该 tick 不应撞威胁（draw≥%d）——威胁应扎堆在乎多的人身上", lo)
	}
}

// TestMaybeEncounterThreat_GateEquivalence 守护「省 ~92% 密度查询」的优化门控（draw≥max 短路 / draw<floor 必命中 /
// [floor,max) 才查密度判定）与朴素 `draw < threatSpawnPerMille(density)` 在大量 tick 上**逐结果等价**——
// 防优化引入 off-by-one（如 draw>max 写成 draw≥max 错位、漏掉 draw<floor 的破圈命中）。覆盖 draw 全程含 floor/max 边界。
func TestMaybeEncounterThreat_GateEquivalence(t *testing.T) {
	for _, density := range []float64{0, 0.5, 1.0} {
		r, ctx := newRunner(t, Config{TickSeconds: 1, Threats: true})
		r.SetAnchorDensityProvider(func(context.Context, string) float64 { return density })
		pm := threatSpawnPerMille(density)
		for tk := int64(1); tk <= 4000; tk++ {
			want := threatRoll1000("s1", "u1", tk) < pm // 朴素判定
			got, _ := r.maybeEncounterThreat(ctx, hotJob(), healthyRec(), scheduler.TierHot, tk)
			if got != want {
				t.Fatalf("density=%.1f tick=%d：优化门控=%v 朴素=%v 不等价（draw=%d pm=%d）",
					density, tk, got, want, threatRoll1000("s1", "u1", tk), pm)
			}
		}
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
