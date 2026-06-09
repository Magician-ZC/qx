package session

// 文件说明：consent 层2/3「离线 A 知情 + 自治回应 + 超时回响卡」测试（设计 事件耦合 §2.3-2.5）。对真实 SQLite，覆盖：
//   ① 层2(CONTESTED)交互成立 → 给离线 A 投待决策回应卡（ReasonCrossConsentPending）；consent_state=contested_pending；
//   ② 层3(REQUIRES_CONSENT) → 挂 pending 同时给 A 投高光卡；consent_state=consent_pending；
//   ③ A 按归因自治回应：对 B 有显著关系/现实压力 → 过 attribution 管线接受；无源 → 隐忍不接受（consent_state=declined）；
//   ④ 超时：层3 失效给发起方 B 投回响卡（ReasonCrossConsentTimeout）；层2 按宪章兜底自治回应。
// 辅助命名一律 cg* 前缀，避免与同包其它测试文件撞名（如 seedRelation/relAxis 已被占用）。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// cgSeedWorldAndUnit 建世界 + 一个落本库的单位（sessionID=worldID，便于 SurfaceFateEvent 落 events 本库行）。
func cgSeedWorldAndUnit(t *testing.T, ctx context.Context, service *Service, repo *unit.Repository, worldID string, ids ...string) {
	t.Helper()
	if _, err := world.Create(ctx, service.db, world.World{ID: worldID, Name: "consent离线测试世界"}); err != nil {
		// 世界可能已建（多用例复用）：忽略重复建错。
		_ = err
	}
	for _, id := range ids {
		rec := unit.BootstrapRecord(1, worldID, "player", "x")
		rec.ID = id
		if err := repo.Save(ctx, rec); err != nil {
			t.Fatalf("save unit %s: %v", id, err)
		}
	}
}

// cgConsentState 读某 cross_event 的 consent_state（best-effort 注记列）。
func cgConsentState(t *testing.T, service *Service, eventID string) string {
	t.Helper()
	var s string
	_ = service.db.QueryRow(`SELECT COALESCE(consent_state,'') FROM cross_events WHERE id=?`, eventID).Scan(&s)
	return s
}

// cgCardCount 数某 owner（actor_unit_id）上「payload.reason 为给定跨玩家 reason-code」的命运卡行数。
// 注意：SurfaceFateEvent 落库 reason_code 是路由档（INBOX_HIGHLIGHT/PENDING_DECISION），FateEvent.ReasonCode 进 payload.reason；
// 故按 payload_json 含该 reason 子串计数（确定性、双驱动安全；payload 为 JSON，"reason":"CODE" 子串足够判定）。
func cgCardCount(t *testing.T, service *Service, ownerID string, code events.ReasonCode) int {
	t.Helper()
	var n int
	like := `%"reason":"` + string(code) + `"%`
	if err := service.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id=? AND payload_json LIKE ?`, ownerID, like,
	).Scan(&n); err != nil {
		t.Fatalf("count cards: %v", err)
	}
	return n
}

// cgSeedRelationRow 直接写一行 A→B 关系四轴（绕过 service，避免与同包 seedRelation 撞名/签名差异）。
func cgSeedRelationRow(t *testing.T, service *Service, src, tgt string, trust, fear, affection, rivalry float64) {
	t.Helper()
	if _, err := service.db.Exec(
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry, notes_json)
		 VALUES (?,?,?,?,?,?, '{}')`,
		src, tgt, trust, fear, affection, rivalry); err != nil {
		t.Fatalf("seed relation %s->%s: %v", src, tgt, err)
	}
}

