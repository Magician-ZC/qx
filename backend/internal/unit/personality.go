package unit

// 文件说明：单位人格参数生成器，按随机种子产出可复现且受边界约束的人格向量。

import (
	"math"
	"math/rand"
)

// Personality 结构体用于承载该模块的核心数据。
type Personality struct {
	Courage     float64 `json:"courage"`
	Loyalty     float64 `json:"loyalty"`
	Aggression  float64 `json:"aggression"`
	Prudence    float64 `json:"prudence"`
	Sociability float64 `json:"sociability"`
	Integrity   float64 `json:"integrity"`
	Stability   float64 `json:"stability"`
	Ambition    float64 `json:"ambition"`
}

// GeneratePersonality 按随机种子生成单位初始人格参数。
func GeneratePersonality(seed int64) Personality {
	rng := rand.New(rand.NewSource(seed))
	return Personality{
		Courage:     normalizedTrait(rng, 0.58),
		Loyalty:     normalizedTrait(rng, 0.61),
		Aggression:  normalizedTrait(rng, 0.47),
		Prudence:    normalizedTrait(rng, 0.52),
		Sociability: normalizedTrait(rng, 0.49),
		Integrity:   normalizedTrait(rng, 0.54),
		Stability:   normalizedTrait(rng, 0.57),
		Ambition:    normalizedTrait(rng, 0.51),
	}
}

// normalizedTrait 生成并裁剪单项人格值到安全区间。
func normalizedTrait(rng *rand.Rand, center float64) float64 {
	value := center + ((rng.Float64() - 0.5) * 0.44)
	value = math.Max(0.05, math.Min(0.95, value))
	return math.Round(value*100) / 100
}
