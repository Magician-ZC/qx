package session

// 文件说明：地图 POI 遭遇玩家直驱结算的集成测试（对真实 SQLite，零 LLM——poi_encounter 全规则路径）。
// 夹具口径照搬 threat_test.go / mainworld_test.go：临时 SQLite（modernc 纯 Go，无需 CGO）+ 直接构造
// State / unit.Record 落库（测试文件在 statuslint 白名单内，可直改状态字段）。
// 覆盖：
//   - 资源 POI「探明」：只落叙事+记忆，不动钱包、不标 consumed（可重复探看）；无 POI 格报错。
//   - 埋伏：ResolveEliteEncounter 全链路跑通（胜利分赃经 Mutator 落钱包）、结算后 consumed + 防重放。
//   - 行商：幂等铺货返回货单、不标 consumed（可反复交易）；顺手验 PlayerTradeWithUnit 买一件成功扣钱加物。
//   - 求助：FNV 确定性分支裁决，钱包/饥饿/士气效果经 status.Mutator 留痕（events 表有对应 reason code 行）、consumed。
//   - 迷途：与野外 NPC 双向小幅正向结识（relations 表 trust/affection > 0）、consumed。
//   - 互斥：ExecutionInProgress=true 时返回哨兵 ErrExecutionBusy。
//   - 站位：距离 >1 报「她还没走到那里」。
// 野外 NPC 的事件类型由 npcEventTypeFor(sessionID, coord, unitID) 确定性派生——测试用「凑 unitID」法
// （fishPOIWildUnitID）逆向凑出想要的类型，全程确定性可复现。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/engine/turns"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// poiTestTurn 是夹具会话的固定回合数（poiEncounterRoll 拌 turn，测试侧复算分支时必须同值）。
const poiTestTurn = 3

// newPOIEncounterService 起一个临时 SQLite 上的完整 Service（含 sessions 仓库——
// ResolvePOIEncounter 经 guardPlayerAction 载入会话、结算后合并持久化，threat_test 的精简夹具不够用）。
func newPOIEncounterService(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "poi.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{
		db:                db,
		sessions:          NewRepository(db),
		units:             repo,
		mutator:           status.NewMutator(db, repo),
		memoryRefreshTurn: map[string]int{},
		memoryRecallTurn:  map[string]int{},
	}
	return db, service
}

// poiPlainsMap 造一张全平原小地图（平原不派生资源 POI，干净底盘；tiles 按 index=(R*Width)+Q 寻址）。
func poiPlainsMap(width, height int) world.MapSnapshot {
	tiles := make([]world.Tile, 0, width*height)
	for r := 0; r < height; r++ {
		for q := 0; q < width; q++ {
			tiles = append(tiles, world.Tile{Coord: world.Coord{Q: q, R: r}, Terrain: world.TerrainPlains})
		}
	}
	return world.MapSnapshot{ID: "map-poi-test", Seed: 7, Width: width, Height: height, Tiles: tiles}
}

// setPOITileTerrain 改某格地形（与 terrainAt 同一寻址口径）。
func setPOITileTerrain(snap *world.MapSnapshot, coord world.Coord, terrain world.TerrainID) {
	snap.Tiles[(coord.R*snap.Width)+coord.Q].Terrain = terrain
}

// newPOIState 造一个固定回合、进行中的主世界会话状态骨架。
func newPOIState(sessionID string, snap world.MapSnapshot) State {
	return State{
		ID:        sessionID,
		Mode:      "single_player",
		Outcome:   OutcomeOngoing,
		Map:       snap,
		TurnState: turns.State{Turn: poiTestTurn},
	}
}

// newPOIHero 造一个归玩家方的可战斗角色（固定 ID 保证确定性掷骰可复现）。
func newPOIHero(sessionID string, id string, coord world.Coord) unit.Record {
	hero := unit.BootstrapRecord(2, sessionID, "player", "阿岚")
	hero.ID = id
	hero.Status.PositionQ = coord.Q
	hero.Status.PositionR = coord.R
	return hero
}

