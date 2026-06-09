// 文件说明：零和监控审计的 DB 集成测试（按付费态分组胜率 + 红线判定 + 付费不进 Score 的观测语义 + 区间过滤）。
package liveops

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"

	"qunxiang/backend/internal/engine/events"
)

// ensureUnit 幂等插入一个最小单位行（events.actor_unit_id 有 FK→units(id)，sqlite 这里默认开启 FK 强制）。
func ensureUnit(t *testing.T, db *sql.DB, actorID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO units (id, session_id, faction_id, display_name)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		actorID, "sess-1", "faction-1", actorID); err != nil {
		t.Fatalf("插入单位失败: %v", err)
	}
}

// insertOutcome 直接往 events 表写一条争夺结局（绕过 Mutator，仅测试用）。先确保 actor 单位存在以满足 FK。
func insertOutcome(t *testing.T, db *sql.DB, worldID, actorID string, tick int, reason events.ReasonCode) {
	t.Helper()
	ensureUnit(t, db, actorID)
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO events (id, session_id, actor_unit_id, event_type, reason_code, world_id, tick)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), "sess-1", actorID, "lifecycle", string(reason), worldID, tick); err != nil {
		t.Fatalf("写结局事件失败: %v", err)
	}
}

// paidResolverFor 返回一个把指定 actor 视为付费的 PaidResolver（仅供审计分组，绝不进 Score）。
func paidResolverFor(paidActors ...string) PaidResolver {
	set := map[string]bool{}
	for _, a := range paidActors {
		set[a] = true
	}
	return func(_ context.Context, actorID string) bool { return set[actorID] }
}

func TestAuditArbitration_PaidWinRateRedline(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-audit"

	// 付费角色 p1 在 20 场争夺里赢 15、输 5（胜率 75% > 60% 红线）。
	for i := 0; i < 15; i++ {
		insertOutcome(t, db, worldID, "p1", 10, events.ReasonCrossContestWin)
	}
	for i := 0; i < 5; i++ {
		insertOutcome(t, db, worldID, "p1", 10, events.ReasonCrossContestLose)
	}
	// 非付费 n1：赢 5 输 5（50%）。
	for i := 0; i < 5; i++ {
		insertOutcome(t, db, worldID, "n1", 10, events.ReasonCrossContestWin)
	}
	for i := 0; i < 5; i++ {
		insertOutcome(t, db, worldID, "n1", 10, events.ReasonCrossContestLose)
	}

	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.Total != 20 || rep.Paid.Wins != 15 {
		t.Fatalf("付费组统计不符: %+v", rep.Paid)
	}
	if rep.Paid.WinRate <= 0.74 || rep.Paid.WinRate >= 0.76 {
		t.Fatalf("付费组胜率应 ≈0.75，得到 %f", rep.Paid.WinRate)
	}
	if rep.NonPaid.Total != 10 || rep.NonPaid.WinRate != 0.5 {
		t.Fatalf("非付费组统计不符: %+v", rep.NonPaid)
	}
	if !rep.SampleSufficient {
		t.Fatalf("付费组 20 样本应达红线门槛")
	}
	if !rep.IssueDetected {
		t.Fatalf("付费组 75%% 应判红线 IssueDetected")
	}
}

func TestAuditArbitration_NoRedlineWhenFair(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-fair"
	// 付费 p1：20 场各赢 10 输 10（50%，公平）。
	for i := 0; i < 10; i++ {
		insertOutcome(t, db, worldID, "p1", 5, events.ReasonCrossContestWin)
		insertOutcome(t, db, worldID, "p1", 5, events.ReasonCrossContestLose)
	}
	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.WinRate != 0.5 {
		t.Fatalf("公平胜率应 0.5，得到 %f", rep.Paid.WinRate)
	}
	if rep.IssueDetected {
		t.Fatalf("50%% 胜率不应判红线")
	}
}

func TestAuditArbitration_SmallSampleNoRedline(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-small"
	// 付费 p1：只有 4 场全赢（100% 但样本不足 minSampleForRedline=20）。
	for i := 0; i < 4; i++ {
		insertOutcome(t, db, worldID, "p1", 1, events.ReasonCrossContestWin)
	}
	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.WinRate != 1.0 {
		t.Fatalf("4 战全胜胜率应 1.0，得到 %f", rep.Paid.WinRate)
	}
	if rep.SampleSufficient || rep.IssueDetected {
		t.Fatalf("样本不足不应判红线: sufficient=%v issue=%v", rep.SampleSufficient, rep.IssueDetected)
	}
}

func TestAuditArbitration_TurnRangeFilter(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-range"
	// tick=5 一场胜、tick=50 一场负。
	insertOutcome(t, db, worldID, "p1", 5, events.ReasonCrossContestWin)
	insertOutcome(t, db, worldID, "p1", 50, events.ReasonCrossContestLose)

	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	// 只审计 [0,10]：应只看到 tick=5 的那场胜。
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 10)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.Total != 1 || rep.Paid.Wins != 1 {
		t.Fatalf("区间过滤应只含 tick=5 的胜场: %+v", rep.Paid)
	}
}

func TestAuditArbitration_LootCountsAsWin(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-loot"
	// ECONOMY_LOOT_ARBITRATED 也算胜方事件（仲裁分赃胜出）。
	insertOutcome(t, db, worldID, "p1", 3, events.ReasonEconomyLootArbitrated)
	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.Wins != 1 || rep.Paid.Total != 1 {
		t.Fatalf("仲裁分赃应记为胜场: %+v", rep.Paid)
	}
}

