package unit

// 文件说明：单位生命状态机实现，处理致命伤、救援、自行恢复与恢复倒计时推进。

import "fmt"

// 常量定义区：集中声明该文件使用的共享配置。
const (
	LifeStateActive     = "active"
	LifeStateDown       = "down"
	LifeStateRecovering = "recovering"
	LifeStateDead       = "dead"
)

// ApplyFatalDamage 处理致命伤害并推进单位生命状态机。
func ApplyFatalDamage(record *Record) error {
	if record.Status.LifeState == LifeStateDead {
		return fmt.Errorf("unit is already permanently dead")
	}

	record.Status.HP = 0
	if record.Status.LivesRemaining <= 1 {
		record.Status.LivesRemaining = 0
		record.Status.LifeState = LifeStateDead
		record.Status.RecoveryTurns = 0
		return nil
	}

	record.Status.LivesRemaining--
	record.Status.LifeState = LifeStateDown
	record.Status.RecoveryTurns = 2
	return nil
}

// Rescue 把倒地单位救回恢复状态并施加心理后遗影响。
func Rescue(record *Record) error {
	if record.Status.LifeState != LifeStateDown {
		return fmt.Errorf("unit is not down")
	}

	record.Status.LifeState = LifeStateRecovering
	record.Status.RecoveryTurns = 1
	record.Status.HP = 30
	record.Personality.Courage = lowerTrait(record.Personality.Courage, 0.05)
	record.Personality.Stability = lowerTrait(record.Personality.Stability, 0.10)
	record.Status.Debuffs = append(record.Status.Debuffs, "rescued_recently")
	return nil
}

// SelfRevive 处理单位自行恢复流程（恢复更慢、血量更低）。
func SelfRevive(record *Record) error {
	if record.Status.LifeState != LifeStateDown {
		return fmt.Errorf("unit is not down")
	}

	record.Status.LifeState = LifeStateRecovering
	record.Status.RecoveryTurns = 3
	record.Status.HP = 20
	record.Personality.Courage = lowerTrait(record.Personality.Courage, 0.05)
	record.Personality.Stability = lowerTrait(record.Personality.Stability, 0.10)
	record.Status.Debuffs = append(record.Status.Debuffs, "battlefield_recovery")
	return nil
}

// TickRecovery 推进恢复倒计时并在到期时恢复 active 状态。
func TickRecovery(record *Record) {
	if record.Status.RecoveryTurns > 0 {
		record.Status.RecoveryTurns--
	}
	if record.Status.RecoveryTurns == 0 && record.Status.LifeState == LifeStateRecovering {
		record.Status.LifeState = LifeStateActive
	}
}

// SetNewbornBattleStats 初始化新生儿单位的战斗基础属性。
// 新生儿是初始化而非状态变更事件，无需经 StatusMutator 生成审计事件行；
// 本函数位于 statuslint 白名单（/internal/unit/），直赋受保护字段 HP 合法，
// 与同文件 ApplyFatalDamage/Rescue/SelfRevive 设 HP 同源。调用方：session/romance.go createChildUnit。
func SetNewbornBattleStats(record *Record, hp, attack, defense, move int) {
	record.Status.HP = hp
	record.Status.Attack = attack
	record.Status.Defense = defense
	record.Status.Move = move
}

// lowerTrait 安全降低人格值，避免小于 0。
func lowerTrait(current float64, delta float64) float64 {
	value := current - delta
	if value < 0 {
		return 0
	}
	return value
}
