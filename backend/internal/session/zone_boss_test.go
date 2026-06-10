package session

import (
	"strings"
	"testing"
)

// TestZoneCreatureLevelDeterministicInBand 校验阶段2 §2：怪物等级确定性派生且恒落在区域等级带内。
func TestZoneCreatureLevelDeterministicInBand(t *testing.T) {
	const min, max = 5, 15
	for _, id := range []string{"npc_a", "npc_b", "npc_c", "npc_d", "npc_e"} {
		got := zoneCreatureLevel("sess1", "zone_freedom_capital", id, min, max)
		if got < min || got > max {
			t.Errorf("等级 %d 越出区域带 [%d,%d] (unit=%s)", got, min, max, id)
		}
		// 确定性：同输入恒同值。
		if again := zoneCreatureLevel("sess1", "zone_freedom_capital", id, min, max); again != got {
			t.Errorf("同输入等级应一致：%d vs %d (unit=%s)", got, again, id)
		}
	}
	// 不同会话/不同区/不同 unit 应能区分（至少不全部相同——确保 salt 进了哈希）。
	a := zoneCreatureLevel("sessX", "zoneX", "u", 1, 30)
	b := zoneCreatureLevel("sessY", "zoneX", "u", 1, 30)
	cc := zoneCreatureLevel("sessX", "zoneY", "u", 1, 30)
	if a == b && a == cc {
		t.Error("不同 session/zone 的等级派生不应恒等（salt 未生效？）")
	}
}

// TestZoneCreatureLevelEdgeBands 校验等级带边界容错：单点带、非法 min、min>max。
func TestZoneCreatureLevelEdgeBands(t *testing.T) {
	if got := zoneCreatureLevel("s", "z", "u", 7, 7); got != 7 {
		t.Errorf("单点带应恒返回该值，得到 %d", got)
	}
	if got := zoneCreatureLevel("s", "z", "u", 0, 0); got != 1 {
		t.Errorf("min<1 应夹到 1，得到 %d", got)
	}
	if got := zoneCreatureLevel("s", "z", "u", 10, 3); got != 10 {
		t.Errorf("min>max 应取 min，得到 %d", got)
	}
}

// TestZoneDungeonFloorsClamp 校验副本层数派生 clamp 到 [1,5]。
func TestZoneDungeonFloorsClamp(t *testing.T) {
	cases := []struct{ levelMax, want int }{
		{5, 1},  // 5/5=1
		{15, 3}, // 15/5=3
		{25, 5}, // 25/5=5
		{30, 5}, // 30/5=6 → clamp 5
		{2, 1},  // 2/5=0 → clamp 1
		{0, 1},  // 0/5=0 → clamp 1
		{-3, 1}, // 负数 → clamp 1
	}
	for _, c := range cases {
		if got := zoneDungeonFloors(c.levelMax); got != c.want {
			t.Errorf("zoneDungeonFloors(%d)=%d，应为 %d", c.levelMax, got, c.want)
		}
	}
}

// TestZoneBossThreatScalesWithLevel 校验阶段2 §3：boss Threat 强度随等级单调放大、名字/RegionID/Tier 正确。
func TestZoneBossThreatScalesWithLevel(t *testing.T) {
	low := zoneBossThreat("s1", "zone_freedom_capital", "赤鬣兽王", 5)
	high := zoneBossThreat("s1", "zone_freedom_wild", "噬骨魔狼", 25)
	if high.HPPool <= low.HPPool {
		t.Errorf("高等级 boss HP 应更高：low=%d high=%d", low.HPPool, high.HPPool)
	}
	if high.Attack <= low.Attack {
		t.Errorf("高等级 boss 攻击应更高：low=%d high=%d", low.Attack, high.Attack)
	}
	if low.Name != "赤鬣兽王" || low.RegionID != "zone_freedom_capital" {
		t.Errorf("boss 名/RegionID 应透传：%+v", low)
	}
	if low.Tier != ThreatTierFieldBoss {
		t.Errorf("区域 boss Tier 应为 field_boss，得到 %s", low.Tier)
	}
	// 稳定身份：同会话同区 ID 一致（掷骰可复现）。
	if again := zoneBossThreat("s1", "zone_freedom_capital", "赤鬣兽王", 5); again.ID != low.ID {
		t.Errorf("同会话同区 boss ID 应稳定：%s vs %s", low.ID, again.ID)
	}
	// 必带排他遗物（走 arbitration 归属）。
	hasRelic := false
	for _, l := range low.Loot {
		if l.ID == "zone_boss_relic" {
			hasRelic = true
		}
	}
	if !hasRelic {
		t.Error("区域 boss 应掉落排他遗物 zone_boss_relic")
	}
	// 空 bossName 容错回退。
	if zoneBossThreat("s", "z", "", 1).Name == "" {
		t.Error("空 bossName 应回退到默认名")
	}
}

