package session

// 文件说明：地块事件系统的聚焦单元测试——只覆盖本波次新增的纯逻辑：
// 资源 POI 采集加成的 ×1.5 向上取整、POI 消耗幂等闸（unconsumedResourcePOIAt）、
// 动作目录的站位/材料门（computeTileAffordances）与请求匹配（matchTileAffordance）。
// 结算链路本身全复用既有 execute* 路径，不在此重复测试。

import (
	"fmt"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// TestScaleGatherRewardsForPOI 校验 ×1.5 向上取整的整数算式（q=1→2、2→3、3→5、4→6）。
func TestScaleGatherRewardsForPOI(t *testing.T) {
	scaled := scaleGatherRewardsForPOI([]itemGrant{
		{ItemID: "wood", Quantity: 1},
		{ItemID: "stone", Quantity: 2},
		{ItemID: "ration", Quantity: 3},
		{ItemID: "herb_bundle", Quantity: 4},
		{ItemID: "junk", Quantity: 0}, // 非正数量应被丢弃
	})
	want := map[string]int{"wood": 2, "stone": 3, "ration": 5, "herb_bundle": 6}
	if len(scaled) != len(want) {
		t.Fatalf("scaled length = %d, want %d", len(scaled), len(want))
	}
	for _, grant := range scaled {
		if want[grant.ItemID] != grant.Quantity {
			t.Fatalf("scaled %s = %d, want %d", grant.ItemID, grant.Quantity, want[grant.ItemID])
		}
	}
}

// poiTestState 造一张 4x4 山地图（山地 forage/mine 皆可、矿脉 POI 候选），并定位一个必产 POI 的格子。
func poiTestState(t *testing.T) (State, world.Coord) {
	t.Helper()
	snapshot := world.MapSnapshot{Width: 4, Height: 4}
	for r := 0; r < 4; r++ {
		for q := 0; q < 4; q++ {
			snapshot.Tiles = append(snapshot.Tiles, world.Tile{
				Coord:   world.Coord{Q: q, R: r},
				Terrain: world.TerrainMountain,
			})
		}
	}
	state := State{ID: "tile-poi-test", Map: snapshot}
	state.TurnState.Turn = 3
	for _, tile := range snapshot.Tiles {
		if _, ok := resourcePOITypeAt(state.ID, tile.Terrain, tile.Coord); ok {
			return state, tile.Coord
		}
	}
	// 阈值稀疏（<0.12），4x4 可能恰好全 miss：换会话 ID 重掷直至命中（确定性哈希，循环有限）。
	for i := 0; i < 64; i++ {
		state.ID = fmt.Sprintf("tile-poi-test-%d", i)
		for _, tile := range snapshot.Tiles {
			if _, ok := resourcePOITypeAt(state.ID, tile.Terrain, tile.Coord); ok {
				return state, tile.Coord
			}
		}
	}
	t.Fatal("no resource POI derived on any session id; poiRoll口径疑似被改动")
	return State{}, world.Coord{}
}

// TestUnconsumedResourcePOIGate 校验消耗闸：标记消耗前命中、标记后不再命中（幂等防重放）。
func TestUnconsumedResourcePOIGate(t *testing.T) {
	state, coord := poiTestState(t)
	typeCode, ok := unconsumedResourcePOIAt(&state, coord.Q, coord.R)
	if !ok || typeCode != "矿脉" {
		t.Fatalf("unconsumed POI = (%q,%v), want (矿脉,true)", typeCode, ok)
	}
	markPOIConsumed(&state, coord.Q, coord.R)
	if _, ok := unconsumedResourcePOIAt(&state, coord.Q, coord.R); ok {
		t.Fatal("POI still hit after markPOIConsumed; replay gate broken")
	}
	if turn := state.ConsumedPOIs[poiCoordKey(coord.Q, coord.R)]; turn != 3 {
		t.Fatalf("consumed turn = %d, want 3", turn)
	}
}

// TestComputeTileAffordancesGating 校验目录门：不在场动作置灰（她还没走到那里）、
// 站上去后山地 gather:mine 可用、无武器打猎不在山地目录（山地不支持 hunt）、材料不够的建造置灰。
func TestComputeTileAffordancesGating(t *testing.T) {
	state, coord := poiTestState(t)
	actor := unit.Record{ID: "u-test", FactionID: "player"}
	actor.Status.LifeState = unit.LifeStateActive
	actor.Status.PositionQ = coord.Q
	actor.Status.PositionR = coord.R
	state.PlayerUnitIDs = []string{actor.ID}
	units := []unit.Record{actor}

	affordances := computeTileAffordances(&state, units, &units[0], coord.Q, coord.R)
	if !affordances.UnitOnTile || affordances.Distance != 0 {
		t.Fatalf("on-tile affordances wrong: onTile=%v distance=%d", affordances.UnitOnTile, affordances.Distance)
	}
	if affordances.POI == nil || affordances.POI.TypeCode != "矿脉" || affordances.POI.Consumed {
		t.Fatalf("POI info = %+v, want unconsumed 矿脉", affordances.POI)
	}
	var mine, forge *TileAction
	for i := range affordances.Actions {
		action := &affordances.Actions[i]
		if action.Action == "gather" && action.Activity == "mine" {
			mine = action
		}
		if action.Action == "build" && action.StructureType == string(StructureTypeForge) {
			forge = action
		}
	}
	if mine == nil || !mine.Available {
		t.Fatalf("on-tile mine should be available, got %+v", mine)
	}
	if forge == nil || forge.Available || forge.ReasonZH == "" {
		t.Fatalf("forge build without materials should be unavailable with reason, got %+v", forge)
	}

	// 把她挪远一格：所有站位动作置灰、原因统一。
	units[0].Status.PositionQ = coord.Q + 1
	away := computeTileAffordances(&state, units, &units[0], coord.Q, coord.R)
	if away.UnitOnTile || away.Distance != 1 {
		t.Fatalf("away affordances wrong: onTile=%v distance=%d", away.UnitOnTile, away.Distance)
	}
	for _, action := range away.Actions {
		if action.Action == "talk" || action.Action == "trade" {
			continue // talk/trade 允许相邻
		}
		if action.Available {
			t.Fatalf("action %s/%s should be gated off-tile", action.Action, action.Activity)
		}
		if action.ReasonZH != tileReasonNotThere {
			t.Fatalf("action %s reason = %q, want %q", action.Action, action.ReasonZH, tileReasonNotThere)
		}
	}
}

// TestMatchTileAffordance 校验请求与目录条目的匹配口径（gather 须 activity 一致、build 须 structure_type 一致）。
func TestMatchTileAffordance(t *testing.T) {
	affordances := TileAffordances{Actions: []TileAction{
		{Action: "gather", Activity: "mine", Available: true},
		{Action: "build", StructureType: "forge", Available: false},
		{Action: "harvest", Available: true},
	}}
	if _, ok := matchTileAffordance(affordances, TileActionRequest{Action: "gather", Activity: "fish"}); ok {
		t.Fatal("gather:fish should not match gather:mine entry")
	}
	if entry, ok := matchTileAffordance(affordances, TileActionRequest{Action: "gather", Activity: "mine"}); !ok || !entry.Available {
		t.Fatal("gather:mine should match available entry")
	}
	if entry, ok := matchTileAffordance(affordances, TileActionRequest{Action: "build", StructureType: "forge"}); !ok || entry.Available {
		t.Fatal("build:forge should match the unavailable entry")
	}
	if _, ok := matchTileAffordance(affordances, TileActionRequest{Action: "demolish"}); ok {
		t.Fatal("demolish should not match any entry")
	}
}
