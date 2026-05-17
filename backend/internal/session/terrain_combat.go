package session

// 文件说明：计算地形/天气/建筑对射程、机动、攻防与饥饿消耗的战斗修正参数。

import (
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// attackReach 先计算地形与驻守设施带来的基础射程修正。
func attackReach(snapshot world.MapSnapshot, structures []Structure, attacker unit.Record) int {
	reach := 1
	if terrainAt(snapshot, world.Coord{Q: attacker.Status.PositionQ, R: attacker.Status.PositionR}) == world.TerrainMountain {
		reach++
	}
	if structure := structureAt(structures, world.Coord{Q: attacker.Status.PositionQ, R: attacker.Status.PositionR}); structure != nil &&
		structure.FactionID == attacker.FactionID &&
		structureReady(*structure) {
		switch structure.Type {
		case StructureTypeTurret:
			if reach < 3 {
				reach = 3
			}
		case StructureTypeWatchtower:
			if reach < 2 {
				reach = 2
			}
		}
	}
	return reach
}

// attackReachWithWeather 在基础射程上叠加天气修正。
func attackReachWithWeather(state State, attacker unit.Record) int {
	reach := attackReach(state.Map, state.Structures, attacker)
	switch state.Weather.Type {
	case WeatherFoggy:
		if reach > 1 {
			reach--
		}
	}
	if reach < 1 {
		reach = 1
	}
	return reach
}

// weatherAdjustedMoveMultiplier 评估天气/地形对移动效率的乘区影响。
func weatherAdjustedMoveMultiplier(state State, actor unit.Record) float64 {
	multiplier := 1.0
	switch state.Weather.Type {
	case WeatherRainy:
		multiplier *= 0.85
	case WeatherFoggy:
		multiplier *= 0.9
	}

	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrain := terrainAt(state.Map, coord)
	if state.Weather.Type == WeatherRainy {
		switch terrain {
		case world.TerrainRoad, world.TerrainRiverValley:
			multiplier *= 1.1
		case world.TerrainSwamp, world.TerrainRiver:
			multiplier *= 0.8
		}
	}
	if multiplier < 0.2 {
		multiplier = 0.2
	}
	return multiplier
}

// weatherAdjustedAdvanceSteps 计算推进动作在天气/地形下的有效步数。
func weatherAdjustedAdvanceSteps(state State, actor unit.Record, desired int) int {
	if desired <= 0 {
		return 0
	}

	steps := desired
	switch state.Weather.Type {
	case WeatherRainy:
		steps--
	case WeatherFoggy:
		if steps > 1 {
			steps = 1
		}
	}

	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrain := terrainAt(state.Map, coord)
	if state.Weather.Type == WeatherRainy {
		switch terrain {
		case world.TerrainRoad:
			steps++
		case world.TerrainSwamp, world.TerrainRiver:
			steps--
		}
	}

	if steps < 1 {
		steps = 1
	}
	if steps > desired {
		steps = desired
	}
	return steps
}

// weatherAdjustedAttackMultiplier 计算天气与距离对攻击稳定性的乘区修正。
func weatherAdjustedAttackMultiplier(state State, attacker unit.Record, target unit.Record) float64 {
	multiplier := 1.0
	distance := unit.HexDistance(
		attacker.Status.PositionQ,
		attacker.Status.PositionR,
		target.Status.PositionQ,
		target.Status.PositionR,
	)
	switch state.Weather.Type {
	case WeatherWindy:
		if distance > 1 {
			multiplier *= 0.9
		}
	case WeatherRainy:
		if distance > 1 {
			multiplier *= 0.9
		}
	case WeatherFoggy:
		if distance > 1 {
			multiplier *= 0.8
		}
	}
	return multiplier
}

// weatherActionHungerPenalty 计算行动额外饥饿消耗惩罚。
func weatherActionHungerPenalty(state State, actor unit.Record) int {
	penalty := 0
	coord := world.Coord{Q: actor.Status.PositionQ, R: actor.Status.PositionR}
	terrain := terrainAt(state.Map, coord)

	switch state.Weather.Type {
	case WeatherRainy:
		penalty++
		if terrain == world.TerrainSwamp || terrain == world.TerrainRiver {
			penalty++
		}
	case WeatherWindy:
		if terrain == world.TerrainDesert {
			penalty++
		}
	}

	return halvedHungerCost(penalty)
}

// calculateDamage 合并地形与设施攻防修正，输出最终伤害（至少为 1）。
func calculateDamage(snapshot world.MapSnapshot, structures []Structure, attacker unit.Record, target unit.Record) int {
	attack := attacker.Status.Attack + terrainAttackBonus(snapshot, attacker, target) + structureAttackBonus(structures, attacker)
	defense := target.Status.Defense + terrainDefenseBonus(snapshot, attacker, target) + structureDefenseBonus(structures, target)

	damage := attack - defense
	if damage < 1 {
		damage = 1
	}
	return damage
}

// terrainAttackBonus 计算攻方与目标地形带来的攻击修正值。
func terrainAttackBonus(snapshot world.MapSnapshot, attacker unit.Record, target unit.Record) int {
	attackerTerrain := terrainAt(snapshot, world.Coord{Q: attacker.Status.PositionQ, R: attacker.Status.PositionR})
	targetTerrain := terrainAt(snapshot, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR})

	bonus := 0
	switch attackerTerrain {
	case world.TerrainMountain:
		bonus += 4
	case world.TerrainGrassland:
		bonus += 1
	}

	switch targetTerrain {
	case world.TerrainRiver:
		bonus += 3
	case world.TerrainSwamp:
		bonus += 1
	}

	return bonus
}

