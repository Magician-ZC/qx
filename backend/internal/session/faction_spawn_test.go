package session

// 文件说明：命运地图舞台——出生点公共阵营 NPC「上图」的集成测试（对真实 SQLite）。
// 覆盖：
//   - NPC 散布有合法坐标、不全叠在 (0,0)、不叠到主角锚点 (1,3) 上、彼此不叠放。
//   - NPC 进 AmbientUnits 快照（前端据此画上命运地图），且**绝不进执行 order**（不自治、零 LLM）。
//   - 轻量游走：QUNXIANG_AMBIENT_WANDER 关时 NPC 静态站着、开时确定性微动（同输入同落点、合法不叠放）。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/world"
)

// seedSpawnSessionWithMap 起一局带 9x7 地图（与命运降生同口径）的 session、落库，并播种公共阵营 NPC，
// 把 NPC IDs 收进 state.AmbientUnitIDs 再保存。返回 sessionID + 落库 NPC IDs，供各测试断言坐标/快照/游走。
func seedSpawnSessionWithMap(t *testing.T, service *Service) (string, []string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	const sessionID = "sess-ambient-stage"
	const seed = int64(20260609)

	state := State{
		ID:              sessionID,
		Mode:            ModeSinglePlayer,
		RandomSeed:      seed,
		PlayerFactionID: "player",
		EnemyFactionID:  "enemy",
		SetupPhase:      SetupPhaseReady,
		TurnState:       turns.NewState(now, turns.DefaultBudgets()),
		Outcome:         OutcomeOngoing,
		Map:             generateBattlefieldWithSize(sessionID, seed, normalizeBattlefieldScriptID("", seed), BattlefieldSizeSmall),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("save session: %v", err)
	}

	npcIDs := service.SeedFactionSpawnBestEffort(ctx, sessionID, faction.IDFreedom, "wildlands", seed+1, world.MapSnapshot{})
	if len(npcIDs) < 8 || len(npcIDs) > 12 {
		t.Fatalf("应播种 8–12 个公共 NPC，得到 %d", len(npcIDs))
	}
	// 把 NPC 收进 AmbientUnitIDs（仿 mainworld.go 降生入口），再持久化。
	reloaded, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	reloaded.AmbientUnitIDs = append(reloaded.AmbientUnitIDs, npcIDs...)
	if err := service.sessions.Save(ctx, &reloaded); err != nil {
		t.Fatalf("save ambient ids: %v", err)
	}
	return sessionID, npcIDs
}

// TestFactionSpawn_NPCsHaveLegalDistinctCoords 验证 NPC 散布的坐标合法性：
// 都在 9x7 地图内、不全叠 (0,0)、不叠主角锚点 (1,3)、彼此坐标互不重复（避叠放）。
func TestFactionSpawn_NPCsHaveLegalDistinctCoords(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()
	sessionID, npcIDs := seedSpawnSessionWithMap(t, service)

	state, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	snapshot := state.Map

	npcSet := map[string]struct{}{}
	for _, id := range npcIDs {
		npcSet[id] = struct{}{}
	}
	records, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}

	seen := map[string]int{}     // coordString -> 计数（检测叠放）
	nonOriginCount := 0          // 非 (0,0) 的 NPC 数
	anchor := factionSpawnAnchor // (1,3)
	checked := 0
	for i := range records {
		rec := records[i]
		if _, ok := npcSet[rec.ID]; !ok {
			continue
		}
		checked++
		coord := world.Coord{Q: rec.Status.PositionQ, R: rec.Status.PositionR}
		if !inBounds(snapshot, coord) {
			t.Fatalf("NPC %s 坐标越界：%+v（地图 %dx%d）", rec.ID, coord, snapshot.Width, snapshot.Height)
		}
		if coord == anchor {
			t.Fatalf("NPC %s 不应叠在主角锚点 %+v 上", rec.ID, anchor)
		}
		if coord.Q != 0 || coord.R != 0 {
			nonOriginCount++
		}
		seen[coordString(coord)]++
	}
	if checked == 0 {
		t.Fatalf("未找到落库的公共 NPC")
	}
	// 不全叠在 (0,0)：9x7 地图围绕 (1,3) 散布，绝大多数应有非原点坐标。
	if nonOriginCount == 0 {
		t.Fatalf("NPC 全叠在 (0,0)：坐标散布未生效")
	}
	// 互不叠放：每个被占坐标至多 1 个 NPC。
	for coord, n := range seen {
		if n > 1 {
			t.Fatalf("坐标 %s 上叠了 %d 个 NPC（应避叠放）", coord, n)
		}
	}
}