// TestConsentContestedSurfacesResponseCardToTarget 断言①：层2(CONTESTED，反目)交互成立时给离线 A 投
// 待决策回应卡（ReasonCrossConsentPending），且 cross_events.consent_state=contested_pending。
func TestConsentContestedSurfacesResponseCardToTarget(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	cgSeedWorldAndUnit(t, ctx, service, repo, "w1", "a", "b")

	// 反目 = 层2 contested。target=b（离线 A），actor=a（发起方 B）。
	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionFallout, 7)
	if err != nil {
		t.Fatalf("fallout: %v", err)
	}
	if res.Tier != "contested" || res.ConsentRequestID == "" {
		t.Fatalf("反目应为 contested 档且建 pending，得 %+v", res)
	}
	// 给离线 A（b）投了一张 ReasonCrossConsentPending 卡。
	if got := cgCardCount(t, service, "b", events.ReasonCrossConsentPending); got < 1 {
		t.Fatalf("层2 应给离线 A 投待回应卡，得卡数 %d", got)
	}
	// cross_event 同意档记 contested_pending。
	if st := cgConsentState(t, service, res.EventID); st != consentStateContestedPending {
		t.Fatalf("层2 consent_state 应为 %q，得 %q", consentStateContestedPending, st)
	}
}

// TestConsentRequiresConsentSurfacesHighlightToTarget 断言②：层3(REQUIRES_CONSENT，联姻)挂 pending 同时给 A 投高光，
// 且 cross_events.consent_state=consent_pending。
func TestConsentRequiresConsentSurfacesHighlightToTarget(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	cgSeedWorldAndUnit(t, ctx, service, repo, "w1", "a", "b")

	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage: %v", err)
	}
	if res.Tier != "requires_consent" || res.ConsentRequestID == "" {
		t.Fatalf("联姻应为 requires_consent 档且建 pending，得 %+v", res)
	}
	if got := cgCardCount(t, service, "b", events.ReasonCrossConsentPending); got < 1 {
		t.Fatalf("层3 应给离线 A 投高光卡，得卡数 %d", got)
	}
	if st := cgConsentState(t, service, res.EventID); st != consentStateConsentPending {
		t.Fatalf("层3 consent_state 应为 %q，得 %q", consentStateConsentPending, st)
	}
}

// TestAutonomousConsentResponse_GroundedAccepts 断言③（接受）：A 对发起方 B 有显著敌视关系（rivalry）→
// 归因落 relation → 过 attribution 管线自治接受（联姻 accept 应用本侧 affection 增量），consent_state=accepted。
func TestAutonomousConsentResponse_GroundedAccepts(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	cgSeedWorldAndUnit(t, ctx, service, repo, "w1", "a", "b")

	// b（离线 A）对 a（B）有强亲近关系 → relation 类前因显著（归一后 affection 0.7 ≥ 0.3）。
	cgSeedRelationRow(t, service, "b", "a", 0, 0, 7, 0)

	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage: %v", err)
	}
	if res.ConsentRequestID == "" {
		t.Fatalf("联姻应建 pending，得 %+v", res)
	}

	// 回合边界自治回应：b 有显著关系 → 接受 1 条。
	state := &State{ID: "w1"}
	accepted, err := service.autoResolveConsentsByCharter(ctx, state, "b")
	if err != nil {
		t.Fatalf("autoResolve: %v", err)
	}
	if accepted != 1 {
		t.Fatalf("有源（显著关系）应自治接受 1 条，得 %d", accepted)
	}
	// accept 应用本侧 a→b 关系增量（联姻 affection+5）。
	if aff := relAxis(t, service, "affection", "a", "b"); aff != 5 {
		t.Fatalf("自治接受后 a→b affection 应为联姻增量 5，得 %v", aff)
	}
	if st := cgConsentState(t, service, res.EventID); st != consentStateAccepted {
		t.Fatalf("接受后 consent_state 应为 %q，得 %q", consentStateAccepted, st)
	}
}

// TestAutonomousConsentResponse_UngroundedHolds 断言③（隐忍）：A 对 B 既无显著关系、也无现实压力 → 无源 →
// 判 OOC 回退保守隐忍：不接受、留 pending、关系不变、consent_state=declined。
func TestAutonomousConsentResponse_UngroundedHolds(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	cgSeedWorldAndUnit(t, ctx, service, repo, "w1", "a", "b")
	// 不种任何关系、b 无压力位（BootstrapRecord 默认健康饱足无债无伤）。

	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage: %v", err)
	}
	if res.ConsentRequestID == "" {
		t.Fatalf("联姻应建 pending，得 %+v", res)
	}

	state := &State{ID: "w1"}
	accepted, err := service.autoResolveConsentsByCharter(ctx, state, "b")
	if err != nil {
		t.Fatalf("autoResolve: %v", err)
	}
	if accepted != 0 {
		t.Fatalf("无源应保守隐忍 0 接受，得 %d", accepted)
	}
	// 仍 pending、关系不变。
	if pend, _ := service.ListPendingConsents(ctx, "b"); len(pend) != 1 {
		t.Fatalf("隐忍应留 pending，得 %d 条", len(pend))
	}
	if aff := relAxis(t, service, "affection", "a", "b"); aff != 0 {
		t.Fatalf("隐忍不应用任何关系增量，得 affection=%v", aff)
	}
	if st := cgConsentState(t, service, res.EventID); st != consentStateDeclined {
		t.Fatalf("隐忍后 consent_state 应为 %q，得 %q", consentStateDeclined, st)
	}
}

