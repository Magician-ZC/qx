package session

// 文件说明：分区大世界阶段2 §1/§2——各区 lazy 播种公共 NPC + 怪物等级带派生。
// 机制（设计 docs/分区大世界设计方案-2026-06-10.md §1/§3）：
//   - 阶段1 建局只在出生区播种公共 NPC，其余 6 区是空的。本文件补上「travel 首次进入某区时给该区 lazy 播种」：
//     复用 SeedFactionSpawnBestEffort（传**该区的 Map**、该区 FactionID、seed 派生），把落库 NPC 的 Status.ZoneID
//     标为该区 id（zoneVisibleUnits 据此过滤后才在该区显示），并 append 进 state.AmbientUnitIDs / state.SeededZoneIDs。
//   - 公共 NPC/野外散人的 unit.Stats.Growth.Level 按所在区域等级带 [LevelMin, LevelMax] 确定性派生
//     （FNV(sessionID+zoneID+unitID)）。Growth.Level 非受保护字段，直改（不走 Mutator）。
// best-effort：播种失败绝不阻断 travel；确定性随机一律 FNV，禁全局 rand。

import (
	"context"
	"hash/fnv"
	"log"

	"qunxiang/backend/internal/faction"
)

// ensureZoneSeededBestEffort 在 travel 成功切区后、Save 前调用：若 zoneID 从未播种过公共 NPC，则给它播种。
//   - 幂等门：zoneID 已在 state.SeededZoneIDs 则直接返回（每区只播一次）。
//   - 播种：复用 SeedFactionSpawnBestEffort，传**该区的 Map**（zone.Map，非 state.Map——虽切区后两者等价，
//     但显式取 zone.Map 更稳健）、该区 FactionID（neutral 区见下方回退）、seed 由 RandomSeed + zoneID 派生（区区不同）。
//   - 落库 NPC：标 Status.ZoneID=zoneID（zoneVisibleUnits 过滤锚）+ 按区域等级带派生 Growth.Level + Save 回库；
//     append 进 state.AmbientUnitIDs（静态上图、不进执行 order）+ state.SeededZoneIDs（标记已播种）。
//
// neutral/中立区处理：SeedFactionSpawn 内部对 faction 调 faction.Normalize，neutral 不被识别（返回空串）→ 直接报错、零造人。
// 中立新手区已在建局播种过（出生区恒在 SeededZoneIDs），故正常不会走到这里给 neutral 播种；万一遇到中立**非出生**区
// （默认世界布局无此情形），SeedFactionSpawn 会安全失败（best-effort 吞错），该区保持空 NPC、仅标记已播种避免反复重试。
//
// **全程 best-effort**：region 解析/播种/标区/Save 任一失败都只 log、不阻断 travel（NPC 是附加体验）。
// 注意：本函数会就地改 state（append 两个切片）与 units（新落库的不在传入 units 里，但 state 切片已更新供 Save）。
func (service *Service) ensureZoneSeededBestEffort(ctx context.Context, state *State, zoneID string) {
	if service == nil || state == nil || zoneID == "" {
		return
	}
	if zoneSeeded(state, zoneID) {
		return // 已播种过 → 幂等 no-op
	}
	idx := findZoneIndex(state, zoneID)
	if idx < 0 {
		return // 不该发生（travel 已校验 zone 存在）；保守不播种
	}
	zone := state.Zones[idx]

	// 中立/无阵营区：SeedFactionSpawn 不认 neutral（faction.Normalize 返回空串、内部报错）→ 跳过造人，但仍标记已播种，
	// 避免每次进该区都重试一次注定失败的播种（确定性、零额外成本）。
	if faction.Normalize(zone.FactionID) == "" {
		appendSeededZone(state, zoneID)
		return
	}

	// 该区独立 seed（RandomSeed 派生 + zoneID 哈希），保证区区 NPC 不同且可复现。
	zoneSeed := state.RandomSeed + int64(zoneSeedSalt(zoneID))
	// regionID 用 zoneID 作该区据点标识（worldID 在场时入世界 + 标 region 作用域，供调度/相遇定位）。
	npcIDs := service.SeedFactionSpawnBestEffort(ctx, state.ID, zone.FactionID, zoneID, zoneSeed, zone.Map)
	if len(npcIDs) == 0 {
		// 播种未落库任何 NPC（已播过/map 缺失/全失败）——仍标记已播种（防反复重试），不 append 空集合。
		appendSeededZone(state, zoneID)
		return
	}

	// 标区归属 + 等级带派生 + Save 回库（best-effort 逐个；失败只 log 不阻断）。
	service.tagZoneAndLevelBestEffort(ctx, state, zoneID, npcIDs)

	state.AmbientUnitIDs = append(state.AmbientUnitIDs, npcIDs...)
	appendSeededZone(state, zoneID)
}

