package session

// 文件说明：ATB 行动条排序的确定性基线测试（GDD §7「先写的5个测试」之④）。
// 覆盖 buildExecutionOrderByATB / hasMomentumPenalty：同输入同序、同阵营连续抢占第3次起 ×0.85
// 势头惩罚生效、速度 clamp[2.4,22]、跨 tick gauge 累积排序与预期序一致。
// 仅新增测试，零生产改动；用 atbDet_ 前缀避免与既有 helper 撞名。

import (
	"math"
	"reflect"
	"testing"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
)

// atbDetUnit 构造一个速度可精确预测的单位记录。
// 把人格清零（personalitySpeedBonus=0）、HP=50%且 Morale=0.5（statusSpeedBonus=0）、无装备、无减益，
// 使得 atbSpeedForUnit 的可变项仅剩 Base=6+Move 与 TaskBias（同优先级下对所有单位为同一常量）。
func atbDetUnit(id, factionID string, move int) unit.Record {
	record := unit.Record{
		ID:        id,
		SessionID: "atbDet",
		FactionID: factionID,
	}
	record.Identity.Name = id
	record.Status.LifeState = unit.LifeStateActive
	record.Status.HP = 50
	record.Status.Move = move
	record.Status.Morale = 0.5
	record.Status.Hunger = 100
	record.Stats.Primary.Constitution = 10 // maxHP=100，HP=50 → hpRatio=0.5 → (0.5-0.5)*2.2=0
	record.Personality = unit.Personality{}
	record.Inventory.Equipment = map[string]unit.ItemStack{}
	return record
}

// atbDetState 构造一个没有指令历史、地图为空的最小可执行状态。
func atbDetState(playerFaction string, playerIDs, enemyIDs []string) State {
	state := State{}
	state.ID = "atbDet"
	state.PlayerFactionID = playerFaction
	state.PlayerUnitIDs = playerIDs
	state.EnemyUnitIDs = enemyIDs
	state.TurnState = turns.State{Turn: 1, Phase: turns.PhaseExecution}
	return state
}

// TestATBDet_StatusBonusZeroBaseline 锁定测试假设：清零人格 + HP50% + Morale0.5 + Hunger100 → 速度仅 Base+TaskBias。
func TestATBDet_StatusBonusZeroBaseline(t *testing.T) {
	u := atbDetUnit("u", "player", 4)
	if got := personalitySpeedBonus(u); got != 0 {
		t.Fatalf("人格清零应使 personalitySpeedBonus=0，得到 %v", got)
	}
	if got := statusSpeedBonus(u); got != 0 {
		t.Fatalf("HP50%%+Morale0.5+Hunger100 应使 statusSpeedBonus=0，得到 %v", got)
	}
	if got := equipmentSpeedBonus(u); got != 0 {
		t.Fatalf("无装备应使 equipmentSpeedBonus=0，得到 %v", got)
	}
}

// TestATBDet_SpeedClamp 验证速度被夹在 [2.4, 22]。
func TestATBDet_SpeedClamp(t *testing.T) {
	state := atbDetState("player", []string{"hi"}, []string{"lo"})

	// 高速：Base=6+Move，Move 给极大值使裸速度远超 22。TaskBias 进一步抬高，clamp 必须封顶到 22。
	fast := atbDetUnit("hi", "player", 1000)
	speedFast, _ := atbSpeedForUnit(state, fast)
	if speedFast != 22 {
		t.Fatalf("超高速应被 clamp 到上限 22，得到 %v", speedFast)
	}

	// 低速：把 Morale 压到 0（statusSpeedBonus = (0-0.5)*3.0 = -1.5），HP 压到很低，制造负的状态修正，
	// 同时给极小 Move，让裸速度 < 2.4，验证下限。
	slow := atbDetUnit("lo", "player", 0)
	slow.Status.Morale = 0
	slow.Status.HP = 1
	slow.Status.Hunger = 1 // 触发 <30 与 <10 两档惩罚，进一步拉低
	slow.Personality.Prudence = 1
	slow.Personality.Stability = 1 // control 项拉高 → personalitySpeedBonus 为大负数
	speedSlow, _ := atbSpeedForUnit(state, slow)
	if speedSlow != 2.4 {
		t.Fatalf("超低速应被 clamp 到下限 2.4，得到 %v", speedSlow)
	}
}

// TestATBDet_Deterministic 验证同输入恒得同序（多次调用结果完全一致）。
func TestATBDet_Deterministic(t *testing.T) {
	state := atbDetState("player", []string{"a", "b"}, []string{"c", "d"})
	byID := map[string]*unit.Record{}
	for _, spec := range []struct {
		id      string
		faction string
		move    int
	}{
		{"a", "player", 8},
		{"b", "player", 4},
		{"c", "enemy", 6},
		{"d", "enemy", 2},
	} {
		rec := atbDetUnit(spec.id, spec.faction, spec.move)
		byID[spec.id] = &rec
	}

	first, _ := buildExecutionOrderByATB(state, byID)
	for i := 0; i < 8; i++ {
		again, _ := buildExecutionOrderByATB(state, byID)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("ATB 应确定性：第 %d 次 %v != 首次 %v", i, again, first)
		}
	}
	if len(first) != 4 {
		t.Fatalf("应排出全部 4 个单位，得到 %v", first)
	}
}