// TestConsentTimeout_RequiresConsentEchoesToInitiator 断言④（层3 超时）：requires_consent 超时失效 → 给发起方 B 投
// 回响卡（ReasonCrossConsentTimeout），consent_state=timeout，且请求被置 expired（不应用任何效果）。
func TestConsentTimeout_RequiresConsentEchoesToInitiator(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	cgSeedWorldAndUnit(t, ctx, service, repo, "w1", "a", "b")

	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8)
	if err != nil {
		t.Fatalf("marriage: %v", err)
	}
	if res.ConsentRequestID == "" {
		t.Fatalf("联姻应建 pending")
	}

	// 用远未来 cutoff 把所有 pending 判超时。
	n, err := service.ExpireStaleConsents(ctx, "9999-12-31 00:00:00")
	if err != nil || n < 1 {
		t.Fatalf("应至少 expire 1 条，得 n=%d err=%v", n, err)
	}
	// 发起方 B（a）收到一张超时回响卡。
	if got := cgCardCount(t, service, "a", events.ReasonCrossConsentTimeout); got < 1 {
		t.Fatalf("层3 超时应给发起方 B 投回响卡，得卡数 %d", got)
	}
	if st := cgConsentState(t, service, res.EventID); st != consentStateTimeout {
		t.Fatalf("层3 超时 consent_state 应为 %q，得 %q", consentStateTimeout, st)
	}
	// 不应用任何关系效果（联姻 affection 增量未落）。
	if aff := relAxis(t, service, "affection", "a", "b"); aff != 0 {
		t.Fatalf("超时失效不应用关系效果，得 affection=%v", aff)
	}
}

// TestConsentTimeout_ContestedCharterFallback 断言④（层2 超时）：contested 超时按 A 的离线宪章兜底自治回应——
// A 对 B 有显著关系（有源）→ 兜底接受应用效果、consent_state=accepted（不再 pending，不计入 expired）。
func TestConsentTimeout_ContestedCharterFallback(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	cgSeedWorldAndUnit(t, ctx, service, repo, "w1", "a", "b")
	// b（离线 A）对 a（B）有强敌视（rivalry 归一 0.7 ≥ 0.3）→ 兜底回应有源。
	cgSeedRelationRow(t, service, "b", "a", 0, 0, 0, 7)

	// 反目 = 层2 contested。
	res, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionFallout, 7)
	if err != nil {
		t.Fatalf("fallout: %v", err)
	}
	if res.ConsentRequestID == "" {
		t.Fatalf("反目应建 pending")
	}

	if _, err := service.ExpireStaleConsents(ctx, "9999-12-31 00:00:00"); err != nil {
		t.Fatalf("expire: %v", err)
	}
	// 层2 兜底接受 → 不再 pending，关系效果应用（反目 trust-3/rivalry+3）。
	if pend, _ := service.ListPendingConsents(ctx, "b"); len(pend) != 0 {
		t.Fatalf("层2 兜底接受后不应仍 pending，得 %d 条", len(pend))
	}
	if tr := relAxis(t, service, "trust", "a", "b"); tr != -3 {
		t.Fatalf("反目兜底接受应应用 a→b trust 增量 -3，得 %v", tr)
	}
	if st := cgConsentState(t, service, res.EventID); st != consentStateAccepted {
		t.Fatalf("层2 兜底接受 consent_state 应为 %q，得 %q", consentStateAccepted, st)
	}
}

