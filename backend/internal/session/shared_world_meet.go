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

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/scheduler"
	"qunxiang/backend/internal/unit"
)

// scopeSharedWorldUnitsToZoneBestEffort 把一组共享世界单位 SetUnitScope 到「复合 region_id=worldID#zoneID」。
//
// 仅在 inSharedWorld(state)（flag 开 + 共享世代）且 zoneID 非空时生效；否则整体 no-op（私有档/flag 关绝不改 region_id，
// 维持 ambient_scheduling 的 sessionID 口径——§5 风险 2 兼容分支不被破坏）。world_id 一并写回共享世代（与 region_id 同事务列），
// 使 ListActiveByRegion(复合 region_id) 能跨 session 命中这些单位。
//
// Phase 5「统一世界推进」对齐（关键）：除了把 units 表的 region_id 列改成复合 region，**还须把该单位在唤醒队列
// （agent_wake_queue）里的 region_id 一并改成同一复合值**——否则 seedAmbientForUnits 先以 region_id==sessionID 入队的 wake
// 与 units 列的复合 region_id 长期分裂：region-runner 仍把共享主角调度在 sessionID 这个「每会话自成一区」的桶里，
// 永远不会与同区别玩家在「worldID#zoneID」这个真·地理子区下同节拍 co-tick（也对不上 ListActiveByRegion 的相遇/撮合视图）。
// requeueSharedWorldWakeBestEffort 把 wake 的 region_id 对齐到 units 列，让共享主角真正进入其地理区的唤醒队列。
//
// best-effort：逐个 SetUnitScope，任一失败只跳过该单位、不中断调用方（降生/travel 绝不因相遇可见性拖垮）。
// 只动作用域列（world_id/region_id）+ 唤醒队列 region_id，不碰单位记录主体、不改受保护字段、不触发 Mutator。
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
		if err := service.units.SetUnitScope(ctx, id, state.WorldID, regionID); err != nil {
			continue // 该单位 scope 失败 → 不再对它重排 wake（避免 units 列与 wake 分裂的反向不一致）。
		}
		// 把唤醒队列里这条单位的 wake 对齐到复合 region_id，使 region-runner 在共享地理子区下调度它（Phase 5）。
		service.requeueSharedWorldWakeBestEffort(ctx, state, id, regionID)
	}
}

// requeueSharedWorldWakeBestEffort 把共享世界单位在唤醒队列里的 region_id 对齐到复合 region_id=worldID#zoneID。
//
// 背景：seedAmbientForUnits 在降生时以 region_id==sessionID（分片关时的默认口径）把单位入队；随后本玩家
// scope 到复合 region 改了 units 列，但 wake 仍停在 sessionID。region-runner 按 wake 的 region_id 调度，
// 故不对齐 wake 就等于「共享主角的 units 列说它在 worldID#zoneID，调度器却在 sessionID 桶里唤醒它」——
// 永远进不了与同区别玩家同节拍的统一推进。本方法用幂等 EnqueueWake（按 unit_id upsert）把 wake 的 region_id
// 改成复合值，wake_at_tick=0 起始 COLD（首次 processOne 按真实空闲度重分层，与 seed 同口径）。
//
// 门控/安全：
//   - 仅 ambientSchedulingEnabled（region-runner 启用，main 按 QUNXIANG_REGION_RUNNER_ENABLED 注入）+ inSharedWorld 才执行；
//     flag 关 / 私有档 → 整体 no-op，wake 维持 seedAmbientForUnits 的 sessionID 口径，零影响。
//   - best-effort：吞错（相遇/统一推进是增强，绝不拖垮降生/travel）。
//   - 幂等：重复 scope（降生→多次 travel 回同区）安全——按 unit_id upsert 覆盖同一复合 region_id。
//   - SessionID 仍填本单位 owner 的 state.ID（保留期清理键 PurgeExpiredSessionData 按它删，与 seed 口径一致）。
func (service *Service) requeueSharedWorldWakeBestEffort(ctx context.Context, state *State, unitID, regionID string) {
	if service == nil || service.db == nil || !service.ambientSchedulingEnabled {
		return // region-runner 未启用：不动唤醒队列（与 seedAmbientForUnits 的开关口径一致）。
	}
	if !inSharedWorld(state) || strings.TrimSpace(unitID) == "" || strings.TrimSpace(regionID) == "" {
		return
	}
	_ = agentqueue.EnqueueWake(ctx, service.db, agentqueue.WakeEntry{
		UnitID: unitID, SessionID: state.ID, WorldID: state.WorldID, RegionID: regionID,
		WakeAtTick: 0, Tier: string(scheduler.TierCold),
	})
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