// TestATBDet_SpeedOrdering 验证纯速度差异下，更快单位先行动（gauge 跨 tick 累积，先越阈值先动）。
func TestATBDet_SpeedOrdering(t *testing.T) {
	// 单阵营、互不相邻的速度，避免势头惩罚干扰。速度由 Move 决定：Move 越大越快。
	state := atbDetState("player", []string{"slow1", "fast", "slow2", "mid"}, nil)
	byID := map[string]*unit.Record{}
	specs := []struct {
		id   string
		move int
	}{
		{"fast", 16}, // Base=22 → clamp 后 ~22（最快）
		{"mid", 8},
		{"slow1", 2},
		{"slow2", 1},
	}
	for _, s := range specs {
		rec := atbDetUnit(s.id, "player", s.move)
		byID[s.id] = &rec
	}

	order, breakdowns := buildExecutionOrderByATB(state, byID)
	if len(order) != 4 {
		t.Fatalf("应排出 4 个单位，得到 %v", order)
	}

	// 速度严格递减，但同阵营连动会从第3名起施加势头惩罚；仍应保证有效速度单调不增。
	prevSpeed := math.Inf(1)
	for i, id := range order {
		s := breakdowns[id].Total()
		clamped := clampFloat(s, 2.4, 22)
		if clamped > prevSpeed+1e-9 {
			t.Fatalf("第 %d 名 %s 速度 %v 不应高于前一名 %v（速度顺序被破坏）", i, id, clamped, prevSpeed)
		}
		prevSpeed = clamped
	}
	// 在如此悬殊的速度差下，最快者必为首个出手。
	if order[0] != "fast" {
		t.Fatalf("最快单位 fast 应首个行动，得到序 %v", order)
	}
}

// TestATBDet_MomentumPenalty 验证 hasMomentumPenalty 的「同阵营连续两次后第三次起惩罚」语义。
func TestATBDet_MomentumPenalty(t *testing.T) {
	factionByID := map[string]string{
		"p1": "player", "p2": "player", "p3": "player",
		"e1": "enemy",
	}

	// order 不足 2 个时，永不惩罚。
	if hasMomentumPenalty(nil, "player", factionByID) {
		t.Fatal("空 order 不应触发势头惩罚")
	}
	if hasMomentumPenalty([]string{"p1"}, "player", factionByID) {
		t.Fatal("order 仅 1 个时不应触发势头惩罚")
	}

	// 末两位都是 player 且候选也是 player → 第3次起惩罚。
	if !hasMomentumPenalty([]string{"p1", "p2"}, "player", factionByID) {
		t.Fatal("同阵营连续两次后，第三次 player 候选应触发势头惩罚")
	}
	// 候选换成 enemy → 不惩罚（打断连动）。
	if hasMomentumPenalty([]string{"p1", "p2"}, "enemy", factionByID) {
		t.Fatal("末两位为 player 但候选是 enemy，不应惩罚")
	}
	// 末两位非同阵营（p1, e1）→ 即便候选是 player 也不惩罚（未连续两次）。
	if hasMomentumPenalty([]string{"p1", "e1"}, "player", factionByID) {
		t.Fatal("末两位非同阵营时不应惩罚")
	}
	if math.Abs(atbMomentumPenaltyFactor-0.85) > 1e-9 {
		t.Fatalf("势头惩罚系数应为 0.85，得到 %v", atbMomentumPenaltyFactor)
	}
}

// TestATBDet_MomentumPenaltyShiftsOrder 端到端验证：当一方单位数远多于另一方时，
// 势头惩罚会让劣势方的单位被「插队」到前面，最终顺序不是纯粹按入队顺序的同阵营连发。
func TestATBDet_MomentumPenaltyShiftsOrder(t *testing.T) {
	// 三个等速 player + 一个等速 enemy。若无势头惩罚，player 因 Index 更靠前会连发三次再轮到 enemy；
	// 有势头惩罚后，player 连发两次后第三次被 ×0.85，enemy（未受罚、有效速度更高）应抢在第三个 player 之前。
	state := atbDetState("player", []string{"p1", "p2", "p3"}, []string{"e1"})
	byID := map[string]*unit.Record{}
	for _, id := range []string{"p1", "p2", "p3"} {
		rec := atbDetUnit(id, "player", 6)
		byID[id] = &rec
	}
	e := atbDetUnit("e1", "enemy", 6)
	byID["e1"] = &e

	order, _ := buildExecutionOrderByATB(state, byID)
	if len(order) != 4 {
		t.Fatalf("应排出 4 个单位，得到 %v", order)
	}
	// 前两位是 player 连发，第三位应被 enemy 抢占（势头惩罚生效的可观察证据）。
	if !(order[0] == "p1" && order[1] == "p2") {
		t.Fatalf("等速下前两名应是按 Index 的 player 连发 p1,p2，得到 %v", order)
	}
	if order[2] != "e1" {
		t.Fatalf("第三名应是被势头惩罚让出的 enemy（e1 抢占），得到 %v（势头惩罚未生效）", order)
	}
	if order[3] != "p3" {
		t.Fatalf("末名应是受罚后才行动的 p3，得到 %v", order)
	}
}
