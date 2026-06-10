package session

// 文件说明：共享世界 Phase 2「玩家相遇」——让同一共享世界、同一区域（zone）的玩家在彼此快照里**看得见对方角色**
// （可见在场，只读上图；不做归属操作/交互——那是 Phase 3）。设计见
// docs/共享世界改造方案-方向B-2026-06-10.md §4 Phase 2 + §5 风险 2（region_id 二义性）。
//
// 三件事，全部 flag + world_id 双门控（仅 QUNXIANG_SHARED_WORLD 开 + world_id==world_shared_v1 才生效）：
//  1. 复合 region_id=worldID#zoneID（sharedRegionID，见 world_link.go）：给共享世界角色一套**专属命名空间**，
//     避开「region_id==sessionID」的二义性——私有档绝不碰，维持 sessionID 口径、零影响。
//  2. scopeSharedWorldUnitsToZoneBestEffort：降生/travel 时把共享世界角色 SetUnitScope 到复合 region_id，
//     使「同区」成为可被 ListActiveByRegion 跨 session 查询的真实地理事实。
//  3. enrichSnapshotWithSharedWorldPeers：构造快照后，额外拉同区**别玩家的主角**并入 OtherWorldUnits（新分类）。
//
// 硬约束（必守）：
//   - 跨玩家**只读**：只把别玩家的 unit.Record 读进快照展示，**绝不 Save / 改他人 units**。
//   - 别玩家角色绝不进 PlayerUnitIDs/EnemyUnitIDs/WildUnitIDs/AmbientUnitIDs——不被本玩家操作、不进执行 order、
//     不被 zoneVisibleUnits 误当本局背景 NPC。它们只活在 Snapshot.OtherWorldUnits 这个**纯展示**分类里。
//   - NPC 取舍（Phase 2）：只让**别玩家的主角（protagonist）**可见即可。共享 NPC 是 Phase 4 的事——本阶段
//     同区别 session 的 NPC（ambient/mortal）一律**不**并入（仍 session 私有，各玩家只见自己那套），
//     优先把「看见别的真人玩家角色」这条头号体验跑通，且避免误把别玩家的私有 NPC 当成世界共享内容上图。

import (
	"context"
	"strings"

	"qunxiang/backend/internal/unit"
)

// scopeSharedWorldUnitsToZoneBestEffort 把一组共享世界单位 SetUnitScope 到「复合 region_id=worldID#zoneID」。
//
// 仅在 inSharedWorld(state)（flag 开 + 共享世代）且 zoneID 非空时生效；否则整体 no-op（私有档/flag 关绝不改 region_id，
// 维持 ambient_scheduling 的 sessionID 口径——§5 风险 2 兼容分支不被破坏）。world_id 一并写回共享世代（与 region_id 同事务列），
// 使 ListActiveByRegion(复合 region_id) 能跨 session 命中这些单位。
//
// best-effort：逐个 SetUnitScope，任一失败只跳过该单位、不中断调用方（降生/travel 绝不因相遇可见性拖垮）。
// 只动作用域列（world_id/region_id），不碰单位记录主体、不改受保护字段、不触发 Mutator。
func (service *Service) scopeSharedWorldUnitsToZoneBestEffort(ctx context.Context, state *State, zoneID string, unitIDs []string) {
	if service == nil || service.units == nil {
		return
	}
	if !inSharedWorld(state) {
		return // flag 关 / 私有档：region_id 维持 sessionID 口径，零影响。
	}
	regionID := sharedRegionID(state.WorldID, zoneID)
	if regionID == "" {
		return // zoneID 空（单区/旧档残留）：不赋复合 region_id，保守不动（避免把单位锚进无效区）。
	}
	for _, id := range unitIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		_ = service.units.SetUnitScope(ctx, id, state.WorldID, regionID)
	}
}

