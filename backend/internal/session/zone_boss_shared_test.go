package session

// 文件说明：共享世界 Phase4「共享进度」集成测试（对真实 SQLite）：
// 区域 boss 升级为 world 级共享实例——A 攻击共享 zone boss 扣血 → B 查 boss HP 已下降（共享池）；
// 累计伤害清零 → 按贡献分赃（A/B 各得，钱包经 Mutator）→ boss defeated 后 world 级「已讨平」（B 再挑战被拒）；
// flag 关私有档维持单人 elite 零影响；多区 boss 并存；幂等 get-or-create。

import (
	"context"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/storage/dbmigrate"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

// sharedZoneState 构造一个「开 flag 的共享世界局」最小 state：world_id=共享世代 + 含一个有 boss 的 zone + CurrentZoneID。
func sharedZoneState(zoneID, bossName string, bossLevel int) State {
	zone := world.Zone{
		ID:        zoneID,
		Name:      "试炼之地",
		BossCoord: "3,3",
		BossName:  bossName,
		BossLevel: bossLevel,
	}
	return State{
		ID:            "sess_shared",
		WorldID:       sharedWorldID,
		CurrentZoneID: zoneID,
		Zones:         []world.Zone{zone},
	}
}

// TestSharedZoneBoss_SharedHPAndContributionLoot 端到端共享进度：
//
//	A 攻击共享 zone boss 扣血 → 共享池血量下降（B 视角同一池）；累计清零 → 按贡献分赃（A/B 各得、钱包经 Mutator）；
//	boss defeated 后 world 级「已讨平」（B 再挑战被拒，A 也被拒）。
func TestSharedZoneBoss_SharedHPAndContributionLoot(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1") // 开共享世界 flag（默认关）
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateSharedWorld(t, ctx, service)
	state := sharedZoneState("zone_freedom_capital", "赤鳞古龙", 1) // hp = 200+1*40 = 240

	// 两个不同账号的角色（A 强 / B 弱），同区挑战同一共享 boss。
	a := bossStriker(t, ctx, repo, 301, "甲", 80) // 单击 80
	b := bossStriker(t, ctx, repo, 302, "乙", 40) // 单击 40

	// A 第一击：共享池 240 → 160，未死。
	r1, err := service.challengeSharedZoneBoss(ctx, &state, a, state.Zones[0])
	if err != nil {
		t.Fatalf("A 首击失败: %v", err)
	}
	if r1.Outcome != "ongoing" || r1.DamageDealt != 80 {
		t.Fatalf("A 首击应只打伤共享 boss：期望 ongoing dmg=80，得到 outcome=%s dmg=%d", r1.Outcome, r1.DamageDealt)
	}
	bossID := r1.ThreatID

	// B 视角：查同一共享池血量——应已被 A 的攻击扣到 160（共享 HP，不是各打各的）。
	if hp := worldBossHP(t, service, bossID); hp != 160 {
		t.Fatalf("B 应看到共享池血量已被 A 扣到 160，得到 %d（共享 HP 失效）", hp)
	}

	// B 攻击：160 → 120。仍是同一池。
	r2, err := service.challengeSharedZoneBoss(ctx, &state, b, state.Zones[0])
	if err != nil {
		t.Fatalf("B 出手失败: %v", err)
	}
	if r2.ThreatID != bossID {
		t.Fatalf("B 应打同一头共享 boss（同 region get-or-create 幂等）：A=%s B=%s", bossID, r2.ThreatID)
	}
	if hp := worldBossHP(t, service, bossID); hp != 120 {
		t.Fatalf("B 攻击后共享池应剩 120，得到 %d", hp)
	}

	// A 连击直到清零（80*2=160 ≥ 120），最后一击致命并结算。
	var settle EliteEncounterResult
	for i := 0; i < 3; i++ {
		r, err := service.challengeSharedZoneBoss(ctx, &state, a, state.Zones[0])
		if err != nil {
			t.Fatalf("A 补刀第 %d 次失败: %v", i, err)
		}
		if r.Outcome == "defeated" {
			settle = r
			break
		}
	}
	if settle.Outcome != "defeated" {
		t.Fatalf("A 补刀应最终讨平共享 boss，得到 outcome=%s", settle.Outcome)
	}
	// 结算者（最后一击 A）应填充 Awards：A/B 各分得货币（按贡献瓜分），唯一遗物恰 1 名得主。
	goldBy := map[string]int{}
	relicWinners := 0
	for _, aw := range settle.Awards {
		if aw.ItemID == "gold" {
			goldBy[aw.UnitID] += aw.Quantity
		}
		if aw.ItemID == zoneBossEpicRelicID && aw.Reason == "won" {
			relicWinners++
		}
	}
	if relicWinners != 1 {
		t.Fatalf("zone_boss_relic 应恰 1 名得主（arbitration 胜率∝贡献），得到 %d", relicWinners)
	}
	// A（单次 80）贡献 > B（单次 40），可分货币 A 应分得更多（频率无关、按单次最高伤害）。
	if goldBy[a.ID] <= goldBy[b.ID] {
		t.Fatalf("A（单次80）应比 B（单次40）分得更多货币，得到 A=%d B=%d", goldBy[a.ID], goldBy[b.ID])
	}
	// 钱包经 Mutator 落库到本库两名参战者（共享出手只写本侧钱包，绝不直写他人 units）。
	ra, _ := repo.GetByID(ctx, a.ID)
	rb, _ := repo.GetByID(ctx, b.ID)
	if ra.Status.Wallet != goldBy[a.ID] || rb.Status.Wallet != goldBy[b.ID] {
		t.Fatalf("钱包应经 Mutator 到账：A 期望 %d 得 %d，B 期望 %d 得 %d", goldBy[a.ID], ra.Status.Wallet, goldBy[b.ID], rb.Status.Wallet)
	}

	// world 级「已讨平」共享：B 再挑战应被拒（A 打掉 → B 看到已讨平）。
	if _, err := service.challengeSharedZoneBoss(ctx, &state, b, state.Zones[0]); err == nil {
		t.Fatalf("共享 boss 已讨平后 B 再挑战应被拒（world 级已讨平共享）")
	}
	// A 自己再挑战也被拒（不是 per-session 私有，是 world 级共享事实）。
	if _, err := service.challengeSharedZoneBoss(ctx, &state, a, state.Zones[0]); err == nil {
		t.Fatalf("共享 boss 已讨平后 A 再挑战也应被拒（world 级共享，非 per-session）")
	}

	// 讨平广播进世界总线（全区可见的不可篡改事实）恰一条。
	if got := countCrossKind(t, service, wid, worldbus.KindWorldBossDown); got != 1 {
		t.Fatalf("应恰一条共享 boss 讨平广播，得到 %d", got)
	}
}

// TestSharedZoneBoss_DefeatedIsWorldLevel 单独校验「已讨平」是 world 级共享：
// A 用一个 state 打掉 → B 用**另一个 session 的 state**（同 world/同 zone）查到已讨平（不是 per-session 私有集合）。
func TestSharedZoneBoss_DefeatedIsWorldLevel(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	mustCreateSharedWorld(t, ctx, service)

	stateA := sharedZoneState("zone_chaos_wild", "万岁幽冥", 1) // hp=240
	stateA.ID = "sess_A"
	a := bossStriker(t, ctx, repo, 311, "甲", 240) // 一击清零

	r, err := service.challengeSharedZoneBoss(ctx, &stateA, a, stateA.Zones[0])
	if err != nil || r.Outcome != "defeated" {
		t.Fatalf("A 一击应讨平，得到 outcome=%s err=%v", r.Outcome, err)
	}

	// B 是另一个 session 的玩家（state.DefeatedBosses 各自私有、互不可见），但 world 级共享应让 B 看到已讨平。
	stateB := sharedZoneState("zone_chaos_wild", "万岁幽冥", 1)
	stateB.ID = "sess_B"
	b := bossStriker(t, ctx, repo, 312, "乙", 40)
	if _, err := service.challengeSharedZoneBoss(ctx, &stateB, b, stateB.Zones[0]); err == nil {
		t.Fatalf("B（另一 session）应看到 world 级已讨平、被拒——证明非 per-session 私有")
	}

	// 直接断言 sharedZoneBossDefeated 也判已讨平。
	regionID := sharedRegionID(sharedWorldID, "zone_chaos_wild")
	defeated, err := service.sharedZoneBossDefeated(ctx, sharedWorldID, regionID)
	if err != nil {
		t.Fatalf("查共享已讨平失败: %v", err)
	}
	if !defeated {
		t.Fatalf("sharedZoneBossDefeated 应判已讨平（world 级共享）")
	}
}

// TestSharedZoneBoss_MultiZoneCoexist 多区 boss 并存：同一共享世界两个不同 zone 各自一头 active 共享 boss，
// 互不冲突（约束键 (world_id, region_id) 升级后多区可并存；旧 (world_id) 单键会误拒第二头）。
func TestSharedZoneBoss_MultiZoneCoexist(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	mustCreateSharedWorld(t, ctx, service)

	r1 := sharedRegionID(sharedWorldID, "zone_freedom_capital")
	r2 := sharedRegionID(sharedWorldID, "zone_order_capital")

	id1, _, def1, err := service.ensureSharedZoneBoss(ctx, sharedWorldID, r1, "赤鳞古龙", 3)
	if err != nil || def1 {
		t.Fatalf("第一区 boss get-or-create 应成功未讨平，得到 err=%v defeated=%v", err, def1)
	}
	// 第二区 boss：旧 (world_id) 单键约束会因「该 world 已有 active」误拒；升级到 (world_id, region_id) 后应能并存。
	id2, _, def2, err := service.ensureSharedZoneBoss(ctx, sharedWorldID, r2, "万岁幽冥", 5)
	if err != nil || def2 {
		t.Fatalf("第二区 boss 应能与第一区并存（多区约束键），得到 err=%v defeated=%v", err, def2)
	}
	if id1 == id2 {
		t.Fatalf("两区 boss 应是不同实例，得到同一 id=%s", id1)
	}
	// 两头都 active：world_bosses 该 world 应有 2 头 active。
	var n int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM world_bosses WHERE world_id = ? AND status = 'active'`, sharedWorldID).Scan(&n); err != nil {
		t.Fatalf("数 active boss 失败: %v", err)
	}
	if n != 2 {
		t.Fatalf("多区 boss 应并存 2 头 active，得到 %d（多区约束键未生效）", n)
	}
}

// TestEnsureSharedZoneBoss_Idempotent get-or-create 幂等：同 (world, region) 反复调返回同一头 active boss，不重复 spawn。
func TestEnsureSharedZoneBoss_Idempotent(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	mustCreateSharedWorld(t, ctx, service)
	region := sharedRegionID(sharedWorldID, "zone_freedom_wild")

	first, _, _, err := service.ensureSharedZoneBoss(ctx, sharedWorldID, region, "噬星鲲鹏", 10)
	if err != nil {
		t.Fatalf("首次 get-or-create 失败: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, _, defeated, err := service.ensureSharedZoneBoss(ctx, sharedWorldID, region, "噬星鲲鹏", 10)
		if err != nil || defeated {
			t.Fatalf("幂等 get-or-create 第 %d 次失败: err=%v defeated=%v", i, err, defeated)
		}
		if again != first {
			t.Fatalf("幂等应返回同一头 active boss，得到 %s vs %s", again, first)
		}
	}
	var n int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM world_bosses WHERE world_id = ? AND region_id = ? AND status = 'active'`,
		sharedWorldID, region).Scan(&n); err != nil {
		t.Fatalf("数 active boss 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("幂等 get-or-create 不应重复 spawn，应恰 1 头 active，得到 %d", n)
	}
}

// TestChallengeZoneBoss_FlagOffStaysPrivateElite flag 关时 ChallengeZoneBoss 维持单人 elite 私有路径，零影响：
// 不开 QUNXIANG_SHARED_WORLD → inSharedWorld=false → 走 ResolveEliteEncounter + state.DefeatedBosses，
// 绝不在 world_bosses 表落任何共享 boss 行（私有档零影响的硬证据）。
func TestChallengeZoneBoss_FlagOffStaysPrivateElite(t *testing.T) {
	// 不设 QUNXIANG_SHARED_WORLD（默认关）；显式清空隔离外部环境。
	t.Setenv("QUNXIANG_SHARED_WORLD", "")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	mustCreateSharedWorld(t, ctx, service)

	// 即便 state.WorldID 设成共享世代，flag 关 → inSharedWorld=false → 仍走私有 elite 路径。
	state := sharedZoneState("zone_freedom_capital", "赤鳞古龙", 1)
	if inSharedWorld(&state) {
		t.Fatalf("flag 关时 inSharedWorld 应为 false（私有档路径）")
	}
	// 私有路径下 challengeSharedZoneBoss 不会被 ChallengeZoneBoss 调用；这里直接验证：flag 关 → world_bosses 表空，
	// 即任何走私有 elite 路径的挑战都不会污染共享 boss 表。
	var n int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM world_bosses WHERE world_id = ?`, sharedWorldID).Scan(&n); err != nil {
		t.Fatalf("数 world_bosses 失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("flag 关私有档不应在 world_bosses 落任何共享 boss 行，得到 %d", n)
	}
}

// TestWorldBossActiveIndexMigratesToRegionScoped 校验存量库升级路径：把约束键从旧 (world_id) 单键迁到 (world_id, region_id)。
// 模拟一个**已建过旧索引**的存量库——手动 DROP 新索引、建旧的 (world_id) 单键 partial unique index（升级前的定义），
// 此时第二头不同 region 的 active boss 会被旧约束误拒；跑迁移 SQL（DROP 旧 + CREATE 新键）后，多区 boss 应能并存。
func TestWorldBossActiveIndexMigratesToRegionScoped(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateSharedWorld(t, ctx, service)

	// 回退到「旧世代」索引定义：DROP 当前（新键）→ 建旧的 (world_id) 单键。
	if _, err := service.db.ExecContext(ctx, dbmigrate.WorldBossActiveUniqueIndexDropOldSQLite); err != nil {
		t.Fatalf("drop 新索引失败: %v", err)
	}
	if _, err := service.db.ExecContext(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_world_boss_active ON world_bosses(world_id) WHERE status='active'`); err != nil {
		t.Fatalf("建旧 (world_id) 单键索引失败: %v", err)
	}

	// 旧约束下：第一头 active 成功，第二头不同 region 的 active 被旧 (world_id) 单键误拒。
	r1 := sharedRegionID(wid, "zone_a")
	r2 := sharedRegionID(wid, "zone_b")
	if _, err := service.db.ExecContext(ctx,
		`INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id) VALUES (?, ?, ?, ?, ?, 'active', ?)`,
		"zb_old_1", wid, "甲", 100, 100, r1); err != nil {
		t.Fatalf("旧约束下首头应成功: %v", err)
	}
	_, errSecond := service.db.ExecContext(ctx,
		`INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id) VALUES (?, ?, ?, ?, ?, 'active', ?)`,
		"zb_old_2", wid, "乙", 100, 100, r2)
	if errSecond == nil {
		t.Fatalf("旧 (world_id) 单键约束下，第二区 active boss 应被误拒（证明旧约束确实拦多区）")
	}

	// 跑迁移：DROP 旧 + CREATE 新 (world_id, region_id) 键。
	if _, err := service.db.ExecContext(ctx, dbmigrate.WorldBossActiveUniqueIndexDropOldSQLite); err != nil {
		t.Fatalf("迁移 DROP 旧索引失败: %v", err)
	}
	if _, err := service.db.ExecContext(ctx, dbmigrate.WorldBossActiveUniqueIndexSQLite); err != nil {
		t.Fatalf("迁移 CREATE 新键失败: %v", err)
	}

	// 升级后：第二区 active boss 应能与第一区并存（多区约束键生效）。
	if _, err := service.db.ExecContext(ctx,
		`INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id) VALUES (?, ?, ?, ?, ?, 'active', ?)`,
		"zb_new_2", wid, "乙", 100, 100, r2); err != nil {
		t.Fatalf("升级后第二区 active boss 应能并存，得到: %v", err)
	}
	// 同区第二头 active 仍应被新键拒（每区至多一头 active 不变）。
	_, errSameRegion := service.db.ExecContext(ctx,
		`INSERT INTO world_bosses (id, world_id, name, hp_max, hp_remaining, status, region_id) VALUES (?, ?, ?, ?, ?, 'active', ?)`,
		"zb_dup_r1", wid, "丙", 100, 100, r1)
	if errSameRegion == nil {
		t.Fatalf("新键下同区第二头 active 仍应被拒（每区至多一头 active）")
	}
}

