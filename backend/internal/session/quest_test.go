package session

// 文件说明：分区大世界阶段3 · 任务系统集成测试（设计 docs/分区大世界设计方案-2026-06-10.md §5）。
// 覆盖端到端：可接任务列举（fallback 文案，LLM=nil）→ 接取 → 进度结算 hook → 完成 → 交付发奖 → 解锁 portal 传送。
// 复用 mainworld_test 的临时 SQLite 夹具（service.llm=nil → 剧情走确定性 fallback，机制护栏锁定，可断言）。

import (
	"context"
	"strings"
	"testing"

	"qunxiang/backend/internal/world"
)

// TestQuestLifecycle_AcceptProgressTurnInUnlock 端到端验证：接主城讨伐任务→讨平 boss 进度→完成→交付→解锁同阵营野外区传送。
func TestQuestLifecycle_AcceptProgressTurnInUnlock(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-quest", MainWorldCharacterInput{
		Name: "任务娘", Origin: "边境游侠", Desire: "扬名立万",
	})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	sessionID, unitID := view.SessionID, view.UnitID

	// 1) travel 到晨曦城郊（border 可达的主城区）——主城区才有讨伐+解锁野外的任务。
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_freedom_capital", ""); err != nil {
		t.Fatalf("travel 到主城应成功: %v", err)
	}

	// 2) 可接任务：主城区应有一桩 slay 任务（UnlockZone=同阵营 wild 区）。LLM=nil → Title 走 fallback（非空）。
	avail, err := service.AvailableQuests(ctx, sessionID, unitID, "")
	if err != nil {
		t.Fatalf("列可接任务失败: %v", err)
	}
	var slay Quest
	var found bool
	for _, q := range avail {
		if q.Type == QuestTypeSlay {
			slay, found = q, true
		}
		if q.Title == "" || q.NarrativeZH == "" {
			t.Errorf("任务 %s 的 Title/NarrativeZH 不应为空（fallback 应兜底）：%+v", q.ID, q)
		}
		if q.State != QuestStateAvailable {
			t.Errorf("可接任务状态应为 available，得到 %q", q.State)
		}
	}
	if !found {
		t.Fatalf("主城区应有讨伐任务，得到 %d 桩：%+v", len(avail), avail)
	}
	if slay.Rewards.UnlockZone != "zone_freedom_wild" {
		t.Fatalf("晨曦城郊讨伐任务应解锁同阵营野外区，得到 UnlockZone=%q", slay.Rewards.UnlockZone)
	}
	if len(slay.Objectives) != 1 || slay.Objectives[0].Kind != ObjectiveDefeatBoss || slay.Objectives[0].Target != "zone_freedom_capital" {
		t.Fatalf("讨伐任务目标应为击败本区 boss，得到 %+v", slay.Objectives)
	}

	// 3) 接取：加入 ActiveQuests，状态 active。
	accepted, err := service.AcceptQuest(ctx, sessionID, unitID, slay.ID)
	if err != nil {
		t.Fatalf("接取任务失败: %v", err)
	}
	if accepted.State != QuestStateActive {
		t.Fatalf("接取后状态应为 active，得到 %q", accepted.State)
	}
	// 确定性 id：accept 重生成的 quest id 与列出的一致。
	if accepted.ID != slay.ID {
		t.Fatalf("接取任务 id 应与可接任务一致（确定性）：%q vs %q", accepted.ID, slay.ID)
	}

	// 进行中列表应含它，且尚未完成。
	active, err := service.ListActiveQuests(ctx, sessionID, unitID)
	if err != nil {
		t.Fatalf("列进行中任务失败: %v", err)
	}
	if len(active) != 1 || active[0].State != QuestStateActive {
		t.Fatalf("进行中应有 1 桩 active，得到 %+v", active)
	}

	// 4) 未完成时交付应被拒（防空交付刷奖）。
	if _, err := service.TurnInQuest(ctx, sessionID, unitID, slay.ID); err == nil {
		t.Fatal("目标未达成时交付应被拒，却成功了")
	}

	// 5) 进度结算 hook（模拟 ChallengeZoneBoss 胜利路径内部所做的 advanceQuestObjectives）：
	//    讨平本区 boss → defeat_boss 目标达成 → quest 转 completed。直接走真实 hook 函数 + 持久化（与生产路径同口径）。
	state, _, err := service.loadSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("载入失败: %v", err)
	}
	advanceQuestObjectives(&state, ObjectiveDefeatBoss, "zone_freedom_capital", 1)
	if state.ActiveQuests[0].State != QuestStateCompleted {
		t.Fatalf("讨平 boss 后任务应转 completed，得到 %q（进度 %d/%d）",
			state.ActiveQuests[0].State, state.ActiveQuests[0].Objectives[0].Current, state.ActiveQuests[0].Objectives[0].Required)
	}
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("持久化进度失败: %v", err)
	}

	// 记交付前钱包/经验，以验奖励确实入账。
	heroBefore, _ := service.units.GetByID(ctx, unitID)
	walletBefore, expBefore := heroBefore.Status.Wallet, heroBefore.Stats.Growth.Experience

	// 6) 交付：发奖（钱包经 Mutator + 经验直增）+ 解锁野外区传送 + 移出 Active 入 Completed。
	turnedIn, err := service.TurnInQuest(ctx, sessionID, unitID, slay.ID)
	if err != nil {
		t.Fatalf("交付任务失败: %v", err)
	}
	if turnedIn.State != QuestStateTurnedIn {
		t.Fatalf("交付后状态应为 turned_in，得到 %q", turnedIn.State)
	}

	// 钱包/经验确实增加（按骨架奖励：晨曦城郊 LevelMax=15 → wallet=20+15*3=65, exp=10+15*5=85）。
	heroAfter, _ := service.units.GetByID(ctx, unitID)
	if heroAfter.Status.Wallet != walletBefore+turnedIn.Rewards.Wallet {
		t.Fatalf("交付后钱包应增 %d：before=%d after=%d", turnedIn.Rewards.Wallet, walletBefore, heroAfter.Status.Wallet)
	}
	if heroAfter.Stats.Growth.Experience != expBefore+turnedIn.Rewards.Exp {
		t.Fatalf("交付后经验应增 %d：before=%d after=%d", turnedIn.Rewards.Exp, expBefore, heroAfter.Stats.Growth.Experience)
	}

	// 7) 任务态收尾：移出 ActiveQuests、入 CompletedQuestIDs、UnlockedZones 含野外区。
	stateAfter, _, _ := service.loadSession(ctx, sessionID)
	if questActiveIndex(&stateAfter, slay.ID) >= 0 {
		t.Fatal("交付后任务应移出 ActiveQuests")
	}
	if !questCompleted(&stateAfter, slay.ID) {
		t.Fatal("交付后任务应入 CompletedQuestIDs")
	}
	if !zoneUnlocked(&stateAfter, "zone_freedom_wild") {
		t.Fatal("交付带 UnlockZone 的任务后，野外区应被解锁")
	}

	// 8) 解锁制生效：之前 portal 锁（travel_test 验过被拒），现在 zonePortalUnlocked 应放开 + travel 成功。
	wildPortal, ok := zonePortalTo(&stateAfter, "zone_freedom_wild")
	if !ok {
		t.Fatal("主城应有通向同阵营野外区的传送门")
	}
	if wildPortal.Kind != "portal" {
		t.Fatalf("主城→野外应是 portal 类，得到 %q", wildPortal.Kind)
	}
	if !zonePortalUnlocked(&stateAfter, wildPortal) {
		t.Fatal("交付解锁后 zonePortalUnlocked 应放开野外区传送门")
	}
	if err := service.TravelToZone(ctx, sessionID, unitID, "zone_freedom_wild", ""); err != nil {
		t.Fatalf("解锁后 travel 到野外区应成功: %v", err)
	}

	// 9) 已交付任务不再出现在可接列表（防重复接取/刷奖）。在主城（用 ?zone= 显式看主城，无需折返——
	//    折返也是 portal、未解锁；用显式 zone 查列表即可验「已交付不复现」）。
	avail2, _ := service.AvailableQuests(ctx, sessionID, unitID, "zone_freedom_capital")
	for _, q := range avail2 {
		if q.ID == slay.ID {
			t.Fatal("已交付的讨伐任务不应再出现在可接列表")
		}
	}
	// 重复接取已交付任务应被拒。
	if _, err := service.AcceptQuest(ctx, sessionID, unitID, slay.ID); err == nil {
		t.Fatal("重复接取已交付任务应被拒，却成功了")
	}
}

