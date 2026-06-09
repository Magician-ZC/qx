package session

// 文件说明：道德漂移结算（F2 ①）的单元/集成测试。
// 守护：①信号分类方向正确（抗命→Freedom、服从→Order、杀伤/背叛→Chaos）；②步长封顶 + 单回合单维上限；
// ③确定性（同输入同输出、不同 actor 区分）；④道德轴 clamp[0,100]；⑤全链路落库 + MORAL_DRIFT 留痕 + 背离连击累积。
// test 文件被 statuslint 白名单豁免，可直接读写字段。

import (
	"context"
	"database/sql"
	"math"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/unit"
)

// TestMoralSignalsForReasonCode_Direction 验证信号分类表的方向正确（按设计「抗命/独立→Freedom；服从→Order；杀伤/背叛→Chaos」）。
func TestMoralSignalsForReasonCode_Direction(t *testing.T) {
	cases := []struct {
		code events.ReasonCode
		axis moralAxis
	}{
		{events.ReasonGoalReassess, moralAxisFreedom},   // 独立追目标 → 自由
		{events.ReasonAmbitionShift, moralAxisFreedom},  // 野心流转 → 自由
		{events.ReasonThreatJoinAuto, moralAxisFreedom}, // 自主迎战 → 自由
		{events.ReasonThreatJoinAdvise, moralAxisOrder}, // 受嘱参战 → 秩序
		{events.ReasonCharterActivated, moralAxisOrder}, // 依章尽责 → 秩序
		{events.ReasonCombatDown, moralAxisChaos},       // 重度杀伤 → 混乱
		{events.ReasonCombatHit, moralAxisChaos},        // 寻常杀伤 → 混乱
		{events.ReasonCrossBetrayal, moralAxisChaos},    // 背叛毁约 → 混乱
		{events.ReasonVengeanceFulfilled, moralAxisChaos},
	}
	for _, c := range cases {
		sigs := moralSignalsForReasonCode(c.code)
		if len(sigs) == 0 {
			t.Fatalf("reason %s 应产出至少一条道德信号", c.code)
		}
		if sigs[0].Axis != c.axis {
			t.Fatalf("reason %s 应推向 %s，得到 %s", c.code, c.axis, sigs[0].Axis)
		}
	}
	// 与道德无关的 code → 零信号（如部署/天气类不在表内）。
	if sigs := moralSignalsForReasonCode(events.ReasonCode("SOME_UNRELATED_CODE")); len(sigs) != 0 {
		t.Fatalf("无关 reason 应产出零信号，得到 %d", len(sigs))
	}
}

// TestMoralDriftStep_Bounds 验证单信号步长 ∈ [floor·strength, cap·strength]，确定性，不同 actor 区分。
func TestMoralDriftStep_Bounds(t *testing.T) {
	sig := moralSignal{Axis: moralAxisChaos, Strength: 1.0, Reason: "X"}
	for turn := 0; turn < 100; turn++ {
		s := moralDriftStep("sess", turn, "u1", sig)
		if s < moralDriftStepFloor-1e-9 || s > moralDriftStepCap+1e-9 {
			t.Fatalf("步长越界 turn=%d step=%.4f 不在 [%.1f,%.1f]", turn, s, moralDriftStepFloor, moralDriftStepCap)
		}
	}
	// 强度缩放：半强度信号上界应为 cap/2。
	half := moralSignal{Axis: moralAxisChaos, Strength: 0.5, Reason: "X"}
	for turn := 0; turn < 50; turn++ {
		if s := moralDriftStep("sess", turn, "u1", half); s > moralDriftStepCap*0.5+1e-9 {
			t.Fatalf("半强度步长越界 %.4f > %.4f", s, moralDriftStepCap*0.5)
		}
	}
	// 确定性：同输入同输出。
	if moralDriftStep("s", 4, "a", sig) != moralDriftStep("s", 4, "a", sig) {
		t.Fatalf("步长非确定性")
	}
	// 不同 actor 区分。
	same := 0
	for turn := 0; turn < 30; turn++ {
		if moralDriftStep("s", turn, "actorA", sig) == moralDriftStep("s", turn, "actorB", sig) {
			same++
		}
	}
	if same == 30 {
		t.Fatalf("不同 actor 全部同值，哈希未区分")
	}
}

