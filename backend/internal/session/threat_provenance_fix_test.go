package session

// 文件说明：PvE 三缺陷修复回归测试（对真实 SQLite + 纯函数）：
//   M4 provenance 闸——排除可能由本次战斗自产的 Injury/Threat 压力位，杜绝 down/fled 败北被「战斗自身伤势」自满足（橡皮图章）。
//   H2 producer 侧——排他仲裁胜负经 EmitProcessEvent 落库（CROSS_CONTEST_WIN/LOSE + Scope.WorldID/Tick），修审计假阴。
// 前缀统一 provFix*/contestRec* 等，避免与既有威胁/命运测试撞名。

import (
	"context"
	"database/sql"
	"testing"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// ---- M4：provenance 闸排除可能由本次战斗自产的 Injury/Threat（纯函数，确定性、无 DB）----

// 仅有 Injury（战后残血 HP<25 自产）→ 不再算可解析前因（修橡皮图章）。Threat（撞见威胁=InCombat）同理。
func TestProvFix_SnapshotHasResolvableCause_CombatSelfInflictedExcluded(t *testing.T) {
	onlyInjury := decision.Snapshot{Pressure: decision.PressureFlags{Injury: true}}
	if snapshotHasResolvableCause(onlyInjury) {
		t.Fatalf("战斗自产的 Injury 不应被认作可解析前因（橡皮图章）")
	}
	onlyThreat := decision.Snapshot{Pressure: decision.PressureFlags{Threat: true}}
	if snapshotHasResolvableCause(onlyThreat) {
		t.Fatalf("战斗自产的 Threat(InCombat) 不应被认作可解析前因")
	}
	injuryAndThreat := decision.Snapshot{Pressure: decision.PressureFlags{Injury: true, Threat: true}}
	if snapshotHasResolvableCause(injuryAndThreat) {
		t.Fatalf("Injury+Threat 皆战斗自产，仍不应自满足前因")
	}
}

// 战前既有压力（Hunger/Fatigue/Debt）仍算可解析前因——修复绝不误杀真实的战前前因。
func TestProvFix_SnapshotHasResolvableCause_PreCombatPressureStillCounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    decision.PressureFlags
	}{
		{"饥饿", decision.PressureFlags{Hunger: true}},
		{"力竭", decision.PressureFlags{Fatigue: true}},
		{"负债", decision.PressureFlags{Debt: true}},
	} {
		if !snapshotHasResolvableCause(decision.Snapshot{Pressure: tc.p}) {
			t.Fatalf("战前既有压力(%s)应仍算可解析前因", tc.name)
		}
	}
}

// dominantProvenanceCauseZH 对仅 Injury/Threat 的快照返回空串（与 snapshotHasResolvableCause 一把尺）。
func TestProvFix_DominantProvenanceCauseZH_NoCombatSelfInflicted(t *testing.T) {
	if got := dominantProvenanceCauseZH(decision.Snapshot{Pressure: decision.PressureFlags{Injury: true}}); got != "" {
		t.Fatalf("仅 Injury(战斗自产)应无归因句，得到 %q", got)
	}
	if got := dominantProvenanceCauseZH(decision.Snapshot{Pressure: decision.PressureFlags{Threat: true}}); got != "" {
		t.Fatalf("仅 Threat(战斗自产)应无归因句，得到 %q", got)
	}
	// 战前 Debt 仍应派生归因句。
	if got := dominantProvenanceCauseZH(decision.Snapshot{Pressure: decision.PressureFlags{Debt: true}}); got == "" {
		t.Fatalf("战前 Debt 应派生归因句")
	}
}

