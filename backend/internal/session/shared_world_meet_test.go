package session

// 文件说明：共享世界 Phase 2「玩家相遇」的集成测试（对真实 SQLite）。
// 覆盖核心不变量：
//   - flag 开：两账号 A/B 降生进同一共享世界 world_shared_v1 同新手区 → A 的快照含 B 的角色（在 OtherWorldUnits），
//     且 B 绝不在 A 的 PlayerUnits/PlayerUnitIDs/EnemyUnits/WildUnits/AmbientUnits 里（只读上图、不可操作）。
//   - 相遇范围随 travel 走：A travel 到别区后，A 的快照不再含 B（跨区不可见）；B 仍在原区不受影响。
//   - 复合 region_id：A/B 的 region_id 都是 worldID#zoneID（不是 sessionID），跨 session 可被 ListActiveByRegion 命中。
//   - flag 关（默认）：私有档零影响——两账号各自私有世界，OtherWorldUnits 恒空（回归）。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/unit"
)

// containsUnitID 判定一组单位记录里是否含某 ID。
func containsUnitID(records []unit.Record, id string) bool {
	for _, r := range records {
		if r.ID == id {
			return true
		}
	}
	return false
}

// containsString 判定字符串切片是否含某值。
func containsString(ids []string, id string) bool {
	for _, s := range ids {
		if s == id {
			return true
		}
	}
	return false
}

// TestSharedWorldMeet_PeersVisibleInSnapshot 验证 flag 开时：同区两玩家在彼此快照里看得见对方主角，
// 且对方绝不进本玩家的可操作/执行分类。
func TestSharedWorldMeet_PeersVisibleInSnapshot(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	// 两个不同账号降生进同一共享世界世代，出生区都是 zone_neutral_start（同种子 → 同出生区）。
	viewA, err := service.CreateMainWorldCharacter(ctx, "meet-acc-A", MainWorldCharacterInput{Name: "甲玩家"})
	if err != nil {
		t.Fatalf("A 降生失败: %v", err)
	}
	viewB, err := service.CreateMainWorldCharacter(ctx, "meet-acc-B", MainWorldCharacterInput{Name: "乙玩家"})
	if err != nil {
		t.Fatalf("B 降生失败: %v", err)
	}
	if viewA.WorldID != sharedWorldID || viewB.WorldID != sharedWorldID {
		t.Fatalf("两账号应都落进共享世界 %q，得到 A=%q B=%q", sharedWorldID, viewA.WorldID, viewB.WorldID)
	}
	if viewA.UnitID == "" || viewB.UnitID == "" || viewA.UnitID == viewB.UnitID {
		t.Fatalf("两玩家应各有一个独立主角单位，得到 A=%q B=%q", viewA.UnitID, viewB.UnitID)
	}

	// 复合 region_id 校验：A/B 的 region_id 都应是 worldID#zoneID（**不是** sessionID）——这是相遇的地理事实源。
	wantRegion := sharedRegionID(sharedWorldID, "zone_neutral_start")
	if got := service.regionOf(ctx, viewA.UnitID); got != wantRegion {
		t.Fatalf("A 主角 region_id 应是复合 %q，得到 %q", wantRegion, got)
	}
	if got := service.regionOf(ctx, viewB.UnitID); got != wantRegion {
		t.Fatalf("B 主角 region_id 应是复合 %q，得到 %q", wantRegion, got)
	}
	if wantRegion == viewA.SessionID || wantRegion == viewB.SessionID {
		t.Fatalf("复合 region_id 绝不应等于 sessionID（否则退化成 session 私有口径）")
	}

	// A 的快照应含 B 的角色（在 OtherWorldUnits），且 B 绝不在 A 的任何可操作/执行分类里。
	snapA, err := service.GetSnapshot(ctx, viewA.SessionID)
	if err != nil {
		t.Fatalf("取 A 快照失败: %v", err)
	}
	if !containsUnitID(snapA.OtherWorldUnits, viewB.UnitID) {
		t.Fatalf("A 的快照 OtherWorldUnits 应含 B 的角色 %q，实得 %d 个其他玩家", viewB.UnitID, len(snapA.OtherWorldUnits))
	}
	if containsUnitID(snapA.PlayerUnits, viewB.UnitID) {
		t.Fatalf("B 绝不应进 A 的 PlayerUnits（不可被 A 操作）")
	}
	// 权威态校验：B 绝不进 A 这一局的 PlayerUnitIDs（执行 order 的归属源）。
	stateA, _, err := service.loadSession(ctx, viewA.SessionID)
	if err != nil {
		t.Fatalf("载入 A 的 state 失败: %v", err)
	}
	if containsString(stateA.PlayerUnitIDs, viewB.UnitID) {
		t.Fatalf("B 绝不应进 A 这一局的 PlayerUnitIDs")
	}
	if containsUnitID(snapA.EnemyUnits, viewB.UnitID) || containsUnitID(snapA.WildUnits, viewB.UnitID) || containsUnitID(snapA.AmbientUnits, viewB.UnitID) {
		t.Fatalf("B 绝不应进 A 的 EnemyUnits/WildUnits/AmbientUnits（只读上图，不进执行 order）")
	}
	// A 自己的主角仍在 A 的 PlayerUnits，且**不**在 OtherWorldUnits（去重正确）。
	if !containsUnitID(snapA.PlayerUnits, viewA.UnitID) {
		t.Fatalf("A 自己的主角应在 A 的 PlayerUnits")
	}
	if containsUnitID(snapA.OtherWorldUnits, viewA.UnitID) {
		t.Fatalf("A 自己的主角绝不应被误并入 OtherWorldUnits（去重失败）")
	}

	// 对称：B 的快照也应含 A 的角色。
	snapB, err := service.GetSnapshot(ctx, viewB.SessionID)
	if err != nil {
		t.Fatalf("取 B 快照失败: %v", err)
	}
	if !containsUnitID(snapB.OtherWorldUnits, viewA.UnitID) {
		t.Fatalf("B 的快照 OtherWorldUnits 应含 A 的角色 %q", viewA.UnitID)
	}
}

