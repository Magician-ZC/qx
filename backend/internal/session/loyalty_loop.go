package session

// 文件说明：loyalty_loop.go，忠诚负反馈闭环（设计 §5.7「越按越不听」）。
//
// 背景：reason code ReasonCommandForced/ReasonCommandAccepted/ReasonLoyaltyGain/ReasonLoyaltyStrain
// 早已在 events 目录登记，但在指令服从主链路上零 Mutator 调用方——忠诚只会在 fate/combat_shake 等
// 旁路被改，「玩家越强按、单位越离心；顺其本心、单位越归心」的核心负反馈一直不存在。本文件把这条闭环
// 接到指令服从判定（resolveDirectiveCompliance）的结果上：每次执行决策落地后，由调用方调用
// settleLoyaltyFromCompliance，按「被强令做了违心的事 → 扣忠诚（越按越不听，重复累积）」「顺其本心
// 服从玩家方针 → 加忠诚（归心）」结算一笔经 Mutator 的小步长忠诚变更。
//
// 设计原则（与本仓库其余忠诚改动一致）：
//   - 忠诚（Status.Loyalty）值域 [0,1]，clamp 由 status.Mutator 处理；步长保守（0.02~0.045 量级），
//     与 fate.go(±0.03/±0.04)、combat_shake(-0.05) 同量级，绝不用 0.2+ 的大步长（那是 0-100 量级的误读）。
//   - 全部经 status.Mutator（applyStatusMutation），受保护字段不直改、可审计、有标准事件行。
//   - 确定性：所有判定仅依赖 obedienceResolution + 状态 + DirectiveHistory 计数，无随机。
//   - best-effort：忠诚结算失败绝不中断执行主循环；调用方对返回的 err 仅作 best-effort 处理。

