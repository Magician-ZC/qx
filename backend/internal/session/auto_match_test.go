package session

// 文件说明：撮合自动扫描的升级单元测试（对真实 SQLite，复用 newThreatTestService）。
// 覆盖事件耦合 §2.2 本轮升级：①region 就近择人（同主导 region 的地理近=满分）；②每日新绑定 ≤ autoMatchDailyBindCap 冷却；
// ③NPC 兜底（真人不足 floor 时占位补齐，玩家分不出）；④过期回收（expires_at 到点 status→expired + ANCHOR_DECAYED 留痕）；
// ⑤severity 定档（候选规模/关系密度 → consent 档）；⑥flag 默认关 no-op；⑦确定性可复现。

import (
	"context"
	"testing"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

// withAutoMatch 临时开启 QUNXIANG_AUTO_MATCH（t.Setenv 测试结束自动还原）。
func withAutoMatch(t *testing.T) {
	t.Helper()
	t.Setenv("QUNXIANG_AUTO_MATCH", "on")
}

// saveUnitsInRegion 存若干玩家阵营角色并给定 region（去规范化列经 SetUnitScope 写）。返回它们的 id。
func saveUnitsInRegion(t *testing.T, ctx context.Context, repo *unit.Repository, worldID, regionID string, names ...string) []unit.Record {
	t.Helper()
	out := make([]unit.Record, 0, len(names))
	for i, name := range names {
		r := unit.BootstrapRecord(int64(100+i), "s1", "player", name)
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("存角色 %s 失败: %v", name, err)
		}
		if regionID != "" {
			if err := repo.SetUnitScope(ctx, r.ID, worldID, regionID); err != nil {
				t.Fatalf("设 region 失败: %v", err)
			}
		}
		out = append(out, r)
	}
	return out
}

// socialObjectColumns 直接读回 region_id/severity/status/expires_at（socialobject.Get 不含这三列，故原生查）。
func socialObjectColumns(t *testing.T, service *Service, objectID string) (regionID string, severity int, status string, expiresAt string) {
	t.Helper()
	var rid, exp interface{}
	if err := service.db.QueryRow(
		`SELECT region_id, severity, status, expires_at FROM social_objects WHERE id = ?`, objectID,
	).Scan(&rid, &severity, &status, &exp); err != nil {
		t.Fatalf("读社会客体列失败: %v", err)
	}
	if s, ok := rid.(string); ok {
		regionID = s
	}
	if s, ok := exp.(string); ok {
		expiresAt = s
	}
	return regionID, severity, status, expiresAt
}

// soMemberIDs 读某社会客体的成员 unit_id 集合。
func soMemberIDs(t *testing.T, service *Service, objectID string) []string {
	t.Helper()
	rows, err := service.db.Query(`SELECT unit_id FROM social_object_members WHERE object_id = ? ORDER BY unit_id`, objectID)
	if err != nil {
		t.Fatalf("读成员失败: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan 成员失败: %v", err)
		}
		out = append(out, id)
	}
	return out
}

