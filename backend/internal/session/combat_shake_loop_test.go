package session

// 文件说明：combat_shake 高压情绪覆写接入决策主循环（resolveExecution）的聚焦测试。
// 主循环插桩本身依赖完整 DB/LLM，难以无副作用单测；前半段验证插桩所依赖的纯逻辑契约：
// 触发时 OverrideDecision 替换原决策、applyCombatShakeOverlay 富化文本、Modifiers 乘性合流；
// 未触发时决策与倍率原样透传（零行为变化）。命名带 combatShakeLoop 前缀避免撞名。
//
// 后半段（审计测试 D）是对真实 SQLite + stub LLM 的端到端集成测试：直接驱动 resolveCombatShake
// （主循环 service.go:1723 的同一调用点），跑通真实 generateCombatShakeChoice（真实 LLM stub 调用 +
// 真实 JSON 解码/归一/校验）、真实 Mutator 批写留痕（surrender → events 表）、真实关键记忆落库
// （emotional_override → memories 表）、真实 rage 战斗效果落库，再复刻主循环 1730-1740 的接线
// （OverrideDecision 改写 → overlay 富化 → 三路倍率乘性合流）做断言；未触发时断言零副作用、零 LLM。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/ai"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// combatShakeLoopApplyOverride 复刻 service.go 主循环插桩在 Triggered 时的决策改写顺序：
// 先用 OverrideDecision 替换，再用 applyCombatShakeOverlay 富化文本。
func combatShakeLoopApplyOverride(normal unitDecisionPayload, resolution combatShakeResolution) unitDecisionPayload {
	decision := normal
	if resolution.Triggered {
		if resolution.OverrideDecision != nil {
			decision = *resolution.OverrideDecision
		}
		decision = applyCombatShakeOverlay(decision, resolution)
	}
	return decision
}

// TestCombatShakeLoopOverridesDecisionWhenTriggered 断言触发且带 OverrideDecision 时，
// 主循环用覆写决策取代正常决策（例如撤退/狂暴），原 LLM 动作被丢弃。
func TestCombatShakeLoopOverridesDecisionWhenTriggered(t *testing.T) {
	normal := unitDecisionPayload{
		Action:     DecisionActionMove,
		TargetQ:    5,
		TargetR:    6,
		Speak:      "原计划前压",
		Memory:     "继续推进",
		Reasoning:  "敌人露出破绽",
		NextAction: "前压",
	}
	override := unitDecisionPayload{
		Action:       DecisionActionAttack,
		TargetUnitID: "enemy-1",
	}
	resolution := combatShakeResolution{
		Triggered:        true,
		OverrideDecision: &override,
		Choice: combatShakeChoicePayload{
			Bubble:    "为他报仇！",
			Memory:    "我看见队友倒下",
			Reasoning: "怒火盖过理智",
		},
		Modifiers: actionModifiers{MoveMultiplier: 0.95, AttackMultiplier: 1.15},
	}

	final := combatShakeLoopApplyOverride(normal, resolution)

	if final.Action != DecisionActionAttack {
		t.Fatalf("触发覆写后动作应为 attack，得到 %q", final.Action)
	}
	if final.TargetUnitID != "enemy-1" {
		t.Fatalf("覆写决策的目标应保留为 enemy-1，得到 %q", final.TargetUnitID)
	}
	// 原决策的移动目标必须被丢弃（不能泄漏回最终决策）。
	if final.TargetQ == 5 && final.TargetR == 6 {
		t.Fatalf("覆写后不应保留原 LLM 决策的移动坐标")
	}
	// applyCombatShakeOverlay 富化文本：override 自身无 Speak/Memory，应补入 Choice 文本。
	if final.Speak != "为他报仇！" {
		t.Fatalf("覆写决策的 Speak 应由 shake choice 补入，得到 %q", final.Speak)
	}
	if final.Memory != "我看见队友倒下" {
		t.Fatalf("覆写决策的 Memory 应由 shake choice 补入，得到 %q", final.Memory)
	}
	if final.Reasoning == "" {
		t.Fatalf("覆写决策的 Reasoning 应包含 shake choice 的推理")
	}
}