// fishPOIWildUnitID 逆向凑出一个让 npcEventTypeFor 派生为 wantType 的野外 NPC unitID（确定性，5 类均摊几轮即中）。
func fishPOIWildUnitID(t *testing.T, sessionID string, coord world.Coord, wantType string, tag string) string {
	t.Helper()
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("wild-%s-%04d", tag, i)
		if npcEventTypeFor(sessionID, coord, id) == wantType {
			return id
		}
	}
	t.Fatalf("10000 次内凑不出事件类型 %q 的野外 NPC id（npcEventTypeFor 派生口径变了？）", wantType)
	return ""
}

// seedPOIWildNPC 落库一个站在 coord 的野外 NPC（id 已由 fishPOIWildUnitID 凑好）。
func seedPOIWildNPC(t *testing.T, ctx context.Context, service *Service, sessionID string, id string, coord world.Coord) unit.Record {
	t.Helper()
	npc := unit.BootstrapRecord(3, sessionID, "wild", "路人")
	npc.ID = id
	npc.Status.PositionQ = coord.Q
	npc.Status.PositionR = coord.R
	if err := service.units.Save(ctx, npc); err != nil {
		t.Fatalf("保存野外 NPC 失败: %v", err)
	}
	return npc
}

// assertPOIConsumedInDB 重新从库里取会话，断言某格（资源 POI 坐标键）的消耗标记与期望一致。
func assertPOIConsumedInDB(t *testing.T, ctx context.Context, service *Service, sessionID string, q, r int, want bool) {
	t.Helper()
	persisted, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("重读会话失败: %v", err)
	}
	if got := isPOIConsumed(&persisted, q, r); got != want {
		t.Fatalf("(%d,%d) 的消耗标记应为 %v，得到 %v（ConsumedPOIs=%v）", q, r, want, got, persisted.ConsumedPOIs)
	}
}

// assertNPCEventConsumedInDB 重新从库里取会话，断言某野外 NPC 事件（unitID 键，跟人走）的消耗标记与期望一致。
func assertNPCEventConsumedInDB(t *testing.T, ctx context.Context, service *Service, sessionID string, unitID string, want bool) {
	t.Helper()
	persisted, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("重读会话失败: %v", err)
	}
	if got := isNPCEventConsumed(&persisted, unitID); got != want {
		t.Fatalf("NPC %s 的事件消耗标记应为 %v，得到 %v（ConsumedPOIs=%v）", unitID, want, got, persisted.ConsumedPOIs)
	}
}

// countPOIEventRows 数 events 表里命中 (session, reason_code[, target_unit_id]) 的行数（Mutator/流程事件留痕口径）。
func countPOIEventRows(t *testing.T, db *sql.DB, sessionID string, reasonCode string, targetUnitID string) int {
	t.Helper()
	query := `SELECT COUNT(*) FROM events WHERE session_id = ? AND reason_code = ?`
	args := []any{sessionID, reasonCode}
	if targetUnitID != "" {
		query += ` AND target_unit_id = ?`
		args = append(args, targetUnitID)
	}
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("统计 events 失败: %v", err)
	}
	return n
}

// poiRelationAxes 读 relations 表某有向边的 trust/affection（不存在即 fatal——迷途结识应已写入）。
func poiRelationAxes(t *testing.T, db *sql.DB, sourceID string, targetID string) (float64, float64) {
	t.Helper()
	var trust, affection float64
	err := db.QueryRow(
		`SELECT trust, affection FROM relations WHERE source_unit_id = ? AND target_unit_id = ?`,
		sourceID, targetID,
	).Scan(&trust, &affection)
	if err != nil {
		t.Fatalf("读取关系 %s→%s 失败（迷途结识应已写 relations）: %v", sourceID, targetID, err)
	}
	return trust, affection
}

