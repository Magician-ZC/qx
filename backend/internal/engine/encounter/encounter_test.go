package encounter

// 文件说明：威胁结算原语测试——贡献评分、战利品确定性分配(arbitration)、比例瓜分、后果分级闸、单人解锁。

import "testing"

func TestContributionScore(t *testing.T) {
	got := ContributionScore(Contribution{Damage: 100, Tank: 50, Role: 10, Risk: 20, Clutch: 5})
	want := 1.0*100 + 0.8*50 + 0.6*10 + 0.5*20 + 1.2*5
	if got != want {
		t.Fatalf("贡献分错误：得到 %f 期望 %f", got, want)
	}
}

func TestAllocateLoot_EpicDeterministicAndMeaningful(t *testing.T) {
	parts := []Participant{
		{UnitID: "a", Score: 50},
		{UnitID: "b", Score: 30},
		{UnitID: "shirker", Score: 1}, // 蹭场者，低于 minMeaningful
	}
	items := []LootItem{{ID: "blade", Rarity: Epic, Quantity: 1}}
	a1 := AllocateLoot("boss12", items, parts, 5.0)
	a2 := AllocateLoot("boss12", items, parts, 5.0)

	// 确定性：同输入同结果。
	if len(a1) != len(a2) {
		t.Fatalf("分配应确定")
	}
	var winner string
	consolation := 0
	for _, aw := range a1 {
		switch aw.Reason {
		case AwardWon:
			winner = aw.UnitID
		case AwardConsolation:
			consolation++
			if aw.UnitID == "shirker" {
				t.Fatalf("蹭场者不应进排名(连补偿也不应有)")
			}
		}
	}
	if a1[0].UnitID != a2[0].UnitID {
		t.Fatalf("胜者应确定")
	}
	if winner != "a" && winner != "b" {
		t.Fatalf("胜者应为有效参与者之一，得到 %q", winner)
	}
	if consolation != 1 { // 两个有效参与者，一个胜、一个补偿
		t.Fatalf("应恰有 1 份败者补偿，得到 %d", consolation)
	}
}

func TestAllocateLoot_PayBlindFrequencyInvariant(t *testing.T) {
	base := []Participant{{UnitID: "a", Score: 50}, {UnitID: "b", Score: 30}}
	// 模拟付费方 b 高频/重复入队——不应改变胜者。
	spam := []Participant{{UnitID: "a", Score: 50}}
	for i := 0; i < 8; i++ {
		spam = append(spam, Participant{UnitID: "b", Score: 30})
	}
	items := []LootItem{{ID: "blade", Rarity: Epic, Quantity: 1}}
	w1 := winnerOf(AllocateLoot("k", items, base, 0))
	w2 := winnerOf(AllocateLoot("k", items, spam, 0))
	if w1 != w2 {
		t.Fatalf("重复入队(高频)不应改变胜者：%q vs %q", w1, w2)
	}
}

func TestAllocateLoot_RareTopNAndCommonSplit(t *testing.T) {
	parts := []Participant{
		{UnitID: "a", Score: 60},
		{UnitID: "b", Score: 30},
		{UnitID: "c", Score: 10},
	}
	items := []LootItem{
		{ID: "ring", Rarity: Rare, Quantity: 2},
		{ID: "ore", Rarity: Common, Quantity: 10},
	}
	awards := AllocateLoot("k", items, parts, 0)
	won, shareTotal := 0, 0
	for _, aw := range awards {
		if aw.ItemID == "ring" && aw.Reason == AwardWon {
			won++
		}
		if aw.ItemID == "ore" && aw.Reason == AwardShare {
			shareTotal += aw.Quantity
		}
	}
	if won != 2 {
		t.Fatalf("稀有2件应有2个胜者，得到 %d", won)
	}
	if shareTotal != 10 {
		t.Fatalf("可分材料应恰好分完 10，得到 %d", shareTotal)
	}
}

func TestSplitProportional(t *testing.T) {
	// 7:3 → 7,3
	s := SplitProportional(10, []Participant{{UnitID: "a", Score: 7}, {UnitID: "b", Score: 3}})
	if s["a"] != 7 || s["b"] != 3 {
		t.Fatalf("比例瓜分错误：%+v", s)
	}
	// 等分余数按名次：3 人各 1 分，total 10 → floor 3/3/3=9，余 1 给字典序最前。
	s2 := SplitProportional(10, []Participant{{UnitID: "c", Score: 1}, {UnitID: "a", Score: 1}, {UnitID: "b", Score: 1}})
	if s2["a"]+s2["b"]+s2["c"] != 10 {
		t.Fatalf("应分完 10，得到 %+v", s2)
	}
	if s2["a"] != 4 {
		t.Fatalf("余数应给字典序最前(a)，得到 %+v", s2)
	}
	// 全零分 → 尽量均分。
	s3 := SplitProportional(5, []Participant{{UnitID: "a"}, {UnitID: "b"}})
	if s3["a"]+s3["b"] != 5 || s3["a"] != 3 {
		t.Fatalf("全零分应均分(3/2)，得到 %+v", s3)
	}
}

func TestPenaltyGate(t *testing.T) {
	// D0 新角色：最坏只到层1。
	if PenaltyCap(10, 1) != 1 {
		t.Fatalf("新角色应锁在层1")
	}
	if DegradePenalty(3, 10, 1) != 1 {
		t.Fatalf("新角色的不可逆惩罚应降级到层1")
	}
	// 中期：层2。
	if PenaltyCap(50, 4) != 2 || PenaltyCap(10, 3) != 2 {
		t.Fatalf("care≥40 或 在世≥3 应到层2")
	}
	if DegradePenalty(3, 50, 4) != 2 {
		t.Fatalf("层2上限角色的不可逆应降到层2")
	}
	// 老角色高牵挂：层3解锁。
	if PenaltyCap(80, 8) != 3 {
		t.Fatalf("care≥70 且 在世≥7 应解锁层3")
	}
	if DegradePenalty(1, 80, 8) != 1 {
		t.Fatalf("候选层低于上限时不应被抬高")
	}
}

func TestSoloAllowed(t *testing.T) {
	if !SoloAllowed(40, 50) {
		t.Fatalf("低威胁应可单人")
	}
	if SoloAllowed(80, 50) {
		t.Fatalf("超过 soloCap 的世界Boss应禁止单人")
	}
}

func winnerOf(awards []Award) string {
	for _, a := range awards {
		if a.Reason == AwardWon {
			return a.UnitID
		}
	}
	return ""
}
