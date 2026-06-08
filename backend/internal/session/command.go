package session

// 文件说明：command.go，玩家自然语言指令入口，负责 doctrine/task/order 解析、校验与落盘。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
)

// SetTaskDirective 以“任务指令”模式下发方针（战略/部署阶段可用）。
func (service *Service) SetTaskDirective(ctx context.Context, sessionID string, unitID string, text string) (Snapshot, error) {
	return service.SetFactionDirective(ctx, sessionID, "", DirectiveKindTask, unitID, text)
}

// SetImmediateOrder 以“即时令”模式下发命令（执行阶段可用）。
func (service *Service) SetImmediateOrder(ctx context.Context, sessionID string, unitID string, text string) (Snapshot, error) {
	return service.SetFactionDirective(ctx, sessionID, "", DirectiveKindOrder, unitID, text)
}

// SetFactionGlobalDirective 更新指定阵营的全局 doctrine。
func (service *Service) SetFactionGlobalDirective(
	ctx context.Context,
	sessionID string,
	commanderFactionID string,
	text string,
) (Snapshot, error) {
	return service.SetFactionDirective(ctx, sessionID, commanderFactionID, DirectiveKindDoctrine, "", text)
}

// SetPlayerDirective 兼容旧接口：按 kind 下发玩家指令。
func (service *Service) SetPlayerDirective(
	ctx context.Context,
	sessionID string,
	kind DirectiveKind,
	targetUnitID string,
	text string,
) (Snapshot, error) {
	return service.SetFactionDirective(ctx, sessionID, "", kind, targetUnitID, text)
}

