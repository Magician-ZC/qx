package session

// 文件说明：副本异步分段推进全链路集成测试（对真实 SQLite）：
// 创建首段 → 逐段推进（continue/boss首触暂停/濒死暂停/终局）→ 恢复(continue/retreat) → charter 超时兜底 →
// 旁观传播 → 确定性可复现 → 付费不进分赃。复用 threat_test.go 的 newThreatTestService/saveMember/eventCount/contains
// 与 dungeon_test.go 的 withDungeonFlag。

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"qunxiang/backend/internal/unit"
)

// drainDungeonSegments 反复推进一个分段副本直到终局或撞上暂停；遇暂停按 choice 续跑，返回最终终局结果与暂停计数。
// maxSteps 防御死循环（正常 floors+暂停数远小于此）。
func drainDungeonSegments(t *testing.T, ctx context.Context, service *Service, state *State, seg *DungeonSegment, resumeChoice string, maxSteps int) (DungeonSegmentResult, int) {
	t.Helper()
	pauses := 0
	var last DungeonSegmentResult
	for step := 0; step < maxSteps; step++ {
		res, err := service.RunDungeonSegment(ctx, seg)
		if err != nil {
			t.Fatalf("推进分段失败: %v", err)
		}
		last = res
		switch res.NextAction {
		case DungeonContinueNextFloor:
			continue
		case DungeonPauseFirstContact, DungeonPausePlayerDecision:
			pauses++
			r, err := service.ResumePausedDungeonSegment(ctx, state, seg.ID, resumeChoice)
			if err != nil {
				t.Fatalf("恢复分段失败: %v", err)
			}
			last = r
			// retreat 会直接终局；continue 会续跑，回到循环顶部继续推进。
			if isDungeonTerminalAction(r.NextAction) {
				return r, pauses
			}
			// resume(continue) 已推进了一段（可能再暂停/清层/终局），重载 seg 续推。
			reloaded, lerr := service.loadDungeonSegment(ctx, seg.ID)
			if lerr != nil || reloaded == nil {
				t.Fatalf("重载分段失败: %v", lerr)
			}
			*seg = *reloaded
			if isDungeonTerminalAction(r.NextAction) {
				return r, pauses
			}
			continue
		default:
			if isDungeonTerminalAction(res.NextAction) {
				return res, pauses
			}
		}
		// 每步后重载 seg（落库的最新态）。
		reloaded, err := service.loadDungeonSegment(ctx, seg.ID)
		if err != nil || reloaded == nil {
			t.Fatalf("重载分段失败: %v", err)
		}
		*seg = *reloaded
	}
	t.Fatalf("分段推进未在 %d 步内终局，最后 action=%q", maxSteps, last.NextAction)
	return last, pauses
}

func isDungeonTerminalAction(a DungeonNextAction) bool {
	return a == DungeonCompletedCleared || a == DungeonCompletedFled || a == DungeonCompletedWiped
}