// TestSettleConsentsAtBoundary_AutonomousReachesNoCharterUnit 断言 ③ 的可达性升级：边界结算 settleConsentsAtBoundary
// 即便对**无离线宪章**的 A（只要有 pending 同意请求）也会过归因管线自治回应——A 对 B 有显著关系 → 接受应用效果。
// （修原先「仅遍历有 SocialMandates 的 UnitCharters」导致升级后的归因管线对无宪章单位不可达的缺口。）
func TestSettleConsentsAtBoundary_AutonomousReachesNoCharterUnit(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	cgSeedWorldAndUnit(t, ctx, service, repo, "w1", "a", "b")
	// b 无任何宪章，但对 a 有强敌视（rivalry 归一 0.7）→ 归因成立。
	cgSeedRelationRow(t, service, "b", "a", 0, 0, 0, 7)

	if _, err := service.RecordSevenInteraction(ctx, "w1", "a", "b", InteractionMarriage, 8); err != nil {
		t.Fatalf("marriage: %v", err)
	}

	// 边界结算：state 无 UnitCharters，但 b 有 pending → 仍应被自治回应路径覆盖并接受。
	state := &State{ID: "w1"}
	service.settleConsentsAtBoundary(ctx, state)

	if pend, _ := service.ListPendingConsents(ctx, "b"); len(pend) != 0 {
		t.Fatalf("无宪章但有源的 A 应在边界被自治接受，pending 应清空，得 %d 条", len(pend))
	}
	if aff := relAxis(t, service, "affection", "a", "b"); aff != 5 {
		t.Fatalf("边界自治接受后应应用联姻 affection 增量 5，得 %v", aff)
	}
}

// cgSeedUnitInSession 落一个属指定 session 的单位（作用域门 JOIN 的是 units.session_id；consent_request 自带 world_id，
// 不依赖 unit.world_id 列，故此处只设 session_id）。与 cgSeedWorldAndUnit 不同：sessionID 可与 worldID 不同——
// 专为「同一 world 下分属不同 session 的单位」作用域测试（验证边界结算只替本 session 所辖 target 回应）。
func cgSeedUnitInSession(t *testing.T, ctx context.Context, repo *unit.Repository, sessionID, id string) {
	t.Helper()
	rec := unit.BootstrapRecord(1, sessionID, "player", id)
	rec.ID = id
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save unit %s in session %s: %v", id, sessionID, err)
	}
}