// TestAccumulateMoralDrift_PerTurnCap 验证同回合多信号累加后单维不超 moralDriftPerTurnCap，方向恒为加。
func TestAccumulateMoralDrift_PerTurnCap(t *testing.T) {
	// 堆 10 条满强度 Chaos 信号（远超单维上限），累加后 Chaos 应被夹到 cap。
	signals := make([]moralSignal, 0, 10)
	for i := 0; i < 10; i++ {
		signals = append(signals, moralSignal{Axis: moralAxisChaos, Strength: 1.0, Reason: "C"})
	}
	d := accumulateMoralDrift("sess", 1, "u", signals)
	if d.Chaos > moralDriftPerTurnCap+1e-9 {
		t.Fatalf("Chaos 单回合累计 %.4f 超上限 %.1f", d.Chaos, moralDriftPerTurnCap)
	}
	if d.Chaos <= 0 {
		t.Fatalf("10 条 Chaos 信号应有正增量，得到 %.4f", d.Chaos)
	}
	if d.Freedom != 0 || d.Order != 0 {
		t.Fatalf("纯 Chaos 信号不应动其它轴：F=%.4f O=%.4f", d.Freedom, d.Order)
	}
	// 确定性。
	if accumulateMoralDrift("s", 2, "u", signals) != accumulateMoralDrift("s", 2, "u", signals) {
		t.Fatalf("累加非确定性")
	}
}

// TestRefreshMoralDriftStreak 验证背离连击：主导≠当前→+1；一致/无主导/无阵营→归 0。
func TestRefreshMoralDriftStreak(t *testing.T) {
	svc := &Service{}
	// 当前 freedom，但道德轴主导 chaos（背离）→ streak 累加。
	rec := &unit.Record{Faction: faction.IDFreedom, MoralAlignment: faction.MoralAlignment{Freedom: 10, Order: 10, Chaos: 80}}
	svc.refreshMoralDriftStreak(rec)
	if rec.MoralDriftStreak != 1 {
		t.Fatalf("背离首回合 streak 应为 1，得到 %d", rec.MoralDriftStreak)
	}
	svc.refreshMoralDriftStreak(rec)
	if rec.MoralDriftStreak != 2 {
		t.Fatalf("背离持续 streak 应为 2，得到 %d", rec.MoralDriftStreak)
	}
	// 主导回到与当前一致 → 归 0。
	rec.MoralAlignment = faction.MoralAlignment{Freedom: 80, Order: 10, Chaos: 10}
	svc.refreshMoralDriftStreak(rec)
	if rec.MoralDriftStreak != 0 {
		t.Fatalf("一致后 streak 应归 0，得到 %d", rec.MoralDriftStreak)
	}
	// 无阵营 → 归 0（即便主导存在）。
	rec.Faction = ""
	rec.MoralDriftStreak = 5
	svc.refreshMoralDriftStreak(rec)
	if rec.MoralDriftStreak != 0 {
		t.Fatalf("无当前阵营应归 0，得到 %d", rec.MoralDriftStreak)
	}
}