// TestPOIEncounter_ResourceScoutDoesNotConsume 验证资源 POI「探明」：
// 只落叙事+记忆，不动钱包、不标 consumed（防点按刷金币的取舍）；可重复探看；无 POI 格报错。
func TestPOIEncounter_ResourceScoutDoesNotConsume(t *testing.T) {
	_, service := newPOIEncounterService(t)
	ctx := context.Background()
	const sessionID = "poi-resource-s1"

	snap := poiPlainsMap(8, 8)
	// 找一个会派生资源 POI 的坐标并把它设为废墟（古迹遗物阈值 0.85 近乎必产，8×8 内必能找到）。
	var target world.Coord
	found := false
	for r := 0; r < snap.Height && !found; r++ {
		for q := 0; q < snap.Width && !found; q++ {
			coord := world.Coord{Q: q, R: r}
			if _, ok := resourcePOITypeAt(sessionID, world.TerrainRuins, coord); ok {
				target = coord
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("8×8 地图上找不到可派生资源 POI 的废墟坐标（resourcePOITypeAt 口径变了？）")
	}
	setPOITileTerrain(&snap, target, world.TerrainRuins)

	hero := newPOIHero(sessionID, "hero-poi-resource", target)
	walletBefore := hero.Status.Wallet
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	state := newPOIState(sessionID, snap)
	state.PlayerUnitIDs = []string{hero.ID}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	res, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, target.Q, target.R)
	if err != nil {
		t.Fatalf("资源探明出错: %v", err)
	}
	if !res.OK || res.Kind != poiKindResource || res.TypeCode != "古迹遗物" {
		t.Fatalf("应是资源 POI（古迹遗物）探明，得到 %+v", res)
	}
	if res.Consumed {
		t.Fatalf("资源探明不应标 consumed（消耗与收益留给采集路径），得到 %+v", res)
	}
	if res.SummaryZH == "" || !contains(res.SummaryZH, "古迹遗物") {
		t.Fatalf("叙事应含资源名「古迹遗物」: %q", res.SummaryZH)
	}

	// 不动钱包 + 落了一条记忆。
	reloaded, err := service.units.GetByID(ctx, hero.ID)
	if err != nil {
		t.Fatalf("重读角色失败: %v", err)
	}
	if reloaded.Status.Wallet != walletBefore {
		t.Fatalf("资源探明不应动钱包：期望 %d，得到 %d", walletBefore, reloaded.Status.Wallet)
	}
	if len(reloaded.Memory.Highlights) == 0 {
		t.Fatalf("资源探明应给她落一条记忆")
	}

	// 库里无消耗标记，二次探看仍可行（kind 不变）。
	assertPOIConsumedInDB(t, ctx, service, sessionID, target.Q, target.R, false)
	again, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, target.Q, target.R)
	if err != nil || !again.OK {
		t.Fatalf("资源 POI 不消耗，二次探看应仍可行: %v", err)
	}

	// 旁边的纯平原格无 POI，应报「这里并无可探看之事」。
	var neighbor world.Coord
	foundNeighbor := false
	for _, candidate := range axialNeighbors(target) {
		if inBounds(snap, candidate) {
			neighbor = candidate
			foundNeighbor = true
			break
		}
	}
	if !foundNeighbor {
		t.Fatalf("找不到在界内的相邻格")
	}
	if _, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, neighbor.Q, neighbor.R); err == nil || !contains(err.Error(), "并无可探看") {
		t.Fatalf("无 POI 格应报「这里并无可探看之事」，得到: %v", err)
	}
}

