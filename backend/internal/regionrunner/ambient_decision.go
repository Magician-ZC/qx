package regionrunner

// 文件说明：region-runner 离线自治的「动机栈候选生成 + 确定性打分」（M7.3-real 动机栈消费，大世界沙盘 §4 / §8.2）。
// 把原先 decideAmbientReflex「单 if-else 直出一个动作」升级为「生成带分候选列表 → 确定性降序排序」：
//   L1 安全护栏（饥饿/士气阈值）→ 候选集与基础分 → L3 偏置（近期记忆 tag 触发）→ 野心乘权（六维野心引力）→ finalScore 降序。
// 这条链让「敛财野心重的人更倾向储备觅食、重人脉者更倾向社交」从纯 prompt 措辞落成可量化的机械打分偏置，且为 HOT-LLM
// 路径提供「候选日常与倾向分」上下文（见 ambient_llm.go 的 prompt 注入）。
//
// ⚠️ 跨包约束（为何打分逻辑在本包内自包含、不 import session）：session 的既有测试 ambient_scheduling_test.go
// **import 了 regionrunner**（构造 Runner）；若 regionrunner（非测试）反向 import session，会在 session 测试二进制里
// 形成 import cycle（go 拒绝编译）。故本文件**镜像** session/ambition_scoring.go 的 ambition 打分纯函数（同常量、同维度→
// 标签映射、同 flag），二者语义逐位对齐——session 侧（AmbientBaseScore/ActionAmbitionTagForAmbient/AmbitionActionWeight）
// 仍导出并独立测试作为权威参考；本镜像有专门的「与 session 对齐」单测钉死，任一侧改动须同步另一侧。
//
// 强不变量：**纯确定性、零 LLM、零 rand**——只读 record 的 hunger/morale/memory/ambition，同输入恒同输出（可复现）。
// 付费不进：打分只聚合生理/情绪/记忆/野心，绝不读 wallet/billing（反 P2W 红线）。flag 行为：野心乘权由
// QUNXIANG_AMBITION_SCORING 控（默认关 → 乘权恒 1.0 → 候选首选退化为与 legacy 反射层 decideAmbientReflex 逐位一致的
// 纯需求强度序，对既有行为零影响）。整条链只在 region-runner flag（QUNXIANG_REGION_RUNNER_*）开时才被 chooseAmbientAction 触达。

import (
	"sort"
	"strings"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
)

// —— 野心打分常量（镜像 session/ambition.go 的 ambitionFloor/ambitionGain，须与之逐位一致）。
const (
	ambitionFloor = 1.0 // 野心引力归一化下限（无野心/中性 → 恒此值，对打分零放大）
	ambitionGain  = 0.6 // 某维野心=1 时对应标签引力达 floor+gain == 1.6
)

// ambitionScoringFlagEnv 是野心乘权灰度开关（镜像 session 的同名 flag，默认关 → 乘权恒中性 1.0）。
const ambitionScoringFlagEnv = "QUNXIANG_AMBITION_SCORING"

// 野心六维 → 受其驱动的自发行为标签（镜像 session/ambition.go 的 ambitionDimensionTags）。
var ambitionDimensionTags = map[string][]string{
	"power":     {"conquer", "bond"},
	"vengeance": {"revenge", "conquer"},
	"wealth":    {"hoard"},
	"lineage":   {"nurture", "bond"},
	"mastery":   {"train", "explore"},
	"freedom":   {"explore"},
}

// ambient 离线日常动作 → 野心标签映射（镜像 session 的 ambientActionAmbitionTagTable）。
// forage→hoard（储备同源敛财）、socialize→bond（人脉）、reflect→explore（精神求索）；rest 不进表 → 中性。
var ambientActionTagTable = map[ambientAction]string{
	actForage:    "hoard",
	actSocialize: "bond",
	actReflect:   "explore",
	// actRest 故意不登记 → 中性 1.0
}

