package session

// 文件说明：M7.3-real-4b 单位 seed 契约测试（对真实 SQLite）。验证 seedAmbientForUnits 写出的「作用域列 + 生命态列 +
// 唤醒队列行」恰好是 region-runner schedulePass 的输入契约（region 非空、立即到点、带 session_id/tier=cold）——
// 与 regionrunner 包既有的「唤醒行→觅食/休息」处理测试相接，即组成端到端证明（不跨包调私有/不引计时 flake）。
// 另验证开关关时全程 no-op（默认建局链路零成本）与幂等（重复 seed 不产生重复唤醒）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/regionrunner"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func newAmbientTestService(t *testing.T, enabled bool) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "ambient.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{db: db, units: repo, mutator: status.NewMutator(db, repo), ambientSchedulingEnabled: enabled}
	return db, repo, service
}

// seedTwoUnits 落两个 active 单位并返回其 ID。
func seedTwoUnits(t *testing.T, ctx context.Context, repo *unit.Repository) []string {
	t.Helper()
	ids := make([]string, 0, 2)
	for i, name := range []string{"她", "他"} {
		rec := unit.BootstrapRecord(int64(i)+1, "s1", "player", name)
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("保存单位失败: %v", err)
		}
		ids = append(ids, rec.ID)
	}
	return ids
}

func TestSeedAmbientForUnits_WritesWakeScopeLifeState(t *testing.T) {
	db, repo, service := newAmbientTestService(t, true)
	ctx := context.Background()
	ids := seedTwoUnits(t, ctx, repo)

	if err := service.seedAmbientForUnits(ctx, "s1", "", ids); err != nil {
		t.Fatalf("seedAmbientForUnits 不应报错: %v", err)
	}

	// ① 唤醒队列：每单位一行、立即到点（WakeAtTick=0）、带 region/session、tier=cold——正是 schedulePass 的输入契约。
	due, err := agentqueue.ListDueWakes(ctx, db, "", "s1", 1_000_000_000, 10)
	if err != nil {
		t.Fatalf("ListDueWakes: %v", err)
	}
	if len(due) != len(ids) {
		t.Fatalf("应为每个单位入一条到点唤醒，期望 %d，得到 %d", len(ids), len(due))
	}
	for _, w := range due {
		if w.RegionID != "s1" || w.SessionID != "s1" {
			t.Fatalf("唤醒应带 region/session=s1（region 非空是 runner 必填不变量、session 是 purge 键），得到 %+v", w)
		}
		if w.Tier != "cold" {
			t.Fatalf("初始唤醒应 tier=cold，得到 %q", w.Tier)
		}
	}

	// ② 单位作用域列：region=s1 + life_state=active → 被 ListActiveByRegion 命中（region 分片/管理查询用）。
	active, err := repo.ListActiveByRegion(ctx, "s1")
	if err != nil {
		t.Fatalf("ListActiveByRegion: %v", err)
	}
	if len(active) != len(ids) {
		t.Fatalf("两个单位都应被 scope 进 region=s1 且 active，得到 %d", len(active))
	}
	// ③ 生命态列显式 active。
	for _, id := range ids {
		if _, lifeState, err := repo.SchedulingState(ctx, id); err != nil || lifeState != string(unit.LifeStateActive) {
			t.Fatalf("单位 %s 生命态列应为 active，得到 %q err=%v", id, lifeState, err)
		}
	}
}

func TestSeedAmbientForUnits_DisabledNoOp(t *testing.T) {
	db, repo, service := newAmbientTestService(t, false) // 开关关
	ctx := context.Background()
	ids := seedTwoUnits(t, ctx, repo)

	if err := service.seedAmbientForUnits(ctx, "s1", "", ids); err != nil {
		t.Fatalf("关时应安静 no-op: %v", err)
	}

	// 关时：不写唤醒队列、不 scope 单位（默认建局链路零成本，且不在运行器开启时撞见关闭期脏唤醒）。
	if due, _ := agentqueue.ListDueWakes(ctx, db, "", "s1", 1_000_000_000, 10); len(due) != 0 {
		t.Fatalf("开关关时不应入任何唤醒，得到 %d", len(due))
	}
	if active, _ := repo.ListActiveByRegion(ctx, "s1"); len(active) != 0 {
		t.Fatalf("开关关时不应把单位 scope 进 region，得到 %d", len(active))
	}
}

