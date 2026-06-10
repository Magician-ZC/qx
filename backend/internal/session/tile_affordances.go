package session

// 文件说明：地块事件系统——「点开一格地图，看她在这里能做什么」的动作目录（affordances）。
// 只读、零 LLM、确定性：动作白名单与 LLM 自治决策完全同口径（复用 production.go 的 terrainSupportsActivity 等谓词，
// 绝不另写第二套规则）。不可用的动作也返回（available=false + reason_zh），供前端置灰展示原因。
// talk/trade 仅列目录（由对话/交易专用端点结算）；poi_encounter 由 poi_encounter.go 的专用端点结算。
// 共享大世界设计修正（2026-06-10）：玩家不能在共用地图上建造/拆除建筑，故目录不含 build/demolish；
// harvest/forge/upgrade 只「使用」世界里已存在的己方设施（NPC 自治建造产生），不创造/改变建筑。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// TilePOIInfo 是某格 POI 的展示信息（派生口径同 map_pois.go，consumed 查 State.ConsumedPOIs）。
type TilePOIInfo struct {
	Kind     string `json:"kind"`      // resource / npc_event
	TypeCode string `json:"type_code"` // 矿脉/药田/灵泉/古迹遗物 或 奇遇/求助/埋伏/行商/迷途
	LabelZH  string `json:"label_zh"`
	UnitID   string `json:"unit_id,omitempty"` // npc_event 时为该野外 NPC
	Consumed bool   `json:"consumed"`
}

// TileStructureInfo 是某格设施的展示信息（从 State.Structures 取）。
type TileStructureInfo struct {
	ID           string `json:"id"`
	Type         string `json:"type"`
	TypeZH       string `json:"type_zh"`
	Progress     int    `json:"progress"`
	Required     int    `json:"required"`
	Complete     bool   `json:"complete"`
	HarvestReady bool   `json:"harvest_ready"` // 仅农田：已完工且到了成熟回合
	OwnerFaction string `json:"owner_faction"`
}

// TileOccupant 是站在该格上的单位摘要。
type TileOccupant struct {
	UnitID    string `json:"unit_id"`
	Name      string `json:"name"`
	FactionID string `json:"faction_id"`
	IsWild    bool   `json:"is_wild"` // 野外 NPC（state.WildUnitIDs）
}

// TileAction 是动作目录里的一项（不可用也下发，前端置灰展示 reason_zh）。
type TileAction struct {
	Action       string `json:"action"`                   // gather/harvest/forge/upgrade/poi_encounter/talk/trade
	Activity     string `json:"activity,omitempty"`       // gather 专用：fish/forage/hunt/mine
	TargetUnitID string `json:"target_unit_id,omitempty"` // talk/trade 专用
	LabelZH      string `json:"label_zh"`
	Available    bool   `json:"available"`
	ReasonZH     string `json:"reason_zh,omitempty"` // 不可用时的中文原因
}

// TileAffordances 是「她在这一格能做什么」的完整目录（只读 DTO）。
type TileAffordances struct {
	Q          int                `json:"q"`
	R          int                `json:"r"`
	Terrain    string             `json:"terrain"`
	TerrainZH  string             `json:"terrain_zh"`
	Landmark   string             `json:"landmark,omitempty"`
	POI        *TilePOIInfo       `json:"poi,omitempty"`
	Structure  *TileStructureInfo `json:"structure,omitempty"`
	Occupants  []TileOccupant     `json:"occupants,omitempty"`
	UnitOnTile bool               `json:"unit_on_tile"`
	Distance   int                `json:"distance"` // 单位到该格的六边形距离
	Actions    []TileAction       `json:"actions"`
}

// 玩家直驱动作的统一中文原因文案。
const (
	tileReasonNotThere     = "她还没走到那里"
	tileReasonNeedAdjacent = "走近些才说得上话" // 仅 talk/trade：允许相邻
	tileReasonPOIConsumed  = "已采掘殆尽"
)

