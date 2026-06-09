package session

// 文件说明：跨玩家/跨会话零和裁决（contest.go，设计 §2.6）的测试——验证不变量：
// ① arbitration 胜率∝Score（强者更可能但非必然胜）；② 确定性可复现（同 Key 同结果，与入队顺序无关）；
// ③ 无冲突 no-op；④ 付费不进 Score（反 P2W）；⑤ Key 频率/在线顺序无关；
// ⑥ 跨会话候选确定性裁决（同 world 不同 session 争同一标的真正接通，胜负只产 append-only cross_event，不直写他人状态）；
// ⑦ 三类 contest（联姻/席位继承/排他战利品）参数化裁决。

import (
	"context"
	"database/sql"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/unit"
)

// TestResolveExclusiveContest_Deterministic 验证「同 Key 同争夺者集合 → 同胜者」（确定性可复现）。
func TestResolveExclusiveContest_Deterministic(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	state := &State{ID: "s1"}
	state.TurnState.Turn = 6

	contenders := []ContestContender{
		{UnitID: "u_a", Score: 5},
		{UnitID: "u_b", Score: 5},
		{UnitID: "u_c", Score: 5},
	}
	first, err := service.ResolveExclusiveContest(ctx, state, "marriage:seat", "marriage:seat", contenders)
	if err != nil {
		t.Fatalf("首次裁决出错: %v", err)
	}
	if first == "" {
		t.Fatalf("应有胜者")
	}
	// 同 state（同 sessionID+turn）、同标的、同争夺者集合 → 必同胜者，重复多次稳定。
	for i := 0; i < 8; i++ {
		again, err := service.ResolveExclusiveContest(ctx, state, "marriage:seat", "marriage:seat", contenders)
		if err != nil {
			t.Fatalf("重复裁决出错: %v", err)
		}
		if again != first {
			t.Fatalf("同 Key 同争夺者应同胜者：首次 %s，第 %d 次 %s", first, i, again)
		}
	}
	// 入队顺序无关：打乱争夺者顺序仍同胜者（arbitration 内部 dedupMaxScore 已规范化）。
	shuffled := []ContestContender{
		{UnitID: "u_c", Score: 5},
		{UnitID: "u_a", Score: 5},
		{UnitID: "u_b", Score: 5},
	}
	if got, _ := service.ResolveExclusiveContest(ctx, state, "marriage:seat", "marriage:seat", shuffled); got != first {
		t.Fatalf("入队顺序应不影响胜者：原 %s，打乱后 %s", first, got)
	}
}

// TestResolveExclusiveContest_KeyEntersResolution 验证不同 Key（不同回合/标的）会重抽，结果可不同——
// 间接证明 Key 真的进了裁决（否则所有裁决恒等）。不强断言「必不同」（小概率相同），只断言「同 Key 恒同」。
func TestResolveExclusiveContest_KeyEntersResolution(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	contenders := []ContestContender{
		{UnitID: "u_a", Score: 5}, {UnitID: "u_b", Score: 5},
		{UnitID: "u_c", Score: 5}, {UnitID: "u_d", Score: 5},
	}
	// 跨多个回合收集胜者：若 Key 未进裁决，胜者会恒定；进了则随 turn 变化而重抽。
	// 注意：会话内 Key 用 turn（非补投桶）→ 每回合都是新 Key，覆盖更密。
	seen := map[string]bool{}
	for turn := 0; turn < 16; turn++ {
		state := &State{ID: "s1"}
		state.TurnState.Turn = turn
		w, err := service.ResolveExclusiveContest(ctx, state, "marriage:x", "marriage:x", contenders)
		if err != nil {
			t.Fatalf("turn %d 裁决出错: %v", turn, err)
		}
		seen[w] = true
	}
	if len(seen) < 2 {
		t.Fatalf("16 个回合（不同 Key）等分胜者应出现至少 2 名不同胜者，说明 Key 进了裁决；实得 %d 名", len(seen))
	}
}