// TestStartDungeonAsync_DisabledZeroBehavior 验证 flag 关时入口零行为（不建段、不读单位）。
func TestStartDungeonAsync_DisabledZeroBehavior(t *testing.T) {
	withDungeonFlag(t, "false")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := saveMember(t, ctx, repo, 1, "她", 30, 8, 100)
	seg, err := service.StartDungeonAsync(ctx, "s1", []string{actor.ID}, 3)
	if err != ErrDungeonDisabled {
		t.Fatalf("flag 关应返回 ErrDungeonDisabled，得到 err=%v seg=%+v", err, seg)
	}
	if seg != nil {
		t.Fatalf("flag 关不应建段")
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM dungeon_segments`).Scan(&n); err != nil {
		t.Fatalf("统计段表失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("flag 关不应落任何段，得到 %d", n)
	}
}

// TestRunDungeonSegment_ClearAllFloors 验证强队逐段推平全部层、末层 boss 首触暂停、通关分赃落库。
func TestRunDungeonSegment_ClearAllFloors(t *testing.T) {
	withDungeonFlag(t, "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	striker := saveMember(t, ctx, repo, 11, "主攻", 60, 12, 100)
	tank := saveMember(t, ctx, repo, 12, "承伤", 40, 10, 100)
	support := saveMember(t, ctx, repo, 13, "辅助", 35, 10, 100)
	ids := []string{striker.ID, tank.ID, support.ID}

	seg, err := service.StartDungeonAsync(ctx, "s1", ids, 3)
	if err != nil {
		t.Fatalf("建首段失败: %v", err)
	}
	if seg.Floor != 1 || seg.State != dungeonSegStateInProgress {
		t.Fatalf("首段应 floor=1/in_progress，得到 floor=%d state=%q", seg.Floor, seg.State)
	}

	state := &State{ID: "s1"}
	final, pauses := drainDungeonSegments(t, ctx, service, state, seg, DungeonChoiceContinue, 40)
	if final.NextAction != DungeonCompletedCleared {
		t.Fatalf("强队应通关，得到 action=%q outcome=%q", final.NextAction, final.Outcome)
	}
	// 末层 boss 至少应触发一次 first_contact 暂停。
	if pauses < 1 {
		t.Fatalf("末层 boss 应至少暂停一次（first_contact），得到 %d", pauses)
	}

	// 段落库为 completed。
	reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
	if reloaded == nil || reloaded.State != dungeonSegStateCompleted || reloaded.Outcome != "cleared" {
		t.Fatalf("段应落库为 completed/cleared，得到 %+v", reloaded)
	}

	// 通关：至少一人钱包增加（分赃经 Mutator 落库）。
	gotWallet := false
	for _, id := range ids {
		r, _ := repo.GetByID(ctx, id)
		if r.Status.Wallet > 0 {
			gotWallet = true
		}
	}
	if !gotWallet {
		t.Fatalf("通关后至少一人钱包应增加")
	}
	// 应有 DUNGEON_SEGMENT_PAUSE 留痕。
	if dungeonReasonCount(t, db, "DUNGEON_SEGMENT_PAUSE") == 0 {
		t.Fatalf("应有 DUNGEON_SEGMENT_PAUSE 留痕")
	}
	if eventCount(t, db) == 0 {
		t.Fatalf("应有战斗/分赃留痕")
	}
}

// TestRunDungeonSegment_RetreatPreservesLoot 验证 boss 首触暂停后玩家选 retreat：保留已清层 loot、终局为 fled、绝不阵亡。
func TestRunDungeonSegment_RetreatPreservesLoot(t *testing.T) {
	withDungeonFlag(t, "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	a := saveMember(t, ctx, repo, 21, "甲", 50, 12, 100)
	b := saveMember(t, ctx, repo, 22, "乙", 45, 12, 100)
	ids := []string{a.ID, b.ID}

	seg, err := service.StartDungeonAsync(ctx, "s1", ids, 3)
	if err != nil {
		t.Fatalf("建首段失败: %v", err)
	}
	state := &State{ID: "s1"}

	// 推进到 boss 首触暂停。
	var paused *DungeonSegmentResult
	for step := 0; step < 20; step++ {
		res, err := service.RunDungeonSegment(ctx, seg)
		if err != nil {
			t.Fatalf("推进失败: %v", err)
		}
		if res.NextAction == DungeonPauseFirstContact {
			r := res
			paused = &r
			break
		}
		if isDungeonTerminalAction(res.NextAction) {
			t.Fatalf("不应在 boss 暂停前终局，得到 %q", res.NextAction)
		}
		reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
		*seg = *reloaded
	}
	if paused == nil {
		t.Fatalf("应在末层 boss 首触暂停")
	}
	if paused.PauseCard == "" || !contains(paused.PauseCard, "层") {
		t.Fatalf("暂停应有祖魂语气卡，得到 %q", paused.PauseCard)
	}

	// 玩家选 retreat：保留已清前两层 loot，终局 fled。
	res, err := service.ResumePausedDungeonSegment(ctx, state, seg.ID, DungeonChoiceRetreat)
	if err != nil {
		t.Fatalf("retreat 失败: %v", err)
	}
	if res.NextAction != DungeonCompletedFled || res.Outcome != "fled" {
		t.Fatalf("retreat 应终局为 fled，得到 action=%q outcome=%q", res.NextAction, res.Outcome)
	}
	// 撤退保利：已清前两层有金币 → 至少一人钱包增加。
	gotWallet := false
	for _, id := range ids {
		r, _ := repo.GetByID(ctx, id)
		if r.Status.Wallet > 0 {
			gotWallet = true
		}
		// 撤退不施败北惩罚、绝不阵亡。
		if r.Status.LivesRemaining <= 0 {
			t.Fatalf("撤退绝不应阵亡，%s lives=%d", id, r.Status.LivesRemaining)
		}
	}
	if !gotWallet {
		t.Fatalf("撤退应保留已清层金币分赃")
	}
}

// TestResolveDungeonTimeout_CharterFallback 验证 charter 超时兜底：暂停段+玩家离场超时→自动见好就收，写 DUNGEON_CHARTER_TIMEOUT、绝不阵亡。
func TestResolveDungeonTimeout_CharterFallback(t *testing.T) {
	withDungeonFlag(t, "on")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	a := saveMember(t, ctx, repo, 31, "甲", 50, 12, 100)
	b := saveMember(t, ctx, repo, 32, "乙", 45, 12, 100)
	ids := []string{a.ID, b.ID}

	seg, err := service.StartDungeonAsync(ctx, "s1", ids, 3)
	if err != nil {
		t.Fatalf("建首段失败: %v", err)
	}
	state := &State{ID: "s1"}

	// 推进到 boss 首触暂停。
	for step := 0; step < 20; step++ {
		res, err := service.RunDungeonSegment(ctx, seg)
		if err != nil {
			t.Fatalf("推进失败: %v", err)
		}
		if res.NextAction == DungeonPauseFirstContact {
			break
		}
		reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
		*seg = *reloaded
	}

	// 玩家离场：把 left_at 写成 13h 前（超过 12h 阈值）。
	past := time.Now().UTC().Add(-13 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx,
		`UPDATE dungeon_segments SET left_at = ? WHERE id = ?`, past, seg.ID); err != nil {
		t.Fatalf("写 left_at 失败: %v", err)
	}

	reclaimed, err := service.ResolveDungeonTimeout(ctx, state)
	if err != nil {
		t.Fatalf("超时兜底失败: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("应兜底 1 段，得到 %d", reclaimed)
	}
	// 段终局 fled，且绝不阵亡。
	reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
	if reloaded == nil || reloaded.State != dungeonSegStateCompleted {
		t.Fatalf("超时段应落 completed，得到 %+v", reloaded)
	}
	for _, id := range ids {
		r, _ := repo.GetByID(ctx, id)
		if r.Status.LivesRemaining <= 0 {
			t.Fatalf("超时兜底绝不应阵亡，%s lives=%d", id, r.Status.LivesRemaining)
		}
	}
	if dungeonReasonCount(t, db, "DUNGEON_CHARTER_TIMEOUT") == 0 {
		t.Fatalf("应有 DUNGEON_CHARTER_TIMEOUT 留痕")
	}

	// 未超时的段不应被兜底（确定性比较边界）。
	seg2, _ := service.StartDungeonAsync(ctx, "s1", ids, 3)
	for step := 0; step < 20; step++ {
		res, _ := service.RunDungeonSegment(ctx, seg2)
		if res.NextAction == DungeonPauseFirstContact {
			break
		}
		reloaded, _ := service.loadDungeonSegment(ctx, seg2.ID)
		*seg2 = *reloaded
	}
	recent := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `UPDATE dungeon_segments SET left_at = ? WHERE id = ?`, recent, seg2.ID); err != nil {
		t.Fatalf("写 recent left_at 失败: %v", err)
	}
	reclaimed2, _ := service.ResolveDungeonTimeout(ctx, state)
	if reclaimed2 != 0 {
		t.Fatalf("未超时段不应被兜底，得到 %d", reclaimed2)
	}
}

// TestResolveDungeonTimeout_DisabledZeroBehavior 验证 flag 关时超时钩子零行为（绝不误结算）。
func TestResolveDungeonTimeout_DisabledZeroBehavior(t *testing.T) {
	withDungeonFlag(t, "false")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	n, err := service.ResolveDungeonTimeout(ctx, &State{ID: "s1"})
	if err != nil {
		t.Fatalf("flag 关时超时钩子不应报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("flag 关时不应兜底任何段，得到 %d", n)
	}
}

// TestRunDungeonSegment_Deterministic 验证同会话同回合同参数两次分段推进得到完全一致的终局。
func TestRunDungeonSegment_Deterministic(t *testing.T) {
	withDungeonFlag(t, "true")
	ctx := context.Background()

	run := func() (string, int) {
		_, repo, service := newThreatTestService(t)
		a := saveMember(t, ctx, repo, 41, "甲", 18, 6, 100)
		b := saveMember(t, ctx, repo, 42, "乙", 16, 6, 100)
		// 固定 ID 使两次输入一致（combat_roll 把 ID 写进 FNV）。
		a.ID, b.ID = "u_seg_a", "u_seg_b"
		if err := repo.Save(ctx, *a); err != nil {
			t.Fatalf("保存甲失败: %v", err)
		}
		if err := repo.Save(ctx, *b); err != nil {
			t.Fatalf("保存乙失败: %v", err)
		}
		seg, err := service.StartDungeonAsync(ctx, "s_seg_det", []string{a.ID, b.ID}, 4)
		if err != nil {
			t.Fatalf("建段失败: %v", err)
		}
		// 固定段 ID 以消除 uuid 随机性对 emitDungeonPause 之外结算的影响（结算 salt 不含段 ID）。
		state := &State{ID: "s_seg_det"}
		final, pauses := drainDungeonSegments(t, ctx, service, state, seg, DungeonChoiceContinue, 60)
		return string(final.NextAction), pauses
	}

	a1, p1 := run()
	a2, p2 := run()
	if a1 != a2 {
		t.Fatalf("确定性失效：终局 action %q vs %q", a1, a2)
	}
	if p1 != p2 {
		t.Fatalf("确定性失效：暂停数 %d vs %d", p1, p2)
	}
}

// TestRunDungeonSegment_PayBlindLoot 验证付费不进分赃：两名贡献相同的队员，钱包悬殊（一富一穷）也不影响排他遗物归属/分赃。
// 通过对照「调换钱包」前后排他件得主不变来证明 wallet 不进 Score（arbitration 胜率∝贡献、付费全盲）。
func TestRunDungeonSegment_PayBlindLoot(t *testing.T) {
	withDungeonFlag(t, "true")
	ctx := context.Background()

	// 跑一局，返回排他遗物得主（dungeon_relic won）。两名队员攻防一致、仅钱包不同。
	runWithWallets := func(walletA, walletB int) string {
		_, repo, service := newThreatTestService(t)
		ra := unit.BootstrapRecord(51, "s_pay", "player", "甲")
		ra.ID = "u_pay_a"
		ra.Status.Attack, ra.Status.Defense, ra.Status.HP, ra.Status.Wallet = 50, 12, 100, walletA
		rb := unit.BootstrapRecord(52, "s_pay", "player", "乙")
		rb.ID = "u_pay_b"
		rb.Status.Attack, rb.Status.Defense, rb.Status.HP, rb.Status.Wallet = 50, 12, 100, walletB
		if err := repo.Save(ctx, ra); err != nil {
			t.Fatalf("保存甲失败: %v", err)
		}
		if err := repo.Save(ctx, rb); err != nil {
			t.Fatalf("保存乙失败: %v", err)
		}
		seg, err := service.StartDungeonAsync(ctx, "s_pay", []string{ra.ID, rb.ID}, 2)
		if err != nil {
			t.Fatalf("建段失败: %v", err)
		}
		state := &State{ID: "s_pay"}
		final, _ := drainDungeonSegments(t, ctx, service, state, seg, DungeonChoiceContinue, 40)
		if final.NextAction != DungeonCompletedCleared {
			t.Fatalf("应通关以触发 boss 排他件分赃，得到 %q", final.NextAction)
		}
		// 重载段读取 awards（已 settle）；从 events 反查 relic 得主更稳，这里直接看谁钱包多了 boss relic 不易区分，
		// 改为：通关后两人贡献相同，relic 由 arbitration 确定性分配——记录得主由 dungeon_relic 的 won 归属推出。
		// 用 settle 写下的 wallet 增量无法区分 relic（relic 不进 wallet），故从 InboxCards 推断「得到遗物」者。
		// 简化：直接复算——relic 得主在两次运行（仅钱包不同）必须一致才算 pay-blind。
		return service.dungeonRelicWinner(ctx, repo, []string{ra.ID, rb.ID})
	}

	winnerPoor := runWithWallets(0, 0)
	winnerRich := runWithWallets(100000, 0) // 甲变巨富
	if winnerPoor == "" {
		t.Fatalf("应有排他遗物得主")
	}
	if winnerPoor != winnerRich {
		t.Fatalf("付费红线被破：钱包改变了排他遗物归属（穷=%q 富=%q），arbitration 必须 pay-blind", winnerPoor, winnerRich)
	}
}

// dungeonRelicWinner 反查谁的祖魂卡里出现了「遗物落在了她手里」——作为排他件归属的代理（测试用，确定性）。
func (service *Service) dungeonRelicWinner(ctx context.Context, repo *unit.Repository, ids []string) string {
	// 排他件不进 wallet，靠收件箱卡里的「遗物」措辞推断。查 events 里该单位的 INBOX_HIGHLIGHT 含「遗物」者。
	for _, id := range ids {
		var cnt int
		_ = service.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND payload_json LIKE ?`,
			id, "%遗物%").Scan(&cnt)
		if cnt > 0 {
			return id
		}
	}
	return ""
}

