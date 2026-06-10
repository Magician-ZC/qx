// 文件说明：POI 消耗态 helper——地图兴趣点本体由 map_pois.go 哈希确定性派生（不落库），
// 这里只管「已被采完/探完」的消耗标记（State.ConsumedPOIs，"q,r" → 消耗时回合数）。
// 下发侧（MapPOIs）据此标 consumed 供前端徽标变淡；结算侧（tile_actions/poi_encounter）据此做幂等防重放闸。

package session

import "fmt"

// poiCoordKey 把六边形坐标编成 ConsumedPOIs 的键（"q,r"）。
func poiCoordKey(q, r int) string {
	return fmt.Sprintf("%d,%d", q, r)
}

// unitInIDList 判断某单位 ID 是否在给定 ID 列表里（PlayerUnitIDs/EnemyUnitIDs/WildUnitIDs 通用）。
func unitInIDList(ids []string, unitID string) bool {
	for _, id := range ids {
		if id == unitID {
			return true
		}
	}
	return false
}

// isPOIConsumed 查询某格 POI 是否已被消耗（采完/探完）。
func isPOIConsumed(state *State, q, r int) bool {
	if state == nil || state.ConsumedPOIs == nil {
		return false
	}
	_, ok := state.ConsumedPOIs[poiCoordKey(q, r)]
	return ok
}

// markPOIConsumed 把某格 POI 标记为已消耗（记录消耗时回合数）。map 惰性初始化以兼容旧存档。
func markPOIConsumed(state *State, q, r int) {
	if state == nil {
		return
	}
	if state.ConsumedPOIs == nil {
		state.ConsumedPOIs = make(map[string]int)
	}
	state.ConsumedPOIs[poiCoordKey(q, r)] = state.TurnState.Turn
}

// npcEventKey 把野外 NPC 事件 POI 编成消耗键（"npc:"+unitID）。与资源 POI 的坐标键分开两层考虑：
// ① NPC 会随执行循环游走，按坐标键标记会让它每挪一格就「重新长出」一个可刷的事件（埋伏反复刷战利品）；
// ② 同格既有 NPC 事件又有地块资源时，坐标键会把两类 POI 合并消耗（探完人就再也采不出加成）。
func npcEventKey(unitID string) string {
	return "npc:" + unitID
}

// isNPCEventConsumed 查询某野外 NPC 身上的事件是否已被触发过（跟人走，不跟格子走）。
func isNPCEventConsumed(state *State, unitID string) bool {
	if state == nil || state.ConsumedPOIs == nil || unitID == "" {
		return false
	}
	_, ok := state.ConsumedPOIs[npcEventKey(unitID)]
	return ok
}

// markNPCEventConsumed 把某野外 NPC 身上的事件标记为已触发（记录触发时回合数）。
func markNPCEventConsumed(state *State, unitID string) {
	if state == nil || unitID == "" {
		return
	}
	if state.ConsumedPOIs == nil {
		state.ConsumedPOIs = make(map[string]int)
	}
	state.ConsumedPOIs[npcEventKey(unitID)] = state.TurnState.Turn
}
