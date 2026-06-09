package session

// 文件说明：六维野心进「行为候选机械打分」的乘权桥（沙盘 §8.2「结算/打分用代码」）。ambition.go 已把六维野心
// （power/vengeance/wealth/lineage/mastery/freedom）翻成行为标签引力 AmbitionBias，但 BiasFor 此前**零调用方**——
// 野心只通过 prompt 措辞影响 LLM 文本、从不进任何确定性打分，故同样野心的两个候选动作在机械层完全等权。本文件补上
// 「候选动作语义 → 野心标签 → 引力乘权」这条确定性链：调用方对候选动作打分时 finalScore = baseScore *
// ambitionActionWeight(record, actionAmbitionTag(action))，让「复仇心重的人更倾向攻伐、敛财者更倾向逐利」从纯措辞
// 落成可量化的打分偏置。
//
// 本文件覆盖三条消费路径，标签表三表解耦、互不污染：①离线 regionrunner ambient（ambientActionAmbitionTagTable）；
// ②战斗/社交候选动词（actionAmbitionTagTable）；③**在线**执行阶段决策（onlineActionAmbitionTagTable + 文件末尾的
// OnlineActionAmbitionTag / OnlineAmbitionActionWeight / PickAmbitionBiasedCandidate）。其中 ③ 补上此前的缺口：
// 在线决策的规则 fallback（LLM 不可用/超时/解析失败时的候选挑选）此前零野心打分调用方，野心在线只进 prompt、不进
// 任何确定性候选排序——现由 llm.go 在多等价候选间调 PickAmbitionBiasedCandidate 做野心 tie-break（flag 关时不重排）。
//
// flag QUNXIANG_AMBITION_SCORING **默认关**：机械乘权直接改候选排序、有平衡回归风险（如野心强单位过度集中于单一行为、
// 削弱多样性），保守起见默认关 → ambitionActionWeight 恒返回 1.0（中性，对既有打分零影响、与当前线上行为逐位一致），
// 仅在显式置 flag 后才灰度启用。纯函数、确定性（只读 record.Ambition，不引随机/时间）、零副作用、零 LLM。

import (
	"strings"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/unit"
)

// ambitionScoringFlagEnv 是本特性的灰度开关环境变量名。默认关 → 乘权恒 1.0（中性）。
// 自包含解析（对齐 ambient_scheduling.go / billing / compliance 的 flag idiom），不引入额外依赖。
const ambitionScoringFlagEnv = "QUNXIANG_AMBITION_SCORING"

// ambitionScoringEnabled 读 QUNXIANG_AMBITION_SCORING（默认关）。开时 ambitionActionWeight 才返回真实野心引力。
// 每次调用现读环境变量——本桥的调用频率（候选打分级）远低于热路径，且便于灰度期动态切换/测试覆盖；
// 若未来上到极热路径可改为建局时一次性快照。
func ambitionScoringEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride(ambitionScoringFlagEnv))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// 候选动作语义 → 野心标签映射表。key 是候选动作的语义动词（调用方传入的动作名，统一小写匹配，做了别名归一），
// value 是 ambition.go 已定义的七个行为标签之一（conquer/revenge/hoard/nurture/train/explore/bond）。
// 设计要点：
//   - 只映射「带野心语义」的动作；纯生存/中性动作（如 forage 觅食、rest 休息）**不进表** → 返回 "" → 按 1.0 中性，
//     绝不让野心污染与野心无关的生存决策（觅食是饿了就吃，与「敛财野心」无关）。
//   - 一个动作只归一个最贴切的标签（取主导语义）；标签内部的多维叠加由 ambition.go 的 ambitionBias 取最大引力处理。
//   - 未知 / 未登记动作 → "" → 中性 1.0（失败安全：不认识的动作绝不被野心放大或抑制）。
var actionAmbitionTagTable = map[string]string{
	// conquer 攻伐扩张 / 夺权上位
	"attack":  "conquer",
	"assault": "conquer",
	"conquer": "conquer",
	"raid":    "conquer",
	"seize":   "conquer",
	"siege":   "conquer",
	// revenge 复仇雪耻
	"revenge":   "revenge",
	"avenge":    "revenge",
	"retaliate": "revenge",
	// hoard 敛财囤积 / 经商逐利
	"gather": "hoard",
	"trade":  "hoard",
	"loot":   "hoard",
	"hoard":  "hoard",
	"sell":   "hoard",
	"buy":    "hoard",
	// nurture 养育血脉 / 联姻成家
	"romance": "nurture",
	"family":  "nurture",
	"marry":   "nurture",
	"nurture": "nurture",
	"care":    "nurture",
	// train 精进技艺 / 钻研学问
	"train":    "train",
	"forge":    "train",
	"craft":    "train",
	"study":    "train",
	"practice": "train",
	// explore 闯荡远游 / 不受拘束
	"explore":  "explore",
	"move-far": "explore",
	"scout":    "explore",
	"wander":   "explore",
	"travel":   "explore",
	// bond 社交结盟 / 经营人脉
	"dialogue":  "bond",
	"ally":      "bond",
	"socialize": "bond",
	"befriend":  "bond",
	"negotiate": "bond",
}