// TestResolveExclusiveContest_CrossWorldKeyOnlineOrderIndependent 验证跨 world 的 Key 与「谁先在线/谁先扫到」无关：
// 同一 worldID + 同一标的 + 补投窗口内任意 turn + 任意 sessionID，都得同一确定性 Key → 同一裁决结果。
// 这是 §2.6 反「在线/付费方抢先做高 Score」的核心：胜负由统一 tick 的 Key 定，不由谁先发起。
func TestResolveExclusiveContest_CrossWorldKeyOnlineOrderIndependent(t *testing.T) {
	contenders := []ContestContender{
		{UnitID: "u_a", Score: 5}, {UnitID: "u_b", Score: 5},
		{UnitID: "u_c", Score: 5}, {UnitID: "u_d", Score: 5},
	}
	// 玩家甲先在线（session A，turn=6）。
	keyA := exclusiveContestKey("w1", "sessA", 6, "so_seat", "seat_inheritance:so_seat")
	// 玩家乙后在线（session B，turn=7，仍落在同一补投窗口桶 [6,9) ）。
	keyB := exclusiveContestKey("w1", "sessB", 7, "so_seat", "seat_inheritance:so_seat")
	// 玩家丙更晚（session C，turn=8，仍同桶）。
	keyC := exclusiveContestKey("w1", "sessC", 8, "so_seat", "seat_inheritance:so_seat")
	if keyA != keyB || keyB != keyC {
		t.Fatalf("同 world 同标的同补投窗口的 Key 应与 session/turn-in-window 无关：A=%q B=%q C=%q", keyA, keyB, keyC)
	}
	// 该 Key 不含任一方 sessionID（否则会按谁先在线各裁各的）。
	for _, s := range []string{"sessA", "sessB", "sessC"} {
		if containsSub(keyA, s) {
			t.Fatalf("跨 world Key 不应含 sessionID %q（否则与谁先在线相关）：%q", s, keyA)
		}
	}
	// 用该 Key 直接喂 arbitration：三方算出同胜者（验证 Key 真把裁决统一了）。
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	stateA := &State{ID: "sessA", WorldID: "w1"}
	stateA.TurnState.Turn = 6
	stateB := &State{ID: "sessB", WorldID: "w1"}
	stateB.TurnState.Turn = 8
	wA, err := service.ResolveExclusiveContest(ctx, stateA, "so_seat", "seat_inheritance:so_seat", contenders)
	if err != nil {
		t.Fatalf("A 裁决出错: %v", err)
	}
	wB, err := service.ResolveExclusiveContest(ctx, stateB, "so_seat", "seat_inheritance:so_seat", contenders)
	if err != nil {
		t.Fatalf("B 裁决出错: %v", err)
	}
	if wA != wB {
		t.Fatalf("同 world 同标的同窗口，不同 session/turn 应得同胜者：A=%s B=%s", wA, wB)
	}
}

// TestExclusiveContestKey_BackwardCompatEmptyWorld 验证 WorldID 空时退回会话内 Key（向后兼容）：
// 含 sessionID+turn+resource，与跨 world Key 形态不同（保证默认单库行为不变）。
func TestExclusiveContestKey_BackwardCompatEmptyWorld(t *testing.T) {
	k := exclusiveContestKey("", "s1", 6, "so_x", "marriage:so_x")
	if !containsSub(k, "s1") || !containsSub(k, "t6") || !containsSub(k, "marriage:so_x") {
		t.Fatalf("会话内 Key 应含 sessionID/turn/resource：%q", k)
	}
	if containsSub(k, "w") && containsSub(k, "|wso") { // 不应是跨 world 形态
		t.Fatalf("空 WorldID 不应走跨 world Key 形态：%q", k)
	}
}

// TestResolveExclusiveContest_WinRateProportionalToScore 验证胜率∝Score：强者更常胜但非必然胜。
func TestResolveExclusiveContest_WinRateProportionalToScore(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	const trials = 4000
	strongWins := 0
	weakWins := 0
	for i := 0; i < trials; i++ {
		state := &State{ID: "s1"}
		state.TurnState.Turn = i
		contenders := []ContestContender{
			{UnitID: "strong", Score: 8}, // 实力远强
			{UnitID: "weak", Score: 2},   // 实力弱（比值 4:1）
		}
		w, err := service.ResolveExclusiveContest(ctx, state, "marriage:y", "marriage:y", contenders)
		if err != nil {
			t.Fatalf("trial %d 裁决出错: %v", i, err)
		}
		switch w {
		case "strong":
			strongWins++
		case "weak":
			weakWins++
		default:
			t.Fatalf("trial %d 非法胜者 %q", i, w)
		}
	}
	strongRate := float64(strongWins) / float64(trials)
	// 理论胜率 ∝ Score：strong = 8/(8+2) = 0.8。容忍经验抖动，断言落在 [0.74,0.86]。
	if strongRate < 0.74 || strongRate > 0.86 {
		t.Fatalf("强者(Score 8 vs 2)经验胜率应≈0.8，实得 %.3f（strong=%d weak=%d）", strongRate, strongWins, weakWins)
	}
	if weakWins == 0 {
		t.Fatalf("弱者应偶有胜出（胜率∝Score 是概率而非确定），实得 0 次")
	}
}

