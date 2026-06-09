package session

// 文件说明：大世界页游入口的集成测试（对真实 SQLite）：账号绑定持久角色的「捏人降生 + resume + 幂等」。
// 覆盖：
//   - 捏人降生正确性：1 个玩家角色（非选秀）、出身/夙愿写进传记、离线宪章落 desire/redline、绑 world_default、敌方 NPC 保留。
//   - 幂等持久：同账号二次 POST 不重复降生（返回既有同一 session/unit），GET resume 拿到同一角色。
//   - 村庄网：QUNXIANG_MAIN_VILLAGE 默认开时身边织 20 人关系网（best-effort，不强校验具体人数，只验有村民落库）。
//   - 关键战接管桥：FateBattleContext 经 SurfaceFateEvent 落库、OpenFateInbox/Feed 读回（payload 后向兼容）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/villageseed"
)

// newMainWorldTestService 起一个临时 SQLite 上的完整 Service（含 sessions 仓库），用于主世界入口集成测试。
// 强制 QUNXIANG_WORLD_BINDING=shared（默认即此，显式置位防外部环境干扰），让降生角色绑 world_default、resume 可查。
func newMainWorldTestService(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	t.Setenv("QUNXIANG_WORLD_BINDING", "shared")
	path := filepath.Join(t.TempDir(), "mainworld.db")
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

// TestCreateMainWorldCharacter_BirthBindingAndCharter 验证捏人降生的正确性：
// 单玩家角色 + 出身/夙愿入传记 + 离线宪章落 desire/redline + 绑 world_default + 敌方 NPC 保留。
func TestCreateMainWorldCharacter_BirthBindingAndCharter(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	in := MainWorldCharacterInput{
		Name:    "苏霜",
		Origin:  "边境猎户",
		Desire:  "找到失散的妹妹",
		Wound:   "她亲手埋葬了整个村子",
		Redline: "绝不向背叛者低头",
	}
	view, err := service.CreateMainWorldCharacter(ctx, "acc-1", in)
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	if !view.HasCharacter || !view.Created {
		t.Fatalf("首次降生应 has_character=true & created=true，得到 %+v", view)
	}
	if view.WorldID != defaultWorldID {
		t.Fatalf("主世界角色应绑 world_default，得到 world_id=%q", view.WorldID)
	}
	if view.Name != "苏霜" || view.Origin != "边境猎户" {
		t.Fatalf("角色名/出身回显不符：%+v", view)
	}

	state, units, err := service.loadSession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("载入 session 失败: %v", err)
	}
	// 单玩家角色（非选秀）。
	if len(state.PlayerUnitIDs) != 1 {
		t.Fatalf("主世界入口应只造 1 个玩家角色，得到 %d", len(state.PlayerUnitIDs))
	}
	// 敌方 NPC 保留（战棋接管战需要对手）。
	if len(state.EnemyUnitIDs) == 0 {
		t.Fatalf("敌方 NPC 阵营应保留，得到 0 个敌方单位")
	}
	if state.WorldID != defaultWorldID {
		t.Fatalf("state.WorldID 应为 world_default，得到 %q", state.WorldID)
	}
	if state.AccountID != "acc-1" {
		t.Fatalf("state.AccountID 应为 acc-1，得到 %q", state.AccountID)
	}

	byID := make(map[string]unit.Record, len(units))
	for _, rec := range units {
		byID[rec.ID] = rec
	}
	hero, ok := byID[view.UnitID]
	if !ok {
		t.Fatalf("主角单位 %q 未落库", view.UnitID)
	}
	// 出身写进 Lineage + 夙愿/创伤写进传记。
	if hero.Identity.Lineage != "边境猎户" {
		t.Fatalf("出身应写进 Lineage，得到 %q", hero.Identity.Lineage)
	}
	if bio := hero.Identity.Biography; bio == "" ||
		!contains(bio, "找到失散的妹妹") || !contains(bio, "她亲手埋葬了整个村子") {
		t.Fatalf("夙愿/创伤应写进传记，得到 %q", bio)
	}
	// 离线宪章：desire → 长期目标；redline → 红线。
	charter, has := GetUnitCharter(&state, view.UnitID)
	if !has {
		t.Fatalf("主角应有离线宪章")
	}
	if !sliceContains(charter.LongTermGoals, "找到失散的妹妹") {
		t.Fatalf("夙愿应落进宪章长期目标，得到 %+v", charter.LongTermGoals)
	}
	if len(charter.Redlines) == 0 || charter.Redlines[0].Text != "绝不向背叛者低头" {
		t.Fatalf("红线应落进宪章 Redlines，得到 %+v", charter.Redlines)
	}
}

