package session

// 文件说明：双向世界化入向探针 + 锚自动 upsert + 反向密度 + 破圈判定的集成测试（对真实 SQLite）。
// 覆盖：①入向反查命中阈值（≥0.35 才扇出）；②冷却（同 actor,target,code 每日 ≤1）；③付费不进（钱包不影响扇出）；
// ④确定性（同输入逐结果一致）；⑤AnchorDensityByRef 反向密度单调；⑥IsZeroAnchorSource 破圈判定；⑦四类 upsert helper + ReasonAnchorLit 留痕。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

// withInbound 临时开启 QUNXIANG_WORLDIZE_INBOUND（t.Setenv 自动在测试结束还原）。
func withInbound(t *testing.T) {
	t.Helper()
	t.Setenv("QUNXIANG_WORLDIZE_INBOUND", "on")
}

// processEventCount 统计某 reason-code 的流程事件条数。
func processEventCount(t *testing.T, service *Service, reason events.ReasonCode) int {
	t.Helper()
	var n int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM events WHERE reason_code = ?`, string(reason)).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", reason, err)
	}
	return n
}

// TestWorldizeInbound_HitsCarerAboveGate 验证：玩家角色背叛 target 后，强烈在乎 target 的旁人（有高权债仇爱锚）被入向命中，
// 落 PROPAGATION_INBOUND + WORLDIZE_OUTBOUND 留痕；与 target 毫无关联的人不被命中。
func TestWorldizeInbound_HitsCarerAboveGate(t *testing.T) {
	withInbound(t)
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// actor（玩家角色）、target（被背叛者）、carer（强烈在乎 target 的旁人）、stranger（无关）。
	actor := unit.BootstrapRecord(1, "s1", "player", "叛者")
	target := unit.BootstrapRecord(4, "s1", "player", "被背叛者")
	carer := unit.BootstrapRecord(2, "s1", "player", "挚友")
	stranger := unit.BootstrapRecord(3, "s1", "player", "路人")
	for _, r := range []unit.Record{actor, target, carer, stranger} {
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("存角色失败: %v", err)
		}
	}
	// carer 以 target 为债仇爱锚（强权重、不衰减），故 target 的事一定牵动 carer。
	if err := service.UpsertAnchor(ctx, carer.ID, relevance.DebtGrudgeLove, target.ID, 1.0, "生死之交", 0); err != nil {
		t.Fatalf("upsert 锚失败: %v", err)
	}

	state := &State{ID: "s1", PlayerFactionID: "player"}
	surfaced, err := service.WorldizeInbound(ctx, state, actor.ID, target.ID, events.ReasonRelationBetray)
	if err != nil {
		t.Fatalf("入向反查出错: %v", err)
	}
	if surfaced < 1 {
		t.Fatalf("强烈在乎 target 的 carer 应被惊动，surfaced=%d", surfaced)
	}
	if n := processEventCount(t, service, events.ReasonWorldizeOutbound); n != 1 {
		t.Fatalf("应有 1 条出向源头留痕，得到 %d", n)
	}
	if n := processEventCount(t, service, events.ReasonPropagationInbound); n < 1 {
		t.Fatalf("应有 ≥1 条入向探针留痕，得到 %d", n)
	}
	_ = db
}

// TestWorldizeInbound_CooldownPerDay 验证：同 (actor,target,code) 当天第二次入向被冷却挡下（不再写第二条 WORLDIZE_OUTBOUND）。
func TestWorldizeInbound_CooldownPerDay(t *testing.T) {
	withInbound(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := unit.BootstrapRecord(1, "s1", "player", "叛者")
	target := unit.BootstrapRecord(4, "s1", "player", "被背叛者")
	carer := unit.BootstrapRecord(2, "s1", "player", "挚友")
	for _, r := range []unit.Record{actor, target, carer} {
		_ = repo.Save(ctx, r)
	}
	_ = service.UpsertAnchor(ctx, carer.ID, relevance.DebtGrudgeLove, target.ID, 1.0, "挚友", 0)

	state := &State{ID: "s1", PlayerFactionID: "player"}
	if _, err := service.WorldizeInbound(ctx, state, actor.ID, target.ID, events.ReasonRelationBetray); err != nil {
		t.Fatalf("首次入向出错: %v", err)
	}
	// 第二次：同 (actor,target,code)，应被冷却挡下。
	if _, err := service.WorldizeInbound(ctx, state, actor.ID, target.ID, events.ReasonRelationBetray); err != nil {
		t.Fatalf("二次入向出错: %v", err)
	}
	if n := processEventCount(t, service, events.ReasonWorldizeOutbound); n != 1 {
		t.Fatalf("冷却应使当天只 1 条出向留痕，得到 %d", n)
	}
}

// TestWorldizeInbound_NotPayToWin 验证：扇出与命中只由锚相关性决定，与 actor 钱包/付费无关——
// actor 钱包置高，命中人数与留痕条数不变（反 P2W 红线）。
func TestWorldizeInbound_NotPayToWin(t *testing.T) {
	withInbound(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	run := func(wallet int) int {
		actor := unit.BootstrapRecord(int64(10+wallet), "s1", "player", "富翁")
		actor.Status.Wallet = wallet
		target := unit.BootstrapRecord(int64(30+wallet), "s1", "player", "被背叛者")
		carer := unit.BootstrapRecord(int64(20+wallet), "s1", "player", "挚友")
		_ = repo.Save(ctx, actor)
		_ = repo.Save(ctx, target)
		_ = repo.Save(ctx, carer)
		_ = service.UpsertAnchor(ctx, carer.ID, relevance.DebtGrudgeLove, target.ID, 1.0, "挚友", 0)
		state := &State{ID: "s1", PlayerFactionID: "player"}
		surfaced, _ := service.WorldizeInbound(ctx, state, actor.ID, target.ID, events.ReasonRelationBetray)
		return surfaced
	}
	poor := run(0)
	rich := run(100000)
	if poor != rich {
		t.Fatalf("付费不进：穷(%d)/富(%d) 命中人数应相同", poor, rich)
	}
}

// TestWorldizeInbound_DisabledNoOp 验证 flag 关时入向 no-op（不写任何留痕）。
func TestWorldizeInbound_DisabledNoOp(t *testing.T) {
	t.Setenv("QUNXIANG_WORLDIZE_INBOUND", "false") // 显式关（默认已开），测关闭路径
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	carer := unit.BootstrapRecord(2, "s1", "player", "挚友")
	_ = repo.Save(ctx, carer)
	_ = service.UpsertAnchor(ctx, carer.ID, relevance.DebtGrudgeLove, "target_x", 1.0, "挚友", 0)
	state := &State{ID: "s1", PlayerFactionID: "player"}
	surfaced, err := service.WorldizeInbound(ctx, state, "actor_x", "target_x", events.ReasonRelationBetray)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if surfaced != 0 {
		t.Fatalf("flag 关应 0 扇出，得到 %d", surfaced)
	}
	if n := processEventCount(t, service, events.ReasonWorldizeOutbound); n != 0 {
		t.Fatalf("flag 关不应留痕，得到 %d", n)
	}
}

// TestWorldizeInbound_NonWhitelistNoOp 验证非白名单 reason-code（如纯生存消耗）不入向。
func TestWorldizeInbound_NonWhitelistNoOp(t *testing.T) {
	withInbound(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	carer := unit.BootstrapRecord(2, "s1", "player", "挚友")
	_ = repo.Save(ctx, carer)
	_ = service.UpsertAnchor(ctx, carer.ID, relevance.DebtGrudgeLove, "target_x", 1.0, "挚友", 0)
	state := &State{ID: "s1", PlayerFactionID: "player"}
	surfaced, _ := service.WorldizeInbound(ctx, state, "actor_x", "target_x", events.ReasonSurvivalHunger)
	if surfaced != 0 {
		t.Fatalf("非白名单 reason 不应入向，得到 %d", surfaced)
	}
}

// TestAnchorDensityByRef_Monotonic 验证反向密度：无人在乎→0；一人在乎→0<density<1；越多人在乎→密度越高。
func TestAnchorDensityByRef_Monotonic(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	if d := service.AnchorDensityByRef(ctx, "npc_a", ""); d != 0 {
		t.Fatalf("无人在乎应密度 0，得 %.3f", d)
	}
	if err := service.UpsertAnchor(ctx, "u1", relevance.DebtGrudgeLove, "npc_a", 1.0, "在乎", 0); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	d1 := service.AnchorDensityByRef(ctx, "npc_a", "")
	if d1 <= 0 || d1 >= 1 {
		t.Fatalf("一人强在乎应 0<density<1，得 %.3f", d1)
	}
	if err := service.UpsertAnchor(ctx, "u2", relevance.Relation, "npc_a", 1.0, "在乎", 0); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	d2 := service.AnchorDensityByRef(ctx, "npc_a", "")
	if d2 <= d1 {
		t.Fatalf("越多人在乎密度应越高：d2=%.3f d1=%.3f", d2, d1)
	}
	// kind 过滤：只数 relation 的应 < 跨所有类别。
	dRel := service.AnchorDensityByRef(ctx, "npc_a", relevance.Relation)
	if dRel <= 0 || dRel >= d2 {
		t.Fatalf("relation-only 密度应在 (0,d2) 内：dRel=%.3f d2=%.3f", dRel, d2)
	}
}

// TestAnchorDensityByRef_Deterministic 验证反向密度确定性（同状态重复查逐结果一致）。
func TestAnchorDensityByRef_Deterministic(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	_ = service.UpsertAnchor(ctx, "u1", relevance.DebtGrudgeLove, "npc_a", 0.7, "在乎", 0)
	_ = service.UpsertAnchor(ctx, "u2", relevance.Geo, "npc_a", 0.4, "同乡", 3)
	a := service.AnchorDensityByRef(ctx, "npc_a", "")
	b := service.AnchorDensityByRef(ctx, "npc_a", "")
	if a != b {
		t.Fatalf("确定性：两次查询应相等，得 %.6f vs %.6f", a, b)
	}
}

// TestIsZeroAnchorSource 验证破圈判定：无人以 actor/target 为锚→零锚来源；有人在乎→非零锚。
func TestIsZeroAnchorSource(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	// 陌生人事件：无人在乎 actor/target → 零锚来源。
	if !service.IsZeroAnchorSource(ctx, FateEvent{ActorID: "stranger", TargetID: "newplace"}) {
		t.Fatalf("无人在乎应判零锚来源")
	}
	// 有人以 target 为锚后 → 非零锚来源。
	_ = service.UpsertAnchor(ctx, "u1", relevance.Geo, "newplace", 0.5, "去过的地方", 3)
	if service.IsZeroAnchorSource(ctx, FateEvent{ActorID: "stranger", TargetID: "newplace"}) {
		t.Fatalf("有人在乎 target 后应判非零锚来源")
	}
}

// TestUpsertTypedAnchors_LitTrail 验证四类业务 upsert helper 都落对应锚 + 写 ReasonAnchorLit 留痕。
func TestUpsertTypedAnchors_LitTrail(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(1, "s1", "player", "主角")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}
	c := hero.ID

	if err := service.UpsertGoalAnchor(ctx, "s1", c, "", 0.8, "光复故土"); err != nil {
		t.Fatalf("goal: %v", err)
	}
	if err := service.UpsertDebtAnchor(ctx, "s1", c, "creditor", 0.6, "欠人情"); err != nil {
		t.Fatalf("debt: %v", err)
	}
	if err := service.UpsertGeoAnchor(ctx, "s1", c, "region_north", 0.5, "北境"); err != nil {
		t.Fatalf("geo: %v", err)
	}
	if err := service.UpsertLegacyAnchor(ctx, "s1", c, "heir_42", 1.0, "血脉"); err != nil {
		t.Fatalf("legacy: %v", err)
	}

	anchors := service.loadPersistentAnchors(ctx, c)
	kinds := map[relevance.AnchorKind]bool{}
	for _, a := range anchors {
		kinds[a.Kind] = true
	}
	for _, k := range []relevance.AnchorKind{relevance.Goal, relevance.DebtGrudgeLove, relevance.Geo, relevance.Legacy} {
		if !kinds[k] {
			t.Fatalf("应落 %s 锚", k)
		}
	}
	// 四次 helper 各写一条 ReasonAnchorLit 留痕。
	if n := processEventCount(t, service, events.ReasonAnchorLit); n != 4 {
		t.Fatalf("应有 4 条 ANCHOR_LIT 留痕，得到 %d", n)
	}
	// geo 锚半衰应为 3 天（离开后地理牵挂渐淡）；legacy 锚不衰减（0）。
	for _, a := range anchors {
		if a.Kind == relevance.Geo && a.HalfLifeDays != geoAnchorHalfLifeDays {
			t.Fatalf("geo 锚半衰应 %.1f，得 %.1f", geoAnchorHalfLifeDays, a.HalfLifeDays)
		}
		if a.Kind == relevance.Legacy && a.HalfLifeDays != 0 {
			t.Fatalf("legacy 锚应不衰减(0)，得 %.1f", a.HalfLifeDays)
		}
	}
}

// TestScanAndWorldizeInbound_OnlyPlayerActors 验证边界扫描只对玩家阵营角色的 worldizing 事件入向（非玩家 actor 的事件不扇出）。
func TestScanAndWorldizeInbound_OnlyPlayerActors(t *testing.T) {
	withInbound(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	playerActor := unit.BootstrapRecord(1, "s1", "player", "玩家角色")
	enemyActor := unit.BootstrapRecord(2, "s1", "enemy", "敌方角色")
	victim := unit.BootstrapRecord(4, "s1", "player", "受害者")
	carer := unit.BootstrapRecord(3, "s1", "player", "挚友")
	for _, r := range []unit.Record{playerActor, enemyActor, victim, carer} {
		_ = repo.Save(ctx, r)
	}
	_ = service.UpsertAnchor(ctx, carer.ID, relevance.DebtGrudgeLove, victim.ID, 1.0, "挚友", 0)

	// 预置两条 worldizing 事件：玩家 actor 的（应扇出）+ 敌方 actor 的（不扇出）。
	for _, actorID := range []string{playerActor.ID, enemyActor.ID} {
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID: "s1", OwnerUnitID: actorID, RelatedUnitID: victim.ID,
			Code: events.ReasonRelationBetray, Category: events.CategoryRelation,
		})
	}

	state := &State{ID: "s1", PlayerFactionID: "player"}
	total, err := service.ScanAndWorldizeInbound(ctx, state, []unit.Record{playerActor, enemyActor, victim, carer})
	if err != nil {
		t.Fatalf("边界扫描出错: %v", err)
	}
	if total < 1 {
		t.Fatalf("玩家 actor 的背叛应惊动 carer，total=%d", total)
	}
	// 只 1 条出向留痕（玩家 actor 的；敌方 actor 的不入向）。
	if n := processEventCount(t, service, events.ReasonWorldizeOutbound); n != 1 {
		t.Fatalf("应只对玩家 actor 入向（1 条出向留痕），得到 %d", n)
	}
}

// insertRawEvent 直接写一条 events 行（可指定 occurred_at 以控制 ORDER BY occurred_at DESC 的排序），供 M2 噪声/窗口测试。
func insertRawEvent(t *testing.T, service *Service, sessionID, actorID, targetID string, code events.ReasonCode, occurredAt time.Time) {
	t.Helper()
	_, err := service.db.Exec(
		`INSERT INTO events (id, session_id, actor_unit_id, target_unit_id, event_type, reason_code, payload_json, occurred_at, tick)
		 VALUES (?, ?, ?, ?, ?, ?, '{}', ?, 0)`,
		fmt.Sprintf("ev-%s-%d", code, occurredAt.UnixNano()),
		sessionID, actorID, targetID, string(events.CategoryCombat), string(code),
		occurredAt.UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		t.Fatalf("插入原始事件失败: %v", err)
	}
}

// TestRecentWorldizingEvents_WhitelistPushedToSQL 是 M2 回归：日内有 >scanLimit 条高噪声 combat/survival 行（occurred_at 更晚），
// 加 1 条**更早**的白名单 CombatDown。修复前 LIMIT 在白名单过滤之前 → 白名单事件被噪声挤出窗口而漏扫；
// 修复后白名单下推进 SQL（reason_code IN (...)），仍能扫到那条早的 CombatDown。
func TestRecentWorldizingEvents_WhitelistPushedToSQL(t *testing.T) {
	withInbound(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(1, "s1", "player", "玩家角色")
	victim := unit.BootstrapRecord(2, "s1", "player", "受害者")
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("存 actor: %v", err)
	}
	if err := repo.Save(ctx, victim); err != nil {
		t.Fatalf("存 victim: %v", err)
	}

	base := dayStartUTC().Add(1 * time.Hour) // 落在当天窗口内
	// ① 先写白名单 CombatDown，occurred_at 最早（base）——它会被 ORDER BY DESC 排到最末。
	insertRawEvent(t, service, "s1", actor.ID, victim.ID, events.ReasonCombatDown, base)
	// ② 再写 >scanLimit 条高噪声非白名单行（combat_damage / survival_hunger），occurred_at 都晚于白名单行。
	noise := inboundCandidateScanLimit + 8 // 64+8=72 条，足以把单条白名单事件挤出 LIMIT 64 窗口（若 LIMIT 在过滤之前）
	for i := 0; i < noise; i++ {
		code := events.ReasonCombatHit // 非白名单 combat 行（COMBAT_HIT，区别于白名单的 COMBAT_DOWN）
		if i%2 == 0 {
			code = events.ReasonSurvivalHunger
		}
		insertRawEvent(t, service, "s1", actor.ID, victim.ID, code, base.Add(time.Duration(i+1)*time.Minute))
	}

	playerSet := map[string]bool{actor.ID: true}
	got := service.recentWorldizingEvents(ctx, "s1", playerSet)
	// 修复后：白名单下推进 SQL，那条早的 CombatDown 仍被扫到。
	foundDown := false
	for _, e := range got {
		if e.ReasonCode == events.ReasonCombatDown && e.ActorID == actor.ID && e.TargetID == victim.ID {
			foundDown = true
		}
	}
	if !foundDown {
		t.Fatalf("M2 回归：高噪声日内的早 CombatDown 应仍被扫到扇出，got=%d 条均非该事件", len(got))
	}
}

// TestWorldizeInbound_PerRecipientDailyCap 是 M3 回归：同一 carer 被 13+ 桩涉同 hub（同 target）的 distinct 事件命中
// （distinct actor，绕开 per-(actor,target,code) 冷却）。修复前每桩事件独立投卡 → carer 当日收到 13+ 张；
// 修复后 per-recipient 当日入向闸（≤inboundDailyProbeBudget）生效 → 只收到 ≤budget 张 PROPAGATION_INBOUND。
func TestWorldizeInbound_PerRecipientDailyCap(t *testing.T) {
	withInbound(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	hub := unit.BootstrapRecord(900, "s1", "player", "枢纽人物")  // 热门 target，被多人涉及
	carer := unit.BootstrapRecord(901, "s1", "player", "牵挂者") // 唯一在乎 hub 的 carer
	if err := repo.Save(ctx, hub); err != nil {
		t.Fatalf("存 hub: %v", err)
	}
	if err := repo.Save(ctx, carer); err != nil {
		t.Fatalf("存 carer: %v", err)
	}
	if err := service.UpsertAnchor(ctx, carer.ID, relevance.DebtGrudgeLove, hub.ID, 1.0, "生死之交", 0); err != nil {
		t.Fatalf("upsert 锚: %v", err)
	}

	state := &State{ID: "s1", PlayerFactionID: "player"}
	// 13+ 个 distinct actor 各背叛同一 hub（每桩 distinct (actor,target,code)，冷却不挡；都命中同一 carer）。
	const distinctEvents = inboundDailyProbeBudget + 4 // 16 桩 > 12 预算
	for i := 0; i < distinctEvents; i++ {
		attacker := unit.BootstrapRecord(int64(1000+i), "s1", "player", fmt.Sprintf("涉事者%d", i))
		if err := repo.Save(ctx, attacker); err != nil {
			t.Fatalf("存 attacker%d: %v", i, err)
		}
		if _, err := service.WorldizeInbound(ctx, state, attacker.ID, hub.ID, events.ReasonRelationBetray); err != nil {
			t.Fatalf("第 %d 桩入向出错: %v", i, err)
		}
	}

	// carer 当日收到的 PROPAGATION_INBOUND（actor_unit_id=carer）应被 per-recipient 闸夹到 ≤ budget。
	var carerInbound int
	if err := service.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		carer.ID, string(events.ReasonPropagationInbound),
	).Scan(&carerInbound); err != nil {
		t.Fatalf("统计 carer 入向探针失败: %v", err)
	}
	if carerInbound > inboundDailyProbeBudget {
		t.Fatalf("M3 回归：同一 carer 当日入向应 ≤%d 张，得到 %d", inboundDailyProbeBudget, carerInbound)
	}
	if carerInbound != inboundDailyProbeBudget {
		t.Fatalf("M3：%d 桩 distinct 事件命中同一 carer，应正好投满预算 %d 张，得到 %d", distinctEvents, inboundDailyProbeBudget, carerInbound)
	}
}