// actionAmbitionTag 把候选动作的语义动词映射到野心行为标签；未知 / 中性动作返回 ""（调用方按中性 1.0 处理）。
// 大小写与首尾空白不敏感（统一归一后查表），便于不同调用方传入未规整的动作名。纯函数、确定性。
func actionAmbitionTag(action string) string {
	key := strings.ToLower(strings.TrimSpace(action))
	if key == "" {
		return ""
	}
	if tag, ok := actionAmbitionTagTable[key]; ok {
		return tag
	}
	return ""
}

// ambitionActionWeight 是候选打分的野心乘权因子，供调用方乘进既有 baseScore：
//
//	finalScore := baseScore * service.ambitionActionWeight(record, actionAmbitionTag(action))
//
// flag QUNXIANG_AMBITION_SCORING 关（默认）→ 恒返回 1.0（中性，对既有打分零影响）。
// flag 开 → 返回该单位六维野心对该动作标签的引力权重 AmbitionBiasOf(record).BiasFor(actionTag)，落在
// [ambitionFloor, ambitionFloor+ambitionGain] == [1.0, 1.6]：野心越强的标签乘得越高（更倾向该类行为），
// 无对应野心的标签 / 中性动作（actionTag==""）恒 1.0（不放大也不抑制）。
// 纯函数、确定性、零副作用——可在候选打分时直接调用。service 接收者仅为与 session 既有打分 API 风格一致、
// 便于未来按局快照 flag（当前不读 service 任何状态）。
func (service *Service) ambitionActionWeight(record unit.Record, actionTag string) float64 {
	// flag 关 → 中性 1.0（零影响，与当前线上行为逐位一致）。actionTag 空（中性/未知动作）→ 也恒中性。
	if !ambitionScoringEnabled() || actionTag == "" {
		return ambitionFloor
	}
	return AmbitionBiasOf(record).BiasFor(actionTag)
}

// AmbitionActionWeight 是 ambitionActionWeight 的导出无接收者版本，供**别的包**（如 regionrunner 离线自治）在
// 候选打分时调用——离线 runner 无 *Service（跨会话、无 session State），故需一条不依赖 service 状态的纯函数桥。
// 语义与 (*Service).ambitionActionWeight 完全一致：flag QUNXIANG_AMBITION_SCORING 关（默认）→ 恒 ambitionFloor(1.0,
// 中性，零影响)；开 → 返回该单位野心对 actionTag 的引力 [1.0,1.6]。纯函数、确定性、零副作用、零 LLM、付费不进
// （只读 record.Ambition，与 wallet/billing 无关）。actionTag 空（中性/未知动作）→ 恒 1.0。
func AmbitionActionWeight(record unit.Record, actionTag string) float64 {
	if !ambitionScoringEnabled() || actionTag == "" {
		return ambitionFloor
	}
	return AmbitionBiasOf(record).BiasFor(actionTag)
}

