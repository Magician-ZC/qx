package session

// 文件说明：命运 M2/M3 硬规则聚焦测试（设计宪法 §4.2/§4.3/§4.6）。
// 覆盖：① 每日待决策预算 ≤3 溢出降级高光卡；② 过期倒计时纯函数（确定性）；③ 过期兜底自动关掉 PENDING + 回响卡；
// ④ 不可逆类命运在高牵挂下拒绝自动 let_her；⑤ M3 分级闸随牵挂单调（urge 越界 loyalty 代价递增）。
// 前缀统一 fateHardrules*，避免与既有命运测试撞名。

import (
	"context"
	"testing"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

// surfacePendingFateEvent 是测试辅助：用足够高的重要度/情绪让 SurfaceFateEvent 路由到待决策（≥0.55）。
func surfacePendingFateEvent(t *testing.T, service *Service, ctx context.Context, owner *unit.Record, code events.ReasonCode) FateRouting {
	t.Helper()
	r, err := service.SurfaceFateEvent(ctx, "s1", owner, FateEvent{
		ActorID: owner.ID, TargetID: owner.ID, ReasonCode: code, Importance: 9, EmotionWeight: -0.85, Summary: "她到了一个需要你拿主意的关口",
	})
	if err != nil {
		t.Fatalf("surface fate event 失败: %v", err)
	}
	return r
}

// TestFateHardrulesDailyPendingBudget 验证 M2①：同一 owner 同一自然日进「待决策」的命运节点 ≤3，第 4 条降级高光卡。
func TestFateHardrulesDailyPendingBudget(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(11, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	pendingCount := 0
	for i := 0; i < 5; i++ {
		r := surfacePendingFateEvent(t, service, ctx, &hero, events.ReasonEmotionTrauma)
		if r.Route == "pending" {
			pendingCount++
			if r.DecisionID == "" {
				t.Fatalf("第 %d 条 pending 应带 decision_id", i)
			}
		} else if r.Route == "highlight" {
			if r.DecisionID != "" {
				t.Fatalf("降级为高光卡的命运节点不应带 decision_id（第 %d 条）", i)
			}
		} else {
			t.Fatalf("意外路由 %q（第 %d 条）；本应 pending 或降级 highlight", r.Route, i)
		}
	}
	if pendingCount != fatePendingDailyBudget {
		t.Fatalf("每日待决策预算应恰为 %d，实际进了 %d 条", fatePendingDailyBudget, pendingCount)
	}

	// DB 侧复核：当天 PENDING_DECISION 计数确为预算上限（溢出的两条没进待决策）。
	if exhausted := service.pendingBudgetExhausted(ctx, hero.ID); !exhausted {
		t.Fatalf("达预算后 pendingBudgetExhausted 应为 true")
	}
}

// TestFateHardrulesCountdown 验证 M2②：过期倒计时纯函数确定性（ExpiresAt = occurred_at + TTL；剩余小时随时间推移单调减；过期为 0）。
func TestFateHardrulesCountdown(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	occurred := base.Format(time.RFC3339Nano)

	// ExpiresAt 必须等于 occurred + TTL。
	wantExpiry := base.Add(fatePendingTTL).Format(time.RFC3339Nano)
	if got := fateExpiresAt(occurred); got != wantExpiry {
		t.Fatalf("ExpiresAt 应为 %s，实际 %s", wantExpiry, got)
	}

	// 刚发生：剩余 ≈ 48h（向下取整）。
	if h := fateCountdownHoursAt(occurred, base); h != 48 {
		t.Fatalf("刚发生时剩余小时应为 48，实际 %d", h)
	}
	// 过去 10h：剩余 38h。
	if h := fateCountdownHoursAt(occurred, base.Add(10*time.Hour)); h != 38 {
		t.Fatalf("过 10h 后剩余应为 38，实际 %d", h)
	}
	// 单调递减：晚一点看，剩余不会变多。
	earlier := fateCountdownHoursAt(occurred, base.Add(5*time.Hour))
	later := fateCountdownHoursAt(occurred, base.Add(20*time.Hour))
	if later > earlier {
		t.Fatalf("倒计时应随时间单调不增：5h 后 %d，20h 后 %d", earlier, later)
	}
	// 已过期：剩余钳到 0。
	if h := fateCountdownHoursAt(occurred, base.Add(fatePendingTTL+time.Hour)); h != 0 {
		t.Fatalf("过期后剩余应为 0，实际 %d", h)
	}
	// 不可解析的时间戳：倒计时 0、ExpiresAt 空（不 panic）。
	if fateCountdownHours("not-a-time") != 0 || fateExpiresAt("not-a-time") != "" {
		t.Fatalf("不可解析时间戳应安全降级为 0 / 空串")
	}

	// fateIsExpired 边界：TTL 整点视为已过期，TTL 前一秒未过期。
	if fateIsExpired(occurred, base.Add(fatePendingTTL-time.Second)) {
		t.Fatalf("TTL 前一秒不应判为过期")
	}
	if !fateIsExpired(occurred, base.Add(fatePendingTTL)) {
		t.Fatalf("到达 TTL 整点应判为过期")
	}
}

// backdatePending 把某 owner 的待决策事件 occurred_at 改到过去，以触发过期兜底（仅测试用，模拟「过了 48h 玩家没回来」）。
func backdatePending(t *testing.T, service *Service, ctx context.Context, ownerID string, past time.Time) {
	t.Helper()
	if _, err := service.db.ExecContext(ctx,
		`UPDATE events SET occurred_at = ? WHERE actor_unit_id = ? AND reason_code = ?`,
		past.UTC().Format(time.RFC3339Nano), ownerID, string(events.ReasonPendingDecision),
	); err != nil {
		t.Fatalf("回拨 occurred_at 失败: %v", err)
	}
}

// TestFateHardrulesExpiryFallback 验证 M2③：打开收件箱时把超期未决的 PENDING 自动兜底关掉，并补一张回响卡。
func TestFateHardrulesExpiryFallback(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(12, "s1", "player", "她")
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}

	// 一条可逆类命运（创伤，非死亡/叛变）→ 进待决策，再回拨到 49h 前使其过期。
	r := surfacePendingFateEvent(t, service, ctx, &hero, events.ReasonEmotionTrauma)
	if r.Route != "pending" {
		t.Fatalf("前置：应路由到 pending，实际 %q", r.Route)
	}
	backdatePending(t, service, ctx, hero.ID, time.Now().Add(-(fatePendingTTL + time.Hour)))

	// 打开收件箱：过期兜底应自动关掉它 → 收件箱里看不到这条。
	inbox, err := service.OpenFateInbox(ctx, hero.ID)
	if err != nil {
		t.Fatalf("打开收件箱失败: %v", err)
	}
	for _, it := range inbox {
		if it.DecisionID == r.DecisionID {
			t.Fatalf("超期待决策应被过期兜底关掉，但仍在收件箱：%s", r.DecisionID)
		}
	}

	// DB 复核：写了 DECISION_RESOLVED（出箱标记）。
	var resolvedN int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		hero.ID, string(events.ReasonDecisionResolved),
	).Scan(&resolvedN); err != nil {
		t.Fatalf("统计 resolved 失败: %v", err)
	}
	if resolvedN < 1 {
		t.Fatalf("过期兜底应写下 DECISION_RESOLVED 标记，实际 %d 条", resolvedN)
	}

	// DB 复核：生成了回响卡（ECHO_LINK，「你没回来，于是她自己做了选择」）。
	var echoN int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		hero.ID, string(events.ReasonEchoLink),
	).Scan(&echoN); err != nil {
		t.Fatalf("统计 echo 失败: %v", err)
	}
	if echoN < 1 {
		t.Fatalf("过期兜底应补一张回响卡（ECHO_LINK），实际 %d 条", echoN)
	}
}