// TestSharedWorldMeet_TravelLeavesRegion 验证相遇范围随 travel 走：A 离开新手区后，A 的快照不再含 B。
func TestSharedWorldMeet_TravelLeavesRegion(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	viewA, err := service.CreateMainWorldCharacter(ctx, "travel-acc-A", MainWorldCharacterInput{Name: "甲"})
	if err != nil {
		t.Fatalf("A 降生失败: %v", err)
	}
	viewB, err := service.CreateMainWorldCharacter(ctx, "travel-acc-B", MainWorldCharacterInput{Name: "乙"})
	if err != nil {
		t.Fatalf("B 降生失败: %v", err)
	}

	// 前置：同区时 A 看得见 B。
	snapBefore, err := service.GetSnapshot(ctx, viewA.SessionID)
	if err != nil {
		t.Fatalf("取 A 快照失败: %v", err)
	}
	if !containsUnitID(snapBefore.OtherWorldUnits, viewB.UnitID) {
		t.Fatalf("travel 前 A 应看见同区的 B")
	}

	// A 从新手区 travel 到自由主城（border 通道，走到即过，必解锁）。
	if err := service.TravelToZone(ctx, viewA.SessionID, viewA.UnitID, "zone_freedom_capital", ""); err != nil {
		t.Fatalf("A travel 到自由主城失败: %v", err)
	}

	// A 的 region_id 应更新为新区复合键（相遇范围跟着走）。
	wantNewRegion := sharedRegionID(sharedWorldID, "zone_freedom_capital")
	if got := service.regionOf(ctx, viewA.UnitID); got != wantNewRegion {
		t.Fatalf("A travel 后 region_id 应更新为 %q，得到 %q", wantNewRegion, got)
	}

	// A 已离开新手区 → A 的快照不再含仍在新手区的 B。
	snapAfter, err := service.GetSnapshot(ctx, viewA.SessionID)
	if err != nil {
		t.Fatalf("取 A travel 后快照失败: %v", err)
	}
	if containsUnitID(snapAfter.OtherWorldUnits, viewB.UnitID) {
		t.Fatalf("A travel 到别区后不应再看见仍在新手区的 B")
	}

	// B 仍在新手区，不受 A travel 影响 → 此刻 B 的快照应不再含 A（A 已离开）。
	snapB, err := service.GetSnapshot(ctx, viewB.SessionID)
	if err != nil {
		t.Fatalf("取 B 快照失败: %v", err)
	}
	if containsUnitID(snapB.OtherWorldUnits, viewA.UnitID) {
		t.Fatalf("A 已离开新手区，B 的快照不应再含 A")
	}
}

// TestSharedWorldMeet_OffPrivateZeroImpact 验证 flag 关（默认）时：私有档零影响——OtherWorldUnits 恒空。
func TestSharedWorldMeet_OffPrivateZeroImpact(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "0")
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	viewA, err := service.CreateMainWorldCharacter(ctx, "off-acc-A", MainWorldCharacterInput{Name: "丙"})
	if err != nil {
		t.Fatalf("A 降生失败: %v", err)
	}
	viewB, err := service.CreateMainWorldCharacter(ctx, "off-acc-B", MainWorldCharacterInput{Name: "丁"})
	if err != nil {
		t.Fatalf("B 降生失败: %v", err)
	}
	// flag 关：两账号各自私有世界 world_default（物理隔离），绝不互相浮现。
	if viewA.WorldID != defaultWorldID || viewB.WorldID != defaultWorldID {
		t.Fatalf("flag 关时应绑私有世界 %q，得到 A=%q B=%q", defaultWorldID, viewA.WorldID, viewB.WorldID)
	}
	// 私有档 region_id 维持 sessionID 口径（绝不被复合 region_id 污染）——验证 §5 风险 2 兼容分支不被破坏。
	// 注：flag 关时 scope 调用整体 no-op，region_id 列保持降生时的默认（NULL，除非 ambientScheduling 开才写 sessionID）。
	if got := service.regionOf(ctx, viewA.UnitID); got == sharedRegionID(sharedWorldID, "zone_neutral_start") {
		t.Fatalf("私有档主角绝不应被赋复合 region_id，得到 %q", got)
	}
	// 核心回归：两份快照的 OtherWorldUnits 恒空（私有档永不相遇）。
	snapA, err := service.GetSnapshot(ctx, viewA.SessionID)
	if err != nil {
		t.Fatalf("取 A 快照失败: %v", err)
	}
	if len(snapA.OtherWorldUnits) != 0 {
		t.Fatalf("flag 关时 A 的 OtherWorldUnits 应为空，得到 %d", len(snapA.OtherWorldUnits))
	}
	snapB, err := service.GetSnapshot(ctx, viewB.SessionID)
	if err != nil {
		t.Fatalf("取 B 快照失败: %v", err)
	}
	if len(snapB.OtherWorldUnits) != 0 {
		t.Fatalf("flag 关时 B 的 OtherWorldUnits 应为空，得到 %d", len(snapB.OtherWorldUnits))
	}
}