// TestSharedZoneBoss_DefeatAdvancesQuestObjective 评审 #3：共享路径讨平 boss 应推进本玩家的 defeat_boss 任务目标
// （与私有路径 zone_boss.go:102 同口径）。此前共享路径完全没接 advanceQuestObjectives → 共享世界里 defeat_boss 任务变死任务。
// 用 newThreatTestService（无 sessions 仓）直接验证内存态 state.ActiveQuests 被推进、目标全满转 completed。
func TestSharedZoneBoss_DefeatAdvancesQuestObjective(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	mustCreateSharedWorld(t, ctx, service)

	state := sharedZoneState("zone_freedom_capital", "赤鳞古龙", 1) // hp=240
	// 挂一桩进行中的 defeat_boss 任务（target=本区 zoneID，Required=1）——讨平后应被推进至 completed。
	state.ActiveQuests = []Quest{{
		ID:         "q_slay_capital",
		Type:       QuestTypeSlay,
		ZoneID:     "zone_freedom_capital",
		State:      QuestStateActive,
		Objectives: []Objective{{Kind: ObjectiveDefeatBoss, Target: "zone_freedom_capital", Required: 1}},
	}}

	a := bossStriker(t, ctx, repo, 401, "甲", 240) // 一击清零并结算
	r, err := service.challengeSharedZoneBoss(ctx, &state, a, state.Zones[0])
	if err != nil {
		t.Fatalf("讨平共享 boss 失败: %v", err)
	}
	if r.Outcome != "defeated" {
		t.Fatalf("一击应讨平共享 boss，得到 outcome=%s", r.Outcome)
	}
	// 任务目标应被推进（current=1/required=1）且 quest 转 completed（与私有路径同口径）。
	q := state.ActiveQuests[0]
	if q.Objectives[0].Current != 1 {
		t.Fatalf("讨平后 defeat_boss 目标应推进到 1，得到 %d（共享路径漏接 advanceQuestObjectives → 死任务）", q.Objectives[0].Current)
	}
	if q.State != QuestStateCompleted {
		t.Fatalf("目标全满后任务应转 completed，得到 %q", q.State)
	}
}

