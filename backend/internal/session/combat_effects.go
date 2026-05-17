package session

// 文件说明：管理战斗临时效果标记（添加、解析、过期清理）并统一以 turn 作为有效期。

import (
	"strconv"
	"strings"

	"qunxiang/backend/internal/unit"
)

// 常量定义区：集中声明该文件使用的共享配置。
const (
	combatEffectPrefix   = "combat:"
	combatEffectGuarded  = combatEffectPrefix + "guarded:"
	combatEffectFocused  = combatEffectPrefix + "focused:"
	combatEffectAssisted = combatEffectPrefix + "assisted:"
	combatEffectRage     = combatEffectPrefix + "rage:"
)

// grantCombatEffect 给单位写入一个带过期回合的战斗效果标记。
func grantCombatEffect(record *unit.Record, prefix string, expireTurn int) bool {
	if record == nil || prefix == "" || expireTurn <= 0 {
		return false
	}
	token := prefix + strconv.Itoa(expireTurn)
	for _, entry := range record.Status.Debuffs {
		if entry == token {
			return false
		}
	}
	record.Status.Debuffs = append(record.Status.Debuffs, token)
	return true
}

// hasCombatEffect 判断单位在当前回合是否仍持有指定战斗效果。
func hasCombatEffect(record unit.Record, prefix string, turn int) bool {
	if prefix == "" || turn <= 0 {
		return false
	}
	for _, entry := range record.Status.Debuffs {
		expireTurn, ok := parseCombatEffect(entry, prefix)
		if !ok {
			continue
		}
		if expireTurn >= turn {
			return true
		}
	}
	return false
}

// clearExpiredCombatEffects 清理已过期的战斗效果标记。
func clearExpiredCombatEffects(record *unit.Record, turn int) bool {
	if record == nil || turn <= 0 || len(record.Status.Debuffs) == 0 {
		return false
	}
	changed := false
	filtered := record.Status.Debuffs[:0]
	for _, entry := range record.Status.Debuffs {
		if !strings.HasPrefix(entry, combatEffectPrefix) {
			filtered = append(filtered, entry)
			continue
		}
		expireTurn, ok := parseCombatEffectTurn(entry)
		if !ok || expireTurn >= turn {
			filtered = append(filtered, entry)
			continue
		}
		changed = true
	}
	record.Status.Debuffs = append([]string{}, filtered...)
	return changed
}

// parseCombatEffect 解析形如 prefix+turn 的效果标记。
func parseCombatEffect(value string, prefix string) (int, bool) {
	if !strings.HasPrefix(value, prefix) {
		return 0, false
	}
	raw := strings.TrimPrefix(value, prefix)
	if raw == "" {
		return 0, false
	}
	expireTurn, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return expireTurn, true
}

// parseCombatEffectTurn 从效果标记尾部提取过期回合。
func parseCombatEffectTurn(value string) (int, bool) {
	index := strings.LastIndex(value, ":")
	if index < 0 || index == len(value)-1 {
		return 0, false
	}
	raw := value[index+1:]
	if raw == "" {
		return 0, false
	}
	expireTurn, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return expireTurn, true
}
