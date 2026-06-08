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
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

// ambientAction 是离线自治可选的环境动作（real-4a 把动作空间从觅食/休息扩到含社交/反思，为 real-3 HOT-LLM 备好「值得用 LLM 决」的选择空间）。
type ambientAction string

const (
	actForage    ambientAction = "forage"    // 觅食（饿）→ hunger+
	actRest      ambientAction = "rest"      // 休息（缓慢消耗）→ hunger-
	actSocialize ambientAction = "socialize" // 社交（士气低→找人攀谈）→ morale+
	actReflect   ambientAction = "reflect"   // 反思（心满意足→独自沉淀）→ morale 小+
)

// 动机栈 L1+L3 阈值/增益（§8.2）。morale ∈ [0,1]、hunger ∈ [0,100]。
const (
	forageThreshold = 40   // hunger < 此值即觅食
	forageGain      = 30   // 觅食补充的 hunger
	restConsume     = 3    // 休息每次消耗的 hunger（缓慢变饿，驱动下次觅食）
	moraleLow       = 0.4  // morale < 此值倾向社交（找人攀谈舒展心情）
	moraleHigh      = 0.8  // morale > 此值倾向反思（心满意足独自沉淀）
	socializeGain   = 0.05 // 社交的 morale 增益
	reflectGain     = 0.02 // 反思的 morale 增益（更小）
)

// 字段饱和边界——镜像 status.Mutator 的 clamp 上下界（morale clampFloat[0,1] / hunger clampInt[0,100]，见 mutator.go
// FieldMorale/FieldHunger）。用于「饱和空写短路」：动作把已达界的字段继续往界外推时，写入会被 clamp 成 before==after 空写。
const (
	moraleMin = 0.0
	moraleMax = 1.0
	hungerMin = 0
	hungerMax = 100
)

// ambientEffect 是一个动作的落地：改哪个字段、增量、reason-code，以及是否「主动响应需求」（决定 HOT 还是降温）。
type ambientEffect struct {
	field  status.Field
	delta  float64
	reason events.ReasonCode
	active bool // 主动响应需求（觅食/社交）→ HOT 盯着；被动（休息/反思）→ 自然降温
}

var ambientEffects = map[ambientAction]ambientEffect{
	actForage:    {status.FieldHunger, forageGain, events.ReasonAmbientForage, true},
	actRest:      {status.FieldHunger, -restConsume, events.ReasonAmbientRest, false},
	actSocialize: {status.FieldMorale, socializeGain, events.ReasonAmbientSocialize, true},
	actReflect:   {status.FieldMorale, reflectGain, events.ReasonAmbientReflect, false},
}

// decideAmbientReflex 是零 LLM 的反射层动作选择（也是 real-3 HOT-LLM 失败/预算耗尽时的 fallback）：
// 饿→觅食；不饿但士气低→社交；心满意足→反思；其余→休息。
func decideAmbientReflex(record unit.Record) ambientAction {
	switch {
	case record.Status.Hunger < forageThreshold:
		return actForage
	case record.Status.Morale < moraleLow:
		return actSocialize
	case record.Status.Morale > moraleHigh:
		return actReflect
	default:
		return actRest
	}
}

// ambientSaturated 判断某动作是否「饱和空写」：字段已在 clamp 边界、效果方向继续往界外推 → 写入后 before==after。
// 此时应跳过落地（不调 Mutator、不落事件、不计数、不升 HOT），否则满意单位（morale 已达上限仍选反思）会每个 COLD
// 周期永久空写 AMBIENT_REFLECT，污染 32 槽记忆环、灌垃圾 events 行、reflected 遥测虚增（real-4a 评审 load-bearing）。
// 用调用方手里已读的 record，零额外 DB 读；stale record 至多多/少跳一个 tick，无正确性影响。实践中只有 reflect 会真正命中
// （其余动作的触发带都远离各自界），但写成通用形以免未来新增动作重蹈「触发带 ⊂ 效果方向」覆辙。
func ambientSaturated(record unit.Record, eff ambientEffect) bool {
	switch eff.field {
	case status.FieldMorale:
		if eff.delta > 0 {
			return record.Status.Morale >= moraleMax
		}
		return record.Status.Morale <= moraleMin
	case status.FieldHunger:
		if eff.delta > 0 {
			return record.Status.Hunger >= hungerMax
		}
		return record.Status.Hunger <= hungerMin
	default:
		return false
	}
}