// TestCreateMainWorldCharacter_Idempotent 验证幂等持久：同账号二次 POST 不重复降生，GET resume 拿到同一角色。
func TestCreateMainWorldCharacter_Idempotent(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	first, err := service.CreateMainWorldCharacter(ctx, "acc-2", MainWorldCharacterInput{Name: "初见"})
	if err != nil {
		t.Fatalf("首次降生失败: %v", err)
	}
	if !first.Created {
		t.Fatalf("首次降生 created 应为 true")
	}

	// 二次 POST：必须返回既有角色、绝不重复造人（防多设备/重复点击）。
	second, err := service.CreateMainWorldCharacter(ctx, "acc-2", MainWorldCharacterInput{Name: "另一个名字"})
	if err != nil {
		t.Fatalf("二次降生失败: %v", err)
	}
	if second.Created {
		t.Fatalf("二次降生应命中既有、created=false，得到 %+v", second)
	}
	if second.SessionID != first.SessionID || second.UnitID != first.UnitID {
		t.Fatalf("二次降生应返回同一 session/unit，first=%+v second=%+v", first, second)
	}
	if second.Name != "初见" {
		t.Fatalf("二次降生不应覆盖既有角色名，得到 %q", second.Name)
	}

	// resume：GET /api/me/character 后端，应拿到同一角色。
	resumed, err := service.ResumeMainWorldCharacter(ctx, "acc-2")
	if err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	if !resumed.HasCharacter || resumed.SessionID != first.SessionID || resumed.UnitID != first.UnitID {
		t.Fatalf("resume 应拿到同一角色，first=%+v resumed=%+v", first, resumed)
	}
	if resumed.Created {
		t.Fatalf("resume 不是新降生，created 应为 false")
	}

	// 落库层校验：同账号在 world_default 至多一局（幂等查询只命中一条）。
	cnt := countSessionsForAccountWorld(ctx, t, service.db, "acc-2", defaultWorldID)
	if cnt != 1 {
		t.Fatalf("同账号在 world_default 应恰好 1 局（防重复降生），得到 %d", cnt)
	}
}

// TestResumeMainWorldCharacter_NoCharacter 验证未捏人的账号 resume 返回 has_character=false（前端据此进捏人）。
func TestResumeMainWorldCharacter_NoCharacter(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	view, err := service.ResumeMainWorldCharacter(ctx, "acc-empty")
	if err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	if view.HasCharacter {
		t.Fatalf("未捏人账号应 has_character=false，得到 %+v", view)
	}
}

// TestMainWorldCharacter_AccountIsolation 验证账号隔离：A 降生不让 B 误命中（resume 按账号精确匹配）。
func TestMainWorldCharacter_AccountIsolation(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	a, err := service.CreateMainWorldCharacter(ctx, "acc-a", MainWorldCharacterInput{Name: "甲"})
	if err != nil {
		t.Fatalf("A 降生失败: %v", err)
	}
	// B 还没降生 → resume 必须 miss（不能误命中 A）。
	if bView, err := service.ResumeMainWorldCharacter(ctx, "acc-b"); err != nil {
		t.Fatalf("B resume 失败: %v", err)
	} else if bView.HasCharacter {
		t.Fatalf("B 未降生却 resume 命中（账号隔离失败）：%+v", bView)
	}
	// B 降生 → 与 A 不同 session/unit。
	b, err := service.CreateMainWorldCharacter(ctx, "acc-b", MainWorldCharacterInput{Name: "乙"})
	if err != nil {
		t.Fatalf("B 降生失败: %v", err)
	}
	if b.SessionID == a.SessionID || b.UnitID == a.UnitID {
		t.Fatalf("A/B 应为不同角色，a=%+v b=%+v", a, b)
	}
}