// TestSettleConsentsAtBoundary_DoesNotWriteOtherSessionRelations 是本次 HIGH 修复的核心回归（跨玩家硬不变量）：
// 两局共享同一 world，离线 A(a1)/发起方 B(b1) 都属 session-1；A 对 B 有显著关系（若被处理必接受）。
// session-2 推进部署边界（settleConsentsAtBoundary，State{ID:"s2"}）——它既不辖 A 也不辖 B——**绝不得**替 session-1 的
// 离线 A 自治接受、绝不得直写发起方 B 一侧 (b1→a1) relations。仿 blood_feud_test 的「绝不直写他人一侧」断言。
//
// 修复前：listPendingConsentTargets 全库扫 pending（无 session/world 谓词），session-2 边界会捞到 a1 的 pending、
// 经 autoResolveConsentsByCharter→ResolveConsentRequest(true)→applyRelationShiftTx 直写 source=b1 的 relations，越界污染 session-1。
func TestSettleConsentsAtBoundary_DoesNotWriteOtherSessionRelations(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, service.db, world.World{ID: "w1", Name: "共享世界"}); err != nil {
		_ = err // 复用容忍重复建
	}
	// A(a1) 与 B(b1) 都属 session-1（同一 world w1）。
	cgSeedUnitInSession(t, ctx, repo, "s1", "a1")
	cgSeedUnitInSession(t, ctx, repo, "s1", "b1")
	// session-2 有自己的无关单位 x2（属 s2），但不辖 a1/b1。
	cgSeedUnitInSession(t, ctx, repo, "s2", "x2")
	// a1（离线 A）对 b1（B）有强敌视（rivalry 归一 0.7 ≥ 0.3）→ 若被处理则归因成立、必自治接受。
	cgSeedRelationRow(t, service, "a1", "b1", 0, 0, 0, 7)

	// B(b1) 对 A(a1) 发起联姻（层3 requires_consent）→ 建一条 target=a1 的 pending。
	res, err := service.RecordSevenInteraction(ctx, "w1", "b1", "a1", InteractionMarriage, 8)
	if err != nil || res.ConsentRequestID == "" {
		t.Fatalf("联姻应建 pending，得 %+v err=%v", res, err)
	}

	// ★ 关键：session-2 推进边界——既不辖 a1 也不辖 b1，绝不得替 session-1 接受/写关系。
	service.settleConsentsAtBoundary(ctx, &State{ID: "s2"})

	// ① 发起方 B 一侧 (b1→a1) relations 绝不被 session-2 边界写入（跨玩家硬不变量）。
	var n int
	if err := service.db.QueryRow(
		`SELECT COUNT(*) FROM relations WHERE source_unit_id='b1' AND target_unit_id='a1'`).Scan(&n); err != nil {
		t.Fatalf("查 b1→a1 关系失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("session-2 边界绝不得直写发起方 B 一侧 (b1→a1) relations（跨玩家硬不变量），却发现 %d 行", n)
	}
	// ② consent_request 仍 pending（未被越界 resolve）。
	if pend, _ := service.ListPendingConsents(ctx, "a1"); len(pend) != 1 {
		t.Fatalf("session-2 不辖 a1，pending 应原封不动留 1 条，得 %d 条", len(pend))
	}

	// ③ 正向对照：a1 的合法 owner session-1 推进边界 → 才会自治接受并写本侧 (b1→a1) 关系。
	service.settleConsentsAtBoundary(ctx, &State{ID: "s1"})
	if pend, _ := service.ListPendingConsents(ctx, "a1"); len(pend) != 0 {
		t.Fatalf("session-1（a1 的 owner）边界应自治接受、清空 pending，得 %d 条", len(pend))
	}
	if aff := relAxis(t, service, "affection", "b1", "a1"); aff != 5 {
		t.Fatalf("owner session-1 自治接受后应写本侧联姻 affection 增量 5，得 %v", aff)
	}
}

// TestListPendingConsentTargets_SessionScoped 断言作用域门：listPendingConsentTargets 只返回 target 属指定 session 的 pending；
// 空 scopeSessionID 一律返回空（无作用域=不替任何人结算，保守）。
func TestListPendingConsentTargets_SessionScoped(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, service.db, world.World{ID: "w1", Name: "共享世界"}); err != nil {
		_ = err
	}
	cgSeedUnitInSession(t, ctx, repo, "s1", "a1") // target 属 s1
	cgSeedUnitInSession(t, ctx, repo, "s1", "b1")
	cgSeedUnitInSession(t, ctx, repo, "s2", "a2") // target 属 s2
	cgSeedUnitInSession(t, ctx, repo, "s2", "b2")

	if _, err := service.RecordSevenInteraction(ctx, "w1", "b1", "a1", InteractionMarriage, 8); err != nil {
		t.Fatalf("s1 联姻: %v", err)
	}
	if _, err := service.RecordSevenInteraction(ctx, "w1", "b2", "a2", InteractionMarriage, 8); err != nil {
		t.Fatalf("s2 联姻: %v", err)
	}

	// s1 作用域只见 a1（不见别局 a2）。
	s1Targets := service.listPendingConsentTargets(ctx, "s1")
	if len(s1Targets) != 1 || s1Targets[0] != "a1" {
		t.Fatalf("s1 作用域应只见自辖 target a1，得 %v", s1Targets)
	}
	// s2 作用域只见 a2。
	s2Targets := service.listPendingConsentTargets(ctx, "s2")
	if len(s2Targets) != 1 || s2Targets[0] != "a2" {
		t.Fatalf("s2 作用域应只见自辖 target a2，得 %v", s2Targets)
	}
	// 空作用域 → 空（保守不替任何人结算）。
	if got := service.listPendingConsentTargets(ctx, ""); len(got) != 0 {
		t.Fatalf("空 scopeSessionID 应返回空切片（无作用域不结算），得 %v", got)
	}
}

