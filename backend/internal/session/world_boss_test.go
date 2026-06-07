package session

// 文件说明：世界Boss异步协作 PvE 集成测试（对真实 SQLite）：
// 共享血池原子扣血 → 总线贡献账本 → 血池清零全员分赃（epic 仲裁 + gold 按贡献瓜分）→ 单次结算闩锁防双结算。

import (
	"context"
	"sync"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

func mustCreateWorld(t *testing.T, ctx context.Context, service *Service) string {
	t.Helper()
	id, err := world.Create(ctx, service.db, world.World{Name: "试炼之地"})
	if err != nil {
		t.Fatalf("建世界失败: %v", err)
	}
	return id
}

func bossStriker(t *testing.T, ctx context.Context, repo *unit.Repository, seed int64, name string, atk int) *unit.Record {
	t.Helper()
	rec := unit.BootstrapRecord(seed, "s1", "player", name)
	rec.Status.Attack = atk
	rec.Status.Wallet = 0
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存出手者失败: %v", err)
	}
	return &rec
}

func countCrossKind(t *testing.T, service *Service, worldID string, kind worldbus.EventKind) int {
	t.Helper()
	evs, err := worldbus.ListByWorld(context.Background(), service.db, worldID, 0)
	if err != nil {
		t.Fatalf("读总线失败: %v", err)
	}
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestWorldBossDefeatAndLoot(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)

	bossID, err := service.SpawnWorldBoss(ctx, wid, "焚天古龙", 50, "r1")
	if err != nil {
		t.Fatalf("投放世界Boss失败: %v", err)
	}
	a := bossStriker(t, ctx, repo, 51, "甲", 30)
	b := bossStriker(t, ctx, repo, 52, "乙", 30)

	r1, err := service.StrikeWorldBoss(ctx, wid, bossID, a) // 50 -> 20
	if err != nil {
		t.Fatalf("甲出手失败: %v", err)
	}
	if r1.Defeated || r1.HPRemaining != 20 {
		t.Fatalf("首击后应剩 20 血、未死，得到 defeated=%v hp=%d", r1.Defeated, r1.HPRemaining)
	}
	r2, err := service.StrikeWorldBoss(ctx, wid, bossID, b) // 20 -> 0，致命一击 + 结算
	if err != nil {
		t.Fatalf("乙出手失败: %v", err)
	}
	if !r2.Defeated || !r2.SettledByMe {
		t.Fatalf("致命一击应判死并由本请求结算，得到 defeated=%v settled=%v", r2.Defeated, r2.SettledByMe)
	}
	if r2.Participants != 2 {
		t.Fatalf("应有 2 名参战者，得到 %d", r2.Participants)
	}

	// 唯一遗物恰有 1 名得主；gold 总量应等于血量 50 且分给两人。
	relicWinners, totalGold := 0, 0
	for _, aw := range r2.Awards {
		if aw.ItemID == worldBossEpicRelicID && aw.Reason == "won" {
			relicWinners++
		}
		if aw.ItemID == "gold" {
			totalGold += aw.Quantity
		}
	}
	if relicWinners != 1 {
		t.Fatalf("唯一遗物应恰 1 名得主，得到 %d", relicWinners)
	}
	if totalGold != 50 {
		t.Fatalf("gold 应恰好瓜分完 50，得到 %d", totalGold)
	}

	// 本库参战者钱包应到账。
	ra, _ := repo.GetByID(ctx, a.ID)
	rb, _ := repo.GetByID(ctx, b.ID)
	if ra.Status.Wallet+rb.Status.Wallet != 50 {
		t.Fatalf("两人钱包合计应 +50，得到 %d", ra.Status.Wallet+rb.Status.Wallet)
	}

	// 讨平广播恰有一条。
	if got := countCrossKind(t, service, wid, worldbus.KindWorldBossDown); got != 1 {
		t.Fatalf("应恰有 1 条讨平广播，得到 %d", got)
	}
}

