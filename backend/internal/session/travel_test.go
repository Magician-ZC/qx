package session

// 文件说明：分区大世界阶段1后端地基的集成测试（设计 docs/分区大世界设计方案-2026-06-10.md §1/§8）。
// 覆盖：建局生成多区域世界、出生区投影、主角/NPC 区域归属、travel 可达切区（border/portal）、
// 不可达拒绝、切区后地图投影与 NPC 按区过滤、ZonesOverview 可达性。复用 mainworld_test 的临时 SQLite 夹具。

import (
	"context"
	"testing"
)

// TestZoneFoundation_WorldGenAndTravel 端到端验证分区世界地基。
func TestZoneFoundation_WorldGenAndTravel(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-zone", MainWorldCharacterInput{
		Name: "分区娘", Origin: "边境游侠", Desire: "看遍天下",
	})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	sessionID, unitID := view.SessionID, view.UnitID

	// 1) 世界生成多区域，出生区=新手区，state.Map 投影为出生区地图。
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("载入失败: %v", err)
	}
	if len(state.Zones) != 7 {
		t.Fatalf("默认世界应有 7 区，得到 %d", len(state.Zones))
	}
	if state.CurrentZoneID != "zone_neutral_start" {
		t.Fatalf("应出生在新手区，得到 %q", state.CurrentZoneID)
	}
	if state.Map.Width != state.Zones[0].Map.Width || len(state.Map.Tiles) != len(state.Zones[0].Map.Tiles) {
		t.Fatal("state.Map 应投影为出生区地图")
	}

	// 2) ZonesOverview：7 区，当前=新手区，三主城 border 可达，三野外不可达。
	zones, current, err := service.ZonesOverview(ctx, sessionID)
	if err != nil {
		t.Fatalf("区域总览失败: %v", err)
	}
	if current != "zone_neutral_start" || len(zones) != 7 {
		t.Fatalf("总览口径不符：current=%q n=%d", current, len(zones))
	}
	reachable := map[string]string{}
	for _, z := range zones {
		if z.Reachable {
			reachable[z.ID] = z.PortalKind
		}
	}
	for _, cap := range []string{"zone_freedom_capital", "zone_order_capital", "zone_chaos_capital"} {
		if reachable[cap] != "border" {
			t.Fatalf("新手区应 border 直达 %s，得到 %q", cap, reachable[cap])
		}
	}
	if _, ok := reachable["zone_freedom_wild"]; ok {
		t.Fatal("新手区不应直达野外区（野外只连同阵营主城）")
	}

	// 3) 出生区 NPC 都标了出生区归属（快照按区过滤需要）。
	snap1 := service.snapshotForSession(t, ctx, sessionID)
	if len(snap1.AmbientUnits) == 0 {
		t.Fatal("出生区应有公共 NPC")
	}
	for _, npc := range snap1.AmbientUnits {
		if npc.Status.ZoneID != "zone_neutral_start" {
			t.Fatalf("出生区 NPC %s 区域归属应为新手区，得到 %q", npc.ID, npc.Status.ZoneID)
		}
	}

	// 4) travel 到晨曦城郊（border 可达）→ 成功，地图换、当前区变、主角 ZoneID 更新。
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_freedom_capital", ""); err != nil {
		t.Fatalf("travel 到主城应成功: %v", err)
	}
	state2, _, _ := service.loadSession(ctx, sessionID)
	if state2.CurrentZoneID != "zone_freedom_capital" {
		t.Fatalf("travel 后当前区应为晨曦城郊，得到 %q", state2.CurrentZoneID)
	}
	heroAfter, err := service.units.GetByID(ctx, unitID)
	if err != nil {
		t.Fatalf("重读主角失败: %v", err)
	}
	if heroAfter.Status.ZoneID != "zone_freedom_capital" {
		t.Fatalf("主角 ZoneID 应更新为晨曦城郊，得到 %q", heroAfter.Status.ZoneID)
	}

	// 5) 切区后快照（阶段2 §1 lazy 播种）：出生区 NPC 被过滤掉；新区首次进入已 lazy 播种公共 NPC，
	//    其 ZoneID=晨曦城郊、Growth.Level 落在该区等级带 [5,15] 内（设计 §1/§2/§3）。
	snap2 := service.snapshotForSession(t, ctx, sessionID)
	if len(snap2.AmbientUnits) == 0 {
		t.Fatal("阶段2：首次进入晨曦城郊应 lazy 播种公共 NPC（得到 0 个）")
	}
	capIdx := findZoneIndex(&state2, "zone_freedom_capital")
	if capIdx < 0 {
		t.Fatal("应能找到晨曦城郊区域")
	}
	capZone := state2.Zones[capIdx]
	for _, npc := range snap2.AmbientUnits {
		if npc.Status.ZoneID != "zone_freedom_capital" {
			t.Fatalf("晨曦城郊 lazy NPC %s 区域归属应为晨曦城郊，得到 %q", npc.ID, npc.Status.ZoneID)
		}
		if npc.Stats.Growth.Level < capZone.LevelMin || npc.Stats.Growth.Level > capZone.LevelMax {
			t.Fatalf("晨曦城郊 lazy NPC %s 等级 %d 应落在区域带 [%d,%d]",
				npc.ID, npc.Stats.Growth.Level, capZone.LevelMin, capZone.LevelMax)
		}
	}
	// SeededZoneIDs 记录了晨曦城郊已播种（幂等门）。
	if !zoneSeeded(&state2, "zone_freedom_capital") {
		t.Fatal("travel 后 SeededZoneIDs 应记录晨曦城郊已播种")
	}

	// 6) 从晨曦城郊去自由荒野是 portal（城镇传送门）——阶段1 解锁门：portal 未解锁 → 被拒（带 UnlockTip）。
	//    （border 恒通、portal 阶段1 锁，待阶段3 任务解锁；评审 major4 的解锁门契约）。
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_freedom_wild", ""); err == nil {
		t.Fatal("portal 类（主城→野外）阶段1 应未解锁、被拒，却成功了")
	}

	// 7) 但 border 仍通：从晨曦城郊回新手区（border）→ 成功。
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_neutral_start", ""); err != nil {
		t.Fatalf("border（主城→新手区）应可达: %v", err)
	}

	// 8) 不可达拒绝：从新手区去秩序荒野（无连接）/不存在区。
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_order_wild", ""); err == nil {
		t.Fatal("无连接的野外应不可达，却成功了")
	}
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_nowhere", ""); err == nil {
		t.Fatal("不存在的区应拒绝，却成功了")
	}

	// 9) ZonesOverview 的 Reachable 反映解锁：从新手区，三主城 border → Reachable=true（前端据此可点前往）。
	zones2, _, _ := service.ZonesOverview(ctx, sessionID)
	for _, z := range zones2 {
		if z.ID == "zone_freedom_capital" && !z.Reachable {
			t.Fatal("从新手区 border 直达晨曦城郊，Reachable 应为 true")
		}
	}

	// 10) portal 解锁门的 Reachable=false：主角到主城后，看同阵营野外（portal 类）应 Reachable=false（前端画锁）。
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_freedom_capital", ""); err != nil {
		t.Fatalf("回主城应可达: %v", err)
	}
	zones3, _, _ := service.ZonesOverview(ctx, sessionID)
	var wildSeen bool
	for _, z := range zones3 {
		if z.ID == "zone_freedom_wild" {
			wildSeen = true
			if z.PortalKind != "portal" || z.Reachable {
				t.Fatalf("主城视角下同阵营野外应是 portal 类且未解锁(Reachable=false)，得到 kind=%q reachable=%v", z.PortalKind, z.Reachable)
			}
		}
	}
	if !wildSeen {
		t.Fatal("主城视角的区域总览应含同阵营野外区")
	}
}

// snapshotForSession 是测试 helper：载入会话并构造快照（取 AmbientUnits 等按区过滤后视图）。
func (service *Service) snapshotForSession(t *testing.T, ctx context.Context, sessionID string) Snapshot {
	t.Helper()
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("快照载入失败: %v", err)
	}
	return buildSnapshot(state, units)
}
