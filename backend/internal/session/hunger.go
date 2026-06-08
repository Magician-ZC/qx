package session

// 文件说明：结算饥饿系统（回合消耗、动作消耗、正式进食动作、断粮惩罚）并统一写入状态变更事件。

import (
	"context"
	"fmt"
	"math"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// hungerActionModifiers 根据饥饿水平返回行动性能修正。
func hungerActionModifiers(actor unit.Record) actionModifiers {
	if actor.Status.Hunger < 30 {
		return actionModifiers{
			MoveMultiplier:   0.8,
			AttackMultiplier: 0.8,
		}
	}
	return actionModifiers{
		MoveMultiplier:   1,
		AttackMultiplier: 1,
	}
}

// combineActionModifiers 合并多个行动修正倍率。
func combineActionModifiers(values ...actionModifiers) actionModifiers {
	combined := actionModifiers{
		MoveMultiplier:   1,
		AttackMultiplier: 1,
	}
	for _, value := range values {
		if value.MoveMultiplier > 0 {
			combined.MoveMultiplier *= value.MoveMultiplier
		}
		if value.AttackMultiplier > 0 {
			combined.AttackMultiplier *= value.AttackMultiplier
		}
	}
	return combined
}

// applyTurnHungerUpkeep 结算回合饥饿消耗与断粮惩罚；进食由正式 eat 候选动作处理。
func (service *Service) applyTurnHungerUpkeep(ctx context.Context, state *State, units []unit.Record) error {
	if units == nil {
		loaded, err := service.units.ListBySession(ctx, state.ID)
		if err != nil {
			return err
		}
		units = loaded
	}

	for index := range units {
		record := &units[index]
		if record.Status.LifeState != unit.LifeStateActive {
			continue
		}

		baseCost := turnHungerCost(state.Map, *record)
		if err := service.applyStatusMutation(
			ctx,
			state,
			record,
			status.FieldHunger,
			-float64(baseCost),
			events.ReasonSurvivalHunger,
			fmt.Sprintf("我在第 %d 回合消耗了 %d 点口粮。", state.TurnState.Turn, baseCost),
		); err != nil {
			return err
		}
		appendLog(
			state,
			"hunger",
			fmt.Sprintf("我本回合基础消耗 %d 点饥饿度，当前 %d。", baseCost, record.Status.Hunger),
			record.ID,
			"",
		)
		if record.Status.Hunger == 0 && hasBackpackItem(*record, "ration") {
			if _, err := service.consumeEmergencyRation(ctx, state, record); err != nil {
				return err
			}
		}

		if record.Status.Hunger < 30 {
			if err := service.applyStatusMutation(
				ctx,
				state,
				record,
				status.FieldMorale,
				-0.08,
				events.ReasonSurvivalHunger,
				"我因持续饥饿而士气下降。",
			); err != nil {
				return err
			}
			appendLog(
				state,
				"hunger_penalty",
				"我的饥饿度低于 30，士气下滑且后续行动效率下降。",
				record.ID,
				"",
			)
		}

		if record.Status.Hunger > 80 && record.Status.HP > 0 && record.Status.HP < 100 {
			if err := service.applyStatusMutation(
				ctx,
				state,
				record,
				status.FieldHP,
				3,
				events.ReasonSurvivalHunger,
				"我饱腹充足，身体自然恢复。",
			); err != nil {
				return err
			}
			appendLog(
				state,
				"satiety_recovery",
				"我的饥饿度高于 80，自动恢复 3 HP。",
				record.ID,
				"",
			)
		}

		if record.Status.Hunger < 10 && record.Status.HP > 0 {
			if err := service.applyStatusMutation(
				ctx,
				state,
				record,
				status.FieldHP,
				-6,
				events.ReasonSurvivalHunger,
				"我饥饿过度，身体开始持续受损。",
			); err != nil {
				return err
			}
			appendLog(
				state,
				"starvation",
				"我的饥饿度低于 10，额外失去 6 HP。",
				record.ID,
				"",
			)
			if record.Status.HP == 0 && record.Status.LifeState == unit.LifeStateActive {
				if err := unit.ApplyFatalDamage(record); err != nil {
					return err
				}
				if err := service.units.Save(ctx, *record); err != nil {
					return err
				}
				appendLog(
					state,
					"starvation_down",
					"我因极度饥饿而倒下。",
					record.ID,
					"",
				)
			}
		}

		if record.Status.Hunger == 0 {
			record.Status.StarvationTurns++
			appendLog(
				state,
				"starvation",
				"我的饥饿度降为 0，因断粮直接死亡。",
				record.ID,
				"",
			)
			// 断粮致死统一收口到状态写入纪律：先经 Mutator 把 HP 归零并留痕事件，
			// 再交由生命状态机 unit.ApplyFatalDamage 推进 lives / LifeState（受保护字段绝不直写）。
			if record.Status.LifeState == unit.LifeStateActive {
				if record.Status.HP > 0 {
					if err := service.applyStatusMutation(
						ctx,
						state,
						record,
						status.FieldHP,
						-float64(record.Status.HP),
						events.ReasonSurvivalHunger,
						"我因断粮而力竭，生命归零。",
					); err != nil {
						return err
					}
				}
				if err := unit.ApplyFatalDamage(record); err != nil {
					return err
				}
				if err := service.units.Save(ctx, *record); err != nil {
					return err
				}
				appendLog(
					state,
					"starvation_down",
					"我因饥饿归零而死亡。",
					record.ID,
					"",
				)
			}
		} else if record.Status.StarvationTurns != 0 {
			record.Status.StarvationTurns = 0
		}

		if err := service.units.Save(ctx, *record); err != nil {
			return err
		}
	}

	return nil
}

// consumeEmergencyRation 在回合维护把饥饿扣到 0 时自动消耗口粮保命。
// 正常进食仍由执行阶段 eat 候选交给单位判断；这里仅处理“背包明明有口粮却因维护结算直接饿死”的硬安全兜底。
func (service *Service) consumeEmergencyRation(ctx context.Context, state *State, record *unit.Record) (bool, error) {
	if record == nil || record.Status.LifeState != unit.LifeStateActive || record.Status.Hunger != 0 || !hasBackpackItem(*record, "ration") {
		return false, nil
	}
	if err := unit.ConsumeBackpackItem(record, "ration", 1); err != nil {
		return false, err
	}
	if err := service.units.Save(ctx, *record); err != nil {
		return false, err
	}
	if err := service.applyStatusMutation(
		ctx,
		state,
		record,
		status.FieldHunger,
		35,
		events.ReasonSurvivalHunger,
		"我在断粮前吃下一份口粮，避免饥饿归零。",
	); err != nil {
		return false, err
	}
	appendLog(
		state,
		"eat",
		"我及时吃下一份口粮，避免因饥饿归零而死亡。",
		record.ID,
		"",
	)
	return true, nil
}

// applyActionHungerCost 结算单次动作的额外饥饿消耗。
func (service *Service) applyActionHungerCost(ctx context.Context, state *State, actor *unit.Record, actionLabel string) error {
	if actor == nil || actor.Status.LifeState != unit.LifeStateActive {
		return nil
	}

	cost := 3
	if state != nil {
		cost += weatherActionHungerPenalty(*state, *actor)
	}
	if err := service.applyStatusMutation(
		ctx,
		state,
		actor,
		status.FieldHunger,
		-float64(cost),
		events.ReasonSurvivalHunger,
		fmt.Sprintf("我在%s中额外消耗了体力与口粮。", actionLabel),
	); err != nil {
		return err
	}
	appendLog(
		state,
		"hunger",
		fmt.Sprintf("我在%s后额外消耗 %d 点饥饿度，当前 %d。", actionLabel, cost, actor.Status.Hunger),
		actor.ID,
		"",
	)
	return nil
}

// applyStatusMutation 统一应用状态变更并记录 raw event 与日志。
func (service *Service) applyStatusMutation(
	ctx context.Context,
	state *State,
	record *unit.Record,
	field status.Field,
	delta float64,
	reasonCode events.ReasonCode,
	reasonText string,
) error {
	result, err := service.mutator.Apply(ctx, status.Mutation{
		UnitID:     record.ID,
		Turn:       state.TurnState.Turn,
		Field:      field,
		Delta:      delta,
		ReasonCode: reasonCode,
		ReasonText: reasonText,
		Actors:     []string{record.ID},
		Location:   fmt.Sprintf("hex_%d_%d", record.Status.PositionQ, record.Status.PositionR),
	})
	if err != nil {
		return err
	}
	*record = result.Record
	appendRawEvent(state, rawEventSpec{
		source:       "status",
		kind:         string(result.Payload.Field),
		summary:      result.Payload.ReasonText,
		actorUnitID:  record.ID,
		targetUnitID: record.ID,
		payload:      result.Payload,
	})
	appendLog(
		state,
		"stat_change",
		fmt.Sprintf(
			"%s 数值变动 %s %.2f (%.2f -> %.2f) [%s]",
			record.DisplayName(),
			result.Payload.Field,
			result.Payload.Delta,
			result.Payload.Before,
			result.Payload.After,
			result.Payload.ReasonCode,
		),
		record.ID,
		record.ID,
	)
	return nil
}

// pendingStatusMutation 承载一次待批量提交的状态变更及其回写目标。
// record 指向调用方持有的单位记录指针：批量应用后会用最终记录回写它，
// 与逐次 applyStatusMutation 的 `*record = result.Record` 语义一致。
type pendingStatusMutation struct {
	record     *unit.Record
	field      status.Field
	delta      float64
	reasonCode events.ReasonCode
	reasonText string
	// after 在该条变更的标准副作用（记录回写 / raw event / stat_change 日志）补齐之后立即执行，
	// 用于复刻逐次调用时紧随状态变更的额外副作用（如 morale_shift 日志、暴怒授予），
	// 从而保持与逐次路径完全一致的日志/事件交错顺序。可为 nil。
	after func() error
}

// applyStatusMutationsBatch 把一组状态变更聚成一次 Mutator.ApplyBatch 提交，降低 DB 往返，
// 同时严格复刻 applyStatusMutation 的副作用顺序（批应用后按入参顺序逐条回写记录指针、追加
// raw event 与 stat_change 日志）。语义与逐次调用 applyStatusMutation 完全等价，仅 DB 写入被收敛。
func (service *Service) applyStatusMutationsBatch(
	ctx context.Context,
	state *State,
	pending []pendingStatusMutation,
) error {
	if len(pending) == 0 {
		return nil
	}

	mutations := make([]status.Mutation, len(pending))
	for i := range pending {
		record := pending[i].record
		mutations[i] = status.Mutation{
			UnitID:     record.ID,
			Turn:       state.TurnState.Turn,
			Field:      pending[i].field,
			Delta:      pending[i].delta,
			ReasonCode: pending[i].reasonCode,
			ReasonText: pending[i].reasonText,
			Actors:     []string{record.ID},
			Location:   fmt.Sprintf("hex_%d_%d", record.Status.PositionQ, record.Status.PositionR),
		}
	}

	results, err := service.mutator.ApplyBatch(ctx, mutations)
	if err != nil {
		return err
	}

	// 结果按入参顺序对齐：逐条回写记录指针并补齐与逐次 Apply 等价的事件/日志副作用。
	for i := range pending {
		record := pending[i].record
		result := results[i]
		*record = result.Record
		appendRawEvent(state, rawEventSpec{
			source:       "status",
			kind:         string(result.Payload.Field),
			summary:      result.Payload.ReasonText,
			actorUnitID:  record.ID,
			targetUnitID: record.ID,
			payload:      result.Payload,
		})
		appendLog(
			state,
			"stat_change",
			fmt.Sprintf(
				"%s 数值变动 %s %.2f (%.2f -> %.2f) [%s]",
				record.DisplayName(),
				result.Payload.Field,
				result.Payload.Delta,
				result.Payload.Before,
				result.Payload.After,
				result.Payload.ReasonCode,
			),
			record.ID,
			record.ID,
		)
		if pending[i].after != nil {
			if err := pending[i].after(); err != nil {
				return err
			}
		}
	}
	return nil
}

// turnHungerCost 计算单位在当前地形下的基础回合饥饿消耗。
func turnHungerCost(snapshot world.MapSnapshot, record unit.Record) int {
	base := 4
	switch terrainAt(snapshot, world.Coord{Q: record.Status.PositionQ, R: record.Status.PositionR}) {
	case world.TerrainDesert:
		return 6
	case world.TerrainSnowfield:
		return 5
	default:
		return base
	}
}

func halvedHungerCost(cost int) int {
	if cost <= 0 {
		return 0
	}
	return int(math.Ceil(float64(cost) / 2))
}

// executeEat 执行正式进食动作：消耗一份口粮并恢复饥饿值。
func (service *Service) executeEat(ctx context.Context, state *State, actor *unit.Record, decision unitDecisionPayload) error {
	if actor == nil || actor.Status.LifeState != unit.LifeStateActive {
		return nil
	}
	if decision.ItemID == "healing_potion" {
		return service.executeHealingPotion(ctx, state, actor, decision)
	}
	if !hasBackpackItem(*actor, "ration") {
		appendLog(state, "eat_blocked", "我想吃口粮，但背包里已经没有了。", actor.ID, "")
		return nil
	}
	if err := unit.ConsumeBackpackItem(actor, "ration", 1); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.applyStatusMutation(
		ctx,
		state,
		actor,
		status.FieldHunger,
		35,
		events.ReasonSurvivalHunger,
		"我决定吃下一份口粮恢复体力。",
	); err != nil {
		return err
	}
	appendLog(
		state,
		"eat",
		firstNonEmptyText(decisionLogText(decision), "我吃下一份口粮，先把体力稳住。"),
		actor.ID,
		"",
	)
	return nil
}

func (service *Service) executeHealingPotion(ctx context.Context, state *State, actor *unit.Record, decision unitDecisionPayload) error {
	if !hasBackpackItem(*actor, "healing_potion") {
		appendLog(state, "heal_blocked", "我想喝药，但背包里已经没有药了。", actor.ID, "")
		return nil
	}
	if actor.Status.HP >= 100 {
		appendLog(state, "heal_blocked", "我状态还满，不需要浪费药。", actor.ID, "")
		return nil
	}
	if err := unit.ConsumeBackpackItem(actor, "healing_potion", 1); err != nil {
		return err
	}
	if err := service.units.Save(ctx, *actor); err != nil {
		return err
	}
	if err := service.applyStatusMutation(
		ctx,
		state,
		actor,
		status.FieldHP,
		25,
		events.ReasonSurvivalHunger,
		"我决定喝下一瓶药剂恢复生命。",
	); err != nil {
		return err
	}
	appendLog(
		state,
		"heal",
		firstNonEmptyText(decisionLogText(decision), "我喝下药剂，先把伤稳住。"),
		actor.ID,
		"",
	)
	return nil
}

// hasBackpackItem 判断背包中是否存在指定物品。
func hasBackpackItem(record unit.Record, itemID string) bool {
	for _, stack := range record.Inventory.Backpack {
		if stack.ItemID == itemID && stack.Quantity > 0 {
			return true
		}
	}
	return false
}
