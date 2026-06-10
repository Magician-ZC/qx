// 文件说明：分区大世界的「区域穿行」（travel）——主角在区域间移动（设计 docs/分区大世界设计方案-2026-06-10.md §8.3）。
// 玩家直驱写操作：过 guardPlayerAction 五道门 + 校验「当前区域有传送门/边界通向目标区域」，
// 然后 setCurrentZone 切区（state.Map 重投影为目标区地图）+ 主角落点坐标 + 更新主角 ZoneID + 落痕。
// 另提供 ZonesOverview 供前端世界地图渲染（区域列表 + 阵营/等级 + 当前区 + 从当前区可达的区域）。

package session

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/world"
)

// ZoneSummary 是给前端世界地图的一个区域摘要（不含整张地图，轻量）。
type ZoneSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	FactionID  string `json:"faction_id"`
	Kind       string `json:"kind"`
	LevelMin   int    `json:"level_min"`
	LevelMax   int    `json:"level_max"`
	IsCurrent  bool   `json:"is_current"`  // 主角当前所在
	Reachable  bool   `json:"reachable"`   // 从当前区有传送门/边界直达
	PortalKind string `json:"portal_kind"` // 直达方式 border/portal（reachable 时有）
	// BossDefeated 是本区 boss 是否已被讨平（来自服务端权威 state.DefeatedBosses）——
	// 暴露给前端做「已讨平」置灰的权威依据，使置灰态跨页面刷新/跨设备持久（不再只靠前端本地即时态）。
	BossDefeated bool `json:"boss_defeated"`
}

// ZonesOverview 返回世界全部区域摘要 + 当前区域 id（前端世界地图据此渲染 + 高亮 + 可达判定）。
// 单区/旧档（Zones 空）返回空列表（前端据此退回纯单图，不显示世界地图）。
func (service *Service) ZonesOverview(ctx context.Context, sessionID string) ([]ZoneSummary, string, error) {
	if service == nil || service.sessions == nil {
		return nil, "", fmt.Errorf("zones overview: service unavailable")
	}
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return nil, "", err
	}
	if len(state.Zones) == 0 {
		return []ZoneSummary{}, "", nil
	}
	portals := make(map[string]world.ZonePortal, len(state.Zones)) // toZoneID → 当前区通向它的传送门
	if zone, ok := currentZone(&state); ok {
		for _, portal := range zone.Portals {
			portals[portal.ToZoneID] = portal
		}
	}
	summaries := make([]ZoneSummary, 0, len(state.Zones))
	for _, zone := range state.Zones {
		portal, has := portals[zone.ID]
		summaries = append(summaries, ZoneSummary{
			ID:        zone.ID,
			Name:      zone.Name,
			FactionID: zone.FactionID,
			Kind:      string(zone.Kind),
			LevelMin:  zone.LevelMin,
			LevelMax:  zone.LevelMax,
			IsCurrent: zone.ID == state.CurrentZoneID,
			// Reachable 反映「解锁后可达」：border 恒可达；portal 阶段1 未解锁 → 不可达（前端据 PortalKind=="portal"
			// 且 Reachable==false 画「锁」）。与 TravelToZone 的解锁门契约一致，阶段3 接任务解锁后同步放开。
			Reachable:  has && zonePortalUnlocked(&state, portal),
			PortalKind: portal.Kind, // 仍暴露（has=false 时为空串），供前端区分边界/传送门/不通
			// 权威防刷态：本区 boss 是否已讨平（state.DefeatedBosses）——前端据此跨刷新置灰挑战按钮。
			BossDefeated: zoneBossDefeated(&state, zone.ID),
		})
	}
	return summaries, state.CurrentZoneID, nil
}