// TestSettleMoralDrift_PersistAndAudit 全链路：写本回合事件→漂移落库→MORAL_DRIFT 留痕→轴 clamp 在界→连击累积。
func TestSettleMoralDrift_PersistAndAudit(t *testing.T) {
	db, repo, service := newDriftTestService(t)
	ctx := context.Background()

	rec := unit.BootstrapRecord(7, "s-moral", "player", "阿玖")
	rec.Faction = faction.IDFreedom
	rec.MoralAlignment = faction.MoralAlignment{Freedom: 70, Order: 15, Chaos: 15}
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 本回合写两条「她作为 actor」的杀伤事件（→Chaos），模拟战斗杀伤信号源。
	turn := 5
	seedActorEvent(t, db, "s-moral", rec.ID, events.ReasonCombatDown, turn)
	seedActorEvent(t, db, "s-moral", rec.ID, events.ReasonCrossBetrayal, turn)

	changes, err := service.settleMoralDrift(ctx, "s-moral", &rec, turn)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(changes) == 0 {
		t.Fatalf("两条 Chaos 信号应至少动一轴")
	}
	// 方向：应只动 Chaos（信号全是 Chaos）。
	for _, c := range changes {
		if c.Axis != string(moralAxisChaos) {
			t.Fatalf("纯 Chaos 信号不应动 %s 轴", c.Axis)
		}
		if c.Delta <= 0 {
			t.Fatalf("Chaos 应正增量，得到 %.4f", c.Delta)
		}
	}

	// 落库后轴在 [0,100]，且 Chaos 确有上升。
	after, err := repo.GetByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("reget: %v", err)
	}
	if after.MoralAlignment.Chaos <= 15 {
		t.Fatalf("Chaos 应上升，得到 %.2f", after.MoralAlignment.Chaos)
	}
	for _, v := range []float64{after.MoralAlignment.Freedom, after.MoralAlignment.Order, after.MoralAlignment.Chaos} {
		if v < 0 || v > 100 {
			t.Fatalf("道德轴越界 %.2f", v)
		}
	}
	// MORAL_DRIFT 留痕恰 1 条。
	if n := reasonEventCount(t, db, rec.ID, events.ReasonMoralDrift); n != 1 {
		t.Fatalf("应留 1 条 MORAL_DRIFT，得到 %d", n)
	}
}

// TestSettleMoralDrift_NoSignalNoOp 验证本回合无道德信号 → 不漂、不留痕（但仍刷新连击）。
func TestSettleMoralDrift_NoSignalNoOp(t *testing.T) {
	db, repo, service := newDriftTestService(t)
	ctx := context.Background()
	rec := unit.BootstrapRecord(3, "s-noop", "player", "阿三")
	rec.Faction = faction.IDOrder
	rec.MoralAlignment = faction.MoralAlignment{Order: 70, Freedom: 15, Chaos: 15}
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	changes, err := service.settleMoralDrift(ctx, "s-noop", &rec, 9)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("无信号应零漂移，得到 %d", len(changes))
	}
	if n := reasonEventCount(t, db, rec.ID, events.ReasonMoralDrift); n != 0 {
		t.Fatalf("无漂移不应留痕，得到 %d", n)
	}
}

// TestSettleMoralDrift_NilSafe 验证 nil/空守护不 panic。
func TestSettleMoralDrift_NilSafe(t *testing.T) {
	var nilService *Service
	if _, err := nilService.settleMoralDrift(context.Background(), "s", &unit.Record{ID: "x"}, 1); err != nil {
		t.Fatalf("nil service 应安全返回，得到 %v", err)
	}
	_, _, service := newDriftTestService(t)
	if _, err := service.settleMoralDrift(context.Background(), "s", nil, 1); err != nil {
		t.Fatalf("nil record 应安全返回，得到 %v", err)
	}
}

// seedActorEvent 往 events 表写一条「actor=unitID、tick=turn」的事件（喂 moralSignalsThisTurn 查询）。
func seedActorEvent(t *testing.T, db *sql.DB, sessionID, unitID string, code events.ReasonCode, turn int) {
	t.Helper()
	if _, err := events.EmitProcessEvent(context.Background(), db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		Code:        code,
		Category:    events.CategoryLifecycle,
		Payload:     map[string]any{"seed": true},
		Tick:        turn,
	}); err != nil {
		t.Fatalf("seed event %s: %v", code, err)
	}
}

// reasonEventCount 统计某单位某 reason code 的事件条数。
func reasonEventCount(t *testing.T, db *sql.DB, unitID string, code events.ReasonCode) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		unitID, string(code),
	).Scan(&n); err != nil {
		t.Fatalf("统计 %s 事件失败: %v", code, err)
	}
	return n
}

// 防御：roundTo2 不引入大误差。
var _ = math.Abs
