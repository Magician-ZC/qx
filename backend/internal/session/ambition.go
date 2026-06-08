package session

// 文件说明：六维野心引力源——把 unit.Profile.Ambition（power/vengeance/wealth/lineage/mastery/freedom）
// 翻译成「自发行为引力」权重，供决策上下文 / ambient 行为选择加权。纯函数、确定性、零副作用、零 LLM，
// 体现「决策用 LLM、结算用代码」原则中的「结算/打分用代码」一侧。本文件不碰 types.go，仅消费 Schema 阶段
// 已建好的 unit.Ambition 字段；调用方（Wire 阶段）把返回的引力映射并入既有决策/ambient 权重即可。

import "qunxiang/backend/internal/unit"

// 常量定义区：集中声明该文件使用的共享配置。
const (
	// ambitionFloor 是野心引力的归一化下限——全 0 野心（旧存档/无明显野心）时各候选行为获得的基础引力，
	// 保证「无野心」单位不会因为乘上 0 引力而被彻底抑制某类行为（引力是加权倾向，不是硬门）。
	ambitionFloor = 1.0
	// ambitionGain 是野心分量对引力的最大加成系数：某维野心=1 时，对应行为标签的引力达到 floor+gain。
	ambitionGain = 0.6
)

// AmbitionBias 是六维野心翻译出的「自发行为引力」映射：key 是行为标签（与 ambient/决策候选的语义标签对齐），
// value 是 [ambitionFloor, ambitionFloor+ambitionGain] 区间的引力权重，越大表示该单位越倾向该类自发行为。
// 调用方把候选行为的标签映射到此表取引力、乘进既有打分即可（缺标签按 1.0 中性处理，见 BiasFor）。
type AmbitionBias map[string]float64

// 野心六维 → 受其驱动的自发行为标签。一维可驱动多个标签，一个标签也可被多维叠加驱动（取最大引力，见 ambitionBias）。
// 标签语义：
//   - conquer  攻伐扩张 / 夺权上位（power、vengeance）
//   - revenge  复仇雪耻（vengeance）
//   - hoard    敛财囤积 / 经商逐利（wealth）
//   - nurture  养育血脉 / 联姻成家（lineage）
//   - train    精进技艺 / 钻研学问（mastery）
//   - explore  闯荡远游 / 不受拘束（freedom、mastery）
//   - bond     社交结盟 / 经营人脉（lineage、power）
var ambitionDimensionTags = map[string][]string{
	"power":     {"conquer", "bond"},
	"vengeance": {"revenge", "conquer"},
	"wealth":    {"hoard"},
	"lineage":   {"nurture", "bond"},
	"mastery":   {"train", "explore"},
	"freedom":   {"explore"},
}

// ambitionBias 是纯函数核心：把六维野心向量翻译成行为标签引力表。确定性（只读 amb，不引随机/时间），
// 同输入恒同输出。某标签被多维驱动时取各维贡献的最大引力（取最强驱动，而非叠加致引力越界）。
func ambitionBias(amb unit.Ambition) AmbitionBias {
	dims := map[string]float64{
		"power":     clamp01(amb.Power),
		"vengeance": clamp01(amb.Vengeance),
		"wealth":    clamp01(amb.Wealth),
		"lineage":   clamp01(amb.Lineage),
		"mastery":   clamp01(amb.Mastery),
		"freedom":   clamp01(amb.Freedom),
	}
	bias := make(AmbitionBias, len(ambitionDimensionTags))
	for dim, tags := range ambitionDimensionTags {
		value := dims[dim]
		if value <= 0 {
			continue
		}
		weight := ambitionFloor + ambitionGain*value
		for _, tag := range tags {
			if existing, ok := bias[tag]; !ok || weight > existing {
				bias[tag] = weight
			}
		}
	}
	return bias
}

// AmbitionBiasOf 是供调用方（决策上下文 / ambient 选择）使用的导出入口：从单位档案取六维野心算引力表。
// 纯函数、确定性、零副作用——可在决策前 / ambient 候选打分时直接调用，结果用于乘进既有权重。
func AmbitionBiasOf(record unit.Record) AmbitionBias {
	return ambitionBias(record.Ambition)
}

// BiasFor 取某行为标签的引力权重；未被任何野心驱动的标签返回 ambitionFloor（1.0 中性，不抑制也不放大）。
// 调用方对候选行为打分时：finalScore = baseScore * bias.BiasFor(actionTag)。
func (bias AmbitionBias) BiasFor(tag string) float64 {
	if bias == nil {
		return ambitionFloor
	}
	if weight, ok := bias[tag]; ok {
		return weight
	}
	return ambitionFloor
}

// Dominant 返回引力最高的行为标签及其权重；引力表为空（无明显野心）时返回 ("", ambitionFloor)。
// 平手时取标签字典序最小者，保证确定性（不依赖 map 迭代顺序）。
func (bias AmbitionBias) Dominant() (string, float64) {
	bestTag := ""
	bestWeight := ambitionFloor
	for tag, weight := range bias {
		if bestTag == "" || weight > bestWeight || (weight == bestWeight && tag < bestTag) {
			bestTag = tag
			bestWeight = weight
		}
	}
	return bestTag, bestWeight
}
