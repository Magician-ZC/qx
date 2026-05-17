package session

// 文件说明：解析玩家文本中的外交禁令策略并计算阵营生效方针，用于约束跨阵营接触与交易。

import "strings"

// diplomacyDirectivePolicy 描述方针对跨阵营交易/接触的限制策略。
type diplomacyDirectivePolicy struct {
	ForbidCrossFactionTrade   bool
	ForbidCrossFactionContact bool
	SourceText                string
}

// parseDiplomacyDirectivePolicy 从自然语言方针中解析外交限制策略。
func parseDiplomacyDirectivePolicy(text string) (diplomacyDirectivePolicy, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return diplomacyDirectivePolicy{}, false
	}
	enemyContext := containsAny(lower, "敌", "敌方", "跨阵营", "跨势力", "对方势力", "异阵营")
	forbidWord := containsAny(lower, "严令", "严禁", "禁止", "不得", "不准")
	allowWord := containsAny(lower, "允许", "可以", "可", "准许")
	tradeWord := containsAny(lower, "交易", "买卖", "互换", "转账", "调拨")
	contactWord := containsAny(lower, "接触", "来往", "交往")

	forbidTrade := containsAny(lower,
		"严令禁止交易",
		"严禁交易",
		"禁止交易",
		"不得交易",
		"不准交易",
	) && (enemyContext || tradeWord)
	if !forbidTrade && forbidWord && tradeWord && (enemyContext || containsAny(lower, "私下")) {
		forbidTrade = true
	}

	forbidContact := containsAny(lower,
		"严令禁止接触",
		"严禁接触",
		"禁止接触",
		"不得接触",
		"不准接触",
		"禁止私下来往",
		"断绝来往",
	) && (enemyContext || contactWord)
	if !forbidContact && forbidWord && contactWord && (enemyContext || containsAny(lower, "私下")) {
		forbidContact = true
	}

	allowTrade := containsAny(lower,
		"允许跨阵营交易",
		"允许跨势力交易",
		"可与敌方交易",
		"可以和敌方交易",
		"允许接触敌方",
		"可以接触敌方",
	)
	if !allowTrade && allowWord && (tradeWord || contactWord) && enemyContext {
		allowTrade = true
	}

	if allowTrade {
		return diplomacyDirectivePolicy{
			ForbidCrossFactionTrade:   false,
			ForbidCrossFactionContact: false,
			SourceText:                strings.TrimSpace(text),
		}, true
	}
	if !forbidTrade && !forbidContact {
		return diplomacyDirectivePolicy{}, false
	}

	if forbidTrade {
		// 禁止交易通常隐含禁止私下接触。
		forbidContact = true
	}
	return diplomacyDirectivePolicy{
		ForbidCrossFactionTrade:   forbidTrade,
		ForbidCrossFactionContact: forbidContact,
		SourceText:                strings.TrimSpace(text),
	}, true
}

// isStrictDiplomacyDirective 判断方针是否属于强约束外交禁令。
func isStrictDiplomacyDirective(text string) bool {
	policy, ok := parseDiplomacyDirectivePolicy(text)
	return ok && (policy.ForbidCrossFactionTrade || policy.ForbidCrossFactionContact)
}

// diplomacyPolicyForFaction 计算某阵营当前生效的外交策略。
func diplomacyPolicyForFaction(state State, factionID string) diplomacyDirectivePolicy {
	if factionID != state.PlayerFactionID {
		return diplomacyDirectivePolicy{}
	}

	policy := diplomacyDirectivePolicy{}
	if parsed, ok := parseDiplomacyDirectivePolicy(state.GlobalDirective.Text); ok {
		policy = parsed
	}
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		kind := normalizeDirectiveKind(directive.Kind)
		if kind != DirectiveKindTask && kind != DirectiveKindOrder && kind != DirectiveKindDoctrine {
			continue
		}
		if kind != DirectiveKindDoctrine && directive.Turn != state.TurnState.Turn {
			continue
		}
		if directive.AppliesTo != "" && directive.AppliesTo != state.PlayerFactionID {
			continue
		}
		parsed, ok := parseDiplomacyDirectivePolicy(directive.Text)
		if !ok {
			continue
		}
		policy = parsed
		break
	}
	return policy
}

// isPlayerEnemyFactionPair 判断两个阵营是否构成当前对局主对抗对。
func isPlayerEnemyFactionPair(state State, leftFactionID string, rightFactionID string) bool {
	left, right, ok := canonicalFactionPair(leftFactionID, rightFactionID)
	if !ok {
		return false
	}
	defaultLeft, defaultRight, defaultOK := canonicalFactionPair(state.PlayerFactionID, state.EnemyFactionID)
	return defaultOK && left == defaultLeft && right == defaultRight
}

// reassignmentDirectiveForUnit 提取单位本回合调岗类任务指令。
func reassignmentDirectiveForUnit(state State, unitID string) string {
	if strings.TrimSpace(unitID) == "" {
		return ""
	}
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		if normalizeDirectiveKind(directive.Kind) != DirectiveKindTask {
			continue
		}
		if directive.Turn != state.TurnState.Turn {
			continue
		}
		if directive.TargetUnitID != unitID || directive.AppliesTo != unitID {
			continue
		}
		text := strings.TrimSpace(directive.Text)
		lower := strings.ToLower(text)
		if !containsAny(lower, "调岗", "调离", "调远", "换位", "换到", "后撤", "靠近", "跟着", "支援", "压到") {
			continue
		}
		return text
	}
	return ""
}
