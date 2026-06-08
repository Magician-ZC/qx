package session

// 文件说明：离线宪章（OfflineCharter）的读写与持久化辅助——玩家不在场时单位据此自治的长效授权地基。
// 纯逻辑、确定性、无 DB、无全局随机；State.UnitCharters 整块随 state_json 持久化。
// 提供：默认空值规范化、按单位读/写、红线展平为 map（供归因校验填充 snap.Redlines，对齐 attribution.Snapshot.Redlines）。

import "strings"

// GetUnitCharter 返回某单位的离线宪章；缺失/state 为 nil 时返回零值空宪章（绝不返回 nil 引发解引用）。
// 第二返回值标识是否真正登记过该单位的宪章（区分「显式空宪章」与「从未设置」）。
func GetUnitCharter(state *State, unitID string) (OfflineCharter, bool) {
	if state == nil || unitID == "" || state.UnitCharters == nil {
		return OfflineCharter{}, false
	}
	charter, ok := state.UnitCharters[unitID]
	if !ok {
		return OfflineCharter{}, false
	}
	return charter, true
}

// SetUnitCharter 写入/覆盖某单位的离线宪章（懒初始化 map）。
// 写入前对宪章做规范化（去空白条目、为缺 ID 的红线补稳定 ID），保证持久化后读回的一致性与归因可引用。
// state/unitID 为空时安全 no-op。
func SetUnitCharter(state *State, unitID string, charter OfflineCharter) {
	if state == nil || unitID == "" {
		return
	}
	if state.UnitCharters == nil {
		state.UnitCharters = make(map[string]OfflineCharter)
	}
	state.UnitCharters[unitID] = NormalizeCharter(unitID, charter)
}

// ClearUnitCharter 删除某单位的离线宪章（撤销长效授权）。安全 no-op 守卫。
func ClearUnitCharter(state *State, unitID string) {
	if state == nil || unitID == "" || state.UnitCharters == nil {
		return
	}
	delete(state.UnitCharters, unitID)
}

// charterRedlinesAsMap 返回某单位宪章红线的「红线 ID → 原文」映射，供归因校验填充 attribution.Snapshot.Redlines。
// 始终返回非 nil map（无红线时为空 map），便于调用方直接赋值。空白原文条目被跳过。
func charterRedlinesAsMap(state *State, unitID string) map[string]string {
	out := make(map[string]string)
	charter, ok := GetUnitCharter(state, unitID)
	if !ok {
		return out
	}
	for i, redline := range charter.Redlines {
		text := strings.TrimSpace(redline.Text)
		if text == "" {
			continue
		}
		id := strings.TrimSpace(redline.ID)
		if id == "" {
			id = fallbackRedlineID(unitID, i)
		}
		out[id] = text
	}
	return out
}

// NormalizeCharter 规范化一份离线宪章：裁剪三段里的空白条目、为缺 ID 的红线补确定性 ID。
// 确定性：同输入恒同输出（不依赖随机/时间），ID 由 unitID+索引派生，保证持久化往返稳定。
func NormalizeCharter(unitID string, charter OfflineCharter) OfflineCharter {
	normalized := OfflineCharter{
		LongTermGoals:  trimNonEmpty(charter.LongTermGoals),
		SocialMandates: trimNonEmpty(charter.SocialMandates),
	}
	if len(charter.Redlines) > 0 {
		redlines := make([]CharterRedline, 0, len(charter.Redlines))
		for i, redline := range charter.Redlines {
			text := strings.TrimSpace(redline.Text)
			if text == "" {
				continue
			}
			id := strings.TrimSpace(redline.ID)
			if id == "" {
				id = fallbackRedlineID(unitID, i)
			}
			redlines = append(redlines, CharterRedline{
				ID:       id,
				Text:     text,
				Severity: strings.TrimSpace(redline.Severity),
			})
		}
		if len(redlines) > 0 {
			normalized.Redlines = redlines
		}
	}
	return normalized
}

// CharterIsEmpty 判定一份宪章是否三段皆空（用于决定是否值得入库/参与自治）。
func CharterIsEmpty(charter OfflineCharter) bool {
	return len(charter.LongTermGoals) == 0 && len(charter.Redlines) == 0 && len(charter.SocialMandates) == 0
}

// fallbackRedlineID 为缺 ID 的红线派生确定性 ID（unitID + 索引），保证同一宪章每次规范化得到稳定引用。
func fallbackRedlineID(unitID string, index int) string {
	return "rl_" + unitID + "_" + itoaNonNeg(index)
}

// trimNonEmpty 去掉切片里的空白条目并裁剪首尾空白；全空时返回 nil（保 omitempty 干净）。
func trimNonEmpty(items []string) []string {
	if len(items) == 0 {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// itoaNonNeg 把非负整数转十进制字符串（避免引入 strconv 仅为一处转换，确定性）。
func itoaNonNeg(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