// —— 在线决策侧：野心进「执行阶段在线决策」的乘权桥 ——————————————————————————————————————
//
// 缺口背景：(*Service).ambitionActionWeight / AmbitionActionWeight 此前仅被离线 regionrunner 消费；执行阶段的在线
// 决策（generateUnitDecision 的规则 fallback / 抗命权衡）从不调用任何野心打分——野心在线只是 prompt 措辞、不进
// 任何确定性候选排序，故「复仇心重的人更倾向攻伐」在 LLM 不可用/超时/解析失败时（走规则 fallback）完全失效。
//
// 下面三个导出 API 把野心接进在线决策：
//   - OnlineActionAmbitionTag：在线 DecisionAction → 野心标签（在线动作空间与战斗动词表 actionAmbitionTagTable、
//     ambient 表 ambientActionAmbitionTagTable 均不同，故独立一张表，三表解耦、互不污染）。
//   - OnlineAmbitionActionWeight：在线候选打分的野心乘权因子（flag 关时恒 1.0，与 AmbitionActionWeight 同语义）。
//   - PickAmbitionBiasedCandidate：在「同优先级多候选」时按野心引力挑最贴野心的那个（供 llm.go 的规则 fallback 接入）。
//
// 全部纯函数、确定性、零副作用、零 LLM、付费不进（只读 record.Ambition / candidate.Action，与 wallet/billing 无关）。
// flag QUNXIANG_AMBITION_SCORING 默认关 → 三者均退化为「与当前线上行为逐位一致」的中性行为（恒 1.0 / 不重排）。

// onlineActionAmbitionTagTable 是在线执行阶段 DecisionAction → 野心行为标签的**独立**映射。
// 设计要点（与主表/ambient 表一致的失败安全原则）：
//   - 只映射「带野心语义」的在线动作；纯生存/战术/中性动作（hold 待命 / observe 观察 / defend 防御 / move 移动 /
//     eat 进食 / pickup 拾取 / skill 技能 / assist 协助 / demolish 拆除）**不进表** → 返回 "" → 中性 1.0，
//     绝不让野心污染与野心无关的战术/生存决策（防御是危险了就防，与「复仇野心」无关）。
//   - attack/charge/heavy_attack → conquer（攻伐扩张/夺权上位）；trade → hoard（经商逐利）；
//     gather/forge/upgrade/equip → train（精进技艺/钻研，gather 取「钻研采集」语义而非囤积，与 ambient 的觅食解耦）；
//     build → train（营造钻研）；romance/family → nurture（养育血脉/联姻成家）；say/dialogue → bond（社交结盟/经营人脉）。
//   - 一个动作只归一个最贴切标签（取主导语义）；标签内部多维叠加由 ambition.go 的 ambitionBias 取最大引力处理。
//   - 未登记动作 → "" → 中性 1.0（失败安全：不认识的动作绝不被野心放大或抑制）。
var onlineActionAmbitionTagTable = map[DecisionAction]string{
	DecisionActionAttack:      "conquer",
	DecisionActionCharge:      "conquer",
	DecisionActionHeavyAttack: "conquer",
	DecisionActionTrade:       "hoard",
	DecisionActionGather:      "train",
	DecisionActionForge:       "train",
	DecisionActionUpgrade:     "train",
	DecisionActionEquip:       "train",
	DecisionActionBuild:       "train",
	DecisionActionRomance:     "nurture",
	DecisionActionFamily:      "nurture",
	DecisionActionSay:         "bond",
	DecisionActionDialogue:    "bond",
	// 以下故意不登记 → 中性 1.0（战术/生存/中性，与任何野心无关）：
	//   hold / observe / defend / move / eat / pickup / skill / assist / demolish
}

// OnlineActionAmbitionTag 把在线执行阶段的 DecisionAction 映射到野心行为标签；中性/未登记动作返回 ""（调用方按中性 1.0 处理）。
// 纯函数、确定性、零副作用——供 llm.go 在线规则 fallback / 抗命权衡候选打分调用。
func OnlineActionAmbitionTag(action DecisionAction) string {
	if tag, ok := onlineActionAmbitionTagTable[action]; ok {
		return tag
	}
	return ""
}