// TestFactionSpawn_AmbientUnitsInSnapshotNotInExecutionOrder 验证契约：
//   - NPC 进 AmbientUnits 快照（前端可见）。
//   - NPC **绝不进** buildExecutionOrderByATB（不自治、零 LLM；否则每拍 +8~12 次 LLM 成本爆炸）。
func TestFactionSpawn_AmbientUnitsInSnapshotNotInExecutionOrder(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()
	sessionID, npcIDs := seedSpawnSessionWithMap(t, service)

	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}

	// 快照：AmbientUnits 应含全部 NPC。
	snapshot := buildSnapshot(state, units)
	if len(snapshot.AmbientUnits) != len(npcIDs) {
		t.Fatalf("AmbientUnits 应含 %d 个 NPC，快照里只有 %d 个", len(npcIDs), len(snapshot.AmbientUnits))
	}
	ambientInSnapshot := map[string]struct{}{}
	for _, rec := range snapshot.AmbientUnits {
		ambientInSnapshot[rec.ID] = struct{}{}
	}
	for _, id := range npcIDs {
		if _, ok := ambientInSnapshot[id]; !ok {
			t.Fatalf("NPC %s 未进 AmbientUnits 快照", id)
		}
	}

	// 执行 order：AmbientUnitIDs 绝不进 buildExecutionOrderByATB（仅取 Player/Enemy/Wild）。
	byID := mapRecordsByID(units)
	order, _ := buildExecutionOrderByATB(state, byID)
	npcSet := map[string]struct{}{}
	for _, id := range npcIDs {
		npcSet[id] = struct{}{}
	}
	for _, id := range order {
		if _, isNPC := npcSet[id]; isNPC {
			t.Fatalf("公共 NPC %s 不应进执行 order（不自治、零 LLM），但出现在 %v 中", id, order)
		}
	}
}

// TestAmbientWander_FlagOffStatic 验证 QUNXIANG_AMBIENT_WANDER 关时 NPC 静态站着（坐标不变）。
func TestAmbientWander_FlagOffStatic(t *testing.T) {
	t.Setenv("QUNXIANG_AMBIENT_WANDER", "") // 显式关（默认即关，防外部环境干扰）
	_, service := newMainWorldTestService(t)
	ctx := context.Background()
	sessionID, npcIDs := seedSpawnSessionWithMap(t, service)

	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	before := coordsByID(t, service, sessionID, npcIDs)

	// 推进若干个回合边界——flag 关时 wander 应 no-op。
	for turn := 1; turn <= 8; turn++ {
		state.TurnState.Turn = turn
		service.wanderAmbientUnits(ctx, &state, units)
	}

	after := coordsByID(t, service, sessionID, npcIDs)
	for _, id := range npcIDs {
		if before[id] != after[id] {
			t.Fatalf("flag 关时 NPC %s 应静态不动：before=%s after=%s", id, before[id], after[id])
		}
	}
}

