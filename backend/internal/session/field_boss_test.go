package session

// 文件说明：组队野外Boss遭遇全链路集成测试（对真实 SQLite）：
// 多人多回合 combat_roll → 胜利按贡献分赃（含 epic 仲裁归属、common 比例瓜分）/ 失败各自分级惩罚 → 留痕 + 每人收件箱卡。

import (
	"context"
	"testing"

	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/unit"
)

func saveMember(t *testing.T, ctx context.Context, repo *unit.Repository, seed int64, name string, atk, def, hp int) *unit.Record {
	t.Helper()
	rec := unit.BootstrapRecord(seed, "s1", "player", name)
	rec.Status.Attack = atk
	rec.Status.Defense = def
	rec.Status.HP = hp
	rec.Status.Wallet = 0
	rec.Status.Morale = 0.7
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存队员失败: %v", err)
	}
	return &rec
}

func TestResolveFieldBoss_VictoryAllocatesLoot(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	// 三人队伍，战力悬殊：主攻手贡献最大，应在 epic 排他件上占优、common 上分得最多。
	striker := saveMember(t, ctx, repo, 11, "主攻", 25, 8, 100)
	tank := saveMember(t, ctx, repo, 12, "承伤", 8, 3, 100)
	support := saveMember(t, ctx, repo, 13, "辅助", 6, 6, 100)
	party := []*unit.Record{striker, tank, support}

	threat := Threat{
		ID: "fb_lizard", Name: "蛮荒巨蜥", Tier: ThreatTierFieldBoss, RegionID: "r1",
		Power: 300, Attack: 6, Defense: 3, HPPool: 120, Severity: 65,
		Loot: []encounter.LootItem{
			{ID: "gold", Rarity: encounter.Common, Quantity: 90},
			{ID: "boss_relic", Rarity: encounter.Epic, Quantity: 1},
		},
	}
	state := State{ID: "s1"}

	res, err := service.ResolveFieldBoss(ctx, &state, party, threat)
	if err != nil {
		t.Fatalf("组队遭遇出错: %v", err)
	}
	if !res.Victory {
		t.Fatalf("强队应讨平弱Boss，得到 rounds=%d members=%d", res.Rounds, len(res.Members))
	}
	if len(res.Members) != 3 {
		t.Fatalf("应有 3 名队员结算，得到 %d", len(res.Members))
	}

	// epic 唯一排他件必有且仅有一名 won 得主；其余为 consolation。
	relicWinners, relicConsolation := 0, 0
	totalGold := 0
	for _, m := range res.Members {
		for _, a := range m.Awards {
			if a.ItemID == "boss_relic" {
				if a.Reason == encounter.AwardWon {
					relicWinners++
				} else if a.Reason == encounter.AwardConsolation {
					relicConsolation++
				}
			}
			if a.ItemID == "gold" {
				totalGold += a.Quantity
			}
		}
	}
	if relicWinners != 1 {
		t.Fatalf("唯一遗物应恰有 1 名得主，得到 %d", relicWinners)
	}
	if totalGold != 90 {
		t.Fatalf("可分割金币应恰好瓜分完 90，得到 %d", totalGold)
	}

	// 落库：得到金币的人钱包应增加。
	reloaded, _ := repo.GetByID(ctx, striker.ID)
	if reloaded.Status.Wallet <= 0 {
		t.Fatalf("主攻手应分得金币，钱包=%d", reloaded.Status.Wallet)
	}
	if eventCount(t, db) == 0 {
		t.Fatalf("应有战斗/分赃留痕")
	}
	for _, m := range res.Members {
		if m.InboxCard == "" || !contains(m.InboxCard, "蛮荒巨蜥") {
			t.Fatalf("每人应有含Boss名的祖魂语气命运卡：%q", m.InboxCard)
		}
	}
}

func TestResolveFieldBoss_DefeatPenaltiesPerMember(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	weak1 := saveMember(t, ctx, repo, 21, "新兵甲", 2, 4, 100)
	weak2 := saveMember(t, ctx, repo, 22, "新兵乙", 2, 4, 100)
	party := []*unit.Record{weak1, weak2}

	threat := Threat{
		ID: "fb_titan", Name: "上古泰坦", Tier: ThreatTierFieldBoss, RegionID: "r1",
		Power: 2000, Attack: 60, Defense: 20, HPPool: 2000, Severity: 95,
	}
	state := State{ID: "s1"}

	res, err := service.ResolveFieldBoss(ctx, &state, party, threat)
	if err != nil {
		t.Fatalf("组队遭遇出错: %v", err)
	}
	if res.Victory {
		t.Fatalf("弱队不应讨平强Boss")
	}
	// 野外Boss失败候选层=2，但新角色经分级闸硬锁，最坏只到「可恢复」层1，绝不阵亡（D0-D3 保护）。
	for _, m := range res.Members {
		if m.PenaltyLayer != 1 {
			t.Fatalf("新角色失败应落分级层1，队员 %s 得到 %d", m.UnitID, m.PenaltyLayer)
		}
		reloaded, _ := repo.GetByID(ctx, m.UnitID)
		if reloaded.Status.LivesRemaining <= 0 {
			t.Fatalf("失败绝不应让新角色阵亡，%s lives=%d", m.UnitID, reloaded.Status.LivesRemaining)
		}
	}
}

func TestPickLowestHPActive(t *testing.T) {
	a := &memberCombatState{rec: &unit.Record{ID: "a"}, status: "contributed"}
	a.rec.Status.HP = 80
	b := &memberCombatState{rec: &unit.Record{ID: "b"}, status: "contributed"}
	b.rec.Status.HP = 30
	c := &memberCombatState{rec: &unit.Record{ID: "c"}, status: "down"}
	c.rec.Status.HP = 5 // 已倒下，不应被选中
	got := pickLowestHPActive([]*memberCombatState{a, b, c})
	if got == nil || got.rec.ID != "b" {
		t.Fatalf("应选活跃中血量最低的 b，得到 %v", got)
	}
}
