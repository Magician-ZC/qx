package session

// 文件说明：PvE 自治四缺口回归测试（对真实 SQLite + 纯函数），与 threat_test.go / threat_provenance_fix_test.go 解耦：
//   ① join_intent 参与意愿评估：确定性、无源不参战(OOC)、护短迎战、advice 偏置非覆盖。
//   ② PvE 专属 reason-code 真写入：威胁浮现/讨平/覆没/同伴倒下/败北分级/排他仲裁(带 Scope)，grep events 断言。
//   ③ 层3 consent：候选层3 升级 RequiresConsent 待决策卡 + 超时降级为残废(COMBAT_MAIMED 改 hp)，绝不阵亡。
//   ④ region_ravaged：败北写 REGION_RAVAGED + 对牵挂者扇出收件箱卡（旁观者收卡）。
// 前缀统一 pveGap*，避免与既有威胁/命运测试撞名。

import (
	"context"
	"database/sql"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// ---- 测试辅助 ----

// pveGapCountEvents 数某 reason_code 的 events 行数（owner 不限）。
func pveGapCountEvents(t *testing.T, db *sql.DB, code events.ReasonCode) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE reason_code = ?`, string(code)).Scan(&n); err != nil {
		t.Fatalf("数 %s 事件失败: %v", code, err)
	}
	return n
}

// pveGapCountOwnerEvents 数某 owner（actor_unit_id）某 reason_code 的 events 行数。
func pveGapCountOwnerEvents(t *testing.T, db *sql.DB, ownerID string, code events.ReasonCode) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		ownerID, string(code)).Scan(&n); err != nil {
		t.Fatalf("数 %s/%s 事件失败: %v", ownerID, code, err)
	}
	return n
}

// pveGapStrongActor 造一名强角色（必胜）、显著好战人格（aggression=0.9）、有钱不饿。
func pveGapStrongActor(t *testing.T, ctx context.Context, repo *unit.Repository, seed int64, name string) unit.Record {
	t.Helper()
	actor := unit.BootstrapRecord(seed, "s1", "player", name)
	actor.Status.HP = 100
	actor.Status.Hunger = 100
	actor.Status.Wallet = 100
	actor.Status.Attack = 40
	actor.Status.Defense = 20
	actor.Status.Morale = 0.7
	actor.Status.Loyalty = 0.8
	actor.Personality.Aggression = 0.9 // 显著好战 → persona_trait 前因可解析
	actor.Personality.Courage = 0.8
	actor.Personality.Prudence = 0.2
	actor.Ambition.Power = 0.9 // 高野心 → 迎战驱动
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存强角色失败: %v", err)
	}
	return actor
}

// pveGapNoCauseActor 造一名「无任何战前可解析前因」的角色：人格全 0.5、无关系/红线/记忆、不饿不疲有钱、野心全 0。
func pveGapNoCauseActor(t *testing.T, ctx context.Context, repo *unit.Repository, name string) unit.Record {
	t.Helper()
	actor := unit.BootstrapRecord(2, "s1", "player", name)
	actor.Personality = unit.Personality{
		Courage: 0.5, Loyalty: 0.5, Aggression: 0.5, Prudence: 0.5,
		Sociability: 0.5, Integrity: 0.5, Stability: 0.5, Ambition: 0.5,
	}
	actor.Ambition = unit.Ambition{} // 全 0：无野心驱动
	actor.Status.HP = 100
	actor.Status.Hunger = 100
	actor.Status.Fatigue = 0
	actor.Status.Wallet = 100
	actor.Status.InCombat = false
	actor.Status.Attack = 40
	actor.Status.Defense = 20
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存无前因角色失败: %v", err)
	}
	return actor
}

// ============ ① join_intent ============

// 无任何可解析前因 → 归因不过 ValidateAttribution → 不参战（OOC）。即便评分够，无源也绝不迎战。
func TestPveGap_JoinIntent_NoCauseDeclines(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapNoCauseActor(t, ctx, repo, "无依")
	state := State{ID: "s1"}
	state.TurnState.Turn = 3

	threat := Threat{ID: "t1", Name: "山魈", Tier: ThreatTierElite}
	// 给最强的迎战信号（护短+目标），但无任何真实可解析前因（关系表空、压力全无、野心全0、人格全0.5）。
	out := service.EvaluateJoinIntent(ctx, &state, &actor, threat, true, true, JoinAdvice{})
	if out.Join {
		t.Fatalf("无源不许参战（OOC），却判迎战：score=%.3f grounded=%v", out.Score, out.Grounded)
	}
	if out.Grounded {
		t.Fatalf("无任何可解析前因，Grounded 应为 false")
	}
	if out.Mode != JoinModeReflexDecl {
		t.Fatalf("无源应退避，Mode=%q", out.Mode)
	}
}

// 反射护栏：HP 危急（<25）→ 直接退避，零评分（怕死的物理上不去）。
func TestPveGap_JoinIntent_ReflexDeclineOnLowHP(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapStrongActor(t, ctx, repo, 2, "重伤者")
	actor.Status.HP = 10 // <25：反射护栏触发
	state := State{ID: "s1"}
	state.TurnState.Turn = 3

	out := service.EvaluateJoinIntent(ctx, &state, &actor, Threat{ID: "t1", Name: "山魈", Tier: ThreatTierElite}, true, true, JoinAdvice{})
	if out.Join {
		t.Fatalf("HP 危急应反射退避，却判迎战")
	}
	if out.Mode != JoinModeReflexDecl {
		t.Fatalf("HP 危急应 reflex_declined，Mode=%q", out.Mode)
	}
}

// 护短/野心迎战：强角色（显著好战人格 + 高野心）有可解析前因 → 自治参战（JoinModeAutonomous）。
func TestPveGap_JoinIntent_GroundedAutoJoins(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapStrongActor(t, ctx, repo, 2, "敢战者")
	state := State{ID: "s1"}
	state.TurnState.Turn = 3

	out := service.EvaluateJoinIntent(ctx, &state, &actor, Threat{ID: "t1", Name: "山魈", Tier: ThreatTierElite}, true, false, JoinAdvice{})
	if !out.Join {
		t.Fatalf("显著好战+高野心+护短的强角色应自治迎战：score=%.3f grounded=%v", out.Score, out.Grounded)
	}
	if !out.Grounded {
		t.Fatalf("显著人格前因应过 ValidateAttribution，Grounded 应为 true")
	}
	if out.Mode != JoinModeAutonomous {
		t.Fatalf("无 advice 的自治迎战应 autonomous，Mode=%q", out.Mode)
	}
}

// 确定性：同输入两次评估恒得同一结果（含 advice 抗命掷骰用 FNV，不用全局 rand）。
func TestPveGap_JoinIntent_Deterministic(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapStrongActor(t, ctx, repo, 2, "甲")
	state := State{ID: "s1"}
	state.TurnState.Turn = 5
	threat := Threat{ID: "t1", Name: "山魈", Tier: ThreatTierFieldBoss}
	advice := JoinAdvice{Present: true, Urge: false, Reject: 0.5} // 叮嘱别去，半概率抗命

	a := service.EvaluateJoinIntent(ctx, &state, &actor, threat, false, false, advice)
	b := service.EvaluateJoinIntent(ctx, &state, &actor, threat, false, false, advice)
	if a.Join != b.Join || a.Score != b.Score || a.Mode != b.Mode {
		t.Fatalf("join_intent 应确定性可复现：a=%+v b=%+v", a, b)
	}
}

// advice 偏置非覆盖：叮嘱「去」给护短强角色加偏置（采纳后仍是她自愿，受嘱参战 advised）。
func TestPveGap_JoinIntent_AdviceBiasNotOverride(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapStrongActor(t, ctx, repo, 2, "听劝者")
	state := State{ID: "s1"}
	state.TurnState.Turn = 3
	// Reject=0：必采纳叮嘱。叮嘱「去」加偏置——强角色本就够阈值，advised 标记体现「听了你的话」。
	out := service.EvaluateJoinIntent(ctx, &state, &actor,
		Threat{ID: "t1", Name: "山魈", Tier: ThreatTierElite}, false, false,
		JoinAdvice{Present: true, Urge: true, Reject: 0})
	if !out.Join {
		t.Fatalf("强角色受嘱「去」应迎战：score=%.3f", out.Score)
	}
	if out.Mode != JoinModeAdvised {
		t.Fatalf("采纳了「去」的叮嘱应 advised，Mode=%q", out.Mode)
	}
}

// ============ ② PvE 专属 reason-code 真写入 ============

// elite 胜利：威胁浮现(THREAT_EMERGED) + 讨平(THREAT_DEFEATED) 真写入 events。
func TestPveGap_EliteVictory_EmitsThreatCodes(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapStrongActor(t, ctx, repo, 2, "阿采")
	threat := Threat{
		ID: "t_shanxiao", Name: "山魈", Tier: ThreatTierElite, RegionID: "r1",
		Power: 120, Attack: 5, Defense: 5, HPPool: 40, Severity: 40,
		Loot: []encounter.LootItem{{ID: "gold", Rarity: encounter.Common, Quantity: 20}},
	}
	state := State{ID: "s1"}
	res, err := service.ResolveEliteEncounter(ctx, &state, &actor, threat)
	if err != nil {
		t.Fatalf("遭遇出错: %v", err)
	}
	if res.Outcome != "defeated" {
		t.Fatalf("强角色应讨平弱精英，得到 %q", res.Outcome)
	}
	if pveGapCountEvents(t, db, events.ReasonThreatEmerged) != 1 {
		t.Fatalf("应恰 1 条 THREAT_EMERGED")
	}
	if pveGapCountEvents(t, db, events.ReasonThreatDefeated) != 1 {
		t.Fatalf("讨平应恰 1 条 THREAT_DEFEATED")
	}
}

// elite 失败：败北分级(PVE_DEFEAT_D1) 真写入；候选层1 绝不 EMOTION_TRAUMA 混淆，绝不阵亡。
func TestPveGap_EliteDefeat_EmitsDefeatTierCode(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := unit.BootstrapRecord(4, "s1", "player", "弱者")
	actor.Status.Attack = 2
	actor.Status.Defense = 5
	actor.Status.HP = 100
	actor.Status.Morale = 0.7
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	threat := Threat{ID: "t_bear", Name: "巨熊", Tier: ThreatTierElite, RegionID: "r1",
		Power: 600, Attack: 50, Defense: 10, HPPool: 300, Severity: 90}
	state := State{ID: "s1"}
	res, err := service.ResolveEliteEncounter(ctx, &state, &actor, threat)
	if err != nil {
		t.Fatalf("遭遇出错: %v", err)
	}
	if res.PenaltyLayer != 1 {
		t.Fatalf("elite 失败应落层1，得到 %d", res.PenaltyLayer)
	}
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonDefeatSetback) != 1 {
		t.Fatalf("层1 败北应恰 1 条 PVE_DEFEAT_D1（专属码，非 EMOTION_TRAUMA）")
	}
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.LivesRemaining <= 0 {
		t.Fatalf("elite 失败绝不应阵亡（D0-D3 硬锁），lives=%d", reloaded.Status.LivesRemaining)
	}
}

// field boss 胜利 + WorldID + ≥2 队员 + 排他遗物 → ECONOMY_LOOT_ARBITRATED 带 Scope.WorldID/Tick 真写入。
func TestPveGap_FieldBossVictory_EmitsArbitratedLootWithScope(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	m1 := unit.BootstrapRecord(2, "s1", "player", "甲")
	m2 := unit.BootstrapRecord(4, "s1", "player", "乙")
	for _, u := range []*unit.Record{&m1, &m2} {
		u.Status.HP = 100
		u.Status.Attack = 60
		u.Status.Defense = 30
		if err := repo.Save(ctx, *u); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	const worldID, tick = "w_fb", 4
	state := State{ID: "s1", WorldID: worldID}
	state.TurnState.Turn = tick
	boss := Threat{
		ID: "fb_arb", Name: "蛮荒巨蜥", Tier: ThreatTierFieldBoss, RegionID: "r1",
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
		t.Fatalf("双强者应讨平弱 field boss")
	}
	// ECONOMY_LOOT_ARBITRATED：排他遗物恰 1 条胜者归属，且带 Scope（world_id + tick）。
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE reason_code = ? AND world_id = ? AND tick = ?`,
		string(events.ReasonEconomyLootArbitrated), worldID, tick).Scan(&n); err != nil {
		t.Fatalf("数仲裁分赃事件失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("排他遗物应恰 1 条 ECONOMY_LOOT_ARBITRATED（带 Scope），得到 %d", n)
	}
	// 反 P2W：payload 绝不含付费维度。
	var payload string
	if err := db.QueryRow(
		`SELECT payload_json FROM events WHERE reason_code = ? AND world_id = ?`,
		string(events.ReasonEconomyLootArbitrated), worldID).Scan(&payload); err != nil {
		t.Fatalf("查 payload 失败: %v", err)
	}
	if contains(payload, "wallet") || contains(payload, "billing") || contains(payload, "paid") {
		t.Fatalf("仲裁分赃 payload 绝不应含付费维度，得到 %q", payload)
	}
	// THREAT_DEFEATED 也应落。
	if pveGapCountEvents(t, db, events.ReasonThreatDefeated) != 1 {
		t.Fatalf("讨平应恰 1 条 THREAT_DEFEATED")
	}
}