// OnlineAmbitionActionWeight 是**在线**决策候选打分的野心乘权因子，供 llm.go 的规则 fallback / 抗命权衡乘进既有 baseScore：
//
//	finalScore := baseScore * OnlineAmbitionActionWeight(record, OnlineActionAmbitionTag(candidate.Action))
//
// 语义与 AmbitionActionWeight 完全一致（在线/离线同一把尺，避免在线离线行为分叉）：
//   - flag QUNXIANG_AMBITION_SCORING 关（默认）→ 恒 ambitionFloor(1.0，中性，对既有在线规则 fallback 零影响、逐位一致)；
//   - flag 开 → 返回该单位六维野心对 actionTag 的引力 [1.0,1.6]（野心越契合该动作乘得越高）；
//   - actionTag 空（中性/未登记动作）→ 恒 1.0（不放大也不抑制）。
//
// 纯函数、确定性、零副作用、零 LLM、付费不进（只读 record.Ambition，与 wallet/billing 无关）。
func OnlineAmbitionActionWeight(record unit.Record, actionTag string) float64 {
	if !ambitionScoringEnabled() || actionTag == "" {
		return ambitionFloor
	}
	return AmbitionBiasOf(record).BiasFor(actionTag)
}

// PickAmbitionBiasedCandidate 在「一组同优先级候选」里按野心引力挑出最贴该单位野心的那一个，供 llm.go 的在线规则
// fallback 在**多个等价候选**之间做确定性 tie-break（如 recommendedDecisionCandidate 末尾的 move/observe/defend/hold
// 兜底集合，或 directive 命中后的同类候选集合）。
//
// 行为契约（向后兼容铁律——flag 关时绝不重排）：
//   - flag QUNXIANG_AMBITION_SCORING 关（默认）→ 直接返回 candidates[0]（= 调用方原有「取首个」语义，逐位一致、零行为变化）；
//   - 空候选集 → (零值, false)；
//   - flag 开 → 返回野心引力最高的候选：score(c) = OnlineAmbitionActionWeight(record, OnlineActionAmbitionTag(c.Action))。
//     平手（含全中性 1.0）取**输入切片中靠前者**（稳定，保留调用方原优先级，仅当后者引力严格更高才改写首选），
//     故确定性、不依赖 map 迭代序，且野心无差别时与「取首个」完全等价。
//
// 纯函数、确定性、零副作用、零 LLM、付费不进。actor 为 nil 时退化为「取首个」（失败安全）。
func PickAmbitionBiasedCandidate(record *unit.Record, candidates []decisionCandidate) (decisionCandidate, bool) {
	if len(candidates) == 0 {
		return decisionCandidate{}, false
	}
	// flag 关 / 无 actor → 取首个（与调用方原语义逐位一致，零行为变化）。
	if record == nil || !ambitionScoringEnabled() {
		return candidates[0], true
	}
	bias := AmbitionBiasOf(*record)
	best := candidates[0]
	bestWeight := bias.BiasFor(OnlineActionAmbitionTag(best.Action))
	for _, candidate := range candidates[1:] {
		weight := bias.BiasFor(OnlineActionAmbitionTag(candidate.Action))
		if weight > bestWeight { // 严格大于才改写 → 平手保留靠前者（稳定 tie-break，确定性）。
			best = candidate
			bestWeight = weight
		}
	}
	return best, true
}

// ambientActionAmbitionTagTable 是 ambient 离线日常动作 → 野心标签的**独立**映射（与战斗/社交候选用的
// actionAmbitionTagTable 解耦：ambient 动作空间只有 forage/rest/socialize/reflect 四个日常动作，语义与战斗动作不同）。
// 设计要点（与主表一致的失败安全原则）：
//   - forage（觅食）→ hoard：囤积/储备食物与「敛财囤积」野心同源（积攒资源的内在驱动），让敛财野心强的人更勤觅食/储备。
//   - socialize（社交）→ bond：找人攀谈结好与「经营人脉」野心同源，让重人脉者更倾向社交。
//   - reflect（独处反思）→ explore：内省/规划远路与「闯荡远游 / 不受拘束」野心同源（精神上的求索），让自由/精进野心者更倾向反思。
//   - rest（休息）**不进表** → 中性：纯生理恢复，与任何野心无关（绝不让野心污染与野心无关的休息决策）。
var ambientActionAmbitionTagTable = map[string]string{
	"forage":    "hoard",
	"socialize": "bond",
	"reflect":   "explore",
	// rest 故意不登记 → 中性 1.0
}

