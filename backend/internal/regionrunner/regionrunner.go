// Package regionrunner 是大世界 region-runner（沙盘 §8.2 阶段 2 / §9）：按真实时钟低频推进世界 tick，
// 按冷热分层唤醒单位决策。它把 internal/engine/scheduler（纯逻辑唤醒原语）与 internal/agentqueue（持久化队列）
// 接成一台后台引擎，让角色在战斗之外也持续自主生活。
//
// 本文件是 **real-1 骨架**：跑通「推 tick → 拉到点单位 → 入队作业 → worker 池原子认领 → 重排唤醒」全机制，
// 但处于 **shadow 模式**——worker 只记日志、**不应用决策、不改任何单位状态**（决策应用是 real-2，HOT 上 LLM 是 real-3，
// 单位 seed/让位战斗过滤是 real-4）。默认关闭（由 main 按 flag 启动）；注入式时钟（now）使全部机制可确定性测试。
//
// tick 模型：currentTick = 真实 Unix 秒 / TickSeconds——**真实时钟派生、持久单调**（跨重启不回退，故已排期的
// wake_at_tick 不会因重启而错过）。这正是 §10 MVP「世界 tick 调度器按真实时钟低频推进」。
package regionrunner

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

// L1 觅食/休息生存循环参数（real-2，§8.2 动机栈 L1）：饿则觅食补口粮，否则休息（缓慢消耗）。
const (
	forageThreshold = 40 // hunger < 此值即觅食
	forageGain      = 30 // 觅食补充的 hunger
	restConsume     = 3  // 休息每次消耗的 hunger（缓慢变饿，驱动下次觅食）
)

// Config 是 region-runner 的运行参数。零值字段由 New 兜底为安全默认。
type Config struct {
	Enabled      bool          // 是否启动（main 按 QUNXIANG_REGION_RUNNER_ENABLED 设）
	Apply        bool          // false=shadow（只记日志，real-1）；true=真应用 L1 决策（real-2，QUNXIANG_REGION_RUNNER_APPLY）
	TickInterval time.Duration // 真实时钟每隔多久跑一次调度 pass
	TickSeconds  int64         // 1 个逻辑 tick = 多少真实秒（wake_at_tick 的时间单位）
	Workers      int           // worker 池 goroutine 数
	MaxInFlight  int           // 全局在途决策上限（背压，§9）
	ReclaimEvery time.Duration // 多久跑一次 stale-running 回收
	StaleAfter   time.Duration // 作业认领后多久未完成算 stale
	MaxAttempt   int           // 作业最大重试次数（超限置 failed）
}

func (c Config) withDefaults() Config {
	if c.TickInterval <= 0 {
		c.TickInterval = 30 * time.Second
	}
	if c.TickSeconds <= 0 {
		c.TickSeconds = 30
	}
	if c.Workers <= 0 {
		c.Workers = 4
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = scheduler.DefaultMaxInFlight
	}
	if c.ReclaimEvery <= 0 {
		c.ReclaimEvery = 2 * time.Minute
	}
	if c.StaleAfter <= 0 {
		c.StaleAfter = 5 * time.Minute
	}
	if c.MaxAttempt <= 0 {
		c.MaxAttempt = 3
	}
	return c
}

// stats 是进程内累计遥测（原子）。
type stats struct {
	ticks, enqueued, claimed, completed, reclaimed, failed, backpressured int64
	foraged, rested, deferred, dropped, conflicted                        int64 // real-2/3 应用态：觅食/休息/让位战斗/丢弃(逝者或删档)/乐观并发冲突退避
}

// Runner 是 region-runner 引擎实例。
type Runner struct {
	db        *sql.DB
	cfg       Config
	log       *slog.Logger
	now       func() time.Time  // 注入式时钟（测试用固定时钟）
	units     *unit.Repository  // 读单位（觅食/休息决策）
	mutator   *status.Mutator   // 经它改饥饿等保护字段、留痕
	execGuard func(string) bool // 让位战斗：某会话在聚焦执行中则跳过其单位（默认恒 false）
	st        stats
}

// New 构造 region-runner。now 用 time.Now；execGuard 默认恒 false（无战斗让位，测试/未接 session 时）。
func New(db *sql.DB, cfg Config, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	units := unit.NewRepository(db)
	return &Runner{
		db:        db,
		cfg:       cfg.withDefaults(),
		log:       log,
		now:       time.Now,
		units:     units,
		mutator:   status.NewMutator(db, units),
		execGuard: func(string) bool { return false },
	}
}

