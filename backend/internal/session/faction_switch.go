package session

// 文件说明：阵营改换（阵营开放世界 F2，设计「道德漂移 + 自治偏置 + 概率切换阵营」之③）。
//
// 角色道德漂移日久，若其数值道德轴的**主导阵营**长期背离当前阵营、且满足**全部隐藏条件**，才有小概率
// 涌现式改换门庭（绝非玩家点按、绝不轻易切）。这是涌现自治的高光时刻，经命运卡 surface（祖魂语气 + 漂移因 provenance）。
//
// 隐藏条件（全满足才有资格 roll，缺一即不切；玩家侧不明示条件/进度——「隐藏」）：
//   - 条件①亲和度差：主导阵营亲和度 − 当前阵营亲和度 ≥ switchAffinityMargin（20）。
//   - 条件②背离连击：MoralDriftStreak ≥ switchStreakTurns（背离已持续够久，由 moral_drift 的 refreshMoralDriftStreak 累积）。
//   - 条件③概率掷骰：确定性 FNV(sessionID+turn+actor) 掷骰过 switchProbability（即便前两条满足也只是小概率切）。
//
// 全满足 → 改 record.Faction = 主导阵营、连击归零 → 经 SurfaceFateEvent 出命运卡（高光/待决策由相关性定档）。
//
// flag QUNXIANG_FACTION_SWITCH **默认关** → maybeSwitchFaction 直接早返回（零行为：不算条件、不 roll、不切、不留痕）。
// best-effort：缺依赖 / 不满足条件 / 写库失败 → 优雅返回（绝不阻断回合推进）。确定性、付费不进（不读 wallet/billing）。

import (
	"context"
	"fmt"
	"hash/fnv"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
)

// factionSwitchFlagEnv 是阵营切换的灰度开关环境变量名。**默认开** → 显式 false/0/no/off 可关，关时 maybeSwitchFaction 直接早返回（零行为）。
const factionSwitchFlagEnv = "QUNXIANG_FACTION_SWITCH"

// 阵营切换的隐藏条件阈值（确定性、苛刻——绝不轻易切）。
const (
	// switchAffinityMargin 条件①：主导阵营亲和度须超当前阵营亲和度此值（道德轴口径，[0,100] 量级），才算「明显背离」。
	switchAffinityMargin = 20.0
	// switchStreakTurns 条件②：主导阵营持续背离当前阵营须达此连击回合数，才算「背离已稳固非一时偏移」。
	switchStreakTurns = 6
	// switchProbability 条件③：前两条都满足时、本回合真正改换的确定性掷骰阈（小概率——苛刻条件 + 低概率双保险）。
	switchProbability = 0.15
)

// factionSwitchEnabled 读 QUNXIANG_FACTION_SWITCH，默认开（显式 false/0/no/off 可关）。开时才启用阵营切换。
func factionSwitchEnabled() bool {
	return featureflags.EnabledWithDefault(factionSwitchFlagEnv, true)
}

// factionAffinity 取单位道德轴对某阵营的亲和度（[0,100]）——某阵营 ID 对应道德轴的同名分量。
// 未知阵营返回 0。纯函数、确定性。
func factionAffinity(m faction.MoralAlignment, factionID string) float64 {
	switch faction.Normalize(factionID) {
	case faction.IDFreedom:
		return m.Freedom
	case faction.IDOrder:
		return m.Order
	case faction.IDChaos:
		return m.Chaos
	default:
		return 0
	}
}

// factionSwitchDecision 是一次切换资格评估的结构化结果（便于单测逐条件断言 + 复盘「为什么切/没切」）。
type factionSwitchDecision struct {
	Eligible      bool    // 三条隐藏条件是否全满足（含概率掷骰）
	Dominant      string  // 漂移后主导阵营 ID
	Current       string  // 当前阵营 ID
	AffinityDelta float64 // 主导阵营亲和度 − 当前阵营亲和度
	Streak        int     // 背离连击回合数
	Roll          float64 // 确定性掷骰值 [0,1)
	MetMargin     bool    // 条件①
	MetStreak     bool    // 条件②
	MetRoll       bool    // 条件③
}

// evaluateFactionSwitch 是切换资格的**纯函数**核心：据道德轴/当前阵营/连击/掷骰判定三条隐藏条件。
//
// 确定性、可单测、零副作用、零 IO。flag 不在此判（由调用方 maybeSwitchFaction 先 gate）——本函数只给纯逻辑判定，
// 便于「flag 关不切」与「条件逻辑」分开测。注意：主导阵营 == 当前阵营 / 无主导 / 无当前阵营 → 直接不合格（无背离）。
func evaluateFactionSwitch(m faction.MoralAlignment, currentFaction string, streak int, roll float64) factionSwitchDecision {
	current := faction.Normalize(currentFaction)
	dominant := faction.DominantFaction(m)
	out := factionSwitchDecision{
		Dominant: dominant,
		Current:  current,
		Streak:   streak,
		Roll:     roll,
	}
	if current == "" || dominant == "" || dominant == current {
		return out // 无背离（一致/无主导/无阵营）→ 不合格。
	}
	out.AffinityDelta = factionAffinity(m, dominant) - factionAffinity(m, current)
	out.MetMargin = out.AffinityDelta >= switchAffinityMargin
	out.MetStreak = streak >= switchStreakTurns
	out.MetRoll = roll < switchProbability
	out.Eligible = out.MetMargin && out.MetStreak && out.MetRoll
	return out
}

