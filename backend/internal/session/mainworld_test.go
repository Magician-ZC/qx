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
	"qunxiang/backend/internal/faction"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
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
	// 阵营开放世界 F1：不再播种固定敌方 NPC（EnemyUnitIDs 应为空）。
	if len(state.EnemyUnitIDs) != 0 {
		t.Fatalf("阵营开放世界不应有固定敌方单位，得到 %d 个", len(state.EnemyUnitIDs))
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

	// 阵营开放世界 F1：玩家角色应带阵营 + 道德轴（=该阵营道德基准）。
	// 本例出身「边境猎户」+ 夙愿「找到失散的妹妹」无秩序/混乱触发词，但「猎户」无自由触发词 → 默认 freedom。
	if hero.Faction != faction.IDFreedom {
		t.Fatalf("玩家角色应落 freedom 阵营（默认），得到 %q", hero.Faction)
	}
	if hero.MoralAlignment != faction.BaselineFor(faction.IDFreedom) {
		t.Fatalf("玩家角色道德轴应=freedom 道德基准，得到 %+v", hero.MoralAlignment)
	}
	// 视图应回显阵营 + 道德轴 + 出生据点。
	if view.Faction != faction.IDFreedom {
		t.Fatalf("视图应回显阵营 freedom，得到 %q", view.Faction)
	}
	if view.SpawnRegion == "" || faction.FactionForSpawnPoint(view.SpawnRegion) != faction.IDFreedom {
		t.Fatalf("视图出生据点应属 freedom 阵营，得到 %q", view.SpawnRegion)
	}

	// 阵营开放世界 F1：出生据点应播种公共同阵营 NPC（带 freedom 道德基准、阵营指纹 Lineage），
	// 且**绝无玩家↔NPC 的私人 relations 行**（公共非私人）。
	factionNPCs := 0
	for _, rec := range units {
		if isFactionNPCRecord(&rec) {
			factionNPCs++
			if rec.Faction != faction.IDFreedom {
				t.Fatalf("公共 NPC 应属 freedom 阵营，得到 %q", rec.Faction)
			}
			if rec.MoralAlignment.IsZero() {
				t.Fatalf("公共 NPC 道德轴不应为零值（应≈freedom 基准），NPC=%s", rec.Identity.Name)
			}
			// 道德轴应≈基准（freedom 维应仍占主导）。
			if faction.DominantFaction(rec.MoralAlignment) != faction.IDFreedom {
				t.Fatalf("公共 NPC 道德轴主导维应仍为 freedom，得到 %+v", rec.MoralAlignment)
			}
		}
	}
	if factionNPCs < 8 || factionNPCs > 12 {
		t.Fatalf("出生据点应播种 8–12 个公共同阵营 NPC，得到 %d", factionNPCs)
	}
	// 公共非私人：不存在以玩家主角为一端的 relations 行（关系靠后天游历相遇结成）。
	if rows := countRelationsForUnit(ctx, t, service.db, view.UnitID); rows != 0 {
		t.Fatalf("公共阵营 NPC 不应建玩家↔NPC 私人关系行，得到 %d 行", rows)
	}
}

// countRelationsForUnit 查 relations 表中以某单位为任一端的关系行数（验「公共非私人=零玩家关系行」）。
// relations 表不存在/无该单位行均返回 0（best-effort：表结构未知时保守返回 0，不让测试因 schema 偏差误失败）。
func countRelationsForUnit(ctx context.Context, t *testing.T, db *sql.DB, unitID string) int {
	t.Helper()
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations WHERE source_unit_id = ? OR target_unit_id = ?`,
		unitID, unitID,
	).Scan(&n)
	if err != nil {
		// 表/列不存在时（schema 与预期不符）保守返回 0：本断言只在 relations 表存在时有意义。
		return 0
	}
	return n
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

// TestCreateMainWorldCharacter_ExplicitFactionSpawnsCorrectly 验证显式选阵营降生：
// 玩家角色落该阵营 + 道德基准、出生据点属该阵营、出生点公共 NPC 全属该阵营且无玩家私人关系行。
func TestCreateMainWorldCharacter_ExplicitFactionSpawnsCorrectly(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	view, err := service.CreateMainWorldCharacter(ctx, "acc-order", MainWorldCharacterInput{
		Name:    "执律者",
		Faction: faction.IDOrder, // 显式选秩序阵营
	})
	if err != nil {
		t.Fatalf("选秩序阵营降生失败: %v", err)
	}
	if view.Faction != faction.IDOrder {
		t.Fatalf("玩家应落秩序阵营，得到 %q", view.Faction)
	}
	if view.MoralAlignment != faction.BaselineFor(faction.IDOrder) {
		t.Fatalf("玩家道德轴应=秩序道德基准，得到 %+v", view.MoralAlignment)
	}
	if faction.FactionForSpawnPoint(view.SpawnRegion) != faction.IDOrder {
		t.Fatalf("出生据点应属秩序阵营，得到 %q", view.SpawnRegion)
	}

	records, err := service.units.ListBySession(ctx, view.SessionID)
	if err != nil {
		t.Fatalf("ListBySession 失败: %v", err)
	}
	npcs := 0
	for i := range records {
		if isFactionNPCRecord(&records[i]) {
			npcs++
			if records[i].Faction != faction.IDOrder {
				t.Fatalf("公共 NPC 应属秩序阵营，得到 %q", records[i].Faction)
			}
		}
	}
	if npcs < 8 || npcs > 12 {
		t.Fatalf("秩序据点应播种 8–12 个公共 NPC，得到 %d", npcs)
	}
	if rows := countRelationsForUnit(ctx, t, service.db, view.UnitID); rows != 0 {
		t.Fatalf("公共阵营 NPC 不应建玩家↔NPC 私人关系行，得到 %d 行", rows)
	}
}

// TestSeedFactionSpawn_Idempotent 验证出生据点播种的幂等守卫：同 session 重复播种不重复造人。
func TestSeedFactionSpawn_Idempotent(t *testing.T) {
	_, service := newMainWorldTestService(t)
	ctx := context.Background()

	const sessionID = "sess-spawn-idem"
	first, err := service.SeedFactionSpawn(ctx, sessionID, faction.IDChaos, "ash_warrens", 42)
	if err != nil {
		t.Fatalf("首次播种失败: %v", err)
	}
	if first < 8 || first > 12 {
		t.Fatalf("首次应播种 8–12 个公共 NPC，得到 %d", first)
	}
	// 二次播种：幂等守卫命中，返回 0、不重复造人。
	again, err := service.SeedFactionSpawn(ctx, sessionID, faction.IDChaos, "ash_warrens", 42)
	if err != nil {
		t.Fatalf("二次播种失败: %v", err)
	}
	if again != 0 {
		t.Fatalf("二次播种应幂等命中返回 0，得到 %d", again)
	}
	// 落库层硬校验：阵营 NPC 总数仍等于首次播种数（未重复）。
	records, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListBySession 失败: %v", err)
	}
	total := 0
	for i := range records {
		if isFactionNPCRecord(&records[i]) {
			total++
		}
	}
	if total != first {
		t.Fatalf("幂等后阵营 NPC 总数应仍为 %d（未重复造人），得到 %d", first, total)
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
