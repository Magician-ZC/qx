package session

// 文件说明：把跨玩家世界总线(worldbus.cross_events)里牵涉到某角色的事件，桥接进她的命运收件箱。
// 这正是「用户之间的角色如何靠事件关联」的落地：别家玩家的角色救了她/背叛了她/与她结盟，
// 都会从那张不可篡改的总线流进她的命运层（设计文档 docs/事件耦合与跨玩家关联.md）。
// 复用 SurfaceFateEvent 的相关性路由：直接发生在她身上的跨玩家事件一定相关 → 走自相关路径入收件箱。

import (
	"context"
	"fmt"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

const crossEventDefaultImportance = 6

// crossSurfaceLimitPerBoundary 单个角色每个部署边界最多拉取/路由的跨玩家事件条数（防一边界刷爆收件箱）。
const crossSurfaceLimitPerBoundary = 6

// surfaceCrossEventsAtBoundary 在部署边界把世界总线上牵涉每个玩家单位的跨玩家事件桥接进其命运收件箱（读出侧触发）。
// 这是「写入侧通、读出侧零触发」P0 断点的落点：SurfaceCrossEventsForCharacter 已实现但原先无人调用。
// 守卫：WorldID 为空（未接入多世界）直接 no-op，避免对单库角色做无意义查询；对单人局也生效（不依赖 region-runner）。
// 全程 best-effort：单角色失败只吞错跳过，绝不影响阶段推进；遍历用值拷贝，避免把指针留进闭包。
func (service *Service) surfaceCrossEventsAtBoundary(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || service.db == nil || state == nil {
		return
	}
	worldID := state.WorldID
	if worldID == "" {
		return // 未接入多世界：跨玩家总线无内容，跳过
	}
	for i := range units {
		u := units[i]
		if state.PlayerFactionID != "" && u.FactionID != state.PlayerFactionID {
			continue // 只为本局玩家阵营单位拉取（敌方/野怪不进玩家命运层）
		}
		if u.Status.LifeState == unit.LifeStateDead {
			continue
		}
		_, _ = service.SurfaceCrossEventsForCharacter(ctx, state.ID, worldID, &u, crossSurfaceLimitPerBoundary)
	}
}

// RecordCrossInteraction 记录一次跨玩家交互：先用世界权威时钟发号（AdvanceTick），再把事件追加进世界总线。
// 这是「谁先动手算谁的」的落地——tick 由世界时钟单调发放，保证全世界事件可全序仲裁。返回事件 ID。
//
// 发号 + 追加在**同一事务**内：既保证两步原子（不会发了号却没记事件），又为 MySQL 把 AdvanceTick 的
// SELECT...FOR UPDATE 锁定在同一连接上（FOR UPDATE 行锁仅在事务内有效）。
func (service *Service) RecordCrossInteraction(ctx context.Context, worldID string, actorID string, targetID string, kind worldbus.EventKind, importance int, payload any) (string, error) {
	if service == nil || service.db == nil {
		return "", fmt.Errorf("record cross interaction: missing db")
	}
	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin cross interaction tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // Commit 后为 no-op

	tick, err := world.AdvanceTick(ctx, tx, worldID, dbdialect.For(service.db))
	if err != nil {
		return "", err
	}
	id, err := worldbus.Append(ctx, tx, worldbus.CrossEvent{
		WorldID:    worldID,
		ActorID:    actorID,
		TargetID:   targetID,
		Kind:       kind,
		Importance: importance,
		WorldTick:  tick,
		Payload:    payload,
	})
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit cross interaction: %w", err)
	}
	return id, nil
}

// SurfaceCrossEventsForCharacter 拉取世界总线上牵涉该角色的跨玩家事件，逐条按相关性投进她的命运收件箱。
// 返回被实际惊动（进高光卡/待决策）的条数。limit<=0 时取默认。
func (service *Service) SurfaceCrossEventsForCharacter(ctx context.Context, sessionID string, worldID string, character *unit.Record, limit int) (int, error) {
	if service == nil || service.db == nil || character == nil {
		return 0, fmt.Errorf("surface cross events: missing dependencies")
	}
	crossEvents, err := worldbus.ListForCharacter(ctx, service.db, worldID, character.ID, limit)
	if err != nil {
		return 0, err
	}
	surfaced := 0
	for _, ce := range crossEvents {
		routing, err := service.SurfaceFateEvent(ctx, sessionID, character, crossEventToFate(ce, character.ID))
		if err != nil {
			return surfaced, err
		}
		if routing.Route != relevance.RouteAutonomous {
			surfaced++
		}
	}
	return surfaced, nil
}

