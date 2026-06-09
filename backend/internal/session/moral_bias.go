package session

// 文件说明：道德自治偏置（阵营开放世界 F2，设计「道德漂移 + 自治偏置 + 概率切换阵营」之②）。
//
// 角色的数值道德轴（unit.Record.MoralAlignment）+ 其阵营信条（faction.Definition.MoralCreed）共同偏置她的自治决策：
//   - prompt 注入（MoralDecisionContext）：仿 ambition 的「你内心最强的渴望」注入，往决策 prompt 加一句
//     「她认同{阵营}——{creed}；近来她的心更偏{主导}」，让 LLM 看见这个角色的阵营底色与道德倾向。
//   - fallback 偏置（MoralActionBias）：仿 ambition 的 OnlineAmbitionActionWeight，规则 fallback 选动作时，
//     符合阵营信条/道德倾向的动作（秩序→守序协作、混乱→破坏攻伐、自由→独立游走）获更高权重。
//
// 默认开（无 flag）：纯措辞注入 + 候选打分偏置，不改受保护字段、不切阵营，低风险。纯函数、确定性、零 LLM、付费不进。
//
// 设计要点（与 ambition_scoring 同纪律）：
//   - 偏置只放大「契合」动作（乘数 ≥1.0），从不抑制不契合动作 → 不与既有人格/记忆/忠诚信号双重惩罚同一动作。
//   - 道德轴全零（旧存档/无明显倾向）→ prompt 跳过道德倾向句、偏置恒 1.0（中性、逐位一致、零行为变化）。
//   - 未登记动作 / 无主导倾向 → 1.0 中性。

import (
	"fmt"
	"strings"

	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/unit"
)

const (
	// moralBiasFloor 道德动作偏置的中性基准（无契合/无倾向时的乘数）。
	moralBiasFloor = 1.0
	// moralBiasGain 道德契合的最大加成：某轴占绝对主导（归一接近 1）时，契合该轴的动作乘数达 floor+gain。
	moralBiasGain = 0.5
	// moralLeaningBaseline 是「构成真实道德倾向」的归一权重门槛（三轴均分点 1/3）。某轴归一权重 ≤ 此值
	// 视作残余/非倾向（不放大其标签动作）——只有真正的道德**主导/偏向**才偏置决策，避免 10% 残余轴误推动作。
	moralLeaningBaseline = 1.0 / 3.0
)

// moralAxisActionTags 是「道德轴 → 受该取向驱动的决策动作标签」映射（与 ambition.go 的标签语义解耦、各管一摊）。
//   - freedom 自由：独立游走/自行其是——explore（移动游走）、defy（自主拒令，由 fallback 候选语义体现）。
//   - order  秩序：守序协作/尽责护众——protect（援助/防御）、build（营造/生产）、bond（社交结盟）。
//   - chaos  混乱：破而后立/暴力夺取——assault（攻伐）、raid（劫掠夺取）。
//
// 标签语义对齐 OnlineActionAmbitionTag 的动作空间，但独立成表（道德偏置与野心偏置正交、可叠加）。
var moralAxisActionTags = map[moralAxis][]string{
	moralAxisFreedom: {"explore"},
	moralAxisOrder:   {"protect", "build", "bond"},
	moralAxisChaos:   {"assault"},
}

// onlineActionMoralTagTable 把在线决策动作映射到道德动作标签（与 onlineActionAmbitionTagTable 解耦）。
//   - move → explore（自由游走）；
//   - assist/defend → protect、build/forge/upgrade → build、say/dialogue → bond（秩序协作）；
//   - attack/charge/heavy_attack → assault（混乱暴力）。
//
// 纯生存/中性动作（eat/pickup/observe/hold/gather/skill/equip…）不进表 → 中性 1.0。
var onlineActionMoralTagTable = map[DecisionAction]string{
	DecisionActionMove:        "explore",
	DecisionActionAssist:      "protect",
	DecisionActionDefend:      "protect",
	DecisionActionBuild:       "build",
	DecisionActionForge:       "build",
	DecisionActionUpgrade:     "build",
	DecisionActionSay:         "bond",
	DecisionActionDialogue:    "bond",
	DecisionActionAttack:      "assault",
	DecisionActionCharge:      "assault",
	DecisionActionHeavyAttack: "assault",
}

// OnlineActionMoralTag 把在线 DecisionAction 映射到道德动作标签；中性/未登记动作返回 ""（调用方按 1.0 中性处理）。
// 纯函数、确定性、零副作用——供 llm.go 在线规则 fallback 候选打分调用。
func OnlineActionMoralTag(action DecisionAction) string {
	if tag, ok := onlineActionMoralTagTable[action]; ok {
		return tag
	}
	return ""
}

// moralAxisWeights 把单位道德轴归一成 {tag → 归一权重 [0,1]} 表：某轴占总轴和的比例 → 该轴标签的归一强度。
// 全零轴（无倾向）返回空表（调用方按中性处理）。纯函数、确定性。
func moralAxisWeights(m faction.MoralAlignment) map[string]float64 {
	total := m.Freedom + m.Order + m.Chaos
	if total <= 0 {
		return nil
	}
	out := make(map[string]float64, 4)
	put := func(axis moralAxis, value float64) {
		norm := value / total // [0,1]，三轴归一
		for _, tag := range moralAxisActionTags[axis] {
			if existing, ok := out[tag]; !ok || norm > existing {
				out[tag] = norm
			}
		}
	}
	put(moralAxisFreedom, m.Freedom)
	put(moralAxisOrder, m.Order)
	put(moralAxisChaos, m.Chaos)
	return out
}

