package session

// 文件说明：共享世界 Phase 3「跨玩家撮合/七交互真跨 session」集成测试（对真实 SQLite）。
// 死守设计宪法红线：**B 永远只能写一条 cross_event，改不了 A 的 units/relations**——
// 跨玩家交互各自只写本侧 + 一条共享 cross_event，绝不直写他人 units。
//
// 覆盖：
//   - 两账号共享世界同区，A 对 B 自动/手动发起七交互（结识）→ 各自本侧 relations 更新 + 一条 cross_event 落库；
//     断言 A 的结算**绝不**直改 B 的 units（B 的 units 行逐字节不变，B 无被 A 写的本侧关系）；只 cross_event。
//   - flag 关 / 非共享世界：跨玩家自动发起 no-op，私有档零影响。
//   - 撮合候选池跨 session：共享世界 buildMatchCandidates 并入同区别玩家。
//   - storage 层写红线：SaveOwnedBy / assertOwnSideUnitWrite 拒绝写他人 session 的 units，并留审计。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// makeSharedProtagonist 在共享世界世代造一个属 sessionID、锚到 worldID#zoneID 复合 region 的存活 protagonist，落库。
func makeSharedProtagonist(t *testing.T, ctx context.Context, repo *unit.Repository, id, sessionID, faction, name, zoneID string) unit.Record {
	t.Helper()
	rec := unit.BootstrapRecord(2, sessionID, faction, name)
	rec.ID = id
	rec.Identity.LifecycleClass = unit.LifecycleProtagonist
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
	// 锚到复合 region=worldID#zoneID（Phase 2 口径），使 ListActiveByRegion 跨 session 命中。
	if err := repo.SetUnitScope(ctx, id, sharedWorldID, sharedRegionID(sharedWorldID, zoneID)); err != nil {
		t.Fatalf("scope %s: %v", id, err)
	}
	return rec
}

// unitRowSnapshot 直读 units 物理行 (version, profile_json, session_id)——断言「他人单位零改写」。
func unitRowSnapshot(t *testing.T, service *Service, unitID string) (int64, string, string) {
	t.Helper()
	var ver int64
	var prof, sess string
	if err := service.db.QueryRow(`SELECT version, profile_json, session_id FROM units WHERE id = ?`, unitID).
		Scan(&ver, &prof, &sess); err != nil {
		t.Fatalf("读 units 行 %q: %v", unitID, err)
	}
	return ver, prof, sess
}

func relationRowCount(t *testing.T, service *Service, src, tgt string) int {
	t.Helper()
	var n int
	if err := service.db.QueryRow(
		`SELECT COUNT(*) FROM relations WHERE source_unit_id = ? AND target_unit_id = ?`, src, tgt).Scan(&n); err != nil {
		t.Fatalf("count relations %s->%s: %v", src, tgt, err)
	}
	return n
}