// TestCombatShakeLoopOverlayWithoutOverride 断言触发但无 OverrideDecision（例如英雄主义只给倍率）时，
// 正常决策的动作保留，仅文本被富化、空字段被补全。
func TestCombatShakeLoopOverlayWithoutOverride(t *testing.T) {
	normal := unitDecisionPayload{
		Action:    DecisionActionAttack,
		Speak:     "", // 留空以验证 overlay 补入
		Reasoning: "正面迎敌",
	}
	resolution := combatShakeResolution{
		Triggered:        true,
		OverrideDecision: nil,
		Choice: combatShakeChoicePayload{
			Bubble:    "挡在他们前面",
			Reasoning: "不能让友军白白牺牲",
		},
		Modifiers: actionModifiers{MoveMultiplier: 1.05, AttackMultiplier: 1.1},
	}

	final := combatShakeLoopApplyOverride(normal, resolution)

	if final.Action != DecisionActionAttack {
		t.Fatalf("无覆写时应保留原决策动作，得到 %q", final.Action)
	}
	if final.Speak != "挡在他们前面" {
		t.Fatalf("空 Speak 应由 overlay 补入，得到 %q", final.Speak)
	}
	if final.Reasoning == "正面迎敌" {
		t.Fatalf("overlay 应把 shake 推理拼接进 Reasoning")
	}
}

// TestCombatShakeLoopUntriggeredIsPassthrough 断言未触发时决策原样透传（零行为变化）。
func TestCombatShakeLoopUntriggeredIsPassthrough(t *testing.T) {
	normal := unitDecisionPayload{
		Action:    DecisionActionMove,
		TargetQ:   3,
		TargetR:   4,
		Speak:     "保持阵型",
		Memory:    "稳住",
		Reasoning: "无异常",
	}
	resolution := combatShakeResolution{
		Triggered: false,
		// 未触发时 Modifiers 默认中性。
		Modifiers: actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1},
	}

	final := combatShakeLoopApplyOverride(normal, resolution)

	if final != normal {
		t.Fatalf("未触发时决策必须原样透传，期望 %+v，得到 %+v", normal, final)
	}
}

// TestCombatShakeLoopModifiersFold 断言主循环把 compliance / 饥饿 / shake 三路倍率乘性合流。
// 复刻 service.go 的 combineActionModifiers(compliance.Modifiers, hungerActionModifiers, shakeResolution.Modifiers)。
func TestCombatShakeLoopModifiersFold(t *testing.T) {
	complianceMods := actionModifiers{MoveMultiplier: 0.9, AttackMultiplier: 1.0}
	// 构造饥饿单位（Hunger<30 → 0.8/0.8）。
	hungry := unit.Record{}
	hungry.Status.Hunger = 10
	shakeMods := actionModifiers{MoveMultiplier: 0.95, AttackMultiplier: 1.15}

	combined := combineActionModifiers(complianceMods, hungerActionModifiers(hungry), shakeMods)

	wantMove := 0.9 * 0.8 * 0.95
	wantAttack := 1.0 * 0.8 * 1.15
	if !floatNear(combined.MoveMultiplier, wantMove) {
		t.Fatalf("MoveMultiplier 合流错误，期望 %.4f，得到 %.4f", wantMove, combined.MoveMultiplier)
	}
	if !floatNear(combined.AttackMultiplier, wantAttack) {
		t.Fatalf("AttackMultiplier 合流错误，期望 %.4f，得到 %.4f", wantAttack, combined.AttackMultiplier)
	}

	// 未触发时 shake 倍率为中性，不应改变合流结果。
	neutralShake := actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1}
	withNeutral := combineActionModifiers(complianceMods, hungerActionModifiers(hungry), neutralShake)
	if !floatNear(withNeutral.MoveMultiplier, 0.9*0.8) || !floatNear(withNeutral.AttackMultiplier, 1.0*0.8) {
		t.Fatalf("中性 shake 倍率不应改变合流，得到 %+v", withNeutral)
	}
}

// floatNear 浮点近似比较，避免乘法精度抖动。
func floatNear(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff < 1e-9
}

// ===== 审计测试 D：resolveCombatShake 对真实 SQLite + stub LLM 的端到端集成 =====