// TestExpireStaleConsents_ContestedScopedFallbackIsolation 断言层2 超时兜底的作用域隔离（HIGH 第二触发路径）：
// 经 expireStaleConsentsScoped 传 session-2 作用域跑超时兜底，绝不替 session-1 所辖 A 走层2 自治接受而越界写 B 侧关系；
// 但仍全局把超 TTL 的 pending 置 expired（清理不写他人 relations，可保持全局）。owner session-1 作用域兜底才接受应用效果。
func TestExpireStaleConsents_ContestedScopedFallbackIsolation(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	if _, err := world.Create(ctx, service.db, world.World{ID: "w1", Name: "共享世界"}); err != nil {
		_ = err
	}
	cgSeedUnitInSession(t, ctx, repo, "s1", "a1")
	cgSeedUnitInSession(t, ctx, repo, "s1", "b1")
	cgSeedUnitInSession(t, ctx, repo, "s2", "x2")
	// a1 对 b1 有强敌视 → 层2 兜底若被处理则有源接受。
	cgSeedRelationRow(t, service, "a1", "b1", 0, 0, 0, 7)

	// 反目 = 层2 contested，b1→a1（target=a1 属 s1）。
	res, err := service.RecordSevenInteraction(ctx, "w1", "b1", "a1", InteractionFallout, 7)
	if err != nil || res.ConsentRequestID == "" {
		t.Fatalf("反目应建 pending，得 %+v err=%v", res, err)
	}

	// ★ session-2 作用域跑超时兜底：不得替 s1 的 a1 走层2 自治接受、不得写 b1→a1 关系。
	if _, err := service.expireStaleConsentsScoped(ctx, "9999-12-31 00:00:00", "s2"); err != nil {
		t.Fatalf("expire(s2 scope): %v", err)
	}
	var n int
	if err := service.db.QueryRow(
		`SELECT COUNT(*) FROM relations WHERE source_unit_id='b1' AND target_unit_id='a1'`).Scan(&n); err != nil {
		t.Fatalf("查 b1→a1 关系失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("session-2 作用域层2 兜底绝不得越界写 (b1→a1) relations，却发现 %d 行", n)
	}
	// 但全局 expire 仍把超 TTL 的 pending 清成 expired（不写他人 relations，可保持全局——避免别局 pending 永挂）。
	if pend, _ := service.ListPendingConsents(ctx, "a1"); len(pend) != 0 {
		t.Fatalf("超 TTL 的 pending 应被全局置 expired，得 %d 条仍 pending", len(pend))
	}
}

// TestConsentResponseAttribution_Pure 断言归因合成纯函数：显著关系→relation 前因；无关系有压力→pressure 前因；
// 皆无→无源（false）。这是 ③ 自治回应「过归因」的核心，纯逻辑可测。
func TestConsentResponseAttribution_Pure(t *testing.T) {
	// 显著关系 → relation 前因。
	snapRel := decision.Snapshot{
		Relations: map[string]decision.RelationAxes{"B": {Rivalry: 0.6}},
	}
	if attr, ok := consentResponseAttribution("B", snapRel); !ok || attr.Primary.Kind != decision.CauseRelation {
		t.Fatalf("显著关系应得 relation 前因，得 ok=%v kind=%v", ok, attr.Primary.Kind)
	}

	// 无显著关系但有现实压力（威胁）→ pressure 前因。
	snapPress := decision.Snapshot{
		Relations: map[string]decision.RelationAxes{"B": {Trust: 0.1}}, // 0.1 < 0.3 不显著
		Pressure:  decision.PressureFlags{Threat: true},
	}
	if attr, ok := consentResponseAttribution("B", snapPress); !ok || attr.Primary.Kind != decision.CausePressure {
		t.Fatalf("无显著关系有压力应得 pressure 前因，得 ok=%v kind=%v", ok, attr.Primary.Kind)
	}

	// 皆无 → 无源。
	snapNone := decision.Snapshot{Relations: map[string]decision.RelationAxes{"B": {Trust: 0.1}}}
	if _, ok := consentResponseAttribution("B", snapNone); ok {
		t.Fatalf("既无显著关系也无压力应判无源（false）")
	}
}