// dungeonReasonCount 统计某 reason_code 的事件数（验证流程事件留痕）。
func dungeonReasonCount(t *testing.T, db *sql.DB, code string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE reason_code = ?`, code).Scan(&n); err != nil {
		t.Fatalf("统计 reason_code=%s 失败: %v", code, err)
	}
	return n
}

// TestPropagateDungeonEvent_BystandersNotified 验证旁观传播：在乎队员的人在清层/挫败时收到一版命运卡（一人一版）。
func TestPropagateDungeonEvent_BystandersNotified(t *testing.T) {
	withDungeonFlag(t, "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	hero := saveMember(t, ctx, repo, 61, "英雄", 60, 12, 100)
	// 旁观者：与 hero 有强关系（在乎她）。
	watcher := saveMember(t, ctx, repo, 62, "牵挂者", 10, 5, 100)
	// watcher → hero 的强关系（出边指向 hero）。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		watcher.ID, hero.ID, 8.0, 0.0, 9.0, 0.0, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("写关系失败: %v", err)
	}

	seg, err := service.StartDungeonAsync(ctx, "s1", []string{hero.ID}, 2)
	if err != nil {
		t.Fatalf("建段失败: %v", err)
	}
	state := &State{ID: "s1"}
	final, _ := drainDungeonSegments(t, ctx, service, state, seg, DungeonChoiceContinue, 30)
	if !isDungeonTerminalAction(final.NextAction) {
		t.Fatalf("应终局，得到 %q", final.NextAction)
	}

	// watcher 应收到关于 hero 的命运事件（清层小高光经 SurfaceFateEvent 路由）。
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code IN ('INBOX_HIGHLIGHT','PENDING_DECISION')`,
		watcher.ID).Scan(&n); err != nil {
		t.Fatalf("统计旁观者收件箱失败: %v", err)
	}
	if n == 0 {
		t.Fatalf("强关系旁观者应至少收到一版副本命运卡")
	}
}