// field boss 全员被打 down → THREAT_WIPE + ALLY_DOWN（同伴目睹倒下，经 Mutator 改 morale）真写入。
func TestPveGap_FieldBossWipe_EmitsWipeAndAllyDown(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	// 两名弱角色，HP 高（避免先触发反射撤退），但攻击弱、防御弱 → 会被强 boss 逐个打 down。
	m1 := unit.BootstrapRecord(2, "s1", "player", "甲")
	m2 := unit.BootstrapRecord(4, "s1", "player", "乙")
	for _, u := range []*unit.Record{&m1, &m2} {
		u.Status.HP = 100
		u.Status.Attack = 3
		u.Status.Defense = 1
		u.Status.Morale = 0.8
		if err := repo.Save(ctx, *u); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	state := State{ID: "s1"}
	state.TurnState.Turn = 5
	// 强 boss：一击秒杀（Attack=200），从满血直接打 down（不触反射撤退阈值）。
	boss := Threat{
		ID: "fb_wipe", Name: "蛮荒巨蜥", Tier: ThreatTierFieldBoss, RegionID: "r1",
		Power: 999, Attack: 200, Defense: 10, HPPool: 9999, Severity: 65,
	}
	res, err := service.ResolveFieldBoss(ctx, &state, []*unit.Record{&m1, &m2}, boss)
	if err != nil {
		t.Fatalf("field boss 遭遇出错: %v", err)
	}
	if res.Victory {
		t.Fatalf("弱队不应讨平强 boss")
	}
	if pveGapCountEvents(t, db, events.ReasonThreatWipe) != 1 {
		t.Fatalf("覆没应恰 1 条 THREAT_WIPE")
	}
	// 至少一名队员先倒下时，在场同伴应收到一条 ALLY_DOWN（经 Mutator 改 morale）。
	if pveGapCountEvents(t, db, events.ReasonThreatAllyDown) < 1 {
		t.Fatalf("同伴倒下应至少 1 条 ALLY_DOWN")
	}
	// 任何一人都绝不阵亡（D0-D3 硬锁）。
	for _, id := range []string{m1.ID, m2.ID} {
		reloaded, _ := repo.GetByID(ctx, id)
		if reloaded.Status.LivesRemaining <= 0 {
			t.Fatalf("覆没绝不应阵亡（D0-D3 硬锁），unit=%s lives=%d", id, reloaded.Status.LivesRemaining)
		}
	}
}

// ============ ③ 层3 consent：RequiresConsent 卡 + 超时降级残废不阵亡 ============

// pveGapDeepCareActor 造一名深牵挂(loyalty 高)、在世久、且有可解析前因(显著人格)的角色——
// 使 PenaltyCap 允许层3、provenance 闸放行。
func pveGapDeepCareActor(t *testing.T, ctx context.Context, repo *unit.Repository, name string) unit.Record {
	t.Helper()
	actor := unit.BootstrapRecord(2, "s1", "player", name)
	actor.Status.HP = 80
	actor.Status.Hunger = 100
	actor.Status.Wallet = 100
	actor.Status.Morale = 0.7
	actor.Status.Loyalty = 1.0      // 高忠诚 → care 高
	actor.Personality.Courage = 0.9 // 显著人格前因（|0.9-0.5|≥0.25）
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存深牵挂角色失败: %v", err)
	}
	return actor
}

