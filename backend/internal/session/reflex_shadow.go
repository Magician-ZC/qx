package session

// 文件说明：反射层(decision.Router)影子接入会话决策链路（设计宪法 §1.5「决策用 LLM、结算用代码」三层模型）。
// 接入策略：影子模式——每一次「本会真的去调 LLM」的决策前，先用纯代码反射层(decision.Router.Route)算一遍，
// 仅统计「反射层本可零成本拿出意图、本可省下这次 LLM」的比例，不改变任何实际行为。
// 目的：用真实流量量化反射层能省下多少 LLM 调用（§11.2 头号成本/延迟杠杆），为后续「真正短路 LLM」提供依据。
//
// 诚实口径：当前是战斗会话，几乎每个 tick 都有敌在视野且玩家在场（高光节点），按 gate 多数应上 LLM；
// 反射层主要在「濒死撤退」「无敌可打的安静 tick」这两类省下调用。开放大世界的海量空闲 tick 才是反射层的主场。

import (
	"sync/atomic"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/unit"
)

// HP 上限约定：本项目 HP 以 100 为上限（与 threat.go、status 阈值一致）。
const reflexHPMaxConvention = 100

// 进程级反射层影子/短路遥测（跨所有会话累计）。
var (
	reflexTotal          atomic.Int64 // 进入决策路径的总次数
	reflexCouldSkip      atomic.Int64 // 其中反射层本可零成本处理、本可省下 LLM 的次数
	reflexShortCircuited atomic.Int64 // 其中**真**短路、实际省下 LLM 的次数（reflexShortCircuit 开启时）
)

// ReflexStats 返回进程级累计：决策总数、反射层本可省下 LLM 的次数、真短路实际省下的次数。
func ReflexStats() (total int64, couldSkip int64, shortCircuited int64) {
	return reflexTotal.Load(), reflexCouldSkip.Load(), reflexShortCircuited.Load()
}

// reflexShortCircuitApplies 判断本次决策是否可零成本反射短路、跳过 LLM：仅「日常安静 tick」
// （反射层 NeedsLLM=false 且动作是 hold/continue）。安全反射（HP 危急撤退/进食等）**不短路**——
// 高风险时点值得花 LLM，且把安全反射映射成丰富 payload 成本/风险高。供 generateUnitDecision 在开关开启时调用。
func reflexShortCircuitApplies(state State, actor *unit.Record, targetIDs []string) bool {
	if actor == nil {
		return false
	}
	// 玩家本回合给该单位下了即时令（已扣指挥力的付费动作）→ **绝不短路**，交 LLM 落实其意图，否则会在安静 tick 静默吞掉
	// 玩家付费的命令（评审 load-bearing）。这是 buildReflexSituation 缺的廉价已知 gate 信号，等价于 HasNewOrder=true。
	if _, has := activeImmediateOrderForUnit(state, actor.ID); has {
		return false
	}
	dec := decision.DefaultRouter().Route(buildReflexSituation(state, actor, targetIDs))
	if dec.NeedsLLM {
		return false
	}
	switch dec.Intent.Action {
	case decision.ActionHold, decision.ActionContinue:
		return true
	default:
		return false
	}
}

// reflexShortCircuitResult 构造一次「反射短路」的非 LLM 结果（标记 provider/model 供审计，$0 成本）。
func reflexShortCircuitResult() ai.CompletionResult {
	return ai.CompletionResult{
		Provider:     "reflex",
		Model:        "shortcircuit",
		UsedFallback: true,
		Debug:        ai.CompletionDebug{FallbackCause: "reflex short-circuit: daily quiet tick, no LLM needed"},
	}
}

// buildReflexSituation 从会话上下文构造反射层输入快照（纯函数，不依赖 DB/LLM）。
// 保守填充：未知的前因一律取「更需要 LLM」的方向，使影子统计不高估可省比例。
func buildReflexSituation(state State, actor *unit.Record, targetIDs []string) decision.Situation {
	enemyInSight := len(targetIDs) > 0 || actor.Status.InCombat
	return decision.Situation{
		UnitID:         actor.ID,
		Tick:           state.TurnState.Turn,
		HP:             actor.Status.HP,
		HPMax:          reflexHPMaxConvention,
		Hunger:         actor.Status.Hunger,
		HasRation:      false, // 保守：不假设有口粮，反射进食在影子里不计，避免高估可省比例
		EnemyInSight:   enemyInSight,
		EnemyAdjacent:  actor.Status.InCombat,
		PlayerWatching: true, // 战斗会话默认玩家在场目睹（高光时点应上 LLM）
		// FirstContact/HasNewOrder/SocialOffer/StrategicFork 现阶段无廉价可知信号，留空（偏向需要 LLM）。
	}
}

// recordReflexShadow 影子记录一次决策：若反射层本可零成本处理（NeedsLLM=false），计入可省。
func recordReflexShadow(state State, actor *unit.Record, targetIDs []string) {
	if actor == nil {
		return
	}
	reflexTotal.Add(1)
	if !decision.DefaultRouter().Route(buildReflexSituation(state, actor, targetIDs)).NeedsLLM {
		reflexCouldSkip.Add(1)
	}
}
