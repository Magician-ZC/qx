package session

// 文件说明：实现执行阶段 ATB 行动条排序，综合速度分项并输出本轮单位执行顺序与拆解数据。

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	atbGaugeThreshold        = 100.0
	atbGaugeTickScale        = 0.1
	atbMomentumPenaltyFactor = 0.85
	atbMaxTicks              = 2048
)

// atbSpeedBreakdown 结构体用于承载该模块的核心数据。
type atbSpeedBreakdown struct {
	Base        float64
	Equipment   float64
	Personality float64
	Status      float64
	Ambush      float64
	TaskBias    float64
}

// Total 汇总单位在 ATB 模型下的总速度值。
func (breakdown atbSpeedBreakdown) Total() float64 {
	return breakdown.Base + breakdown.Equipment + breakdown.Personality + breakdown.Status + breakdown.Ambush + breakdown.TaskBias
}

// atbActorState 结构体用于承载该模块的核心数据。
type atbActorState struct {
	UnitID    string
	FactionID string
	Speed     float64
	Index     int
	Gauge     float64
	Activated bool
}

// taskBiasFn uses the logarithmic saturation curve from v1.7:
// ln(1+priority) * 1.8. Priority 10 yields ~= 4.3 bonus.
func taskBiasFn(priority int) float64 {
	if priority < 0 {
		priority = 0
	}
	return math.Log1p(float64(priority)) * 1.8
}

// buildExecutionOrderByATB 使用 ATB（行动条）模拟生成本轮执行顺序。
// 速度由基础值、装备、人格、状态、伏击地形与任务优先级共同决定，并带连动惩罚。
func buildExecutionOrderByATB(
	state State,
	byID map[string]*unit.Record,
) ([]string, map[string]atbSpeedBreakdown) {
	candidateIDs := append([]string{}, state.PlayerUnitIDs...)
	candidateIDs = append(candidateIDs, state.EnemyUnitIDs...)
	candidateIDs = append(candidateIDs, state.WildUnitIDs...)

	actors := make([]*atbActorState, 0, len(candidateIDs))
	breakdowns := make(map[string]atbSpeedBreakdown, len(candidateIDs))
	factionByID := make(map[string]string, len(candidateIDs))
	for index, unitID := range candidateIDs {
		record := byID[unitID]
		if record == nil || !isBattleReady(*record) {
			continue
		}
		speed, breakdown := atbSpeedForUnit(state, *record)
		actors = append(actors, &atbActorState{
			UnitID:    unitID,
			FactionID: record.FactionID,
			Speed:     speed,
			Index:     index,
			Gauge:     0,
			Activated: false,
		})
		breakdowns[unitID] = breakdown
		factionByID[unitID] = record.FactionID
	}
	if len(actors) == 0 {
		return nil, breakdowns
	}

	order := make([]string, 0, len(actors))
	for tick := 0; len(order) < len(actors) && tick < atbMaxTicks; tick++ {
		for _, actor := range actors {
			if actor.Activated {
				continue
			}
			increment := actor.Speed * atbGaugeTickScale
			if hasMomentumPenalty(order, actor.FactionID, factionByID) {
				increment *= atbMomentumPenaltyFactor
			}
			actor.Gauge += increment
		}

		ready := make([]*atbActorState, 0, len(actors))
		for _, actor := range actors {
			if actor.Activated || actor.Gauge < atbGaugeThreshold {
				continue
			}
			ready = append(ready, actor)
		}
		if len(ready) == 0 {
			continue
		}

		sort.SliceStable(ready, func(i, j int) bool {
			left := ready[i]
			right := ready[j]

			leftEffectiveSpeed := left.Speed
			rightEffectiveSpeed := right.Speed
			if hasMomentumPenalty(order, left.FactionID, factionByID) {
				leftEffectiveSpeed *= atbMomentumPenaltyFactor
			}
			if hasMomentumPenalty(order, right.FactionID, factionByID) {
				rightEffectiveSpeed *= atbMomentumPenaltyFactor
			}
			if leftEffectiveSpeed != rightEffectiveSpeed {
				return leftEffectiveSpeed > rightEffectiveSpeed
			}
			if left.Gauge != right.Gauge {
				return left.Gauge > right.Gauge
			}
			return left.Index < right.Index
		})

		chosen := ready[0]
		chosen.Gauge -= atbGaugeThreshold
		chosen.Activated = true
		order = append(order, chosen.UnitID)
	}

	if len(order) < len(actors) {
		remaining := make([]*atbActorState, 0, len(actors)-len(order))
		for _, actor := range actors {
			if actor.Activated {
				continue
			}
			remaining = append(remaining, actor)
		}
		sort.SliceStable(remaining, func(i, j int) bool {
			if remaining[i].Gauge != remaining[j].Gauge {
				return remaining[i].Gauge > remaining[j].Gauge
			}
			return remaining[i].Index < remaining[j].Index
		})
		for _, actor := range remaining {
			order = append(order, actor.UnitID)
		}
	}

	return order, breakdowns
}

// hasMomentumPenalty 判断同阵营是否已连续行动两次；若是，则第三次起触发速度惩罚。
func hasMomentumPenalty(order []string, candidateFactionID string, factionByID map[string]string) bool {
	if len(order) < 2 {
		return false
	}
	lastFaction := factionByID[order[len(order)-1]]
	prevFaction := factionByID[order[len(order)-2]]
	return lastFaction != "" && lastFaction == candidateFactionID && prevFaction == candidateFactionID
}