// SetFactionDirective 统一处理 doctrine/task/order 三类指令下发。
func (service *Service) SetFactionDirective(
	ctx context.Context,
	sessionID string,
	commanderFactionID string,
	kind DirectiveKind,
	targetUnitID string,
	text string,
) (Snapshot, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	if state.SetupPhase == SetupPhaseDrafting {
		if _, draftErr := service.ApplyOpeningDraft(ctx, sessionID, nil); draftErr != nil {
			return Snapshot{}, draftErr
		}
		state, units, err = service.loadSession(ctx, sessionID)
		if err != nil {
			return Snapshot{}, err
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return Snapshot{}, fmt.Errorf("directive text is required")
	}
	// 内容安全审核（玩家输入侧，flag QUNXIANG_CONTENT_SAFETY 默认关→放行）：违规指令文本在落 DirectiveHistory 前即拒，不入库、不喂 LLM。
	if v := service.ModerateText(ctx, text, "input"); !v.Allowed {
		return Snapshot{}, fmt.Errorf("directive rejected by content safety: %s", v.Reason)
	}
	if state.Outcome != OutcomeOngoing {
		return Snapshot{}, fmt.Errorf("session is already finished")
	}
	commanderFactionID = normalizeCommanderFactionID(state, commanderFactionID)
	if commanderFactionID == "" {
		return Snapshot{}, fmt.Errorf("invalid commander faction")
	}
	ensureCommandPower(&state)

	kind = normalizeDirectiveKind(kind)
	directiveTurn := state.TurnState.Turn
	directivePhase := state.TurnState.Phase
	deferredToNextTurn := false
	switch kind {
	case DirectiveKindDoctrine:
		if state.TurnState.Phase == turns.PhaseExecution {
			directiveTurn++
			directivePhase = turns.PhaseDeployment
			deferredToNextTurn = true
		} else if state.TurnState.Phase != turns.PhaseDeployment {
			return Snapshot{}, fmt.Errorf("global directive can only be updated during deployment/execution phase")
		}
	case DirectiveKindTask:
		if state.TurnState.Phase == turns.PhaseExecution {
			directiveTurn++
			directivePhase = turns.PhaseDeployment
			deferredToNextTurn = true
		} else if state.TurnState.Phase != turns.PhaseDeployment {
			return Snapshot{}, fmt.Errorf("task directive is only available during deployment/execution phase")
		}
	case DirectiveKindOrder:
		if state.TurnState.Phase != turns.PhaseExecution {
			return Snapshot{}, fmt.Errorf("immediate order is only available during execution phase")
		}
	default:
		return Snapshot{}, fmt.Errorf("unknown directive kind")
	}

	intent := fallbackDirectiveIntent(units, commanderFactionID, targetUnitID, text)
	if kind == DirectiveKindTask || kind == DirectiveKindOrder {
		parsedIntent, _, interaction := service.parseDirectiveIntent(
			ctx,
			state,
			units,
			kind,
			commanderFactionID,
			targetUnitID,
			text,
		)
		intent = parsedIntent
		service.appendLLMInteractionWithSpend(ctx, &state, interaction)
	}

	if normalized := strings.TrimSpace(intent.NormalizedText); normalized != "" {
		text = normalized
	}
	priority := normalizeDirectivePriority(intent.Priority)
	if priority == "" {
		priority = "normal"
	}

	targetUnitID = resolveDirectiveTargetUnitID(
		units,
		commanderFactionID,
		targetUnitID,
		intent.TargetUnitID,
		intent.TargetUnitName,
		text,
	)
	if kind == DirectiveKindDoctrine {
		targetUnitID = ""
	}
	if kind == DirectiveKindOrder && targetUnitID == "" {
		return Snapshot{}, fmt.Errorf("immediate order must target an allied unit")
	}

	if targetUnitID != "" {
		targetRecord, ok := findDirectiveTarget(units, commanderFactionID, targetUnitID)
		if !ok || targetRecord.Status.LifeState != unit.LifeStateActive || targetRecord.Status.HP <= 0 {
			return Snapshot{}, fmt.Errorf("target unit is not available")
		}
	}

	if kind == DirectiveKindOrder {
		if state.CommandPower.Current < state.CommandPower.OrderCost {
			return Snapshot{}, fmt.Errorf("insufficient command power: %d/%d", state.CommandPower.Current, state.CommandPower.OrderCost)
		}
		state.CommandPower.Current -= state.CommandPower.OrderCost
	}
	strictTaskCost := 0
	if kind == DirectiveKindTask && isStrictDiplomacyDirective(text) {
		if state.CommandPower.Current < state.CommandPower.OrderCost {
			return Snapshot{}, fmt.Errorf("insufficient command power for strict task: %d/%d", state.CommandPower.Current, state.CommandPower.OrderCost)
		}
		state.CommandPower.Current -= state.CommandPower.OrderCost
		strictTaskCost = state.CommandPower.OrderCost
	}

	appliesTo := commanderFactionID
	if targetUnitID != "" {
		appliesTo = targetUnitID
	}
	directive := Directive{
		ID:           uuid.NewString(),
		Turn:         directiveTurn,
		Phase:        directivePhase,
		Kind:         kind,
		Text:         text,
		Priority:     priority,
		TargetUnitID: targetUnitID,
		IssuedAt:     time.Now().UTC(),
		IssuedBy:     commanderFactionID,
		AppliesTo:    appliesTo,
	}
	appendDirective(&state, directive)
	phaseReadyCleared := false
	if kind == DirectiveKindDoctrine && !deferredToNextTurn && state.TurnState.Phase == turns.PhaseDeployment {
		ensurePhaseReady(&state)
		if state.PhaseReady[commanderFactionID] {
			state.PhaseReady[commanderFactionID] = false
			phaseReadyCleared = true
		}
	}

	commanderLabel := factionCommanderLabel(state, commanderFactionID)
	switch kind {
	case DirectiveKindDoctrine:
		if deferredToNextTurn {
			appendLog(&state, "directive", fmt.Sprintf("%s预设了第 %d 回合全局方针：%s", commanderLabel, directiveTurn, text), "", "")
		} else if phaseReadyCleared {
			appendLog(&state, "directive", fmt.Sprintf("%s重新修改了全局方针，已取消进入下一阶段确认：%s", commanderLabel, text), "", "")
		} else {
			appendLog(&state, "directive", fmt.Sprintf("%s更新了全局方针：%s", commanderLabel, text), "", "")
		}
	case DirectiveKindTask:
		prefix := fmt.Sprintf("%s发布任务指令", commanderLabel)
		if deferredToNextTurn {
			prefix = fmt.Sprintf("%s预设第 %d 回合任务指令", commanderLabel, directiveTurn)
		}
		if targetUnitID == "" {
			if strictTaskCost > 0 {
				appendLog(
					&state,
					"task_directive",
					fmt.Sprintf("%s（%s）：%s。该严令消耗 %d 指挥力，剩余 %d/%d。", prefix, priority, text, strictTaskCost, state.CommandPower.Current, state.CommandPower.Max),
					"",
					"",
				)
			} else {
				appendLog(&state, "task_directive", fmt.Sprintf("%s（%s）：%s", prefix, priority, text), "", "")
			}
		} else {
			targetName := targetUnitDisplayName(units, targetUnitID)
			if strictTaskCost > 0 {
				appendLog(
					&state,
					"task_directive",
					fmt.Sprintf("%s向 %s（%s）：%s。该严令消耗 %d 指挥力，剩余 %d/%d。", prefix, targetName, priority, text, strictTaskCost, state.CommandPower.Current, state.CommandPower.Max),
					targetUnitID,
					"",
				)
			} else {
				appendLog(&state, "task_directive", fmt.Sprintf("%s向 %s（%s）：%s", prefix, targetName, priority, text), targetUnitID, "")
			}
		}
	case DirectiveKindOrder:
		targetName := targetUnitDisplayName(units, targetUnitID)
		appendLog(
			&state,
			"order_directive",
			fmt.Sprintf(
				"%s向 %s 下达即时令（%s）：%s。消耗 %d 指挥力，剩余 %d/%d。",
				commanderLabel,
				targetName,
				priority,
				text,
				state.CommandPower.OrderCost,
				state.CommandPower.Current,
				state.CommandPower.Max,
			),
			targetUnitID,
			"",
		)
	}
	if kind == DirectiveKindDoctrine && commanderFactionID == state.PlayerFactionID && !deferredToNextTurn {
		service.refreshEnemyGlobalDirectiveForDeploymentPhase(ctx, &state, units, "player_directive_updated")
	}

	if err := service.saveSession(ctx, &state); err != nil {
		return Snapshot{}, err
	}
	return buildSnapshot(state, units), nil
}

// findDirectiveTarget 校验目标单位是否属于下令阵营。
func findDirectiveTarget(units []unit.Record, commanderFactionID string, targetUnitID string) (unit.Record, bool) {
	for _, record := range units {
		if record.ID == targetUnitID && record.FactionID == commanderFactionID {
			return record, true
		}
	}
	return unit.Record{}, false
}

// targetUnitDisplayName 返回目标单位显示名（缺失时回落 ID/全队）。
func targetUnitDisplayName(units []unit.Record, targetUnitID string) string {
	if targetUnitID == "" {
		return "全队"
	}
	for _, record := range units {
		if record.ID == targetUnitID {
			return record.DisplayName()
		}
	}
	return targetUnitID
}

// normalizeDirectivePriority 规范化指令优先级枚举。
func normalizeDirectivePriority(priority string) string {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case "urgent":
		return "urgent"
	case "high":
		return "high"
	case "low":
		return "low"
	default:
		return "normal"
	}
}

// normalizeCommanderFactionID 规范化指挥官阵营标识。
func normalizeCommanderFactionID(state State, commanderFactionID string) string {
	commanderFactionID = strings.TrimSpace(commanderFactionID)
	if commanderFactionID == "" {
		return state.PlayerFactionID
	}
	if commanderFactionID == "player" {
		return state.PlayerFactionID
	}
	if commanderFactionID == "enemy" {
		return state.EnemyFactionID
	}
	if commanderFactionID == state.PlayerFactionID || commanderFactionID == state.EnemyFactionID {
		return commanderFactionID
	}
	return ""
}

// factionCommanderLabel 返回日志中使用的指挥官称谓。
func factionCommanderLabel(state State, commanderFactionID string) string {
	switch commanderFactionID {
	case state.PlayerFactionID:
		return "玩家"
	case state.EnemyFactionID:
		return "对手指挥官"
	default:
		return commanderFactionID
	}
}