// TestPOIEncounter_AmbushRunsEliteEncounterAndConsumes 验证埋伏分支：
// ResolveEliteEncounter 全链路跑通（强角色讨平 scaledElite、分赃经 Mutator 落钱包 + ECONOMY_LOOT 留痕），
// 无论胜负结算后标 consumed，同格第二次触发被防重放闸拒绝。
func TestPOIEncounter_AmbushRunsEliteEncounterAndConsumes(t *testing.T) {
	db, service := newPOIEncounterService(t)
	ctx := context.Background()
	const sessionID = "poi-ambush-s1"

	snap := poiPlainsMap(8, 8)
	npcCoord := world.Coord{Q: 2, R: 2}
	npcID := fishPOIWildUnitID(t, sessionID, npcCoord, "埋伏", "ambush")

	hero := newPOIHero(sessionID, "hero-poi-ambush", npcCoord) // 同格触发（距离 0）
	hero.Status.Attack = 30                                    // 强角色：对 scaledElite（防 10、血 90）5 次命中即可讨平，30 回合内失败概率可忽略
	walletBefore := hero.Status.Wallet
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	seedPOIWildNPC(t, ctx, service, sessionID, npcID, npcCoord)

	state := newPOIState(sessionID, snap)
	state.PlayerUnitIDs = []string{hero.ID}
	state.WildUnitIDs = []string{npcID}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	res, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R)
	if err != nil {
		t.Fatalf("埋伏结算出错: %v", err)
	}
	if !res.OK || res.Kind != poiKindAmbush || res.TypeCode != "埋伏" {
		t.Fatalf("应是埋伏分支，得到 %+v", res)
	}
	if !res.Consumed {
		t.Fatalf("埋伏无论胜负都应标 consumed，得到 %+v", res)
	}
	if res.SummaryZH == "" {
		t.Fatalf("埋伏应有祖魂语气收件箱卡叙事")
	}
	if res.Outcome.EncounterOutcome != "defeated" {
		t.Fatalf("强角色应讨平埋伏精英，得到 %q（damage_taken=%d）", res.Outcome.EncounterOutcome, res.Outcome.DamageTaken)
	}
	wantAward := "金币×15" // scaledElite 固定掉落 gold 15，单人独得
	hasGold := false
	for _, award := range res.Outcome.Awards {
		if award == wantAward {
			hasGold = true
		}
	}
	if !hasGold {
		t.Fatalf("胜利分赃应含 %q，得到 %v", wantAward, res.Outcome.Awards)
	}

	// 分赃经 Mutator 真落钱包 + ECONOMY_LOOT 事件留痕；人活着。
	reloaded, err := service.units.GetByID(ctx, hero.ID)
	if err != nil {
		t.Fatalf("重读角色失败: %v", err)
	}
	if reloaded.Status.Wallet != walletBefore+15 {
		t.Fatalf("钱包应 +15=%d，得到 %d", walletBefore+15, reloaded.Status.Wallet)
	}
	if reloaded.Status.HP <= 0 {
		t.Fatalf("讨平埋伏后应活着，HP=%d", reloaded.Status.HP)
	}
	if countPOIEventRows(t, db, sessionID, "ECONOMY_LOOT", hero.ID) == 0 {
		t.Fatalf("分赃应有 ECONOMY_LOOT 事件留痕（经 Mutator）")
	}

	// 防重放：消耗标记已持久化（NPC 事件按 unitID 键，跟人走），同一伙第二次触发被拒。
	assertNPCEventConsumedInDB(t, ctx, service, sessionID, npcID, true)
	if _, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R); err == nil || !contains(err.Error(), "已被探看过") {
		t.Fatalf("同格第二次应报「这里已被探看过了」，得到: %v", err)
	}
}