// combatShakeStubLLM 是只为 combat_shake 路径服务的 stub completionClient：
// 仅认 session_combat_shake_choice schema 的请求并回固定合法 JSON（绕开真实网络与 gojsonschema，
// 因为注入 stub 后 ai.Service 的强校验不在链路上——见 generateCombatShakeChoice 直接 Unmarshal stub.Output）；
// 其它 schema 的调用一律报错（本测试只直接调 resolveCombatShake，不应触达单位决策那条 LLM 路径）。
// 同时统计真实发起的调用次数，供「未触发零 LLM」断言。
type combatShakeStubLLM struct {
	shakeJSON string
	calls     int
}

func (s *combatShakeStubLLM) GenerateJSON(_ context.Context, req ai.CompletionRequest) (ai.CompletionResult, error) {
	s.calls++
	if req.SchemaName != "session_combat_shake_choice" {
		return ai.CompletionResult{}, &combatShakeStubUnexpected{schema: req.SchemaName}
	}
	return ai.CompletionResult{
		Provider: "stub",
		Model:    "stub-shake",
		Output:   []byte(s.shakeJSON),
	}, nil
}

// combatShakeStubUnexpected 标记 stub 收到了非预期 schema 的请求（让断言能看出链路跑偏）。
type combatShakeStubUnexpected struct{ schema string }

func (e *combatShakeStubUnexpected) Error() string {
	return "combat shake stub 收到非预期 schema: " + e.schema
}

// newCombatShakeLoopTestService 起一个临时 SQLite + 完整 Service（含 sessions/units/mutator），注入 stub LLM。
func newCombatShakeLoopTestService(t *testing.T, llm completionClient) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "combat_shake_loop.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := NewServiceWithColdStore(db, llm, nil)
	return db, service.units, service
}

// combatShakeLoopActor 构造一个战斗就绪、可控触发器的角色记录。
func combatShakeLoopActor(name, factionID string) unit.Record {
	actor := unit.BootstrapRecord(2, "s-shake", factionID, name)
	actor.Status.LifeState = unit.LifeStateActive
	actor.Status.LivesRemaining = 3
	actor.Status.HP = 100
	actor.Status.Hunger = 100
	actor.Status.Morale = 0.8
	actor.Status.Attack = 20
	actor.Status.Defense = 5
	actor.Status.PositionQ = 0
	actor.Status.PositionR = 0
	return actor
}

// eventCountByReason 统计 events 表中指定 reason_code 的行数，用于验证状态变更确实经 Mutator 留痕。
func eventCountByReason(t *testing.T, db *sql.DB, reasonCode string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE reason_code = ?`, reasonCode).Scan(&n); err != nil {
		t.Fatalf("按 reason_code=%s 统计 events 失败: %v", reasonCode, err)
	}
	return n
}

// memoryCount 统计指定单位的 memories 行数，用于验证关键情绪覆写记忆确实落库。
func memoryCount(t *testing.T, db *sql.DB, unitID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE unit_id = ?`, unitID).Scan(&n); err != nil {
		t.Fatalf("统计 memories 失败: %v", err)
	}
	return n
}

