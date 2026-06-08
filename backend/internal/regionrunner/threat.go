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
	// threatChancePerMille 是 HOT 单位每次唤醒撞见威胁的确定性概率（千分比）。MVP 取较稀疏值，呼应「关键节点稀疏」；
	// HOT 单位 ~每 TickSeconds 才唤醒一次，故真实遭遇频率远低于此。完整版（region.threat_level + 锚加权选址）会取代固定概率。
	threatChancePerMille = 30 // 3%
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

// rollThreat 确定性判定某单位本 tick 是否撞见威胁（FNV-64a(sessionID:unitID:tick) mod 1000 < 概率）。
// 确定性——同 sessionID+unitID+tick 必同结果，不依赖全局 rand，可复现（与 session 模拟逻辑一致）。
func rollThreat(sessionID string, unitID string, tick int64) bool {
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
	return h.Sum64()%1000 < threatChancePerMille
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
	if !r.threatsEnabled || currentTier != scheduler.TierHot || !rollThreat(job.SessionID, job.UnitID, tick) {
		return false, scheduler.TierCold
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
