package world

// 文件说明：分区大世界生成器（设计见 docs/分区大世界设计方案-2026-06-10.md §2）。
// 在 generator.go「单张地图」生成之上的世界编排层：确定性产出多个区域（Zone），每区一张地图 +
// 阵营归属 + 等级带 + 城镇 + 通往其他区域的传送门。世界由「1 中立新手区 + 三阵营各 1 主城区 + 三阵营野外区」
// 组成（魔兽式分区世界），区域间用传送门/边界连成一张可穿行的图。
// 纯 world 层（不依赖 session）：Zone 只是「一张地图 + 元数据」，游戏侧（boss/副本/任务）后续在 session 层挂载。

import "fmt"

// ZoneKind 是区域类型。
type ZoneKind string

const (
	ZoneStarter ZoneKind = "starter" // 中立新手区（出生地，通往三阵营）
	ZoneCapital ZoneKind = "capital" // 阵营主城区
	ZoneWild    ZoneKind = "wild"    // 阵营野外区（高等级）
)

// ZonePortal 是从本区通往另一区域的出口（传送门或边界口）。
type ZonePortal struct {
	AtCoord   string `json:"at_coord"`             // 本区出口坐标 "q,r"
	ToZoneID  string `json:"to_zone_id"`           // 目标区域 id
	ToCoord   string `json:"to_coord"`             // 目标区落点坐标 "q,r"
	Kind      string `json:"kind"`                 // portal(传送门,需解锁) / border(边界,走到即过)
	UnlockTip string `json:"unlock_tip,omitempty"` // 未解锁时的中文提示（portal 用）
}

// Zone 是世界的一个区域：一张地图 + 阵营/等级/城镇/传送门元数据。
type Zone struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	FactionID   string       `json:"faction_id"` // freedom/order/chaos/neutral
	Kind        ZoneKind     `json:"kind"`
	LevelMin    int          `json:"level_min"` // 区域等级带（怪物等级范围）
	LevelMax    int          `json:"level_max"`
	Map         MapSnapshot  `json:"map"`
	Portals     []ZonePortal `json:"portals"`
	Settlements []string     `json:"settlements"` // 本区城镇坐标 "q,r"
	// 阶段2：区域 boss（命运地图可挑战，设计 §4）。仅 capital/wild 区有，neutral/starter 区无（三者均为空）。
	// 全部 omitempty——旧档（阶段1 存档）反序列化为空，向后兼容、零影响。
	BossCoord string `json:"boss_coord,omitempty"` // 区域 boss 坐标 "q,r"（放区域中心或某 settlement）；空=本区无 boss
	BossName  string `json:"boss_name,omitempty"`  // 区域 boss 名（祖魂语气威胁名）
	BossLevel int    `json:"boss_level,omitempty"` // 区域 boss 等级（= LevelMax，威胁强度据此缩放）
	// DungeonCoord 是本区副本入口坐标 "q,r"（设计 §4「副本」）。仅 capital 区有（标在某 settlement/城镇），其余区为空。
	DungeonCoord string `json:"dungeon_coord,omitempty"`
}

// zoneSpec 是区域生成蓝图（worldgen 内部用）。
type zoneSpec struct {
	id        string
	name      string
	factionID string
	kind      ZoneKind
	levelMin  int
	levelMax  int
	width     int
	height    int
}

// 默认世界蓝图：7 区（1 新手 + 三阵营各 1 主城 + 三阵营各 1 野外）。等级带递进 1→25。
// 区域地图先用与命运主世界一致的 24×16（视野裁剪由前端阶段1 配套，届时可调大）。
var defaultWorldSpecs = []zoneSpec{
	{id: "zone_neutral_start", name: "无名谷", factionID: "neutral", kind: ZoneStarter, levelMin: 1, levelMax: 5, width: 24, height: 16},
	{id: "zone_freedom_capital", name: "晨曦城郊", factionID: "freedom", kind: ZoneCapital, levelMin: 5, levelMax: 15, width: 24, height: 16},
	{id: "zone_order_capital", name: "铁律城郊", factionID: "order", kind: ZoneCapital, levelMin: 5, levelMax: 15, width: 24, height: 16},
	{id: "zone_chaos_capital", name: "裂隙城郊", factionID: "chaos", kind: ZoneCapital, levelMin: 5, levelMax: 15, width: 24, height: 16},
	{id: "zone_freedom_wild", name: "自由荒野", factionID: "freedom", kind: ZoneWild, levelMin: 15, levelMax: 25, width: 24, height: 16},
	{id: "zone_order_wild", name: "秩序荒野", factionID: "order", kind: ZoneWild, levelMin: 15, levelMax: 25, width: 24, height: 16},
	{id: "zone_chaos_wild", name: "混乱荒野", factionID: "chaos", kind: ZoneWild, levelMin: 15, levelMax: 25, width: 24, height: 16},
}

// GenerateWorld 确定性生成默认分区世界（同 seed 同世界）。返回的 Zones[0] 恒为新手出生区。
func GenerateWorld(seed int64) []Zone {
	zones := make([]Zone, 0, len(defaultWorldSpecs))
	for i, spec := range defaultWorldSpecs {
		// 每区用 seed 派生的独立子种子（确定性，区与区地形互不相同）。
		zoneSeed := seed + int64(i+1)*1_000_003
		snapshot := GenerateMap(zoneSeed, spec.width, spec.height)
		zone := Zone{
			ID:          spec.id,
			Name:        spec.name,
			FactionID:   spec.factionID,
			Kind:        spec.kind,
			LevelMin:    spec.levelMin,
			LevelMax:    spec.levelMax,
			Map:         snapshot,
			Settlements: settlementCoordsOf(snapshot),
		}
		zones = append(zones, zone)
	}
	wireDefaultPortals(zones)
	wireZoneContent(zones)
	return zones
}