// TestCreateMainWorldCharacter_OriginStillSeedsVillage 是 H1 回归：带「出身」的主角降生后，身边仍应织满 20 人村庄。
// bug：applyMainWorldPersona 把主角 Lineage 覆写为出身原型（如「边境猎户」），命中 isSeededVillagerRecord 的村民
// Lineage 指纹；旧版 sessionAlreadyHasVillage 不剔除玩家单位，会把先于 seedVillage 落库的主角误判为「本局已织村」，
// 致整张 20 人关系网被整体跳过（零村民），破「她身边已有二十个有名有姓的人」核心承诺。
// 这里**显式校验村民人数恰为 20**（剔除玩家单位后命中村民指纹的记录数），正是旧测试漏掉的断言。
func TestCreateMainWorldCharacter_OriginStillSeedsVillage(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-origin", MainWorldCharacterInput{
		Name:   "苏霜",
		Origin: "边境猎户", // 触发 bug 的关键：出身覆写主角 Lineage，命中村民 Lineage 指纹
	})
	if err != nil {
		t.Fatalf("带出身降生失败: %v", err)
	}

	records, err := service.units.ListBySession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("ListBySession 失败: %v", err)
	}
	// 玩家主角单位集合（须从村民计数中剔除——主角带出身会命中村民指纹）。
	playerSet := map[string]struct{}{view.UnitID: {}}

	villagers := 0
	heroLineage := ""
	heroCountedAsVillager := false
	for i := range records {
		if _, isPlayer := playerSet[records[i].ID]; isPlayer {
			heroLineage = records[i].Identity.Lineage
			// 确认主角确实命中了村民指纹（否则 bug 前提不成立、测试无意义）。
			if isSeededVillagerRecord(&records[i]) {
				heroCountedAsVillager = true
			}
			continue
		}
		if isSeededVillagerRecord(&records[i]) {
			villagers++
		}
	}
	if !heroCountedAsVillager {
		t.Fatalf("前提失效：带出身的主角应命中村民 Lineage 指纹（lineage=%q），否则本回归无意义", heroLineage)
	}
	if villagers != villageseed.VillageSize {
		t.Fatalf("带出身降生后村民应恰为 %d 人（H1：出身致整张村庄被跳过的回归），得到 %d", villageseed.VillageSize, villagers)
	}
}

// TestCreateMainWorldCharacter_ConcurrentNoDoubleCharacter 是 H2 回归：同账号并发降生应恰得 1 个角色。
// bug：query-first 幂等守卫（FindMainWorldSessionID）与终写 Save 之间窗口很宽，两个并发请求可能各越过守卫、
// 各插一行不同 uuid id 的 session，致同账号在 world_default 出现两个角色。修复靠唯一索引
// uniq_single_player_sessions_account_world + Save 撞唯一冲突时回退查既有角色返回。
// 校验：并发后库中该 (account, world_default) 恰 1 条 session，且两次返回收敛到同一 session/unit。
func TestCreateMainWorldCharacter_ConcurrentNoDoubleCharacter(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	const accountID = "acc-concurrent"
	const parallel = 4

	var wg sync.WaitGroup
	results := make([]MainWorldCharacter, parallel)
	errs := make([]error, parallel)
	start := make(chan struct{})
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // 同时起跑，最大化 TOCTOU 竞态窗口重叠
			results[idx], errs[idx] = service.CreateMainWorldCharacter(ctx, accountID, MainWorldCharacterInput{Name: "并发降生"})
		}(i)
	}
	close(start)
	wg.Wait()

	// 所有并发调用都应成功（输家走 dup-key 回退查既有，不报错）。
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发降生第 %d 个失败（应优雅收敛而非报错）: %v", i, err)
		}
	}
	// 落库层硬校验：同账号在 world_default 恰 1 条 session（唯一索引 + 回退兜住 TOCTOU）。
	if cnt := countSessionsForAccountWorld(ctx, t, service.db, accountID, defaultWorldID); cnt != 1 {
		t.Fatalf("并发降生后同账号在 world_default 应恰 1 局（H2：并发 TOCTOU 造双角色的回归），得到 %d", cnt)
	}
	// 所有返回应收敛到同一 session/unit（赢家的角色）。
	winnerSession := results[0].SessionID
	winnerUnit := results[0].UnitID
	if winnerSession == "" || winnerUnit == "" {
		t.Fatalf("并发降生返回视图缺 session/unit：%+v", results[0])
	}
	for i := 1; i < parallel; i++ {
		if results[i].SessionID != winnerSession || results[i].UnitID != winnerUnit {
			t.Fatalf("并发降生应全部收敛到同一角色，第 0 个=%+v 第 %d 个=%+v", results[0], i, results[i])
		}
	}
}