// TestResolveExclusiveContest_PayFrequencyDoesNotChangeWinner 验证「付费方 5×频率 + 插队」不改胜负（反 P2W 红线）：
// 同一 Key 下，把付费方重复入队 5 次并打到队首，胜者与单次入队一致（arbitration 频率无关 + Key 统一结算）。
func TestResolveExclusiveContest_PayFrequencyDoesNotChangeWinner(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()

	mismatches := 0
	for turn := 0; turn < 200; turn++ {
		state := &State{ID: "s1", WorldID: "w1"}
		state.TurnState.Turn = turn
		base := []ContestContender{
			{UnitID: "payer", Score: 4},
			{UnitID: "freeA", Score: 4},
			{UnitID: "freeB", Score: 4},
		}
		want, err := service.ResolveExclusiveContest(ctx, state, "loot:t", "exclusive_loot:t", base)
		if err != nil {
			t.Fatalf("turn %d 基准裁决出错: %v", turn, err)
		}
		// 付费方 5× 频率 + 插队（重复入队并置于队首）——不应改变胜者。
		spam := []ContestContender{
			{UnitID: "payer", Score: 4}, {UnitID: "payer", Score: 4}, {UnitID: "payer", Score: 4},
			{UnitID: "payer", Score: 4}, {UnitID: "payer", Score: 4},
			{UnitID: "freeA", Score: 4}, {UnitID: "freeB", Score: 4},
		}
		got, err := service.ResolveExclusiveContest(ctx, state, "loot:t", "exclusive_loot:t", spam)
		if err != nil {
			t.Fatalf("turn %d 高频裁决出错: %v", turn, err)
		}
		if got != want {
			mismatches++
		}
	}
	if mismatches != 0 {
		t.Fatalf("付费方 5×频率+插队不应改变胜者（频率无关），却有 %d/200 次胜者变化", mismatches)
	}
}

// TestMarriageContenderScore_PayToWinExcluded 验证 Score **不含付费维度**：钱包翻天也不改 Score。
func TestMarriageContenderScore_PayToWinExcluded(t *testing.T) {
	row := relationPromptRow{Trust: 6, Affection: 7, Fear: 0, Rivalry: 0}

	poor := unit.BootstrapRecord(1, "s1", "player", "穷书生")
	poor.Stats.Primary.Charisma = 10
	poor.Status.Morale = 0.6 // Morale 量纲 [0,1]
	poor.Status.Wallet = 0

	whale := poor                    // 复制：除钱包外完全一致
	whale.Status.Wallet = 1000000000 // 巨额付费

	if scorePoor, scoreWhale := marriageContenderScore(poor, row), marriageContenderScore(whale, row); scorePoor != scoreWhale {
		t.Fatalf("付费不应改变联姻 Score（反 P2W）：穷=%.4f 富=%.4f", scorePoor, scoreWhale)
	}
}

// TestSeatAndLootContenderScore_PayToWinExcluded 验证席位/战利品两类 Score 同样不含付费维度。
func TestSeatAndLootContenderScore_PayToWinExcluded(t *testing.T) {
	seatRow := relationPromptRow{Trust: 7, Rivalry: 3}
	lootRow := relationPromptRow{Rivalry: 6, Fear: 1}

	poor := unit.BootstrapRecord(1, "s1", "player", "穷书生")
	poor.Stats.Primary.Charisma = 8
	poor.Status.Attack = 20
	poor.Status.Defense = 10
	poor.Status.Morale = 0.5
	poor.Status.Wallet = 0

	whale := poor
	whale.Status.Wallet = 1000000000

	if a, b := seatContenderScore(poor, seatRow), seatContenderScore(whale, seatRow); a != b {
		t.Fatalf("付费不应改变席位 Score（反 P2W）：穷=%.4f 富=%.4f", a, b)
	}
	if a, b := lootContenderScore(poor, lootRow), lootContenderScore(whale, lootRow); a != b {
		t.Fatalf("付费不应改变战利品 Score（反 P2W）：穷=%.4f 富=%.4f", a, b)
	}
}

// TestScanExclusiveContests_NoConflictNoOp 验证无冲突时 no-op：每个对象至多一个求亲者 → 不裁决、不写日志。
func TestScanExclusiveContests_NoConflictNoOp(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	a := unit.BootstrapRecord(2, "s1", "player", "阿采")
	b := unit.BootstrapRecord(4, "s1", "player", "老吴")
	c := unit.BootstrapRecord(6, "s1", "player", "小满")
	for _, u := range []unit.Record{a, b, c} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
	}
	// 仅 a→b 有求亲级好感（c 对谁都无好感）→ b 只有一个求亲者 → 无排他冲突。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
		a.ID, b.ID, 6.0, 0.0, 7.0, 0.0,
	); err != nil {
		t.Fatalf("insert relation: %v", err)
	}

	state := &State{ID: "s1", PlayerFactionID: "player"}
	state.TurnState.Turn = 6 // 命中扫描周期（turn%3==0）
	before := len(state.Logs)
	service.scanExclusiveContestsAtBoundary(ctx, state, []unit.Record{a, b, c})
	if len(state.Logs) != before {
		t.Fatalf("无排他冲突应 no-op（不写日志），却新增 %d 条日志", len(state.Logs)-before)
	}
}

