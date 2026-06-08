package session

// 文件说明：FateScore 三因子（不可逆度×牵挂相关度×情绪强度）的单调/边界单元测试，
// 以及 ResolveFateDecision 真后果落地（经 status.Mutator 改 morale/loyalty，读回断言）与归因因果句入卡。

import (
	"context"
	"database/sql"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

// --- 因子一：不可逆度 ---

func TestFateIrreversibility_BoundedAndDeathHigherThanReward(t *testing.T) {
	death := fateIrreversibility(FateEvent{ReasonCode: events.ReasonCombatDown, Importance: 9})
	reward := fateIrreversibility(FateEvent{ReasonCode: events.ReasonEmotionReward, Importance: 5})
	if death <= reward {
		t.Fatalf("死亡的不可逆度应高于奖励：death=%.3f reward=%.3f", death, reward)
	}
	// 恒在 [floor,1]。
	for _, code := range []events.ReasonCode{
		events.ReasonCombatDown, events.ReasonEmotionTrauma, events.ReasonCommandForced,
		events.ReasonCombatHit, events.ReasonEmotionReward, events.ReasonCommandAccepted,
		events.ReasonInboxHighlight, // 未登记 → 取中位
	} {
		v := fateIrreversibility(FateEvent{ReasonCode: code, Importance: 5})
		if v < fateIrreversibilityFloor-1e-9 || v > 1+1e-9 {
			t.Fatalf("不可逆度应落在 [%.2f,1]，code=%s 得到 %.3f", fateIrreversibilityFloor, code, v)
		}
	}
}

func TestFateIrreversibility_MonotonicInImportance(t *testing.T) {
	low := fateIrreversibility(FateEvent{ReasonCode: events.ReasonCombatHit, Importance: 1})
	high := fateIrreversibility(FateEvent{ReasonCode: events.ReasonCombatHit, Importance: 10})
	if high < low {
		t.Fatalf("同 reason-code 下不可逆度应随 importance 单调不减：low=%.3f high=%.3f", low, high)
	}
}

// --- 因子三：情绪强度 ---

func TestFateEmotionIntensity_FlooredAndMonotonic(t *testing.T) {
	zero := fateEmotionIntensity(FateEvent{EmotionWeight: 0})
	if zero < fateEmotionFloor-1e-9 || zero > fateEmotionFloor+1e-9 {
		t.Fatalf("情绪缺省应取下限 %.2f（不清零命运），得到 %.3f", fateEmotionFloor, zero)
	}
	mild := fateEmotionIntensity(FateEvent{EmotionWeight: -0.3})
	strong := fateEmotionIntensity(FateEvent{EmotionWeight: -0.9})
	full := fateEmotionIntensity(FateEvent{EmotionWeight: -1.5}) // 超界
	if !(zero <= mild && mild <= strong && strong <= full) {
		t.Fatalf("情绪强度应随 |EmotionWeight| 单调不减：%.3f %.3f %.3f %.3f", zero, mild, strong, full)
	}
	if full < 1-1e-9 || full > 1+1e-9 {
		t.Fatalf("|EmotionWeight|≥1 应饱和到 1，得到 %.3f", full)
	}
}

func TestClampFateFactor_Boundaries(t *testing.T) {
	if got := clampFateFactor(-1, 0.7); got != 0.7 {
		t.Fatalf("v≤0 应取 floor，得到 %.3f", got)
	}
	if got := clampFateFactor(2, 0.7); got != 1 {
		t.Fatalf("v≥1 应取 1，得到 %.3f", got)
	}
	mid := clampFateFactor(0.5, 0.7) // 0.7 + 0.5*0.3 = 0.85
	if mid < 0.85-1e-9 || mid > 0.85+1e-9 {
		t.Fatalf("中间值应线性插值到 [floor,1]：期望 0.85 得到 %.3f", mid)
	}
}

// --- 组合：三因子相乘后仍能跨过既有阈值（回归保护） ---

func TestFateScore_StrongCareStillRoutesPending(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	a := unit.BootstrapRecord(2, "s1", "player", "阿采")
	b := unit.BootstrapRecord(4, "s1", "player", "老吴")
	for _, r := range []unit.Record{a, b} {
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	// 强羁绊（careRelevance≈0.8）+ 死亡（irreversibility≈1）→ 即便 EmotionWeight=0（情绪取下限 0.7），
	// fateScore ≈ 1×0.8×0.7 = 0.56 ≥ 0.55，仍应升级待决策（三因子不应误杀本该牵动她的事）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, b.ID, 8.0, 0.0, 8.0, 0.0,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}
	ev := FateEvent{ActorID: b.ID, TargetID: b.ID, ReasonCode: events.ReasonCombatDown, Importance: 9, Summary: "老吴倒下了"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &a, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RoutePending {
		t.Fatalf("强羁绊下密友之死应仍升级待决策，得到 %s（fateScore=%.3f）", routing.Route, routing.Relevance)
	}
}

func TestFateScore_LowStakesDailyDemotes(t *testing.T) {
	// 与上同样的 careRelevance，但用低不可逆度的「奖励」事件 → fateScore 被压低应降一档。
	// 直接比较两条事件的 fateScore（纯函数层）：死亡 > 奖励。
	deathScore := fateIrreversibility(FateEvent{ReasonCode: events.ReasonCombatDown, Importance: 9}) *
		fateEmotionIntensity(FateEvent{ReasonCode: events.ReasonCombatDown, Importance: 9})
	rewardScore := fateIrreversibility(FateEvent{ReasonCode: events.ReasonEmotionReward, Importance: 3}) *
		fateEmotionIntensity(FateEvent{ReasonCode: events.ReasonEmotionReward, Importance: 3})
	if rewardScore >= deathScore {
		t.Fatalf("低 stakes 日常事件的命运分应低于死亡：reward=%.3f death=%.3f", rewardScore, deathScore)
	}
}

// --- 归因因果句入卡（item ③） ---

func TestFateCard_AppendsAttributionCause(t *testing.T) {
	with := fateCard(
		FateEvent{ReasonCode: events.ReasonCombatDown, Summary: "老吴倒下了", AttributionZH: "她为了护住孩子才挡在前面"},
		relevance.RoutePending, "",
	)
	if !contains(with, "她为了护住孩子才挡在前面") {
		t.Fatalf("带归因句时命运卡应追加因果句，得到：%s", with)
	}
	without := fateCard(
		FateEvent{ReasonCode: events.ReasonCombatDown, Summary: "老吴倒下了"},
		relevance.RoutePending, "",
	)
	if contains(without, "（") {
		t.Fatalf("无归因句时不应追加括号因果句，得到：%s", without)
	}
}

// --- item ②：ResolveFateDecision 真后果经 Mutator 落地（读回断言） ---

// surfacePendingDecisionFor 给 owner 造一条真实 pending 待决策（密友 b 之死路由到 owner），返回其 decisionID。
// 用于 resolve 类测试——避免用伪造 decisionID（现已被归属校验拒绝）。
func surfacePendingDecisionFor(t *testing.T, ctx context.Context, db *sql.DB, repo *unit.Repository, service *Service, owner unit.Record, friendSeed int64) string {
	t.Helper()
	b := unit.BootstrapRecord(friendSeed, "s1", "player", "老吴")
	if err := repo.Save(ctx, b); err != nil {
		t.Fatalf("save friend: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		owner.ID, b.ID, 8.0, 0.0, 8.0, 0.0,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}
	ev := FateEvent{ActorID: b.ID, TargetID: b.ID, ReasonCode: events.ReasonCombatDown, Importance: 9, Summary: "老吴倒下了"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &owner, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RoutePending || routing.DecisionID == "" {
		t.Fatalf("应升级待决策：%+v", routing)
	}
	return routing.DecisionID
}

func TestResolveFateDecision_LetHerRaisesMoraleAndLoyalty(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(2, "s1", "player", "阿采") // 默认 morale=0.7 loyalty=0.7
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	decisionID := surfacePendingDecisionFor(t, ctx, db, repo, service, a, 3)

	before, err := repo.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("get before: %v", err)
	}
	if err := service.ResolveFateDecision(ctx, "s1", a.ID, decisionID, "let_her"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	after, err := repo.GetByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if !(after.Status.Morale > before.Status.Morale) {
		t.Fatalf("let_her 应经 Mutator 提升士气：before=%.3f after=%.3f", before.Status.Morale, after.Status.Morale)
	}
	if !(after.Status.Loyalty > before.Status.Loyalty) {
		t.Fatalf("let_her 应经 Mutator 提升忠诚：before=%.3f after=%.3f", before.Status.Loyalty, after.Status.Loyalty)
	}
}

func TestResolveFateDecision_UrgeLowersLoyalty(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(4, "s1", "player", "阿采")
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	decisionID := surfacePendingDecisionFor(t, ctx, db, repo, service, a, 5)
	before, _ := repo.GetByID(ctx, a.ID)
	if err := service.ResolveFateDecision(ctx, "s1", a.ID, decisionID, "urge"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	after, _ := repo.GetByID(ctx, a.ID)
	if !(after.Status.Loyalty < before.Status.Loyalty) {
		t.Fatalf("urge（玩家越界干预）应经 Mutator 降低忠诚：before=%.3f after=%.3f", before.Status.Loyalty, after.Status.Loyalty)
	}
}

// TestResolveFateDecision_RejectsForgedDecisionID 验证伪造（从未 Surface 过）的 decisionID 被拒绝、绝不施加后果。
// 这是评审 load-bearing 安全修复：杜绝客户端用任意 decisionID + unitID 凭空刷保护字段。
func TestResolveFateDecision_RejectsForgedDecisionID(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(2, "s1", "player", "阿采")
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	before, _ := repo.GetByID(ctx, a.ID)
	if err := service.ResolveFateDecision(ctx, "s1", a.ID, "fd_forged", "let_her"); err == nil {
		t.Fatalf("伪造 decisionID 应被拒绝（无对应 PENDING_DECISION 事件）")
	}
	after, _ := repo.GetByID(ctx, a.ID)
	if after.Status.Morale != before.Status.Morale || after.Status.Loyalty != before.Status.Loyalty {
		t.Fatalf("被拒绝的伪造决断绝不应改动保护字段：morale %.3f→%.3f loyalty %.3f→%.3f",
			before.Status.Morale, after.Status.Morale, before.Status.Loyalty, after.Status.Loyalty)
	}
}

// TestResolveFateDecision_IdempotentOnRepeat 验证同一 decisionID 重复 resolve 只施加一次后果（原子抢占去重）。
// 这是评审 load-bearing 修复：杜绝双击/重试/脚本反复 POST 把 morale/loyalty 刷到边界。
func TestResolveFateDecision_IdempotentOnRepeat(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(2, "s1", "player", "阿采")
	if err := repo.Save(ctx, a); err != nil {
		t.Fatalf("save: %v", err)
	}
	decisionID := surfacePendingDecisionFor(t, ctx, db, repo, service, a, 3)

	before, _ := repo.GetByID(ctx, a.ID)
	if err := service.ResolveFateDecision(ctx, "s1", a.ID, decisionID, "let_her"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	once, _ := repo.GetByID(ctx, a.ID)
	// 再 resolve 两次：应幂等 no-op，状态不再变化。
	for i := 0; i < 2; i++ {
		if err := service.ResolveFateDecision(ctx, "s1", a.ID, decisionID, "let_her"); err != nil {
			t.Fatalf("repeat resolve %d 应幂等 no-op 而非报错: %v", i, err)
		}
	}
	twice, _ := repo.GetByID(ctx, a.ID)
	if twice.Status.Morale != once.Status.Morale || twice.Status.Loyalty != once.Status.Loyalty {
		t.Fatalf("重复 resolve 不应叠加后果：首次后 morale=%.3f loyalty=%.3f，重复后 morale=%.3f loyalty=%.3f",
			once.Status.Morale, once.Status.Loyalty, twice.Status.Morale, twice.Status.Loyalty)
	}
	_ = before
}

func TestResolveFateDecision_StillMarksResolvedAndLeavesInbox(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	a := unit.BootstrapRecord(6, "s1", "player", "阿采")
	b := unit.BootstrapRecord(8, "s1", "player", "老吴")
	for _, r := range []unit.Record{a, b} {
		if err := repo.Save(ctx, r); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, b.ID, 8.0, 0.0, 8.0, 0.0,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}
	ev := FateEvent{ActorID: b.ID, TargetID: b.ID, ReasonCode: events.ReasonCombatDown, Importance: 9, Summary: "老吴倒下了"}
	routing, err := service.SurfaceFateEvent(ctx, "s1", &a, ev)
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if routing.Route != relevance.RoutePending || routing.DecisionID == "" {
		t.Fatalf("应升级待决策：%+v", routing)
	}
	// 处理后既有真后果、又写了 DECISION_RESOLVED 标记 → 应出箱。
	if err := service.ResolveFateDecision(ctx, "s1", a.ID, routing.DecisionID, "acknowledge"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	inbox, _ := service.OpenFateInbox(ctx, a.ID)
	if len(inbox) != 0 {
		t.Fatalf("处理后收件箱应为空（DECISION_RESOLVED 标记生效），得到 %d", len(inbox))
	}
}