// Config 是 region-runner 的运行参数。零值字段由 New 兜底为安全默认。
type Config struct {
	Enabled bool // 是否启动（main 按 QUNXIANG_REGION_RUNNER_ENABLED 设）
	Apply   bool // false=shadow（只记日志，real-1）；true=真应用 L1 决策（real-2，QUNXIANG_REGION_RUNNER_APPLY）
	Threats bool // 是否对 HOT 单位 roll PvE 威胁（QUNXIANG_REGION_RUNNER_THREATS，默认关；真遭遇还需注入 threatHandler）。
	// ⚠️ 威胁 roll 在 applyAmbientL1 内，故**依赖 Apply=true**——Apply=false 是纯 shadow 骨架(handleJob 只记日志、不触达 applyAmbientL1)，
	// 此时 Threats 即使开也不会 roll（threats_rolled 恒 0）。要观测威胁需 Enabled+Apply+Threats 三者皆开。
	TickInterval time.Duration // 真实时钟每隔多久跑一次调度 pass
	TickSeconds  int64         // 1 个逻辑 tick = 多少真实秒（wake_at_tick 的时间单位）
	Workers      int           // worker 池 goroutine 数
	MaxInFlight  int           // 全局在途决策上限（背压，§9）
	ReclaimEvery time.Duration // 多久跑一次 stale-running 回收
	StaleAfter   time.Duration // 作业认领后多久未完成算 stale
	MaxAttempt   int           // 作业最大重试次数（超限置 failed）
	LeaseTTL     time.Duration // region 租约有效期（§8.2 多实例分片；须 > 数个 TickInterval，留出续租余量）。
	// 仅当 flag QUNXIANG_REGION_LEASES 开时生效；flag 关时 AcquireLease/RenewLease/ReleaseLease 恒 no-op，零行为变化。
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
	if c.LeaseTTL <= 0 {
		// 默认租约 = max(90s, 3×TickInterval)：远大于一个调度 pass，留足续租余量；持租实例崩溃后约一个 TTL 内别的实例可接管。
		c.LeaseTTL = 3 * c.TickInterval
		if c.LeaseTTL < 90*time.Second {
			c.LeaseTTL = 90 * time.Second
		}
	}
	return c
}