// SetExecutionGuard 注入「会话是否在聚焦战斗执行中」判定（main 接 session.IsExecutionRunning），用于让位战斗。
func (r *Runner) SetExecutionGuard(guard func(string) bool) {
	if guard != nil {
		r.execGuard = guard
	}
}

// withClock 注入时钟（测试用）。
func (r *Runner) withClock(now func() time.Time) *Runner {
	r.now = now
	return r
}

// currentTick 返回真实时钟派生的持久单调 tick。
func (r *Runner) currentTick() int64 {
	return r.now().Unix() / r.cfg.TickSeconds
}

// schedulePass 跑一个调度 tick：遍历有唤醒排期的 region，拉到点单位，入队决策作业（背压闸把守）。返回本次入队数。
// 处理过的 wake 先 RemoveWake 再入队 job——单位在「被拉出到重排」之间无 wake 行，故不会被重复入队。
func (r *Runner) schedulePass(ctx context.Context) (int, error) {
	atomic.AddInt64(&r.st.ticks, 1)
	tick := r.currentTick()
	regions, err := agentqueue.DistinctWakeRegions(ctx, r.db)
	if err != nil {
		return 0, err
	}
	enqueued := 0
	for _, reg := range regions {
		due, err := agentqueue.ListDueWakes(ctx, r.db, reg.WorldID, reg.RegionID, tick, 256)
		if err != nil {
			r.log.Warn("region-runner list due wakes", "region", reg.RegionID, "error", err)
			continue
		}
		for _, w := range due {
			inflight, err := agentqueue.CountJobsByStatus(ctx, r.db, agentqueue.StatusRunning)
			if err != nil {
				r.log.Warn("region-runner count inflight", "error", err)
				break
			}
			if !scheduler.AdmitDecision(inflight, r.cfg.MaxInFlight) {
				atomic.AddInt64(&r.st.backpressured, 1)
				break // 背压：本 region 本 tick 不再入队，下个 tick 再来
			}
			// 原子出队成 job（DELETE wake + INSERT job 同事务），避免「删了 wake 却没入 job」致单位永久丢失。
			if _, err := agentqueue.PromoteWakeToJob(ctx, r.db, w, tick); err != nil {
				r.log.Warn("region-runner promote wake to job", "unit", w.UnitID, "error", err)
				continue
			}
			enqueued++
			atomic.AddInt64(&r.st.enqueued, 1)
		}
	}
	return enqueued, nil
}

// processOne 让一个 worker 原子认领并处理一条作业：派发给 handleJob（shadow 记日志 / apply 真应用 L1），
// 据其返回决定是否重排唤醒，最后完成作业。返回是否处理了一条（false 表示队列空）。
func (r *Runner) processOne(ctx context.Context) (bool, error) {
	job, err := agentqueue.ClaimNextJob(ctx, r.db)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}
	atomic.AddInt64(&r.st.claimed, 1)

	tier, reschedule := r.handleJob(ctx, job)
	if reschedule {
		next := scheduler.NextWakeTick(tier, r.currentTick())
		if err := agentqueue.EnqueueWake(ctx, r.db, agentqueue.WakeEntry{
			UnitID: job.UnitID, SessionID: job.SessionID, WorldID: job.WorldID, RegionID: job.RegionID,
			WakeAtTick: next, Tier: string(tier),
		}); err != nil {
			r.log.Warn("region-runner re-enqueue wake", "unit", job.UnitID, "error", err)
		}
	}

	if err := agentqueue.CompleteJob(ctx, r.db, job.ID, agentqueue.StatusDone); err != nil {
		return true, err
	}
	atomic.AddInt64(&r.st.completed, 1)
	return true, nil
}

// handleJob 处理一条作业，返回 (下次唤醒分层, 是否重排)。
//   - shadow（!cfg.Apply）：只记日志、不碰单位，固定 WARM 重排。
//   - apply（cfg.Apply）：跑 L1 反射决策并经 Mutator 应用，按真实冷热分层重排；单位已逝/在战则不重排或让位。
func (r *Runner) handleJob(ctx context.Context, job *agentqueue.DecisionJob) (scheduler.Tier, bool) {
	if !r.cfg.Apply {
		r.log.Debug("region-runner shadow decision", "unit", job.UnitID, "session", job.SessionID, "tick", job.Tick)
		return scheduler.TierWarm, true
	}
	return r.applyAmbientL1(ctx, job)
}