// TestFateBattleContext_RoundTrip 验证关键战接管桥：FateBattleContext 经 SurfaceFateEvent 落库、命运卡读回（后向兼容）。
func TestFateBattleContext_RoundTrip(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-battle", MainWorldCharacterInput{Name: "执剑人"})
	if err != nil {
		t.Fatalf("降生失败: %v", err)
	}
	hero, err := service.units.GetByID(ctx, view.UnitID)
	if err != nil {
		t.Fatalf("取主角失败: %v", err)
	}

	// 直接路由一张带战斗上下文的待决策卡（Importance 高 + 自身锚，必进 pending/highlight）。
	ev := FateEvent{
		ActorID:       hero.ID,
		TargetID:      hero.ID,
		ReasonCode:    events.ReasonPendingDecision,
		Importance:    9,
		EmotionWeight: 0.9,
		Summary:       "一头黑甲凶兽拦住了去路。",
		Battle: &FateBattleContext{
			SessionID:  view.SessionID,
			ThreatID:   "threat-x",
			ThreatName: "黑甲凶兽",
			Tier:       "elite",
			Takeover:   true,
		},
	}
	routing, err := service.SurfaceFateEvent(ctx, view.SessionID, &hero, ev)
	if err != nil {
		t.Fatalf("SurfaceFateEvent 失败: %v", err)
	}
	_ = routing

	// 命运卡读回：inbox 或 feed 至少一处能读到 battle 上下文（依路由档而定，二者覆盖 pending/highlight）。
	if !battleContextSurfaced(ctx, t, service, hero.ID) {
		t.Fatalf("FateBattleContext 应经命运卡 surfaced（takeover=true 的关键战接管桥）")
	}
}

// battleContextSurfaced 检查某角色的命运 inbox/feed 是否有一张卡带回了 battle 上下文（且字段正确）。
func battleContextSurfaced(ctx context.Context, t *testing.T, service *Service, unitID string) bool {
	t.Helper()
	inbox, err := service.OpenFateInbox(ctx, unitID)
	if err != nil {
		t.Fatalf("OpenFateInbox 失败: %v", err)
	}
	for _, it := range inbox {
		if it.Battle != nil && it.Battle.ThreatName == "黑甲凶兽" && it.Battle.Takeover {
			return true
		}
	}
	feed, err := service.OpenFateFeed(ctx, unitID, 30)
	if err != nil {
		t.Fatalf("OpenFateFeed 失败: %v", err)
	}
	for _, it := range feed {
		if it.Battle != nil && it.Battle.ThreatName == "黑甲凶兽" && it.Battle.Takeover {
			return true
		}
	}
	return false
}

// countSessionsForAccountWorld 直接查 single_player_sessions 计某账号在某世界的局数（验幂等不重复降生的落库口径）。
func countSessionsForAccountWorld(ctx context.Context, t *testing.T, db *sql.DB, accountID, worldID string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM single_player_sessions WHERE account_id = ? AND world_id = ?`,
		accountID, worldID,
	).Scan(&n); err != nil {
		t.Fatalf("count sessions 失败: %v", err)
	}
	return n
}

// sliceContains 判定字符串切片是否含某项（测试用小工具）。
func sliceContains(items []string, target string) bool {
	for _, it := range items {
		if it == target {
			return true
		}
	}
	return false
}
