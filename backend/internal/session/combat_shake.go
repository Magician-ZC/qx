package session

// 文件说明：结算战斗“动摇”与情绪覆写逻辑，可在高压场景覆盖原决策并附加行动倍率修正。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// emotionalOverrideKind 类型定义用于统一该模块的数据表达。
type emotionalOverrideKind string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	emotionalOverrideRevenge  emotionalOverrideKind = "revenge"
	emotionalOverrideCollapse emotionalOverrideKind = "collapse"
	emotionalOverrideHeroism  emotionalOverrideKind = "heroism"
	emotionalOverrideFear     emotionalOverrideKind = "fear"
)

// combatShakeResolution 结构体用于承载该模块的核心数据。
type combatShakeResolution struct {
	Triggered        bool
	Choice           combatShakeChoicePayload
	Result           ai.CompletionResult
	OverrideDecision *unitDecisionPayload
	Modifiers        actionModifiers
	Emotion          emotionalOverrideKind
}

// resolveCombatShake 结算“战斗动摇”事件，并在必要时覆盖单位原决策。
// 结果可能附带情绪覆写（复仇/崩溃/英雄主义/恐惧）及对应行动系数修改。
func (service *Service) resolveCombatShake(
	ctx context.Context,
	state *State,
	byID map[string]*unit.Record,
	actor *unit.Record,
) (combatShakeResolution, error) {
	resolution := combatShakeResolution{
		Modifiers: actionModifiers{
			MoveMultiplier:   1,
			AttackMultiplier: 1,
		},
	}
	if state == nil || actor == nil || !isBattleReady(*actor) {
		return resolution, nil
	}

	triggers := combatShakeTriggers(*state, byID, actor)
	if len(triggers) == 0 {
		return resolution, nil
	}
	resolution.Triggered = true

	choice, result, interaction, err := service.generateCombatShakeChoice(ctx, *state, byID, *actor, triggers)
	service.appendLLMInteractionWithSpend(ctx, state, interaction)
	if err != nil {
		return resolution, err
	}

	resolution.Choice = choice
	resolution.Result = result
	shakeMessage := strings.TrimSpace(firstNonEmptyText(choice.Bubble, choice.Reasoning, choice.Memory))
	if shakeMessage != "" {
		appendLog(
			state,
			"shake",
			shakeMessage,
			actor.ID,
			"",
		)
	}

	switch choice.Action {
	case "retreat":
		target, ok := retreatTargetCoord(*state, byID, actor)
		narrative := strings.TrimSpace(firstNonEmptyText(choice.Bubble, choice.Memory, choice.Reasoning))
		decision := unitDecisionPayload{
			Action:     DecisionActionHold,
			NextAction: limitTextRunes(narrative, 12),
			Speak:      choice.Bubble,
			Memory:     choice.Memory,
			Reasoning:  choice.Reasoning,
		}
		if ok {
			decision.Action = DecisionActionMove
			decision.TargetQ = target.Q
			decision.TargetR = target.R
		}
		resolution.OverrideDecision = &decision
	case "surrender":
		narrative := strings.TrimSpace(firstNonEmptyText(choice.Memory, choice.Bubble, choice.Reasoning))
		// 投降对同一单位连续两次变更（士气 + 忠诚），二者字段独立、互不读取对方结果、无数据依赖，
		// 聚成一次 ApplyBatch 收敛 DB 往返；顺序（先 Morale 后 Loyalty）与逐次路径一致，事件/日志交错不变。
		if err := service.applyStatusMutationsBatch(ctx, state, []pendingStatusMutation{
			{
				record:     actor,
				field:      status.FieldMorale,
				delta:      -0.18,
				reasonCode: events.ReasonEmotionTrauma,
				reasonText: narrative,
			},
			{
				record:     actor,
				field:      status.FieldLoyalty,
				delta:      -0.05,
				reasonCode: events.ReasonCommandForced,
				reasonText: narrative,
			},
		}); err != nil {
			return resolution, err
		}
		decision := unitDecisionPayload{
			Action:     DecisionActionHold,
			NextAction: limitTextRunes(strings.TrimSpace(firstNonEmptyText(choice.Bubble, choice.Memory, choice.Reasoning)), 12),
			Speak:      choice.Bubble,
			Memory:     choice.Memory,
			Reasoning:  choice.Reasoning,
		}
		resolution.OverrideDecision = &decision
	case "rage":
		resolution.Modifiers = actionModifiers{
			MoveMultiplier:   0.95,
			AttackMultiplier: 1,
		}
		// 用与主循环校验一致的「可见敌」集选目标——主循环 validateDecision 只认 visibleOpposingIDs（开雾时过滤视野外敌）；
		// 若仍用全体 opposingIDs 选中视野外敌，攻击会被判非法降级 HOLD，而 rage 效果却已落库（评审 load-bearing 修复）。
		target := nearestBattleReady(visibleOpposingIDs(*state, byID, actor), byID, actor)
		if target != nil {
			// 仅在确有可打的可见目标时才持久化 rage 效果，避免「效果入库却被降级待命」。
			if grantCombatEffect(actor, combatEffectRage, state.TurnState.Turn+1) {
				if err := service.units.Save(ctx, *actor); err != nil {
					return resolution, err
				}
			}
			decision := unitDecisionPayload{
				Action:       DecisionActionAttack,
				TargetUnitID: target.ID,
				NextAction:   limitTextRunes(strings.TrimSpace(firstNonEmptyText(choice.Bubble, choice.Memory, choice.Reasoning)), 12),
				Speak:        choice.Bubble,
				Memory:       choice.Memory,
				Reasoning:    choice.Reasoning,
			}
			resolution.OverrideDecision = &decision
		}
	default:
		// continue
	}

	emotion := inferEmotionalOverride(choice.Action, *actor, triggers)
	if emotion != "" {
		resolution.Emotion = emotion
		resolution.Modifiers = combineActionModifiers(resolution.Modifiers, emotionalOverrideModifiers(emotion))
		if summary := strings.TrimSpace(firstNonEmptyText(choice.Bubble, choice.Reasoning, choice.Memory)); summary != "" {
			appendLog(
				state,
				"emotional_override",
				summary,
				actor.ID,
				"",
			)
		}
		if err := service.rememberEmotionalOverrideCritical(ctx, actor, state.TurnState.Turn, emotion, choice); err != nil {
			return resolution, err
		}
	}

	return resolution, nil
}

