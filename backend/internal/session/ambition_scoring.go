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
