package session

// 文件说明：提供按会话种子与回合号可复现的天气生成逻辑，服务战斗与行动修正链路。

import (
	"fmt"
	"hash/fnv"
)

// weatherForTurn 基于 sessionID 推导稳定种子，并计算当前回合天气。
func weatherForTurn(sessionID string, turn int) WeatherState {
	return weatherForTurnBySeed(seedFromSessionID(sessionID), turn)
}

// weatherForTurnBySeed 按固定阈值把随机值映射为天气类型，保证同种子同回合可复现。
func weatherForTurnBySeed(seed int64, turn int) WeatherState {
	if turn <= 0 {
		turn = 1
	}
	roll := deterministicWeatherRoll(seed, turn)
	switch {
	case roll < 0.4:
		return WeatherState{
			Type:        WeatherClear,
			DisplayName: "晴朗",
			Note:        "视野清晰，常规作战稳定。",
			Turn:        turn,
		}
	case roll < 0.65:
		return WeatherState{
			Type:        WeatherWindy,
			DisplayName: "大风",
			Note:        "草原火攻更凶猛，远程压制更不稳。",
			Turn:        turn,
		}
	case roll < 0.85:
		return WeatherState{
			Type:        WeatherRainy,
			DisplayName: "阴雨",
			Note:        "火攻被抑制，行军更消耗体力。",
			Turn:        turn,
		}
	default:
		return WeatherState{
			Type:        WeatherFoggy,
			DisplayName: "浓雾",
			Note:        "观察受阻，近距离动作更安全。",
			Turn:        turn,
		}
	}
}

// deterministicWeatherRoll 使用哈希构造 [0,1) 伪随机值，避免引入全局随机状态。
func deterministicWeatherRoll(seed int64, turn int) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(fmt.Sprintf("seed:%d", seed)))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", turn)))
	return float64(hasher.Sum32()%10000) / 10000
}

// seedFromSessionID 由会话 ID 生成稳定整数种子，用于天气与其他可复现随机流程。
func seedFromSessionID(sessionID string) int64 {
	if sessionID == "" {
		sessionID = "session"
	}
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(sessionID))
	return int64(hasher.Sum64())
}