// TestPhase3_CrossPlayerAcquaint_OnlyWritesOwnSidePlusCrossEvent 是 Phase 3 头号红线回归：
// 共享世界同区，A 的 protagonist 对 B 的 protagonist 发起结识（层1 unilateral）→
//   - A 本侧关系 a→b 被写（A 的 own-side outgoing edge）；
//   - 一条 cross_event 落库（共享事实源）；
//   - B 的 units 行**逐字节不变**（version/profile_json/session_id 不被 A 改写）；
//   - B 无「被 A 写」的本侧 outgoing 关系（b→a 关系行不存在——A 绝不替 B 写 B 的 relations）。
func TestPhase3_CrossPlayerAcquaint_OnlyWritesOwnSidePlusCrossEvent(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	if _, err := world.Create(ctx, service.db, world.World{ID: sharedWorldID, Name: "共享世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}
	// A（session s1）与 B（session s2）同区相遇。
	a := makeSharedProtagonist(t, ctx, repo, "phase3_a", "s1", "free", "甲", "zone_x")
	b := makeSharedProtagonist(t, ctx, repo, "phase3_b", "s2", "order", "乙", "zone_x")

	// B 的物理行快照（A 发起前）。
	verB0, profB0, sessB0 := unitRowSnapshot(t, service, b.ID)

	// A 对 B 发起结识（统一管线，层1 unilateral 立即只改本侧）。
	res, err := service.RecordSevenInteraction(ctx, sharedWorldID, a.ID, b.ID, InteractionAcquaint, 4)
	if err != nil {
		t.Fatalf("结识发起失败: %v", err)
	}
	if !res.Applied || res.Tier != "unilateral" {
		t.Fatalf("结识应层1 unilateral 立即成立，得 %+v", res)
	}

	// ① A 本侧关系 a→b 被写（own-side）。
	if relationRowCount(t, service, a.ID, b.ID) != 1 {
		t.Fatalf("结识后应有 A 的本侧关系行 a→b（own-side）")
	}
	if relAxis(t, service, "trust", a.ID, b.ID) <= 0 {
		t.Fatalf("结识应增 a→b 信任（本侧）")
	}

	// ② 一条 cross_event 落库（共享事实源，append-only）。
	var crossN int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cross_events WHERE world_id = ? AND actor_unit_id = ? AND target_unit_id = ?`,
		sharedWorldID, a.ID, b.ID).Scan(&crossN); err != nil {
		t.Fatalf("count cross_events: %v", err)
	}
	if crossN != 1 {
		t.Fatalf("A 对 B 的结识应恰落 1 条 cross_event，得到 %d", crossN)
	}

	// ③ 红线：B 的 units 物理行逐字节不变（A 绝不直写 B 的 units）。
	verB1, profB1, sessB1 := unitRowSnapshot(t, service, b.ID)
	if verB1 != verB0 || profB1 != profB0 || sessB1 != sessB0 {
		t.Fatalf("跨玩家硬不变量被破坏：B 的 units 行被改写（version %d→%d, profile_changed=%v, session %q→%q）",
			verB0, verB1, profB1 != profB0, sessB0, sessB1)
	}

	// ④ 红线：A 绝不替 B 写 B 的本侧 outgoing 关系（b→a 关系行不存在）。
	if relationRowCount(t, service, b.ID, a.ID) != 0 {
		t.Fatalf("跨玩家硬不变量被破坏：A 不应替 B 写 B 的本侧关系 b→a")
	}
}

// TestPhase3_AutoCrossSocialize_SharedWorldOnly 验证玩法内自动发起：QUNXIANG_AUTO_SOCIAL_CROSS 开 + 共享世界局，
// 部署边界自动让同区 A 对 B 发起结识（落 cross_event + A 本侧关系 + 本侧日志）；flag 关时 no-op、零影响。
func TestPhase3_AutoCrossSocialize_SharedWorldOnly(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	t.Setenv("QUNXIANG_AUTO_SOCIAL_CROSS", "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, service.db, world.World{ID: sharedWorldID, Name: "共享世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}

	a := makeSharedProtagonist(t, ctx, repo, "auto_a", "s1", "free", "甲", "zone_x")
	b := makeSharedProtagonist(t, ctx, repo, "auto_b", "s2", "order", "乙", "zone_x")
	verB0, profB0, _ := unitRowSnapshot(t, service, b.ID)

	state := &State{ID: "s1", WorldID: sharedWorldID, CurrentZoneID: "zone_x", PlayerUnitIDs: []string{a.ID}}
	state.TurnState.Turn = 4 // 命中 crossSocialEveryNTurns 周期（turn%4==0）

	// 本 session units 只含 A；B 经同区跨 session peer 拉入。多周期重试以越过概率门（确定性，但某 epoch 才命中）。
	fired := false
	for _, turn := range []int{4, 8, 12, 16, 20, 24, 28, 32} {
		state.TurnState.Turn = turn
		service.scanAndSocializeCrossPlayer(ctx, state, []unit.Record{a})
		var n int
		if err := service.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM cross_events WHERE actor_unit_id = ? AND target_unit_id = ?`, a.ID, b.ID).Scan(&n); err != nil {
			t.Fatalf("count cross_events: %v", err)
		}
		if n > 0 {
			fired = true
			break
		}
	}
	if !fired {
		t.Fatalf("自动跨玩家发起应在若干周期内对同区 B 落至少一条 cross_event（概率门确定性，多周期必命中）")
	}
	// 红线：B 的 units 行不被改写。
	verB1, profB1, _ := unitRowSnapshot(t, service, b.ID)
	if verB1 != verB0 || profB1 != profB0 {
		t.Fatalf("自动跨玩家发起绝不应直写 B 的 units：version %d→%d profile_changed=%v", verB0, verB1, profB1 != profB0)
	}
	// 本侧应有 cross_social 日志（A 视角）。
	hasLog := false
	for _, l := range state.Logs {
		if l.Kind == "cross_social" {
			hasLog = true
		}
	}
	if !hasLog {
		t.Fatalf("自动跨玩家发起应留本侧 cross_social 日志")
	}
}