// bossNamesByFaction 给三阵营 capital/wild 区各派一个主题化的区域 boss 名（祖魂语气威胁名）。
// 同阵营的 capital/wild 各用一名，避免重名混淆（前端命运地图可据 BossName 区分）。
var bossNamesByFaction = map[string][2]string{
	"freedom": {"晨曦平原之主·赤鬣兽王", "自由荒野的噬骨魔狼"},
	"order":   {"铁律城郊的钢甲傀儡", "秩序荒野的肃刑巨像"},
	"chaos":   {"裂隙城郊的混沌触手", "混乱荒野的虚空噬主"},
}

// wireZoneContent 给每个 capital/wild 区填充区域 boss（设计 §4）+ 给 capital 区标记副本入口。
//   - 区域 boss：坐标放区域中心（地图中心格，避免落在城镇与功能性 NPC 抢位）；boss 名按阵营/kind 主题化；等级=LevelMax。
//   - 副本入口：仅 capital 区，标在首个 settlement（城镇）坐标；无城镇则不标（前端无入口可点）。
//   - neutral/starter 区无 boss、无副本（出生新手区保持低压力，留作引导）。
//
// 确定性：坐标只由地图尺寸/settlement 派生，无随机；同 seed 同世界同内容。
func wireZoneContent(zones []Zone) {
	for i := range zones {
		zone := &zones[i]
		switch zone.Kind {
		case ZoneCapital, ZoneWild:
			// 区域中心格作 boss 坐标（确定性、与城镇错开）。
			zone.BossCoord = fmt.Sprintf("%d,%d", zone.Map.Width/2, zone.Map.Height/2)
			zone.BossName = bossNameFor(zone.FactionID, zone.Kind)
			zone.BossLevel = zone.LevelMax
		}
		// 副本入口：仅主城区，标在首个城镇坐标（功能性 NPC/任务锚点同处，符合「城镇里有副本入口」直觉）。
		if zone.Kind == ZoneCapital && len(zone.Settlements) > 0 {
			zone.DungeonCoord = zone.Settlements[0]
		}
	}
}

// bossNameFor 按阵营 + kind 取区域 boss 名。capital 用第 0 个、wild 用第 1 个；未知阵营回退通用名。
func bossNameFor(factionID string, kind ZoneKind) string {
	if names, ok := bossNamesByFaction[factionID]; ok {
		if kind == ZoneWild {
			return names[1]
		}
		return names[0]
	}
	return "盘踞此地的凶物"
}

// settlementCoordsOf 扫一张地图，收集城镇（city/village）坐标 "q,r"，供传送门/功能性NPC/任务锚定。
func settlementCoordsOf(snapshot MapSnapshot) []string {
	coords := make([]string, 0, 4)
	for _, tile := range snapshot.Tiles {
		if tile.Terrain == TerrainCity || tile.Terrain == TerrainVillage {
			coords = append(coords, fmt.Sprintf("%d,%d", tile.Coord.Q, tile.Coord.R))
		}
	}
	return coords
}

// portalAnchor 取一个区域适合放传送门的坐标：优先首个城镇，否则地图中心。
func portalAnchor(zone Zone) string {
	if len(zone.Settlements) > 0 {
		return zone.Settlements[0]
	}
	return fmt.Sprintf("%d,%d", zone.Map.Width/2, zone.Map.Height/2)
}

// wireDefaultPortals 给默认 7 区连传送门：
//
//	新手区 ↔ 三主城（border，走到即过，新手自由通往三阵营）；
//	每主城 ↔ 同阵营野外（portal，城镇枢纽传送）；
//	三主城两两 ↔（border，阵营边境战场，高风险通道）。
func wireDefaultPortals(zones []Zone) {
	byID := make(map[string]int, len(zones))
	for i := range zones {
		byID[zones[i].ID] = i
	}
	link := func(fromID, toID, kind, tip string) {
		fi, ok1 := byID[fromID]
		ti, ok2 := byID[toID]
		if !ok1 || !ok2 {
			return
		}
		zones[fi].Portals = append(zones[fi].Portals, ZonePortal{
			AtCoord:   portalAnchor(zones[fi]),
			ToZoneID:  toID,
			ToCoord:   portalAnchor(zones[ti]),
			Kind:      kind,
			UnlockTip: tip,
		})
	}
	capitals := []string{"zone_freedom_capital", "zone_order_capital", "zone_chaos_capital"}
	wilds := map[string]string{
		"zone_freedom_capital": "zone_freedom_wild",
		"zone_order_capital":   "zone_order_wild",
		"zone_chaos_capital":   "zone_chaos_wild",
	}
	// 新手区 ↔ 三主城（双向 border）。
	for _, cap := range capitals {
		link("zone_neutral_start", cap, "border", "")
		link(cap, "zone_neutral_start", "border", "")
	}
	// 每主城 ↔ 同阵营野外（双向 portal）。
	for cap, wild := range wilds {
		link(cap, wild, "portal", "需先在此城落脚才能开通往荒野的传送")
		link(wild, cap, "portal", "")
	}
	// 三主城两两边境互通（border）。
	for i := 0; i < len(capitals); i++ {
		for j := i + 1; j < len(capitals); j++ {
			link(capitals[i], capitals[j], "border", "")
			link(capitals[j], capitals[i], "border", "")
		}
	}
}