// stats 是进程内累计遥测（原子）。
type stats struct {
	ticks, enqueued, claimed, completed, reclaimed, failed, backpressured int64
	foraged, rested, socialized, reflected                                int64 // real-2/4a 动作计数：觅食/休息/社交/反思
	deferred, dropped, conflicted, settled                                int64 // 让位战斗/丢弃(逝者或删档)/乐观并发冲突退避/饱和空写短路
	llmCalls, llmFallbacks                                                int64 // real-3：HOT 单位经 LLM 决策成功 / LLM 失败回退反射
	threatsRolled, threatsEncountered, threatsFled, encounterErrors       int64 // PvE：roll 命中威胁/升级为遭遇/HP 危急撤退/真遭遇失败
	leasesAcquired, leasesLost, regionsSkipped                            int64 // 分片：本实例抢到 region 租约/续租失败丢区/因他人持租跳过的 region
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

	// region 分片（§8.2「per-region 唯一处理者」）。instanceID 标识本 runner 实例；leases 抢/续/释 region 租约。
	// flag QUNXIANG_REGION_LEASES 关时 leases 全程 no-op（恒抢到、不触 DB）→ 单实例零行为变化。
	instanceID  string                           // 本实例唯一 ID（New 时生成 uuid）
	leases      *LeaseManager                    // region 租约管理器
	heldRegions map[string]agentqueue.WakeRegion // 本实例当前持租的 region（region_id → world/region），由 schedulePass 维护、worker 据此 region-scoped 认领
	heldMu      sync.RWMutex                     // 守护 heldRegions（schedulePass 写 / worker 读 并发）

	// real-3 HOT-LLM（默认 llm==nil → 全程反射；main 按 QUNXIANG_REGION_RUNNER_LLM 注入才启用，见 ambient_llm.go）。
	llm               ambientLLM    // 离线决策 LLM 客户端
	costEstimate      costEstimator // 用量→USD（注入 session.EstimateLLMCostUSD）
	llmBudgetMicroUSD int64         // 进程级预算上限（micro-USD，0=不限）
	llmSpentMicroUSD  int64         // 已花（atomic，micro-USD）
	llmLatched        int32         // 预算耗尽闩（atomic，1=此后全转反射）

	// PvE 接入（默认关；main 按 QUNXIANG_REGION_RUNNER_THREATS 开，threatHandler/anchorDensity 由 PvE-2/4 注入，见 threat.go）。
	threatsEnabled bool                                                      // 是否对 HOT 单位 roll 威胁
	threatRouter   decision.Router                                           // 关键节点闸（HP 危急撤退 / StrategicFork 升级）
	threatHandler  func(ctx context.Context, sessionID, unitID string) error // 真遭遇结算（nil=shadow 只计遥测）
	anchorDensity  func(ctx context.Context, unitID string) float64          // 锚密度查询（PvE-4 锚加权；nil=密度恒 0）
}

// New 构造 region-runner。now 用 time.Now；execGuard 默认恒 false（无战斗让位，测试/未接 session 时）。
func New(db *sql.DB, cfg Config, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	units := unit.NewRepository(db)
	return &Runner{
		db:             db,
		cfg:            cfg.withDefaults(),
		log:            log,
		now:            time.Now,
		units:          units,
		mutator:        status.NewMutator(db, units),
		execGuard:      func(string) bool { return false },
		threatsEnabled: cfg.Threats,
		threatRouter:   decision.DefaultRouter(),
		instanceID:     uuid.NewString(),
		leases:         NewLeaseManager(db),
		heldRegions:    make(map[string]agentqueue.WakeRegion),
	}
}

// SetExecutionGuard 注入「会话是否在聚焦战斗执行中」判定（main 接 session.IsExecutionRunning），用于让位战斗。
func (r *Runner) SetExecutionGuard(guard func(string) bool) {
	if guard != nil {
		r.execGuard = guard
	}
}

// withClock 注入时钟（测试用）。同步注入到 LeaseManager，使租约抢/续/过期窗口与 runner 同钟（确定性分片测试）。
func (r *Runner) withClock(now func() time.Time) *Runner {
	r.now = now
	if r.leases != nil {
		r.leases = r.leases.withClock(now)
	}
	return r
}

// currentTick 返回真实时钟派生的持久单调 tick。
func (r *Runner) currentTick() int64 {
	return r.now().Unix() / r.cfg.TickSeconds
}