// TestCombatShakeLoopE2E_SurrenderOverridesAndPersists 端到端（真实 DB+LLM stub）：
// 低血触发 → LLM stub 回 surrender → resolveCombatShake 真实落 Mutator 批写（Morale/Loyalty）与关键记忆，
// 产出 HOLD 覆写决策；再复刻主循环接线断言覆写动作、overlay 富化、三路倍率乘性合流。
func TestCombatShakeLoopE2E_SurrenderOverridesAndPersists(t *testing.T) {
	stub := &combatShakeStubLLM{shakeJSON: `{"action":"surrender","bubble":"我撑不住了……","memory":"那一刻我彻底崩了","reasoning":"敌势太盛，再打必死"}`}
	db, repo, service := newCombatShakeLoopTestService(t, stub)
	ctx := context.Background()

	actor := combatShakeLoopActor("阿采", "player")
	actor.Status.HP = 20 // <=30 → 触发「血量低于30%」，无需地图/阵营即可命中触发器
	morale0 := actor.Status.Morale
	loyalty0 := actor.Status.Loyalty
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}

	state := &State{ID: "s-shake", PlayerFactionID: "player"}
	state.TurnState.Turn = 1
	byID := mapRecordsByID([]unit.Record{actor})

	resolution, err := service.resolveCombatShake(ctx, state, byID, byID[actor.ID])
	if err != nil {
		t.Fatalf("resolveCombatShake 出错: %v", err)
	}

	// 1) 触发 + LLM stub 真实被调一次。
	if !resolution.Triggered {
		t.Fatalf("低血应触发 combat shake（triggers 非空）")
	}
	if stub.calls != 1 {
		t.Fatalf("应恰好发起 1 次 shake LLM 调用，实际 %d", stub.calls)
	}
	// 2) surrender → 产出 HOLD 覆写决策，携带 stub 文本。
	if resolution.OverrideDecision == nil {
		t.Fatalf("surrender 应产出覆写决策")
	}
	if resolution.OverrideDecision.Action != DecisionActionHold {
		t.Fatalf("surrender 覆写动作应为 hold，得到 %q", resolution.OverrideDecision.Action)
	}
	// 3) Mutator 真实批写留痕：events 表应各有一条 morale(EMOTION_TRAUMA) 与 loyalty(COMMAND_FORCED_ORDER)。
	if got := eventCountByReason(t, db, "EMOTION_TRAUMA"); got < 1 {
		t.Fatalf("surrender 应经 Mutator 写一条 EMOTION_TRAUMA 士气留痕，得到 %d", got)
	}
	if got := eventCountByReason(t, db, "COMMAND_FORCED_ORDER"); got < 1 {
		t.Fatalf("surrender 应经 Mutator 写一条 COMMAND_FORCED_ORDER 忠诚留痕，得到 %d", got)
	}
	// 4) 落库的士气/忠诚确实下降（证明走 Mutator 而非纯内存）。
	reloaded, err := repo.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("回读角色失败: %v", err)
	}
	if !(reloaded.Status.Morale < morale0) {
		t.Fatalf("surrender 后士气应下降：起始 %v，落库 %v", morale0, reloaded.Status.Morale)
	}
	if !(reloaded.Status.Loyalty < loyalty0) {
		t.Fatalf("surrender 后忠诚应下降：起始 %v，落库 %v", loyalty0, reloaded.Status.Loyalty)
	}
	// 5) 关键情绪覆写记忆落库（collapse → rememberEmotionalOverrideCritical 插入 memories）。
	if got := memoryCount(t, db, actor.ID); got < 1 {
		t.Fatalf("surrender(collapse) 应写一条关键情绪覆写记忆，得到 %d", got)
	}
	// 6) collapse 情绪覆写：倍率被乘性合入（0.9/0.85），不再中性。
	if resolution.Emotion != emotionalOverrideCollapse {
		t.Fatalf("surrender 应推断为 collapse 情绪覆写，得到 %q", resolution.Emotion)
	}
	if floatNear(resolution.Modifiers.MoveMultiplier, 1) || floatNear(resolution.Modifiers.AttackMultiplier, 1) {
		t.Fatalf("collapse 覆写后倍率不应中性，得到 %+v", resolution.Modifiers)
	}

	// 7) 复刻主循环接线（service.go:1730-1740）：override → overlay → 三路倍率乘性合流。
	normal := unitDecisionPayload{Action: DecisionActionMove, TargetQ: 9, TargetR: 9, Reasoning: "原计划"}
	final := combatShakeLoopApplyOverride(normal, resolution)
	if final.Action != DecisionActionHold {
		t.Fatalf("接线后最终动作应被 surrender 覆写为 hold，得到 %q", final.Action)
	}
	if final.TargetQ == 9 && final.TargetR == 9 {
		t.Fatalf("覆写后不应保留原 LLM 决策的移动坐标")
	}
	if final.Speak != "我撑不住了……" {
		t.Fatalf("overlay 应补入 stub 的 Speak，得到 %q", final.Speak)
	}
	// 三路倍率乘性合流：compliance(中性) × 饥饿(满食=中性) × shake(collapse=0.9/0.85)。
	combined := combineActionModifiers(
		actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1},
		hungerActionModifiers(*byID[actor.ID]),
		resolution.Modifiers,
	)
	if !floatNear(combined.MoveMultiplier, resolution.Modifiers.MoveMultiplier) ||
		!floatNear(combined.AttackMultiplier, resolution.Modifiers.AttackMultiplier) {
		t.Fatalf("满食+中性 compliance 下合流应等于 shake 倍率本身，得到 %+v vs shake %+v", combined, resolution.Modifiers)
	}
	// 攻击倍率确被 collapse 压低 (<1)，移动倍率也被压低 (<1)。
	if !(combined.AttackMultiplier < 1) || !(combined.MoveMultiplier < 1) {
		t.Fatalf("collapse 合流后移动/攻击倍率都应 <1，得到 %+v", combined)
	}
}

