package session

// 文件说明：聚合 doctrine/task/order/对话上下文并管理指挥力，为单位决策提供统一指令视图。

import "strings"

// normalizeDirectiveKind 规范化指令类型，非法值回落为 doctrine。
func normalizeDirectiveKind(kind DirectiveKind) DirectiveKind {
	switch DirectiveKind(strings.ToLower(strings.TrimSpace(string(kind)))) {
	case DirectiveKindTask:
		return DirectiveKindTask
	case DirectiveKindOrder:
		return DirectiveKindOrder
	default:
		return DirectiveKindDoctrine
	}
}

// defaultCommandPower 返回指挥力系统的默认参数。
func defaultCommandPower() CommandPowerState {
	return CommandPowerState{
		Current:   3,
		Max:       3,
		Regen:     2,
		OrderCost: 1,
	}
}

// ensureCommandPower 校正并补齐指挥力字段，避免状态异常。
func ensureCommandPower(state *State) {
	if state == nil {
		return
	}
	if state.CommandPower.Max <= 0 {
		state.CommandPower = defaultCommandPower()
		return
	}
	if state.CommandPower.OrderCost <= 0 {
		state.CommandPower.OrderCost = 1
	}
	if state.CommandPower.Regen <= 0 {
		state.CommandPower.Regen = 1
	}
	if state.CommandPower.Current > state.CommandPower.Max {
		state.CommandPower.Current = state.CommandPower.Max
	}
	if state.CommandPower.Current < 0 {
		state.CommandPower.Current = 0
	}
}

// rechargeCommandPower 在新回合恢复可用指挥力。
func rechargeCommandPower(state *State) {
	ensureCommandPower(state)
	if state == nil {
		return
	}
	state.CommandPower.Current += state.CommandPower.Regen
	if state.CommandPower.Current > state.CommandPower.Max {
		state.CommandPower.Current = state.CommandPower.Max
	}
}

// directiveForUnit 返回某单位可见的指令上下文。
func directiveForUnit(state State, unitID string, factionID string) string {
	return directiveContextForActor(state, unitID, factionID)
}

// directiveForFaction 汇总某阵营当前可见的方针与任务指令。
func directiveForFaction(state State, factionID string) string {
	factionID = strings.TrimSpace(factionID)
	if factionID == "" {
		factionID = state.PlayerFactionID
	}

	parts := make([]string, 0, 4)
	seen := map[string]struct{}{}
	appendDirectivePart(&parts, seen, factionDoctrineText(state, factionID))

	for index := len(state.DirectiveHistory) - 1; index >= 0 && len(parts) < 4; index-- {
		directive := state.DirectiveHistory[index]
		if normalizeDirectiveKind(directive.Kind) != DirectiveKindTask {
			continue
		}
		if directive.Turn != state.TurnState.Turn {
			continue
		}
		if directive.AppliesTo != factionID && directive.AppliesTo != "" {
			continue
		}
		appendDirectivePart(&parts, seen, directive.Text)
	}

	if len(parts) == 0 {
		if factionID == state.EnemyFactionID {
			return "压上去击溃对手，但不要把自己送进白白送死的位置。"
		}
		return "稳住阵型，优先保全队伍。"
	}
	return strings.Join(parts, "\n")
}

// directiveContextForActor 汇总单单位指令上下文（方针/任务/命令/最近对话）。
func directiveContextForActor(state State, unitID string, factionID string) string {
	factionID = strings.TrimSpace(factionID)
	if factionID == "" {
		factionID = state.PlayerFactionID
	}
	parts := make([]string, 0, 8)
	seen := map[string]struct{}{}
	appendDirectivePart(&parts, seen, factionDoctrineText(state, factionID))

	for index := len(state.DirectiveHistory) - 1; index >= 0 && len(parts) < 6; index-- {
		directive := state.DirectiveHistory[index]
		kind := normalizeDirectiveKind(directive.Kind)
		switch kind {
		case DirectiveKindTask:
			if directive.Turn != state.TurnState.Turn {
				continue
			}
			if directive.TargetUnitID != "" && directive.TargetUnitID != unitID {
				continue
			}
			if directive.AppliesTo != unitID && directive.AppliesTo != factionID && directive.AppliesTo != "" {
				continue
			}
			appendDirectivePart(&parts, seen, directive.Text)
		case DirectiveKindOrder:
			if directive.Turn != state.TurnState.Turn {
				continue
			}
			if directive.TargetUnitID != unitID || directive.AppliesTo != unitID {
				continue
			}
			appendDirectivePart(&parts, seen, directive.Text)
		}
	}

	for index := len(state.DialogueHistory) - 1; index >= 0 && len(parts) < 8; index-- {
		entry := state.DialogueHistory[index]
		if entry.UnitID != unitID {
			continue
		}
		speaker := strings.ToLower(strings.TrimSpace(entry.Speaker))
		if speaker != "player" && speaker != "enemy_commander" {
			continue
		}
		appendDirectivePart(&parts, seen, entry.Message)
	}

	if len(parts) == 0 {
		if factionID == state.EnemyFactionID {
			return "压上去击溃对手，但不要把自己送进白白送死的位置。"
		}
		return "稳住阵型，优先保全队伍。"
	}
	return strings.Join(parts, "\n")
}

// factionDoctrineText 获取阵营当前生效的 doctrine 文本。
func factionDoctrineText(state State, factionID string) string {
	factionID = strings.TrimSpace(factionID)
	if factionID == "" {
		factionID = state.PlayerFactionID
	}
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		if normalizeDirectiveKind(directive.Kind) != DirectiveKindDoctrine {
			continue
		}
		if directive.Turn > state.TurnState.Turn {
			continue
		}
		if directive.AppliesTo == factionID {
			return strings.TrimSpace(directive.Text)
		}
		if factionID == state.PlayerFactionID && directive.AppliesTo == "" {
			return strings.TrimSpace(directive.Text)
		}
	}
	if factionID == state.PlayerFactionID {
		return strings.TrimSpace(state.GlobalDirective.Text)
	}
	return ""
}

// activeImmediateOrderForUnit 查询单位本回合是否存在即时命令。
func activeImmediateOrderForUnit(state State, unitID string) (Directive, bool) {
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		if normalizeDirectiveKind(directive.Kind) != DirectiveKindOrder {
			continue
		}
		if directive.Turn != state.TurnState.Turn {
			continue
		}
		if directive.TargetUnitID == unitID && directive.AppliesTo == unitID {
			return directive, true
		}
	}
	return Directive{}, false
}

// appendDirectivePart 追加去重后的指令片段。
func appendDirectivePart(parts *[]string, seen map[string]struct{}, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if _, ok := seen[text]; ok {
		return
	}
	seen[text] = struct{}{}
	*parts = append(*parts, text)
}