// 候选层3 → 升级 RequiresConsent 待决策卡（PENDING_DECISION），当下只落层2，绝不阵亡。
// 直接测 defeatLayerAfterConsent（层3 落地前的 consent 路径单元），隔离 DegradePenalty 的 care 门（其门由 encounter 包另测）。
func TestPveGap_Layer3_SurfacesRequiresConsentNotKill(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapDeepCareActor(t, ctx, repo, "老兵")
	state := State{ID: "s1"}
	state.TurnState.Turn = 10
	threat := Threat{ID: "wb1", Name: "山岳巨灵", Tier: ThreatTierWorldBoss, RegionID: "r1", Power: 3000}

	layer := service.defeatLayerAfterConsent(ctx, &state, &actor, threat, "她栽在那头山岳巨灵手里，正是她的性子使然")
	// 层3 经 consent 路径降到层2（当下只落可恢复代价），绝不在玩家未应答时落地不可逆。
	if layer != 2 {
		t.Fatalf("层3 落地前应经 consent 降到层2，得到 %d", layer)
	}
	// 升级了 RequiresConsent 待决策卡（用不可逆类 COMBAT_DOWN 路由进待决策）。
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonPendingDecision) < 1 {
		t.Fatalf("层3 应升级一张 RequiresConsent 待决策卡（PENDING_DECISION）")
	}
	// 落了 pending 标记（PVE_DEFEAT_D3 流程广播，含 deadline_turn/degrade=maimed）。
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonDefeatCrippled) < 1 {
		t.Fatalf("层3 应落一条 PVE_DEFEAT_D3 pending 标记")
	}
	var payload string
	if err := db.QueryRow(`SELECT payload_json FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		actor.ID, string(events.ReasonDefeatCrippled)).Scan(&payload); err != nil {
		t.Fatalf("查 D3 pending payload 失败: %v", err)
	}
	if !contains(payload, "requires_consent") || !contains(payload, "maimed") {
		t.Fatalf("D3 pending 标记应含 consent_tier=requires_consent + degrade=maimed，得到 %q", payload)
	}
	// 绝不阵亡：当下不可逆未落地。
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.LivesRemaining <= 0 {
		t.Fatalf("层3 consent 待决策期绝不应阵亡，lives=%d", reloaded.Status.LivesRemaining)
	}
}

// 超时未应答 → 自动降级为残废（COMBAT_MAIMED 改 hp，floored >0），绝不 FELL_IN_DEFEAT 阵亡。
func TestPveGap_Layer3_TimeoutDegradesToMaimedNotDeath(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapDeepCareActor(t, ctx, repo, "迟暮")
	actor.Status.HP = 40
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}
	state.TurnState.Turn = 16 // 已过 deadline

	hpBefore := actor.Status.HP
	maimed, err := service.degradeLayer3OnTimeout(ctx, &state, &actor, "山岳巨灵")
	if err != nil {
		t.Fatalf("超时降级出错: %v", err)
	}
	if !maimed {
		t.Fatalf("超时应降级为残废")
	}
	// COMBAT_MAIMED 真写入（改 hp）。
	if pveGapCountOwnerEvents(t, db, actor.ID, events.ReasonCombatMaimed) < 1 {
		t.Fatalf("超时降级应落 COMBAT_MAIMED")
	}
	// 绝不 FELL_IN_DEFEAT 阵亡。
	if pveGapCountEvents(t, db, events.ReasonFellInDefeat) != 0 {
		t.Fatalf("超时降级绝不应阵亡（FELL_IN_DEFEAT），却写了陨落事件")
	}
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.HP <= 0 {
		t.Fatalf("残废 HP 应 floored 在 >0，得到 %d", reloaded.Status.HP)
	}
	if reloaded.Status.HP >= hpBefore {
		t.Fatalf("残废应永久折损 HP，前=%d 后=%d", hpBefore, reloaded.Status.HP)
	}
	if reloaded.Status.LivesRemaining <= 0 {
		t.Fatalf("残废绝不阵亡，lives=%d", reloaded.Status.LivesRemaining)
	}
}

// 残废下限：低血角色超时降级也 floored 在 HP=1（绝不被这条降到 0/致死）。
func TestPveGap_Layer3_MaimFlooredAboveZero(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	actor := pveGapDeepCareActor(t, ctx, repo, "残烛")
	actor.Status.HP = 5 // 低于折损幅度
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}
	state.TurnState.Turn = 16
	if _, err := service.degradeLayer3OnTimeout(ctx, &state, &actor, "山岳巨灵"); err != nil {
		t.Fatalf("超时降级出错: %v", err)
	}
	reloaded, _ := repo.GetByID(ctx, actor.ID)
	if reloaded.Status.HP < 1 {
		t.Fatalf("残废 HP 应 floored ≥1（绝不致死），得到 %d", reloaded.Status.HP)
	}
}

// ============ ④ region_ravaged 旁观者收卡 ============

// elite 失败且 threat 有 region 归属 → 写 REGION_RAVAGED；牵挂 victim 的旁观者按相关性收到一版收件箱卡。
func TestPveGap_RegionRavaged_BystanderReceivesCard(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// victim：弱角色，必败（被打 down/fled）。region 归属非空 → 触发 region_ravaged。
	victim := unit.BootstrapRecord(2, "s1", "player", "守望者")
	victim.Status.Attack = 2
	victim.Status.Defense = 5
	victim.Status.HP = 100
	victim.Status.Morale = 0.7
	if err := repo.Save(ctx, victim); err != nil {
		t.Fatalf("save victim: %v", err)
	}
	// bystander：对 victim 有强牵挂（出边指向 victim，强 affection）——「她母亲」。
	mother := unit.BootstrapRecord(4, "s1", "player", "她母亲")
	if err := repo.Save(ctx, mother); err != nil {
		t.Fatalf("save mother: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		mother.ID, victim.ID, 6.0, 0.0, 10.0, 0.0,
	); err != nil {
		t.Fatalf("插入牵挂关系失败: %v", err)
	}

	threat := Threat{ID: "t_bear", Name: "巨熊", Tier: ThreatTierElite, RegionID: "故乡河谷",
		Power: 600, Attack: 50, Defense: 10, HPPool: 300, Severity: 90}
	state := State{ID: "s1"}
	res, err := service.ResolveEliteEncounter(ctx, &state, &victim, threat)
	if err != nil {
		t.Fatalf("遭遇出错: %v", err)
	}
	if res.Outcome == "defeated" {
		t.Fatalf("弱者不应讨平强精英")
	}
	// REGION_RAVAGED 世界事件真写入（victim 名下一条）。
	if pveGapCountOwnerEvents(t, db, victim.ID, events.ReasonRegionRavaged) < 1 {
		t.Fatalf("败北且有 region 归属应写 REGION_RAVAGED")
	}
	// 旁观者（她母亲）按相关性收到一版收件箱卡（高光或待决策，actor=母亲）。
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code IN (?, ?)`,
		mother.ID, string(events.ReasonInboxHighlight), string(events.ReasonPendingDecision)).Scan(&n); err != nil {
		t.Fatalf("数旁观者收件箱卡失败: %v", err)
	}
	if n < 1 {
		t.Fatalf("强牵挂的旁观者应收到一版 region_ravaged 收件箱卡，得到 %d", n)
	}
}

// 无 region 归属（单人 elite，RegionID 空）→ 绝不写 REGION_RAVAGED（家园之难才有此码）。
func TestPveGap_RegionRavaged_NoRegionNoEmit(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	victim := unit.BootstrapRecord(2, "s1", "player", "独行")
	victim.Status.Attack = 2
	victim.Status.Defense = 5
	victim.Status.HP = 100
	victim.Status.Morale = 0.7
	if err := repo.Save(ctx, victim); err != nil {
		t.Fatalf("save: %v", err)
	}
	threat := Threat{ID: "t_bear", Name: "巨熊", Tier: ThreatTierElite, RegionID: "", // 无 region
		Power: 600, Attack: 50, Defense: 10, HPPool: 300, Severity: 90}
	state := State{ID: "s1"}
	if _, err := service.ResolveEliteEncounter(ctx, &state, &victim, threat); err != nil {
		t.Fatalf("遭遇出错: %v", err)
	}
	if pveGapCountEvents(t, db, events.ReasonRegionRavaged) != 0 {
		t.Fatalf("无 region 归属绝不应写 REGION_RAVAGED")
	}
}