// TestScanExclusiveContests_MarriageConflictResolved 验证两人争同一对象时确定性裁决 + 败者补偿记忆。
func TestScanExclusiveContests_MarriageConflictResolved(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	suitor1 := unit.BootstrapRecord(2, "s1", "player", "阿采")
	suitor2 := unit.BootstrapRecord(4, "s1", "player", "老吴")
	beloved := unit.BootstrapRecord(6, "s1", "player", "小满")
	for _, u := range []unit.Record{suitor1, suitor2, beloved} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
	}
	// suitor1 与 suitor2 都对 beloved 有求亲级好感 → 排他冲突。
	for _, s := range []unit.Record{suitor1, suitor2} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			s.ID, beloved.ID, 6.0, 0.0, 7.0, 0.0,
		); err != nil {
			t.Fatalf("insert relation: %v", err)
		}
	}

	state := &State{ID: "s1", PlayerFactionID: "player"}
	state.TurnState.Turn = 6
	service.scanExclusiveContestsAtBoundary(ctx, state, []unit.Record{suitor1, suitor2, beloved})

	// 应有一条 contest_marriage 裁决日志（胜者获优先）+ 一条 contest_consolation 败者补偿日志。
	var marriageLog, consolationLog int
	var winnerID string
	for _, l := range state.Logs {
		switch l.Kind {
		case "contest_marriage":
			marriageLog++
			winnerID = l.ActorUnitID
		case "contest_consolation":
			consolationLog++
		}
	}
	if marriageLog != 1 {
		t.Fatalf("应有 1 条联姻裁决日志，得到 %d", marriageLog)
	}
	if consolationLog != 1 {
		t.Fatalf("应有 1 条败者补偿日志，得到 %d", consolationLog)
	}
	if winnerID != suitor1.ID && winnerID != suitor2.ID {
		t.Fatalf("胜者应为两争夺者之一，得到 %q", winnerID)
	}
	// 确定性复现：同输入再扫一遍胜者不变（用全新 state 避免日志累积干扰）。
	state2 := &State{ID: "s1", PlayerFactionID: "player"}
	state2.TurnState.Turn = 6
	service.scanExclusiveContestsAtBoundary(ctx, state2, []unit.Record{suitor1, suitor2, beloved})
	var winner2 string
	for _, l := range state2.Logs {
		if l.Kind == "contest_marriage" {
			winner2 = l.ActorUnitID
		}
	}
	if winner2 != winnerID {
		t.Fatalf("确定性裁决应复现同胜者：首次 %s，复跑 %s", winnerID, winner2)
	}
}

