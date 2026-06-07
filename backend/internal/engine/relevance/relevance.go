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

// FactorWeights 表达各锚类别的「相对重要度排序」（沿用设计宪法 §4.1 的相对大小，[待测试/可调]）。
// 仅用其相对比例：内部归一化为 RelativeImportance（最重要的类别=1.0），避免「加权和为 1 → 单锚被
// 自身权重封顶、永远过不了 0.35 阈」的标定缺陷。多锚通过 noisy-OR 组合，故单根强锚也能过阈、
// 多锚共振次可加、相关性恒在 [0,1]。
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

// FactorWeight 返回某锚类别的原始因子权重（相对重要度的来源）。
func FactorWeight(kind AnchorKind) float64 {
	return FactorWeights[kind]
}

// maxFactorWeight 返回当前最重要类别的权重（用于归一化为相对重要度）。
func maxFactorWeight() float64 {
	m := 0.0
	for _, w := range FactorWeights {
		if w > m {
			m = w
		}
	}
	if m <= 0 {
		return 1
	}
	return m
}

// RelativeImportance 把因子权重归一化为 [0,1]（最重要类别=1.0），保留相对排序但不缩水单锚量级。
func RelativeImportance(kind AnchorKind) float64 {
	return FactorWeights[kind] / maxFactorWeight()
}

// contribution 是单根被点亮的锚对相关性的贡献，夹在 [0,1]。
func contribution(h Hit, hopFidelity float64) float64 {
	return clamp01(clamp01(h.Anchor.Weight) * RelativeImportance(h.Anchor.Kind) * TimeDecay(h.AgeDays, h.Anchor.HalfLifeDays) * hopFidelity)
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

// Score 把一组「被事件点亮的锚」聚合为相关性分 ∈ [0,1]。
// 组合用 noisy-OR：relevance = 1 − Π_hits (1 − contribution)，其中
// contribution = clamp01(weight · relativeImportance(kind) · timeDecay(age,halfLife) · hopFidelity)。
// 性质：单根强锚即可过阈；多锚共振次可加(不重复计数)；恒在 [0,1]；对任一 weight 单调不减。
func Score(hits []Hit, hopFidelity float64) float64 {
	if hopFidelity <= 0 {
		hopFidelity = 1
	}
	inverse := 1.0
	for _, h := range hits {
		inverse *= 1 - contribution(h, hopFidelity)
	}
	return 1 - inverse
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