// TestPOIEncounter_MerchantManifestAndTrade 验证行商分支：幂等铺货返回货单（≥3 件、含报价）、不标 consumed
// （可反复交易）；顺手验 PlayerTradeWithUnit 买一件成功——扣 单价×1、物入她行囊、行商货底真实扣减。
func TestPOIEncounter_MerchantManifestAndTrade(t *testing.T) {
	_, service := newPOIEncounterService(t)
	ctx := context.Background()
	const sessionID = "poi-merchant-s1"

	snap := poiPlainsMap(8, 8)
	npcCoord := world.Coord{Q: 3, R: 2}
	npcID := fishPOIWildUnitID(t, sessionID, npcCoord, "行商", "merchant")

	hero := newPOIHero(sessionID, "hero-poi-merchant", world.Coord{Q: 3, R: 1}) // 相邻格（距离 1）
	hero.Status.Wallet = 500                                                    // 钱备足，任一件货都买得起
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	seedPOIWildNPC(t, ctx, service, sessionID, npcID, npcCoord)

	state := newPOIState(sessionID, snap)
	state.PlayerUnitIDs = []string{hero.ID}
	state.WildUnitIDs = []string{npcID}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	res, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R)
	if err != nil {
		t.Fatalf("行商结算出错: %v", err)
	}
	if !res.OK || res.Kind != poiKindMerchant || res.MerchantUnitID != npcID {
		t.Fatalf("应是行商分支且回带行商 unitID，得到 %+v", res)
	}
	if res.Consumed {
		t.Fatalf("行商不应标 consumed（可反复交易），得到 %+v", res)
	}
	if len(res.MerchantGoods) < 3 {
		t.Fatalf("铺货后货单应 ≥3 件，得到 %d 件: %+v", len(res.MerchantGoods), res.MerchantGoods)
	}
	for _, good := range res.MerchantGoods {
		if good.BuyPrice <= 0 || good.SellPrice <= 0 || good.Quantity <= 0 {
			t.Fatalf("货单报价/数量应为正: %+v", good)
		}
	}
	assertPOIConsumedInDB(t, ctx, service, sessionID, npcCoord.Q, npcCoord.R, false)

	// 可反复触发（货单幂等：不重复铺货）。
	again, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R)
	if err != nil || !again.OK {
		t.Fatalf("行商不消耗，二次触发应仍可行: %v", err)
	}
	if len(again.MerchantGoods) != len(res.MerchantGoods) {
		t.Fatalf("二次触发不应重复铺货：首次 %d 件，二次 %d 件", len(res.MerchantGoods), len(again.MerchantGoods))
	}

	// 买一件：扣 单价×1、物入行囊、行商货底扣减。
	good := res.MerchantGoods[0]
	trade, err := service.PlayerTradeWithUnit(ctx, sessionID, hero.ID, PlayerTradeRequest{
		TargetUnitID: npcID,
		Mode:         "buy",
		ItemID:       good.ItemID,
		Quantity:     1,
	})
	if err != nil {
		t.Fatalf("向行商买入失败: %v", err)
	}
	if !trade.OK || trade.WalletAfter != 500-good.BuyPrice {
		t.Fatalf("买入后钱包应为 %d，得到 %+v", 500-good.BuyPrice, trade)
	}
	gotItem := false
	for _, it := range trade.Items {
		if it.ItemID == good.ItemID && it.Quantity >= 1 {
			gotItem = true
		}
	}
	if !gotItem {
		t.Fatalf("买入后她的行囊应有「%s」，得到 %+v", good.ItemID, trade.Items)
	}
	merchant, err := service.units.GetByID(ctx, npcID)
	if err != nil {
		t.Fatalf("重读行商失败: %v", err)
	}
	remaining := 0
	for _, stack := range merchant.Inventory.Backpack {
		if stack.ItemID == good.ItemID {
			remaining += stack.Quantity
		}
	}
	if remaining != good.Quantity-1 {
		t.Fatalf("行商货底应扣减 1（%d→%d），得到 %d", good.Quantity, good.Quantity-1, remaining)
	}
}