// applyCombatShakeOverlay 把战斗动摇产出的气泡/记忆/推理信息合并回最终决策文本。
func applyCombatShakeOverlay(decision unitDecisionPayload, resolution combatShakeResolution) unitDecisionPayload {
	if !resolution.Triggered {
		return decision
	}
	if decision.Speak == "" && resolution.Choice.Bubble != "" {
		decision.Speak = resolution.Choice.Bubble
	}
	if decision.Memory == "" && resolution.Choice.Memory != "" {
		decision.Memory = resolution.Choice.Memory
	}
	if resolution.Choice.Reasoning != "" && !strings.Contains(decision.Reasoning, resolution.Choice.Reasoning) {
		decision.Reasoning = strings.TrimSpace(fmt.Sprintf("%s %s", decision.Reasoning, resolution.Choice.Reasoning))
	}
	return decision
}

// combatShakeTriggers 收集触发动摇判定的即时信号（低血、低士气、敌压、友军阵亡、负面记忆）。
func combatShakeTriggers(state State, byID map[string]*unit.Record, actor *unit.Record) []string {
	seen := map[string]struct{}{}
	triggers := make([]string, 0, 5)
	add := func(value string) {
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		triggers = append(triggers, value)
	}

	if actor.Status.HP <= 30 {
		add("血量低于30%")
	}
	if actor.Status.Morale <= 0.35 {
		add("士气过低")
	}
	if hasLocalEnemyAdvantage(state, byID, actor) {
		add("局部战场敌压过高")
	}
	if hasRecentAllyDown(state, byID, actor) {
		add("附近友军刚倒下")
	}
	if memoryContainsAny(*actor, "倒下", "濒死", "崩溃", "太险", "断粮") {
		add("记忆中有强烈负面战损")
	}

	return triggers
}