// TestCombatShakeLoopE2E_RagePersistsEffectAndBoostsAttack 端到端（真实 DB+LLM stub）：
// 低血 + 有可见敌 → LLM stub 回 rage → 覆写决策应攻击该敌、rage 战斗效果落库、复仇情绪把攻击倍率乘到 >1。
func TestCombatShakeLoopE2E_RagePersistsEffectAndBoostsAttack(t *testing.T) {
	stub := &combatShakeStubLLM{shakeJSON: `{"action":"rage","bubble":"我跟你拼了！","memory":"怒火烧穿了恐惧","reasoning":"必须打回去"}`}
	db, repo, service := newCombatShakeLoopTestService(t, stub)
	ctx := context.Background()

	actor := combatShakeLoopActor("怒者", "player")
	actor.Status.HP = 20 // 触发血量低于30%
	actor.Status.PositionQ = 0
	actor.Status.PositionR = 0
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	enemy := combatShakeLoopActor("山贼", "enemy")
	enemy.Status.PositionQ = 1
	enemy.Status.PositionR = 0
	if err := repo.Save(ctx, enemy); err != nil {
		t.Fatalf("保存敌人失败: %v", err)
	}

	// 关战关系：opposedUnitIDs 仅在 player↔enemy 为 war 时才返回敌方 ID，rage 才能选中目标。无雾(默认)→可见敌==全体敌。
	state := &State{
		ID:               "s-shake",
		PlayerFactionID:  "player",
		EnemyFactionID:   "enemy",
		PlayerUnitIDs:    []string{actor.ID},
		EnemyUnitIDs:     []string{enemy.ID},
		FactionRelations: []FactionRelation{{LeftFactionID: "player", RightFactionID: "enemy", State: FactionRelationWar}},
	}
	state.TurnState.Turn = 1
	byID := mapRecordsByID([]unit.Record{actor, enemy})

	resolution, err := service.resolveCombatShake(ctx, state, byID, byID[actor.ID])
	if err != nil {
		t.Fatalf("resolveCombatShake 出错: %v", err)
	}

	if !resolution.Triggered {
		t.Fatalf("低血应触发 combat shake")
	}
	if resolution.OverrideDecision == nil {
		t.Fatalf("rage 在有可见敌时应产出覆写决策")
	}
	if resolution.OverrideDecision.Action != DecisionActionAttack {
		t.Fatalf("rage 覆写动作应为 attack，得到 %q", resolution.OverrideDecision.Action)
	}
	if resolution.OverrideDecision.TargetUnitID != enemy.ID {
		t.Fatalf("rage 覆写应攻击最近可见敌 %s，得到 %q", enemy.ID, resolution.OverrideDecision.TargetUnitID)
	}
	// rage 战斗效果应落库（下回合到期）。
	reloaded, err := repo.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("回读角色失败: %v", err)
	}
	if !hasCombatEffect(reloaded, combatEffectRage, state.TurnState.Turn+1) {
		t.Fatalf("rage 应把 combat:rage 效果落库（debuffs=%v）", reloaded.Status.Debuffs)
	}
	// revenge 情绪覆写：攻击倍率乘到 >1（0.95×1.15=1.0925），移动倍率 <1。
	if resolution.Emotion != emotionalOverrideRevenge {
		t.Fatalf("rage 应推断为 revenge 情绪覆写，得到 %q", resolution.Emotion)
	}
	if !(resolution.Modifiers.AttackMultiplier > 1) {
		t.Fatalf("revenge 合流后攻击倍率应 >1，得到 %v", resolution.Modifiers.AttackMultiplier)
	}
	if !(resolution.Modifiers.MoveMultiplier < 1) {
		t.Fatalf("revenge 合流后移动倍率应 <1，得到 %v", resolution.Modifiers.MoveMultiplier)
	}
	// 关键情绪覆写记忆落库。
	if got := memoryCount(t, db, actor.ID); got < 1 {
		t.Fatalf("rage(revenge) 应写一条关键情绪覆写记忆，得到 %d", got)
	}

	// 复刻主循环接线：override(attack) → overlay 补 Speak → 三路倍率乘性合流仍 >1（攻击）。
	normal := unitDecisionPayload{Action: DecisionActionMove, TargetQ: 5, TargetR: 5}
	final := combatShakeLoopApplyOverride(normal, resolution)
	if final.Action != DecisionActionAttack || final.TargetUnitID != enemy.ID {
		t.Fatalf("接线后最终决策应为攻击该敌，得到 action=%q target=%q", final.Action, final.TargetUnitID)
	}
	combined := combineActionModifiers(
		actionModifiers{MoveMultiplier: 1, AttackMultiplier: 1},
		hungerActionModifiers(reloaded),
		resolution.Modifiers,
	)
	if !(combined.AttackMultiplier > 1) {
		t.Fatalf("revenge 三路合流后攻击倍率应仍 >1，得到 %v", combined.AttackMultiplier)
	}
}