// factionSwitchRoll 生成与会话上下文绑定的确定性掷骰值 [0,1)（FNV-32a，复用 obedience deterministicDirectiveRoll 口径）。
func factionSwitchRoll(sessionID string, turn int, actorID string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(sessionID))
	_, _ = hasher.Write([]byte(actorID))
	_, _ = hasher.Write([]byte(fmt.Sprintf("|%d|factionswitch", turn)))
	return float64(hasher.Sum32()%10000) / 10000
}

// maybeSwitchFaction 在回合边界尝试一次涌现式阵营改换（flag 门控、隐藏条件、概率）。
//
// flag QUNXIANG_FACTION_SWITCH 关（默认）→ 直接返回 false（零行为）。开时：评估三条隐藏条件，全满足才改
// record.Faction = 主导阵营、连击归零、落 FACTION_SWITCH 留痕，并经 SurfaceFateEvent 出命运卡（祖魂语气 + 漂移因）。
//
// 在 settleAutonomyAtDeploymentBoundary 紧随 settleMoralDrift 之后调用（漂移先把连击/道德轴算好，再判切换）。
// best-effort：缺依赖 / 不合格 / 写库失败 → 优雅返回 false（绝不阻断主链路）。返回是否真的切了。
func (service *Service) maybeSwitchFaction(ctx context.Context, sessionID string, record *unit.Record, turn int) bool {
	if service == nil || service.db == nil || record == nil || record.ID == "" {
		return false
	}
	if !factionSwitchEnabled() {
		return false // flag 关：零行为（不算条件、不 roll、不切、不留痕）。
	}
	if ctx == nil {
		ctx = context.Background()
	}

	roll := factionSwitchRoll(sessionID, turn, record.ID)
	decision := evaluateFactionSwitch(record.MoralAlignment, record.Faction, record.MoralDriftStreak, roll)
	if !decision.Eligible {
		return false
	}

	oldFaction := record.Faction
	oldZH := factionDisplayZH(oldFaction)
	newZH := factionDisplayZH(decision.Dominant)

	// 改非保护字段（直接读写，不走 Mutator）：切阵营 + 连击归零（已认了新归属、不再背离）。
	record.Faction = decision.Dominant
	record.MoralDriftStreak = 0
	if err := service.units.Save(ctx, *record); err != nil {
		// 写库失败：回滚内存改动，best-effort 返回 false（不留痕、不出卡——状态一致优先）。
		record.Faction = oldFaction
		return false
	}

	// FACTION_SWITCH 留痕（流程事件旁路；Faction 非受保护字段，不走 status.Mutator）。
	provenanceZH := fmt.Sprintf("她的心，渐渐偏离了%s，认了%s——背离已持续 %d 个回合，新归属的亲和已超出旧阵营 %.0f。",
		oldZH, newZH, decision.Streak, decision.AffinityDelta)
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: record.ID,
		Code:        events.ReasonFactionSwitch,
		Category:    events.CategoryLifecycle,
		Payload: map[string]any{
			"from":           oldFaction,
			"to":             decision.Dominant,
			"affinity_delta": decision.AffinityDelta,
			"drift_streak":   decision.Streak,
			"provenance_zh":  provenanceZH,
		},
		Tick: turn,
	}); err != nil {
		// 切换已落库；留痕失败只吞错（不回滚——状态变更胜过审计缺一条）。
		return true
	}

	// 经命运卡 surface（高光/待决策由相关性定档）：祖魂语气 + 漂移因 provenance。best-effort，绝不阻断。
	cardSummary := fmt.Sprintf("她的心，渐渐偏离了%s，认了%s……", oldZH, newZH)
	_, _ = service.SurfaceFateEvent(ctx, sessionID, record, FateEvent{
		ActorID:       record.ID,
		TargetID:      record.ID,
		ReasonCode:    events.ReasonFactionSwitch,
		Importance:    8,
		EmotionWeight: 0.8,
		Summary:       cardSummary,
		AttributionZH: provenanceZH,
	})

	service.pushRealtime(sessionID, "faction_switch", map[string]any{
		"unit_id": record.ID,
		"from":    oldFaction,
		"to":      decision.Dominant,
	})
	return true
}

// factionDisplayZH 返回阵营中文名（未知阵营回落「无阵营」）。
func factionDisplayZH(factionID string) string {
	if def, ok := faction.Get(factionID); ok {
		return def.NameZH
	}
	return "无阵营"
}