// TestScanAndMatch_FlagOffNoOp 验证 flag 关（默认）时整方法 no-op：零社会客体、零成员写入。
func TestScanAndMatch_FlagOffNoOp(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	// 不设 QUNXIANG_AUTO_MATCH（默认关）。
	t.Setenv("QUNXIANG_AUTO_MATCH", "off")

	units := saveUnitsInRegion(t, ctx, repo, "w1", "r1", "甲", "乙", "丙", "丁")
	state := &State{ID: "s1", WorldID: "w1", PlayerFactionID: "player"}
	state.TurnState.Turn = autoMatchEveryNTurns // 到周期，但 flag 关应 no-op
	service.scanAndMatch(ctx, state, units)

	var n int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM social_objects`).Scan(&n); err != nil {
		t.Fatalf("统计社会客体失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("flag 关时应零社会客体写入，得到 %d", n)
	}
}

// TestScanAndMatch_RegionStampAndGeoNear 验证：撮合写 region_id（候选群主导 region），且同主导 region 的候选地理近=满分入选。
func TestScanAndMatch_RegionStampAndGeoNear(t *testing.T) {
	withAutoMatch(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 四人全在 r-north → 主导 region 应为 r-north，社会客体 region_id 落 r-north。
	units := saveUnitsInRegion(t, ctx, repo, "w1", "r-north", "甲", "乙", "丙", "丁")
	state := &State{ID: "s1", WorldID: "w1", PlayerFactionID: "player"}
	state.TurnState.Turn = autoMatchEveryNTurns
	service.scanAndMatch(ctx, state, units)

	var objID string
	if err := service.db.QueryRow(`SELECT id FROM social_objects LIMIT 1`).Scan(&objID); err != nil {
		t.Fatalf("应建出一个社会客体: %v", err)
	}
	regionID, _, status, _ := socialObjectColumns(t, service, objID)
	if regionID != "r-north" {
		t.Fatalf("社会客体 region_id 应为主导 region r-north，得到 %q", regionID)
	}
	if status != "active" {
		t.Fatalf("新建社会客体应为 active，得到 %q", status)
	}

	// geoNearByRegion：同主导 region 满分、异区随质心距衰减。
	u := units[0]
	if g := geoNearByRegion(u, "r-north", "r-north", float64(u.Status.PositionQ), float64(u.Status.PositionR)); g != 1.0 {
		t.Fatalf("同主导 region 地理近应为 1.0，得到 %v", g)
	}
	if g := geoNearByRegion(u, "r-south", "r-north", float64(u.Status.PositionQ)+12, float64(u.Status.PositionR)); g >= 1.0 {
		t.Fatalf("异区 + 远离质心地理近应 <1.0，得到 %v", g)
	}
}

// TestScanAndMatch_DailyBindCap 验证每日新绑定 ≤ autoMatchDailyBindCap：当日已绑满的角色不再入候选。
func TestScanAndMatch_DailyBindCap(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	units := saveUnitsInRegion(t, ctx, repo, "w1", "r1", "甲")
	u := units[0]

	// 注入「当日已绑满」的 SOCIAL_OBJECT_BIND 留痕（恰好 autoMatchDailyBindCap 条）。
	for i := 0; i < autoMatchDailyBindCap; i++ {
		if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID: "w1", OwnerUnitID: u.ID, Code: events.ReasonSocialObjectBind, Category: events.CategoryFate,
			Payload: map[string]any{"object_id": "x"}, WorldID: "w1",
		}); err != nil {
			t.Fatalf("注入绑定留痕失败: %v", err)
		}
	}
	if !service.dailyBindExhausted(ctx, u.ID) {
		t.Fatalf("当日已绑 %d 条，应判为达上限", autoMatchDailyBindCap)
	}

	// 另一个零绑定角色应未达上限。
	fresh := unit.BootstrapRecord(999, "s1", "player", "乙")
	if err := repo.Save(ctx, fresh); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}
	if service.dailyBindExhausted(ctx, fresh.ID) {
		t.Fatalf("零绑定角色不应判为达上限")
	}

	// buildMatchCandidates 应把已达上限的 u 滤掉。
	state := &State{ID: "s1", WorldID: "w1", PlayerFactionID: "player"}
	cands := service.buildMatchCandidates(ctx, state, []unit.Record{u, fresh})
	for _, c := range cands {
		if c.UnitID == u.ID {
			t.Fatalf("已达每日上限的角色不应入候选")
		}
	}
}

// TestScanAndMatch_NPCBackfill 验证 NPC 兜底：仅两名真人但 floor 要求成局，撮合后成员被 NPC 占位补齐到 floor，且 NPC id 玩家分不出（无 units 行）。
func TestScanAndMatch_NPCBackfill(t *testing.T) {
	withAutoMatch(t)
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	// 仅一名真人能入候选（撮合需 ≥2 候选才进 MatchIntoSocialObject，故这里用 backfillWithNPC 直接验兜底）。
	objID := "so_test_backfill"
	if _, err := service.db.ExecContext(ctx,
		`INSERT INTO social_objects (id, world_id, kind, label, status, created_at) VALUES (?,?,?,?,?,?)`,
		objID, "w1", autoMatchKind, "野外同行·0", "active", time.Now().UTC().Format(autoMatchTimeLayout),
	); err != nil {
		t.Fatalf("建测试客体失败: %v", err)
	}
	real := []string{"real-1"} // 真人不足 floor
	got := service.backfillWithNPC(ctx, objID, "w1", 0, real, autoMatchSlots)
	if len(got) < autoMatchBackfillFloor {
		t.Fatalf("NPC 兜底后成员应补齐到 floor=%d，得到 %d", autoMatchBackfillFloor, len(got))
	}
	// 补进来的应是 NPC 占位 id（npc_so_ 前缀），且其在 members 表里有行（玩家分不出对方是 NPC 还是另一个玩家）。
	members := soMemberIDs(t, service, objID)
	npcCount := 0
	for _, m := range members {
		if len(m) >= 7 && m[:7] == "npc_so_" {
			npcCount++
			// NPC 不应是任何真实 units 行。
			var n int
			if err := service.db.QueryRow(`SELECT COUNT(*) FROM units WHERE id = ?`, m).Scan(&n); err != nil {
				t.Fatalf("查 units 失败: %v", err)
			}
			if n != 0 {
				t.Fatalf("NPC 占位不应是真实 units 行")
			}
		}
	}
	if npcCount < 1 {
		t.Fatalf("应至少补进一名 NPC 占位成员，members=%v", members)
	}

	// 真人 ≥ floor 时不补 NPC。
	got2 := service.backfillWithNPC(ctx, "so_other", "w1", 0, []string{"a", "b"}, autoMatchSlots)
	if len(got2) != 2 {
		t.Fatalf("真人达 floor 时不应补 NPC，得到 %d", len(got2))
	}
}

// TestScanAndMatch_NPCBackfillDeterministic 验证 NPC 占位 id 确定性可复现（同 objectID/epoch/序号 → 同 id）。
func TestScanAndMatch_NPCBackfillDeterministic(t *testing.T) {
	a := npcBackfillID("obj-x", 3, 0)
	b := npcBackfillID("obj-x", 3, 0)
	if a != b {
		t.Fatalf("同输入 NPC id 应一致：%q vs %q", a, b)
	}
	if npcBackfillID("obj-x", 3, 0) == npcBackfillID("obj-x", 3, 1) {
		t.Fatalf("不同序号应产不同 NPC id")
	}
	if npcBackfillID("obj-x", 3, 0) == npcBackfillID("obj-y", 3, 0) {
		t.Fatalf("不同 objectID 应产不同 NPC id")
	}
}

// TestScanAndMatch_ExpiryReclaim 验证过期回收：expires_at 已过的 active 客体被翻成 expired，并对真人成员落 ANCHOR_DECAYED；NPC 成员不落留痕。
func TestScanAndMatch_ExpiryReclaim(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	u := saveUnitsInRegion(t, ctx, repo, "w1", "r1", "甲")[0]

	// 一个已过期（expires_at 在过去）的 active 客体 + 一个真人成员 + 一个 NPC 成员。
	pastExpiry := time.Now().UTC().Add(-1 * time.Hour).Format(autoMatchTimeLayout)
	objID := "so_expired"
	if _, err := service.db.ExecContext(ctx,
		`INSERT INTO social_objects (id, world_id, kind, label, status, created_at, expires_at) VALUES (?,?,?,?,?,?,?)`,
		objID, "w1", autoMatchKind, "野外同行·旧", "active", time.Now().UTC().Format(autoMatchTimeLayout), pastExpiry,
	); err != nil {
		t.Fatalf("建过期客体失败: %v", err)
	}
	for _, mid := range []string{u.ID, "npc_so_deadbeef"} {
		if _, err := service.db.ExecContext(ctx,
			`INSERT INTO social_object_members (object_id, unit_id, score, joined_at) VALUES (?,?,?,?)`,
			objID, mid, 0.5, time.Now().UTC().Format(autoMatchTimeLayout),
		); err != nil {
			t.Fatalf("绑成员失败: %v", err)
		}
	}

	// 一个未过期的 active 客体（不应被回收）。
	futureExpiry := time.Now().UTC().Add(24 * time.Hour).Format(autoMatchTimeLayout)
	if _, err := service.db.ExecContext(ctx,
		`INSERT INTO social_objects (id, world_id, kind, label, status, created_at, expires_at) VALUES (?,?,?,?,?,?,?)`,
		"so_fresh", "w1", autoMatchKind, "野外同行·新", "active", time.Now().UTC().Format(autoMatchTimeLayout), futureExpiry,
	); err != nil {
		t.Fatalf("建未过期客体失败: %v", err)
	}

	service.reclaimExpiredSocialObjects(ctx, "w1")

	_, _, status, _ := socialObjectColumns(t, service, objID)
	if status != "expired" {
		t.Fatalf("过期客体应被翻成 expired，得到 %q", status)
	}
	_, _, freshStatus, _ := socialObjectColumns(t, service, "so_fresh")
	if freshStatus != "active" {
		t.Fatalf("未过期客体应保持 active，得到 %q", freshStatus)
	}

	// 真人成员应有 ANCHOR_DECAYED 留痕；NPC 成员不应有（它不是任何玩家的角色）。
	var decayForReal, decayForNPC int
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM events WHERE reason_code = ? AND actor_unit_id = ?`,
		string(events.ReasonAnchorDecayed), u.ID).Scan(&decayForReal); err != nil {
		t.Fatalf("统计真人 ANCHOR_DECAYED 失败: %v", err)
	}
	if decayForReal < 1 {
		t.Fatalf("过期回收应对真人成员落 ANCHOR_DECAYED 留痕，得到 %d", decayForReal)
	}
	if err := service.db.QueryRow(`SELECT COUNT(*) FROM events WHERE reason_code = ? AND actor_unit_id = ?`,
		string(events.ReasonAnchorDecayed), "npc_so_deadbeef").Scan(&decayForNPC); err != nil {
		t.Fatalf("统计 NPC ANCHOR_DECAYED 失败: %v", err)
	}
	if decayForNPC != 0 {
		t.Fatalf("NPC 占位成员不应有 ANCHOR_DECAYED 留痕，得到 %d", decayForNPC)
	}
}

