package session

// 文件说明：威胁刷新真调度测试（设计 PvE威胁系统.md §1）。覆盖：
//   ① threat_spawn_score 公式（0.5·level + 0.3·anchor + 0.2·fresh）对各项单调；
//   ② threat_level 近似随威胁/战斗事件累积单调不减；
//   ③ 锚密度高 → 出没概率高（更易落在她在乎处）；
//   ④ freshness 反扎堆 + 破圈下限（刚出过则压低，但 floor 恒保留）；
//   ⑤ arbitration 选址确定性可复现（同局同回合同候选集 → 同首位）。
// 纯函数项用单测；落库项对真实 SQLite。全程不依赖 wallet/billing（反 P2W）。

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"qunxiang/backend/internal/engine/arbitration"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

// TestThreatSpawnScore_Formula 验证 threat_spawn_score 三项各自单调 + 全零=0 + 全满=1（公式标定正确）。
func TestThreatSpawnScore_Formula(t *testing.T) {
	if s := threatSpawnScore(0, 0, 0); s != 0 {
		t.Fatalf("三项全零 score 应为 0，得到 %v", s)
	}
	if s := threatSpawnScore(100, 1, 1); s < 0.999 || s > 1.001 {
		t.Fatalf("三项全满 score 应为 1，得到 %v", s)
	}
	// threat_level 项单调（其余固定）。
	if threatSpawnScore(0, 0.3, 0.3) >= threatSpawnScore(100, 0.3, 0.3) {
		t.Fatalf("threat_level 升高应使 score 升高")
	}
	// anchor 项单调。
	if threatSpawnScore(50, 0, 0.3) >= threatSpawnScore(50, 1, 0.3) {
		t.Fatalf("anchor_density 升高应使 score 升高")
	}
	// freshness 项单调。
	if threatSpawnScore(50, 0.3, 0) >= threatSpawnScore(50, 0.3, 1) {
		t.Fatalf("freshness 升高应使 score 升高")
	}
	// 权重比例：level 权重 0.5 > anchor 0.3 > fresh 0.2（同样 +1 单位贡献递减）。
	dLevel := threatSpawnScore(100, 0, 0) - threatSpawnScore(0, 0, 0)
	dAnchor := threatSpawnScore(0, 1, 0) - threatSpawnScore(0, 0, 0)
	dFresh := threatSpawnScore(0, 0, 1) - threatSpawnScore(0, 0, 0)
	if !(dLevel > dAnchor && dAnchor > dFresh) {
		t.Fatalf("权重应满足 level>anchor>fresh，得到 %v %v %v", dLevel, dAnchor, dFresh)
	}
}

// TestSpawnProbFromScore_FloorAndCap 验证 score→概率映射：破圈下限恒保留、上限封顶、单调。
func TestSpawnProbFromScore_FloorAndCap(t *testing.T) {
	if p := spawnProbFromScore(0); p != threatSpawnFloor {
		t.Fatalf("score=0 应正好是破圈下限 %v，得到 %v", threatSpawnFloor, p)
	}
	if p := spawnProbFromScore(1); p != threatSpawnCap {
		t.Fatalf("score=1 应正好是上限 %v，得到 %v", threatSpawnCap, p)
	}
	// 越界 score 仍夹在 [floor,cap]。
	if p := spawnProbFromScore(-5); p != threatSpawnFloor {
		t.Fatalf("score<0 应夹到下限，得到 %v", p)
	}
	if p := spawnProbFromScore(5); p != threatSpawnCap {
		t.Fatalf("score>1 应夹到上限，得到 %v", p)
	}
	// 单调。
	if spawnProbFromScore(0.2) >= spawnProbFromScore(0.8) {
		t.Fatalf("概率应随 score 单调升高")
	}
}

// TestFreshnessFromTurns_Refractory 验证 freshness 反扎堆：同回合刚出=0（最强压制）、窗口外=1（完全恢复）、窗口内线性回升、回拨保护。
func TestFreshnessFromTurns_Refractory(t *testing.T) {
	if f := freshnessFromTurns(10, 10); f != 0 {
		t.Fatalf("Δturn=0（刚出）freshness 应为 0，得到 %v", f)
	}
	if f := freshnessFromTurns(10, 10+threatFreshnessWindowTurns); f != 1 {
		t.Fatalf("Δturn≥窗口 freshness 应为 1，得到 %v", f)
	}
	// 窗口内线性回升：半个窗口 ≈ 0.5。
	mid := freshnessFromTurns(10, 10+threatFreshnessWindowTurns/2)
	if mid <= 0 || mid >= 1 {
		t.Fatalf("窗口内 freshness 应在 (0,1)，得到 %v", mid)
	}
	// 单调回升。
	if freshnessFromTurns(10, 11) >= freshnessFromTurns(10, 13) {
		t.Fatalf("freshness 应随 Δturn 单调回升")
	}
	// 时钟回拨保护：curTurn<lastTurn 视为刚出（0）。
	if f := freshnessFromTurns(20, 5); f != 0 {
		t.Fatalf("回拨应视为刚出 freshness=0，得到 %v", f)
	}
}