// crossEventToFate 把一条跨玩家事件从「她」的视角翻译成命运事件。
// 这是她的收件箱条目，故 TargetID 固定为她本人：既保证走「直接发生在她身上」的自相关路径，
// 又让底层 events 表的 target_unit_id 落在本库真实存在的她身上（对手可能是跨分片角色、非本库 units 行，
// 不能进受 FK 约束的列）。对手身份保留在 ActorID（仅入 payload，非 FK）与措辞里。
func crossEventToFate(ce worldbus.CrossEvent, characterID string) FateEvent {
	importance := ce.Importance
	if importance <= 0 {
		importance = crossEventDefaultImportance
	}
	counterpart := ce.ActorID
	if counterpart == characterID {
		counterpart = ce.TargetID // 她是发起方时，对手是 target
	}
	return FateEvent{
		ActorID:       counterpart,
		TargetID:      characterID,
		ReasonCode:    crossKindReason(ce.Kind),
		Importance:    importance,
		EmotionWeight: crossKindValence(ce.Kind),
		Summary:       crossSummary(ce, characterID),
	}
}

// crossKindValence 跨玩家交互的情绪效价（驱动祖魂语气基调与相关性强度）。
func crossKindValence(kind worldbus.EventKind) float64 {
	switch kind {
	case worldbus.KindRescue:
		return 0.6
	case worldbus.KindGift:
		return 0.4
	case worldbus.KindAlliance:
		return 0.3
	case worldbus.KindWorldBossDown:
		return 0.7
	case worldbus.KindBetrayal:
		return -0.7
	case worldbus.KindAttack:
		return -0.6
	case worldbus.KindWorldBossStrike:
		return -0.1
	default:
		return 0
	}
}

// crossKindReason 选一个语气合适的 reason-code（仅用于祖魂语气基调；落库 reason 由 SurfaceFateEvent 决定）。
func crossKindReason(kind worldbus.EventKind) events.ReasonCode {
	switch kind {
	case worldbus.KindRescue:
		return events.ReasonRelationRescue
	case worldbus.KindBetrayal:
		return events.ReasonRelationBetray
	case worldbus.KindAttack:
		return events.ReasonCombatHit
	case worldbus.KindGift:
		return events.ReasonEconomyReward
	default:
		return events.ReasonRelevanceMatch
	}
}

// crossSummary 从「她」的视角措辞（跨分片角色暂无显示名，用克制而有画面的通用措辞）。
func crossSummary(ce worldbus.CrossEvent, characterID string) string {
	isActor := ce.ActorID == characterID
	switch ce.Kind {
	case worldbus.KindRescue:
		if isActor {
			return "她在危急关头，把一个素不相识的人从鬼门关拉了回来。"
		}
		return "危急关头，有人拼死把她拉了回来。"
	case worldbus.KindBetrayal:
		if isActor {
			return "她对曾经信任她的人，下了狠手。"
		}
		return "她信过的人，在背后捅了她一刀。"
	case worldbus.KindAttack:
		if isActor {
			return "她对别家的人动了手。"
		}
		return "有人朝她动了手。"
	case worldbus.KindGift:
		if isActor {
			return "她把要紧的东西，赠给了远方的某个人。"
		}
		return "远方有人，馈赠了她一份要紧的东西。"
	case worldbus.KindAlliance:
		return "她与另一方，结成了同盟。"
	case worldbus.KindWorldBossStrike:
		return "她朝那头撼动全境的巨物，挥出了一击。"
	case worldbus.KindWorldBossDown:
		return "那头撼动全境的巨物，终于倒下了——她也在场。"
	default:
		return "一件牵动她的事，在世界的另一头发生了。"
	}
}