// TileAffordances 加载会话并计算某格的动作目录（只读，不写任何状态）。
// 校验：单位归属本会话（防越权）+ 属玩家方（只能差遣自家人）+ 坐标在界内。
func (service *Service) TileAffordances(ctx context.Context, sessionID string, unitID string, q int, r int) (TileAffordances, error) {
	if service == nil || service.units == nil {
		return TileAffordances{}, fmt.Errorf("tile affordances: service unavailable")
	}
	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		return TileAffordances{}, fmt.Errorf("tile affordances: %w", err)
	}
	actor := findUnitInSession(units, unitID)
	if actor == nil {
		return TileAffordances{}, ErrTileUnitNotFound
	}
	if !isPlayerSideUnit(state, unitID) {
		return TileAffordances{}, ErrTileUnitNotYours
	}
	if !inBounds(state.Map, world.Coord{Q: q, R: r}) {
		return TileAffordances{}, fmt.Errorf("那里在天地之外，去不得")
	}
	return computeTileAffordances(&state, units, actor, q, r), nil
}

// isPlayerSideUnit 判断单位是否属于本会话玩家方（state.PlayerUnitIDs）。
func isPlayerSideUnit(state State, unitID string) bool {
	for _, id := range state.PlayerUnitIDs {
		if id == unitID {
			return true
		}
	}
	return false
}

// isWildUnit 判断单位是否为野外 NPC（state.WildUnitIDs）。
func isWildUnit(state *State, unitID string) bool {
	if state == nil {
		return false
	}
	for _, id := range state.WildUnitIDs {
		if id == unitID {
			return true
		}
	}
	return false
}

// landmarkAt 安全读取指定坐标的地标（寻址口径与 terrainAt 一致：index=(R*Width)+Q，越界回空）。
func landmarkAt(snapshot world.MapSnapshot, coord world.Coord) string {
	if !inBounds(snapshot, coord) {
		return ""
	}
	index := (coord.R * snapshot.Width) + coord.Q
	if index < 0 || index >= len(snapshot.Tiles) {
		return ""
	}
	return snapshot.Tiles[index].Landmark
}

// tilePOIAt 派生某格的 POI 展示信息（口径同 map_pois.go：先看站在本格的野外 NPC 身上的事件，
// 再看地块特殊资源；两者并存时 NPC 事件优先——人比物更醒目）。无 POI 返回 nil。
func tilePOIAt(state *State, byID map[string]*unit.Record, q int, r int) *TilePOIInfo {
	coord := world.Coord{Q: q, R: r}
	for _, id := range state.WildUnitIDs {
		rec := byID[id]
		if rec == nil || rec.Status.LifeState != unit.LifeStateActive {
			continue
		}
		if rec.Status.PositionQ != q || rec.Status.PositionR != r {
			continue
		}
		eventType := npcEventTypeFor(state.ID, coord, rec.ID)
		return &TilePOIInfo{
			Kind:     string(MapPOINPCEvent),
			TypeCode: eventType,
			LabelZH:  eventType,
			UnitID:   rec.ID,
			// NPC 事件的消耗态跟人走（unitID 键），不与同格资源 POI 的坐标键混用。
			Consumed: isNPCEventConsumed(state, rec.ID),
		}
	}
	if typeCode, ok := resourcePOITypeAt(state.ID, terrainAt(state.Map, coord), coord); ok {
		return &TilePOIInfo{
			Kind:     string(MapPOIResource),
			TypeCode: typeCode,
			LabelZH:  typeCode,
			Consumed: isPOIConsumed(state, q, r),
		}
	}
	return nil
}

