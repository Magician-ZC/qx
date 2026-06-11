package session

// 文件说明：命运/PvE 因果链三特性的聚焦测试——①PvE 后果 provenance 闸（无源前因强制降到层1）、
// ②LongTermGoals 驱动 goal_reassess（长期目标拼入 prompt）、③破圈预算（零锚来源事件按日配额≤1升档进高光）。
// 前缀统一 sp*（serendipity/provenance），避免与既有命运/威胁测试撞名。纯函数测试无 DB；集成测试复用 newThreatTestService。

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

// ---- ① PvE 后果 provenance 闸 ----

// TestSnapshotHasResolvableCause 验证「可解析前因」判定的各分支：人格显著/可锚记忆/红线/显著关系/压力/玩家动作。
func TestSnapshotHasResolvableCause(t *testing.T) {
	// 空快照（无任何前因）→ 无可解析前因。
	empty := decision.Snapshot{
		Traits:               map[string]float64{"courage": 0.5}, // 恰为 0.5，不显著
		Memories:             map[string]decision.MemoryMeta{},
		Redlines:             map[string]string{},
		Relations:            map[string]decision.RelationAxes{},
		PlayerActionEventIDs: map[string]struct{}{},
	}
	if snapshotHasResolvableCause(empty) {
		t.Fatalf("空快照不应有可解析前因")
	}

	// 显著人格（|0.85-0.5|=0.35 ≥ 0.25）→ 有前因。
	trait := empty
	trait.Traits = map[string]float64{"aggression": 0.85}
	if !snapshotHasResolvableCause(trait) {
		t.Fatalf("显著人格应算可解析前因")
	}

	// 可锚记忆（importance ≥ 6）→ 有前因。
	mem := empty
	mem.Memories = map[string]decision.MemoryMeta{"m1": {Importance: 7}}
	if !snapshotHasResolvableCause(mem) {
		t.Fatalf("高重要度记忆应算可解析前因")
	}
	// 低重要度记忆（importance < 6）→ 不算。
	memLow := empty
	memLow.Memories = map[string]decision.MemoryMeta{"m1": {Importance: 3}}
	if snapshotHasResolvableCause(memLow) {
		t.Fatalf("低重要度记忆不应算可解析前因")
	}

	// 红线 → 有前因。
	red := empty
	red.Redlines = map[string]string{"rl1": "永不背叛"}
	if !snapshotHasResolvableCause(red) {
		t.Fatalf("红线应算可解析前因")
	}

	// 显著关系（|轴| ≥ 0.3）→ 有前因。
	rel := empty
	rel.Relations = map[string]decision.RelationAxes{"u2": {Affection: 0.6}}
	if !snapshotHasResolvableCause(rel) {
		t.Fatalf("显著关系应算可解析前因")
	}
	// 弱关系（|轴| < 0.3）→ 不算。
	relWeak := empty
	relWeak.Relations = map[string]decision.RelationAxes{"u2": {Affection: 0.1}}
	if snapshotHasResolvableCause(relWeak) {
		t.Fatalf("弱关系不应算可解析前因")
	}

	// 战前现实压力（饥饿）→ 有前因。Injury/Threat 可能由本次战斗自产、已被 M4 修复剔除，故改用战前 Hunger。
	pres := empty
	pres.Pressure = decision.PressureFlags{Hunger: true}
	if !snapshotHasResolvableCause(pres) {
		t.Fatalf("战前现实压力应算可解析前因")
	}
	// 战斗自产的伤势（Injury）不再单独构成可解析前因（M4：避免「败了所以伤、伤了所以判有前因」橡皮图章）。
	injuryOnly := empty
	injuryOnly.Pressure = decision.PressureFlags{Injury: true}
	if snapshotHasResolvableCause(injuryOnly) {
		t.Fatalf("仅战斗自产伤势不应算可解析前因（橡皮图章已修）")
	}

	// 玩家动作 → 有前因。
	act := empty
	act.PlayerActionEventIDs = map[string]struct{}{"e1": {}}
	if !snapshotHasResolvableCause(act) {
		t.Fatalf("玩家动作应算可解析前因")
	}
}