// enrichSnapshotWithSharedWorldPeers 在快照构造完成后，把**同区别玩家的主角**并入 snapshot.OtherWorldUnits（只读上图）。
//
// 分叉（与 §5 风险 2 的兼容契约严格对齐）：
//   - 私有档 / flag 关 / world_id 空 / CurrentZoneID 空：直接返回，snapshot 逐字节不变（OtherWorldUnits 留 nil）。
//   - 共享世界角色（inSharedWorld + CurrentZoneID 非空）：ListActiveByRegion(worldID#当前zone) 拉同区所有 active 单位，
//     排除本 session 已在快照里的（去重），只保留**别 session 的主角（LifecycleProtagonist）**并入 OtherWorldUnits。
//
// 只读：只把别玩家 Record 读进快照，绝不 Save/改他人 units。失败 best-effort 吞掉（相遇可见性是增强，绝不拖垮取快照）。
func (service *Service) enrichSnapshotWithSharedWorldPeers(ctx context.Context, state *State, snapshot *Snapshot) {
	if service == nil || service.units == nil || snapshot == nil || state == nil {
		return
	}
	if !inSharedWorld(state) {
		return // 私有档 / flag 关：完全不变（行为逐字节同现状）。
	}
	zoneID := strings.TrimSpace(state.CurrentZoneID)
	if zoneID == "" {
		return // 单区/旧档无当前区：不做相遇合并（保守，与 zoneVisibleUnits 的空 zone 兜底同口径）。
	}
	regionID := sharedRegionID(state.WorldID, zoneID)
	if regionID == "" {
		return
	}
	peers, err := service.units.ListActiveByRegion(ctx, regionID)
	if err != nil || len(peers) == 0 {
		return // 查错/同区无别人：best-effort 静默回退，快照只含本 session 单位。
	}

	// 本 session 已在快照里的单位 ID 集合（主角 + 敌方 + 野外 + 公共 NPC + 选秀池），用于去重——
	// 同 session 的单位绝不重复并入 OtherWorldUnits（它们已在各自分类里）。
	own := make(map[string]struct{}, len(snapshot.PlayerUnits)+len(snapshot.EnemyUnits)+len(snapshot.WildUnits)+len(snapshot.AmbientUnits))
	markOwn := func(records []unit.Record) {
		for _, r := range records {
			own[r.ID] = struct{}{}
		}
	}
	markOwn(snapshot.PlayerUnits)
	markOwn(snapshot.EnemyUnits)
	markOwn(snapshot.WildUnits)
	markOwn(snapshot.AmbientUnits)
	// 兜底：按 state 的 ID 列表也标一遍（覆盖「快照分类因 hidden/zone 过滤剔除了某单位、但它仍属本 session」的残留，
	// 防止本 session 的角色被误当「别玩家」并入）。
	for _, id := range append(append(append([]string{}, state.PlayerUnitIDs...), state.EnemyUnitIDs...), state.AmbientUnitIDs...) {
		own[strings.TrimSpace(id)] = struct{}{}
	}
	for _, id := range state.WildUnitIDs {
		own[strings.TrimSpace(id)] = struct{}{}
	}

	others := make([]unit.Record, 0, len(peers))
	for _, peer := range peers {
		if _, isOwn := own[peer.ID]; isOwn {
			continue // 去重：本 session 自己的单位不重复并入。
		}
		if peer.SessionID == state.ID {
			continue // 双保险：同 session 的单位（即便上面去重漏了）绝不进 OtherWorldUnits。
		}
		// Phase 2 NPC 取舍：只并入**别玩家的主角**（protagonist）。同区别 session 的 NPC（mortal/functional）
		// 仍 session 私有、不在本阶段共享上图（Phase 4 再共享世界 NPC 进度）。
		if peer.Identity.LifecycleClass != unit.LifecycleProtagonist {
			continue
		}
		others = append(others, peer)
	}
	if len(others) == 0 {
		return
	}
	snapshot.OtherWorldUnits = others
}