// —— 需求强度基础分常量（镜像 session 的 ambientBaseFloor/Ceil/NeedStrong/NeutralBase）。
const (
	ambientBaseFloor   = 0.5 // 未触发需求阈值的动作底分
	ambientBaseCeil    = 1.0 // 已越阈值的主动响应需求上限
	ambientNeedStrong  = 0.9 // 强需求（已越阈值：饿→觅食 / 低落→社交 / 满足→反思）
	ambientNeutralBase = 0.6 // 中性兜底（rest——无需求驱动时的自然首选，= legacy 反射「其余→休息」）
)

// L3 偏置（近期记忆触发）的乘权常量。
const (
	memoryBiasMultiplier  = 1.15 // 近期记忆命中某动作语义 → 该候选 ×此值（轻微强化，确定性）
	candidateScoreEpsilon = 1e-9 // 平手判定容差（浮点比较，避免等分时排序抖动）
)

// 各 ambient 动作的「记忆触发关键词」——近期记忆 highlight 含任一关键词 → 该动作受 L3 偏置抬升。纯查表、确定性。
var ambientMemoryTriggerKeywords = map[ambientAction][]string{
	actForage:    {"觅食", "采集", "饥饿", "食物", "储备"},
	actSocialize: {"社交", "攀谈", "结识", "盟友", "倾诉", "孤独"},
	actReflect:   {"反思", "沉淀", "独处", "顿悟", "回想"},
	actRest:      {"休息", "疲惫", "恢复", "歇息"},
}

// ambientCandidate 是一个带分的离线日常候选动作（动机栈打分的中间产物）。
type ambientCandidate struct {
	action     ambientAction
	baseScore  float64 // L1/L3 后的需求强度基础分（野心乘权前）
	ambitionW  float64 // 野心引力乘权（flag 关恒 1.0）
	finalScore float64 // baseScore × ambitionW，候选排序键
}

// decideAmbientContextual 是动机栈候选生成 + 确定性打分的入口：
//
//	L1 安全护栏（hunger<阈值 → 强制 forage 并 early-return；morale 高低决定加 socialize/reflect；否则 rest）
//	→ 各候选 ambientBaseScore（需求强度）→ L3 偏置（记忆命中 → ×1.15）
//	→ 野心乘权（finalScore = baseScore × ambitionActionWeight(record, tag)）
//	→ 按 finalScore 降序（平手按动作名字典序，确定性），至少留一候选。
//
// 返回非空候选切片（首元素即「最强渴望」），供 chooseAmbientAction 取首选 / ambient_llm.go 注入 prompt。
// 纯确定性、零 LLM、零 rand、付费不进。
func decideAmbientContextual(record unit.Record) []ambientCandidate {
	// —— L1 安全护栏：生理硬需求优先，直接 early-return（饿到阈值以下，一切让位于觅食）。
	if record.Status.Hunger < forageThreshold {
		return []ambientCandidate{scoreCandidate(record, actForage)}
	}

	// —— L1/L3：按士气状态组装候选集（rest 永远在场，作为安全底）。
	candidateActions := []ambientAction{actRest}
	switch {
	case record.Status.Morale < moraleLow:
		candidateActions = append(candidateActions, actSocialize) // 低落 → 社交舒展
	case record.Status.Morale > moraleHigh:
		candidateActions = append(candidateActions, actReflect) // 满足 → 独处沉淀
	default:
		// 中间态：社交与反思都进候选（让野心/记忆偏置在打分层决定倾向，而非在此硬选）。
		candidateActions = append(candidateActions, actSocialize, actReflect)
	}
	// 不饿但允许「储备型觅食」进候选——基础分因不饿而低，靠野心/记忆才可能压过 rest。
	candidateActions = append(candidateActions, actForage)
	candidateActions = dedupeActions(candidateActions)

	candidates := make([]ambientCandidate, 0, len(candidateActions))
	for _, act := range candidateActions {
		candidates = append(candidates, scoreCandidate(record, act))
	}
	sortCandidates(candidates)
	if len(candidates) == 0 { // 理论不可达（rest 恒在）；失败安全兜底 rest。
		return []ambientCandidate{scoreCandidate(record, actRest)}
	}
	return candidates
}