// ActionAmbitionTagForAmbient 把 ambient 离线日常动作映射到野心行为标签；rest / 未知动作返回 ""（调用方按中性 1.0 处理）。
// 大小写与首尾空白不敏感。纯函数、确定性、零副作用——供 regionrunner 离线自治候选打分调用。
func ActionAmbitionTagForAmbient(action string) string {
	key := strings.ToLower(strings.TrimSpace(action))
	if key == "" {
		return ""
	}
	if tag, ok := ambientActionAmbitionTagTable[key]; ok {
		return tag
	}
	return ""
}

// ambient 候选基础分（野心乘权前）的边界与档位常量（落在 [0.5,1.0]，再乘野心引力 [1.0,1.6] → finalScore∈[0.5,1.6]）。
const (
	ambientBaseFloor   = 0.5 // 候选基础分下限（未触发需求阈值的动作底分，保证至少留得住一个候选）
	ambientBaseCeil    = 1.0 // 候选基础分上限（已越阈值的主动响应需求）
	ambientNeedStrong  = 0.9 // 强需求（已越阈值的主动响应：饿了觅食 / 低落社交 / 满足反思）
	ambientNeutralBase = 0.6 // 中性兜底（rest——无需求驱动时的自然首选，等价于 legacy 反射层的「其余→休息」）
)

// AmbientBaseScore 算一个 ambient 候选动作的「需求强度基础分」（野心乘权前），落在 [ambientBaseFloor, ambientBaseCeil]
// == [0.5,1.0]。设计原则——**与 legacy 反射层 decideAmbientReflex 的优先级精确对齐**（向后兼容铁律）：
//   - 某动作的**触发阈值已被跨过**（饿了→forage / 低落→socialize / 满足→reflect）→ ambientNeedStrong(0.9)，
//     必压过 rest 的中性 0.6，故首选与 legacy 反射逐位一致（饿→觅食、低落→社交、满足→反思）。
//   - rest → ambientNeutralBase(0.6)：无需求驱动时的自然首选（= legacy 的「其余→休息」），且永远在场作安全底。
//   - 其余未触发动作 → ambientBaseFloor(0.5)：候选保留但分低（不饿时的储备型觅食、中间态的社交/反思），
//     **唯有野心乘权（×1.0..1.6）或记忆偏置（×1.15）才可能把它压过 rest**——这正是「野心打分消费」的偏移入口。
//
// 故 **野心 flag 关时**（乘权恒 1.0）首选恒等于 legacy 反射；flag 开时才允许内在驱动改写日常倾向。
// **确定性、纯函数、零随机/时间、付费不进**（只读 record.Status 的 hunger/morale，与 wallet/billing 无关）。
//
//	finalScore := AmbientBaseScore(record, action) * AmbitionActionWeight(record, ActionAmbitionTagForAmbient(action))
//
// 参数 hungerThreshold/moraleLow/moraleHigh 由调用方（regionrunner）传入其动机栈阈值，保持单一事实源（runner 改阈值无需改本函数）。
func AmbientBaseScore(record unit.Record, action string, hungerThreshold int, moraleLow, moraleHigh float64) float64 {
	hunger := record.Status.Hunger
	morale := record.Status.Morale
	var score float64
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "forage":
		// 已越饥饿阈值 → 强需求；否则底分（不饿时觅食是储备倾向，靠野心/记忆才可能被抬到首选）。
		if hunger < hungerThreshold {
			score = ambientNeedStrong
		} else {
			score = ambientBaseFloor
		}
	case "socialize":
		// 已低于士气下限 → 强需求（找人攀谈舒展）；否则底分。
		if morale < moraleLow {
			score = ambientNeedStrong
		} else {
			score = ambientBaseFloor
		}
	case "reflect":
		// 心满意足（越上限）→ 强需求（独处沉淀）；否则底分。
		if morale > moraleHigh {
			score = ambientNeedStrong
		} else {
			score = ambientBaseFloor
		}
	case "rest":
		// 纯被动恢复：无需求驱动时的中性首选（= legacy 反射的「其余→休息」），永远可选的安全底。
		score = ambientNeutralBase
	default:
		score = ambientBaseFloor
	}
	// clampFloat 复用 relation.go 的同包工具（夹到 [lo,hi]）。
	return clampFloat(score, ambientBaseFloor, ambientBaseCeil)
}
