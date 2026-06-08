package session

// 文件说明：跨玩家零和裁决（contest.go，设计 §2.6）的测试——验证四条不变量：
// ① arbitration 胜率∝Score（强者更可能但非必然胜）；② 确定性可复现（同 Key 同结果）；
// ③ 无冲突 no-op（每对象至多一个求亲者时不裁决）；④ 付费不影响（Score 不含钱包/付费维度）。

import (
	"context"
	"testing"

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
	// 同 state（同 sessionID+turn）、同 resource、同争夺者集合 → 必同胜者，重复多次稳定。
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

// TestResolveExclusiveContest_DifferentKeyMayDiffer 验证不同 Key（不同回合/标的）会重抽，结果可不同——
// 间接证明 Key 真的进了裁决（否则所有裁决恒等）。不强断言「必不同」（小概率相同），只断言「同 Key 恒同」。
func TestResolveExclusiveContest_KeyEntersResolution(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	contenders := []ContestContender{
		{UnitID: "u_a", Score: 5}, {UnitID: "u_b", Score: 5},
		{UnitID: "u_c", Score: 5}, {UnitID: "u_d", Score: 5},
	}
	// 跨多个回合收集胜者：若 Key 未进裁决，胜者会恒定；进了则随 turn 变化而重抽。
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

// TestResolveExclusiveContest_WinRateProportionalToScore 验证胜率∝Score：强者更常胜但非必然胜。
// 用大量不同 Key（不同回合）做经验分布：高 Score 一方应显著多胜，但弱者也应偶有胜出（非确定碾压）。
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
	// 非必然胜：弱者也应偶有胜出（否则退化为「强者确定碾压」，违反胜率∝Score 的概率语义）。
	if weakWins == 0 {
		t.Fatalf("弱者应偶有胜出（胜率∝Score 是概率而非确定），实得 0 次")
	}
}

// TestMarriageContenderScore_PayToWinExcluded 验证 Score **不含付费维度**：钱包翻天也不改 Score。
// 两个角色除钱包外完全相同 → marriageContenderScore 必相等（付费买不到更高 Score → 买不到「保证赢」）。
func TestMarriageContenderScore_PayToWinExcluded(t *testing.T) {
	row := relationPromptRow{Trust: 6, Affection: 7, Fear: 0, Rivalry: 0}

	poor := unit.BootstrapRecord(1, "s1", "player", "穷书生")
	poor.Stats.Primary.Charisma = 10
	poor.Status.Morale = 0.6 // Morale 量纲 [0,1]
	poor.Status.Wallet = 0

	whale := poor                    // 复制：除钱包外完全一致
	whale.Status.Wallet = 1000000000 // 巨额付费

	if scorePoor, scoreWhale := marriageContenderScore(poor, row), marriageContenderScore(whale, row); scorePoor != scoreWhale {
		t.Fatalf("付费不应改变争夺 Score（反 P2W）：穷=%.4f 富=%.4f", scorePoor, scoreWhale)
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