import (
	"context"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

// 忠诚闭环步长常量（[0,1] 量级，保守小步长）。
const (
	// loyaltyForcedBaseStrain 是「被即时令强令做违心事」的基础离心步长。
	loyaltyForcedBaseStrain = 0.025
	// loyaltyForcedStrainPerRepeat 是「越按越不听」的重复累积增量：本会话内每多一道点名即时令，
	// 再被强令时的离心代价线性叠加这一步长（玩家越频繁地强按同一个人，她越离心）。
	loyaltyForcedStrainPerRepeat = 0.015
	// loyaltyForcedStrainCap 是单笔强令离心的上限，避免重复累积把单笔扣得过猛（仍由 Mutator clamp 兜底）。
	loyaltyForcedStrainCap = 0.08
	// loyaltyReluctantStrain 是「高风险方针下违心顺从（concerned/reluctant，非即时令强制）」的轻微离心步长。
	loyaltyReluctantStrain = 0.015
	// loyaltyAlignedGain 是「顺其本心服从玩家方针（steady、低风险）」的归心步长。
	loyaltyAlignedGain = 0.02
)

// settleLoyaltyFromCompliance 在一次指令服从判定（resolveDirectiveCompliance）落地后结算忠诚闭环。
//
// 接入点：执行主循环里 resolveDirectiveCompliance → logDirectiveCompliance 之后调用本方法（actor 在玩家阵营、
// 处于玩家指令管辖下时才会有非零结算）。actor 指针在调用前应已是本 tick 的最新状态；本方法经 Mutator 改其忠诚
// 并回写 actor 指针（与 applyStatusMutation 的 `*record = result.Record` 语义一致）。
//
// 结算分支（互斥，至多一笔）：
//  1. 强令违心（ForcedByImmediateOrder && WouldDefyUnderForce）→ ReasonCommandForced 离心，
//     步长随本会话对该单位的点名即时令次数累积（越按越不听），封顶 loyaltyForcedStrainCap。
//  2. 违心顺从（非强令的 concerned/reluctant/refused，即高风险方针下被压着执行/抗命）→ ReasonLoyaltyStrain
//     轻微离心。注意：refused（彻底抗命）也算离心——她已经在离你而去，这一步只是把它落到忠诚数值上。
//  3. 顺其本心（steady 且在玩家指令下、风险不高）→ ReasonLoyaltyGain 归心。
//
// best-effort：返回的 error 由调用方按「不中断主循环」处理。
func (service *Service) settleLoyaltyFromCompliance(
	ctx context.Context,
	state *State,
	actor *unit.Record,
	compliance obedienceResolution,
) error {
	if service == nil || service.mutator == nil || state == nil || actor == nil {
		return nil
	}
	// 仅在玩家指令管辖下结算：无玩家指令时单位是纯自主，不存在「听不听话」的忠诚问题。
	if !compliance.HasPlayerDirective {
		return nil
	}
	if actor.FactionID != state.PlayerFactionID {
		return nil
	}
	if actor.Status.LifeState != unit.LifeStateActive {
		return nil
	}

	delta, reasonCode, reasonText := loyaltyDeltaFromCompliance(state, actor, compliance)
	if delta == 0 {
		return nil
	}
	return service.applyStatusMutation(ctx, state, actor, status.FieldLoyalty, delta, reasonCode, reasonText)
}

// loyaltyDeltaFromCompliance 把一次服从判定映射为「忠诚增量 + reason code + 旁白」。
// 纯函数、确定性（仅依赖 compliance 状态与 DirectiveHistory 计数）；delta==0 表示本次不结算忠诚。
// 抽出为独立函数便于单测，且让结算分支与步长一目了然。
func loyaltyDeltaFromCompliance(
	state *State,
	actor *unit.Record,
	compliance obedienceResolution,
) (float64, events.ReasonCode, string) {
	// 无玩家指令管辖（纯自主行为）一律零结算——「听不听话」的忠诚问题只在玩家下了命令时才存在。
	// 此处显式守门，使本纯函数在隔离调用下也自洽（不依赖调用方先行过滤）。
	if !compliance.HasPlayerDirective {
		return 0, "", ""
	}
	switch {
	// 分支 1：被即时令强令做了违心的事 → 越按越不听（重复累积）。
	case compliance.ForcedByImmediateOrder:
		if !compliance.WouldDefyUnderForce {
			// 强令做的是她本来也愿意做的事：不扣忠诚（避免「下达任何即时令都掉好感」的错觉）。
			return 0, "", ""
		}
		repeats := countForcedOrdersForUnit(*state, actor.ID)
		strain := loyaltyForcedBaseStrain + float64(maxInt(repeats-1, 0))*loyaltyForcedStrainPerRepeat
		if strain > loyaltyForcedStrainCap {
			strain = loyaltyForcedStrainCap
		}
		text := "你又一次强按着她去做她不愿做的事，她更不听你了。"
		if repeats <= 1 {
			text = "你强按着她去做她不愿做的事，她心里生了疏离。"
		}
		return -strain, events.ReasonCommandForced, text

	// 分支 2：高风险方针下违心顺从 / 抗命（非即时令强制）→ 轻微离心。
	case compliance.State == obedienceConcerned ||
		compliance.State == obedienceReluctant ||
		compliance.State == obedienceRefused:
		return -loyaltyReluctantStrain, events.ReasonLoyaltyStrain, "你的命令一次次拧着她的本心，她离你越来越远。"

	// 分支 3：steady 且在玩家指令下、风险不高 → 顺其本心，归心。
	case compliance.State == obedienceSteady && complianceIsAligned(compliance):
		return loyaltyAlignedGain, events.ReasonLoyaltyGain, "你的安排正合她的心意，她更认你了。"

	default:
		return 0, "", ""
	}
}

// complianceIsAligned 判定一次「steady 顺从」是否构成「顺其本心」、值得归心一笔。
//
// 保守门槛（避免每个安静 tick 都白嫖忠诚）：必须确有玩家指令管辖（HasPlayerDirective），
// 且本次指令对她而言带一定进攻/执行强度（RiskScore>0.1，即 directiveOrderRisk 不是「无进攻意图」的兜底 0.1），
// 又没有触发任何违心信号（State==steady）。即「玩家明确下了有分量的命令、她欣然照办」才算顺其本心。
func complianceIsAligned(compliance obedienceResolution) bool {
	if !compliance.HasPlayerDirective {
		return false
	}
	// directiveOrderRisk 对「无进攻强度的指令」返回 0.1 兜底；>0.1 才说明这是一道有分量、她本可纠结却照办的命令。
	return compliance.RiskScore > 0.1
}

// countForcedOrdersForUnit 统计本会话（DirectiveHistory 全程）中点名下给该单位的即时令（order）条数，
// 用于「越按越不听」的重复累积。确定性、纯计数，无随机。
func countForcedOrdersForUnit(state State, unitID string) int {
	unitID = strings.TrimSpace(unitID)
	if unitID == "" {
		return 0
	}
	count := 0
	for index := range state.DirectiveHistory {
		directive := state.DirectiveHistory[index]
		if normalizeDirectiveKind(directive.Kind) != DirectiveKindOrder {
			continue
		}
		if directive.TargetUnitID == unitID && directive.AppliesTo == unitID {
			count++
		}
	}
	return count
}
