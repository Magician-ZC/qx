package regionrunner

// 文件说明：region-runner 骨架的确定性测试（注入式固定时钟 + 真实 SQLite 队列）。
// 手动驱动 schedulePass/processOne 验证全机制；另跑一次真实短循环验证 Run 的优雅启停。

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func newRunner(t *testing.T, cfg Config) (*Runner, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "rr.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r := New(db, cfg, nil).withClock(func() time.Time { return fixed })
	return r, context.Background()
}

func TestSchedulePassEnqueuesDueOnly(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1})
	tick := r.currentTick()

	// 到点的 u1（wake<=tick）、未到点的 u2（wake>tick）。
	_ = agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u1", SessionID: "s1", RegionID: "s1", WakeAtTick: tick - 1})
	_ = agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u2", SessionID: "s1", RegionID: "s1", WakeAtTick: tick + 100})

	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 1 {
		t.Fatalf("仅到点的 u1 应入队，得到 %d", enq)
	}
	// u1 的 wake 应被移除（防重复入队）；u2 仍在。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+1000, 100); len(due) != 1 || due[0].UnitID != "u2" {
		t.Fatalf("u1 wake 应已移除、u2 仍在，得到 %+v", due)
	}
	// 入队的 job 带 session_id（保留期清理键）。
	job, _ := agentqueue.ClaimNextJob(ctx, r.db)
	if job == nil || job.UnitID != "u1" || job.SessionID != "s1" {
		t.Fatalf("应入队 u1 作业且带 session_id，得到 %+v", job)
	}
}

func TestProcessOneShadowCycle(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1})

	// 空队列 → false。
	if worked, err := r.processOne(ctx); err != nil || worked {
		t.Fatalf("空队列应返回 false，得到 worked=%v err=%v", worked, err)
	}

	id, _ := agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: r.currentTick()})
	worked, err := r.processOne(ctx)
	if err != nil || !worked {
		t.Fatalf("应处理一条，得到 worked=%v err=%v", worked, err)
	}
	// 作业应已完成。
	if n, _ := agentqueue.CountJobsByStatus(ctx, r.db, agentqueue.StatusDone); n != 1 {
		t.Fatalf("作业应 done，得到 %d done", n)
	}
	if n, _ := agentqueue.CountJobsByStatus(ctx, r.db, agentqueue.StatusRunning); n != 0 {
		t.Fatalf("不应有 running，得到 %d", n)
	}
	// 单位下次唤醒被重排到未来 tick（WARM），故当前 tick 不到点、不会立即再处理。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", r.currentTick(), 100); len(due) != 0 {
		t.Fatalf("重排的 wake 应在未来 tick、当前不到点，得到 %+v", due)
	}
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", r.currentTick()+100, 100); len(due) != 1 || due[0].UnitID != "u1" {
		t.Fatalf("u1 应被重排到未来，得到 %+v", due)
	}
	_ = id
}

func TestSchedulePassBackpressure(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, MaxInFlight: 2})
	tick := r.currentTick()

	// 先把在途顶满到上限 2（入队 2 条并认领为 running）。
	for i := 0; i < 2; i++ {
		_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "busy", SessionID: "s1", RegionID: "s1"})
		_, _ = agentqueue.ClaimNextJob(ctx, r.db)
	}
	// 再来一个到点单位——背压应拒绝入队。
	_ = agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u1", SessionID: "s1", RegionID: "s1", WakeAtTick: tick})

	enq, err := r.schedulePass(ctx)
	if err != nil {
		t.Fatalf("schedulePass: %v", err)
	}
	if enq != 0 {
		t.Fatalf("在途达上限应背压、不入队，得到 %d", enq)
	}
	if r.Stats()["backpressured"].(int64) == 0 {
		t.Fatalf("应记一次背压")
	}
	// u1 的 wake 未被移除（背压时不该 RemoveWake），下个 tick 还能再试。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick, 100); len(due) != 1 || due[0].UnitID != "u1" {
		t.Fatalf("背压时不应移除 u1 的 wake，得到 %+v", due)
	}
}

// seedActiveUnit 在 runner 的 db 里建一个单位（指定 hunger/lifeState），返回其 ID。
func seedActiveUnit(t *testing.T, r *Runner, ctx context.Context, id string, hunger int, lifeState string) {
	t.Helper()
	repo := unit.NewRepository(r.db)
	rec := unit.BootstrapRecord(1, "s1", "player", "测试单位")
	rec.ID = id
	rec.Status.Hunger = hunger
	rec.Status.LifeState = lifeState
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
}

