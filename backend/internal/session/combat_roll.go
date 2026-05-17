package session

// 文件说明：提供战斗结算使用的确定性掷骰函数，保证同会话同回合同参数结果可复放。

import (
	"fmt"
	"hash/fnv"

	"qunxiang/backend/internal/unit"
)

// combatActionRoll 生成战斗动作结算使用的稳定随机值。
func combatActionRoll(state State, attacker unit.Record, target unit.Record, salt string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte(attacker.ID))
	_, _ = hasher.Write([]byte(target.ID))
	_, _ = hasher.Write([]byte(salt))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", state.TurnState.Turn)))
	return float64(hasher.Sum32()%10000) / 10000
}