// provFixNoCauseActor 造一名「无任何战前前因」的角色：人格全 0.5、无记忆/红线/关系、Hunger=100、Fatigue=0、Wallet>0。
func provFixNoCauseActor(t *testing.T, ctx context.Context, repo *unit.Repository, name string) unit.Record {
	t.Helper()
	actor := unit.BootstrapRecord(2, "s1", "player", name)
	// 人格全部夷平到 0.5：无任何显著人格前因（|v-0.5| < 0.25）。
	actor.Personality = unit.Personality{
		Courage: 0.5, Loyalty: 0.5, Aggression: 0.5, Prudence: 0.5,
		Sociability: 0.5, Integrity: 0.5, Stability: 0.5, Ambition: 0.5,
	}
	actor.Status.HP = 100
	actor.Status.Hunger = 100 // 不饿（Hunger 压力位 false）
	actor.Status.Fatigue = 0  // 不疲（Fatigue 压力位 false）
	actor.Status.Wallet = 100 // 有钱（Debt 压力位 false）
	actor.Status.InCombat = false
	actor.Status.Attack = 2 // 弱：必败被打 down
	actor.Status.Defense = 1
	actor.Status.Morale = 0.7
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存无前因角色失败: %v", err)
	}
	return actor
}

// M4 集成：跑完整 down 结局（field_boss 把 HP 打到 0）。角色无任何战前前因，field_boss 候选层2 经 provenance 闸
// 被降到层1（绝不无源致残）。修复前——战后残血 HP<25→Injury=true→snapshotHasResolvableCause 自满足→层2 误落地。
func TestProvFix_FieldBossDownNoCause_DowngradedToLayer1(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := provFixNoCauseActor(t, ctx, repo, "无依")

	// turn≥3 → PenaltyCap(daysAlive) ≥ 2：确保 DegradePenalty 不会先把候选层2 夹到1，
	// 让「层2→层1」唯一来源是 provenance 闸（隔离被测路径）。
	state := State{ID: "s1"}
	state.TurnState.Turn = 5

	// 强 field_boss：一击秒杀。Attack 足够大，使单次 eliteDamage(Attack, Defense) ≥ 满血——
	// 角色从 HP=100(≥25，不触反射撤退) 被一击打到 ≤0 → "down"（而非 HP 先掉到 <25 触发 "fled"）。
	boss := Threat{
		ID: "fb_provfix", Name: "蛮荒巨蜥", Tier: ThreatTierFieldBoss, RegionID: "r1",
		Power: 999, Attack: 200, Defense: 10, HPPool: 999, Severity: 65,
		Loot: []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: 30}},
	}
	res, err := service.ResolveFieldBoss(ctx, &state, []*unit.Record{&actor}, boss)
	if err != nil {
		t.Fatalf("field boss 遭遇出错: %v", err)
	}
	if res.Victory {
		t.Fatalf("弱者单挑强 field_boss 不应取胜")
	}
	if len(res.Members) != 1 {
		t.Fatalf("应恰一名队员结算，得到 %d", len(res.Members))
	}
	m := res.Members[0]
	if m.Outcome != "down" {
		t.Fatalf("弱者应被打 down，得到 %q（HP 应被打到 0）", m.Outcome)
	}
	// 关键：候选层2 无战前前因 → 经 provenance 闸降到层1（绝不无源致残）。
	if m.PenaltyLayer != 1 {
		t.Fatalf("无战前前因的 down 败北应被 provenance 闸降到层1，得到层 %d（橡皮图章未修则会停在层2）", m.PenaltyLayer)
	}
	// D0-D3 硬锁：绝不阵亡。
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.LivesRemaining <= 0 {
		t.Fatalf("败北绝不应阵亡（D0-D3 硬锁），lives=%d", reloaded.Status.LivesRemaining)
	}
}

// ---- H2：排他仲裁胜负经 EmitProcessEvent 落库（CROSS_CONTEST_WIN/LOSE + Scope.WorldID/Tick）----