// scoreCandidate 给单个候选动作算 (baseScore, ambitionW, finalScore)。纯确定性，只读 record。
func scoreCandidate(record unit.Record, action ambientAction) ambientCandidate {
	base := ambientBaseScore(record, action)
	base *= memoryBias(record, action)
	ambitionW := ambitionActionWeight(record, action)
	return ambientCandidate{
		action:     action,
		baseScore:  base,
		ambitionW:  ambitionW,
		finalScore: base * ambitionW,
	}
}

// ambientBaseScore 算需求强度基础分（镜像 session.AmbientBaseScore，须逐位一致）：
//   - 触发阈值已跨过（饿→forage / 低落→socialize / 满足→reflect）→ ambientNeedStrong(0.9)，必压过 rest 中性，
//     故首选与 legacy 反射 decideAmbientReflex 逐位一致。
//   - rest → ambientNeutralBase(0.6)：无需求驱动时的自然首选（= legacy「其余→休息」）。
//   - 其余未触发动作 → ambientBaseFloor(0.5)：候选保留但分低，唯有野心/记忆偏置才可能压过 rest（野心消费入口）。
func ambientBaseScore(record unit.Record, action ambientAction) float64 {
	hunger := record.Status.Hunger
	morale := record.Status.Morale
	var score float64
	switch action {
	case actForage:
		if hunger < forageThreshold {
			score = ambientNeedStrong
		} else {
			score = ambientBaseFloor
		}
	case actSocialize:
		if morale < moraleLow {
			score = ambientNeedStrong
		} else {
			score = ambientBaseFloor
		}
	case actReflect:
		if morale > moraleHigh {
			score = ambientNeedStrong
		} else {
			score = ambientBaseFloor
		}
	case actRest:
		score = ambientNeutralBase
	default:
		score = ambientBaseFloor
	}
	return clampScore(score, ambientBaseFloor, ambientBaseCeil)
}

// ambitionActionWeight 是野心乘权因子（镜像 session.AmbitionActionWeight + ambitionBias，须逐位一致）：
// flag QUNXIANG_AMBITION_SCORING 关（默认）→ 恒 ambitionFloor(1.0,中性,零影响)；
// rest（无野心标签）→ 恒中性；开 → 该单位野心对动作标签的引力 [1.0,1.6]。纯函数、确定性、付费不进。
func ambitionActionWeight(record unit.Record, action ambientAction) float64 {
	tag := ambientActionTagTable[action]
	if !ambitionScoringEnabled() || tag == "" {
		return ambitionFloor
	}
	return ambitionBiasForTag(record.Ambition, tag)
}

// ambitionBiasForTag 取某行为标签在该单位六维野心下的引力权重（镜像 session.ambitionBias + BiasFor）。
// 某标签被多维野心驱动时取最强驱动（最大引力，不叠加致越界）；无驱动标签 → ambitionFloor(中性)。
func ambitionBiasForTag(amb unit.Ambition, tag string) float64 {
	dims := map[string]float64{
		"power":     clamp01(amb.Power),
		"vengeance": clamp01(amb.Vengeance),
		"wealth":    clamp01(amb.Wealth),
		"lineage":   clamp01(amb.Lineage),
		"mastery":   clamp01(amb.Mastery),
		"freedom":   clamp01(amb.Freedom),
	}
	best := ambitionFloor
	for dim, tags := range ambitionDimensionTags {
		value := dims[dim]
		if value <= 0 {
			continue
		}
		for _, t := range tags {
			if t != tag {
				continue
			}
			if w := ambitionFloor + ambitionGain*value; w > best {
				best = w
			}
		}
	}
	return best
}

