package regionrunner

// 文件说明：region-runner PvE 接入（沙盘 §8.2 / docs/PvE威胁系统.md / docs/region-runner-PvE接入方案.md）——
// 让被唤醒的离线 **HOT（正活跃）** 单位会在路上撞见 elite 硬茬。决策走 engine/decision.Router 的关键节点闸：
// HP 危急先反射撤退保命（零 LLM），否则 StrategicFork 升级为遭遇。威胁刷新 MVP 用**简化确定性概率**
// （完整的 region.threat_level 累积 + 锚加权选址登记后续）。整块 flag-gated（QUNXIANG_REGION_RUNNER_THREATS，默认关）。
// 真遭遇结算经**注入式 threatHandler**（main 注入 session.TriggerEliteEncounter，保持 regionrunner 不依赖 session）；
// 未注入（PvE-1 shadow）则只计遥测、不改单位。
//
// 并发硬化（PvE-2 + PvE-3，与 real-2→real-3-0 同款）：触发 handler 前 maybeEncounterThreat 已查一次让位（execGuard=
// IsExecutionRunning），故已在异步战斗执行中的会话不会触发遭遇。**PvE-3 已落地**：session.ResolveEliteEncounter 的每回合
// HP/钱包/士气写已改用 status.Mutator.ApplyOptimistic + 冲突重试（applyEliteMutation），覆盖**所有并发写者**（不止异步战斗，
// 还有 execGuard 不覆盖的部署期 HTTP 写）——遭遇**进行中**会话进入战斗/HTTP 写时，遭遇的写冲突即重读重试、**绝不覆盖**战斗/HTTP
// 的写（有并发不变量测试证「战斗 hunger 写永不被遭遇覆盖」）。至此战斗可达会话开 THREATS_APPLY 已安全。

import (
	"context"
	"hash/fnv"
	"sync/atomic"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/unit"
)

const (
	// 锚加权威胁概率参数（PvE-4，‰=千分比）。每次 HOT 唤醒的威胁概率 = region 威胁度项 + 锚密度项，夹 [破圈下限, 上限]。
	// HOT 单位 ~每 TickSeconds 才唤醒一次，故真实遭遇频率远低于这些 per-tick ‰。
	threatBaseLevel            = 50 // region 基线威胁度 [0,100]（MVP 常量；完整版随后台世界事件累积 + 上限/冷却）
	threatLevelPerMilleAtFull  = 20 // threat_level=100 时贡献概率(‰)
	threatAnchorPerMilleAtFull = 60 // anchor_density=1 时贡献概率(‰)——越在乎，威胁越易找上她
	threatFloorPerMille        = 5  // 破圈下限(‰)：零锚单位也有的最低威胁概率（世界仍有危险，不全扎堆）
	threatMaxPerMille          = 80 // 概率上限(‰)
	// hpMaxForThreat 是 HP clamp 上限（mutator FieldHP clampInt(0,100)），喂 Router 的 HP/HPMax 危急护栏。
	hpMaxForThreat = 100
)

// SetThreatHandler 注入「真遭遇结算」回调（main 注入 session.TriggerEliteEncounter 包装）。
// 不注入（PvE-1 shadow）则威胁只计遥测、不改单位。nil-safe。
func (r *Runner) SetThreatHandler(handler func(ctx context.Context, sessionID string, unitID string) error) {
	if r == nil {
		return
	}
	r.threatHandler = handler
}

// SetAnchorDensityProvider 注入「某单位在乎程度」的密度查询（main 接 session.AnchorDensity），用于锚加权威胁刷新（PvE-4）。
// 不注入则密度恒 0 → 威胁只按 region 基线 + 破圈下限（不按锚加权）。保持 regionrunner 不依赖 session。nil-safe。
func (r *Runner) SetAnchorDensityProvider(provider func(ctx context.Context, unitID string) float64) {
	if r == nil {
		return
	}
	r.anchorDensity = provider
}

// threatRoll1000 是确定性均匀抽样 [0,1000)（FNV-64a(sessionID:unitID:tick) mod 1000）。
// 不依赖全局 rand、可复现（与 session 模拟逻辑一致）；与阈值 threatSpawnPerMille 比较决定是否撞威胁。
func threatRoll1000(sessionID string, unitID string, tick int64) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{':'})
	_, _ = h.Write([]byte(unitID))
	_, _ = h.Write([]byte{':'})
	var tb [8]byte
	for i := 0; i < 8; i++ {
		tb[i] = byte(tick >> (8 * i))
	}
	_, _ = h.Write(tb[:])
	return int(h.Sum64() % 1000)
}