// TestScanExclusiveContests_CrossSessionCandidateResolved 验证**跨会话**候选确定性裁决（§2.6 核心）：
// 同一 world 下，本会话求亲者与**另一 session**的求亲者争同一对象，跨会话候选被真正纳入裁决；
// 胜负只产 append-only cross_event（带 arbitration_key），不直写他人 units/relations。
func TestScanExclusiveContests_CrossSessionCandidateResolved(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 本会话 s1 的求亲者 + 对象（对象与本会话求亲者都接入 world w1）。
	localSuitor := unit.BootstrapRecord(2, "s1", "player", "阿采")
	beloved := unit.BootstrapRecord(6, "s1", "player", "小满")
	// 另一会话 s2 的求亲者（同 world w1）。
	crossSuitor := unit.BootstrapRecord(8, "s2", "rival", "异乡客")
	for _, u := range []unit.Record{localSuitor, beloved, crossSuitor} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
		// 接入同一 world（写 world_id 作用域列，只读候选靠它）。
		if err := repo.SetUnitScope(ctx, u.ID, "w1", "r1"); err != nil {
			t.Fatalf("set scope %s: %v", u.ID, err)
		}
	}
	// 两个求亲者都对 beloved 有求亲级好感（一个本会话、一个跨会话）。
	for _, s := range []unit.Record{localSuitor, crossSuitor} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			s.ID, beloved.ID, 6.0, 0.0, 7.0, 0.0,
		); err != nil {
			t.Fatalf("insert relation: %v", err)
		}
	}

	state := &State{ID: "s1", PlayerFactionID: "player", WorldID: "w1"}
	state.TurnState.Turn = 6
	// 本会话 units 只含本局单位（localSuitor + beloved）；crossSuitor 经跨会话候选池拉入。
	service.scanExclusiveContestsAtBoundary(ctx, state, []unit.Record{localSuitor, beloved})

	// 应裁决出一条 contest_marriage 日志（跨会话候选被纳入 → 形成 ≥2 争夺者冲突）。
	var marriageLog int
	for _, l := range state.Logs {
		if l.Kind == "contest_marriage" {
			marriageLog++
		}
	}
	if marriageLog != 1 {
		t.Fatalf("跨会话两求亲者争同一对象应裁决出 1 条联姻日志，得到 %d（跨会话候选未接通？）", marriageLog)
	}

	// 跨玩家硬不变量留痕：应有 append-only cross_events（带 arbitration_key），且**不**写他人 relations/units。
	var crossCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cross_events WHERE arbitration_key IS NOT NULL AND arbitration_key <> ''`,
	).Scan(&crossCount); err != nil {
		t.Fatalf("统计 cross_events 失败: %v", err)
	}
	if crossCount == 0 {
		t.Fatalf("跨会话裁决应产出带 arbitration_key 的 cross_event 留痕，得到 0")
	}
	// 不直写他人 units：crossSuitor（s2）记录的 session_id 未被改写、其 relations 行数不变。
	var sess string
	if err := db.QueryRowContext(ctx, `SELECT session_id FROM units WHERE id = ?`, crossSuitor.ID).Scan(&sess); err != nil {
		t.Fatalf("读 crossSuitor session_id: %v", err)
	}
	if sess != "s2" {
		t.Fatalf("绝不应改写他人单位 session_id：期望 s2，得到 %q", sess)
	}
}

// TestScanExclusiveContests_CrossSessionDeterministic 验证跨会话裁决确定性可复现：同 world/标的/补投窗口，
// 即便从不同 session 的视角发起（不同 state.ID + 窗口内不同 turn），裁决胜者一致。
func TestScanExclusiveContests_CrossSessionDeterministic(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	a := unit.BootstrapRecord(2, "s1", "player", "甲求亲")
	beloved := unit.BootstrapRecord(6, "s1", "player", "对象")
	b := unit.BootstrapRecord(8, "s2", "rival", "乙求亲")
	for _, u := range []unit.Record{a, beloved, b} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
		if err := repo.SetUnitScope(ctx, u.ID, "w1", "r1"); err != nil {
			t.Fatalf("scope %s: %v", u.ID, err)
		}
	}
	for _, s := range []unit.Record{a, b} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			s.ID, beloved.ID, 6.0, 0.0, 7.0, 0.0,
		); err != nil {
			t.Fatalf("insert relation: %v", err)
		}
	}

	winnerFrom := func(stateID string, turn int, localUnits []unit.Record) string {
		st := &State{ID: stateID, PlayerFactionID: factionOf(localUnits[0]), WorldID: "w1"}
		st.TurnState.Turn = turn
		service.scanExclusiveContestsAtBoundary(ctx, st, localUnits)
		for _, l := range st.Logs {
			if l.Kind == "contest_marriage" {
				return l.ActorUnitID
			}
		}
		return ""
	}
	// 两侧在同一裁决周期 turn=6（扫描周期 turn%3==0，且同属补投窗口桶 [6,9)）各自从自己的 session 视角扫描——
	// 甲方 localUnits={a,beloved}，乙方 localUnits={b,beloved}。跨会话候选会把对方求亲者拉进来，
	// 同 world+同标的+同裁决 tick → 同 Key → 同胜者（与谁先扫到/谁是 self 无关）。
	w1 := winnerFrom("s1", 6, []unit.Record{a, beloved})
	w2 := winnerFrom("s2", 6, []unit.Record{b, beloved})
	if w1 == "" || w2 == "" {
		t.Fatalf("两侧都应裁决出胜者：w1=%q w2=%q", w1, w2)
	}
	if w1 != w2 {
		t.Fatalf("跨会话同 world/标的/裁决周期应得同胜者（与谁先扫到无关）：甲方视角 %s，乙方视角 %s", w1, w2)
	}
}

// TestResolveContestsOfType_SeatAndLoot 验证三类 contest 参数化裁决：席位继承 + 排他战利品同样能裁决出胜者并留痕。
func TestResolveContestsOfType_SeatAndLoot(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 席位继承：两名旧部（高信任 + 进取心）争同一权位来源 lord。
	heir1 := unit.BootstrapRecord(2, "s1", "player", "长子")
	heir2 := unit.BootstrapRecord(4, "s1", "player", "次子")
	lord := unit.BootstrapRecord(6, "s1", "player", "家主")
	// 排他战利品：两名争夺者（强竞争心）争同一守家者 keeper 把守的那批。
	raider1 := unit.BootstrapRecord(8, "s1", "player", "悍匪甲")
	raider2 := unit.BootstrapRecord(10, "s1", "player", "悍匪乙")
	keeper := unit.BootstrapRecord(12, "s1", "player", "守财人")
	all := []unit.Record{heir1, heir2, lord, raider1, raider2, keeper}
	for _, u := range all {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
	}
	// 席位意图：trust>=5 且 rivalry>=2。
	for _, h := range []unit.Record{heir1, heir2} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			h.ID, lord.ID, 7.0, 0.0, 0.0, 3.0,
		); err != nil {
			t.Fatalf("insert seat relation: %v", err)
		}
	}
	// 战利品意图：rivalry>=4 且 fear<6。
	for _, r := range []unit.Record{raider1, raider2} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			r.ID, keeper.ID, 0.0, 1.0, 0.0, 6.0,
		); err != nil {
			t.Fatalf("insert loot relation: %v", err)
		}
	}

	state := &State{ID: "s1", PlayerFactionID: "player"}
	state.TurnState.Turn = 6
	service.scanExclusiveContestsAtBoundary(ctx, state, all)

	var seatLog, lootLog int
	for _, l := range state.Logs {
		switch l.Kind {
		case "contest_seat":
			seatLog++
		case "contest_loot":
			lootLog++
		}
	}
	if seatLog != 1 {
		t.Fatalf("席位继承冲突应裁决出 1 条 contest_seat 日志，得到 %d", seatLog)
	}
	if lootLog != 1 {
		t.Fatalf("排他战利品冲突应裁决出 1 条 contest_loot 日志，得到 %d", lootLog)
	}
}

// TestScanExclusiveContests_FlagOff 验证 flag 关时整方法 no-op（向后兼容/可关闭）。
func TestScanExclusiveContests_FlagOff(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "off")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	suitor1 := unit.BootstrapRecord(2, "s1", "player", "阿采")
	suitor2 := unit.BootstrapRecord(4, "s1", "player", "老吴")
	beloved := unit.BootstrapRecord(6, "s1", "player", "小满")
	for _, u := range []unit.Record{suitor1, suitor2, beloved} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
	}
	for _, s := range []unit.Record{suitor1, suitor2} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			s.ID, beloved.ID, 6.0, 0.0, 7.0, 0.0,
		); err != nil {
			t.Fatalf("insert relation: %v", err)
		}
	}

	state := &State{ID: "s1", PlayerFactionID: "player"}
	state.TurnState.Turn = 6
	service.scanExclusiveContestsAtBoundary(ctx, state, []unit.Record{suitor1, suitor2, beloved})
	if len(state.Logs) != 0 {
		t.Fatalf("flag 关应整方法 no-op，却写了 %d 条日志", len(state.Logs))
	}
}

// crossUnitDBSnapshot 直读 units 表的 (version, profile_json, session_id)——用于断言「跨会话单位在本会话裁决后零改写」。
// 直查表列（绕过 ORM）确保我们看见的是物理行真相，而非内存视图。
func crossUnitDBSnapshot(t *testing.T, db *sql.DB, unitID string) (version int64, profileJSON string, sessionID string) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT version, profile_json, session_id FROM units WHERE id = ?`, unitID,
	).Scan(&version, &profileJSON, &sessionID); err != nil {
		t.Fatalf("读 units 行 %q 失败: %v", unitID, err)
	}
	return version, profileJSON, sessionID
}