// TestPOIEncounter_HelpBranchAppliesEffectsViaMutator 验证求助分支：FNV 确定性分支裁决（测试侧用同口径复算
// 期望分支）、钱包/饥饿/士气效果经 status.Mutator 落库且 events 表有对应 reason code 行、结算后 consumed + 防重放。
func TestPOIEncounter_HelpBranchAppliesEffectsViaMutator(t *testing.T) {
	db, service := newPOIEncounterService(t)
	ctx := context.Background()
	const sessionID = "poi-help-s1"

	snap := poiPlainsMap(8, 8)
	npcCoord := world.Coord{Q: 1, R: 2}
	npcID := fishPOIWildUnitID(t, sessionID, npcCoord, "求助", "help")

	hero := newPOIHero(sessionID, "hero-poi-help", npcCoord)
	before := hero.Status // wallet=100 hunger=100 morale=0.7（BootstrapRecord 默认）
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	seedPOIWildNPC(t, ctx, service, sessionID, npcID, npcCoord)

	state := newPOIState(sessionID, snap)
	state.PlayerUnitIDs = []string{hero.ID}
	state.WildUnitIDs = []string{npcID}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	res, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R)
	if err != nil {
		t.Fatalf("求助结算出错: %v", err)
	}
	if !res.OK || res.Kind != poiKindHelp || !res.Consumed {
		t.Fatalf("应是求助分支且结算后 consumed，得到 %+v", res)
	}

	// 同口径复算期望分支（poiEncounterRoll 只依赖 sessionID/坐标/turn，全确定性）。
	rollState := State{ID: sessionID, TurnState: turns.State{Turn: poiTestTurn}}
	specs := helpBranchSpecs()
	spec := specs[pickByRoll(poiEncounterRoll(rollState, npcCoord.Q, npcCoord.R, "branch:"+poiKindHelp), len(specs))]
	expectedWallet := before.Wallet + spec.branch.WalletDelta
	if expectedWallet < 0 {
		expectedWallet = 0
	}
	expectedHunger := clampInt(before.Hunger+spec.branch.HungerDelta, 0, 100)
	expectedMorale := clampFloat(before.Morale+spec.branch.MoraleDelta, 0.05, 1)

	reloaded, err := service.units.GetByID(ctx, hero.ID)
	if err != nil {
		t.Fatalf("重读角色失败: %v", err)
	}
	if reloaded.Status.Wallet != expectedWallet {
		t.Fatalf("分支[%s]钱包应为 %d，得到 %d", spec.branch.Label, expectedWallet, reloaded.Status.Wallet)
	}
	if reloaded.Status.Hunger != expectedHunger {
		t.Fatalf("分支[%s]饥饿应为 %d，得到 %d", spec.branch.Label, expectedHunger, reloaded.Status.Hunger)
	}
	if math.Abs(reloaded.Status.Morale-expectedMorale) > 1e-6 {
		t.Fatalf("分支[%s]士气应≈%v，得到 %v", spec.branch.Label, expectedMorale, reloaded.Status.Morale)
	}
	if res.Outcome.WalletDelta != expectedWallet-before.Wallet || res.Outcome.HungerDelta != expectedHunger-before.Hunger {
		t.Fatalf("返回的增减明细与期望分支不符：%+v（期望 wallet%+d hunger%+d）",
			res.Outcome, expectedWallet-before.Wallet, expectedHunger-before.Hunger)
	}

	// 受保护/钱包字段恒经 Mutator：每个变化字段在 events 表都应有对应 reason code 行（三分支都至少动士气）。
	if expectedMorale != before.Morale {
		code := "EMOTION_REWARD"
		if expectedMorale < before.Morale {
			code = "EMOTION_TRAUMA"
		}
		if countPOIEventRows(t, db, sessionID, code, hero.ID) == 0 {
			t.Fatalf("士气变化应有 %s 事件留痕（经 Mutator）", code)
		}
	}
	if expectedWallet != before.Wallet {
		code := "ECONOMY_REWARD"
		if expectedWallet < before.Wallet {
			code = "ECONOMY_PURCHASE"
		}
		if countPOIEventRows(t, db, sessionID, code, hero.ID) == 0 {
			t.Fatalf("钱包变化应有 %s 事件留痕（经 Mutator）", code)
		}
	}
	if expectedHunger != before.Hunger && countPOIEventRows(t, db, sessionID, "SURVIVAL_HUNGER", hero.ID) == 0 {
		t.Fatalf("饥饿变化应有 SURVIVAL_HUNGER 事件留痕（经 Mutator）")
	}
	// 整次结算的流程事件留痕（POI_ENCOUNTER_RESOLVED，best-effort 但本路径应成功）。
	if countPOIEventRows(t, db, sessionID, "POI_ENCOUNTER_RESOLVED", "") == 0 {
		t.Fatalf("应有 POI_ENCOUNTER_RESOLVED 流程事件留痕")
	}

	// 防重放（NPC 事件按 unitID 键，跟人走）。
	assertNPCEventConsumedInDB(t, ctx, service, sessionID, npcID, true)
	if _, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R); err == nil || !contains(err.Error(), "已被探看过") {
		t.Fatalf("同格第二次应报「这里已被探看过了」，得到: %v", err)
	}
}