func TestSeedAmbientForUnits_Idempotent(t *testing.T) {
	db, repo, service := newAmbientTestService(t, true)
	ctx := context.Background()
	ids := seedTwoUnits(t, ctx, repo)

	for i := 0; i < 2; i++ { // 重复 seed（如重连/重试）应幂等：唤醒按 unit_id upsert，不产生重复行。
		if err := service.seedAmbientForUnits(ctx, "s1", "", ids); err != nil {
			t.Fatalf("第 %d 次 seed: %v", i+1, err)
		}
	}
	due, err := agentqueue.ListDueWakes(ctx, db, "", "s1", 1_000_000_000, 10)
	if err != nil {
		t.Fatalf("ListDueWakes: %v", err)
	}
	if len(due) != len(ids) {
		t.Fatalf("重复 seed 应幂等（每单位仍仅一条唤醒），期望 %d，得到 %d", len(ids), len(due))
	}
}

// TestSeedAmbientForNewUnit_PlayerFactionOnly 守 real-4b 评审发现的 load-bearing：建局后中途新生/归化的单位也要进
// 离线调度，且只 seed 玩家阵营（与建局口径一致）。婚育子嗣/野民归化两处造人点都经此 helper，故测它即守住那条策略。
func TestSeedAmbientForNewUnit_PlayerFactionOnly(t *testing.T) {
	db, repo, service := newAmbientTestService(t, true)
	ctx := context.Background()
	state := &State{ID: "s1", PlayerFactionID: "player", EnemyFactionID: "enemy"}

	child := unit.BootstrapRecord(1, "s1", "player", "幼") // 玩家阵营中途新生 → seed
	if err := repo.Save(ctx, child); err != nil {
		t.Fatalf("保存子嗣失败: %v", err)
	}
	service.seedAmbientForNewUnit(ctx, state, child)

	foe := unit.BootstrapRecord(2, "s1", "enemy", "敌") // 敌方中途新生 → 不 seed
	if err := repo.Save(ctx, foe); err != nil {
		t.Fatalf("保存敌方失败: %v", err)
	}
	service.seedAmbientForNewUnit(ctx, state, foe)

	due, err := agentqueue.ListDueWakes(ctx, db, "", "s1", 1_000_000_000, 10)
	if err != nil {
		t.Fatalf("ListDueWakes: %v", err)
	}
	if len(due) != 1 || due[0].UnitID != child.ID {
		t.Fatalf("仅玩家阵营中途新生单位应入离线调度，得到 %+v", due)
	}
}

// TestSeedAmbientForNewUnit_DisabledNoOp：开关关时中途造人也不 seed（零成本一致性）。
func TestSeedAmbientForNewUnit_DisabledNoOp(t *testing.T) {
	db, repo, service := newAmbientTestService(t, false)
	ctx := context.Background()
	state := &State{ID: "s1", PlayerFactionID: "player"}
	child := unit.BootstrapRecord(1, "s1", "player", "幼")
	if err := repo.Save(ctx, child); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	service.seedAmbientForNewUnit(ctx, state, child)
	if due, _ := agentqueue.ListDueWakes(ctx, db, "", "s1", 1_000_000_000, 10); len(due) != 0 {
		t.Fatalf("开关关时中途新生不应 seed，得到 %d", len(due))
	}
}

// TestSeedAmbientForUnits_EndToEndRunnerForages 是 real-4b 的真·端到端：seedAmbientForUnits 把饿单位 seed 进调度，
// 再启动一台真正的 region-runner（Apply）跑其导出的 Run 循环，断言它确实把单位唤醒并觅食把 hunger 抬上来——
// 跨「session 的 seed」与「regionrunner 的处理」两层，证明循环真能自转。用短 TickInterval + 宽轮询窗，避免计时脆性。
func TestSeedAmbientForUnits_EndToEndRunnerForages(t *testing.T) {
	db, repo, service := newAmbientTestService(t, true)
	ctx := context.Background()

	rec := unit.BootstrapRecord(2, "s1", "player", "她")
	rec.ID = "u1"
	rec.Status.Hunger = 20 // 饿（<40）→ 觅食
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
	if err := service.seedAmbientForUnits(ctx, "s1", "", []string{"u1"}); err != nil {
		t.Fatalf("seedAmbientForUnits: %v", err)
	}

	runner := regionrunner.New(db, regionrunner.Config{
		Enabled: true, Apply: true, TickSeconds: 1,
		TickInterval: 10 * time.Millisecond, Workers: 1,
	}, nil)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { runner.Run(runCtx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	hunger := 0
	for time.Now().Before(deadline) {
		if cur, err := repo.GetByID(ctx, "u1"); err == nil && cur.Status.Hunger >= 50 {
			hunger = cur.Status.Hunger
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done // 等 Run 优雅退出（worker 池 WaitGroup）

	if hunger < 50 {
		final, _ := repo.GetByID(ctx, "u1")
		t.Fatalf("启用 region-runner 后应 seed→唤醒→觅食把 hunger 抬到 ≥50，5s 内未达成（最终 hunger=%d）", final.Status.Hunger)
	}
}