// TestZoneBossPerilGuard 校验阶段2 §3「等级护栏」软门：boss 高出主角 ≥5 级才判凶险，且提示带「此地凶险」+ 两个等级。
func TestZoneBossPerilGuard(t *testing.T) {
	cases := []struct {
		name                   string
		bossLevel, playerLevel int
		wantPerilous           bool
	}{
		{"差4级不拦", 9, 5, false},   // gap=4 < 5
		{"差5级拦", 10, 5, true},    // gap=5 == 阈值
		{"差悬殊拦", 25, 1, true},    // gap=24
		{"主角更高不拦", 5, 20, false}, // gap=-15
		{"持平不拦", 10, 10, false},  // gap=0
	}
	for _, c := range cases {
		msg, perilous := zoneBossPerilGuard("赤鬣兽王", c.bossLevel, c.playerLevel)
		if perilous != c.wantPerilous {
			t.Errorf("%s：zoneBossPerilGuard(boss=%d,player=%d) perilous=%v，应为 %v",
				c.name, c.bossLevel, c.playerLevel, perilous, c.wantPerilous)
		}
		if perilous {
			if !strings.Contains(msg, "此地凶险") {
				t.Errorf("%s：凶险提示应含「此地凶险」，得到 %q", c.name, msg)
			}
			if !strings.Contains(msg, "Lv") {
				t.Errorf("%s：凶险提示应含等级标注，得到 %q", c.name, msg)
			}
		} else if msg != "" {
			t.Errorf("%s：未凶险时消息应为空，得到 %q", c.name, msg)
		}
	}
	// 空 bossName 容错：仍出凶险提示且带默认名（不空字符串）。
	if msg, ok := zoneBossPerilGuard("", 30, 1); !ok || !strings.Contains(msg, "此地凶险") {
		t.Errorf("空 bossName 应仍判凶险并带提示，得到 ok=%v msg=%q", ok, msg)
	}
}

// TestSeededAndDefeatedSets 校验已播种区 / 已讨平 boss 集合的幂等增删判。
func TestSeededAndDefeatedSets(t *testing.T) {
	state := &State{}
	if zoneSeeded(state, "z1") {
		t.Error("空集合不应判已播种")
	}
	appendSeededZone(state, "z1")
	appendSeededZone(state, "z1") // 幂等
	appendSeededZone(state, "z2")
	if !zoneSeeded(state, "z1") || !zoneSeeded(state, "z2") {
		t.Error("append 后应判已播种")
	}
	if len(state.SeededZoneIDs) != 2 {
		t.Errorf("幂等 append 不应重复，得到 %d", len(state.SeededZoneIDs))
	}

	if zoneBossDefeated(state, "z1") {
		t.Error("空集合不应判已讨平")
	}
	appendDefeatedBoss(state, "z1")
	appendDefeatedBoss(state, "z1") // 幂等
	if !zoneBossDefeated(state, "z1") {
		t.Error("append 后应判已讨平")
	}
	if len(state.DefeatedBosses) != 1 {
		t.Errorf("幂等 append 不应重复，得到 %d", len(state.DefeatedBosses))
	}
}