// TestCombatShakeLoopE2E_UntriggeredZeroSideEffects 端到端（真实 DB+LLM stub）：
// 平静角色（满血满士气、无敌压、无负面记忆）→ resolveCombatShake 在触发器判定阶段早返回：
// 零 LLM 调用、零 events 留痕、零 memories、中性倍率、无覆写；复刻接线后决策原样透传。
func TestCombatShakeLoopE2E_UntriggeredZeroSideEffects(t *testing.T) {
	stub := &combatShakeStubLLM{shakeJSON: `{"action":"surrender","bubble":"x","memory":"y","reasoning":"z"}`}
	db, repo, service := newCombatShakeLoopTestService(t, stub)
	ctx := context.Background()

	actor := combatShakeLoopActor("沉静", "player") // HP=100 Morale=0.8 Hunger=100 → 无任何触发器
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}

	state := &State{ID: "s-shake", PlayerFactionID: "player"}
	state.TurnState.Turn = 1
	byID := mapRecordsByID([]unit.Record{actor})

	resolution, err := service.resolveCombatShake(ctx, state, byID, byID[actor.ID])
	if err != nil {
		t.Fatalf("resolveCombatShake 出错: %v", err)
	}

	if resolution.Triggered {
		t.Fatalf("平静角色不应触发 combat shake")
	}
	if stub.calls != 0 {
		t.Fatalf("未触发时绝不应发起 shake LLM 调用，实际 %d", stub.calls)
	}
	if resolution.OverrideDecision != nil {
		t.Fatalf("未触发时不应有覆写决策")
	}
	if !floatNear(resolution.Modifiers.MoveMultiplier, 1) || !floatNear(resolution.Modifiers.AttackMultiplier, 1) {
		t.Fatalf("未触发时倍率应中性 {1,1}，得到 %+v", resolution.Modifiers)
	}
	// 零副作用：无任何状态留痕、无记忆。
	if got := eventCount(t, db); got != 0 {
		t.Fatalf("未触发时不应写任何 events 留痕，得到 %d", got)
	}
	if got := memoryCount(t, db, actor.ID); got != 0 {
		t.Fatalf("未触发时不应写任何记忆，得到 %d", got)
	}
	// 落库状态未被改动。
	reloaded, err := repo.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("回读角色失败: %v", err)
	}
	if reloaded.Status.Morale != actor.Status.Morale || reloaded.Status.Loyalty != actor.Status.Loyalty {
		t.Fatalf("未触发时状态不应变化：morale %v→%v loyalty %v→%v",
			actor.Status.Morale, reloaded.Status.Morale, actor.Status.Loyalty, reloaded.Status.Loyalty)
	}

	// 复刻主循环接线：未触发 → 决策原样透传。
	normal := unitDecisionPayload{Action: DecisionActionMove, TargetQ: 3, TargetR: 4, Speak: "保持阵型", Reasoning: "无异常"}
	final := combatShakeLoopApplyOverride(normal, resolution)
	if final != normal {
		t.Fatalf("未触发时决策必须原样透传，期望 %+v，得到 %+v", normal, final)
	}
}