// tagZoneAndLevelBestEffort 给一批新播种的 NPC 标 Status.ZoneID=zoneID + 按区域等级带派生 Growth.Level，Save 回库。
// Status.ZoneID / Stats.Growth.Level 均为非受保护字段，直改（不走 Mutator）。best-effort：单个失败只 log、继续其余。
func (service *Service) tagZoneAndLevelBestEffort(ctx context.Context, state *State, zoneID string, ids []string) {
	if service == nil || service.units == nil || state == nil || zoneID == "" {
		return
	}
	idx := findZoneIndex(state, zoneID)
	if idx < 0 {
		return
	}
	zone := state.Zones[idx]
	for _, id := range ids {
		rec, err := service.units.GetByID(ctx, id)
		if err != nil {
			log.Printf("zone seed: load npc %s for zone tag failed (best-effort): %v", id, err)
			continue
		}
		rec.Status.ZoneID = zoneID
		rec.Stats.Growth.Level = zoneCreatureLevel(state.ID, zoneID, id, zone.LevelMin, zone.LevelMax)
		if err := service.units.Save(ctx, rec); err != nil {
			log.Printf("zone seed: save npc %s zone/level tag failed (best-effort): %v", id, err)
		}
	}
}

// tagZoneCreatureLevelsBestEffort 给一批已属某区的 NPC 仅派生 Growth.Level（不动 ZoneID——出生区 NPC 的 ZoneID
// 已由 tagAmbientZoneBestEffort 设过）。用于建局时给出生区 NPC 补设等级带。best-effort：单个失败只 log、继续其余。
// 幂等友好：已是目标等级的不重复 Save（等级派生确定性，重复调用恒得同值，仅省一次写）。
func (service *Service) tagZoneCreatureLevelsBestEffort(ctx context.Context, state *State, zoneID string, ids []string) {
	if service == nil || service.units == nil || state == nil || zoneID == "" {
		return
	}
	idx := findZoneIndex(state, zoneID)
	if idx < 0 {
		return
	}
	zone := state.Zones[idx]
	for _, id := range ids {
		rec, err := service.units.GetByID(ctx, id)
		if err != nil {
			log.Printf("zone level: load npc %s failed (best-effort): %v", id, err)
			continue
		}
		level := zoneCreatureLevel(state.ID, zoneID, id, zone.LevelMin, zone.LevelMax)
		if rec.Stats.Growth.Level == level {
			continue // 已是目标等级（幂等重调）→ 省一次写
		}
		rec.Stats.Growth.Level = level
		if err := service.units.Save(ctx, rec); err != nil {
			log.Printf("zone level: save npc %s level failed (best-effort): %v", id, err)
		}
	}
}

// zoneSeeded 判定某区是否已 lazy 播种过（在 state.SeededZoneIDs 集合里）。
func zoneSeeded(state *State, zoneID string) bool {
	if state == nil {
		return false
	}
	for _, id := range state.SeededZoneIDs {
		if id == zoneID {
			return true
		}
	}
	return false
}

// appendSeededZone 幂等地把 zoneID 记入 state.SeededZoneIDs（已在则 no-op）。
func appendSeededZone(state *State, zoneID string) {
	if state == nil || zoneID == "" || zoneSeeded(state, zoneID) {
		return
	}
	state.SeededZoneIDs = append(state.SeededZoneIDs, zoneID)
}

// zoneSeedSalt 把 zoneID 哈希成一个稳定的 seed 扰动量（让每区的 RandomSeed 派生子种子互不相同）。
func zoneSeedSalt(zoneID string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("zoneseed"))
	_, _ = h.Write([]byte(zoneID))
	return h.Sum32()
}

// zoneCreatureLevel 据所在区域等级带 [min, max] 为某 NPC/野外散人确定性派生怪物等级（设计 §3）。
// FNV(sessionID+zoneID+unitID) → [min, max]（闭区间）。同输入恒同等级，可复现、禁全局 rand。
// 容错：min>max 时取 min；min<1 时夹到 1（等级最低 1 级）。
func zoneCreatureLevel(sessionID, zoneID, unitID string, min, max int) int {
	if min < 1 {
		min = 1
	}
	if max < min {
		return min
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte("creaturelevel"))
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte(zoneID))
	_, _ = h.Write([]byte(unitID))
	span := uint32(max - min + 1)
	return min + int(h.Sum32()%span)
}