// computeTileAffordances 纯内存计算动作目录（TileAffordances 与 ExecuteTileAction 复检共用同一份口径）。
func computeTileAffordances(state *State, units []unit.Record, actor *unit.Record, q int, r int) TileAffordances {
	coord := world.Coord{Q: q, R: r}
	terrain := terrainAt(state.Map, coord)
	distance := unit.HexDistance(actor.Status.PositionQ, actor.Status.PositionR, q, r)
	onTile := distance == 0
	byID := mapRecordsByID(units)

	out := TileAffordances{
		Q:          q,
		R:          r,
		Terrain:    string(terrain),
		TerrainZH:  terrainDisplayName(terrain),
		Landmark:   landmarkAt(state.Map, coord),
		POI:        tilePOIAt(state, byID, q, r),
		UnitOnTile: onTile,
		Distance:   distance,
	}

	structure := structureAt(state.Structures, coord)
	if structure != nil {
		out.Structure = &TileStructureInfo{
			ID:           structure.ID,
			Type:         string(structure.Type),
			TypeZH:       structureDisplayName(structure.Type),
			Progress:     structure.BuildProgress,
			Required:     structure.BuildRequired,
			Complete:     structureReady(*structure),
			OwnerFaction: structure.FactionID,
		}
		out.Structure.HarvestReady = structure.Type == StructureTypeFarmland &&
			structureReady(*structure) &&
			structure.HarvestReadyTurn > 0 &&
			state.TurnState.Turn >= structure.HarvestReadyTurn
	}

	for i := range units {
		rec := &units[i]
		if rec.Status.PositionQ != q || rec.Status.PositionR != r {
			continue
		}
		if rec.Status.LifeState != unit.LifeStateActive {
			continue
		}
		out.Occupants = append(out.Occupants, TileOccupant{
			UnitID:    rec.ID,
			Name:      rec.DisplayName(),
			FactionID: rec.FactionID,
			IsWild:    isWildUnit(state, rec.ID),
		})
	}

	out.Actions = buildTileActions(state, actor, structure, out, terrain, onTile, distance)
	return out
}

// requireOnTile 把「必须站在该格」收敛成 (available, reason) 的统一口径。
func requireOnTile(onTile bool, available bool, reason string) (bool, string) {
	if !onTile {
		return false, tileReasonNotThere
	}
	return available, reason
}

