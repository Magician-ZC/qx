package session

// 文件说明：命运主世界地图的「兴趣点(POI)」确定性标注——给地块派生特殊资源、给野外 NPC 派生身上的事件类型，
// 供前端在格子上画徽标 + 点击查看，并作为「她走到附近冒遭遇命运 beat」的触发源。
// 全确定性：只用 sessionID + 坐标 + salt 的 FNV 哈希（**不拌 turn**，否则标注每回合漂移闪烁），禁 time.Now/全局 rand。

import (
	"context"
	"fmt"
	"hash/fnv"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// MapPOIKind 是 POI 类别：地块资源 / 野外 NPC 身上的事件。
type MapPOIKind string

const (
	MapPOIResource MapPOIKind = "resource"  // 地块特殊资源（矿脉/药田/灵泉/古迹）
	MapPOINPCEvent MapPOIKind = "npc_event" // 野外 NPC 身上的事件（奇遇/求助/埋伏/行商/迷途）
)

// MapPOI 是地图上一个兴趣点（前端画徽标 + 点击查看用）。
type MapPOI struct {
	Q        int        `json:"q"`
	R        int        `json:"r"`
	Kind     MapPOIKind `json:"kind"`
	TypeCode string     `json:"type_code"` // 矿脉/药田/灵泉/古迹 或 奇遇/求助/埋伏/行商/迷途
	LabelZH  string     `json:"label_zh"`  // 展示文案
	UnitID   string     `json:"unit_id,omitempty"`
	Consumed bool       `json:"consumed"` // 已被采完/探完（State.ConsumedPOIs 按坐标标记），前端徽标变淡
}

// npcEventTypes 是野外 NPC 身上可能携带的事件类型（确定性抽一个）。
var npcEventTypes = []string{"奇遇", "求助", "埋伏", "行商", "迷途"}

// poiRoll 返回 [0,1) 的确定性掷骰：sessionID + 坐标 + salt（不含 turn，POI 是地图静态标注）。
func poiRoll(sessionID string, coord world.Coord, salt string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(sessionID))
	_, _ = hasher.Write([]byte("|"))
	_, _ = hasher.Write([]byte(coordString(coord)))
	_, _ = hasher.Write([]byte("|"))
	_, _ = hasher.Write([]byte(salt))
	return float64(hasher.Sum32()%10000) / 10000
}

// resourceForTerrain 把地形映射到候选特殊资源 typeCode（不产 POI 的地形返回空串）。
func resourceForTerrain(terrain world.TerrainID) string {
	switch terrain {
	case world.TerrainMountain:
		return "矿脉"
	case world.TerrainForest:
		return "药田"
	case world.TerrainRiver, world.TerrainRiverValley:
		return "灵泉"
	case world.TerrainRuins:
		return "古迹遗物"
	default:
		return ""
	}
}

// resourcePOITypeAt 与 computeMapResourcePOIs 完全同口径地判断「某格是否派生特殊资源 POI」，
// 返回 typeCode（矿脉/药田/灵泉/古迹遗物）。供地块动作目录（tile_affordances）与采集加成（executeGather）复用，
// 避免派生规则出现第二份口径。废墟(古迹)放宽阈值近乎必产；其余地形稀疏 (<0.12)。确定性、纯函数。
func resourcePOITypeAt(sessionID string, terrain world.TerrainID, coord world.Coord) (string, bool) {
	typeCode := resourceForTerrain(terrain)
	if typeCode == "" {
		return "", false
	}
	threshold := 0.12
	if terrain == world.TerrainRuins {
		threshold = 0.85 // 废墟≈古迹，近乎必产
	}
	if poiRoll(sessionID, coord, "resource") >= threshold {
		return "", false
	}
	return typeCode, true
}

// unconsumedResourcePOIAt 判断某格是否存在「尚未被消耗」的特殊资源 POI，返回 typeCode。
// 派生与 computeMapResourcePOIs 同口径 + 消耗态查 State.ConsumedPOIs（poi_state.go）。
func unconsumedResourcePOIAt(state *State, q int, r int) (string, bool) {
	if state == nil {
		return "", false
	}
	coord := world.Coord{Q: q, R: r}
	typeCode, ok := resourcePOITypeAt(state.ID, terrainAt(state.Map, coord), coord)
	if !ok {
		return "", false
	}
	if isPOIConsumed(state, q, r) {
		return "", false
	}
	return typeCode, true
}

// computeMapResourcePOIs 确定性地给部分地块派生特殊资源 POI（稀疏，少而点睛）。
// 派生口径收敛在 resourcePOITypeAt；下发时按 ConsumedPOIs 标 consumed（徽标变淡）。
func computeMapResourcePOIs(state State) []MapPOI {
	out := make([]MapPOI, 0, 16)
	for _, tile := range state.Map.Tiles {
		typeCode, ok := resourcePOITypeAt(state.ID, tile.Terrain, tile.Coord)
		if !ok {
			continue
		}
		out = append(out, MapPOI{
			Q:        tile.Coord.Q,
			R:        tile.Coord.R,
			Kind:     MapPOIResource,
			TypeCode: typeCode,
			LabelZH:  typeCode,
			Consumed: isPOIConsumed(&state, tile.Coord.Q, tile.Coord.R),
		})
	}
	return out
}

// （terrainAt 复用 terrain_combat.go 的同名 helper：越界回落平原、按 index=(R*Width)+Q 寻址。）

// npcEventTypeFor 给某野外 NPC 确定性派生身上的事件类型（与 computeMapNPCEventPOIs 同口径，供触发复用）。
func npcEventTypeFor(sessionID string, coord world.Coord, unitID string) string {
	idx := int(poiRoll(sessionID, coord, "npc_event:"+unitID) * float64(len(npcEventTypes)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(npcEventTypes) {
		idx = len(npcEventTypes) - 1
	}
	return npcEventTypes[idx]
}

// computeMapNPCEventPOIs 给每个野外 NPC 派生一个身上的事件类型（确定性，按 NPC id + 位置）。
func computeMapNPCEventPOIs(state State, byID map[string]*unit.Record) []MapPOI {
	out := make([]MapPOI, 0, len(state.WildUnitIDs))
	for _, id := range state.WildUnitIDs {
		rec := byID[id]
		if rec == nil {
			continue
		}
		if rec.Status.LifeState == unit.LifeStateDead {
			continue // 死者不再派生地图事件 POI（防幽灵：死亡链已摘 id，此处兜底战斗击杀等残留）
		}
		coord := world.Coord{Q: rec.Status.PositionQ, R: rec.Status.PositionR}
		typeCode := npcEventTypeFor(state.ID, coord, rec.ID)
		out = append(out, MapPOI{
			Q:        coord.Q,
			R:        coord.R,
			Kind:     MapPOINPCEvent,
			TypeCode: typeCode,
			LabelZH:  typeCode,
			UnitID:   rec.ID,
			// NPC 事件的消耗态跟人走（unitID 键）：NPC 游走后徽标仍保持「已探过」，不会在新格重新点亮。
			Consumed: isNPCEventConsumed(&state, rec.ID),
		})
	}
	return out
}

// MapPOIs 加载某会话、计算其地图全部 POI（资源 + 野外 NPC 事件）。只读、确定性。
func (service *Service) MapPOIs(ctx context.Context, sessionID string) ([]MapPOI, error) {
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("map pois: %w", err)
	}
	byID := mapRecordsByID(units)
	pois := computeMapResourcePOIs(state)
	pois = append(pois, computeMapNPCEventPOIs(state, byID)...)
	return pois, nil
}