// TestPhase3_AutoCrossSocialize_FlagOffNoOp 验证 flag 关 / 非共享世界局：跨玩家自动发起整段 no-op，私有档零影响。
func TestPhase3_AutoCrossSocialize_FlagOffNoOp(t *testing.T) {
	// 不开 QUNXIANG_AUTO_SOCIAL_CROSS（默认关）。即便共享世界 flag 开、同区有别玩家，也不发起。
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	t.Setenv("QUNXIANG_AUTO_SOCIAL_CROSS", "0")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, service.db, world.World{ID: sharedWorldID, Name: "共享世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}
	a := makeSharedProtagonist(t, ctx, repo, "off_a", "s1", "free", "甲", "zone_x")
	b := makeSharedProtagonist(t, ctx, repo, "off_b", "s2", "order", "乙", "zone_x")

	state := &State{ID: "s1", WorldID: sharedWorldID, CurrentZoneID: "zone_x", PlayerUnitIDs: []string{a.ID}}
	for _, turn := range []int{4, 8, 12, 16, 20, 24} {
		state.TurnState.Turn = turn
		service.scanAndSocializeCrossPlayer(ctx, state, []unit.Record{a})
	}
	var n int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cross_events WHERE actor_unit_id = ? AND target_unit_id = ?`, a.ID, b.ID).Scan(&n); err != nil {
		t.Fatalf("count cross_events: %v", err)
	}
	if n != 0 {
		t.Fatalf("flag 关时跨玩家自动发起应 no-op，却落了 %d 条 cross_event", n)
	}
	if len(state.Logs) != 0 {
		t.Fatalf("flag 关时不应写任何日志，却写了 %d 条", len(state.Logs))
	}
}

// TestPhase3_MatchPoolIncludesCrossSessionPeer 验证撮合候选池跨 session：共享世界局 buildMatchCandidates
// 把同区别玩家（跨 session）并入候选池；非共享世界 / flag 关时只含本 session 单位。
func TestPhase3_MatchPoolIncludesCrossSessionPeer(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, service.db, world.World{ID: sharedWorldID, Name: "共享世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}
	a := makeSharedProtagonist(t, ctx, repo, "mp_a", "s1", "free", "甲", "zone_x")
	b := makeSharedProtagonist(t, ctx, repo, "mp_b", "s2", "order", "乙", "zone_x")

	state := &State{ID: "s1", WorldID: sharedWorldID, CurrentZoneID: "zone_x", PlayerFactionID: "free"}
	state.TurnState.Turn = 4

	// 共享世界：候选池应含本 session 的 A + 跨 session 的 B（同区）。
	cands := service.buildMatchCandidates(ctx, state, []unit.Record{a})
	ids := map[string]bool{}
	for _, c := range cands {
		ids[c.UnitID] = true
	}
	if !ids[a.ID] || !ids[b.ID] {
		t.Fatalf("共享世界撮合候选池应含本 session A(%s) + 跨 session 同区 B(%s)，得到 %v", a.ID, b.ID, ids)
	}

	// 对照：非共享世界（WorldID 非共享世代）时只含本 session 单位（B 不并入）。
	privState := &State{ID: "s1", WorldID: "world_default", CurrentZoneID: "zone_x", PlayerFactionID: "free"}
	privState.TurnState.Turn = 4
	privCands := service.buildMatchCandidates(ctx, privState, []unit.Record{a})
	for _, c := range privCands {
		if c.UnitID == b.ID {
			t.Fatalf("非共享世界局不应并入跨 session 的 B（私有档零影响）")
		}
	}
}

// TestPhase3_SaveOwnedBy_RejectsCrossSessionWrite 验证 storage 层写红线：SaveOwnedBy 拒绝写**他人 session** 的 units，
// 并保证他人物理行逐字节不变；写**本侧** session 的 units 则放行。
func TestPhase3_SaveOwnedBy_RejectsCrossSessionWrite(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	owner := makeBasicUnit(t, ctx, repo, "own_u", "s1", "甲方")
	other := makeBasicUnit(t, ctx, repo, "other_u", "s2", "乙方")

	verO0, profO0, _ := unitRowSnapshot(t, service, other.ID)

	// 越界：以 s1 操作方身份试图写 s2 的单位 → 必拒（ErrCrossSessionWrite），他人行不变。
	mutated := other
	mutated.Identity.Nickname = "被篡改"
	if err := repo.SaveOwnedBy(ctx, mutated, "s1"); err == nil {
		t.Fatalf("SaveOwnedBy 应拒绝写他人 session 的单位（跨玩家硬不变量），却放行了")
	}
	verO1, profO1, sessO1 := unitRowSnapshot(t, service, other.ID)
	if verO1 != verO0 || profO1 != profO0 || sessO1 != "s2" {
		t.Fatalf("被拒写后他人 units 行必须逐字节不变：version %d→%d profile_changed=%v session=%q",
			verO0, verO1, profO1 != profO0, sessO1)
	}

	// 本侧：以 s1 操作方身份写 s1 自己的单位 → 放行。
	mine := owner
	mine.Identity.Nickname = "本侧改名"
	if err := repo.SaveOwnedBy(ctx, mine, "s1"); err != nil {
		t.Fatalf("SaveOwnedBy 应放行写本侧单位: %v", err)
	}
	reloaded, err := repo.GetByID(ctx, owner.ID)
	if err != nil {
		t.Fatalf("reload owner: %v", err)
	}
	if reloaded.Identity.Nickname != "本侧改名" {
		t.Fatalf("本侧写应落盘，nickname 期望「本侧改名」得 %q", reloaded.Identity.Nickname)
	}
}

// TestPhase3_AssertOwnSideUnitWrite_DeniesAndAudits 验证 session 层断言 + 审计：assertOwnSideUnitWrite 对他人单位拒绝
// 并落一条 CROSS_WRITE_DENIED 审计；对本侧单位放行。saveOwnSideUnit 门面同样守红线。
func TestPhase3_AssertOwnSideUnitWrite_DeniesAndAudits(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	owner := makeBasicUnit(t, ctx, repo, "asrt_own", "s1", "甲方")
	other := makeBasicUnit(t, ctx, repo, "asrt_other", "s2", "乙方")

	// 本侧：放行（nil）。
	if err := service.assertOwnSideUnitWrite(ctx, "s1", owner.ID, "test_self"); err != nil {
		t.Fatalf("本侧单位断言应放行: %v", err)
	}
	// 越界：拒绝 + 审计。
	if err := service.assertOwnSideUnitWrite(ctx, "s1", other.ID, "test_cross"); err == nil {
		t.Fatalf("他人单位断言应拒绝")
	}
	var auditN int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE reason_code = ?`, string(events.ReasonCrossWriteDenied)).Scan(&auditN); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if auditN < 1 {
		t.Fatalf("越界写应落至少 1 条 CROSS_WRITE_DENIED 审计，得到 %d", auditN)
	}

	// saveOwnSideUnit 门面：越界写他人单位 → 拒绝且物理行不变。
	verO0, profO0, _ := unitRowSnapshot(t, service, other.ID)
	bad := other
	bad.Identity.Nickname = "越界改名"
	if err := service.saveOwnSideUnit(ctx, "s1", bad, "facade_cross"); err == nil {
		t.Fatalf("saveOwnSideUnit 应拒绝越界写他人单位")
	}
	verO1, profO1, _ := unitRowSnapshot(t, service, other.ID)
	if verO1 != verO0 || profO1 != profO0 {
		t.Fatalf("saveOwnSideUnit 被拒后他人行必须不变：version %d→%d profile_changed=%v", verO0, verO1, profO1 != profO0)
	}
}