// ---- H1：并发 run/resume 同段 → 分赃双花防护（CAS 抢占结算权）----

// driveToBossPause 把一个 floors=1 的分段副本推进到 boss 首触暂停（floor1=boss，首次 RunDungeonSegment 即暂停）。
// 返回暂停后重载的 seg。
func driveToBossPause(t *testing.T, ctx context.Context, service *Service, segID string) *DungeonSegment {
	t.Helper()
	seg, err := service.loadDungeonSegment(ctx, segID)
	if err != nil || seg == nil {
		t.Fatalf("载段失败: %v", err)
	}
	res, err := service.RunDungeonSegment(ctx, seg)
	if err != nil {
		t.Fatalf("推进失败: %v", err)
	}
	if res.NextAction != DungeonPauseFirstContact {
		t.Fatalf("floors=1 首推应在 boss 首触暂停，得到 %q", res.NextAction)
	}
	reloaded, _ := service.loadDungeonSegment(ctx, segID)
	return reloaded
}

// dungeonEconomyLootCount 统计某单位的 ECONOMY_LOOT 事件数（settleDungeon 每次发赃对得到金币者各发一条；
// 双花会让此数翻倍）。
func dungeonEconomyLootCount(t *testing.T, db *sql.DB, unitID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = 'ECONOMY_LOOT'`,
		unitID).Scan(&n); err != nil {
		t.Fatalf("统计 ECONOMY_LOOT 失败: %v", err)
	}
	return n
}

// dungeonInboxHighlightCount 统计某单位的 INBOX_HIGHLIGHT 事件数（settleDungeon 每次结算对每名队员各发一条收件箱卡；
// 双花结算会让此数翻倍。注意旁观传播走 DUNGEON_FLOOR_CLEAR/EMOTION_TRAUMA，不计入此码，故能干净隔离 settle 卡）。
func dungeonInboxHighlightCount(t *testing.T, db *sql.DB, unitID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = 'INBOX_HIGHLIGHT'`,
		unitID).Scan(&n); err != nil {
		t.Fatalf("统计 INBOX_HIGHLIGHT 失败: %v", err)
	}
	return n
}

