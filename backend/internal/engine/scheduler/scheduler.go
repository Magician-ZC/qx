// Package scheduler 提供大世界 region-runner 的**纯逻辑唤醒原语**（沙盘 §8.2 / §9），不碰 DB、确定性、可测。
//
// 核心思想（§9 成本三道闸之一「ATB gauge 唤醒制」）：多数角色多 tick 才决策一次。每个单位按其「冷热分层」决定
// 下次在哪个世界 tick 唤醒——刚活跃过的 HOT 单位每 tick 都醒（它正卷在事件里），长期沉寂的 COLD 单位很久才醒一次，
// 从而把真实 LLM 决策压到 <2%。本包只给「分层 / 算下次唤醒 tick / 是否到点 / 在途背压闸」这几个判定，
// 由 region-runner（后续 M7.3-real）接 agent_wake_queue / agent_decision_jobs 表与执行编排。
package scheduler

// Tier 是单位的冷热活跃分层。
type Tier string

const (
	TierHot  Tier = "hot"  // 刚活跃过：每 tick 都唤醒（正卷在战斗/事件里）
	TierWarm Tier = "warm" // 近期活跃：低频唤醒
	TierCold Tier = "cold" // 长期沉寂：很久才唤醒一次（省算力）
)

// 分层与节奏的确定性默认参数（region-runner 可按 region 负载调，但默认值保证纯函数确定性）。
const (
	// 分层阈值：以「空闲 tick 数 = 当前 tick − 上次活跃 tick」划分。
	WarmIdleThreshold = 3  // 空闲 < 3 → HOT
	ColdIdleThreshold = 24 // 空闲 < 24 → WARM；否则 COLD

	// 各层唤醒间隔（tick）：HOT 每 tick、WARM 每 4 tick、COLD 每 16 tick。
	HotWakeInterval  = 1
	WarmWakeInterval = 4
	ColdWakeInterval = 16

	// DefaultMaxInFlight 是 worker 池全局在途决策上限（§9：worker 池全局在途上限 64 + batch 背压）。
	DefaultMaxInFlight = 64
)

// ClassifyTier 按「空闲 tick 数」给单位分层。idle<0（时钟回拨/坏数据）按 HOT 兜底（宁可多醒不漏醒）。
func ClassifyTier(currentTick int64, lastActiveTick int64) Tier {
	idle := currentTick - lastActiveTick
	switch {
	case idle < WarmIdleThreshold:
		return TierHot
	case idle < ColdIdleThreshold:
		return TierWarm
	default:
		return TierCold
	}
}

// WakeInterval 返回某层的唤醒间隔（tick）。
func WakeInterval(tier Tier) int64 {
	switch tier {
	case TierHot:
		return HotWakeInterval
	case TierWarm:
		return WarmWakeInterval
	default:
		return ColdWakeInterval
	}
}

// NextWakeTick 给定当前 tick 与分层，返回该单位下一次应唤醒的世界 tick（严格大于 currentTick，保证向前推进、不自旋）。
func NextWakeTick(tier Tier, currentTick int64) int64 {
	interval := WakeInterval(tier)
	if interval < 1 {
		interval = 1
	}
	return currentTick + interval
}

// PlanNextWake 是「单位刚被处理完、按其新鲜活跃度重排下次唤醒」的组合便捷式：
// 以 currentTick 为最近活跃点分层（idle=0 → HOT），返回下次唤醒 tick。region-runner 处理完一个唤醒后调它重排队。
func PlanNextWake(currentTick int64, lastActiveTick int64) (Tier, int64) {
	tier := ClassifyTier(currentTick, lastActiveTick)
	return tier, NextWakeTick(tier, currentTick)
}

// ShouldWake 判断某单位（其排定唤醒 tick 为 wakeAtTick）在 currentTick 是否到点。
func ShouldWake(wakeAtTick int64, currentTick int64) bool {
	return currentTick >= wakeAtTick
}

// AdmitDecision 是 worker 池的在途背压闸：当前在途 inFlight 个决策，是否还能再放行一个（§9 全局在途上限）。
// maxInFlight<=0 时回退 DefaultMaxInFlight。
func AdmitDecision(inFlight int, maxInFlight int) bool {
	if maxInFlight <= 0 {
		maxInFlight = DefaultMaxInFlight
	}
	return inFlight < maxInFlight
}
