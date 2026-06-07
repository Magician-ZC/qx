package relevance

// 文件说明：相关性与传播的确定性原语（设计文档 docs/事件耦合与跨玩家关联.md 的「结算」核心）。
// 一个角色对世界保有一组「锚」(她在乎的人/红线/目标/债仇爱/地方/传承)；世界事件是探针，
// 命运 = 探针点亮一根锚。本包把「锚命中 → 相关性评分 → 沿关系图衰减传播」做成纯函数、零依赖、可测试，
// 与 engine/arbitration(裁决)、engine/decision(归因/反射) 一起构成结算层确定性的几块基石。

import "math"

// AnchorKind 是相关性锚的六个类别（与设计宪法 Relevance 因子 + attribution cause 对齐）。
type AnchorKind string

const (
	Relation       AnchorKind = "relation"         // 她在乎的人
	Redline        AnchorKind = "redline"          // 离线宪章红线
	Goal           AnchorKind = "goal"             // 当前目标
	DebtGrudgeLove AnchorKind = "debt_grudge_love" // 债/仇/爱
	Geo            AnchorKind = "geo"              // 所在地
	Legacy         AnchorKind = "legacy"           // 传家物/血脉
)

// FactorWeights 是各锚类别的相关性因子权重（设计宪法 §4.1 默认值，[待测试/可调]）。
// 注意：这是「加权求和」而非「加权平均」——多根锚被同一事件点亮时相关性可叠加越过阈值，
// 单根锚的贡献以其权重为上限，故重大命运 beat 往往需要多锚共振或较高 weight。
var FactorWeights = map[AnchorKind]float64{
	Relation:       0.32,
	Redline:        0.28,
	Goal:           0.18,
	DebtGrudgeLove: 0.14,
	Geo:            0.08,
	Legacy:         0.14,
}

// 阈值与传播参数（设计文档默认值，[待测试/可调]）。
const (
	RelevanceGate = 0.35 // 相关性 ≥ 此值才进前台候选
	MatchGate     = 0.45 // 撮合分 ≥ 此值才入候选池
	MaxHops       = 2    // 关系图传播最多跳数（防全图洪泛）
	TransmitFloor = 0.15 // 单跳传递重要度地板，低于即停
	FidelityFloor = 0.30 // 可信度地板，低于即停
	HopDecay      = 0.6  // 每跳可信度衰减系数
)

// Anchor 是角色对世界的一根弦。
type Anchor struct {
	Kind         AnchorKind
	Ref          string
	Weight       float64 // [0,1]
	HalfLifeDays float64 // <=0 表示不衰减（如红线/传承）
}

// Hit 是一根被事件点亮的锚，AgeDays 是该锚自上次刷新以来的天数（用于时间衰减）。
type Hit struct {
	Anchor  Anchor
	AgeDays float64
}

// FactorWeight 返回某锚类别的因子权重。
func FactorWeight(kind AnchorKind) float64 {
	return FactorWeights[kind]
}

// TimeDecay 按半衰期做时间衰减：2^(-age/halfLife)。半衰期 ≤0 或 age ≤0 时返回 1。
func TimeDecay(ageDays float64, halfLifeDays float64) float64 {
	if halfLifeDays <= 0 || ageDays <= 0 {
		return 1
	}
	return math.Pow(2, -ageDays/halfLifeDays)
}

// HopFidelity 返回传播 hop 跳后的可信度：HopDecay^hop（hop≤0 为直击，可信度 1）。
func HopFidelity(hop int) float64 {
	if hop <= 0 {
		return 1
	}
	return math.Pow(HopDecay, float64(hop))
}

// Score 把一组「被事件点亮的锚」聚合为相关性分。
// 公式：Σ_hits weight·factorWeight(kind)·timeDecay(age,halfLife)·hopFidelity。
func Score(hits []Hit, hopFidelity float64) float64 {
	if hopFidelity <= 0 {
		hopFidelity = 1
	}
	total := 0.0
	for _, h := range hits {
		total += clamp01(h.Anchor.Weight) * FactorWeight(h.Anchor.Kind) * TimeDecay(h.AgeDays, h.Anchor.HalfLifeDays) * hopFidelity
	}
	return total
}

// PassesGate 判断相关性是否过阈、可进前台候选。
func PassesGate(relevance float64) bool {
	return relevance >= RelevanceGate
}

// StopPropagation 判断关系图传播是否应在此跳停止（任一条件成立即停，防全图洪泛）。
func StopPropagation(hop int, transmit float64, fidelity float64) bool {
	return hop >= MaxHops || transmit < TransmitFloor || fidelity < FidelityFloor
}

// ConsentTier 是异步跨玩家交互的知情权档位。
type ConsentTier string

const (
	Unilateral      ConsentTier = "unilateral"       // 单方成立，事后知情（层1 可恢复）
	Contested       ConsentTier = "contested"        // 单方成立但可被回应（层2 高代价）
	RequiresConsent ConsentTier = "requires_consent" // 需对方角色自治同意（层3 不可逆）
)

// ConsentTierFor 把交互对目标的「最坏后果分级层」映射为所需的知情权档位。
func ConsentTierFor(worstConsequenceLayer int) ConsentTier {
	switch {
	case worstConsequenceLayer >= 3:
		return RequiresConsent
	case worstConsequenceLayer == 2:
		return Contested
	default:
		return Unilateral
	}
}

// 撮合分四因子权重（设计文档 §2.2 默认值，[待测试/可调]）。
const (
	matchGeoWeight     = 0.35
	matchHookWeight    = 0.30
	matchRelWeight     = 0.20
	matchDensityWeight = 0.15
)

// MatchScore 计算把两个玩家角色并入同一社会客体的撮合分（密度优先、钩子驱动）。
// densityAdj：该角色当前活跃跨玩家关系越少越高（防大 R 垄断社交）。
func MatchScore(geoNear float64, hookFit float64, relationIntersect float64, densityAdj float64) float64 {
	return matchGeoWeight*clamp01(geoNear) +
		matchHookWeight*clamp01(hookFit) +
		matchRelWeight*clamp01(relationIntersect) +
		matchDensityWeight*clamp01(densityAdj)
}

// PassesMatch 判断撮合分是否过阈、可入候选池。
func PassesMatch(score float64) bool {
	return score >= MatchGate
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