// dominantAmbitionTag 返回该单位引力最高的野心标签（镜像 session AmbitionBias.Dominant 的语义：平手取字典序最小，
// 确定性）；无明显野心 / flag 关时返回 ""（调用方不注入主导渴望）。供 ambient_llm.go 的 prompt 主导渴望段使用。
func dominantAmbitionTag(record unit.Record) string {
	if !ambitionScoringEnabled() {
		return ""
	}
	amb := record.Ambition
	dims := map[string]float64{
		"power":     clamp01(amb.Power),
		"vengeance": clamp01(amb.Vengeance),
		"wealth":    clamp01(amb.Wealth),
		"lineage":   clamp01(amb.Lineage),
		"mastery":   clamp01(amb.Mastery),
		"freedom":   clamp01(amb.Freedom),
	}
	bestTag := ""
	bestWeight := ambitionFloor
	// 收集所有标签的最大引力，再取最高（平手字典序最小）——与 session 的 Dominant 一致。
	tagWeight := make(map[string]float64)
	for dim, tags := range ambitionDimensionTags {
		value := dims[dim]
		if value <= 0 {
			continue
		}
		w := ambitionFloor + ambitionGain*value
		for _, t := range tags {
			if existing, ok := tagWeight[t]; !ok || w > existing {
				tagWeight[t] = w
			}
		}
	}
	for tag, w := range tagWeight {
		if bestTag == "" || w > bestWeight || (w == bestWeight && tag < bestTag) {
			bestTag = tag
			bestWeight = w
		}
	}
	return bestTag
}

// memoryBias 是 L3 偏置：近期记忆 highlight 命中某动作语义关键词 → memoryBiasMultiplier（轻微强化），否则 1.0。
func memoryBias(record unit.Record, action ambientAction) float64 {
	keywords := ambientMemoryTriggerKeywords[action]
	if len(keywords) == 0 {
		return 1.0
	}
	for _, line := range unit.RecentHighlights(record, 6) {
		for _, kw := range keywords {
			if strings.Contains(line, kw) {
				return memoryBiasMultiplier
			}
		}
	}
	return 1.0
}

// topAmbientAction 取候选列表里 finalScore 最高的动作（列表已降序，取首元素）。空列表兜底 rest（失败安全）。
func topAmbientAction(candidates []ambientCandidate) ambientAction {
	if len(candidates) == 0 {
		return actRest
	}
	return candidates[0].action
}

// sortCandidates 按 finalScore 降序；平手（差 < epsilon）按动作名字典序升序——完全确定性（不依赖输入顺序/map 迭代序）。
func sortCandidates(candidates []ambientCandidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		di := candidates[i].finalScore - candidates[j].finalScore
		if di > candidateScoreEpsilon {
			return true
		}
		if di < -candidateScoreEpsilon {
			return false
		}
		return candidates[i].action < candidates[j].action
	})
}

// dedupeActions 去重保序（候选集组装时可能重复 append 同一动作）。
func dedupeActions(actions []ambientAction) []ambientAction {
	seen := make(map[ambientAction]struct{}, len(actions))
	out := make([]ambientAction, 0, len(actions))
	for _, a := range actions {
		if _, dup := seen[a]; dup {
			continue
		}
		seen[a] = struct{}{}
		out = append(out, a)
	}
	return out
}

// ambitionScoringEnabled 读 QUNXIANG_AMBITION_SCORING（默认关），与 session 侧解析逐位一致。
// 走 featureflags.EnvOrOverride：GM 后台对该 flag 的运行时 override 在离线 region-runner 侧也生效。
func ambitionScoringEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride(ambitionScoringFlagEnv))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// clamp01 把 v 夹到 [0,1]（野心分量归一）。
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// clampScore 把 v 夹到 [lo,hi]。
func clampScore(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
