package session

// 文件说明：六维野心进「行为候选机械打分」的乘权桥（沙盘 §8.2「结算/打分用代码」）。ambition.go 已把六维野心
// （power/vengeance/wealth/lineage/mastery/freedom）翻成行为标签引力 AmbitionBias，但 BiasFor 此前**零调用方**——
// 野心只通过 prompt 措辞影响 LLM 文本、从不进任何确定性打分，故同样野心的两个候选动作在机械层完全等权。本文件补上
// 「候选动作语义 → 野心标签 → 引力乘权」这条确定性链：调用方对候选动作打分时 finalScore = baseScore *
// ambitionActionWeight(record, actionAmbitionTag(action))，让「复仇心重的人更倾向攻伐、敛财者更倾向逐利」从纯措辞
// 落成可量化的打分偏置。
//
// flag QUNXIANG_AMBITION_SCORING **默认关**：机械乘权直接改候选排序、有平衡回归风险（如野心强单位过度集中于单一行为、
// 削弱多样性），保守起见默认关 → ambitionActionWeight 恒返回 1.0（中性，对既有打分零影响、与当前线上行为逐位一致），
// 仅在显式置 flag 后才灰度启用。纯函数、确定性（只读 record.Ambition，不引随机/时间）、零副作用、零 LLM。

import (
	"os"
	"strings"

	"qunxiang/backend/internal/unit"
)

// ambitionScoringFlagEnv 是本特性的灰度开关环境变量名。默认关 → 乘权恒 1.0（中性）。
// 自包含解析（对齐 ambient_scheduling.go / billing / compliance 的 flag idiom），不引入额外依赖。
const ambitionScoringFlagEnv = "QUNXIANG_AMBITION_SCORING"

// ambitionScoringEnabled 读 QUNXIANG_AMBITION_SCORING（默认关）。开时 ambitionActionWeight 才返回真实野心引力。
// 每次调用现读环境变量——本桥的调用频率（候选打分级）远低于热路径，且便于灰度期动态切换/测试覆盖；
// 若未来上到极热路径可改为建局时一次性快照。
func ambitionScoringEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(ambitionScoringFlagEnv))) {
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