func unitHunger(t *testing.T, r *Runner, ctx context.Context, id string) int {
	t.Helper()
	rec, err := unit.NewRepository(r.db).GetByID(ctx, id)
	if err != nil {
		t.Fatalf("读单位失败: %v", err)
	}
	return rec.Status.Hunger
}

func TestApplyL1ForageWhenHungry(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()
	seedActiveUnit(t, r, ctx, "u1", 20, unit.LifeStateActive) // 饿（<40）
	if _, err := agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick}); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}

	if worked, err := r.processOne(ctx); err != nil || !worked {
		t.Fatalf("应处理一条: worked=%v err=%v", worked, err)
	}
	// 觅食：hunger 上升（经 Mutator）。
	if h := unitHunger(t, r, ctx, "u1"); h != 20+forageGain {
		t.Fatalf("饿则觅食应补口粮到 %d，得到 %d", 20+forageGain, h)
	}
	if r.Stats()["foraged"].(int64) != 1 {
		t.Fatalf("应记一次觅食")
	}
	// 觅食后 HOT：重排到 currentTick+1。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+1, 10); len(due) != 1 || due[0].Tier != string( /*HOT*/ "hot") {
		t.Fatalf("觅食后应 HOT 重排到下 tick，得到 %+v", due)
	}
	// last_active_tick 被标记为当前 tick。
	la, _, _ := unit.NewRepository(r.db).SchedulingState(ctx, "u1")
	if la != tick {
		t.Fatalf("觅食后应 TouchLastActiveTick=%d，得到 %d", tick, la)
	}
}

// seedUnitMorale 同 seedActiveUnit 但额外设 morale，用于社交/反思决策测试。
func seedUnitMorale(t *testing.T, r *Runner, ctx context.Context, id string, hunger int, morale float64) {
	t.Helper()
	repo := unit.NewRepository(r.db)
	rec := unit.BootstrapRecord(1, "s1", "player", "测试单位")
	rec.ID = id
	rec.Status.Hunger = hunger
	rec.Status.Morale = morale
	rec.Status.LifeState = unit.LifeStateActive
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
}

func unitMorale(t *testing.T, r *Runner, ctx context.Context, id string) float64 {
	t.Helper()
	rec, err := unit.NewRepository(r.db).GetByID(ctx, id)
	if err != nil {
		t.Fatalf("读单位失败: %v", err)
	}
	return rec.Status.Morale
}

func TestDecideAmbientReflex(t *testing.T) {
	mk := func(hunger int, morale float64) unit.Record {
		var rec unit.Record
		rec.Status.Hunger = hunger
		rec.Status.Morale = morale
		return rec
	}
	cases := []struct {
		hunger int
		morale float64
		want   ambientAction
	}{
		{20, 0.7, actForage},    // 饿 → 觅食（优先）
		{80, 0.3, actSocialize}, // 不饿、士气低 → 社交
		{80, 0.9, actReflect},   // 不饿、心满意足 → 反思
		{80, 0.6, actRest},      // 不饿、士气平 → 休息
		// 等号边界：分支用严格不等号（<40 / <0.4 / >0.8），故三个阈值精确值都落入「闭带→后续分支」，钉死契约防未来改 < 为 <=。
		{40, 0.7, actRest}, // hunger==40 不算饿（非<40）→ 落 rest
		{80, 0.4, actRest}, // morale==0.4 不算低（非<0.4）→ 落 rest
		{80, 0.8, actRest}, // morale==0.8 不算满意（非>0.8）→ 落 rest
	}
	for _, c := range cases {
		if got := decideAmbientReflex(mk(c.hunger, c.morale)); got != c.want {
			t.Fatalf("decideAmbientReflex(h=%d,m=%.1f)=%s, want %s", c.hunger, c.morale, got, c.want)
		}
	}
}

func TestApplyL1Socialize(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()
	seedUnitMorale(t, r, ctx, "u1", 80, 0.3) // 不饿、士气低 → 社交
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	_, _ = r.processOne(ctx)
	// 精确断言增益（对齐同包 hunger 测试的精度，能钉死 socializeGain 常量与目标字段；只验「涨了」会放过常量/字段误配）。
	if m := unitMorale(t, r, ctx, "u1"); math.Abs(m-(0.3+socializeGain)) > 1e-9 {
		t.Fatalf("社交应把士气从 0.3 提到 %.2f，得到 %.3f", 0.3+socializeGain, m)
	}
	if r.Stats()["socialized"].(int64) != 1 {
		t.Fatalf("应记一次社交")
	}
	// 社交是主动响应（士气低）→ HOT 重排。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+1, 10); len(due) != 1 || due[0].Tier != "hot" {
		t.Fatalf("社交后应 HOT 重排，得到 %+v", due)
	}
}