// TestAutoMatchSeverity 验证 severity 定档：成员规模/关系密度 → consent 档（层1/2/3）。
func TestAutoMatchSeverity(t *testing.T) {
	// 小规模浅羁绊 → 层1（unilateral）。
	low := autoMatchSeverity(2, []MatchCandidate{{RelationIntersect: 0.1}, {RelationIntersect: 0.05}})
	if low != 1 {
		t.Fatalf("小规模浅羁绊 severity 应为 1，得到 %d", low)
	}
	if ConsentTierForSocialObject(low) != relevance.Unilateral {
		t.Fatalf("severity 1 应映射 unilateral 档")
	}
	// 中等规模 → 层2（contested）。
	mid := autoMatchSeverity(3, []MatchCandidate{{RelationIntersect: 0.1}})
	if mid != 2 {
		t.Fatalf("中等规模 severity 应为 2，得到 %d", mid)
	}
	if ConsentTierForSocialObject(mid) != relevance.Contested {
		t.Fatalf("severity 2 应映射 contested 档")
	}
	// 大规模/深羁绊 → 层3（requires_consent）。
	high := autoMatchSeverity(4, []MatchCandidate{{RelationIntersect: 0.9}})
	if high != 3 {
		t.Fatalf("大规模 severity 应为 3，得到 %d", high)
	}
	if ConsentTierForSocialObject(high) != relevance.RequiresConsent {
		t.Fatalf("severity 3 应映射 requires_consent 档")
	}
	// 深羁绊但小规模也升档（avgRel ≥ 0.6）。
	deep := autoMatchSeverity(2, []MatchCandidate{{RelationIntersect: 0.7}, {RelationIntersect: 0.7}})
	if deep != 3 {
		t.Fatalf("深羁绊（avgRel≥0.6）应升到 severity 3，得到 %d", deep)
	}
}