// TestPhase3_ConsentAccept_WritesOnlyAcceptorOwnSideEdge 是 major-2 修复的核心回归：
// 跨玩家 consent accept 只写**接受方自己 owner 一侧**的出边（source=target=接受方 → target=actor=发起方），
// **绝不**替发起方写其 outgoing relations。共享世界两账号同区，A 对 B 发起联姻（层3 requires_consent）→ 建 pending；
// B（接受方）的合法 owner session 自治接受 → 只写 B→A 出边（B 的本侧），A→B 出边（发起方一侧）零行。
func TestPhase3_ConsentAccept_WritesOnlyAcceptorOwnSideEdge(t *testing.T) {
	t.Setenv("QUNXIANG_SHARED_WORLD", "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, service.db, world.World{ID: sharedWorldID, Name: "共享世界"}); err != nil {
		t.Fatalf("create world: %v", err)
	}
	// A=发起方(session s1)，B=接受方(session s2)。
	a := makeSharedProtagonist(t, ctx, repo, "c_a", "s1", "free", "甲", "zone_x")
	b := makeSharedProtagonist(t, ctx, repo, "c_b", "s2", "order", "乙", "zone_x")
	// 给接受方 B 对发起方 A 种一条显著关系（affection 7），使自治回应归因成立（≥0.3）。
	cgSeedRelationRow(t, service, b.ID, a.ID, 0, 0, 7, 0)

	verA0, profA0, _ := unitRowSnapshot(t, service, a.ID)

	// A 对 B 发起联姻（层3）→ 建 target=B 的 pending（暂不应用）。
	res, err := service.RecordSevenInteraction(ctx, sharedWorldID, a.ID, b.ID, InteractionMarriage, 8)
	if err != nil || res.ConsentRequestID == "" {
		t.Fatalf("联姻应建 pending，得 %+v err=%v", res, err)
	}

	// 接受方 B 的合法 owner session（s2）边界结算 → 自治接受。
	service.settleConsentsAtBoundary(ctx, &State{ID: "s2"})
	if pend, _ := service.ListPendingConsents(ctx, b.ID); len(pend) != 0 {
		t.Fatalf("接受方 owner 边界应自治接受、清空 pending，得 %d 条", len(pend))
	}

	// ① 只写接受方本侧出边 B→A（联姻 affection 7+5 clamp 到 10）。
	if relationRowCount(t, service, b.ID, a.ID) != 1 {
		t.Fatalf("accept 应写接受方本侧出边 B→A")
	}
	if aff := relAxis(t, service, "affection", b.ID, a.ID); aff != 10 {
		t.Fatalf("接受方本侧 B→A affection 应为 7+5 clamp 到 10，得 %v", aff)
	}
	// ② 红线：绝不替发起方 A 写其出边 A→B。
	if relationRowCount(t, service, a.ID, b.ID) != 0 {
		t.Fatalf("跨玩家硬不变量被破坏：accept 不应替发起方 A 写出边 A→B")
	}
	// ③ 红线：发起方 A 的 units 物理行逐字节不变。
	verA1, profA1, _ := unitRowSnapshot(t, service, a.ID)
	if verA1 != verA0 || profA1 != profA0 {
		t.Fatalf("accept 绝不应直写发起方 A 的 units：version %d→%d profile_changed=%v", verA0, verA1, profA1 != profA0)
	}
}