// TravelToZone 把主角从当前区域穿行到 toZoneID（设计 §8.3）。
// 校验：玩家直驱五道门 → 当前区有通向 toZone 的传送门/边界 → 解析落点 → 切区投影 → 主角落点 + ZoneID → 落痕。
func (service *Service) TravelToZone(ctx context.Context, sessionID, unitID, toZoneID, toCoordKey string) error {
	if service == nil || service.units == nil {
		return fmt.Errorf("travel: service unavailable")
	}
	toZoneID = strings.TrimSpace(toZoneID)
	if toZoneID == "" {
		return fmt.Errorf("要去哪片天地，先指明白")
	}
	state, _, rec, release, err := service.guardPlayerAction(ctx, sessionID, unitID)
	if err != nil {
		return err
	}
	defer release()
	if len(state.Zones) == 0 {
		return fmt.Errorf("这方世界尚未分疆裂土")
	}
	if toZoneID == state.CurrentZoneID {
		return fmt.Errorf("她已身在此地")
	}
	// 可达性：当前区必须有一条传送门/边界通向目标区域。
	portal, ok := zonePortalTo(&state, toZoneID)
	if !ok {
		return fmt.Errorf("从这里去不了那片天地")
	}
	// 解锁门（设计 §8.3「解锁制」）：border（相邻区域边界）恒通；portal（城镇传送门）阶段1 暂未解锁，
	// 待阶段3 任务系统的 UnlockZone 接入「已解锁区域集合」后放开。未解锁返回该传送门的中文提示。
	if !zonePortalUnlocked(&state, portal) {
		tip := strings.TrimSpace(portal.UnlockTip)
		if tip == "" {
			tip = "这道传送门尚未开通"
		}
		return fmt.Errorf("%s", tip)
	}
	// 落点：优先用传送门指定的落点，其次请求指定，最后退回新区中心。
	coordKey := strings.TrimSpace(portal.ToCoord)
	if coordKey == "" {
		coordKey = strings.TrimSpace(toCoordKey)
	}
	// 切区前记下来路区（用于回程 portal 解锁，防穿过单向解锁的 portal 后被困新区）。
	fromZoneID := state.CurrentZoneID
	// 切区 + 投影（state.Map 变为目标区地图）。
	zone, err := setCurrentZone(&state, toZoneID)
	if err != nil {
		return err
	}
	// 回程解锁（阶段3 §6）：成功穿过一道 portal 进入新区后，把「来路区」记入 UnlockedZones——
	// 这样回程的 portal（worldgen 把 capital↔wild 连成双向 portal）也随之解锁，玩家不会被困在新区。
	// border 走到即过、本就不查解锁集，此 append 对 border 回程无副作用（幂等）。
	if portal.Kind == "portal" && fromZoneID != "" {
		appendUnlockedZone(&state, fromZoneID)
	}
	// 阶段2 §1：首次进入某区则 lazy 播种公共 NPC（复用 SeedFactionSpawn，标 ZoneID + 等级带派生 + 入 AmbientUnitIDs）。
	// 须在 setCurrentZone 之后（state.Map 已投影为目标区）、saveSessionMergingExternalEvents 之前调，使新增的
	// AmbientUnitIDs / SeededZoneIDs 随本次 session Save 一并落库。best-effort：播种失败绝不阻断 travel。
	service.ensureZoneSeededBestEffort(ctx, &state, toZoneID)
	coord, ok := parseCoordKey(coordKey)
	if !ok || !inBounds(state.Map, coord) {
		coord = world.Coord{Q: zone.Map.Width / 2, R: zone.Map.Height / 2}
	}
	// 阻挡地形门（与 PlayerMoveUnit 同口径）：落点为水/山时纠偏到可走格，避免主角落进河里/山上。
	if t := terrainAt(state.Map, coord); t == world.TerrainRiver || t == world.TerrainMountain {
		coord = walkableLanding(state.Map, zone, coord)
	}
	// 主角落点 + 区域归属更新前暂存旧值，供 session 落库失败时补偿回滚（关闭跨存储一致性窗口）。
	// 保留「units.Save 先行」顺序是自愈关键：session 未提交前旧区传送门仍在，玩家重试可重新自愈
	// （翻转顺序会被上方「她已身在此地」门卡死，造成不可恢复态）。位置/ZoneID 非受保护字段，直改。
	prevQ, prevR, prevZone := rec.Status.PositionQ, rec.Status.PositionR, rec.Status.ZoneID
	rec.Status.PositionQ = coord.Q
	rec.Status.PositionR = coord.R
	rec.Status.ZoneID = toZoneID
	if err := service.units.Save(ctx, *rec); err != nil {
		return fmt.Errorf("travel (save unit): %w", err)
	}
	// 共享世界 Phase 2「玩家相遇」：若本局是共享世界局，主角随 travel 把**复合 region_id** 更新为目标区
	// （worldID#toZoneID），使「相遇范围」跟着她走——到新区即对该区其他玩家可见、离开旧区即从旧区玩家快照消失。
	// 仅 flag 开 + 共享世代生效；私有档 travel 此调用整体 no-op，绝不改 region_id（维持 sessionID 口径，零影响）。
	// best-effort：scope 更新失败不回滚 travel（相遇可见性是增强，绝不让它拖垮已成功的穿行）。
	service.scopeSharedWorldUnitsToZoneBestEffort(ctx, &state, toZoneID, []string{rec.ID})
	// 任务进度 hook（阶段3 §5.3）：到达新区 → 推进匹配的 reach_zone objective（target=toZoneID）。
	// best-effort、纯逻辑（只改 state.ActiveQuests，随下方 saveSessionMergingExternalEvents 一并落库）。
	advanceQuestObjectives(&state, ObjectiveReachZone, toZoneID, 1)
	// 落痕：一句中文叙事 + 流程事件（她跨越了一片天地，作生活 beat 冒进命运 feed）。
	narrative := fmt.Sprintf("依你的指引，%s跋涉来到了「%s」。", rec.DisplayName(), zone.Name)
	appendLog(&state, "travel", narrative, rec.ID, "")
	if service.db != nil {
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:   state.ID,
			OwnerUnitID: rec.ID,
			Code:        events.ReasonLifeBeat,
			Category:    events.CategoryLifecycle,
			Payload: map[string]any{
				"narrative":  narrative,
				"unit_id":    rec.ID,
				"turn":       state.TurnState.Turn,
				"kind":       "travel",
				"to_zone":    toZoneID,
				"importance": 3,
			},
			WorldID:  state.WorldID,
			RegionID: state.ID,
			Tick:     state.TurnState.Turn,
		})
	}
	service.pushRealtime(state.ID, "fate_life_beat", map[string]any{
		"unit_id":   rec.ID,
		"narrative": narrative,
		"turn":      state.TurnState.Turn,
	})
	if err := service.saveSessionMergingExternalEvents(ctx, &state); err != nil {
		// session 未落库则主角已落新区坐标/ZoneID 而 CurrentZoneID 仍旧——补偿回滚 unit 保两库一致（同旧区），
		// 让玩家重试可重新自愈（旧区传送门仍在，不触发上方「她已身在此地」门）。best-effort 回滚。
		rec.Status.PositionQ, rec.Status.PositionR, rec.Status.ZoneID = prevQ, prevR, prevZone
		_ = service.units.Save(ctx, *rec)
		return fmt.Errorf("travel (save session): %w", err)
	}
	return nil
}

// walkableLanding 把 travel 落点纠偏到可走格（与 PlayerMoveUnit 的阻挡口径一致）：
// 先试本区首个城镇（settlement 恒为 city/village，可走），再试落点六邻可走格，都不行则返回原坐标（极端兜底，不阻断穿行）。
func walkableLanding(snap world.MapSnapshot, zone world.Zone, fallback world.Coord) world.Coord {
	blocked := func(c world.Coord) bool {
		t := terrainAt(snap, c)
		return t == world.TerrainRiver || t == world.TerrainMountain
	}
	for _, s := range zone.Settlements {
		if c, ok := parseCoordKey(s); ok && inBounds(snap, c) && !blocked(c) {
			return c
		}
	}
	for _, n := range axialNeighbors(fallback) {
		if inBounds(snap, n) && !blocked(n) {
			return n
		}
	}
	return fallback
}