// hasLocalEnemyAdvantage 判断单位附近局部战场是否存在明显敌我人数劣势。
func hasLocalEnemyAdvantage(state State, byID map[string]*unit.Record, actor *unit.Record) bool {
	const radius = 2
	allies := 0
	enemies := 0
	for _, alliedID := range alliedIDs(state, actor.FactionID) {
		ally := byID[alliedID]
		if ally == nil || !isBattleReady(*ally) {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, ally.Status.PositionQ, ally.Status.PositionR) <= radius {
			allies++
		}
	}
	for _, enemyID := range opposingIDs(state, actor.FactionID) {
		enemy := byID[enemyID]
		if enemy == nil || !isBattleReady(*enemy) {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, enemy.Status.PositionQ, enemy.Status.PositionR) <= radius {
			enemies++
		}
	}

	return enemies >= allies+2 || (enemies >= 3 && allies <= 1)
}

// hasRecentAllyDown 检测近期日志中是否出现“附近友军倒下”事件。
func hasRecentAllyDown(state State, byID map[string]*unit.Record, actor *unit.Record) bool {
	start := len(state.Logs) - 12
	if start < 0 {
		start = 0
	}
	for index := len(state.Logs) - 1; index >= start; index-- {
		entry := state.Logs[index]
		if !strings.Contains(entry.Message, "倒下") {
			continue
		}
		if entry.TargetUnitID == "" {
			continue
		}
		target := byID[entry.TargetUnitID]
		if target == nil {
			continue
		}
		if target.FactionID != actor.FactionID || target.ID == actor.ID {
			continue
		}
		if unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, target.Status.PositionQ, target.Status.PositionR) <= 2 {
			return true
		}
	}
	return false
}