// TestPhase3_AssertOwnSideRelationWrite_DeniesReversedSourceAndAudits 验证关系写红线断言**已真实武装**（修复 major-1）：
// assertOwnSideRelationWrite 对「source≠接受方」（方向写反/试图改写发起方出边）拒绝并落一条 CROSS_WRITE_DENIED 审计；
// 对「source==接受方」放行。这是 consent accept 写前的把关断言，不再是装好没接线的休眠工具。
func TestPhase3_AssertOwnSideRelationWrite_DeniesReversedSourceAndAudits(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	owner := makeBasicUnit(t, ctx, repo, "rel_acceptor", "s1", "接受方")
	initiator := makeBasicUnit(t, ctx, repo, "rel_initiator", "s2", "发起方")

	tx, err := service.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 本侧出边（source==接受方）→ 放行。
	if err := service.assertOwnSideRelationWrite(ctx, tx, owner.ID, owner.ID, "test_self"); err != nil {
		t.Fatalf("接受方本侧出边应放行: %v", err)
	}
	// 方向写反（source=发起方≠接受方）→ 拒绝。
	if err := service.assertOwnSideRelationWrite(ctx, tx, initiator.ID, owner.ID, "test_reversed"); err == nil {
		t.Fatalf("source 非接受方（试图改写发起方出边）应被拒绝")
	}
	_ = tx.Commit()

	// 落了一条 CROSS_WRITE_DENIED 审计（越界企图可追溯）。
	var auditN int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE reason_code = ?`, string(events.ReasonCrossWriteDenied)).Scan(&auditN); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	if auditN < 1 {
		t.Fatalf("方向写反的关系写应落至少 1 条 CROSS_WRITE_DENIED 审计，得 %d", auditN)
	}
}

// makeBasicUnit 造一个属 sessionID 的普通存活单位并落库（不锚 region，用于纯 storage 红线测试）。
func makeBasicUnit(t *testing.T, ctx context.Context, repo *unit.Repository, id, sessionID, name string) unit.Record {
	t.Helper()
	rec := unit.BootstrapRecord(2, sessionID, "player", name)
	rec.ID = id
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save %s: %v", id, err)
	}
	return rec
}