// schedulePass 跑一个调度 tick：遍历有唤醒排期的 region，拉到点单位，入队决策作业（背压闸把守）。返回本次入队数。
// 处理过的 wake 先 RemoveWake 再入队 job——单位在「被拉出到重排」之间无 wake 行，故不会被重复入队。
//
// region 分片（§8.2「per-region 唯一处理者」）：遍历前对每个 due region 先 AcquireLease(regionID, instanceID, ttl)——
// 抢到才处理（入队其作业 + 把该 region 记入 heldRegions 供 worker region-scoped 认领）；抢不到（别的实例持租）就跳过、
// 让位。本 pass 持租的 region 在 pass 末统一 RenewLease 续租。flag QUNXIANG_REGION_LEASES 关时 AcquireLease 恒 true →
// 不跳过任何 region、heldRegions 含全部 region；worker 仍走全局 ClaimNextJob（见 claimNextJobScoped）→ 零行为变化。
func (r *Runner) schedulePass(ctx context.Context) (int, error) {
	atomic.AddInt64(&r.st.ticks, 1)
	tick := r.currentTick()
	regions, err := agentqueue.DistinctWakeRegions(ctx, r.db)
	if err != nil {
		return 0, err
	}
	enqueued := 0
	held := make(map[string]agentqueue.WakeRegion, len(regions)) // 本 pass 新确认持租的 region（pass 末覆写 heldRegions）
	for _, reg := range regions {
		// 抢区锁：抢到才处理该 region；抢不到（他人持租）跳过、让位。flag 关时恒抢到（单实例零变化）。
		ok, lerr := r.leases.AcquireLease(ctx, reg.RegionID, r.instanceID, r.cfg.LeaseTTL)
		if lerr != nil {
			r.log.Warn("region-runner acquire lease", "region", reg.RegionID, "error", lerr)
			continue
		}
		if !ok {
			atomic.AddInt64(&r.st.regionsSkipped, 1)
			continue // 别的实例持有此 region 的租约 → 不碰它的单位/作业
		}
		atomic.AddInt64(&r.st.leasesAcquired, 1)
		held[reg.RegionID] = reg

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
	// 发布本 pass 持租的 region 集合（worker 据此 region-scoped 认领作业），并续租它们以免在下个 pass 前过期。
	r.publishHeldRegions(ctx, held)
	return enqueued, nil
}

// publishHeldRegions 覆写 heldRegions 为本 pass 确认持租的集合，并对每个续租（顺延 TTL）。续租失败（已被他人抢走）
// 的 region 立即从持租集合剔除，避免 worker 继续认领已失去区锁的作业。flag 关时 RenewLease 恒 true、不触 DB。
func (r *Runner) publishHeldRegions(ctx context.Context, held map[string]agentqueue.WakeRegion) {
	for regionID := range held {
		ok, err := r.leases.RenewLease(ctx, regionID, r.instanceID, r.cfg.LeaseTTL)
		if err != nil {
			r.log.Warn("region-runner renew lease", "region", regionID, "error", err)
			continue
		}
		if !ok {
			// 续不到 = 已失去区锁（被别的实例抢走）→ 本 pass 不再持有它。
			atomic.AddInt64(&r.st.leasesLost, 1)
			delete(held, regionID)
		}
	}
	r.heldMu.Lock()
	r.heldRegions = held
	r.heldMu.Unlock()
}

// heldRegionList 快照当前持租的 (world,region) 列表（worker region-scoped 认领时遍历）。
func (r *Runner) heldRegionList() []agentqueue.WakeRegion {
	r.heldMu.RLock()
	defer r.heldMu.RUnlock()
	out := make([]agentqueue.WakeRegion, 0, len(r.heldRegions))
	for _, reg := range r.heldRegions {
		out = append(out, reg)
	}
	return out
}

// claimNextJobScoped 是 worker 的认领入口：
//   - flag QUNXIANG_REGION_LEASES 关（单实例）→ 走全局 agentqueue.ClaimNextJob，认领任意 pending 作业（**与接线前完全一致**）。
//   - flag 开（多实例分片）→ 只认领本实例持租 region 的作业：遍历 heldRegions，逐区 ClaimNextJobInRegion，取到第一条即返回。
//
// 这样实例只处理自己持租 region 的单位，维护 §8.2 per-region 单写者不变量。持租集合空时返回 (nil,nil)（worker 退避）。
func (r *Runner) claimNextJobScoped(ctx context.Context) (*agentqueue.DecisionJob, error) {
	if !LeasesEnabled() {
		return agentqueue.ClaimNextJob(ctx, r.db)
	}
	for _, reg := range r.heldRegionList() {
		job, err := agentqueue.ClaimNextJobInRegion(ctx, r.db, reg.RegionID)
		if err != nil {
			return nil, err
		}
		if job != nil {
			return job, nil
		}
	}
	return nil, nil
}

// processOne 让一个 worker 原子认领并处理一条作业：派发给 handleJob（shadow 记日志 / apply 真应用 L1），
// 据其返回决定是否重排唤醒，最后完成作业。返回是否处理了一条（false 表示队列空）。
func (r *Runner) processOne(ctx context.Context) (bool, error) {
	job, err := r.claimNextJobScoped(ctx)
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

	// 选动作（real-3）：当前 HOT 单位且 LLM 启用且预算未耗尽 → LLM 在觅食/休息/社交/反思间决；否则零成本反射
	// decideAmbientReflex（LLM 任何失败也回退它）。再经下方饱和短路 + applyAction 落地（对 LLM/反射动作同等处理）。
	currentTier := scheduler.ClassifyTier(tick, lastActive)
	// PvE（默认关）：HOT 单位先 roll 威胁；命中则本次唤醒用于遭遇/规避（不再走日常 ambient）。见 threat.go。
	if handled, tier := r.maybeEncounterThreat(ctx, job, record, currentTier, tick); handled {
		return tier, true
	}
	action := r.chooseAmbientAction(ctx, record, currentTier)
	// 饱和空写短路：动作把已达 clamp 边界的字段继续往界外推（如满意单位 morale 已 1.0 仍选反思）→ 写入是 before==after 空写。
	// 跳过它（不落地/不计数/不升 HOT），按既有活跃度自然降温——否则会每 COLD 周期永久空写污染记忆环/事件表/遥测（评审 load-bearing）。
	if ambientSaturated(record, ambientEffects[action]) {
		atomic.AddInt64(&r.st.settled, 1)
		return scheduler.ClassifyTier(tick, lastActive), true
	}
	active, applied, err := r.applyAction(ctx, job.UnitID, action, tick)
	if err != nil {
		r.log.Warn("region-runner apply ambient action", "unit", job.UnitID, "action", string(action), "error", err)
		return scheduler.ClassifyTier(tick, lastActive), true
	}
	if !applied {
		// 乐观并发冲突：自读取以来被战斗/HTTP 改过 → 不覆盖、退避，COLD 稍后再来。
		atomic.AddInt64(&r.st.conflicted, 1)
		return scheduler.ClassifyTier(tick, lastActive), true
	}
	r.countAction(action)
	// 主动响应需求（觅食/社交）→ 标记活跃 → HOT（继续盯着）；被动（休息/反思）→ 按既有活跃度自然降温至 COLD。
	if active {
		if err := r.units.TouchLastActiveTick(ctx, job.UnitID, tick); err != nil {
			r.log.Warn("region-runner touch last active", "unit", job.UnitID, "error", err)
		}
		return scheduler.TierHot, true
	}
	return scheduler.ClassifyTier(tick, lastActive), true
}

// applyAction 经 Mutator 乐观写落地一个动作的效果。返回 (是否主动响应, 是否真写入(applied), 错误)。
// applied=false（且 err=nil）= 乐观并发冲突（自读取以来被并发写者改过），调用方退避不覆盖（real-3-0 硬化）。
func (r *Runner) applyAction(ctx context.Context, unitID string, action ambientAction, tick int64) (active bool, applied bool, err error) {
	eff, ok := ambientEffects[action]
	if !ok {
		return false, false, fmt.Errorf("unknown ambient action %q", action)
	}
	if _, err := r.mutator.ApplyOptimistic(ctx, status.Mutation{
		UnitID: unitID, Turn: int(tick), Field: eff.field, Delta: eff.delta, ReasonCode: eff.reason,
	}); err != nil {
		if errors.Is(err, status.ErrConcurrentModification) {
			return eff.active, false, nil // 冲突：退避，非真错误
		}
		return eff.active, false, err
	}
	return eff.active, true, nil
}

// countAction 按动作累计遥测。
func (r *Runner) countAction(action ambientAction) {
	switch action {
	case actForage:
		atomic.AddInt64(&r.st.foraged, 1)
	case actRest:
		atomic.AddInt64(&r.st.rested, 1)
	case actSocialize:
		atomic.AddInt64(&r.st.socialized, 1)
	case actReflect:
		atomic.AddInt64(&r.st.reflected, 1)
	}
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
	r.releaseHeldRegions() // 退出时主动释放持租的 region，让别的实例立刻可接管（flag 关时 no-op）。
	r.log.Info("region-runner stopped", "stats", r.Stats())
}

// releaseHeldRegions 在 runner 退出时释放本实例当前持有的所有 region 租约（别的实例可立即抢占接管）。
// 用独立的短超时 context（Run 的 ctx 此刻已取消，无法再做 DB 写）。flag 关时 ReleaseLease no-op、不触 DB。
func (r *Runner) releaseHeldRegions() {
	regions := r.heldRegionList()
	if len(regions) == 0 {
		return
	}
	relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, reg := range regions {
		if err := r.leases.ReleaseLease(relCtx, reg.RegionID, r.instanceID); err != nil {
			r.log.Warn("region-runner release lease on stop", "region", reg.RegionID, "error", err)
		}
	}
	r.heldMu.Lock()
	r.heldRegions = make(map[string]agentqueue.WakeRegion)
	r.heldMu.Unlock()
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
		"socialized":    atomic.LoadInt64(&r.st.socialized),
		"reflected":     atomic.LoadInt64(&r.st.reflected),
		"deferred":      atomic.LoadInt64(&r.st.deferred),
		"dropped":       atomic.LoadInt64(&r.st.dropped),
		"conflicted":    atomic.LoadInt64(&r.st.conflicted),
		"settled":       atomic.LoadInt64(&r.st.settled),
		// region 分片遥测：instance_id 本实例 ID；leases_enabled flag 是否开（开才真互斥）；leases_acquired 抢到的 region 次数；
		// leases_lost 续租失败丢区；regions_skipped 因他人持租而跳过的 region 次数；regions_held 当前持租 region 数。
		"instance_id":     r.instanceID,
		"leases_enabled":  LeasesEnabled(),
		"leases_acquired": atomic.LoadInt64(&r.st.leasesAcquired),
		"leases_lost":     atomic.LoadInt64(&r.st.leasesLost),
		"regions_skipped": atomic.LoadInt64(&r.st.regionsSkipped),
		"regions_held":    len(r.heldRegionList()),
		// real-3 HOT-LLM 遥测：llm_enabled 是否注入了客户端；llm_calls 成功经 LLM 决策次数；llm_fallbacks LLM 失败回退反射；
		// llm_spent_usd 进程级累计成本；llm_latched 预算是否已耗尽闩死（此后全转反射）。
		"llm_enabled":   r.llm != nil,
		"llm_calls":     atomic.LoadInt64(&r.st.llmCalls),
		"llm_fallbacks": atomic.LoadInt64(&r.st.llmFallbacks),
		"llm_spent_usd": float64(atomic.LoadInt64(&r.llmSpentMicroUSD)) / 1e6,
		"llm_latched":   atomic.LoadInt32(&r.llmLatched) == 1,
		// PvE 遥测：threats_enabled 是否 roll 威胁；rolled 命中威胁次数；encountered 升级为遭遇；fled HP 危急撤退；
		// encounter_errors 真遭遇失败（threatHandler 注入后才可能 >0；shadow 模式 encountered 计数但不真触发）。
		"threats_enabled":     r.threatsEnabled,
		"threats_rolled":      atomic.LoadInt64(&r.st.threatsRolled),
		"threats_encountered": atomic.LoadInt64(&r.st.threatsEncountered),
		"threats_fled":        atomic.LoadInt64(&r.st.threatsFled),
		"encounter_errors":    atomic.LoadInt64(&r.st.encounterErrors),
	}
}