// retreatTargetCoord 推导撤退目标坐标：从 actor 的**相邻单格**中选离最近威胁最远、且在界内未被占的那一格。
// 必须返回相邻格（distance==1）——否则覆写出的 Move 决策过不了 validateDecision 的 distance==1 约束会被降级为
// HOLD，导致「该撤退的单位原地待命」（评审 load-bearing 修复）。无可退之相邻格则返回 false（retreat 分支保持 HOLD）。
func retreatTargetCoord(state State, byID map[string]*unit.Record, actor *unit.Record) (world.Coord, bool) {
	threat := nearestBattleReady(opposingIDs(state, actor.FactionID), byID, actor)
	if threat == nil {
		return world.Coord{}, false
	}
	cur := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	best := world.Coord{}
	bestDist := -1
	for _, c := range axialNeighbors(cur) {
		if !inBounds(state.Map, c) || occupiedByAnother(byID, actor.ID, c) {
			continue
		}
		// 离威胁越远越好（朝背离方向后撤一格）。
		d := unit.HexDistance(c.Q, c.R, threat.Status.PositionQ, threat.Status.PositionR)
		if d > bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist < 0 {
		return world.Coord{}, false // 四周皆越界/被占，无处可退
	}
	return best, true
}

// inferEmotionalOverride 根据 shake 行为与触发器推断情绪覆写类型。
func inferEmotionalOverride(action string, actor unit.Record, triggers []string) emotionalOverrideKind {
	switch action {
	case "rage":
		return emotionalOverrideRevenge
	case "surrender":
		return emotionalOverrideCollapse
	case "retreat":
		return emotionalOverrideFear
	default:
		if actor.Personality.Courage >= 0.65 &&
			(containsTrigger(triggers, "附近友军刚倒下") || containsTrigger(triggers, "局部战场敌压过高")) {
			return emotionalOverrideHeroism
		}
		return ""
	}
}

// containsTrigger 判断触发器列表是否包含指定信号。
func containsTrigger(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// emotionalOverrideModifiers 返回不同情绪覆写对移动/攻击倍率的修正值。
func emotionalOverrideModifiers(emotion emotionalOverrideKind) actionModifiers {
	switch emotion {
	case emotionalOverrideRevenge:
		return actionModifiers{MoveMultiplier: 0.95, AttackMultiplier: 1.15}
	case emotionalOverrideCollapse:
		return actionModifiers{MoveMultiplier: 0.9, AttackMultiplier: 0.85}
	case emotionalOverrideHeroism:
		return actionModifiers{MoveMultiplier: 1.05, AttackMultiplier: 1.1}
	case emotionalOverrideFear:
		return actionModifiers{MoveMultiplier: 1.12, AttackMultiplier: 0.9}
	default:
		return actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1}
	}
}

// emotionalOverrideLabel 把情绪覆写枚举转换为中文展示标签。
func emotionalOverrideLabel(emotion emotionalOverrideKind) string {
	switch emotion {
	case emotionalOverrideRevenge:
		return "复仇"
	case emotionalOverrideCollapse:
		return "崩溃"
	case emotionalOverrideHeroism:
		return "英雄主义"
	case emotionalOverrideFear:
		return "恐惧"
	default:
		return "无"
	}
}

// rememberEmotionalOverrideCritical 把关键情绪覆写事件写入永久记忆。
// 同时维护 FTS、显著性刷新、容量裁剪与单位高亮同步。
func (service *Service) rememberEmotionalOverrideCritical(
	ctx context.Context,
	actor *unit.Record,
	turn int,
	emotion emotionalOverrideKind,
	choice combatShakeChoicePayload,
) error {
	if service == nil || actor == nil {
		return nil
	}
	if turn <= 0 {
		turn = 1
	}

	summary := strings.TrimSpace(choice.Memory)
	if summary == "" {
		summary = strings.TrimSpace(choice.Bubble)
	}
	if summary == "" {
		summary = strings.TrimSpace(choice.Reasoning)
	}
	if summary == "" {
		return nil
	}
	summary = limitTextRunes(summary, 28)

	var latestSummary string
	_ = service.db.QueryRowContext(
		ctx,
		`SELECT summary FROM memories WHERE unit_id = ? ORDER BY created_at DESC LIMIT 1`,
		actor.ID,
	).Scan(&latestSummary)
	if strings.TrimSpace(latestSummary) == summary {
		return nil
	}

	meta := memoryMetadata{
		Turn:         turn,
		Importance:   10,
		BaseSalience: 1.4,
		Permanent:    true,
		Source:       "emotional_override",
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal emotional override memory metadata: %w", err)
	}
	emotionWeight := 2.2
	salience := computeMemorySalience(turn, meta, emotionWeight)
	memoryID := uuid.NewString()

	if _, err := service.db.ExecContext(
		ctx,
		`
		INSERT INTO memories (
			id,
			unit_id,
			category,
			summary,
			emotion_weight,
			salience,
			recall_count,
			metadata_json,
			created_at
		) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
		`,
		memoryID,
		actor.ID,
		memoryCategoryEvent,
		summary,
		emotionWeight,
		salience,
		string(metaJSON),
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("insert emotional override memory: %w", err)
	}
	if service.ensureMemoryFTS(ctx) == nil {
		_ = service.upsertMemoryFTS(ctx, memoryID, actor.ID, summary)
	}
	if err := service.refreshUnitMemorySalience(ctx, actor.ID, turn); err != nil {
		return err
	}
	service.markUnitMemoryRefreshedTurn(actor.ID, turn)
	if err := service.enforceMemoryCaps(ctx, actor.ID); err != nil {
		return err
	}
	if err := service.syncRecordHighlightsFromMemories(ctx, actor, memoryHighlightBudget); err != nil {
		return err
	}
	return service.units.Save(ctx, *actor)
}