// TestAmbientWander_FlagOnDeterministicMovement 验证 flag 开时：
//   - NPC 会发生（至少一个）确定性微动（坐标变化）。
//   - 同一 (session, turn) 输入重复跑落点完全一致（确定性、可复现）。
//   - 落点恒合法（地图内、互不叠放）。
func TestAmbientWander_FlagOnDeterministicMovement(t *testing.T) {
	t.Setenv("QUNXIANG_AMBIENT_WANDER", "1")
	_, service := newMainWorldTestService(t)
	ctx := context.Background()
	sessionID, npcIDs := seedSpawnSessionWithMap(t, service)

	state, units, err := service.loadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	snapshot := state.Map
	before := coordsByID(t, service, sessionID, npcIDs)

	// 单拍游走：固定 turn，跑两次（第二次先回滚坐标），落点应完全一致。
	const probeTurn = 5
	state.TurnState.Turn = probeTurn
	service.wanderAmbientUnits(ctx, &state, units)
	afterFirst := coordsByID(t, service, sessionID, npcIDs)

	// 至少一个 NPC 挪动了（30% 概率 × 8~12 个 NPC，几乎必然有人动；若极端全不动也算合法，故只软性检查）。
	moved := 0
	occupied := map[string]int{}
	for _, id := range npcIDs {
		if before[id] != afterFirst[id] {
			moved++
		}
		occupied[afterFirst[id]]++
	}
	if moved == 0 {
		t.Fatalf("flag 开时应有 NPC 发生微动（确定性 30%% × %d 个 NPC），但无人移动", len(npcIDs))
	}
	// 落点合法性：互不叠放。
	for coord, n := range occupied {
		if n > 1 {
			t.Fatalf("游走后坐标 %s 叠了 %d 个 NPC（应避叠放）", coord, n)
		}
	}
	// 落点在界内。
	records := coordRecordsByID(t, service, sessionID, npcIDs)
	for id, coord := range records {
		if !inBounds(snapshot, coord) {
			t.Fatalf("游走后 NPC %s 越界：%+v", id, coord)
		}
	}

	// 确定性复现：把坐标回滚到 afterFirst 的「起点」(before)，同 turn 再跑一次，落点应与首次一致。
	resetCoords(t, service, sessionID, before)
	state2, units2, err := service.loadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	state2.TurnState.Turn = probeTurn
	service.wanderAmbientUnits(ctx, &state2, units2)
	afterSecond := coordsByID(t, service, sessionID, npcIDs)
	for _, id := range npcIDs {
		if afterFirst[id] != afterSecond[id] {
			t.Fatalf("游走应确定性：NPC %s 两次落点不同 %s != %s", id, afterFirst[id], afterSecond[id])
		}
	}
}

// coordsByID 读回一批 NPC 的当前坐标字符串（coordString），供前后比对。
func coordsByID(t *testing.T, service *Service, sessionID string, ids []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for id, coord := range coordRecordsByID(t, service, sessionID, ids) {
		out[id] = coordString(coord)
	}
	return out
}

// coordRecordsByID 读回一批 NPC 的当前坐标（world.Coord）。
func coordRecordsByID(t *testing.T, service *Service, sessionID string, ids []string) map[string]world.Coord {
	t.Helper()
	ctx := context.Background()
	records, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}
	want := map[string]struct{}{}
	for _, id := range ids {
		want[id] = struct{}{}
	}
	out := map[string]world.Coord{}
	for i := range records {
		if _, ok := want[records[i].ID]; !ok {
			continue
		}
		out[records[i].ID] = world.Coord{Q: records[i].Status.PositionQ, R: records[i].Status.PositionR}
	}
	return out
}

// resetCoords 把一批 NPC 坐标硬写回给定的 coordString 值（用于确定性复现：回到起点再跑一次）。
func resetCoords(t *testing.T, service *Service, sessionID string, coords map[string]string) {
	t.Helper()
	ctx := context.Background()
	records, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("list units: %v", err)
	}
	for i := range records {
		want, ok := coords[records[i].ID]
		if !ok {
			continue
		}
		q, r := parseCoordString(t, want)
		records[i].Status.PositionQ = q
		records[i].Status.PositionR = r
		if err := service.units.Save(ctx, records[i]); err != nil {
			t.Fatalf("reset save %s: %v", records[i].ID, err)
		}
	}
}

// parseCoordString 把 "q:r" 解析回整数坐标（与 coordString 互逆）。
func parseCoordString(t *testing.T, s string) (int, int) {
	t.Helper()
	var q, r int
	if _, err := fmt.Sscanf(s, "%d:%d", &q, &r); err != nil {
		t.Fatalf("解析坐标串 %q 失败: %v", s, err)
	}
	return q, r
}
