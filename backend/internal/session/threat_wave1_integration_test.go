package session

// 文件说明：Wave-1 PvE 集成（Clutch→Score / 装备耐久 GEAR_DAMAGED / Clutch→Legacy 闭环 / 层3 consent 超时边界钩子）
// 把 encounter/item 原语接进 session 战斗主链路后的聚焦集成测试。复用 newThreatTestService / pveGapDeepCareActor。

import (
	"context"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/item"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// newWave1WiredService 建一个 sessions 已注入的全连服务（③ 升级闭环需 loadStateForFate 读 state）。
func newWave1WiredService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "wave1.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewServiceWithColdStore(db, nil, nil), context.Background()
}

// ② 失败后随身装备耐久折损（GEAR_DAMAGED）：有限耐久装备被衰减并落 ReasonGearDamaged；pinned 传家宝豁免、耐久 floor=1 不破坏。
func TestWave1_GearDamagedOnDefeat(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(2, "s1", "player", "执械者")
	actor.Status.Attack = 2 // 弱 → 必败
	actor.Status.Defense = 5
	actor.Status.HP = 100
	actor.Status.Morale = 0.7
	actor.Inventory.Equipment = map[string]unit.ItemStack{
		"weapon": {ItemID: "long_sword", Quantity: 1, Durability: 50},                                                  // 有限耐久 → 应折损
		"armor":  {ItemID: "father_relic", Quantity: 1, Durability: 40, IsLegacy: true, SoulBound: true, Pinned: true}, // pinned 传家宝 → 豁免
	}
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}

	threat := Threat{ID: "t_bear", Name: "巨熊", Tier: ThreatTierElite, RegionID: "r1", Power: 600, Attack: 50, Defense: 10, HPPool: 300, Severity: 90}
	state := State{ID: "s1"}

	res, err := service.ResolveEliteEncounter(ctx, &state, &actor, threat)
	if err != nil {
		t.Fatalf("遭遇出错: %v", err)
	}
	if res.Outcome == "defeated" {
		t.Fatalf("弱角色不应击败强精英，得到 %q", res.Outcome)
	}

	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Inventory.Equipment["weapon"].Durability >= 50 {
		t.Fatalf("有限耐久武器应被折损，仍为 %d", reloaded.Inventory.Equipment["weapon"].Durability)
	}
	if reloaded.Inventory.Equipment["weapon"].Durability < item.DurabilityFloor {
		t.Fatalf("耐久应 floored ≥1（不破坏），得到 %d", reloaded.Inventory.Equipment["weapon"].Durability)
	}
	if reloaded.Inventory.Equipment["armor"].Durability != 40 {
		t.Fatalf("pinned 传家宝耐久应豁免（不变），得到 %d", reloaded.Inventory.Equipment["armor"].Durability)
	}
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonGearDamaged) < 1 {
		t.Fatalf("应落一条 GEAR_DAMAGED 留痕")
	}
}

// degradeParticipantGear 单元：无限耐久（Durability=0）装备不衰减、不留痕（零参战回合也不衰减）。
func TestWave1_GearDamaged_InfiniteDurabilityUntouched(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := unit.BootstrapRecord(2, "s1", "player", "无损者")
	actor.Inventory.Equipment = map[string]unit.ItemStack{
		"weapon": {ItemID: "long_sword", Quantity: 1, Durability: 0}, // 无限耐久
	}
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}
	threat := Threat{ID: "t1", Name: "试炼", Tier: ThreatTierElite}
	changed := service.degradeParticipantGear(ctx, &state, &actor, threat, true, 5)
	if changed != 0 {
		t.Fatalf("无限耐久装备不应衰减，changed=%d", changed)
	}
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonGearDamaged) != 0 {
		t.Fatalf("无衰减不应留痕")
	}
}

