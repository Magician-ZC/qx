package session

// 文件说明：combat_roll FNV 确定性掷骰测试（GDD §7「先写的5个测试」之②）。
// 覆盖 combatActionRoll：同 (sessionID,attacker,target,salt,turn) 恒得同值、不同 salt/turn 得不同值、值域合理 [0,1)。
// 仅新增测试，零生产改动；用 combatRollDet_ 前缀避免与既有 helper 撞名。

import (
	"testing"

	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/unit"
)

// combatRollDetActor 构造一个仅带 ID 的最小单位（combatActionRoll 只读 attacker.ID/target.ID）。
func combatRollDetActor(id string) unit.Record {
	rec := unit.Record{ID: id}
	return rec
}

// combatRollDetState 构造一个仅带 ID 与回合号的最小状态（combatActionRoll 只读 state.ID/state.TurnState.Turn）。
func combatRollDetState(id string, turn int) State {
	state := State{}
	state.ID = id
	state.TurnState = turns.State{Turn: turn}
	return state
}

// TestCombatRollDet_Stable 验证同 (sessionID,attacker,target,salt,turn) 多次调用恒得同值。
func TestCombatRollDet_Stable(t *testing.T) {
	state := combatRollDetState("sess-1", 7)
	attacker := combatRollDetActor("atk-1")
	target := combatRollDetActor("tgt-1")

	want := combatActionRoll(state, attacker, target, "hit")
	for i := 0; i < 100; i++ {
		got := combatActionRoll(state, attacker, target, "hit")
		if got != want {
			t.Fatalf("掷骰应确定性：第 %d 次 %v != 首次 %v", i, got, want)
		}
	}
}

// TestCombatRollDet_Range 验证返回值落在 [0, 1) 区间（Sum32()%10000/10000）。
func TestCombatRollDet_Range(t *testing.T) {
	salts := []string{"hit", "crit", "dodge", "block", "flee", "panic"}
	for atk := 0; atk < 40; atk++ {
		for tgt := 0; tgt < 5; tgt++ {
			state := combatRollDetState("sess", atk%6)
			attacker := combatRollDetActor("atk-" + itoaSmall(atk))
			target := combatRollDetActor("tgt-" + itoaSmall(tgt))
			for _, salt := range salts {
				v := combatActionRoll(state, attacker, target, salt)
				if v < 0 || v >= 1 {
					t.Fatalf("掷骰值应在 [0,1)，atk=%d tgt=%d salt=%s 得到 %v", atk, tgt, salt, v)
				}
				// 因为是 Sum32()%10000/10000，必为 1/10000 的整数倍。
				scaled := v * 10000
				rounded := float64(int(scaled + 0.5))
				if scaled-rounded > 1e-6 || rounded-scaled > 1e-6 {
					t.Fatalf("掷骰应为 1/10000 整数倍，得到 %v", v)
				}
			}
		}
	}
}

// TestCombatRollDet_SaltDiverges 验证不同 salt 在同上下文下应产生不同值（同 salt 同值，跨 salt 大多分散）。
func TestCombatRollDet_SaltDiverges(t *testing.T) {
	state := combatRollDetState("sess-x", 3)
	attacker := combatRollDetActor("atk")
	target := combatRollDetActor("tgt")

	salts := []string{"hit", "crit", "dodge", "block", "flee", "skill", "panic", "retreat"}
	seen := map[float64]string{}
	collisions := 0
	for _, salt := range salts {
		v := combatActionRoll(state, attacker, target, salt)
		if other, ok := seen[v]; ok {
			collisions++
			_ = other
		}
		seen[v] = salt
	}
	// 至少应有显著分散：8 个不同 salt 不应坍缩到 ≤2 个不同值。
	if len(seen) < 6 {
		t.Fatalf("不同 salt 应产生分散的掷骰值，8 个 salt 只得到 %d 个不同值", len(seen))
	}
}

// TestCombatRollDet_TurnDiverges 验证不同 turn 在同上下文同 salt 下应产生不同值（turn 进入哈希）。
func TestCombatRollDet_TurnDiverges(t *testing.T) {
	attacker := combatRollDetActor("atk")
	target := combatRollDetActor("tgt")

	values := map[float64]int{}
	for turn := 1; turn <= 30; turn++ {
		state := combatRollDetState("sess", turn)
		v := combatActionRoll(state, attacker, target, "hit")
		values[v]++
	}
	// turn 进入哈希，30 个回合不应坍缩到极少的几个值。
	if len(values) < 20 {
		t.Fatalf("turn 应影响掷骰：30 个回合只得到 %d 个不同值", len(values))
	}
}

// TestCombatRollDet_ParticipantsDiverge 验证交换 attacker/target 或更换会话会改变掷骰。
func TestCombatRollDet_ParticipantsDiverge(t *testing.T) {
	state := combatRollDetState("sess", 5)
	a := combatRollDetActor("alpha")
	b := combatRollDetActor("bravo")

	ab := combatActionRoll(state, a, b, "hit")
	ba := combatActionRoll(state, b, a, "hit")
	if ab == ba {
		t.Fatalf("attacker/target 顺序应影响掷骰：a→b=%v b→a=%v 不应相等", ab, ba)
	}

	other := combatRollDetState("sess-other", 5)
	if combatActionRoll(other, a, b, "hit") == ab {
		t.Fatalf("不同 sessionID 应改变掷骰，得到相同值 %v", ab)
	}
}

// itoaSmall 是一个极简整数转字符串（避免引入 strconv，与本特性局部自包含）。
func itoaSmall(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := [12]byte{}
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