// TestScanAndMatch_EndToEndStampsSeverityAndExpiry 验证端到端：撮合成局后 severity ∈ [1,3] 且 expires_at 落库（非空、在未来）。
func TestScanAndMatch_EndToEndStampsSeverityAndExpiry(t *testing.T) {
	withAutoMatch(t)
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	units := saveUnitsInRegion(t, ctx, repo, "w1", "r1", "甲", "乙", "丙", "丁")
	state := &State{ID: "s1", WorldID: "w1", PlayerFactionID: "player"}
	state.TurnState.Turn = autoMatchEveryNTurns
	service.scanAndMatch(ctx, state, units)

	var objID string
	if err := service.db.QueryRow(`SELECT id FROM social_objects LIMIT 1`).Scan(&objID); err != nil {
		t.Fatalf("应建出社会客体: %v", err)
	}
	_, severity, status, expiresAt := socialObjectColumns(t, service, objID)
	if severity < 1 || severity > 3 {
		t.Fatalf("severity 应落 [1,3]，得到 %d", severity)
	}
	if status != "active" {
		t.Fatalf("新客体应 active，得到 %q", status)
	}
	if expiresAt == "" {
		t.Fatalf("expires_at 应被落库（非空）")
	}
	if expiresAt <= time.Now().UTC().Format(autoMatchTimeLayout) {
		t.Fatalf("expires_at 应在未来，得到 %q", expiresAt)
	}
}