// applyAmbientL1 跑 L1 反射决策（觅食/休息生存循环）并经 Mutator 应用。返回 (下次分层, 是否重排)。
//
// ⚠️ 并发前置（评审 load-bearing，real-3 硬化前必读）：Mutator.Apply 是 GetByID→改→Save 的读改写（整 status_json
// 全量写、无单位级锁），与战斗循环/HTTP 对同一单位的全量 Save 并发时存在「后写覆盖」丢更新。本步靠 ①让位战斗
// （execGuard，下面在改前再查一次收窄窗口）②默认 Apply=false 兜底。**在任何会被战斗循环触达的会话开启 Apply 前，
// 必须先落地单位级串行化（进程级 per-session 锁）或乐观并发（Save … WHERE updated_at=?）**——否则会丢战斗的 HP/士气改动。
func (r *Runner) applyAmbientL1(ctx context.Context, job *agentqueue.DecisionJob) (scheduler.Tier, bool) {
	tick := r.currentTick()

	// 生命态/存在性优先判定（先于让位战斗，故逝者/删档无论是否在战都尽早清出调度）。
	lastActive, lifeState, err := r.units.SchedulingState(ctx, job.UnitID)
	if err != nil {
		// 单位已不存在（删档/清理）→ 不重排，自然退出调度。
		r.log.Debug("region-runner unit gone, drop", "unit", job.UnitID, "error", err)
		atomic.AddInt64(&r.st.dropped, 1)
		return scheduler.TierCold, false
	}
	switch lifeState {
	case unit.LifeStateActive:
		// 继续日常决策。
	case unit.LifeStateDead:
		// 已逝：永久退出日常调度（死亡走世界化，不归 region-runner）→ 不重排。
		atomic.AddInt64(&r.st.dropped, 1)
		return scheduler.TierCold, false
	default:
		// 倒地/恢复中等**暂态**：不动单位，但 COLD 低频回查——恢复为 active 后会重新进入日常觅食/休息，
		// 不能像逝者那样 drop（否则恢复后再无 wake、永久脱离离线模拟）。
		return scheduler.TierCold, true
	}

	// 让位战斗：会话在聚焦执行中 → 不打扰，HOT 稍后重试（避免与战斗循环并发改同一单位）。
	if r.execGuard(job.SessionID) {
		atomic.AddInt64(&r.st.deferred, 1)
		return scheduler.TierHot, true
	}

	record, err := r.units.GetByID(ctx, job.UnitID)
	if err != nil {
		atomic.AddInt64(&r.st.dropped, 1)
		return scheduler.TierCold, false
	}
	if record.Status.InCombat {
		atomic.AddInt64(&r.st.deferred, 1)
		return scheduler.TierHot, true
	}
	// 改前再查一次让位（收窄 RMW 竞态窗口：战斗可能在上面两次 DB 读期间刚启动）。
	if r.execGuard(job.SessionID) {
		atomic.AddInt64(&r.st.deferred, 1)
		return scheduler.TierHot, true
	}

	// L1 反射决策（零 LLM）：饿则觅食补口粮（HOT 盯到吃饱），否则日常消耗（缓慢变饿，驱动下次觅食，自然降温至 COLD）。
	// 经 ApplyOptimistic 乐观并发写：若自读取以来该单位被战斗/HTTP 改过 → ErrConcurrentModification → 让位（不覆盖、
	// 不计觅食/休息），COLD 重排稍后再来。这是 real-3-0 对「Mutator 读改写与并发写者丢更新」的硬化（§8.6 单位级串行化）。
	if record.Status.Hunger < forageThreshold {
		if _, err := r.mutator.ApplyOptimistic(ctx, status.Mutation{
			UnitID: job.UnitID, Turn: int(tick), Field: status.FieldHunger, Delta: forageGain,
			ReasonCode: events.ReasonAmbientForage,
		}); err != nil {
			if errors.Is(err, status.ErrConcurrentModification) {
				atomic.AddInt64(&r.st.conflicted, 1)
			} else {
				r.log.Warn("region-runner forage mutate", "unit", job.UnitID, "error", err)
			}
			return scheduler.ClassifyTier(tick, lastActive), true
		}
		atomic.AddInt64(&r.st.foraged, 1)
		if err := r.units.TouchLastActiveTick(ctx, job.UnitID, tick); err != nil {
			r.log.Warn("region-runner touch last active", "unit", job.UnitID, "error", err)
		}
		return scheduler.TierHot, true
	}
	if _, err := r.mutator.ApplyOptimistic(ctx, status.Mutation{
		UnitID: job.UnitID, Turn: int(tick), Field: status.FieldHunger, Delta: -restConsume,
		ReasonCode: events.ReasonAmbientRest,
	}); err != nil {
		if errors.Is(err, status.ErrConcurrentModification) {
			atomic.AddInt64(&r.st.conflicted, 1)
		} else {
			r.log.Warn("region-runner rest mutate", "unit", job.UnitID, "error", err)
		}
	} else {
		atomic.AddInt64(&r.st.rested, 1)
	}
	return scheduler.ClassifyTier(tick, lastActive), true
}

