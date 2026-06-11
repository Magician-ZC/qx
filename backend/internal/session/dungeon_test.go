package session

// 文件说明：副本(Dungeon)同步核心全链路集成测试（对真实 SQLite）：
// 多层逐层 combat_roll → 通关总分赃（含 epic 仲裁归属）/ 中途败北分级惩罚 / flag 关零行为 / 确定性可复现。
// 复用 threat_test.go 的 newThreatTestService / saveMember / eventCount / contains 辅助。

import (
	"context"
	"os"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/unit"
)

// withDungeonFlag 在测试期临时置 QUNXIANG_DUNGEON，结束自动复原（不污染其他测试的进程级环境）。
func withDungeonFlag(t *testing.T, value string) {
	t.Helper()
	orig, had := os.LookupEnv("QUNXIANG_DUNGEON")
	if err := os.Setenv("QUNXIANG_DUNGEON", value); err != nil {
		t.Fatalf("设置 flag 失败: %v", err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("QUNXIANG_DUNGEON", orig)
		} else {
			_ = os.Unsetenv("QUNXIANG_DUNGEON")
		}
	})
}

// TestRunDungeon_DisabledZeroBehavior 验证 flag 默认关时入口返回 ErrDungeonDisabled、零行为（不读单位、不改任何状态）。
func TestRunDungeon_DisabledZeroBehavior(t *testing.T) {
	withDungeonFlag(t, "false") // 显式关
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(1, "s1", "player", "她")
	actor.Status.Attack = 30
	actor.Status.HP = 100
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存角色失败: %v", err)
	}
	before := eventCount(t, db)

	state := State{ID: "s1"}
	res, err := service.RunDungeon(ctx, &state, []string{actor.ID}, 3)
	if err != ErrDungeonDisabled {
		t.Fatalf("flag 关时应返回 ErrDungeonDisabled，得到 err=%v res=%+v", err, res)
	}
	if len(res.FloorResults) != 0 || len(res.Awards) != 0 {
		t.Fatalf("flag 关时应零行为，得到 floors=%d awards=%d", len(res.FloorResults), len(res.Awards))
	}
	if eventCount(t, db) != before {
		t.Fatalf("flag 关时不应产生任何事件留痕")
	}
	if len(state.Logs) != 0 {
		t.Fatalf("flag 关时不应写日志")
	}
}

// TestRunDungeon_ClearAllocatesLoot 验证强队通关全部层、总分赃（epic 唯一遗物恰一名得主、common 瓜分完），落库+留痕+收件箱卡。
func TestRunDungeon_ClearAllocatesLoot(t *testing.T) {
	withDungeonFlag(t, "true")
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 三人强队，逐层应能稳定推平。
	striker := saveMember(t, ctx, repo, 11, "主攻", 60, 12, 100)
	tank := saveMember(t, ctx, repo, 12, "承伤", 40, 10, 100)
	support := saveMember(t, ctx, repo, 13, "辅助", 35, 10, 100)

	state := State{ID: "s1"}
	res, err := service.RunDungeon(ctx, &state, []string{striker.ID, tank.ID, support.ID}, 3)
	if err != nil {
		t.Fatalf("副本出错: %v", err)
	}
	if res.Outcome != "cleared" {
		t.Fatalf("强队应通关，得到 outcome=%q clear=%d floors=%v", res.Outcome, res.FloorsClear, res.FloorResults)
	}
	if res.FloorsClear != 3 {
		t.Fatalf("应通关全部 3 层，得到 %d", res.FloorsClear)
	}
	if res.FloorResults[2].IsBoss != true {
		t.Fatalf("末层应是 boss")
	}

	// epic 唯一排他遗物（末层 boss 掉落）恰有且仅有一名 won 得主。
	relicWinners := 0
	totalGold := 0
	for _, a := range res.Awards {
		if a.ItemID == "dungeon_relic" && a.Reason == encounter.AwardWon {
			relicWinners++
		}
		if a.ItemID == "gold" {
			totalGold += a.Quantity
		}
	}
	if relicWinners != 1 {
		t.Fatalf("唯一副本遗物应恰有 1 名得主，得到 %d", relicWinners)
	}
	if totalGold <= 0 {
		t.Fatalf("通关应分得金币，得到 %d", totalGold)
	}

	// 落库：分到金币的人钱包增加。
	gotWallet := false
	for _, id := range []string{striker.ID, tank.ID, support.ID} {
		reloaded, _ := repo.GetByID(ctx, id)
		if reloaded.Status.Wallet > 0 {
			gotWallet = true
		}
	}
	if !gotWallet {
		t.Fatalf("通关后至少一人钱包应增加")
	}
	if eventCount(t, db) == 0 {
		t.Fatalf("应有战斗/分赃留痕")
	}
	// 每人一张含层数信息的祖魂语气命运卡。
	for _, id := range []string{striker.ID, tank.ID, support.ID} {
		card := res.InboxCards[id]
		if card == "" || !contains(card, "副本") {
			t.Fatalf("每人应有含「副本」的命运卡，%s 得到 %q", id, card)
		}
	}
	if len(state.Logs) == 0 {
		t.Fatalf("应写入遭遇日志")
	}
}