// TestPOIEncounter_LostBefriendsStranger 验证迷途分支：她与野外 NPC 双向小幅结识
// （relations 表两个方向 trust/affection 均 > 0）、返回关系变化说明、结算后 consumed。
func TestPOIEncounter_LostBefriendsStranger(t *testing.T) {
	db, service := newPOIEncounterService(t)
	ctx := context.Background()
	const sessionID = "poi-lost-s1"

	snap := poiPlainsMap(8, 8)
	npcCoord := world.Coord{Q: 2, R: 3}
	npcID := fishPOIWildUnitID(t, sessionID, npcCoord, "迷途", "lost")

	hero := newPOIHero(sessionID, "hero-poi-lost", npcCoord)
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	seedPOIWildNPC(t, ctx, service, sessionID, npcID, npcCoord)

	state := newPOIState(sessionID, snap)
	state.PlayerUnitIDs = []string{hero.ID}
	state.WildUnitIDs = []string{npcID}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	res, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R)
	if err != nil {
		t.Fatalf("迷途结算出错: %v", err)
	}
	if !res.OK || res.Kind != poiKindLost || !res.Consumed {
		t.Fatalf("应是迷途分支且结算后 consumed，得到 %+v", res)
	}
	if res.Outcome.RelationZH == "" || !contains(res.Outcome.RelationZH, "结识") {
		t.Fatalf("应返回结识的关系变化说明: %q", res.Outcome.RelationZH)
	}

	// 双向正向：trust/affection 各 +1~2，clamp 在 relation.go 内。
	for _, pair := range [][2]string{{hero.ID, npcID}, {npcID, hero.ID}} {
		trust, affection := poiRelationAxes(t, db, pair[0], pair[1])
		if trust <= 0 || affection <= 0 {
			t.Fatalf("迷途结识 %s→%s 应 trust/affection 均为正，得到 trust=%v affection=%v", pair[0], pair[1], trust, affection)
		}
	}

	assertNPCEventConsumedInDB(t, ctx, service, sessionID, npcID, true)
}

// TestPOIEncounter_ExecutionBusyMutex 验证执行互斥：ExecutionInProgress=true 时返回哨兵 ErrExecutionBusy
// （router 据 errors.Is 映射 409），不做任何结算。
func TestPOIEncounter_ExecutionBusyMutex(t *testing.T) {
	_, service := newPOIEncounterService(t)
	ctx := context.Background()
	const sessionID = "poi-busy-s1"

	snap := poiPlainsMap(8, 8)
	hero := newPOIHero(sessionID, "hero-poi-busy", world.Coord{Q: 0, R: 0})
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	state := newPOIState(sessionID, snap)
	state.PlayerUnitIDs = []string{hero.ID}
	state.ExecutionInProgress = true
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	_, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, 0, 0)
	if !errors.Is(err, ErrExecutionBusy) {
		t.Fatalf("执行中应返回哨兵 ErrExecutionBusy，得到: %v", err)
	}
}

// TestPOIEncounter_TooFarRejected 验证站位限距：她与目标格距离 >1 时报「她还没走到那里」，不做任何结算。
func TestPOIEncounter_TooFarRejected(t *testing.T) {
	_, service := newPOIEncounterService(t)
	ctx := context.Background()
	const sessionID = "poi-far-s1"

	snap := poiPlainsMap(8, 8)
	npcCoord := world.Coord{Q: 5, R: 5}
	npcID := fishPOIWildUnitID(t, sessionID, npcCoord, "迷途", "far")

	hero := newPOIHero(sessionID, "hero-poi-far", world.Coord{Q: 0, R: 0}) // 距 (5,5) 远超 1 格
	if err := service.units.Save(ctx, hero); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	seedPOIWildNPC(t, ctx, service, sessionID, npcID, npcCoord)

	state := newPOIState(sessionID, snap)
	state.PlayerUnitIDs = []string{hero.ID}
	state.WildUnitIDs = []string{npcID}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("保存会话失败: %v", err)
	}

	_, err := service.ResolvePOIEncounter(ctx, sessionID, hero.ID, npcCoord.Q, npcCoord.R)
	if err == nil || !contains(err.Error(), "她还没走到那里") {
		t.Fatalf("距离 >1 应报「她还没走到那里」，得到: %v", err)
	}
	assertPOIConsumedInDB(t, ctx, service, sessionID, npcCoord.Q, npcCoord.R, false)
}