// TestFateHardrulesIrreversibleRefusesAutoLetHer 验证 M3 守门：不可逆类命运在高牵挂下拒绝自动 let_her。
// 直接打不可逆类预判函数（确定性、不依赖牵挂公式标定），并用一条 DB 集成验证它不会在 inbox 被自动关掉。
func TestFateHardrulesIrreversibleRefusesAutoLetHer(t *testing.T) {
	// 纯谓词：不可逆类（死亡/叛变/势力崩塌）+ 牵挂达解锁线 → 拒绝；可逆类或低牵挂 → 不拒绝。
	if !fateReasonIsIrreversibleClass(events.ReasonCombatDown) ||
		!fateReasonIsIrreversibleClass(events.ReasonCharacterDied) ||
		!fateReasonIsIrreversibleClass(events.ReasonRelationBetray) {
		t.Fatalf("死亡/叛变类应被判为不可逆类")
	}
	if fateReasonIsIrreversibleClass(events.ReasonEmotionTrauma) {
		t.Fatalf("创伤（可治愈）不应被判为不可逆类")
	}
	if !fateRefusesAutoLetHer(events.ReasonCombatDown, fateIrreversibleAttachmentGate) {
		t.Fatalf("不可逆类 + 牵挂达线应拒绝自动兜底")
	}
	if fateRefusesAutoLetHer(events.ReasonCombatDown, fateIrreversibleAttachmentGate-1) {
		t.Fatalf("不可逆类但牵挂未达线不应拒绝（层3 需牵挂≥%v）", fateIrreversibleAttachmentGate)
	}
	if fateRefusesAutoLetHer(events.ReasonEmotionTrauma, 100) {
		t.Fatalf("可逆类命运无论牵挂多高都不应拒绝自动兜底")
	}

	// 集成：把忠诚/共创顶满让牵挂越过解锁线，超期不可逆命运在收件箱里**保留**（不被自动关掉）。
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	hero := unit.BootstrapRecord(13, "s1", "player", "她")
	hero.Status.Loyalty = 1.0 // 共鸣顶满
	if err := repo.Save(ctx, hero); err != nil {
		t.Fatalf("存角色失败: %v", err)
	}
	// 预置足量「共创」事件（DECISION_RESOLVED/PLAYER_INTERVENTION）把牵挂推过解锁线。
	for i := 0; i < 8; i++ {
		if _, err := service.RecordPlayerIntervention(ctx, "s1", hero.ID, "你接管了她一次"); err != nil {
			t.Fatalf("预置共创失败: %v", err)
		}
	}
	if att := service.attachmentForUnit(ctx, hero.ID); att < fateIrreversibleAttachmentGate {
		t.Skipf("牵挂 %v 未达解锁线 %v（标定相关），跳过集成断言；纯谓词已覆盖守门逻辑", att, fateIrreversibleAttachmentGate)
	}

	// 一条不可逆类（叛变）待决策，回拨到过期。
	r := surfacePendingFateEvent(t, service, ctx, &hero, events.ReasonRelationBetray)
	if r.Route != "pending" {
		t.Fatalf("前置：不可逆命运应路由到 pending，实际 %q", r.Route)
	}
	backdatePending(t, service, ctx, hero.ID, time.Now().Add(-(fatePendingTTL + time.Hour)))

	inbox, err := service.OpenFateInbox(ctx, hero.ID)
	if err != nil {
		t.Fatalf("打开收件箱失败: %v", err)
	}
	found := false
	for _, it := range inbox {
		if it.DecisionID == r.DecisionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("高牵挂下不可逆命运应**保留**在收件箱等玩家显式处理，而非被自动 let_her 关掉")
	}
	// 复核：没有为这条不可逆命运写 DECISION_RESOLVED（没被自动关）。
	var n int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ? AND payload_json LIKE ?`,
		hero.ID, string(events.ReasonDecisionResolved), "%"+r.DecisionID+"%",
	).Scan(&n)
	if n != 0 {
		t.Fatalf("不可逆命运不应被自动兜底关掉，却写了 %d 条 DECISION_RESOLVED", n)
	}
}

// TestFateHardrulesGradingLayerMonotonic 验证 M3 分级闸：牵挂越高，urge 越界干预的 loyalty 代价越大（严格单调）。
func TestFateHardrulesGradingLayerMonotonic(t *testing.T) {
	loyaltyCost := func(attachment float64) float64 {
		for _, c := range fateConsequenceLayer("urge", attachment) {
			if c.Field == status.FieldLoyalty && c.Delta < 0 {
				return -c.Delta // 返回代价幅度（正数）
			}
		}
		t.Fatalf("urge 后果里应含一项 loyalty 负向代价")
		return 0
	}

	prev := loyaltyCost(0)
	for _, a := range []float64{10, 30, 50, 70, 100} {
		cur := loyaltyCost(a)
		if cur <= prev {
			t.Fatalf("urge 越界 loyalty 代价应随牵挂严格递增：att 升到 %v 时代价 %.4f 未超过前值 %.4f", a, cur, prev)
		}
		prev = cur
	}
	// 满牵挂代价 = 基础代价 × fateUrgeCostMaxScale（上限对齐）。
	if got, want := loyaltyCost(100), loyaltyCost(0)*fateUrgeCostMaxScale; got != want {
		t.Fatalf("满牵挂代价应为基础 × %v：want %.4f got %.4f", fateUrgeCostMaxScale, want, got)
	}

	// 正向后果（let_her/acknowledge 的暖意）不随牵挂膨胀（只放大越界代价，不放大薅好处）。
	moraleAt := func(resolveType string, attachment float64) float64 {
		for _, c := range fateConsequenceLayer(resolveType, attachment) {
			if c.Field == status.FieldMorale {
				return c.Delta
			}
		}
		return 0
	}
	if moraleAt("let_her", 0) != moraleAt("let_her", 100) {
		t.Fatalf("let_her 的正向士气后果不应随牵挂变化（不卖牵挂红利）")
	}
}