// TestApproxThreatLevel_MonotonicWithEvents 验证 session 内 threat_level 近似随威胁/战斗类事件累积单调不减、封顶 100。
func TestApproxThreatLevel_MonotonicWithEvents(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	u := unit.BootstrapRecord(2, "s1", "player", "她")
	if err := repo.Save(ctx, u); err != nil {
		t.Fatalf("存角色: %v", err)
	}

	prev := service.approxThreatLevel(ctx, "s1")
	if prev != 0 {
		t.Fatalf("无事件时近似威胁度应为 0，得到 %d", prev)
	}
	// 逐条写威胁类事件，近似威胁度应单调不减。
	for i := 0; i < 30; i++ {
		if _, err := events.EmitProcessEvent(ctx, db, events.ProcessEvent{
			SessionID: "s1", OwnerUnitID: u.ID, Code: events.ReasonThreatEmerged,
			Category: events.CategoryLifecycle, Tick: i,
		}); err != nil {
			t.Fatalf("写威胁事件: %v", err)
		}
		cur := service.approxThreatLevel(ctx, "s1")
		if cur < prev {
			t.Fatalf("近似威胁度应单调不减：第 %d 条后 %d < %d", i, cur, prev)
		}
		prev = cur
	}
	if prev <= 0 {
		t.Fatalf("写够威胁事件后近似威胁度应 >0，得到 %d", prev)
	}
	if prev > int64(threatLevelScoreMax) {
		t.Fatalf("近似威胁度应封顶 %v，得到 %d", threatLevelScoreMax, prev)
	}
}

// TestRefreshThreats_HigherAnchorHigherSpawn 验证锚密度高 → 出没概率高：两个等同单位，一个被多人当作锚指向（高密度），
// 一个零锚；扫多回合，高锚单位被刷威胁的次数应明显多于零锚单位（威胁更易落在她在乎处）。
func TestRefreshThreats_HigherAnchorHigherSpawn(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	highAnchor := unit.BootstrapRecord(2, "s1", "player", "她")
	highAnchor.FactionID = "player"
	highAnchor.Status.HP = 100
	highAnchor.Status.LivesRemaining = 1
	zeroAnchor := unit.BootstrapRecord(3, "s1", "player", "路人")
	zeroAnchor.FactionID = "player"
	zeroAnchor.Status.HP = 100
	zeroAnchor.Status.LivesRemaining = 1
	for _, u := range []unit.Record{highAnchor, zeroAnchor} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("存角色: %v", err)
		}
	}
	// 让多名角色以 highAnchor 为锚（反向密度高）：写若干 relevance_anchors 指向 highAnchor.ID。
	for i := 0; i < 6; i++ {
		carer := "carer_" + string(rune('a'+i))
		// 先建一个挂锚的 owner 角色（满足 character_unit_id 列即可；无 FK 强约束到 units 的 anchor）。
		if err := service.UpsertAnchor(ctx, carer, relevance.DebtGrudgeLove, highAnchor.ID, 1.0, "在乎她", 0); err != nil {
			t.Fatalf("挂锚: %v", err)
		}
	}
	dHigh := service.AnchorDensityByRef(ctx, highAnchor.ID, "")
	dZero := service.AnchorDensityByRef(ctx, zeroAnchor.ID, "")
	if !(dHigh > dZero) {
		t.Fatalf("高锚单位反向密度应 > 零锚：%v vs %v", dHigh, dZero)
	}

	// 直接在「相同基线（threat_level=0、freshness=1）」下统计各单位每回合的确定性出没掷骰过阈率：
	// 隔离 threat_level 累积/arbitration 选址干扰，纯看锚密度对出没概率的影响（高密度→高 prob→更多过阈）。
	// 出没掷骰、阈值映射都是生产函数本体（threatRoll / spawnProbFromScore / threatSpawnScore），故这正是真调度的锚加权效应。
	passes := func(density float64) int {
		prob := spawnProbFromScore(threatSpawnScore(0, density, 1))
		uid := "probe" // 统一探针 unitID 不影响相对比较（两次用同一 roll 序列，只 prob 不同）
		n := 0
		for turn := 1; turn <= 1000; turn++ {
			if threatRoll("s1", turn, uid) < prob {
				n++
			}
		}
		return n
	}
	highPasses := passes(dHigh)
	zeroPasses := passes(dZero)
	if highPasses <= zeroPasses {
		t.Fatalf("高锚单位过阈次数应 > 零锚单位（威胁更易落她在乎处）：high=%d zero=%d (dHigh=%.3f dZero=%.3f)",
			highPasses, zeroPasses, dHigh, dZero)
	}
	// 破圈下限：零锚单位（density=0）每回合仍有 threatSpawnFloor 概率过阈，1000 回合内必被刷到（世界处处有危险）。
	if zeroPasses == 0 {
		t.Fatalf("零锚单位也应有破圈下限被刷到（1000 回合内 ≥1），得到 0")
	}

	// 端到端 sanity：真实跑 refreshThreats（含 arbitration 选址）一批回合，被刷的事件确有产生（投卡或开打留痕）。
	var surfaced int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE reason_code = ?`, string(events.ReasonInboxHighlight)).Scan(&surfaced)
	for turn := 1; turn <= 40; turn++ {
		st := State{ID: "s1", PlayerFactionID: "player"}
		st.TurnState.Turn = turn
		service.refreshThreats(ctx, &st, []unit.Record{highAnchor, zeroAnchor})
	}
	var after int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE reason_code = ?`, string(events.ReasonInboxHighlight)).Scan(&after)
	if after <= surfaced {
		t.Fatalf("真实 refreshThreats 应至少刷出一些威胁卡，before=%d after=%d", surfaced, after)
	}
}