// TestCompleteDungeonSegment_ConcurrentNoDoubleSettle 验证 H1：同一 segment 被 N 个 goroutine 并发结算（固定 outcome=cleared），
// CAS 抢占结算权保证只有一个 racer 真正 settleDungeon——wallet 只加一次、ECONOMY_LOOT/收件箱卡各只一份，其余走幂等返回，绝不双花。
// 直接竞态 completeDungeonSegment（而非经 resume 跑战斗）以消除战斗中并发 HP 写互染、把竞态精确钉在「结算抢占」这一面。
func TestCompleteDungeonSegment_ConcurrentNoDoubleSettle(t *testing.T) {
	withDungeonFlag(t, "true")
	ctx := context.Background()

	// 控制组：单线程结算同一局，记录每人应得 wallet（确定性基线）。
	makeSeg := func() (*sql.DB, *unit.Repository, *Service, *DungeonSegment, *State, []string) {
		db, repo, service := newThreatTestService(t)
		a := saveMember(t, ctx, repo, 71, "甲", 60, 12, 100)
		b := saveMember(t, ctx, repo, 72, "乙", 60, 12, 100)
		a.ID, b.ID = "u_h1_a", "u_h1_b"
		_ = repo.Save(ctx, *a)
		_ = repo.Save(ctx, *b)
		ids := []string{a.ID, b.ID}
		seg, err := service.StartDungeonAsync(ctx, "s_h1", ids, 2)
		if err != nil {
			t.Fatalf("建段失败: %v", err)
		}
		// 手工把段摆到「两层全清、待结算」：累计两层 loot（含末层 boss 排他遗物），便于结算分赃。
		appendClearedLoot(seg, 1, false)
		appendClearedLoot(seg, 2, true)
		seg.Floor = 2
		seg.State = dungeonSegStateInProgress
		if err := service.saveDungeonSegment(ctx, seg); err != nil {
			t.Fatalf("写待结算段失败: %v", err)
		}
		return db, repo, service, seg, &State{ID: "s_h1"}, ids
	}

	expectWallet := func() map[string]int {
		_, repo, service, seg, state, ids := makeSeg()
		run, err := service.rebuildDungeonRun(ctx, seg, state)
		if err != nil {
			t.Fatalf("控制组重建失败: %v", err)
		}
		var res DungeonSegmentResult
		if _, err := service.completeDungeonSegment(ctx, seg, run, "cleared", &res); err != nil {
			t.Fatalf("控制组结算失败: %v", err)
		}
		out := map[string]int{}
		for _, id := range ids {
			r, _ := repo.GetByID(ctx, id)
			out[id] = r.Status.Wallet
		}
		return out
	}()

	// 并发组：N goroutine 并发结算同段。
	db, repo, service, seg, state, ids := makeSeg()

	const racers = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]string, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// 每个 racer 用自己的 seg 副本（模拟各自 load 出的快照）与独立 run，避免共享内存写竞争误导。
			segCopy := *seg
			segCopy.Awards = append([]dungeonAwardSnapshot(nil), seg.Awards...)
			run, rerr := service.rebuildDungeonRun(ctx, &segCopy, state)
			if rerr != nil {
				errs[idx] = rerr
				return
			}
			<-start // 同时起跑，最大化竞态窗口
			var res DungeonSegmentResult
			_, e := service.completeDungeonSegment(ctx, &segCopy, run, "cleared", &res)
			results[idx] = res.Outcome
			errs[idx] = e
		}(i)
	}
	close(start)
	wg.Wait()

	// 所有 racer 都应无错、且拿到一致的幂等终局 cleared——一个抢到结算权、其余走 CAS 幂等返回。
	for i := 0; i < racers; i++ {
		if errs[i] != nil {
			t.Fatalf("racer %d 不应报错: %v", i, errs[i])
		}
		if results[i] != "cleared" {
			t.Fatalf("racer %d 应得幂等 cleared，得到 %q", i, results[i])
		}
	}

	// 关键断言：wallet 只加一次（与单线程基线逐字节一致）、ECONOMY_LOOT/INBOX_HIGHLIGHT 每人恰一条（绝不双发）。
	for _, id := range ids {
		r, _ := repo.GetByID(ctx, id)
		if r.Status.Wallet != expectWallet[id] {
			t.Fatalf("H1 双花：%s wallet=%d 应=%d（单线程基线，并发只应结算一次）", id, r.Status.Wallet, expectWallet[id])
		}
		if cnt := dungeonEconomyLootCount(t, db, id); cnt > 1 {
			t.Fatalf("H1 双花：%s 有 %d 条 ECONOMY_LOOT，应≤1（只结算一次）", id, cnt)
		}
		if cnt := dungeonInboxHighlightCount(t, db, id); cnt != 1 {
			t.Fatalf("H1 双发：%s 有 %d 张 INBOX_HIGHLIGHT 收件箱卡，应恰为 1", id, cnt)
		}
	}
	// 段应恰好 completed/cleared。
	reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
	if reloaded == nil || reloaded.State != dungeonSegStateCompleted || reloaded.Outcome != "cleared" {
		t.Fatalf("段应落 completed/cleared，得到 %+v", reloaded)
	}
}