// TestQuestCollectProgressAndAutonomyContext 验证收集类进度累计/clamp + 任务作「她的目标」喂自治上下文。
func TestQuestCollectProgressAndAutonomyContext(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()
	view, err := service.CreateMainWorldCharacter(ctx, "acc-quest2", MainWorldCharacterInput{Name: "采办娘", Origin: "市井游女"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	sessionID, unitID := view.SessionID, view.UnitID

	// 新手区即有收集任务（采 2 份口粮）。接取它。
	avail, err := service.AvailableQuests(ctx, sessionID, unitID, "")
	if err != nil {
		t.Fatalf("列可接任务失败: %v", err)
	}
	var collect Quest
	var found bool
	for _, q := range avail {
		if q.Type == QuestTypeCollect {
			collect, found = q, true
		}
	}
	if !found {
		t.Fatalf("新手区应有收集任务，得到 %+v", avail)
	}
	required := collect.Objectives[0].Required
	if _, err := service.AcceptQuest(ctx, sessionID, unitID, collect.ID); err != nil {
		t.Fatalf("接取收集任务失败: %v", err)
	}

	// 进度结算 hook：采集口粮累计（含溢出 clamp 不超额）。
	state, _, _ := service.loadSession(ctx, sessionID)
	advanceQuestObjectives(&state, ObjectiveCollectItem, questGatherItemID, 1)
	if got := state.ActiveQuests[0].Objectives[0].Current; got != 1 {
		t.Fatalf("采 1 份后进度应为 1，得到 %d", got)
	}
	// 一次超额采集：clamp 到 Required，且任务转 completed。
	advanceQuestObjectives(&state, ObjectiveCollectItem, questGatherItemID, required+10)
	if got := state.ActiveQuests[0].Objectives[0].Current; got != required {
		t.Fatalf("超额采集后进度应 clamp 到 %d，得到 %d", required, got)
	}
	if state.ActiveQuests[0].State != QuestStateCompleted {
		t.Fatalf("采满后任务应转 completed，得到 %q", state.ActiveQuests[0].State)
	}

	// 自治上下文集成：主角的进行中任务作「她当前的目标」喂进 directiveContextForActor（设计 §5.3 步骤 5）。
	ctxText := directiveContextForActor(state, unitID, state.PlayerFactionID)
	if !strings.Contains(ctxText, "她当前的目标") {
		t.Fatalf("自治上下文应含主角当前任务作目标，得到：%q", ctxText)
	}
	if !strings.Contains(ctxText, collect.Title) {
		t.Fatalf("自治上下文应含任务标题 %q，得到：%q", collect.Title, ctxText)
	}

	// 非玩家方单位（NPC）的上下文不应混入主角任务。取一个 ambient NPC（若有）。
	if len(state.AmbientUnitIDs) > 0 {
		npcCtx := directiveContextForActor(state, state.AmbientUnitIDs[0], "")
		if strings.Contains(npcCtx, "她当前的目标") {
			t.Fatal("NPC 的自治上下文不应混入主角任务目标")
		}
	}
}

// TestQuestSkeletonsDeterministic 验证骨架投放确定性 + 同阵营野外解锁推导 + id 唯一稳定。
func TestQuestSkeletonsDeterministic(t *testing.T) {
	zones := world.GenerateWorld(42)
	var capital world.Zone
	for _, z := range zones {
		if z.ID == "zone_freedom_capital" {
			capital = z
		}
	}
	if capital.ID == "" {
		t.Fatal("应能找到晨曦城郊主城区")
	}
	sk := questSkeletonsForZone(capital)
	if len(sk) < 1 {
		t.Fatalf("主城区应至少有一桩骨架，得到 %d", len(sk))
	}
	// 同区两次取骨架一致（确定性）。
	sk2 := questSkeletonsForZone(capital)
	if len(sk) != len(sk2) {
		t.Fatal("同区骨架数量应确定")
	}
	// id 确定性 + 唯一。
	ids := map[string]struct{}{}
	for _, s := range sk {
		id := questDeterministicID("sess1", capital.ID, s.Key)
		if id == questDeterministicID("sess1", capital.ID, s.Key+"x") {
			t.Fatal("不同 skeletonKey 的 id 应不同")
		}
		if _, dup := ids[id]; dup {
			t.Fatalf("骨架 id 应唯一，重复：%s", id)
		}
		ids[id] = struct{}{}
	}
	// 同阵营野外解锁推导。
	if got := sameFactionWildZoneID(capital); got != "zone_freedom_wild" {
		t.Fatalf("晨曦城郊应推出同阵营野外 zone_freedom_wild，得到 %q", got)
	}
	// 非主城区不推导。
	if got := sameFactionWildZoneID(world.Zone{ID: "zone_neutral_start", Kind: world.ZoneStarter}); got != "" {
		t.Fatalf("非主城区不应推导野外解锁，得到 %q", got)
	}
}