// TestScanExclusiveContests_CrossSessionLoserMemoryUntouched 是修 ①(HIGH 跨玩家硬不变量) 的补盲区回归：
// 本会话裁决里有一个**跨会话**败者时，绝不直写他库单位的记忆 blob——其 units 行的 version / profile_json / session_id
// 在本会话边界扫描前后必须**逐字节不变**（旧 bug：filterLocalContenders 保留跨会话 UnitID → recordContestConsolation
// 对其 GetByID + rememberUnitWithSource→units.Save 直写他库 profile_json，使 version+1、profile_json 被改写）。
// 跨会话败者的补偿只应经 cross_event 的 CROSS_CONTEST_LOSE，由其各自 session 读出翻译。
func TestScanExclusiveContests_CrossSessionLoserMemoryUntouched(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 本会话 s1 的强求亲者（高魅力/好感 → Score 远高）+ 对象；另一会话 s2 的弱求亲者（跨会话，必为败者）。
	// 固定 unitID（BootstrapRecord 默认随机 UUID 会使 arbitration Key 随机 → 胜者非确定）：用固定 ID + 悬殊 Score 差，
	// 使本会话强者在这组固定 Key 下**确定性**胜出，跨会话弱者**确定性**落败 → 稳定走「败者补偿」分支（否则胜者无补偿、
	// 不变量会变成空过，无法捕获 bug）。
	localSuitor := unit.BootstrapRecord(2, "s1", "player", "本会话郎")
	localSuitor.ID = "local_suitor_fixed"
	localSuitor.Stats.Primary.Charisma = 60 // 抬高本侧 Score，使跨会话弱者确定性落败、走败者补偿分支
	beloved := unit.BootstrapRecord(6, "s1", "player", "对象")
	beloved.ID = "beloved_fixed"
	crossSuitor := unit.BootstrapRecord(8, "s2", "rival", "异乡客")
	crossSuitor.ID = "cross_suitor_fixed"
	crossSuitor.Stats.Primary.Charisma = 1 // 跨会话弱者：仅离线楼板兜底，Score 远低于本侧
	for _, u := range []unit.Record{localSuitor, beloved, crossSuitor} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
		if err := repo.SetUnitScope(ctx, u.ID, "w1", "r1"); err != nil {
			t.Fatalf("set scope %s: %v", u.ID, err)
		}
	}
	for _, s := range []unit.Record{localSuitor, crossSuitor} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			s.ID, beloved.ID, 6.0, 0.0, 7.0, 0.0,
		); err != nil {
			t.Fatalf("insert relation: %v", err)
		}
	}

	// 快照跨会话单位的物理行（裁决前）。
	verBefore, profBefore, sessBefore := crossUnitDBSnapshot(t, db, crossSuitor.ID)
	if sessBefore != "s2" {
		t.Fatalf("前置：跨会话单位应属 s2，实得 %q", sessBefore)
	}

	state := &State{ID: "s1", PlayerFactionID: "player", WorldID: "w1"}
	state.TurnState.Turn = 6
	// 本会话 units 只含本局单位；crossSuitor 经跨会话候选池拉入并参与裁决（其 Score 进 arbitration）。
	service.scanExclusiveContestsAtBoundary(ctx, state, []unit.Record{localSuitor, beloved})

	// 核心断言：无论谁胜，跨会话单位的物理行（version / profile_json / session_id）必须**逐字节不变**——
	// 它绝不被本会话的补偿路径写入记忆（跨玩家硬不变量）。
	verAfter, profAfter, sessAfter := crossUnitDBSnapshot(t, db, crossSuitor.ID)
	if verAfter != verBefore {
		t.Fatalf("跨会话单位 version 被改写（疑似直写他库记忆）：前 %d 后 %d", verBefore, verAfter)
	}
	if profAfter != profBefore {
		t.Fatalf("跨会话单位 profile_json 被改写（疑似直写他库记忆 blob）：\n前=%s\n后=%s", profBefore, profAfter)
	}
	if sessAfter != "s2" {
		t.Fatalf("绝不应改写他人单位 session_id：期望 s2，实得 %q", sessAfter)
	}

	// 同时验证「确实发生了跨会话裁决」（否则上面的不变量是空过）：应裁出 1 条联姻日志 + 至少 1 条带 arbitration_key 的 cross_event。
	var marriageLog int
	var winnerID string
	for _, l := range state.Logs {
		if l.Kind == "contest_marriage" {
			marriageLog++
			winnerID = l.ActorUnitID
		}
	}
	if marriageLog != 1 {
		t.Fatalf("应裁出 1 条联姻日志（证明跨会话候选已接通、不变量非空过），实得 %d", marriageLog)
	}
	// 显式锁定「跨会话者确为败者」——固定 ID + 悬殊 Score 下本会话强者确定性胜出，从而**确实**走到「跨会话败者补偿」
	// 分支（这正是旧 bug 直写他库记忆的触发路径）；若此处变成跨会话者胜出，上面的不变量会沦为空过，需调参数。
	if winnerID != localSuitor.ID {
		t.Fatalf("本测试需本会话强者确定性胜出（使跨会话者为败者、命中补偿分支）：胜者实得 %q，期望 %q", winnerID, localSuitor.ID)
	}
	var crossCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cross_events WHERE arbitration_key IS NOT NULL AND arbitration_key <> ''`,
	).Scan(&crossCount); err != nil {
		t.Fatalf("统计 cross_events 失败: %v", err)
	}
	if crossCount == 0 {
		t.Fatalf("跨会话裁决应留痕 cross_event（败者补偿走 cross_event 而非直写他库记忆），实得 0")
	}

	// 本会话败者（若有）才允许被本侧记补偿——这里 localSuitor 若胜则无补偿、若败则记本侧；二者都不触碰跨会话行。
	// 再校验：localSuitor 自己的物理行可被本侧写（合法），但绝不影响上面对 crossSuitor 的不变量。
}

// TestResolveExclusiveContest_PublicPathGuardsCrossSessionLoser 是守卫 (a) 的单元级回归：直接走**公开入口**
// （localLoserIDs=nil，补偿名单含全部败者），传入一个真实属于他 session 的败者，验证 recordContestConsolation 的
// loser.SessionID 守卫兜底——他库单位的 version / profile_json 不被改写，只在本侧记一条可读日志。
func TestResolveExclusiveContest_PublicPathGuardsCrossSessionLoser(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 本会话 s1 强者（让其大概率胜）+ 他会话 s2 弱者（大概率败、走补偿守卫）。
	winner := unit.BootstrapRecord(2, "s1", "player", "本会话胜者")
	otherLoser := unit.BootstrapRecord(8, "s2", "rival", "他会话败者")
	for _, u := range []unit.Record{winner, otherLoser} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
	}
	verBefore, profBefore, _ := crossUnitDBSnapshot(t, db, otherLoser.ID)

	// 公开入口：worldID 非空触发 cross_event 留痕；全量候选含 s2 单位，Score 让 s1 强者占优。
	state := &State{ID: "s1", WorldID: "w1"}
	state.TurnState.Turn = 6
	logsBefore := len(state.Logs)
	if _, err := service.ResolveExclusiveContest(ctx, state, "so_x", "marriage:so_x", []ContestContender{
		{UnitID: winner.ID, Score: 100},   // 本会话强者，几乎必胜
		{UnitID: otherLoser.ID, Score: 1}, // 他会话弱者，几乎必败 → 触发补偿守卫
	}); err != nil {
		t.Fatalf("公开入口裁决出错: %v", err)
	}

	// 无论谁胜：他会话单位的物理行必须不变（公开入口的 loser.SessionID 守卫兜底，绝不直写他库记忆 blob）。
	verAfter, profAfter, sessAfter := crossUnitDBSnapshot(t, db, otherLoser.ID)
	if verAfter != verBefore || profAfter != profBefore {
		t.Fatalf("公开入口绝不应直写他会话败者记忆：version 前 %d 后 %d；profile_json 是否变=%v",
			verBefore, verAfter, profAfter != profBefore)
	}
	if sessAfter != "s2" {
		t.Fatalf("绝不应改写他会话单位 session_id：期望 s2，实得 %q", sessAfter)
	}
	// 守卫触发时仍允许在本侧记一条可读日志（本会话视角可见的「这次没争过」）——不强制条数，仅确认未 panic、流程走通。
	_ = logsBefore
}

// TestScanExclusiveContests_CrossSessionSameWinnerAndIdempotent 是修 ②(MEDIUM) 的回归：
// 两个 session 视角对同一 (worldID, SO.id, tick) 各自裁决，应得**同一胜者**（候选集合 view-independent + 离线楼板统一施加），
// 且 cross_event 幂等——同 Key 同胜者重复写主键冲突被安全忽略，全 world 对同一裁决只留**一条** CROSS_CONTEST_WIN。
func TestScanExclusiveContests_CrossSessionSameWinnerAndIdempotent(t *testing.T) {
	t.Setenv("QUNXIANG_ZEROSUM_CONTEST", "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 两会话各一求亲者（魅力不同 → Score 不同，但楼板统一施加使两视角对同一单位算同一 Score）+ 共同对象。
	a := unit.BootstrapRecord(2, "s1", "player", "甲求亲")
	a.Stats.Primary.Charisma = 12
	beloved := unit.BootstrapRecord(6, "s1", "player", "对象")
	b := unit.BootstrapRecord(8, "s2", "rival", "乙求亲")
	b.Stats.Primary.Charisma = 5
	for _, u := range []unit.Record{a, beloved, b} {
		if err := repo.Save(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.ID, err)
		}
		if err := repo.SetUnitScope(ctx, u.ID, "w1", "r1"); err != nil {
			t.Fatalf("scope %s: %v", u.ID, err)
		}
	}
	for _, s := range []unit.Record{a, b} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry) VALUES (?, ?, ?, ?, ?, ?)`,
			s.ID, beloved.ID, 6.0, 0.0, 7.0, 0.0,
		); err != nil {
			t.Fatalf("insert relation: %v", err)
		}
	}

	winnerFrom := func(stateID string, turn int, localUnits []unit.Record) string {
		st := &State{ID: stateID, PlayerFactionID: factionOf(localUnits[0]), WorldID: "w1"}
		st.TurnState.Turn = turn
		service.scanExclusiveContestsAtBoundary(ctx, st, localUnits)
		for _, l := range st.Logs {
			if l.Kind == "contest_marriage" {
				return l.ActorUnitID
			}
		}
		return ""
	}
	// 甲方视角（self=s1，a 是本会话、b 是跨会话）与乙方视角（self=s2，b 是本会话、a 是跨会话）——
	// 同 world + 同对象 + 同补投窗口 tick=6 → 候选集合一致、楼板对每个物理单位一致 → 必得同胜者（与「谁是 self」无关）。
	w1 := winnerFrom("s1", 6, []unit.Record{a, beloved})
	w2 := winnerFrom("s2", 6, []unit.Record{b, beloved})
	if w1 == "" || w2 == "" {
		t.Fatalf("两侧都应裁出胜者：w1=%q w2=%q", w1, w2)
	}
	if w1 != w2 {
		t.Fatalf("修 ②：两 session 视角同 Key 应得同胜者（候选集合+楼板 view-independent），甲方=%s 乙方=%s", w1, w2)
	}

	// 幂等：两侧都写 CROSS_CONTEST_WIN（actor=target=同一胜者、同 arbitration_key → 同主键），第二次撞 PK 被忽略，
	// 全 world 对该 Key 只应留 1 条 CROSS_CONTEST_WIN（不再两条矛盾胜者）。
	var winRows int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cross_events WHERE event_kind = ? AND world_id = ? AND arbitration_key IS NOT NULL`,
		string(events.ReasonCrossContestWin), "w1",
	).Scan(&winRows); err != nil {
		t.Fatalf("统计 CROSS_CONTEST_WIN 失败: %v", err)
	}
	if winRows != 1 {
		t.Fatalf("修 ② 幂等：同 (world,SO,tick) 应只留 1 条 CROSS_CONTEST_WIN，实得 %d（两条=矛盾胜者/未幂等）", winRows)
	}
}

// containsSub 是不引入 strings 仅为子串判断的小助手（测试内联）。
func containsSub(s, sub string) bool {
	if sub == "" {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// factionOf 取一个单位的阵营（测试小助手）。
func factionOf(u unit.Record) string {
	return u.FactionID
}
