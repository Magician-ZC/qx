package session

// 文件说明：世界Boss异步协作 PvE 集成测试（对真实 SQLite）：
// 共享血池原子扣血 → 总线贡献账本 → 血池清零全员分赃（epic 仲裁 + gold 按贡献瓜分）→ 单次结算闩锁防双结算。

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
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

// countActiveWorldBosses 数某世界当前 status='active' 的世界Boss行数。
func countActiveWorldBosses(t *testing.T, service *Service, worldID string) int {
	t.Helper()
	var n int
	if err := service.db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM world_bosses WHERE world_id = ? AND status = 'active'`, worldID).Scan(&n); err != nil {
		t.Fatalf("数 active Boss 失败: %v", err)
	}
	return n
}

// seedFieldBossTrace 在某世界沉淀一条 field_boss/elite「威胁浮现」痕迹（events 表 THREAT_EMERGED + world_id），
// 作为 world_boss 自动升级的 provenance 信号（②：world_boss 仅当该 region 已沉淀 ≥1 个 field_boss/elite 痕迹时才升级）。
// 痕迹挂在一个真实 owner 单位上（与生产一致：field_boss/elite 的 THREAT_EMERGED 由参战角色产出；events.actor_unit_id 有 units FK）。
func seedFieldBossTrace(t *testing.T, ctx context.Context, service *Service, worldID string) {
	t.Helper()
	owner := unit.BootstrapRecord(900, "s_trace", "player", "痕迹归属")
	if err := service.units.Save(ctx, owner); err != nil {
		t.Fatalf("保存痕迹归属单位失败: %v", err)
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		OwnerUnitID: owner.ID,
		Code:        events.ReasonThreatEmerged,
		Category:    events.CategoryLifecycle,
		Payload:     map[string]any{"tier": "field_boss"},
		WorldID:     worldID,
	}); err != nil {
		t.Fatalf("沉淀 field_boss 痕迹失败: %v", err)
	}
}

// 回归（修审计§8 已知低危 TOCTOU 竞态）：自动刷新路径并发 100 goroutine 同时调 maybeRefreshWorldBoss，
// 必须恰好生成 1 头 active Boss（原子条件 INSERT ... WHERE NOT EXISTS），绝不出现多头（违反单世界至多一头）。
// 早先「COUNT active → 无则 INSERT」两步实现下，多请求都见 0 → 都 INSERT → 会插多头；本测试正是该缺陷的护栏。
func TestWorldBossAutoRefreshConcurrentSpawnsAtMostOne(t *testing.T) {
	t.Setenv("QUNXIANG_WORLD_BOSS_AUTO", "1") // 开自动刷新 flag（默认关）
	_, _, service := newThreatTestService(t)
	service.db.SetMaxOpenConns(1) // 串行化底层写，让并发体现在 Go 层交错（考验原子条件 INSERT），避免 SQLITE_BUSY
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	seedFieldBossTrace(t, ctx, service, wid) // ② provenance 门：先沉淀 field_boss 痕迹，自动升级才会发生

	const goroutines = 100
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("并发自动刷新不应报错（best-effort），首个错误: %v", firstErr)
	}
	// 关键不变量：恰好一头 active Boss——既不能 0（flag 已开、世界存在，应生成一头），也绝不能 >1（TOCTOU 多头）。
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("并发自动刷新后应恰有 1 头 active Boss，得到 %d（>1 即 TOCTOU 多头缺陷）", got)
	}
}

// 自动刷新具有幂等性：已有 active Boss（含手动投放）时再调不应新增；不同世界互不干扰。
func TestWorldBossAutoRefreshIdempotentAndScoped(t *testing.T) {
	t.Setenv("QUNXIANG_WORLD_BOSS_AUTO", "1")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	seedFieldBossTrace(t, ctx, service, wid) // ② provenance 门：先沉淀 field_boss 痕迹

	// 首次自动刷新：无 active → 生成一头。
	if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
		t.Fatalf("首次自动刷新失败: %v", err)
	}
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("首次自动刷新应生成恰 1 头，得到 %d", got)
	}
	// 再调多次：已有 active → 原子条件 INSERT 被 WHERE NOT EXISTS 挡下，不新增、不报错。
	for i := 0; i < 5; i++ {
		if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
			t.Fatalf("幂等自动刷新失败: %v", err)
		}
	}
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("已有 active Boss 时自动刷新不应新增，仍应为 1，得到 %d", got)
	}

	// 另一世界互不干扰：各自独立刷出一头。
	wid2 := mustCreateWorld(t, ctx, service)
	seedFieldBossTrace(t, ctx, service, wid2) // ② provenance 门：第二世界也先沉淀痕迹
	if err := service.maybeRefreshWorldBoss(ctx, wid2); err != nil {
		t.Fatalf("第二世界自动刷新失败: %v", err)
	}
	if got := countActiveWorldBosses(t, service, wid2); got != 1 {
		t.Fatalf("第二世界应独立生成恰 1 头，得到 %d", got)
	}
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("第一世界不应被第二世界刷新影响，仍应为 1，得到 %d", got)
	}
}

// flag 默认关时自动刷新整方法 no-op：零 DB 写、不生成任何 Boss。
func TestWorldBossAutoRefreshDisabledNoOp(t *testing.T) {
	// 不设 QUNXIANG_WORLD_BOSS_AUTO（默认关）；显式清空以隔离外部环境。
	t.Setenv("QUNXIANG_WORLD_BOSS_AUTO", "")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)

	if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
		t.Fatalf("flag 关时应 no-op 且不报错，得到 %v", err)
	}
	if got := countActiveWorldBosses(t, service, wid); got != 0 {
		t.Fatalf("flag 关时不应生成任何 Boss，得到 %d", got)
	}
}

// 回归（L4 唯一兜底）：partial unique index uq_world_boss_active 是硬兜底——同一世界第二头 active Boss 的裸 INSERT
// 必触发 UNIQUE 约束冲突，且该错误必被 isDupKeyErr 识别（maybeRefreshWorldBoss 据此把并发双插的冲突收敛为正常兜底）。
func TestWorldBossActiveUniqueIndexRejectsSecond(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)

	// 第一头 active（裸 INSERT，无 NOT EXISTS 守护）——成功。
	if _, err := service.SpawnWorldBoss(ctx, wid, "甲龙", 100, ""); err != nil {
		t.Fatalf("首头投放失败: %v", err)
	}
	// 第二头 active 同世界——partial unique index 必拒，返回 UNIQUE 约束冲突。
	_, err := service.SpawnWorldBoss(ctx, wid, "乙龙", 100, "")
	if err == nil {
		t.Fatalf("同世界第二头 active 应被 uq_world_boss_active 拒绝（硬兜底失效）")
	}
	// 该错误必被 isDupKeyErr 识别——这是 maybeRefreshWorldBoss 把冲突当「已有 active 正常兜底」的判据。
	if !isDupKeyErr(err) {
		t.Fatalf("唯一冲突错误应被 isDupKeyErr 识别，得到: %v", err)
	}
	// 不变量：仍恰好一头 active（第二头被拒、未落库）。
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("唯一冲突后仍应恰一头 active，得到 %d", got)
	}
}

// isDupKeyErr 的纯判定：UNIQUE/constraint/duplicate 子串命中（大小写不敏感）即判 dup-key；普通错误不误判。
func TestIsDupKeyErr(t *testing.T) {
	for _, s := range []string{
		"UNIQUE constraint failed: world_bosses.world_id",
		"Error 1062: Duplicate entry 'w1' for key 'uq_world_boss_active'",
		"some CONSTRAINT violation",
	} {
		if !isDupKeyErr(errorString(s)) {
			t.Fatalf("应判为 dup-key：%q", s)
		}
	}
	for _, s := range []string{"connection refused", "no such table", ""} {
		if isDupKeyErr(errorString(s)) {
			t.Fatalf("不应判为 dup-key：%q", s)
		}
	}
	if isDupKeyErr(nil) {
		t.Fatalf("nil 不应判为 dup-key")
	}
}

// errorString 是测试用的简易 error（避免引 errors 仅为构造字符串错误）。
type errorString string

func (e errorString) Error() string { return string(e) }

// 回归（L4 唯一兜底）：并发自动刷新被收敛为「恰好一头 active」，且任一并发请求绝不因唯一冲突外抛错误。
// SQLite 是单写者模型，写被串行化（SetMaxOpenConns(1) 避免 SQLITE_BUSY 噪声）：此时 NOT EXISTS 主护栏先挡下后到者，
// 收敛为一头、无错。唯一冲突（partial unique index）的硬兜底分支由 TestWorldBossActiveUniqueIndexRejectsSecond
// （真实 UNIQUE 错误）+ TestIsDupKeyErr（错误识别）确定性覆盖——maybeRefreshWorldBoss 据此把 dup-key 当正常兜底吞掉。
// 本测试守的是端到端不变量：无论走 NOT EXISTS 还是唯一冲突兜底，并发后恒「恰好一头 active、零外抛」。
func TestWorldBossAutoRefreshConcurrentConvergesToOne(t *testing.T) {
	t.Setenv("QUNXIANG_WORLD_BOSS_AUTO", "1")
	_, _, service := newThreatTestService(t)
	service.db.SetMaxOpenConns(1) // 串行化底层写，让并发体现在 Go 层交错，避免 SQLITE_BUSY
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	seedFieldBossTrace(t, ctx, service, wid) // ② provenance 门：先沉淀 field_boss 痕迹

	const goroutines = 60
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	// 唯一冲突/已有 active 必须被吞为正常兜底——绝不外抛（best-effort，绝不中断回合推进）。
	if firstErr != nil {
		t.Fatalf("并发自动刷新应被收敛为正常兜底、不外抛，首个错误: %v", firstErr)
	}
	// 关键不变量：恰好一头 active（既不 0、也绝不 >1）。
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("并发自动刷新后应恰一头 active，得到 %d", got)
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

// ---- ① 单人物理锁（设计 §7：world_boss severity>cap → 单人物理不解锁，必须组队）----

// worldBossHP 读某 Boss 当前血量（测试断言「锁住时未扣血」用）。
func worldBossHP(t *testing.T, service *Service, bossID string) int {
	t.Helper()
	var hp int
	if err := service.db.QueryRowContext(context.Background(),
		`SELECT hp_remaining FROM world_bosses WHERE id = ?`, bossID).Scan(&hp); err != nil {
		t.Fatalf("查 Boss 血量失败: %v", err)
	}
	return hp
}

// 单人撞高 severity 世界Boss：StrikeWorldBossParty 声明单人 party → 物理锁高光卡、**不开打**（不扣血、不记账本）。
func TestWorldBossSoloPartyPhysicallyLocked(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	bossID, err := service.SpawnWorldBoss(ctx, wid, "存亡级古龙", 200_000, "r1")
	if err != nil {
		t.Fatalf("投放世界Boss失败: %v", err)
	}
	lone := bossStriker(t, ctx, repo, 201, "独行客", 40)

	// 显式声明单人 party（仅含自己）→ 物理锁。
	r, err := service.StrikeWorldBossParty(ctx, wid, bossID, lone, []string{lone.ID})
	if err != nil {
		t.Fatalf("单人出手不应报错（应被锁、返回高光卡）: %v", err)
	}
	if !r.SoloLocked {
		t.Fatalf("单人撞高 severity 世界Boss 应被物理锁，得到 SoloLocked=%v", r.SoloLocked)
	}
	if r.Defeated || r.Damage != 0 {
		t.Fatalf("物理锁不应开打：期望 defeated=false damage=0，得到 defeated=%v damage=%d", r.Defeated, r.Damage)
	}
	if r.SoloCard == "" {
		t.Fatalf("物理锁应返回「这不是一个人能撼动的」高光卡，得到空")
	}
	// 关键不变量：未扣血（满血）、账本无该击（没真打）。
	if hp := worldBossHP(t, service, bossID); hp != 200_000 {
		t.Fatalf("物理锁不应扣血，期望满血 200000，得到 %d", hp)
	}
	if got := countCrossKind(t, service, wid, worldbus.KindWorldBossStrike); got != 0 {
		t.Fatalf("物理锁不应记账本，期望 0 条出手，得到 %d", got)
	}
}

// 组队（≥2 成员）解锁：声明双人 party → 不锁，真打、扣血、记账本。
func TestWorldBossGroupPartyUnlocks(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	bossID, _ := service.SpawnWorldBoss(ctx, wid, "存亡级古龙", 200_000, "r1")
	a := bossStriker(t, ctx, repo, 211, "甲", 40)
	b := bossStriker(t, ctx, repo, 212, "乙", 40)

	// 双人 party → 解锁，正常出手。
	r, err := service.StrikeWorldBossParty(ctx, wid, bossID, a, []string{a.ID, b.ID})
	if err != nil {
		t.Fatalf("组队出手失败: %v", err)
	}
	if r.SoloLocked {
		t.Fatalf("组队（≥2）应解锁单人门，得到 SoloLocked=true")
	}
	if r.Damage != 40 || r.HPRemaining != 200_000-40 {
		t.Fatalf("组队应真打：期望 damage=40 hp=%d，得到 damage=%d hp=%d", 200_000-40, r.Damage, r.HPRemaining)
	}
	if got := countCrossKind(t, service, wid, worldbus.KindWorldBossStrike); got != 1 {
		t.Fatalf("组队真打应记 1 条账本，得到 %d", got)
	}
}

// 协作共享血池出手（StrikeWorldBoss，party 未声明=nil）不受单人物理锁约束：共享血池机制本身即协作语义。
func TestWorldBossCooperativeStrikeNotSoloLocked(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	bossID, _ := service.SpawnWorldBoss(ctx, wid, "存亡级古龙", 200_000, "")
	a := bossStriker(t, ctx, repo, 221, "甲", 40)

	r, err := service.StrikeWorldBoss(ctx, wid, bossID, a)
	if err != nil {
		t.Fatalf("协作出手失败: %v", err)
	}
	if r.SoloLocked {
		t.Fatalf("协作共享血池出手（party 未声明）不应触发单人物理锁")
	}
	if r.Damage != 40 {
		t.Fatalf("协作出手应真打，期望 damage=40，得到 %d", r.Damage)
	}
}

// 账本已沉淀别的出手者时，即便声明单人 party 的迟到者也解锁（这场围猎已是群体协力，不是孤身独闯）。
func TestWorldBossSoloPartyUnlockedWhenLedgerHasOthers(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	bossID, _ := service.SpawnWorldBoss(ctx, wid, "存亡级古龙", 200_000, "")
	a := bossStriker(t, ctx, repo, 231, "先驱", 40)
	b := bossStriker(t, ctx, repo, 232, "后到", 40)

	// 先驱以协作入口出手，账本里留下一名出手者。
	if _, err := service.StrikeWorldBoss(ctx, wid, bossID, a); err != nil {
		t.Fatalf("先驱出手失败: %v", err)
	}
	// 后到者即便声明单人 party：账本已有别人（先驱）→ 围猎已成群体 → 解锁。
	r, err := service.StrikeWorldBossParty(ctx, wid, bossID, b, []string{b.ID})
	if err != nil {
		t.Fatalf("后到者出手失败: %v", err)
	}
	if r.SoloLocked {
		t.Fatalf("账本已有别的出手者时，迟到的单人也应解锁（群体围猎），得到 SoloLocked=true")
	}
	if r.Damage != 40 {
		t.Fatalf("解锁后应真打，期望 damage=40，得到 %d", r.Damage)
	}
}

// isSoloParty / worldBossSeverity 的纯判定（确定性、付费不进）。
func TestSoloPartyAndSeverityPure(t *testing.T) {
	// party 未声明（nil/[]）→ 协作语义，非单人。
	if isSoloParty("u1", nil) {
		t.Fatalf("party 未声明应判协作（非单人）")
	}
	if isSoloParty("u1", []string{}) {
		t.Fatalf("空 party 应判协作（非单人）")
	}
	// 仅含自己 / 自己重复 → 单人。
	if !isSoloParty("u1", []string{"u1"}) {
		t.Fatalf("party 仅含自己应判单人")
	}
	if !isSoloParty("u1", []string{"u1", "u1"}) {
		t.Fatalf("party 自己重复仍应判单人")
	}
	// 含别人 → 组队。
	if isSoloParty("u1", []string{"u1", "u2"}) {
		t.Fatalf("party 含别的成员应判组队（非单人）")
	}
	// world_boss severity 恒 >cap（任何血量档）：单人门必触发。
	for _, hp := range []int{1, 5, 120_000, 200_000, 600_000, maxWorldBossHP} {
		sev := worldBossSeverity(hp)
		if sev <= worldBossSeverityCap {
			t.Fatalf("world_boss severity 应恒 >cap(%v)，hp=%d 得到 severity=%v", worldBossSeverityCap, hp, sev)
		}
		if encounter.SoloAllowed(sev, worldBossSeverityCap) {
			t.Fatalf("world_boss(hp=%d severity=%v) 单人门应不解锁", hp, sev)
		}
	}
	// severity 随血量单调不降（确定性）。
	if worldBossSeverity(200_000) < worldBossSeverity(120_000) {
		t.Fatalf("severity 应随血量单调不降")
	}
}

// ---- ② 威胁度链升级 provenance（设计 §1：world_boss 仅当沉淀 ≥1 个 field_boss/elite 痕迹时才升级，不凭空刷）----

// 无 provenance 信号（无任何 field_boss/elite 痕迹）→ 自动刷新不凭空 spawn（即便 flag 开）。
func TestWorldBossAutoRefreshNoProvenanceNoSpawn(t *testing.T) {
	t.Setenv("QUNXIANG_WORLD_BOSS_AUTO", "1")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)

	// 不沉淀任何 THREAT_EMERGED 痕迹 → 威胁度链无前因 → 不应凭空 spawn。
	if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
		t.Fatalf("无 provenance 时应 no-op 且不报错，得到 %v", err)
	}
	if got := countActiveWorldBosses(t, service, wid); got != 0 {
		t.Fatalf("无 provenance 信号不应凭空 spawn，得到 %d 头", got)
	}
}

// 有 field_boss 痕迹时才升级：沉淀 ≥1 个 THREAT_EMERGED 后自动刷新生成一头，并记 provenance 流程事件。
func TestWorldBossAutoRefreshWithProvenanceSpawnsAndRecords(t *testing.T) {
	t.Setenv("QUNXIANG_WORLD_BOSS_AUTO", "1")
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	wid := mustCreateWorld(t, ctx, service)
	seedFieldBossTrace(t, ctx, service, wid) // 沉淀一个 field_boss 痕迹（provenance 信号）

	if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
		t.Fatalf("有 provenance 时自动刷新失败: %v", err)
	}
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("有 provenance 信号应升级生成恰 1 头，得到 %d", got)
	}
	// provenance 应被记入世界总线（kind=WORLD_BOSS_SPAWN + payload.provenance=threat_escalation）。
	if got := countWorldBossProvenance(t, service, wid); got != 1 {
		t.Fatalf("应恰记 1 条 world_boss 升级 provenance 留痕，得到 %d", got)
	}

	// 幂等：再调（已有 active）不新增 Boss、也不再重复记 provenance（RowsAffected==0 不留痕）。
	if err := service.maybeRefreshWorldBoss(ctx, wid); err != nil {
		t.Fatalf("幂等自动刷新失败: %v", err)
	}
	if got := countActiveWorldBosses(t, service, wid); got != 1 {
		t.Fatalf("已有 active 时不应新增，仍应为 1，得到 %d", got)
	}
	if got := countWorldBossProvenance(t, service, wid); got != 1 {
		t.Fatalf("no-op 兜底不应重复记 provenance，仍应为 1，得到 %d", got)
	}
}

// countWorldBossProvenance 数某世界总线上的 world_boss 升级 provenance 留痕（kind=WORLD_BOSS_SPAWN + provenance=threat_escalation）。
func countWorldBossProvenance(t *testing.T, service *Service, worldID string) int {
	t.Helper()
	evs, err := worldbus.ListByWorldKind(context.Background(), service.db, worldID, worldBossSpawnProvenanceKind)
	if err != nil {
		t.Fatalf("读 provenance 总线失败: %v", err)
	}
	n := 0
	for _, e := range evs {
		var p struct {
			Provenance string `json:"provenance"`
			Tier       string `json:"tier"`
		}
		if raw, ok := e.Payload.(json.RawMessage); ok {
			_ = json.Unmarshal(raw, &p)
		}
		if p.Provenance == "threat_escalation" && p.Tier == "world_boss" {
			n++
		}
	}
	return n
}