// TestDeriveDefeatProvenance 验证败北归因句的派生：有前因→「她栽在那头X手里，<引子>」；无前因→空串。
func TestDeriveDefeatProvenance(t *testing.T) {
	threat := Threat{Name: "巨熊"}
	snap := decision.Snapshot{Pressure: decision.PressureFlags{Hunger: true}}
	got := deriveDefeatProvenance(snap, threat)
	if !strings.Contains(got, "巨熊") || !strings.Contains(got, "力气") {
		t.Fatalf("有战前压力前因应派生含威胁名与引子的归因句，得到 %q", got)
	}
	// 无前因 → 空串。
	if got := deriveDefeatProvenance(decision.Snapshot{}, threat); got != "" {
		t.Fatalf("无前因应返回空串，得到 %q", got)
	}
	// 红线优先于压力（dominantProvenanceCauseZH 顺序）。
	red := decision.Snapshot{Redlines: map[string]string{"rl1": "永不退"}, Pressure: decision.PressureFlags{Injury: true}}
	if got := deriveDefeatProvenance(red, threat); !strings.Contains(got, "红线") {
		t.Fatalf("红线应优先解释败北，得到 %q", got)
	}
}

// TestApplyDefeatPenalty_FieldBossUngroundedDownToLayer1 验证 §4.3 接 PvE：候选层2 的 field-boss 失败，
// 若角色无可解析前因（无记忆/无红线/无关系/无压力 且人格不显著），强制降到层1——绝不无源致残。
// 用一个**牵挂足够高**(care≥40 或 daysAlive≥3)的角色保证「若不被 provenance 闸拦住，本会落层2」，
// 从而 provenance 闸的降级效果可观测。
func TestApplyDefeatPenalty_FieldBossUngroundedDownToLayer1(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 牵挂高（忠诚拉满）→ care≥40 → PenaltyCap≥2；但无任何可解析前因 → provenance 闸应把层2 降到层1。
	actor := unit.BootstrapRecord(30, "s1", "player", "无源者")
	actor.Status.HP = 100
	actor.Status.Loyalty = 1.0
	// 抹平人格到 0.5 附近（不显著），并清空压力（HP 高、不饥饿、无受伤）。
	actor.Personality = unit.Personality{
		Courage: 0.5, Loyalty: 0.5, Aggression: 0.5, Prudence: 0.5,
		Sociability: 0.5, Integrity: 0.5, Stability: 0.5, Ambition: 0.5,
	}
	actor.Status.Hunger = 100
	actor.Status.Fatigue = 0
	actor.Status.Wallet = 100 // 非 0 → 无 Debt 压力
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}
	state.TurnState.Turn = 30 // daysAlive 大 → 进一步保证 cap≥2

	// 候选层 2 的威胁（field_boss）。
	boss := Threat{ID: "fb1", Name: "蛮荒巨蜥", Tier: ThreatTierFieldBoss}
	layer, err := service.applyDefeatPenalty(ctx, &state, &actor, boss)
	if err != nil {
		t.Fatalf("applyDefeatPenalty: %v", err)
	}
	if layer != 1 {
		t.Fatalf("无可解析前因的层2 候选应被 provenance 闸降到层1，得到层 %d", layer)
	}
	// 绝不致死：lives 完好。
	if actor.Status.LivesRemaining <= 0 {
		t.Fatalf("无源败北绝不应致死，lives=%d", actor.Status.LivesRemaining)
	}
}

// TestApplyDefeatPenalty_FieldBossGroundedKeepsLayer 验证：同为候选层2、同样高牵挂，但角色携带可解析前因
// （一条红线）→ provenance 闸放行，层2 不被降级（落地层 == DegradePenalty 的结果）。并验证 provenance 凭证落进命运卡。
func TestApplyDefeatPenalty_FieldBossGroundedKeepsLayer(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(31, "s1", "player", "有源者")
	actor.Status.HP = 100
	actor.Status.Loyalty = 1.0
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}
	state.TurnState.Turn = 30
	// 给该单位一条离线宪章红线（直接挂进传入的内存 state）→ buildDecisionAttributionContext 读到 Redline，
	// 前因快照命中 → provenance 闸放行（无需注入 sessions 仓库，applyDefeatPenalty 直接用传入 state）。
	SetUnitCharter(&state, actor.ID, OfflineCharter{
		Redlines: []CharterRedline{{Text: "护住族人是她的底线", Severity: "absolute"}},
	})

	boss := Threat{ID: "fb2", Name: "蛮荒巨蜥", Tier: ThreatTierFieldBoss}
	layer, err := service.applyDefeatPenalty(ctx, &state, &actor, boss)
	if err != nil {
		t.Fatalf("applyDefeatPenalty: %v", err)
	}
	// 高牵挂(care≥40 via loyalty=1.0)+久陪伴(daysAlive=30) → cap≥2；有前因 → 不被降级，落层2。
	if layer != 2 {
		t.Fatalf("有可解析前因的层2 候选应保留层2，得到层 %d", layer)
	}
}

// ---- ② LongTermGoals → goal_reassess ----