// atbSpeedForUnit 计算单位 ATB 总速度及分项拆解。
func atbSpeedForUnit(state State, actor unit.Record) (float64, atbSpeedBreakdown) {
	breakdown := atbSpeedBreakdown{
		Base:        6 + float64(actor.Status.Move),
		Equipment:   equipmentSpeedBonus(actor),
		Personality: personalitySpeedBonus(actor),
		Status:      statusSpeedBonus(actor),
		Ambush:      ambushSpeedBonus(state.Map, actor),
		TaskBias:    taskBiasSpeedBonus(state, actor),
	}
	total := breakdown.Total()
	total = clampFloat(total, 2.4, 22)
	return total, breakdown
}

// equipmentSpeedBonus 汇总装备提供的机动加成。
func equipmentSpeedBonus(actor unit.Record) float64 {
	bonus := 0.0
	for _, equipped := range actor.Inventory.Equipment {
		if equipped.ItemID == "" || equipped.Quantity <= 0 {
			continue
		}
		definition, found := item.Lookup(equipped.ItemID)
		if !found {
			continue
		}
		bonus += float64(definition.MoveBonus * equipped.Quantity)
	}
	return bonus
}

// personalitySpeedBonus 根据进攻倾向与克制倾向平衡计算人格速度修正。
func personalitySpeedBonus(actor unit.Record) float64 {
	offense := actor.Personality.Aggression*0.45 + actor.Personality.Courage*0.35 + actor.Personality.Ambition*0.20
	control := actor.Personality.Prudence*0.35 + actor.Personality.Stability*0.30
	return (offense - control) * 4.2
}

// statusSpeedBonus 根据当前生命、士气、饥饿和减益状态计算速度修正。
func statusSpeedBonus(actor unit.Record) float64 {
	maxHP := float64(actor.Stats.Primary.Constitution * 10)
	if maxHP < 1 {
		maxHP = 100
	}
	hpRatio := clampFloat(float64(actor.Status.HP)/maxHP, 0, 1)
	bonus := (hpRatio - 0.5) * 2.2
	bonus += (actor.Status.Morale - 0.5) * 3.0
	if actor.Status.Hunger < 30 {
		bonus -= 1.2
	}
	if actor.Status.Hunger < 10 {
		bonus -= 0.9
	}
	if len(actor.Status.Debuffs) > 0 {
		bonus -= float64(len(actor.Status.Debuffs)) * 0.2
	}
	return bonus
}

// ambushSpeedBonus 在森林/沼泽/废墟等伏击地形上给予潜行侦查型单位额外速度。
func ambushSpeedBonus(snapshot world.MapSnapshot, actor unit.Record) float64 {
	terrain := terrainAt(snapshot, world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR})
	if terrain != world.TerrainForest && terrain != world.TerrainSwamp && terrain != world.TerrainRuins {
		return 0
	}
	stealth := float64(actor.Skills.Survival.Stealth)
	scouting := float64(actor.Skills.Survival.Scouting)
	if stealth+scouting < 50 {
		return 0
	}
	return 1.1
}

// taskBiasSpeedBonus 把单位当前任务优先级映射为行动速度偏置。
func taskBiasSpeedBonus(state State, actor unit.Record) float64 {
	priority := directivePriorityForUnit(state, actor)
	return taskBiasFn(priority)
}

// directivePriorityForUnit 推导单位当前任务优先级。
// 玩家点名即时令优先级最高，其次任务指令，再退化到人格默认值。
func directivePriorityForUnit(state State, actor unit.Record) int {
	if actor.FactionID != state.PlayerFactionID {
		if actor.Personality.Aggression >= 0.65 {
			return 7
		}
		return 5
	}

	priority := 5
	for index := len(state.DirectiveHistory) - 1; index >= 0; index-- {
		directive := state.DirectiveHistory[index]
		if directive.Turn != state.TurnState.Turn {
			continue
		}
		kind := normalizeDirectiveKind(directive.Kind)
		switch kind {
		case DirectiveKindOrder:
			if directive.TargetUnitID == actor.ID && directive.AppliesTo == actor.ID {
				return 10
			}
		case DirectiveKindTask:
			if directive.TargetUnitID != "" && directive.TargetUnitID != actor.ID {
				continue
			}
			if directive.AppliesTo != "" && directive.AppliesTo != actor.ID && directive.AppliesTo != state.PlayerFactionID {
				continue
			}
			if score := directivePriorityScore(directive.Priority); score > priority {
				priority = score
			}
		}
	}
	if reassignmentDirectiveForUnit(state, actor.ID) != "" && priority < 8 {
		priority = 8
	}
	return priority
}

// directivePriorityScore 把 low/normal/high/urgent 映射为数值优先级。
func directivePriorityScore(priority string) int {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case "urgent":
		return 10
	case "high":
		return 8
	case "low":
		return 2
	default:
		return 5
	}
}

// describeExecutorLoop 生成执行阶段排序说明文本，便于日志观测 ATB 结果。
func describeExecutorLoop(
	order []string,
	byID map[string]*unit.Record,
	breakdowns map[string]atbSpeedBreakdown,
) string {
	if len(order) == 0 {
		return "执行阶段未找到可行动单位。"
	}
	sample := make([]string, 0, len(order))
	limit := 4
	if limit > len(order) {
		limit = len(order)
	}
	for _, unitID := range order[:limit] {
		record := byID[unitID]
		if record == nil {
			continue
		}
		part := fmt.Sprintf("%s", record.DisplayName())
		if breakdown, ok := breakdowns[unitID]; ok {
			part = fmt.Sprintf("%s(speed=%.1f,bias=%.1f)", record.DisplayName(), breakdown.Total(), breakdown.TaskBias)
		}
		sample = append(sample, part)
	}
	if len(sample) == 0 {
		return "执行阶段未找到可行动单位。"
	}
	if len(order) > limit {
		sample = append(sample, "...")
	}
	return fmt.Sprintf("ExecutorLoop(L1-L5) 行动窗口顺序：%s。", strings.Join(sample, " -> "))
}
