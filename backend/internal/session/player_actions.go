package session

// 文件说明：玩家「在线直接操作角色」的动作（命运开盒的混合模型——离线她自治，上线玩家可直接干预）。
// 这些是玩家即时直驱的动作，与世界自治推进共存：玩家随时可指挥她移动/穿装备，世界往前走时她仍按自治决策行动。
// 只动非受保护字段（位置 PositionQ/R、背包/装备）——不碰 HP/Hunger/Morale/Loyalty/Mood/LivesRemaining（那些恒经 Mutator）。
// 全程校验单位归属本会话，防跨会话越权操作他人角色。

import (
	"context"
	"fmt"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// findUnitInSession 在本会话单位里按 id 找一个可改的记录指针（不属本局返回 nil）。
func findUnitInSession(units []unit.Record, unitID string) *unit.Record {
	for i := range units {
		if units[i].ID == unitID {
			return &units[i]
		}
	}
	return nil
}

// PlayerMoveUnit 玩家直接把某角色移到目标格（在线操作）。校验单位归属本会话 + 目标在地图内 + 非阻挡地形。
// 位置是非受保护字段，直接改 + 持久化。返回移动后的坐标。
func (service *Service) PlayerMoveUnit(ctx context.Context, sessionID, unitID string, q, r int) (int, int, error) {
	if service == nil || service.units == nil {
		return 0, 0, fmt.Errorf("player move: service unavailable")
	}
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return 0, 0, fmt.Errorf("player move: %w", err)
	}
	coord := world.Coord{Q: q, R: r}
	if !inBounds(state.Map, coord) {
		return 0, 0, fmt.Errorf("那里在天地之外，去不得")
	}
	// 阻挡：水域/山地不可直接踏入（与战棋移动同口径，避免她站进河里）。
	switch terrainAt(state.Map, coord) {
	case world.TerrainRiver, world.TerrainMountain:
		return 0, 0, fmt.Errorf("那里过不去（水/山阻路）")
	}
	rec := findUnitInSession(units, unitID)
	if rec == nil {
		return 0, 0, fmt.Errorf("这不是本局的人")
	}
	rec.Status.PositionQ = q
	rec.Status.PositionR = r
	if err := service.units.Save(ctx, *rec); err != nil {
		return 0, 0, fmt.Errorf("player move (save): %w", err)
	}
	return q, r, nil
}

// PlayerEquipItem 玩家给某角色从背包穿上某装备（在线操作）。复用 unit.EquipBackpackItem（含槽位/重算派生攻防）。
func (service *Service) PlayerEquipItem(ctx context.Context, sessionID, unitID, itemID string) error {
	if service == nil || service.units == nil {
		return fmt.Errorf("player equip: service unavailable")
	}
	_, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("player equip: %w", err)
	}
	rec := findUnitInSession(units, unitID)
	if rec == nil {
		return fmt.Errorf("这不是本局的人")
	}
	if err := unit.EquipBackpackItem(rec, itemID); err != nil {
		return err
	}
	return service.units.Save(ctx, *rec)
}