// contestRecCountByCode 数某 world_id + tick 下某 reason_code 且 actor 非空的事件行数（镜像审计 queryOutcomes 过滤口径）。
func contestRecCountByCode(t *testing.T, db *sql.DB, worldID string, tick int, code events.ReasonCode) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE world_id = ? AND tick = ? AND reason_code = ? AND actor_unit_id IS NOT NULL`,
		worldID, tick, string(code)).Scan(&n); err != nil {
		t.Fatalf("数争夺事件失败: %v", err)
	}
	return n
}

// ≥2 合格争夺者争一件排他件 → 胜者发 CROSS_CONTEST_WIN、每个败者发 CROSS_CONTEST_LOSE，皆带 Scope.WorldID/Tick。
func TestContestRec_TwoContenders_EmitsWinAndLoseWithScope(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	winner := unit.BootstrapRecord(2, "s1", "player", "胜")
	loserA := unit.BootstrapRecord(4, "s1", "player", "败甲")
	loserB := unit.BootstrapRecord(6, "s1", "player", "败乙")
	for _, u := range []unit.Record{winner, loserA, loserB} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
	}

	const worldID, tick = "w_contest", 7
	// 手造排他件 award：1 胜（AwardWon）+ 2 败（AwardConsolation）= 3 争夺者。
	awards := []encounter.Award{
		{ItemID: "boss_relic", UnitID: winner.ID, Quantity: 1, Reason: encounter.AwardWon},
		{ItemID: "boss_relic", UnitID: loserA.ID, Quantity: 1, Reason: encounter.AwardConsolation},
		{ItemID: "boss_relic", UnitID: loserB.ID, Quantity: 1, Reason: encounter.AwardConsolation},
		// 可分件不应触发争夺留痕（AwardShare 跳过）。
		{ItemID: "gold", UnitID: winner.ID, Quantity: 10, Reason: encounter.AwardShare},
	}
	scoreByUnit := map[string]float64{winner.ID: 40, loserA.ID: 10, loserB.ID: 5}
	service.recordExclusiveContestOutcomes(ctx, "s1", worldID, tick, awards, scoreByUnit)

	if got := contestRecCountByCode(t, db, worldID, tick, events.ReasonCrossContestWin); got != 1 {
		t.Fatalf("应恰 1 条 CROSS_CONTEST_WIN（带 scope），得到 %d", got)
	}
	if got := contestRecCountByCode(t, db, worldID, tick, events.ReasonCrossContestLose); got != 2 {
		t.Fatalf("应恰 2 条 CROSS_CONTEST_LOSE（带 scope），得到 %d", got)
	}
	// 胜者那条 owner 必是 winner（审计按 actor_unit_id 分组判付费胜率）。
	var winOwner string
	if err := db.QueryRow(
		`SELECT actor_unit_id FROM events WHERE world_id = ? AND tick = ? AND reason_code = ?`,
		worldID, tick, string(events.ReasonCrossContestWin)).Scan(&winOwner); err != nil {
		t.Fatalf("查胜者 owner 失败: %v", err)
	}
	if winOwner != winner.ID {
		t.Fatalf("CROSS_CONTEST_WIN 的 owner 应为胜者 %s，得到 %s", winner.ID, winOwner)
	}
	// 反 P2W：payload 只记 actor/score/contenders，绝不含 wallet/billing。
	var payload string
	if err := db.QueryRow(
		`SELECT payload_json FROM events WHERE world_id = ? AND tick = ? AND reason_code = ?`,
		worldID, tick, string(events.ReasonCrossContestWin)).Scan(&payload); err != nil {
		t.Fatalf("查 payload 失败: %v", err)
	}
	if contains(payload, "wallet") || contains(payload, "billing") || contains(payload, "paid") {
		t.Fatalf("争夺留痕 payload 绝不应含付费维度（反 P2W），得到 %q", payload)
	}
}

// 单参与者（只有 AwardWon、无 consolation）= 无争夺 → 不发任何争夺事件。
func TestContestRec_SingleParticipant_EmitsNothing(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	solo := unit.BootstrapRecord(2, "s1", "player", "独行")
	if err := repo.Save(ctx, solo); err != nil {
		t.Fatalf("save: %v", err)
	}
	const worldID, tick = "w_solo", 3
	awards := []encounter.Award{
		{ItemID: "boss_relic", UnitID: solo.ID, Quantity: 1, Reason: encounter.AwardWon},
		{ItemID: "gold", UnitID: solo.ID, Quantity: 15, Reason: encounter.AwardShare},
	}
	service.recordExclusiveContestOutcomes(ctx, "s1", worldID, tick, awards, map[string]float64{solo.ID: 40})

	if got := contestRecCountByCode(t, db, worldID, tick, events.ReasonCrossContestWin); got != 0 {
		t.Fatalf("单参与者无争夺，不应发 WIN，得到 %d", got)
	}
	if got := contestRecCountByCode(t, db, worldID, tick, events.ReasonCrossContestLose); got != 0 {
		t.Fatalf("单参与者无争夺，不应发 LOSE，得到 %d", got)
	}
}

// worldID 为空（未接入多世界）→ 不发（审计按 world 检索，无世界归属即无意义）。
func TestContestRec_NoWorld_EmitsNothing(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	w := unit.BootstrapRecord(2, "s1", "player", "甲")
	l := unit.BootstrapRecord(4, "s1", "player", "乙")
	for _, u := range []unit.Record{w, l} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	awards := []encounter.Award{
		{ItemID: "boss_relic", UnitID: w.ID, Quantity: 1, Reason: encounter.AwardWon},
		{ItemID: "boss_relic", UnitID: l.ID, Quantity: 1, Reason: encounter.AwardConsolation},
	}
	before := eventCount(t, db)
	service.recordExclusiveContestOutcomes(ctx, "s1", "", 7, awards, map[string]float64{w.ID: 40, l.ID: 10})
	if after := eventCount(t, db); after != before {
		t.Fatalf("worldID 为空时不应写任何事件，事件数从 %d 变为 %d", before, after)
	}
}

// H2 端到端：ResolveFieldBoss 胜利且 WorldID 非空 + ≥2 队员 → 排他遗物经仲裁后落 WIN/LOSE 各带 scope。
func TestContestRec_FieldBossVictory_EmitsContestOutcomes(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	m1 := unit.BootstrapRecord(2, "s1", "player", "甲")
	m2 := unit.BootstrapRecord(4, "s1", "player", "乙")
	for _, u := range []*unit.Record{&m1, &m2} {
		u.Status.HP = 100
		u.Status.Attack = 60 // 双双强：齐力速破，皆有贡献成为合格争夺者
		u.Status.Defense = 30
		if err := repo.Save(ctx, *u); err != nil {
			t.Fatalf("save: %v", err)
		}
	}

	state := State{ID: "s1", WorldID: "w_fb"}
	state.TurnState.Turn = 4
	// boss + 含唯一遗物：HPPool=120 > 单个队员单击伤害(59)，故**任一队员都无法一击秒杀**——
	// 两人都必出手命中后才破，皆有贡献 → 排他遗物恒有 2 名合格争夺者（避免单人速破致争夺者只 1 的偶发）。
	// boss 攻击极弱(Attack=3)：不会把任何人打到 <25 触发反射撤退，两人全程在场。
	boss := Threat{
		ID: "fb_contest", Name: "蛮荒巨蜥", Tier: ThreatTierFieldBoss, RegionID: "r1",
		Power: 100, Attack: 3, Defense: 1, HPPool: 120, Severity: 65,
		Loot: []encounter.LootItem{
			{ID: "gold", Rarity: encounter.Common, Quantity: 60},
			{ID: "boss_relic", Rarity: encounter.Epic, Quantity: 1},
		},
	}
	res, err := service.ResolveFieldBoss(ctx, &state, []*unit.Record{&m1, &m2}, boss)
	if err != nil {
		t.Fatalf("field boss 遭遇出错: %v", err)
	}
	if !res.Victory {
		t.Fatalf("双强者协力应讨平弱 field_boss")
	}
	if got := contestRecCountByCode(t, db, "w_fb", 4, events.ReasonCrossContestWin); got != 1 {
		t.Fatalf("排他遗物应恰 1 条 WIN（带 scope），得到 %d", got)
	}
	if got := contestRecCountByCode(t, db, "w_fb", 4, events.ReasonCrossContestLose); got != 1 {
		t.Fatalf("排他遗物应恰 1 条 LOSE（带 scope），得到 %d", got)
	}
}