// MoralActionBias 是在线决策候选打分的道德乘权因子，供 llm.go 的规则 fallback 乘进既有 baseScore：
//
//	finalScore := baseScore * MoralActionBias(record, OnlineActionMoralTag(candidate.Action))
//
// 语义（与 OnlineAmbitionActionWeight 同形、但默认开——道德偏置低风险、是阵营开放世界的核心乐趣）：
//   - 道德轴全零（无倾向）/ actionTag 空（中性动作）/ 标签未被任何轴驱动 → 恒 moralBiasFloor(1.0，中性、零影响)；
//   - 否则返回 floor + gain·归一强度 ∈ [1.0, 1.5]（道德倾向越契合该动作乘得越高）。
//
// 仅放大不抑制（乘数 ≥1.0）。纯函数、确定性、零副作用、零 LLM、付费不进（只读 record.MoralAlignment）。
func MoralActionBias(record unit.Record, actionTag string) float64 {
	if actionTag == "" {
		return moralBiasFloor
	}
	weights := moralAxisWeights(record.MoralAlignment)
	if weights == nil {
		return moralBiasFloor
	}
	norm, ok := weights[actionTag]
	if !ok {
		return moralBiasFloor
	}
	// 只有归一权重越过均分门槛（真实倾向）才放大；门槛以下（残余轴）视作中性，避免 10% 残余误推动作。
	if norm <= moralLeaningBaseline {
		return moralBiasFloor
	}
	// 把 (baseline,1] 的超额线性映射到 (0,1]，使「均分→0 加成、绝对主导→满 gain」，倾向越强乘得越高。
	excess := (norm - moralLeaningBaseline) / (1.0 - moralLeaningBaseline)
	return moralBiasFloor + moralBiasGain*clamp01(excess)
}

// PickMoralBiasedCandidate 在「一组同优先级候选」里按道德契合挑最贴该单位道德倾向的一个，供 llm.go 在线规则
// fallback 做确定性 tie-break（仿 PickAmbitionBiasedCandidate）。
//
// 行为契约（向后兼容）：空候选 → (零值,false)；道德轴全零/全中性 → 取首个（保留调用方原优先级，逐位一致）；
// 否则取道德契合权最高者，平手取**输入靠前者**（稳定 tie-break，仅当后者严格更高才改写）。
// 纯函数、确定性、零副作用、零 LLM、付费不进。record 为 nil 时退化为「取首个」（失败安全）。
func PickMoralBiasedCandidate(record *unit.Record, candidates []decisionCandidate) (decisionCandidate, bool) {
	if len(candidates) == 0 {
		return decisionCandidate{}, false
	}
	if record == nil {
		return candidates[0], true
	}
	best := candidates[0]
	bestWeight := MoralActionBias(*record, OnlineActionMoralTag(best.Action))
	for _, candidate := range candidates[1:] {
		weight := MoralActionBias(*record, OnlineActionMoralTag(candidate.Action))
		if weight > bestWeight { // 严格大于才改写 → 平手保留靠前者（稳定）。
			best = candidate
			bestWeight = weight
		}
	}
	return best, true
}

// MoralDecisionContext 生成注入决策 prompt 的一句道德上下文（仿 ambition 的「你内心最强的渴望」注入）。
//
// 输出形如：「你认同{阵营中文名}——{creed}；近来你的心更偏{主导取向}。」
//   - 阵营缺失/未识别 → 跳过阵营从属句（只在有合法阵营时给信条）。
//   - 道德轴全零（无倾向）→ 跳过「心更偏」从句（不编造倾向）。
//   - 全缺 → 返回空串（调用方据空跳过该行，prompt 零增量、向后兼容）。
//
// 纯函数、确定性、零副作用——供 llm.go 在 buildDecisionPrompt 注入。语气以「你」为主语，与既有 prompt 一致。
func MoralDecisionContext(record unit.Record) string {
	var parts []string
	if def, ok := faction.Get(record.Faction); ok {
		parts = append(parts, fmt.Sprintf("你认同%s——%s", def.NameZH, strings.TrimRight(def.MoralCreed, "。")))
	}
	if dominant := faction.DominantFaction(record.MoralAlignment); dominant != "" {
		if leaning := moralLeaningDisplay(dominant); leaning != "" {
			parts = append(parts, fmt.Sprintf("近来你的心更偏%s", leaning))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "；") + "。"
}

// moralLeaningDisplay 把主导阵营 ID 翻成「心更偏…」的道德倾向中文短语（与 faction creed 语义对齐）。
func moralLeaningDisplay(dominantFactionID string) string {
	switch faction.Normalize(dominantFactionID) {
	case faction.IDFreedom:
		return "自由不羁、各凭本心"
	case faction.IDOrder:
		return "秩序律法、各守其分"
	case faction.IDChaos:
		return "破立无常、不拘旧规"
	default:
		return ""
	}
}
