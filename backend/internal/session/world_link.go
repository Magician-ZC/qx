package session

// 文件说明：把一局会话接入某个世界，并在实战交互发生时自动写进世界总线（设计 耦合 §1 / 大世界 §8）。
// 现阶段当前对局是单人战棋（敌方是 NPC），自动写总线只在 state.WorldID 非空（接入多世界）时生效——
// 这是为「世界里有别的玩家角色」预备的管线：一旦多世界落地，击杀/救援/馈赠即自动成为跨玩家事实源。

import (
	"context"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

// AssignSessionToWorld 把本局接入某世界：设 state.WorldID，并把本局所有单位接入该世界（幂等）。
func (service *Service) AssignSessionToWorld(ctx context.Context, state *State, worldID string) error {
	if service == nil || service.db == nil || state == nil {
		return nil
	}
	state.WorldID = worldID
	if worldID == "" {
		return nil
	}
	dialect := dbdialect.For(service.db)
	for _, id := range append(append([]string{}, state.PlayerUnitIDs...), state.EnemyUnitIDs...) {
		if id != "" {
			_ = world.Join(ctx, service.db, worldID, id, "inhabitant", dialect)
		}
	}
	return nil
}

// recordWorldizedKill 在接入世界后，把一次击杀作为 CROSS_ATTACK 写进世界总线（best-effort，gate 在 WorldID）。
func (service *Service) recordWorldizedKill(ctx context.Context, state *State, attacker *unit.Record, victim *unit.Record) {
	if service == nil || state == nil || state.WorldID == "" || attacker == nil || victim == nil {
		return
	}
	_, _ = service.RecordCrossInteraction(ctx, state.WorldID, attacker.ID, victim.ID,
		worldbus.KindAttack, 8, map[string]any{"fatal": true})
}