func TestApplyL1Reflect(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()
	seedUnitMorale(t, r, ctx, "u1", 80, 0.9) // 不饿、心满意足 → 反思
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	_, _ = r.processOne(ctx)
	// 精确断言：seed 0.9 未饱和（<1.0）→ reflect 应恰好 +reflectGain 到 0.92。
	if m := unitMorale(t, r, ctx, "u1"); math.Abs(m-(0.9+reflectGain)) > 1e-9 {
		t.Fatalf("反思应把士气从 0.9 提到 %.2f，得到 %.3f", 0.9+reflectGain, m)
	}
	if r.Stats()["reflected"].(int64) != 1 {
		t.Fatalf("应记一次反思")
	}
	// 反思是被动 → 降温（last_active 早 → COLD）。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+16, 10); len(due) != 1 || due[0].Tier != "cold" {
		t.Fatalf("反思应降温至 COLD，得到 %+v", due)
	}
}

// TestApplyL1ReflectConvergesNoChurn：反思的 morale 单向上行会停在 clamp 上界并被「饱和空写短路」截住——
// 验证满意单位不会每个 COLD 周期永久空写 AMBIENT_REFLECT（real-4a 评审 load-bearing 修复）。
// seed morale=0.9：morale 阶梯爬到 1.0（恰 5 步真写，reflected==5），此后每步走 settled 短路（reflected 不再增长、morale 恒 1.0）。
// 无修复时 reflected 会随步数无界增长（==steps），事件表/记忆环随之 churn。
func TestApplyL1ReflectConvergesNoChurn(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	// 可变时钟：每步推进 >COLD 间隔(16 tick)，让被动反思重排的 COLD wake 下轮到点、被 schedulePass 再次拉起。
	clk := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	r.withClock(func() time.Time { return clk })

	seedUnitMorale(t, r, ctx, "u1", 80, 0.9) // 不饿、心满意足 → 反思
	_ = agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u1", SessionID: "s1", RegionID: "s1", WakeAtTick: r.currentTick()})

	const steps = 12
	for i := 0; i < steps; i++ {
		if _, err := r.schedulePass(ctx); err != nil { // 到点 wake → job
			t.Fatalf("step %d schedulePass: %v", i, err)
		}
		if _, err := r.processOne(ctx); err != nil {
			t.Fatalf("step %d processOne: %v", i, err)
		}
		clk = clk.Add(20 * time.Second) // 推进 20 tick > COLD 16，使重排的 wake 下轮到点
	}

	if m := unitMorale(t, r, ctx, "u1"); math.Abs(m-1.0) > 1e-9 {
		t.Fatalf("反思应把 morale 收敛到上界 1.0，得到 %.3f", m)
	}
	if got := r.Stats()["reflected"].(int64); got != 5 {
		t.Fatalf("reflected 应有界=5（0.9→1.0 五步真写），得到 %d —— 饱和空写短路失效会随步数无界增长", got)
	}
	if got := r.Stats()["settled"].(int64); got != steps-5 {
		t.Fatalf("到界后应每步走饱和短路，settled 期望 %d，得到 %d", steps-5, got)
	}
}

// TestApplyActionUnknown：applyAction 对未注册动作（real-3 LLM 可能吐出 ambientEffects 之外的字符串）应返回 err、
// 不写状态、不计数。这条兜底防线当前无调用方触发（decideAmbientReflex 只产 4 个已注册动作），故直接单测钉死，
// 与「乐观冲突 applied=false」两条 return 区分开——前者 err!=nil 走 Warn 重排，后者 err==nil 走 conflicted++。
func TestApplyActionUnknown(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	seedActiveUnit(t, r, ctx, "u1", 50, unit.LifeStateActive)
	before := unitHunger(t, r, ctx, "u1")

	active, applied, err := r.applyAction(ctx, "u1", ambientAction("bogus"), r.currentTick())
	if err == nil {
		t.Fatalf("未知动作应返回 err")
	}
	if applied || active {
		t.Fatalf("未知动作不应 applied/active，得到 applied=%v active=%v", applied, active)
	}
	if after := unitHunger(t, r, ctx, "u1"); after != before {
		t.Fatalf("未知动作不应改状态：before=%d after=%d", before, after)
	}
}