// reclaim 跑一次 stale-running 回收，累计遥测。
func (r *Runner) reclaim(ctx context.Context) {
	reclaimed, failed, err := agentqueue.ReclaimStaleJobs(ctx, r.db, r.cfg.StaleAfter, r.cfg.MaxAttempt)
	if err != nil {
		r.log.Warn("region-runner reclaim stale jobs", "error", err)
		return
	}
	atomic.AddInt64(&r.st.reclaimed, int64(reclaimed))
	atomic.AddInt64(&r.st.failed, int64(failed))
}

// Run 启动后台引擎：调度 ticker + worker 池 + 定期回收，阻塞至 ctx 取消即优雅退出（worker 跑完手头作业即停）。
// 未 Enabled 时立即返回（不启动任何 goroutine）。
func (r *Runner) Run(ctx context.Context) {
	if !r.cfg.Enabled {
		return
	}
	r.log.Info("region-runner started", "tick_interval", r.cfg.TickInterval, "workers", r.cfg.Workers, "max_in_flight", r.cfg.MaxInFlight)
	var wg sync.WaitGroup

	// 调度 goroutine。
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(r.cfg.TickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := r.schedulePass(ctx); err != nil {
					r.log.Warn("region-runner schedule pass", "error", err)
				}
			}
		}
	}()

	// 回收 goroutine。
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(r.cfg.ReclaimEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.reclaim(ctx)
			}
		}
	}()

	// worker 池：各自循环认领作业；无作业时短暂退避。
	for i := 0; i < r.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if ctx.Err() != nil {
					return
				}
				worked, err := r.processOneRecovered(ctx)
				if err != nil {
					r.log.Warn("region-runner process job", "error", err)
				}
				if !worked {
					// 队列空：退避，避免空转打满 CPU；ctx 取消则退出。
					select {
					case <-ctx.Done():
						return
					case <-time.After(200 * time.Millisecond):
					}
				}
			}
		}()
	}

	<-ctx.Done()
	wg.Wait()
	r.log.Info("region-runner stopped", "stats", r.Stats())
}

// processOneRecovered 包 recover——单条作业 panic 不拖垮 worker（崩溃作业由 stale-reclaim 重试）。
func (r *Runner) processOneRecovered(ctx context.Context) (worked bool, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			r.log.Error("region-runner worker panic recovered", "panic", rec)
			worked, err = true, nil // 视为「处理过一条」，避免误判队列空而退避
		}
	}()
	return r.processOne(ctx)
}

// Stats 返回当前累计遥测（供 /healthz 暴露）。
func (r *Runner) Stats() map[string]any {
	return map[string]any{
		"enabled":       r.cfg.Enabled,
		"shadow":        !r.cfg.Apply, // Apply=false 即 shadow（只记日志不应用）
		"current_tick":  r.currentTick(),
		"ticks":         atomic.LoadInt64(&r.st.ticks),
		"enqueued":      atomic.LoadInt64(&r.st.enqueued),
		"claimed":       atomic.LoadInt64(&r.st.claimed),
		"completed":     atomic.LoadInt64(&r.st.completed),
		"reclaimed":     atomic.LoadInt64(&r.st.reclaimed),
		"failed":        atomic.LoadInt64(&r.st.failed),
		"backpressured": atomic.LoadInt64(&r.st.backpressured),
		"foraged":       atomic.LoadInt64(&r.st.foraged),
		"rested":        atomic.LoadInt64(&r.st.rested),
		"deferred":      atomic.LoadInt64(&r.st.deferred),
		"dropped":       atomic.LoadInt64(&r.st.dropped),
		"conflicted":    atomic.LoadInt64(&r.st.conflicted),
	}
}
