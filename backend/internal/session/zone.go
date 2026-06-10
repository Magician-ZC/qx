// 文件说明：分区大世界的「当前区域」投影与查询（设计 docs/分区大世界设计方案-2026-06-10.md §1.2）。
// 核心机制：state.Map 恒是 CurrentZoneID 区域地图的投影拷贝——这样渲染/移动/采集/POI/威胁等所有读
// state.Map 的旧逻辑无需改动，天然作用于「当前区域」。travel 切区时调 setCurrentZone 重投影。
// 旧单图存档（Zones 为空）下所有 helper 优雅降级：currentZone 返回零值、setCurrentZone no-op，state.Map 即唯一图。

package session

import (
	"context"
	"fmt"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// findZoneIndex 在 state.Zones 里按 id 找区域下标（找不到返回 -1）。
func findZoneIndex(state *State, zoneID string) int {
	if state == nil {
		return -1
	}
	for i := range state.Zones {
		if state.Zones[i].ID == zoneID {
			return i
		}
	}
	return -1
}

// currentZone 返回主角当前所在区域（含其地图/阵营/等级/传送门）。
// 单区/兼容旧档（Zones 空或 CurrentZoneID 未命中）返回 (zero, false)。
func currentZone(state *State) (world.Zone, bool) {
	idx := findZoneIndex(state, state.CurrentZoneID)
	if idx < 0 {
		return world.Zone{}, false
	}
	return state.Zones[idx], true
}

// setCurrentZone 把主角当前区域切换为 zoneID，并把 state.Map 重投影为该区地图。
// 返回该区域（供调用方读阵营/等级/传送门）。zoneID 不存在返回错误（不改 state）。
func setCurrentZone(state *State, zoneID string) (world.Zone, error) {
	idx := findZoneIndex(state, zoneID)
	if idx < 0 {
		return world.Zone{}, fmt.Errorf("没有这片天地（zone %q）", zoneID)
	}
	state.CurrentZoneID = zoneID
	// 投影：state.Map = 当前区域地图。Tiles 深拷贝（避免与 Zones[idx].Map.Tiles 共享底层数组）——
	// 防日后任何对 state.Map 的 in-place tile 改写（动态地形/迷雾/地块破坏）污染 Zones 里的权威区地图。
	projected := state.Zones[idx].Map
	projected.Tiles = append([]world.Tile(nil), state.Zones[idx].Map.Tiles...)
	state.Map = projected
	return state.Zones[idx], nil
}

// reprojectCurrentZone 是 loadSession 归一化兜底：若有分区且 CurrentZoneID 命中，强制把 state.Map 重投影为
// 当前区地图——把「state.Map 恒等于当前区地图」从约定升级为 load 时强制，防手工改档/未来写路径留下错区投影。
func reprojectCurrentZone(state *State) {
	if state == nil || len(state.Zones) == 0 || state.CurrentZoneID == "" {
		return
	}
	_, _ = setCurrentZone(state, state.CurrentZoneID)
}

// zonePortalUnlocked 判定一条传送门当前是否已解锁可穿行（设计 §8.3「解锁制」）。
// 阶段3 任务解锁制（升级阶段2 的「portal 恒锁」）：
//   - border（相邻区域边界）恒解锁——走到即过；
//   - portal（城镇传送门）须目标区已在 state.UnlockedZones 集合里才解锁（由任务交付的 UnlockZone 落地）。
//
// state 为 nil（极端兜底）时退回阶段2 语义（portal 恒锁），保守不放开。
func zonePortalUnlocked(state *State, portal world.ZonePortal) bool {
	if portal.Kind != "portal" {
		return true // border 恒通
	}
	return zoneUnlocked(state, portal.ToZoneID)
}

// zonePortalTo 在「当前区域」的传送门里找通往 toZoneID 的那条（用于 travel 可达性校验）。
// 找不到返回 (zero, false)——表示当前区域不直接通往目标区域。
func zonePortalTo(state *State, toZoneID string) (world.ZonePortal, bool) {
	zone, ok := currentZone(state)
	if !ok {
		return world.ZonePortal{}, false
	}
	for _, portal := range zone.Portals {
		if portal.ToZoneID == toZoneID {
			return portal, true
		}
	}
	return world.ZonePortal{}, false
}

// parseCoordKey 把 "q,r" 解析成坐标（容错：解析失败返回 (zero, false)）。
func parseCoordKey(key string) (world.Coord, bool) {
	var q, r int
	if _, err := fmt.Sscanf(key, "%d,%d", &q, &r); err != nil {
		return world.Coord{}, false
	}
	return world.Coord{Q: q, R: r}, true
}

// tagAmbientZoneBestEffort 把给定 NPC 标记区域归属（建局时出生区 NPC = 出生区），best-effort 逐个 Save。
// 已是目标区或找不到记录的跳过；用于让快照的「按主角当前区过滤 NPC」生效。
func (service *Service) tagAmbientZoneBestEffort(ctx context.Context, zoneID string, ids []string, units []unit.Record) {
	if service == nil || service.units == nil || zoneID == "" {
		return
	}
	byID := mapRecordsByID(units)
	for _, id := range ids {
		rec, ok := byID[id]
		if !ok || rec == nil || rec.Status.ZoneID == zoneID {
			continue
		}
		rec.Status.ZoneID = zoneID
		_ = service.units.Save(ctx, *rec)
	}
}

// zoneVisibleUnits 按主角当前区域过滤要上图的 NPC（ambient/wild）：只显示 Status.ZoneID 等于
// 主角当前区域的单位。CurrentZoneID 为空（单区/兼容旧档）则不过滤、全显示（与改造前行为一致）。
// 主角自身（PlayerUnits）不经此过滤，恒显示。
func zoneVisibleUnits(state *State, ids []string, byID map[string]unit.Record) []unit.Record {
	all := orderedUnits(ids, byID)
	if state == nil || state.CurrentZoneID == "" {
		return all // 单区/旧档：不过滤
	}
	visible := make([]unit.Record, 0, len(all))
	for _, rec := range all {
		// ZoneID 空的 NPC 视为属于「当前世界的默认显示」——但分区世界里 NPC 落库时已显式设区，
		// 故空 ZoneID 仅出现在迁移残留，保守归当前区显示（不凭空消失）。
		if rec.Status.ZoneID == "" || rec.Status.ZoneID == state.CurrentZoneID {
			visible = append(visible, rec)
		}
	}
	return visible
}