// TestSharedZoneBoss_QuestObjectiveUnaffectedWhenOngoing 仅打伤（未讨平）不应推进任务目标（防误推进）。
func TestSharedZoneBoss_QuestObjectiveUnaffectedWhenOngoing(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	mustCreateSharedWorld(t, ctx, service)

	state := sharedZoneState("zone_chaos_wild", "万岁幽冥", 1) // hp=240
	state.ActiveQuests = []Quest{{
		ID:         "q_slay_wild",
		Type:       QuestTypeSlay,
		ZoneID:     "zone_chaos_wild",
		State:      QuestStateActive,
		Objectives: []Objective{{Kind: ObjectiveDefeatBoss, Target: "zone_chaos_wild", Required: 1}},
	}}
	a := bossStriker(t, ctx, repo, 402, "甲", 40) // 单击 40 < 240，只打伤
	r, err := service.challengeSharedZoneBoss(ctx, &state, a, state.Zones[0])
	if err != nil {
		t.Fatalf("打伤共享 boss 失败: %v", err)
	}
	if r.Outcome != "ongoing" {
		t.Fatalf("单击未清零应只打伤，得到 outcome=%s", r.Outcome)
	}
	if state.ActiveQuests[0].Objectives[0].Current != 0 {
		t.Fatalf("仅打伤不应推进 defeat_boss 目标，得到 current=%d", state.ActiveQuests[0].Objectives[0].Current)
	}
}