// threatSpawnPerMille 把 region 威胁度 + 锚密度算成本次唤醒的威胁概率(‰)——anchor 越高威胁越易找上她（天然扎堆她在乎处），
// 零锚仍有破圈下限（世界仍有危险）。完整版会把 threatBaseLevel 换成随世界事件累积的 region.threat_level 并加 freshness 反扎堆。
func threatSpawnPerMille(anchorDensity float64) int {
	if anchorDensity < 0 {
		anchorDensity = 0
	} else if anchorDensity > 1 {
		anchorDensity = 1
	}
	pm := threatLevelPerMilleAtFull*threatBaseLevel/100 + int(float64(threatAnchorPerMilleAtFull)*anchorDensity+0.5)
	if pm < threatFloorPerMille {
		pm = threatFloorPerMille
	}
	if pm > threatMaxPerMille {
		pm = threatMaxPerMille
	}
	return pm
}

// situationFromRecord 把单位状态填成 decision.Situation：StrategicFork=true（撞威胁=战略岔路，触发关键节点闸），
// HP/Hunger 喂 Router 的 L1 护栏（HP<25% 危急即撤退）。HasRation 留 false——饥饿由 ambient 层处理，威胁时只关心保命。
func situationFromRecord(record unit.Record, tick int64) decision.Situation {
	return decision.Situation{
		UnitID:        record.ID,
		Tick:          int(tick),
		HP:            record.Status.HP,
		HPMax:         hpMaxForThreat,
		Hunger:        record.Status.Hunger,
		EnemyInSight:  true,
		FirstContact:  true,
		StrategicFork: true,
	}
}

// maybeEncounterThreat：HOT 单位确定性 roll 威胁；命中则过 Router——HP 危急撤退保命 / 否则关键节点遭遇。
// 返回 handled=true 表示本次唤醒被威胁消耗（调用方据返回 tier 重排、不再走日常 ambient）。
// flag 关 / 非 HOT / 未命中 → handled=false（继续日常 ambient）。真遭遇仅在注入了 handler 时发生（PvE-2）；
// shadow（handler==nil）只计遥测。触发 handler 前再查一次让位，收窄「读 record 后会话刚进战斗」的并发窗口。
func (r *Runner) maybeEncounterThreat(ctx context.Context, job *agentqueue.DecisionJob, record unit.Record, currentTier scheduler.Tier, tick int64) (bool, scheduler.Tier) {
	if !r.threatsEnabled || currentTier != scheduler.TierHot {
		return false, scheduler.TierCold
	}
	// 锚加权（PvE-4）：越在乎的角色威胁越易找上她。先抽样、仅当 draw 落在「锚相关区间」[floor,max) 才查锚密度——
	// draw≥max 必不命中、draw<floor 必命中（破圈），都无需查密度，省掉约 92% 的 anchorDensity DB 查询。
	// （anchorDensity 变化慢，未来可进一步缓存；当前按需查已足够稀疏。）
	draw := threatRoll1000(job.SessionID, job.UnitID, tick)
	if draw >= threatMaxPerMille {
		return false, scheduler.TierCold // 超过任何可能阈值 → 必不命中
	}
	if draw >= threatFloorPerMille { // 破圈下限以上：按锚密度阈值判定（以下则必命中）
		density := 0.0
		if r.anchorDensity != nil {
			density = r.anchorDensity(ctx, job.UnitID)
		}
		if draw >= threatSpawnPerMille(density) {
			return false, scheduler.TierCold
		}
	}
	atomic.AddInt64(&r.st.threatsRolled, 1)

	dec := r.threatRouter.Route(situationFromRecord(record, tick))
	if dec.Intent.Action == decision.ActionFlee {
		// L1 护栏：HP 危急 → 撤退保命、不应战。本次唤醒用于规避，HOT 重排（仍紧张）。
		atomic.AddInt64(&r.st.threatsFled, 1)
		return true, scheduler.TierHot
	}

	// 关键节点（StrategicFork）→ 遭遇。
	atomic.AddInt64(&r.st.threatsEncountered, 1)
	if r.threatHandler != nil {
		if r.execGuard(job.SessionID) {
			atomic.AddInt64(&r.st.deferred, 1)
			return true, scheduler.TierHot
		}
		if err := r.threatHandler(ctx, job.SessionID, job.UnitID); err != nil {
			atomic.AddInt64(&r.st.encounterErrors, 1)
			r.log.Warn("region-runner threat encounter", "unit", job.UnitID, "error", err)
		}
	}
	return true, scheduler.TierHot
}