// TestApplyL1ConflictNeverClobbers：乐观并发冲突路径的承重保证——region-runner 全链路（applyAmbientL1→applyAction→
// ApplyOptimistic）与一个无条件写者并发时绝不覆盖对方的写（real-3-0 防丢更新硬化，本切片把冲突处理收敛进 applyAction）。
// A=战斗（唯一 HP 写者，每次 HP-1）；B=runner（离线觅食/休息，只写 hunger/morale）。不变量：A 的每次 HP 扣减都不被 B 的
// status_json 整块回写覆盖 → 终值 HP==100-aWrites。若重构破坏「冲突→退避不写」（误用无条件 Save / 冲突仍写），B 会用
// stale HP 覆盖 A，终值偏高、测试红。冲突计数本身随机（取决于交错），但此不变量恒成立，故确定性可断言。
func TestApplyL1ConflictNeverClobbers(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	repo := unit.NewRepository(r.db)
	rec := unit.BootstrapRecord(1, "s1", "player", "测试单位")
	rec.ID = "u1"
	rec.Status.HP = 100
	rec.Status.Hunger = 20 // 饿 → 觅食（B 真写 hunger，绝不写 HP）
	rec.Status.LifeState = unit.LifeStateActive
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const aWrites = 40
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { // A：战斗——无条件 Save，每次 HP-1。
		defer wg.Done()
		for i := 0; i < aWrites; i++ {
			cur, err := repo.GetByID(ctx, "u1")
			if err != nil {
				t.Errorf("A get: %v", err)
				return
			}
			cur.Status.HP -= 1
			if err := repo.Save(ctx, cur); err != nil {
				t.Errorf("A save: %v", err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() { // B：runner——并发处理 u1 的离线作业，冲突时退避。
		defer wg.Done()
		for i := 0; i < aWrites*3; i++ {
			if _, err := agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: r.currentTick()}); err != nil {
				t.Errorf("B enqueue: %v", err)
				return
			}
			if _, err := r.processOne(ctx); err != nil {
				t.Errorf("B processOne: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	final, err := repo.GetByID(ctx, "u1")
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if final.Status.HP != 100-aWrites {
		t.Fatalf("A 的 HP 扣减不应被 region-runner 覆盖：期望 %d，实际 %d（乐观冲突退避失效）", 100-aWrites, final.Status.HP)
	}
	worked := r.Stats()["foraged"].(int64) + r.Stats()["rested"].(int64) + r.Stats()["socialized"].(int64) + r.Stats()["reflected"].(int64)
	if worked == 0 {
		t.Fatalf("runner 应至少成功应用一次动作（否则不变量空洞），动作计数全 0")
	}
}

func TestApplyL1RestWhenFull(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()
	seedActiveUnit(t, r, ctx, "u1", 80, unit.LifeStateActive) // 不饿
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	_, _ = r.processOne(ctx)
	if h := unitHunger(t, r, ctx, "u1"); h != 80-restConsume {
		t.Fatalf("不饿则休息缓慢消耗到 %d，得到 %d", 80-restConsume, h)
	}
	if r.Stats()["rested"].(int64) != 1 {
		t.Fatalf("应记一次休息")
	}
	// 休息且 last_active 早（bootstrap 0，currentTick 巨大）→ 降温到 COLD（+16）。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+16, 10); len(due) != 1 || due[0].Tier != "cold" {
		t.Fatalf("休息应降温至 COLD，得到 %+v", due)
	}
}

func TestApplyL1DropsDeadUnit(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()
	seedActiveUnit(t, r, ctx, "u1", 20, unit.LifeStateDead) // 已逝
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	_, _ = r.processOne(ctx)
	// 逝者不重排、不觅食。
	if h := unitHunger(t, r, ctx, "u1"); h != 20 {
		t.Fatalf("逝者不应被觅食改动，得到 %d", h)
	}
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+1000, 10); len(due) != 0 {
		t.Fatalf("逝者不应被重排，得到 %+v", due)
	}
	if r.Stats()["dropped"].(int64) != 1 {
		t.Fatalf("应记一次 dropped")
	}
}

func TestApplyL1TransientStateRescheduledNotDropped(t *testing.T) {
	// 暂态（恢复中）单位不应像逝者那样永久 drop——应 COLD 低频回查，恢复后继续日常（否则永久脱离模拟）。
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()
	seedActiveUnit(t, r, ctx, "u1", 20, unit.LifeStateRecovering)
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	_, _ = r.processOne(ctx)
	// 不觅食（暂态不动单位）。
	if h := unitHunger(t, r, ctx, "u1"); h != 20 {
		t.Fatalf("恢复中单位不应被觅食改动，得到 %d", h)
	}
	// 但应被 COLD 重排（回查），而非 drop。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+16, 10); len(due) != 1 || due[0].Tier != "cold" {
		t.Fatalf("恢复中单位应 COLD 回查重排（不 drop），得到 %+v", due)
	}
	if r.Stats()["dropped"].(int64) != 0 {
		t.Fatalf("暂态不应计 dropped")
	}
}

func TestApplyL1DefersToBattle(t *testing.T) {
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	r.SetExecutionGuard(func(string) bool { return true }) // 该会话在聚焦战斗中
	tick := r.currentTick()
	seedActiveUnit(t, r, ctx, "u1", 20, unit.LifeStateActive)
	_, _ = agentqueue.EnqueueJob(ctx, r.db, agentqueue.DecisionJob{UnitID: "u1", SessionID: "s1", RegionID: "s1", Tick: tick})

	_, _ = r.processOne(ctx)
	// 让位战斗：不觅食（hunger 不变）、HOT 稍后重试。
	if h := unitHunger(t, r, ctx, "u1"); h != 20 {
		t.Fatalf("让位战斗时不应觅食，得到 %d", h)
	}
	if r.Stats()["deferred"].(int64) != 1 {
		t.Fatalf("应记一次 deferred")
	}
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick+1, 10); len(due) != 1 || due[0].Tier != "hot" {
		t.Fatalf("让位后应 HOT 重试，得到 %+v", due)
	}
}

func TestApplyL1FullCycle(t *testing.T) {
	// 端到端自洽循环：唤醒到点 → schedulePass 入队 → processOne 觅食 → 重排。
	r, ctx := newRunner(t, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()
	seedActiveUnit(t, r, ctx, "u1", 15, unit.LifeStateActive)
	if err := agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{UnitID: "u1", SessionID: "s1", RegionID: "s1", WakeAtTick: tick - 1}); err != nil {
		t.Fatalf("enqueue wake: %v", err)
	}

	enq, err := r.schedulePass(ctx)
	if err != nil || enq != 1 {
		t.Fatalf("schedulePass 应入队 1: enq=%d err=%v", enq, err)
	}
	worked, err := r.processOne(ctx)
	if err != nil || !worked {
		t.Fatalf("processOne 应处理一条: worked=%v err=%v", worked, err)
	}
	if h := unitHunger(t, r, ctx, "u1"); h != 15+forageGain {
		t.Fatalf("全链路觅食后 hunger 应为 %d，得到 %d", 15+forageGain, h)
	}
	if n, _ := agentqueue.CountJobsByStatus(ctx, r.db, agentqueue.StatusDone); n != 1 {
		t.Fatalf("作业应 done，得到 %d", n)
	}
	// 重排到下个 HOT tick，当前 tick 不再到点。
	if due, _ := agentqueue.ListDueWakes(ctx, r.db, "", "s1", tick, 10); len(due) != 0 {
		t.Fatalf("重排应在未来 tick，当前不到点，得到 %+v", due)
	}
}

func TestRunGracefulStop(t *testing.T) {
	// 真实短循环：启动 Run，很快取消 ctx，应在合理时间内优雅返回（不挂死）。
	r, _ := newRunner(t, Config{Enabled: true, TickSeconds: 1, TickInterval: 10 * time.Millisecond, Workers: 2, ReclaimEvery: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		r.Run(ctx)
		close(done)
	}()
	time.Sleep(40 * time.Millisecond) // 让 ticker/worker 转几圈
	cancel()
	select {
	case <-done:
		// 优雅退出。
	case <-time.After(3 * time.Second):
		t.Fatalf("Run 未在取消后优雅退出（疑似挂死）")
	}
	// 未 Enabled 时 Run 立即返回。
	r2, _ := newRunner(t, Config{Enabled: false})
	doneCh := make(chan struct{})
	go func() { r2.Run(context.Background()); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(time.Second):
		t.Fatalf("未启用时 Run 应立即返回")
	}
}