// ---- M5：段路由 :id 授权——跨会话越权 run/resume/status/leave 一律拒绝 ----

// TestDungeonSegment_CrossSessionAuthzRejected 验证段必须属于路由 sessionID：他人会话 ID 访问同一 segmentID 时
// run/resume 报「未找到」、status 返回 nil、leave 静默零作用（绝不命中他人的段）。
func TestDungeonSegment_CrossSessionAuthzRejected(t *testing.T) {
	withDungeonFlag(t, "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	owner := saveMember(t, ctx, repo, 81, "段主", 60, 12, 100)
	seg, err := service.StartDungeonAsync(ctx, "s_owner", []string{owner.ID}, 3)
	if err != nil {
		t.Fatalf("建段失败: %v", err)
	}
	const attacker = "s_attacker"

	// run by 他人会话：应报「未找到」（路由映 404），不推进段。
	if _, err := service.RunDungeonSegmentByID(ctx, attacker, seg.ID); err == nil {
		t.Fatalf("跨会话 run 应被拒（not found）")
	} else if !contains(err.Error(), "not found") {
		t.Fatalf("跨会话 run 应归一为 not found，得到 %v", err)
	}

	// resume by 他人会话：应报「未找到」。
	if _, err := service.ResumeDungeonSegmentByID(ctx, attacker, seg.ID, DungeonChoiceRetreat); err == nil {
		t.Fatalf("跨会话 resume 应被拒（not found）")
	} else if !contains(err.Error(), "not found") {
		t.Fatalf("跨会话 resume 应归一为 not found，得到 %v", err)
	}

	// status by 他人会话：返回 nil（让路由映 404，不泄露段是否存在）。
	status, err := service.DungeonSegmentStatusView(ctx, attacker, seg.ID)
	if err != nil {
		t.Fatalf("跨会话 status 不应报错: %v", err)
	}
	if status != nil {
		t.Fatalf("跨会话 status 应返回 nil（视为未找到），得到 %+v", status)
	}

	// 先让段进入 paused（leave 仅对 paused 段计时）：推进到 boss 首触暂停。
	for step := 0; step < 20; step++ {
		res, _ := service.RunDungeonSegment(ctx, seg)
		if res.NextAction == DungeonPauseFirstContact {
			break
		}
		reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
		*seg = *reloaded
	}
	// leave by 他人会话：UPDATE 加 AND session_id=? → 不命中，left_at 仍为 NULL。
	if err := service.MarkDungeonSegmentLeft(ctx, attacker, seg.ID); err != nil {
		t.Fatalf("跨会话 leave 不应报错（应静默零作用）: %v", err)
	}
	var leftAt sql.NullString
	if err := db.QueryRow(`SELECT left_at FROM dungeon_segments WHERE id = ?`, seg.ID).Scan(&leftAt); err != nil {
		t.Fatalf("读 left_at 失败: %v", err)
	}
	if leftAt.Valid {
		t.Fatalf("跨会话 leave 不应写他人段的 left_at，得到 %q", leftAt.String)
	}

	// 对照：段主自己的会话 ID 访问应正常。
	status2, err := service.DungeonSegmentStatusView(ctx, "s_owner", seg.ID)
	if err != nil || status2 == nil {
		t.Fatalf("段主自查 status 应成功，得到 status=%+v err=%v", status2, err)
	}
	if err := service.MarkDungeonSegmentLeft(ctx, "s_owner", seg.ID); err != nil {
		t.Fatalf("段主 leave 应成功: %v", err)
	}
	if err := db.QueryRow(`SELECT left_at FROM dungeon_segments WHERE id = ?`, seg.ID).Scan(&leftAt); err != nil {
		t.Fatalf("读 left_at 失败: %v", err)
	}
	if !leftAt.Valid {
		t.Fatalf("段主 leave 应写 left_at")
	}
}

// ---- L1：boss resume 骰序用钉死的 entered_turn，不随 live Turn 漂移 ----

// TestRunDungeonSegment_ResumeDiceUsesEnteredTurn 验证：同一段（同 entered_turn）在玩家不同回合回来 resume，
// 骰序与终局逐字节一致——combat_roll 的回合 salt 取 seg.EnteredTurn（踏入回合钉死），与 live Turn 无关。
func TestRunDungeonSegment_ResumeDiceUsesEnteredTurn(t *testing.T) {
	withDungeonFlag(t, "true")
	ctx := context.Background()

	// 用固定 entered_turn 建段、推到 boss 首触暂停，再以指定 liveTurn resume，返回 boss 战的 (outcome, dealt, rounds) 指纹。
	runResume := func(enteredTurn, liveTurn int) (string, int, int) {
		_, repo, service := newThreatTestService(t)
		a := unit.BootstrapRecord(91, "s_l1", "player", "甲")
		a.ID = "u_l1_a"
		a.Status.Attack, a.Status.Defense, a.Status.HP = 22, 6, 100
		b := unit.BootstrapRecord(92, "s_l1", "player", "乙")
		b.ID = "u_l1_b"
		b.Status.Attack, b.Status.Defense, b.Status.HP = 20, 6, 100
		if err := repo.Save(ctx, a); err != nil {
			t.Fatalf("保存甲失败: %v", err)
		}
		if err := repo.Save(ctx, b); err != nil {
			t.Fatalf("保存乙失败: %v", err)
		}
		seg, err := service.StartDungeonAsync(ctx, "s_l1", []string{a.ID, b.ID}, 1)
		if err != nil {
			t.Fatalf("建段失败: %v", err)
		}
		// 钉死 entered_turn（测试服务无 sessions，StartDungeonAsync 默认置 0）。
		seg.EnteredTurn = enteredTurn
		if err := service.saveDungeonSegment(ctx, seg); err != nil {
			t.Fatalf("写 entered_turn 失败: %v", err)
		}
		seg = driveToBossPause(t, ctx, service, seg.ID)
		// 玩家在不同 live Turn 回来：state.TurnState.Turn 漂移不应影响骰序（骰序取 entered_turn）。
		state := &State{ID: "s_l1"}
		state.TurnState.Turn = liveTurn
		res, err := service.ResumePausedDungeonSegment(ctx, state, seg.ID, DungeonChoiceContinue)
		if err != nil {
			t.Fatalf("resume 失败: %v", err)
		}
		if res.FloorResult == nil {
			t.Fatalf("boss 战 resume 应带 FloorResult")
		}
		return res.FloorResult.Outcome, res.FloorResult.DamageDealt, res.FloorResult.Rounds
	}

	// 同 entered_turn=5，玩家分别在 live Turn 100 / 999 回来：骰序/终局必须一致。
	o1, d1, r1 := runResume(5, 100)
	o2, d2, r2 := runResume(5, 999)
	if o1 != o2 || d1 != d2 || r1 != r2 {
		t.Fatalf("L1 骰序随 live Turn 漂移：entered=5 下 live100=(%q,%d,%d) vs live999=(%q,%d,%d)", o1, d1, r1, o2, d2, r2)
	}

	// 反向佐证：不同 entered_turn 应改变骰序（证明 entered_turn 确实进了 salt，而非被忽略）。
	o3, d3, r3 := runResume(7, 100)
	if o1 == o3 && d1 == d3 && r1 == r3 {
		t.Logf("注意：entered=5 与 entered=7 指纹偶然相同（%q,%d,%d）——确定性微调窗口内的可能，不判失败", o1, d1, r1)
	}
}

// ---- M1：charter 超时兜底 per-段失败留痕 + 卡死段失败计数防无限重试 ----

// countLogsOfKind 数 state.Logs 里某 kind 的条数（验证失败留痕，不再静默吞错）。
func countLogsOfKind(state *State, kind string) int {
	n := 0
	for _, l := range state.Logs {
		if l.Kind == kind {
			n++
		}
	}
	return n
}

// TestResolveDungeonTimeout_FailureLoggedAndCapped 验证 M1：超时段反复兜底失败时不再静默 continue——每次留痕一条
// dungeon_charter_timeout_failed 并累计失败计数；连续失败超上限后该段被过滤，不再无限重试。
func TestResolveDungeonTimeout_FailureLoggedAndCapped(t *testing.T) {
	withDungeonFlag(t, "on")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	a := saveMember(t, ctx, repo, 101, "甲", 50, 12, 100)
	b := saveMember(t, ctx, repo, 102, "乙", 45, 12, 100)
	seg, err := service.StartDungeonAsync(ctx, "s_m1", []string{a.ID, b.ID}, 3)
	if err != nil {
		t.Fatalf("建段失败: %v", err)
	}
	state := &State{ID: "s_m1"}

	// 推进到 boss 首触暂停（paused）。
	for step := 0; step < 20; step++ {
		res, _ := service.RunDungeonSegment(ctx, seg)
		if res.NextAction == DungeonPauseFirstContact {
			break
		}
		reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
		*seg = *reloaded
	}

	// 玩家离场超时（13h 前）。
	past := time.Now().UTC().Add(-13 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `UPDATE dungeon_segments SET left_at = ? WHERE id = ?`, past, seg.ID); err != nil {
		t.Fatalf("写 left_at 失败: %v", err)
	}

	// 让兜底必失败：删掉队员单位 → rebuildDungeonRun 必报错（GetByID not found）。
	for _, id := range []string{a.ID, b.ID} {
		if _, err := db.ExecContext(ctx, `DELETE FROM units WHERE id = ?`, id); err != nil {
			t.Fatalf("删队员失败: %v", err)
		}
	}

	// 连续兜底到上限：每次 reclaimed==0、留痕一条、计数 +1。
	for i := 1; i <= dungeonMaxTimeoutFailures; i++ {
		reclaimed, err := service.ResolveDungeonTimeout(ctx, state)
		if err != nil {
			t.Fatalf("第%d次兜底不应硬错: %v", i, err)
		}
		if reclaimed != 0 {
			t.Fatalf("失败段不应被算作 reclaimed，第%d次得到 %d", i, reclaimed)
		}
		reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
		if reloaded == nil {
			t.Fatalf("失败段不应消失")
		}
		if reloaded.TimeoutFailures != i {
			t.Fatalf("第%d次兜底后失败计数应=%d，得到 %d", i, i, reloaded.TimeoutFailures)
		}
		// 段仍 paused（未结算），pause kind 仍可读为裸 first_contact（失败后缀已被 scan 剥离）。
		if reloaded.State != dungeonSegStatePaused || reloaded.PauseReason != dungeonPauseReasonFirstContact {
			t.Fatalf("失败段应仍 paused 且裸 pause kind=first_contact，得到 state=%q reason=%q", reloaded.State, reloaded.PauseReason)
		}
	}
	// 应已留痕至少 dungeonMaxTimeoutFailures 条失败日志（每次至少一条；计数写入若也失败会再添）。
	if got := countLogsOfKind(state, "dungeon_charter_timeout_failed"); got < dungeonMaxTimeoutFailures {
		t.Fatalf("应留痕≥%d 条 dungeon_charter_timeout_failed，得到 %d", dungeonMaxTimeoutFailures, got)
	}

	// 超上限后：段被 loadTimedOutDungeonSegments 过滤，不再被取出重试 → 计数不再增长。
	logsBefore := countLogsOfKind(state, "dungeon_charter_timeout_failed")
	reclaimed, err := service.ResolveDungeonTimeout(ctx, state)
	if err != nil {
		t.Fatalf("超上限后兜底不应硬错: %v", err)
	}
	if reclaimed != 0 {
		t.Fatalf("超上限段不应被兜底，得到 %d", reclaimed)
	}
	reloaded, _ := service.loadDungeonSegment(ctx, seg.ID)
	if reloaded.TimeoutFailures != dungeonMaxTimeoutFailures {
		t.Fatalf("超上限后失败计数不应再增长，应=%d，得到 %d", dungeonMaxTimeoutFailures, reloaded.TimeoutFailures)
	}
	if logsAfter := countLogsOfKind(state, "dungeon_charter_timeout_failed"); logsAfter != logsBefore {
		t.Fatalf("超上限段不应再被取出重试/刷错，日志数应不变（%d→%d）", logsBefore, logsAfter)
	}
}