func TestAuditArbitration_BadRange(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db)
	if _, err := svc.AuditArbitration(ctx, "w", 10, 5); err == nil {
		t.Fatalf("start>end 应报错")
	}
}

// TestAuditArbitration_EmptyIntervalInconclusive 覆盖 H2 的核心修复：
// 区间内零结局事件（生产者尚未带 Scope.WorldID+Scope.Tick 落库，审计查不到任何行）时，
// 必须置 Inconclusive=true 且 Note 含「审计未接通/不可作为安全判据」语义——杜绝把假阴呈现为「已验证无 P2W」。
func TestAuditArbitration_EmptyIntervalInconclusive(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	rep, err := svc.AuditArbitration(ctx, "world-empty", 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.Total != 0 || rep.NonPaid.Total != 0 {
		t.Fatalf("空区间两组都应为空: %+v / %+v", rep.Paid, rep.NonPaid)
	}
	if rep.IssueDetected {
		t.Fatalf("空区间绝不应判 IssueDetected（那是假信心）")
	}
	if !rep.Inconclusive {
		t.Fatalf("空区间必须置 Inconclusive=true（审计未接通），实得 false")
	}
	if !strings.Contains(rep.Note, "未接通") || !strings.Contains(rep.Note, "不可作为安全判据") {
		t.Fatalf("空区间 Note 须含未接通/不可作为安全判据语义，实得: %q", rep.Note)
	}
	if !strings.Contains(rep.Note, "未见仲裁结局事件") {
		t.Fatalf("空区间 Note 须点明未见仲裁结局事件，实得: %q", rep.Note)
	}
}

// TestAuditArbitration_SingleGroupInconclusive 覆盖 H2 的「分组不可比」分支：
// 区间内只有付费组有样本、没有非付费对照（或反之）时，无法判断付费有没有不公平地赢——置 Inconclusive=true。
// 注意：此处付费组 25 样本即便胜率 100% 也绝不判 IssueDetected，因为没有对照组可比。
func TestAuditArbitration_SingleGroupInconclusive(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-single-group"
	for i := 0; i < 25; i++ {
		insertOutcome(t, db, worldID, "p1", 7, events.ReasonCrossContestWin)
	}
	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.Total != 25 || rep.NonPaid.Total != 0 {
		t.Fatalf("应仅付费组有样本: %+v / %+v", rep.Paid, rep.NonPaid)
	}
	if rep.IssueDetected {
		t.Fatalf("无对照组时不应判 IssueDetected")
	}
	if !rep.Inconclusive {
		t.Fatalf("仅单组有样本须置 Inconclusive=true，实得 false")
	}
	if !strings.Contains(rep.Note, "不可比") || !strings.Contains(rep.Note, "不可作为安全判据") {
		t.Fatalf("单组 Note 须含不可比/不可作为安全判据语义，实得: %q", rep.Note)
	}
}

// TestAuditArbitration_ComparableNotInconclusive 把关：付费+非付费双组齐全且付费组样本足量、胜率在红线内时，
// 才允许给出「本区间未见 P2W 迹象」的可作判据结论（Inconclusive=false）。
func TestAuditArbitration_ComparableNotInconclusive(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-comparable"
	// 付费 p1：20 场 10 胜 10 负（50%，红线内）。
	for i := 0; i < 10; i++ {
		insertOutcome(t, db, worldID, "p1", 4, events.ReasonCrossContestWin)
		insertOutcome(t, db, worldID, "p1", 4, events.ReasonCrossContestLose)
	}
	// 非付费 n1：对照组，提供可比性。
	for i := 0; i < 6; i++ {
		insertOutcome(t, db, worldID, "n1", 4, events.ReasonCrossContestWin)
		insertOutcome(t, db, worldID, "n1", 4, events.ReasonCrossContestLose)
	}
	svc := NewService(db).WithPaidResolver(paidResolverFor("p1"))
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Inconclusive {
		t.Fatalf("双组齐全、样本足量、红线内时应可作判据（Inconclusive=false），实得 true，Note=%q", rep.Note)
	}
	if rep.IssueDetected {
		t.Fatalf("50%% 胜率不应判红线")
	}
	if !strings.Contains(rep.Note, "未见 P2W 迹象") {
		t.Fatalf("可作判据结论 Note 应含未见 P2W 迹象，实得: %q", rep.Note)
	}
}

func TestAuditArbitration_NoPaidResolverAllNonPaid(t *testing.T) {
	ctx, db := newLiveopsDB(t)
	worldID := "world-nopaid"
	for i := 0; i < 30; i++ {
		insertOutcome(t, db, worldID, "p1", 1, events.ReasonCrossContestWin)
	}
	// 未注入 PaidResolver：全员视为非付费，付费组为空，绝不误报红线。
	svc := NewService(db)
	rep, err := svc.AuditArbitration(ctx, worldID, 0, 100)
	if err != nil {
		t.Fatalf("审计失败: %v", err)
	}
	if rep.Paid.Total != 0 {
		t.Fatalf("无 resolver 时付费组应为空，得到 %d", rep.Paid.Total)
	}
	if rep.NonPaid.Total != 30 {
		t.Fatalf("无 resolver 时全进非付费组，得到 %d", rep.NonPaid.Total)
	}
	if rep.IssueDetected {
		t.Fatalf("付费组空不应判红线")
	}
}