// buildTileActions 生成动作目录。动作族与可用性谓词全部复用既有结算路径的口径：
// gather → terrainSupportsActivity；harvest/forge/upgrade → 世界中已有的己方农田/铁匠铺前置。
// 共享大世界（所有人共用一张地图）下，玩家不得在地图上建造/拆除建筑——故 build/demolish 不进目录
// （2026-06-10 设计修正）；harvest/forge/upgrade 只「使用」世界里已存在的己方设施（NPC 自治建造产生），
// 不创造/改变建筑：本格没有相应己方设施时这些动作自然不出现。
func buildTileActions(
	state *State,
	actor *unit.Record,
	structure *Structure,
	info TileAffordances,
	terrain world.TerrainID,
	onTile bool,
	distance int,
) []TileAction {
	actions := make([]TileAction, 0, 12)

	// —— gather：按地形给可用采集活动（farm 收获单列为 harvest，不进 gather）——
	type gatherSpec struct {
		activity ProductionActivity
		label    string
	}
	for _, spec := range []gatherSpec{
		{ProductionActivityForage, "🌿 就地采集"},
		{ProductionActivityHunt, "🏹 在此打猎"},
		{ProductionActivityMine, "⛏ 在此挖矿"},
		{ProductionActivityFish, "🎣 在此垂钓"},
	} {
		if !terrainSupportsActivity(terrain, spec.activity) {
			continue
		}
		available, reason := true, ""
		if spec.activity == ProductionActivityHunt && !hasUsableWeapon(*actor) {
			available, reason = false, "没有趁手的武器"
		}
		available, reason = requireOnTile(onTile, available, reason)
		actions = append(actions, TileAction{
			Action:    "gather",
			Activity:  string(spec.activity),
			LabelZH:   spec.label,
			Available: available,
			ReasonZH:  reason,
		})
	}

	// —— harvest：本格己方完工农田 ——
	if structure != nil && structure.FactionID == actor.FactionID &&
		structure.Type == StructureTypeFarmland && structureReady(*structure) {
		available, reason := true, ""
		if info.Structure == nil || !info.Structure.HarvestReady {
			available = false
			reason = fmt.Sprintf("农田尚未成熟（T%d 成熟）", structure.HarvestReadyTurn)
		}
		available, reason = requireOnTile(onTile, available, reason)
		actions = append(actions, TileAction{
			Action:    "harvest",
			LabelZH:   "🌾 收割农田",
			Available: available,
			ReasonZH:  reason,
		})
	}

	// —— forge / upgrade：使用世界中已有的己方完工铁匠铺（玩家不能自建，仅利用现成设施）——
	if structure != nil && structure.FactionID == actor.FactionID &&
		structure.Type == StructureTypeForge && structureReady(*structure) {
		if forgeable := forgeableEquipmentIDs(*actor); len(forgeable) > 0 {
			available, reason := requireOnTile(onTile, true, "")
			actions = append(actions, TileAction{
				Action:    "forge",
				LabelZH:   fmt.Sprintf("⚒ 锻造%s", displayItemName(forgeable[0])),
				Available: available,
				ReasonZH:  reason,
			})
		} else {
			actions = append(actions, TileAction{
				Action:    "forge",
				LabelZH:   "⚒ 锻造装备",
				Available: false,
				ReasonZH:  "材料不够，打不了铁",
			})
		}
		if upgradeStack, ok := defaultUpgradeTarget(*actor); ok {
			available, reason := requireOnTile(onTile, true, "")
			actions = append(actions, TileAction{
				Action:    "upgrade",
				LabelZH:   fmt.Sprintf("🛠 强化%s到 +%d", displayStackName(upgradeStack), upgradeStack.Level+1),
				Available: available,
				ReasonZH:  reason,
			})
		} else if len(upgradeableEquipment(*actor)) > 0 {
			actions = append(actions, TileAction{
				Action:    "upgrade",
				LabelZH:   "🛠 强化装备",
				Available: false,
				ReasonZH:  "材料不够",
			})
		}
	}

	// —— poi_encounter：本格有 POI 时列出（结算走 poi-encounter 专用端点）——
	if info.POI != nil {
		available, reason := true, ""
		if info.POI.Consumed {
			available, reason = false, tileReasonPOIConsumed
		}
		available, reason = requireOnTile(onTile, available, reason)
		actions = append(actions, TileAction{
			Action:    "poi_encounter",
			LabelZH:   poiEncounterLabel(*info.POI),
			Available: available,
			ReasonZH:  reason,
		})
	}

	// —— talk / trade：本格站着的具名 NPC（不含她自己），允许相邻（distance≤1）。仅列目录，结算走专用端点 ——
	for _, occupant := range info.Occupants {
		if occupant.UnitID == actor.ID || strings.TrimSpace(occupant.Name) == "" {
			continue
		}
		available, reason := true, ""
		if distance > 1 {
			available, reason = false, tileReasonNeedAdjacent
		}
		actions = append(actions, TileAction{
			Action:       "talk",
			TargetUnitID: occupant.UnitID,
			LabelZH:      fmt.Sprintf("💬 与%s交谈", occupant.Name),
			Available:    available,
			ReasonZH:     reason,
		})
		actions = append(actions, TileAction{
			Action:       "trade",
			TargetUnitID: occupant.UnitID,
			LabelZH:      fmt.Sprintf("🪙 与%s交易", occupant.Name),
			Available:    available,
			ReasonZH:     reason,
		})
	}

	return actions
}

// poiEncounterLabel 按 POI 类型给 poi_encounter 动作的中文文案。
func poiEncounterLabel(poi TilePOIInfo) string {
	if poi.Kind == string(MapPOIResource) {
		return fmt.Sprintf("🔍 探一探那处%s", poi.TypeCode)
	}
	switch poi.TypeCode {
	case "埋伏":
		return "⚔ 会一会那伙埋伏"
	case "行商":
		return "🪙 见见那位行商"
	case "求助":
		return "🤝 应一声求助"
	case "奇遇":
		return "✨ 赴一场奇遇"
	case "迷途":
		return "🧭 指点那位迷途人"
	default:
		return fmt.Sprintf("🔍 探一探那桩%s", poi.TypeCode)
	}
}

// defaultUpgradeTarget 返回第一件「材料够、可强化」的装备（口径同 buildEquipmentCandidates 的 upgrade 候选）。
func defaultUpgradeTarget(record unit.Record) (unit.ItemStack, bool) {
	for _, stack := range upgradeableEquipment(record) {
		if hasBackpackCosts(record, upgradeCosts(stack)) {
			return stack, true
		}
	}
	return unit.ItemStack{}, false
}