func TestWorldBossStrikeAfterDefeatRejected(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)

	bossID, _ := service.SpawnWorldBoss(ctx, wid, "脆皮", 5, "")
	a := bossStriker(t, ctx, repo, 61, "甲", 30)
	if _, err := service.StrikeWorldBoss(ctx, wid, bossID, a); err != nil {
		t.Fatalf("首击失败: %v", err)
	}
	// 已被讨平，再出手应被拒。
	if _, err := service.StrikeWorldBoss(ctx, wid, bossID, a); err == nil {
		t.Fatalf("对已讨平的 Boss 出手应返回 ErrWorldBossInactive")
	}
	// 对不存在的 Boss 出手也应被拒。
	if _, err := service.StrikeWorldBoss(ctx, wid, "nope", a); err == nil {
		t.Fatalf("对不存在的 Boss 出手应被拒")
	}
}

// 回归（review finding consistency-critical）：扣血与记账本同事务——账本写入失败时扣血必须回滚。
// 用「Boss 所在世界被删除」模拟 step-2(AdvanceTick) 失败：出手应报错，且 Boss 血量不变、账本无该击。
func TestWorldBossStrikeRollsBackOnLedgerFailure(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	bossID, err := service.SpawnWorldBoss(ctx, wid, "焚天古龙", 100, "")
	if err != nil {
		t.Fatalf("投放失败: %v", err)
	}
	a := bossStriker(t, ctx, repo, 81, "甲", 30)

	// 抽掉世界（让 AdvanceTick 必失败），模拟账本写入故障。
	if _, err := service.db.ExecContext(ctx, `DELETE FROM worlds WHERE id = ?`, wid); err != nil {
		t.Fatalf("删世界失败: %v", err)
	}
	if _, err := service.StrikeWorldBoss(ctx, wid, bossID, a); err == nil {
		t.Fatalf("账本写入失败时出手应报错")
	}
	// 关键：扣血必须已回滚——Boss 仍是满血、active。
	var hp int
	var st string
	if err := service.db.QueryRowContext(ctx, `SELECT hp_remaining, status FROM world_bosses WHERE id = ?`, bossID).Scan(&hp, &st); err != nil {
		t.Fatalf("查 Boss 失败: %v", err)
	}
	if hp != 100 || st != "active" {
		t.Fatalf("账本失败应回滚扣血，期望 hp=100 active，得到 hp=%d status=%s", hp, st)
	}
	if got := countCrossKind(t, service, wid, worldbus.KindWorldBossStrike); got != 0 {
		t.Fatalf("回滚后账本不应有该击，得到 %d 条", got)
	}
}

// 回归（review finding antip2w-critical）：频率无关——反复刷同一头 Boss 不应刷高分赃份额。
// 弱者狂刷 vs 强者一击：贡献按单次最高伤害算，强者应在排他遗物上占优、可分件分得更多。
func TestWorldBossContributionIsFrequencyInvariant(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	bossID, _ := service.SpawnWorldBoss(ctx, wid, "古神", 100, "")

	spammer := bossStriker(t, ctx, repo, 91, "狂刷", 5)  // 弱，但刷很多次
	striker := bossStriker(t, ctx, repo, 92, "强者", 40) // 强，出手少

	// 狂刷者连刷 15 次（75 伤害），强者补刀（40 伤害，致命）。总计 >100。
	var settle WorldBossStrikeResult
	for i := 0; i < 15; i++ {
		if _, err := service.StrikeWorldBoss(ctx, wid, bossID, spammer); err != nil {
			break // 若中途被打死则停
		}
	}
	r, err := service.StrikeWorldBoss(ctx, wid, bossID, striker)
	if err != nil {
		t.Fatalf("强者补刀失败: %v", err)
	}
	if r.SettledByMe {
		settle = r
	} else {
		t.Fatalf("强者这一击应清零并结算，得到 hp=%d defeated=%v", r.HPRemaining, r.Defeated)
	}

	// 贡献按单次最高：狂刷=5、强者=40。可分金币按贡献确定性瓜分，强者必分得更多——
	// 这是频率无关的确定性证据：狂刷 15 次也没把份额刷过单次 40 的强者。
	goldBy := map[string]int{}
	relicWinners := 0
	for _, a := range settle.Awards {
		if a.ItemID == "gold" {
			goldBy[a.UnitID] += a.Quantity
		}
		if a.ItemID == worldBossEpicRelicID && a.Reason == "won" {
			relicWinners++
		}
	}
	if goldBy[striker.ID] <= goldBy[spammer.ID] {
		t.Fatalf("强者（单次40）应比狂刷者（单次5×15）分得更多金币，得到 强者=%d 狂刷=%d", goldBy[striker.ID], goldBy[spammer.ID])
	}
	// 唯一遗物恰 1 名得主（具体归属由 arbitration 按贡献概率定，不强断言谁——∝Score 本就允许弱者偶得）。
	if relicWinners != 1 {
		t.Fatalf("唯一遗物应恰 1 名得主，得到 %d", relicWinners)
	}
}

