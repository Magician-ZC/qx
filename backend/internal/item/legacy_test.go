package item

// 文件说明：耐久衰减（GEAR_DAMAGED）与传家物升级（Legacy）纯原语的测试。
// 覆盖：DegradeDurability 边界（不破坏 pinned / 不归零硬锁 / 无限耐久 no-op / 确定性）、
// LegacyUpgrade 设 IsLegacy+SoulBound（+Pinned）、IsPermanentAnchor、DefeatDurabilityLoss 确定性。

import "testing"

// TestDegradeDurability_PinnedNeverDamaged pinned 传家宝绝不被耐久衰减。
func TestDegradeDurability_PinnedNeverDamaged(t *testing.T) {
	r := DegradeDurability(50, 10, true)
	if r.Changed || r.Durability != 50 {
		t.Fatalf("pinned 物不应衰减：%+v", r)
	}
}

// TestDegradeDurability_Floor 不归零硬锁——衰减最多降到 1，永不破坏。
func TestDegradeDurability_Floor(t *testing.T) {
	r := DegradeDurability(3, 100, false)
	if r.Durability != DurabilityFloor {
		t.Fatalf("超量衰减应锁在下限 %d，得到 %d", DurabilityFloor, r.Durability)
	}
	if r.Durability != 1 {
		t.Fatalf("硬锁下限应为 1（不归零=不破坏）")
	}
	if !r.Floored {
		t.Fatalf("触及下限应标 Floored")
	}
	if !r.Changed {
		t.Fatalf("从 3 降到 1 应记为 Changed")
	}
	// 已在下限再衰减 → 不再变化（幂等、不破坏）。
	r2 := DegradeDurability(1, 5, false)
	if r2.Changed || r2.Durability != 1 {
		t.Fatalf("已在下限不应再衰减：%+v", r2)
	}
}

// TestDegradeDurability_Infinite 无限耐久（current<=0）恒不衰减。
func TestDegradeDurability_Infinite(t *testing.T) {
	if r := DegradeDurability(0, 9, false); r.Changed || r.Durability != 0 {
		t.Fatalf("无限耐久不应衰减：%+v", r)
	}
}

// TestDegradeDurability_NonPositiveAmount 非正扣减 no-op。
func TestDegradeDurability_NonPositiveAmount(t *testing.T) {
	if r := DegradeDurability(40, 0, false); r.Changed {
		t.Fatalf("0 扣减不应变化")
	}
	if r := DegradeDurability(40, -5, false); r.Changed {
		t.Fatalf("负扣减不应变化")
	}
}

// TestDegradeDurability_Normal 正常衰减。
func TestDegradeDurability_Normal(t *testing.T) {
	r := DegradeDurability(40, 7, false)
	if r.Durability != 33 || !r.Changed || r.Floored {
		t.Fatalf("正常衰减应为 33/Changed/未触底：%+v", r)
	}
}

// TestDegradeDurability_Deterministic 同输入同输出（确定性、可复算）。
func TestDegradeDurability_Deterministic(t *testing.T) {
	a := DegradeDurability(40, 7, false)
	b := DegradeDurability(40, 7, false)
	if a != b {
		t.Fatalf("耐久衰减应确定性：%+v vs %+v", a, b)
	}
}

// TestDefeatDurabilityLoss 失败/苦战折损量确定性派生（非随机、非钱包）。
func TestDefeatDurabilityLoss(t *testing.T) {
	if DefeatDurabilityLoss(false, 0) != 0 {
		t.Fatalf("未参战不折损")
	}
	// 胜利 3 回合 → 3 点磨损。
	if got := DefeatDurabilityLoss(false, 3); got != 3 {
		t.Fatalf("胜利 3 回合应折 3，得到 %d", got)
	}
	// 落败比胜利折损更重（额外折损）。
	if DefeatDurabilityLoss(true, 3) <= DefeatDurabilityLoss(false, 3) {
		t.Fatalf("落败折损应重于胜利")
	}
	// 回合磨损封顶（防一场仗打废）。
	if got := DefeatDurabilityLoss(false, 999); got != maxDefeatLoss {
		t.Fatalf("回合磨损应封顶 %d，得到 %d", maxDefeatLoss, got)
	}
}

// TestLegacyUpgrade 传家物升级设 IsLegacy + SoulBound（+ Pinned）。
func TestLegacyUpgrade(t *testing.T) {
	out := LegacyUpgrade(LegacyFlags{})
	if !out.IsLegacy {
		t.Fatalf("升级后应 IsLegacy=true")
	}
	if !out.SoulBound {
		t.Fatalf("升级后应 SoulBound=true")
	}
	if !out.Pinned {
		t.Fatalf("升级后应 Pinned=true（喂 sell_pinned 硬门）")
	}
	// 幂等：已升级再升级仍为永久锚。
	again := LegacyUpgrade(out)
	if again != out {
		t.Fatalf("LegacyUpgrade 应幂等：%+v vs %+v", again, out)
	}
}

// TestIsPermanentAnchor 三重标记齐备即永久锚（不可变卖）。
func TestIsPermanentAnchor(t *testing.T) {
	if !IsPermanentAnchor(LegacyUpgrade(LegacyFlags{})) {
		t.Fatalf("升级后应构成永久锚")
	}
	if IsPermanentAnchor(LegacyFlags{IsLegacy: true, SoulBound: true}) {
		t.Fatalf("缺 Pinned 不算永久锚")
	}
	if IsPermanentAnchor(LegacyFlags{}) {
		t.Fatalf("空标记不是永久锚")
	}
}