// TestRunDungeon_WipeAppliesPenalty 验证弱队中途败北（wiped）：不分赃、各自经分级闸落败北惩罚、绝不阵亡（D0-D3 硬锁）。
func TestRunDungeon_WipeAppliesPenalty(t *testing.T) {
	withDungeonFlag(t, "1")
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	weak1 := saveMember(t, ctx, repo, 21, "新兵甲", 2, 4, 100)
	weak2 := saveMember(t, ctx, repo, 22, "新兵乙", 2, 4, 100)

	state := State{ID: "s1"}
	res, err := service.RunDungeon(ctx, &state, []string{weak1.ID, weak2.ID}, 5)
	if err != nil {
		t.Fatalf("副本出错: %v", err)
	}
	if res.Outcome == "cleared" {
		t.Fatalf("弱队不应通关 5 层")
	}
	// 中途败北：不应分得排他遗物。
	for _, a := range res.Awards {
		if a.ItemID == "dungeon_relic" && a.Reason == encounter.AwardWon {
			t.Fatalf("败北不应分得 boss 遗物")
		}
	}
	// 各自落分级惩罚，且经分级闸硬锁——新角色最坏只到「可恢复」层1，绝不阵亡。
	for _, id := range []string{weak1.ID, weak2.ID} {
		if res.Outcome == "wiped" {
			if layer := res.PenaltyLayer[id]; layer != 1 {
				t.Fatalf("新角色败北应落分级层1，%s 得到 %d", id, layer)
			}
		}
		reloaded, _ := repo.GetByID(ctx, id)
		if reloaded.Status.LivesRemaining <= 0 {
			t.Fatalf("副本失败绝不应让新角色阵亡（D0-D3 保护），%s lives=%d", id, reloaded.Status.LivesRemaining)
		}
	}
}

// TestRunDungeon_Deterministic 验证同会话同回合同参数两次运行结果完全一致（确定性可复现）。
func TestRunDungeon_Deterministic(t *testing.T) {
	withDungeonFlag(t, "on")
	ctx := context.Background()

	run := func() DungeonResult {
		_, repo, service := newThreatTestService(t)
		a := saveMember(t, ctx, repo, 31, "甲", 18, 6, 100)
		b := saveMember(t, ctx, repo, 32, "乙", 16, 6, 100)
		// 固定 unit ID 使两次运行输入完全一致——combat_roll 把 actor.ID 写进确定性 FNV，
		// 而 BootstrapRecord 的 ID 是随机 uuid（每次 saveMember 不同），不锁 ID 则两次掷骰天然不同（是测试 artifact 非算法不确定）。
		a.ID, b.ID = "u_det_a", "u_det_b"
		if err := repo.Save(ctx, *a); err != nil {
			t.Fatalf("固定ID保存甲失败: %v", err)
		}
		if err := repo.Save(ctx, *b); err != nil {
			t.Fatalf("固定ID保存乙失败: %v", err)
		}
		state := State{ID: "s_det"}
		res, err := service.RunDungeon(ctx, &state, []string{a.ID, b.ID}, 4)
		if err != nil {
			t.Fatalf("副本出错: %v", err)
		}
		return res
	}

	r1 := run()
	r2 := run()

	if r1.Outcome != r2.Outcome {
		t.Fatalf("确定性失效：outcome %q vs %q", r1.Outcome, r2.Outcome)
	}
	if r1.FloorsClear != r2.FloorsClear {
		t.Fatalf("确定性失效：通关层数 %d vs %d", r1.FloorsClear, r2.FloorsClear)
	}
	if len(r1.FloorResults) != len(r2.FloorResults) {
		t.Fatalf("确定性失效：层数 %d vs %d", len(r1.FloorResults), len(r2.FloorResults))
	}
	for i := range r1.FloorResults {
		f1, f2 := r1.FloorResults[i], r2.FloorResults[i]
		if f1.Outcome != f2.Outcome || f1.Rounds != f2.Rounds || f1.DamageDealt != f2.DamageDealt || f1.DamageTaken != f2.DamageTaken {
			t.Fatalf("确定性失效：第%d层 %+v vs %+v", i+1, f1, f2)
		}
	}
}