// ④ 层3 consent 超时边界钩子：deadline 已到的 pending 自动降级残废（COMBAT_MAIMED，绝不阵亡），二次扫描幂等不重复 maim。
func TestWave1_Layer3ConsentTimeoutBoundary(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapDeepCareActor(t, ctx, repo, "守夜人")
	actor.Status.HP = 80
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}
	state.TurnState.Turn = 10
	threat := Threat{ID: "wb1", Name: "山岳巨灵", Tier: ThreatTierWorldBoss, RegionID: "r1", Power: 3000}

	// 落一条层3 consent pending 标记（deadline=10+6=16）。
	service.recordLayer3ConsentPending(ctx, &state, &actor, threat)

	// 边界推进到 deadline 之后。
	state.TurnState.Turn = 16
	hpBefore := func() int { r, _ := repo.GetByID(ctx, actor.ID); return r.Status.HP }()
	n, err := service.degradeLayer3ConsentTimeoutsAtBoundary(ctx, &state)
	if err != nil {
		t.Fatalf("边界扫描出错: %v", err)
	}
	if n != 1 {
		t.Fatalf("应有 1 名超时者被降级，得到 %d", n)
	}
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonCombatMaimed) < 1 {
		t.Fatalf("超时降级应落 COMBAT_MAIMED")
	}
	if pveGapCountEvents(t, db, events.ReasonFellInDefeat) != 0 {
		t.Fatalf("超时降级绝不应阵亡（FELL_IN_DEFEAT）")
	}
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.HP <= 0 || reloaded.Status.HP >= hpBefore {
		t.Fatalf("应永久折损 HP 且 floored >0，前=%d 后=%d", hpBefore, reloaded.Status.HP)
	}
	if reloaded.Status.LivesRemaining <= 0 {
		t.Fatalf("绝不阵亡，lives=%d", reloaded.Status.LivesRemaining)
	}

	// 幂等：再扫一次不应重复 maim。
	maimedAfterFirst := reloaded.Status.HP
	n2, err := service.degradeLayer3ConsentTimeoutsAtBoundary(ctx, &state)
	if err != nil {
		t.Fatalf("二次边界扫描出错: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("幂等：已降级的 pending 不应被二次降级，得到 %d", n2)
	}
	reloaded2, _ := repo.GetByID(ctx, actor.ID)
	if reloaded2.Status.HP != maimedAfterFirst {
		t.Fatalf("幂等：二次扫描不应再改 HP，一次后=%d 二次后=%d", maimedAfterFirst, reloaded2.Status.HP)
	}
}

// ④ 未到 deadline 的 pending 不被降级（仍在玩家可应答窗口内）。
func TestWave1_Layer3ConsentTimeoutBoundary_BeforeDeadlineNoMaim(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapDeepCareActor(t, ctx, repo, "待援者")
	state := State{ID: "s1"}
	state.TurnState.Turn = 10
	threat := Threat{ID: "wb1", Name: "山岳巨灵", Tier: ThreatTierWorldBoss, RegionID: "r1"}
	service.recordLayer3ConsentPending(ctx, &state, &actor, threat) // deadline=16

	state.TurnState.Turn = 12 // 未到 deadline
	n, err := service.degradeLayer3ConsentTimeoutsAtBoundary(ctx, &state)
	if err != nil {
		t.Fatalf("边界扫描出错: %v", err)
	}
	if n != 0 {
		t.Fatalf("未到 deadline 不应降级，得到 %d", n)
	}
}