// TestRefreshThreats_ArbitrationSiteDeterministic 验证跨阈选址用 arbitration 确定性首位、可复现：
// 同一 (sessionID, turn, 候选集) 重复跑 refreshThreats，选中的刷新点（被投卡单位）逐次一致。
func TestRefreshThreats_ArbitrationSiteDeterministic(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 三个等同合格单位，确保某回合多于一个过阈、必须靠 arbitration 选址（而非「碰巧只有一个候选」）。
	var units []unit.Record
	for i := 0; i < 3; i++ {
		u := unit.BootstrapRecord(int64(2+i), "s1", "player", "候选"+string(rune('A'+i)))
		u.FactionID = "player"
		u.Status.HP = 100
		u.Status.LivesRemaining = 1
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("存角色: %v", err)
		}
		units = append(units, u)
	}

	// 找一个「至少两个单位过阈」的回合，验证 arbitration 在该回合选址确定性。
	scoreAndProb := func(turn int, u unit.Record) (float64, bool) {
		st := State{ID: "s1", PlayerFactionID: "player"}
		st.TurnState.Turn = turn
		density := service.AnchorDensityByRef(ctx, u.ID, "")
		fresh := service.threatFreshness(ctx, &st, u.ID)
		score := threatSpawnScore(service.regionThreatLevel(ctx, &st), density, fresh)
		prob := spawnProbFromScore(score)
		return score, threatRoll("s1", turn, u.ID) < prob
	}

	chosenTurn := -1
	for turn := 1; turn <= 200 && chosenTurn < 0; turn++ {
		passed := 0
		for _, u := range units {
			if _, ok := scoreAndProb(turn, u); ok {
				passed++
			}
		}
		if passed >= 2 {
			chosenTurn = turn
		}
	}
	if chosenTurn < 0 {
		t.Skip("200 回合内未出现「≥2 单位同回合过阈」的样本，跳过选址确定性断言")
	}

	// 直接对该回合的候选集复算 arbitration 首位，验证确定性（同 Key+同 Contestants → 同 WinnerID）。
	contestants := make([]arbitration.Contestant, 0, len(units))
	for _, u := range units {
		score, ok := scoreAndProb(chosenTurn, u)
		if ok {
			contestants = append(contestants, arbitration.Contestant{UnitID: u.ID, Score: score})
		}
	}
	key := "threat-spawn:s1:" + strconv.Itoa(chosenTurn)
	a := arbitration.Resolve(arbitration.Contest{Key: key, Resource: "wild_threat_site", Contestants: contestants})
	b := arbitration.Resolve(arbitration.Contest{Key: key, Resource: "wild_threat_site", Contestants: contestants})
	if a.WinnerID == "" {
		t.Fatalf("arbitration 应选出一个刷新点")
	}
	if a.WinnerID != b.WinnerID {
		t.Fatalf("同 Key+同候选集选址应确定性可复现：%q vs %q", a.WinnerID, b.WinnerID)
	}

	// 端到端：实际跑一次 refreshThreats（该回合），被投卡/记痕的单位应正是 arbitration 复算首位
	// （确认 refreshThreats 真用的就是 arbitration 确定性选址，而非纯随机）。
	// 注：跑过一次后该 winner 的 freshness 被 recordThreatHit 压低（反扎堆）→ 重跑会换人，这是设计正确行为，故只断言首次落地。
	investigate := func() string {
		st := State{ID: "s1", PlayerFactionID: "player"}
		st.TurnState.Turn = chosenTurn
		before := map[string]int{}
		for _, u := range units {
			before[u.ID] = cardCount(t, db, u.ID)
		}
		service.refreshThreats(ctx, &st, units)
		for _, u := range units {
			if cardCount(t, db, u.ID) > before[u.ID] {
				return u.ID
			}
		}
		return ""
	}
	first := investigate()
	if first == "" {
		t.Fatalf("该回合应至少投出一张卡")
	}
	// 落地选址应与 arbitration 复算首位一致（确认 refreshThreats 用的就是 arbitration 选址，非纯随机）。
	if first != a.WinnerID {
		t.Fatalf("refreshThreats 落地选址应等于 arbitration 复算首位：%q vs %q", first, a.WinnerID)
	}
}

// cardCount 统计某单位被投出的威胁出没卡（ReasonInboxHighlight）数。
func cardCount(t *testing.T, db *sql.DB, uid string) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		uid, string(events.ReasonInboxHighlight)).Scan(&n); err != nil {
		t.Fatalf("统计卡数: %v", err)
	}
	return n
}