// TestBuildGoalReassessPrompt_IncludesLongTermGoals 验证：buildGoalReassessPrompt 读 charter.LongTermGoals 并拼入「长期目标」一节。
func TestBuildGoalReassessPrompt_IncludesLongTermGoals(t *testing.T) {
	record := unit.BootstrapRecord(40, "s1", "player", "阿织")
	state := State{ID: "s1"}
	state.TurnState.Turn = 24

	// 注意：收尾的「要求」行本就提到「长期目标」字样，故断言用**节头**（「长期目标（玩家为你立下」）区分有/无宪章。
	const goalSectionHeader = "长期目标（玩家为你立下"

	// 无宪章 → prompt 不含「长期目标」节头。
	noGoal := buildGoalReassessPrompt(state, record, 24)
	if strings.Contains(noGoal, goalSectionHeader) {
		t.Fatalf("无宪章时不应出现「长期目标」节：\n%s", noGoal)
	}

	// 立下两条长期目标 → prompt 应含「长期目标」节头且两条都在。
	SetUnitCharter(&state, record.ID, OfflineCharter{
		LongTermGoals: []string{"重建被焚的织坊", "把妹妹送去江南学医"},
	})
	withGoal := buildGoalReassessPrompt(state, record, 24)
	if !strings.Contains(withGoal, goalSectionHeader) {
		t.Fatalf("有长期目标时应出现「长期目标」节：\n%s", withGoal)
	}
	if !strings.Contains(withGoal, "重建被焚的织坊") || !strings.Contains(withGoal, "把妹妹送去江南学医") {
		t.Fatalf("两条长期目标都应拼入 prompt：\n%s", withGoal)
	}
}

// TestCharterLongTermGoals 验证 charterLongTermGoals 取值与去空白：无宪章→nil；有目标→去空白条目。
func TestCharterLongTermGoals(t *testing.T) {
	if got := charterLongTermGoals(nil, "u1"); got != nil {
		t.Fatalf("nil state 应返回 nil，得到 %v", got)
	}
	state := &State{}
	if got := charterLongTermGoals(state, "u1"); got != nil {
		t.Fatalf("无宪章应返回 nil，得到 %v", got)
	}
	SetUnitCharter(state, "u1", OfflineCharter{LongTermGoals: []string{"  攒钱赎身  ", "", "护住女儿"}})
	got := charterLongTermGoals(state, "u1")
	if len(got) != 2 || got[0] != "攒钱赎身" || got[1] != "护住女儿" {
		t.Fatalf("应去空白条目并裁剪，得到 %v", got)
	}
}

// ---- ③ 破圈预算（serendipity） ----

// TestIsZeroAnchorEvent 验证零锚来源判定：自身事件/命中锚/非陌生来源都不算；陌生人或陌生 region 且未命中锚才算。
func TestIsZeroAnchorEvent(t *testing.T) {
	owner := &unit.Record{ID: "u_owner"}

	// 自身事件 → 不算破圈。
	if isZeroAnchorEvent(FateEvent{ActorID: owner.ID, TargetID: owner.ID, SourceIsStranger: true}, owner, "") {
		t.Fatalf("自身事件不应算破圈")
	}
	// 命中了锚（anchorKind 非空）→ 非零锚。
	if isZeroAnchorEvent(FateEvent{ActorID: "stranger", SourceIsStranger: true}, owner, string(relevance.Relation)) {
		t.Fatalf("命中锚的事件不应算零锚")
	}
	// 未命中锚但来源非陌生（无 region、非 stranger）→ 不算。
	if isZeroAnchorEvent(FateEvent{ActorID: "stranger"}, owner, "") {
		t.Fatalf("非陌生来源不应算破圈种子")
	}
	// 未命中锚 + 陌生人 → 算。
	if !isZeroAnchorEvent(FateEvent{ActorID: "stranger", TargetID: "stranger", SourceIsStranger: true}, owner, "") {
		t.Fatalf("陌生人零锚事件应算破圈种子")
	}
	// 未命中锚 + 陌生 region → 算。
	if !isZeroAnchorEvent(FateEvent{ActorID: "stranger", SourceRegionID: "r_faraway"}, owner, "") {
		t.Fatalf("陌生 region 零锚事件应算破圈种子")
	}
	// owner 为 nil → 安全 false。
	if isZeroAnchorEvent(FateEvent{ActorID: "x", SourceIsStranger: true}, nil, "") {
		t.Fatalf("nil owner 应安全返回 false")
	}
}