// 回归（review finding concurrency-critical）：账本读取不设上限——出手数超过旧 LIMIT(200) 仍全员计入。
func TestWorldBossLedgerNotTruncated(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	bossID, _ := service.SpawnWorldBoss(ctx, wid, "耐久古龙", 100000, "")

	// 早期出手者：先打 1 次（这条若被 LIMIT 截断就会漏掉它的贡献）。
	early := bossStriker(t, ctx, repo, 101, "先锋", 7)
	if _, err := service.StrikeWorldBoss(ctx, wid, bossID, early); err != nil {
		t.Fatalf("先锋出手失败: %v", err)
	}
	// 制造 >200 条后续出手（同一个强者刷很多次，把先锋的早期记录挤出旧 LIMIT 窗口）。
	filler := bossStriker(t, ctx, repo, 102, "填充", 3)
	for i := 0; i < 210; i++ {
		if _, err := service.StrikeWorldBoss(ctx, wid, bossID, filler); err != nil {
			t.Fatalf("填充出手失败: %v", err)
		}
	}
	// 致命一击。
	finisher := bossStriker(t, ctx, repo, 103, "终结", 100000)
	r, err := service.StrikeWorldBoss(ctx, wid, bossID, finisher)
	if err != nil || !r.SettledByMe {
		t.Fatalf("终结一击应结算，得到 err=%v settled=%v", err, r.SettledByMe)
	}
	// 先锋（最早、会被旧 LIMIT 截断的那条）必须仍在参战名单里。
	if r.Participants != 3 {
		t.Fatalf("三名出手者都应计入（先锋未被截断），得到 %d", r.Participants)
	}
	foundEarly := false
	for _, a := range r.Awards {
		if a.UnitID == early.ID {
			foundEarly = true
		}
	}
	if !foundEarly {
		t.Fatalf("最早的出手者应仍分得战利品（账本未被 LIMIT 截断）")
	}
}

func TestWorldBossConcurrentSettleOnce(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	service.db.SetMaxOpenConns(1) // 串行化底层写，让并发体现在 Go 层交错（考验结算闩锁），避免 SQLITE_BUSY
	wid := mustCreateWorld(t, ctx, service)

	bossID, _ := service.SpawnWorldBoss(ctx, wid, "并发古神", 100, "")
	strikers := make([]*unit.Record, 0, 6)
	for i := 0; i < 6; i++ {
		strikers = append(strikers, bossStriker(t, ctx, repo, int64(70+i), "众", 25)) // 6×25=150 ≥ 100，必死
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	settledCount := 0
	for _, s := range strikers {
		wg.Add(1)
		go func(actor *unit.Record) {
			defer wg.Done()
			res, err := service.StrikeWorldBoss(ctx, wid, bossID, actor)
			if err != nil {
				return // 已被讨平的迟到出手会被拒，正常
			}
			if res.SettledByMe {
				mu.Lock()
				settledCount++
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()

	if settledCount != 1 {
		t.Fatalf("并发下应恰有 1 个请求执行结算（闩锁），得到 %d", settledCount)
	}
	if got := countCrossKind(t, service, wid, worldbus.KindWorldBossDown); got != 1 {
		t.Fatalf("并发下应恰有 1 条讨平广播，得到 %d", got)
	}
}