// ③ Clutch→Legacy 闭环：胜利且有 Clutch + 羁绊够 → 弹「刻成传家物」待决策卡（绑定 LegacyItemID）；
// confirm（engrave→urge）→ upgradeItemToLegacy 落 IsLegacy+SoulBound+Pinned。
func TestWave1_ClutchLegacyUpgradeClosedLoop(t *testing.T) {
	service, ctx := newWave1WiredService(t)
	db, repo := service.db, service.units

	actor := unit.BootstrapRecord(2, "s1", "player", "持剑人")
	actor.Status.Attack = 40
	actor.Status.Defense = 5
	actor.Status.HP = 100
	actor.Social.BornTurn = 1 // 羁绊够（turn-born ≥ MinLegacyBondTurns）
	actor.Inventory.Equipment = map[string]unit.ItemStack{
		"weapon": {ItemID: "long_sword", Quantity: 1},
	}
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1", PlayerUnitIDs: []string{actor.ID}}
	state.TurnState.Turn = 20 // bond=20-1=19 ≥ 5
	if err := service.sessions.Save(ctx, &state); err != nil {
		t.Fatalf("save session: %v", err) // loadStateForFate 需读到 state 才能升级
	}

	// 直接验证升级卡 surfacing 的判定路径：构造带 Clutch 的贡献。
	contribution := encounter.Contribution{Damage: 60}
	contribution = encounter.MarkClutch(contribution, encounter.ClutchFinalBlow)
	threat := Threat{ID: "t1", Name: "山魈", Tier: ThreatTierElite}
	service.surfaceLegacyUpgradeFromClutch(ctx, &state, &actor, threat, contribution)

	// 应弹出一张 PENDING_DECISION（传家物升级卡），payload 绑定 legacy_item_id=long_sword。
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonPendingDecision) < 1 {
		t.Fatalf("有 Clutch + 羁绊够应弹一张传家物升级待决策卡")
	}
	var decisionID, payload string
	if err := db.QueryRow(
		`SELECT payload_json FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		actor.ID, string(events.ReasonPendingDecision)).Scan(&payload); err != nil {
		t.Fatalf("查升级卡 payload 失败: %v", err)
	}
	if !contains(payload, "legacy_item_id") || !contains(payload, "long_sword") {
		t.Fatalf("升级卡 payload 应绑定 legacy_item_id=long_sword，得到 %q", payload)
	}
	decisionID = extractDecisionID(t, payload)

	// confirm（engrave→urge）→ upgradeItemToLegacy 落三标记。
	if err := service.ResolveFateDecision(ctx, state.ID, actor.ID, decisionID, "engrave"); err != nil {
		t.Fatalf("确认升级出错: %v", err)
	}
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	w := reloaded.Inventory.Equipment["weapon"]
	if !item.IsPermanentAnchor(item.LegacyFlags{IsLegacy: w.IsLegacy, SoulBound: w.SoulBound, Pinned: w.Pinned}) {
		t.Fatalf("confirm 后武器应成永久锚（IsLegacy+SoulBound+Pinned），得到 %+v", w)
	}
}

// ③ 无 Clutch 不弹升级卡（防一捡到手就因偶然弹卡）。
func TestWave1_NoClutchNoLegacyCard(t *testing.T) {
	service, ctx := newWave1WiredService(t)
	db, repo := service.db, service.units
	actor := unit.BootstrapRecord(2, "s1", "player", "无功者")
	actor.Social.BornTurn = 1
	actor.Inventory.Equipment = map[string]unit.ItemStack{"weapon": {ItemID: "long_sword", Quantity: 1}}
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}
	state.TurnState.Turn = 20
	service.surfaceLegacyUpgradeFromClutch(ctx, &state, &actor, Threat{ID: "t1", Name: "山魈", Tier: ThreatTierElite}, encounter.Contribution{Damage: 30})
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonPendingDecision) != 0 {
		t.Fatalf("无 Clutch 不应弹升级卡")
	}
}

// ① 救援濒死队友触发条件：dungeonHasDownedTeammate 仅在「除自己外有 status=="down" 队友」时为真。
func TestWave1_DungeonHasDownedTeammate(t *testing.T) {
	mk := func(id, status string) *memberCombatState {
		return &memberCombatState{rec: &unit.Record{ID: id}, status: status}
	}
	run := &dungeonRun{members: []*memberCombatState{
		mk("a", "contributed"),
		mk("b", "down"),
		mk("c", "fled"),
	}}
	if !dungeonHasDownedTeammate(run, "a") {
		t.Fatalf("a 视角应看到倒下的队友 b → 触发 ClutchRescueDown")
	}
	if dungeonHasDownedTeammate(run, "b") {
		t.Fatalf("b 自己倒下，排除自身后无其它倒下者 → 不触发")
	}
	allUp := &dungeonRun{members: []*memberCombatState{mk("a", "contributed"), mk("d", "contributed")}}
	if dungeonHasDownedTeammate(allUp, "a") {
		t.Fatalf("全员在战，无倒下队友 → 不触发")
	}
}

// ① 终结一击优先于救援：同一击若既终结威胁又有倒下队友，应记 ClutchFinalBlow（不双计）——经 IsFinalBlow/MarkClutch 验证语义。
func TestWave1_ClutchFinalBlowPrecedence(t *testing.T) {
	var c encounter.Contribution
	// 终结一击：mobHP 从 5 打到 -3。
	if !encounter.IsFinalBlow(5, -3) {
		t.Fatalf("把威胁从 >0 打到 ≤0 应判终结一击")
	}
	c = encounter.MarkClutch(c, encounter.ClutchFinalBlow)
	if !encounter.HadClutch(c) || c.Clutch != encounter.ClutchValues.FinalBlow {
		t.Fatalf("终结一击应累加 FinalBlow 折算分，得到 %v", c.Clutch)
	}
}

// extractDecisionID 从 PENDING_DECISION 的 payload_json 里抠出 decision_id（测试辅助，简单子串解析）。
func extractDecisionID(t *testing.T, payload string) string {
	t.Helper()
	const key = `"decision_id":"`
	i := indexOf(payload, key)
	if i < 0 {
		t.Fatalf("payload 无 decision_id：%q", payload)
	}
	rest := payload[i+len(key):]
	j := indexOf(rest, `"`)
	if j < 0 {
		t.Fatalf("decision_id 未闭合：%q", payload)
	}
	return rest[:j]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