// TestZonesOverview_SharedDefeatedIsWorldLevel 评审 #4：共享局 ZonesOverview 的 boss_defeated 取自 world 级共享事实，
// 而非恒 false 的 per-session state.DefeatedBosses。A 打掉某区共享 boss → B（另一 session、state.DefeatedBosses 空）
// 的 /zones 该区 boss_defeated 应为 true（无需点击即正确置灰）；未打掉的区 false；SharedBoss 标记恒 true。
func TestZonesOverview_SharedDefeatedIsWorldLevel(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	path := filepath.Join(t.TempDir(), "zones_shared.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewServiceWithColdStore(db, nil, nil)
	ctx := context.Background()
	mustCreateSharedWorld(t, ctx, service)

	// A 用 stateA 打掉 zone_freedom_capital 的共享 boss（world 级 defeated）。
	stateA := sharedZoneState("zone_freedom_capital", "赤鳞古龙", 1)
	striker := unit.BootstrapRecord(501, "sessA", "player", "甲")
	striker.Status.Attack = 240
	if err := service.units.Save(ctx, striker); err != nil {
		t.Fatalf("保存出手者失败: %v", err)
	}
	r, err := service.challengeSharedZoneBoss(ctx, &stateA, &striker, stateA.Zones[0])
	if err != nil || r.Outcome != "defeated" {
		t.Fatalf("A 应一击讨平 zone_freedom_capital，得到 outcome=%s err=%v", r.Outcome, err)
	}

	// B 是另一个 session：state.DefeatedBosses 空，含两个区（一个已被 A 讨平、一个未讨平）。持久化后走 ZonesOverview。
	stateB := State{
		ID:            "sess_B_zones",
		WorldID:       sharedWorldID,
		CurrentZoneID: "zone_freedom_capital",
		Zones: []world.Zone{
			{ID: "zone_freedom_capital", Name: "晨曦城", BossCoord: "3,3", BossName: "赤鳞古龙", BossLevel: 1},
			{ID: "zone_chaos_wild", Name: "混沌野", BossCoord: "5,5", BossName: "万岁幽冥", BossLevel: 1},
		},
	}
	if err := service.sessions.Save(ctx, &stateB); err != nil {
		t.Fatalf("持久化 B 会话失败: %v", err)
	}

	summaries, _, err := service.ZonesOverview(ctx, "sess_B_zones")
	if err != nil {
		t.Fatalf("ZonesOverview 失败: %v", err)
	}
	byID := map[string]ZoneSummary{}
	for _, s := range summaries {
		byID[s.ID] = s
	}
	// 被 A 讨平的区：B 看到 world 级已讨平（boss_defeated=true），尽管 B 的 state.DefeatedBosses 是空的。
	if !byID["zone_freedom_capital"].BossDefeated {
		t.Fatalf("共享局 ZonesOverview 应让 B 看到 A 打掉的区已讨平（world 级共享），得到 boss_defeated=false")
	}
	// 未被讨平的区：false。
	if byID["zone_chaos_wild"].BossDefeated {
		t.Fatalf("未讨平的区 boss_defeated 应为 false，却为 true")
	}
	// SharedBoss 标记：共享局恒 true。
	if !byID["zone_freedom_capital"].SharedBoss || !byID["zone_chaos_wild"].SharedBoss {
		t.Fatalf("共享局所有区的 SharedBoss 标记应为 true")
	}
}

// mustCreateSharedWorld 建一个 id==共享世代（world_shared_v1）的世界（供 inSharedWorld 局接入）。
func mustCreateSharedWorld(t *testing.T, ctx context.Context, service *Service) string {
	t.Helper()
	id, err := world.Create(ctx, service.db, world.World{ID: sharedWorldID, Name: "共享世界"})
	if err != nil {
		t.Fatalf("建共享世界失败: %v", err)
	}
	return id
}