// TestSerendipityEnabledFlag 验证 flag 默认开、显式关值与各开值识别。
func TestSerendipityEnabledFlag(t *testing.T) {
	t.Setenv("QUNXIANG_SERENDIPITY", "false")
	if serendipityEnabled() {
		t.Fatalf("显式 false 时破圈应关")
	}
	t.Setenv("QUNXIANG_SERENDIPITY", "") // 空串=未设=默认开
	if !serendipityEnabled() {
		t.Fatalf("未设置时破圈应默认开")
	}
	for _, on := range []string{"true", "1", "yes", "on", "ON", "True"} {
		t.Setenv("QUNXIANG_SERENDIPITY", on)
		if !serendipityEnabled() {
			t.Fatalf("%q 应识别为开", on)
		}
	}
	t.Setenv("QUNXIANG_SERENDIPITY", "off")
	if serendipityEnabled() {
		t.Fatalf("off 应识别为关")
	}
}

// TestSerendipityBreakout_DailyQuotaAndUpgrade 验证破圈端到端：开 flag 后，零锚来源低权事件升一档进高光卡
// 并落 ReasonSerendipityNewAnchor；当天第二件零锚来源事件因配额(≤1)用尽不再升档（恒自治、不入箱）。
func TestSerendipityBreakout_DailyQuotaAndUpgrade(t *testing.T) {
	t.Setenv("QUNXIANG_SERENDIPITY", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	owner := unit.BootstrapRecord(50, "s1", "player", "守关人")
	if err := repo.Save(ctx, owner); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 零锚来源、低权事件（importance 低、无情绪 → 本会落 RouteAutonomous）。
	seed := FateEvent{
		ActorID:          "stranger_001",
		TargetID:         "stranger_001",
		ReasonCode:       events.ReasonInboxHighlight,
		Importance:       1,
		Summary:          "一个素不相识的旅人在邻镇出了名",
		SourceRegionID:   "r_neighbor",
		SourceIsStranger: true,
	}

	r1, err := service.SurfaceFateEvent(ctx, "s1", &owner, seed)
	if err != nil {
		t.Fatalf("surface 1: %v", err)
	}
	if r1.Route != relevance.RouteHighlight {
		t.Fatalf("当天首件零锚来源事件应破圈升档进高光，得到 %s", r1.Route)
	}
	// DB 侧：应落一条 ReasonSerendipityNewAnchor。
	if n := countReason(t, db, owner.ID, events.ReasonSerendipityNewAnchor); n != 1 {
		t.Fatalf("应落 1 条 SERENDIPITY_NEW_ANCHOR，得到 %d", n)
	}

	// 第二件零锚来源事件（同一 UTC 日）→ 配额用尽，不再升档 → 恒自治。
	seed2 := seed
	seed2.ActorID = "stranger_002"
	seed2.TargetID = "stranger_002"
	r2, err := service.SurfaceFateEvent(ctx, "s1", &owner, seed2)
	if err != nil {
		t.Fatalf("surface 2: %v", err)
	}
	if r2.Route != relevance.RouteAutonomous {
		t.Fatalf("配额用尽后第二件应恒自治（不升档），得到 %s", r2.Route)
	}
	if n := countReason(t, db, owner.ID, events.ReasonSerendipityNewAnchor); n != 1 {
		t.Fatalf("配额用尽后不应再落新锚事件，仍应为 1，得到 %d", n)
	}
}

// TestSerendipityBreakout_Disabled 验证 flag 显式关时破圈零行为：零锚来源事件仍走 RouteAutonomous。
func TestSerendipityBreakout_DisabledByDefault(t *testing.T) {
	t.Setenv("QUNXIANG_SERENDIPITY", "false") // 显式关（默认已开），测关闭路径
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	owner := unit.BootstrapRecord(51, "s1", "player", "守关人")
	if err := repo.Save(ctx, owner); err != nil {
		t.Fatalf("save: %v", err)
	}
	seed := FateEvent{
		ActorID: "stranger_x", TargetID: "stranger_x", ReasonCode: events.ReasonInboxHighlight,
		Importance: 1, Summary: "陌生人的小事", SourceIsStranger: true,
	}
	r, err := service.SurfaceFateEvent(ctx, "s1", &owner, seed)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if r.Route != relevance.RouteAutonomous {
		t.Fatalf("flag 关时破圈应零行为（自治），得到 %s", r.Route)
	}
	if n := countReason(t, db, owner.ID, events.ReasonSerendipityNewAnchor); n != 0 {
		t.Fatalf("flag 关时不应落新锚事件，得到 %d", n)
	}
}

// countReason 统计某 owner 名下某 reason-code 的事件数（破圈配额断言用）。
func countReason(t *testing.T, db *sql.DB, ownerID string, code events.ReasonCode) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		ownerID, string(code),
	).Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", code, err)
	}
	return n
}