// terrainDefenseBonus 计算目标地形带来的防御修正值。
func terrainDefenseBonus(snapshot world.MapSnapshot, attacker unit.Record, target unit.Record) int {
	_ = attacker
	switch terrainAt(snapshot, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR}) {
	case world.TerrainForest:
		return 4
	case world.TerrainMountain:
		return 6
	case world.TerrainRuins:
		return 3
	case world.TerrainVillage:
		return 2
	case world.TerrainCity:
		return 8
	case world.TerrainRiver:
		return -4
	case world.TerrainSwamp:
		return -2
	default:
		return 0
	}
}

// structureAttackBonus 计算驻守建筑带来的攻击加成。
func structureAttackBonus(structures []Structure, attacker unit.Record) int {
	structure := structureAt(structures, world.Coord{Q: attacker.Status.PositionQ, R: attacker.Status.PositionR})
	if structure == nil || structure.FactionID != attacker.FactionID || !structureReady(*structure) {
		return 0
	}
	switch structure.Type {
	case StructureTypeForge:
		return 4
	case StructureTypeTurret:
		return 8
	case StructureTypeWatchtower:
		return 2
	default:
		return 0
	}
}

// structureDefenseBonus 包含铁匠铺/炮台/瞭望塔驻守时的防御增益。
func structureDefenseBonus(structures []Structure, target unit.Record) int {
	structure := structureAt(structures, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR})
	if structure == nil || structure.FactionID != target.FactionID || !structureReady(*structure) {
		return 0
	}
	switch structure.Type {
	case StructureTypeForge:
		return 3
	case StructureTypeTurret:
		return 2
	case StructureTypeWatchtower:
		return 1
	default:
		return 0
	}
}

// terrainAt 安全读取指定坐标的地形（越界回落为平原）。
func terrainAt(snapshot world.MapSnapshot, coord world.Coord) world.TerrainID {
	if !inBounds(snapshot, coord) {
		return world.TerrainPlains
	}

	index := (coord.R * snapshot.Width) + coord.Q
	if index < 0 || index >= len(snapshot.Tiles) {
		return world.TerrainPlains
	}
	return snapshot.Tiles[index].Terrain
}

// terrainDisplayName 将地形 ID 转换为中文显示名。
func terrainDisplayName(terrain world.TerrainID) string {
	for _, definition := range world.TerrainCatalog() {
		if definition.ID == terrain {
			return definition.DisplayName
		}
	}
	return string(terrain)
}
