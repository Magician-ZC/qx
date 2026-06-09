package session

// 文件说明：F4 H2 回归测试——异步执行收尾（advanceAfterAsyncExecution，生产恒走）确有跑全 Execution→Deployment 边界钩子。
//
// 修前：同步 AdvancePhase 的 PhaseExecution 分支内联了整批边界钩子，但异步完成路径 advanceAfterAsyncExecution 全缺，
// 致前面多波建的边界功能（道德漂移/阵营切换/目标重估/人格漂移/consent/威胁/跨玩家/副本/世界化/世界 Boss/撮合/社交/
// 阵营冲突/零和）在生产默认**全不运行**。修后抽成公共 settleExecutionToDeploymentBoundary，两路共用。
//
// 本测试用「道德漂移留痕 MORAL_DRIFT」作可观测锚：在执行回合 N 落一条战斗杀伤事件（道德信号源），跑异步收尾后
// 应留下 MORAL_DRIFT——证明 settleAutonomyAtDeploymentBoundary→settleMoralDrift 确在异步路径运行，且按执行回合 N
// 查到了信号（同时回归 H1 的 tick 契约）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/faction"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// TestAdvanceAfterAsyncExecution_RunsBoundaryHooks 验证 F4 H2：异步收尾确跑边界钩子（以道德漂移留痕为锚）。
func TestAdvanceAfterAsyncExecution_RunsBoundaryHooks(t *testing.T) {
	db, service := newAsyncBoundaryService(t)
	ctx := context.Background()

	const executionTurn = 5 // 执行回合 N（Advance 前）

	// 一名玩家可控、存活、背离倾向角色：执行回合落两条战斗杀伤事件（→Chaos 道德信号）。
	actor := unit.BootstrapRecord(21, "s-async", "player", "阿戍")
	actor.Faction = faction.IDFreedom
	actor.MoralAlignment = faction.MoralAlignment{Freedom: 70, Order: 15, Chaos: 15}
	actor.Status.HP = 100
	actor.Status.LifeState = unit.LifeStateActive
	if err := service.units.Save(ctx, actor); err != nil {
		t.Fatalf("save actor: %v", err)
	}
	seedActorEvent(t, db, "s-async", actor.ID, events.ReasonCombatDown, executionTurn)
	seedActorEvent(t, db, "s-async", actor.ID, events.ReasonCrossBetrayal, executionTurn)

	// 会话处于 PhaseExecution / Ongoing，回合 = N，含该玩家单位；落库供异步收尾的 Save/snapshot 取回。
	state := &State{
		ID:              "s-async",
		PlayerFactionID: "player",
		Outcome:         OutcomeOngoing,
		PlayerUnitIDs:   []string{actor.ID},
	}
	state.TurnState.Phase = turns.PhaseExecution
	state.TurnState.Turn = executionTurn
	state.ExecutionInProgress = true
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	// 修前 MORAL_DRIFT 应为 0（异步路径不跑边界钩子）。
	if n := reasonEventCount(t, db, actor.ID, events.ReasonMoralDrift); n != 0 {
		t.Fatalf("前置应无 MORAL_DRIFT，得 %d", n)
	}

	// 异步收尾（生产恒走此路）：Advance 到 N+1，executedTurn=N 跑边界钩子。
	if err := service.advanceAfterAsyncExecution(ctx, state, []unit.Record{actor}); err != nil {
		t.Fatalf("advanceAfterAsyncExecution: %v", err)
	}

	// 推进到部署回合 N+1（确认走了 Advance 分支）。
	if state.TurnState.Phase != turns.PhaseDeployment {
		t.Fatalf("收尾后应进部署阶段，得 %q", state.TurnState.Phase)
	}
	if state.TurnState.Turn != executionTurn+1 {
		t.Fatalf("收尾后回合应为 N+1=%d，得 %d", executionTurn+1, state.TurnState.Turn)
	}

	// 核心：异步收尾确跑了 settleMoralDrift（边界钩子之一）→ 留下 MORAL_DRIFT（按执行回合 N 命中战斗杀伤信号）。
	if n := reasonEventCount(t, db, actor.ID, events.ReasonMoralDrift); n != 1 {
		t.Fatalf("异步收尾应跑边界钩子并留 1 条 MORAL_DRIFT（H2 修），得 %d", n)
	}
	// 留痕 Tick 应落执行回合（H1：信号源与审计同回合自洽）。
	if tick := reasonEventTick(t, db, actor.ID, events.ReasonMoralDrift); tick != executionTurn {
		t.Fatalf("MORAL_DRIFT 留痕 Tick 应 = 执行回合 %d，得 %d", executionTurn, tick)
	}

	// 道德轴确有 Chaos 上升（漂移真落库）。
	reloaded, err := service.units.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("reget: %v", err)
	}
	if reloaded.MoralAlignment.Chaos <= 15 {
		t.Fatalf("Chaos 应因战斗杀伤信号上升，得 %.2f", reloaded.MoralAlignment.Chaos)
	}
}

// TestAdvanceAfterAsyncExecution_FirstTurnNoStaleSignal 验证首回合安全：执行回合 0 无历史，不误漂。
func TestAdvanceAfterAsyncExecution_FirstTurnNoStaleSignal(t *testing.T) {
	db, service := newAsyncBoundaryService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(22, "s-async1", "player", "阿初")
	actor.Faction = faction.IDFreedom
	actor.MoralAlignment = faction.MoralAlignment{Freedom: 70, Order: 15, Chaos: 15}
	actor.Status.HP = 100
	actor.Status.LifeState = unit.LifeStateActive
	if err := service.units.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := &State{ID: "s-async1", PlayerFactionID: "player", Outcome: OutcomeOngoing, PlayerUnitIDs: []string{actor.ID}}
	state.TurnState.Phase = turns.PhaseExecution
	state.TurnState.Turn = 1 // 执行回合 1 → executedTurn=0 无历史事件
	state.ExecutionInProgress = true
	if err := service.sessions.Save(ctx, state); err != nil {
		t.Fatalf("save state: %v", err)
	}

	if err := service.advanceAfterAsyncExecution(ctx, state, []unit.Record{actor}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	// 执行回合 0 无信号 → 不应误漂、不留痕。
	if n := reasonEventCount(t, db, actor.ID, events.ReasonMoralDrift); n != 0 {
		t.Fatalf("首回合执行回合 0 无信号不应漂移，得 %d 条 MORAL_DRIFT", n)
	}
}

// newAsyncBoundaryService 起一个带 sessions/units/mutator 的全功能 Service（异步收尾需 loadSession/Save/snapshot）。
func newAsyncBoundaryService(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "async_boundary.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, NewServiceWithColdStore(db, nil, nil)
}
