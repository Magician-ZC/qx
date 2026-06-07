// Package narration 把确定性留痕（reason-code + 事实摘要）渲染成「祖魂语气」的命运卡 beat。
//
// 设计动机（设计宪法 / 「玩家是否愿意读编年史」）：编年史能不能被读下去，取决于叙事不重复、有温度、有宿命感。
// 本包是纯函数、零 LLM、确定性可复现：同样的输入永远得到同样的 beat；不同事件用种子打散模板，避免千篇一律。
//
// 祖魂 = 一位在香火那头垂看后人命运的先祖；语气克制、宿命、温热而有距离。叙事只「框」事实，不臆造情节。
package narration

import "hash/fnv"

// tone 是 beat 的情感基调，由 reason-code 与情绪效价共同决定。
type tone int

const (
	toneNeutral    tone = iota // 中性陈述
	toneGrave                  // 凶险/创伤/濒死
	toneWarm                   // 好事/奖励/获救
	toneConnective             // 牵动她在乎的人或物
)

// 各基调的祖魂语气模板库；统一为「祖魂框定 + 分隔符 + 事实」的前缀式，%s 始终落在句末，
// 无论事实摘要本身是否已是完整句子都读得自然。多变体由种子打散，保证编年史不重复。
var beatBanks = map[tone][]string{
	toneNeutral: {
		"祖魂垂看着：%s",
		"香火那头，先祖记下了这一笔：%s",
		"先祖的目光落在她身上：%s",
	},
	toneGrave: {
		"祖魂垂看着，眉头紧锁：%s",
		"香火那头传来一声长叹：%s",
		"先祖的目光沉了下来：%s",
		"血脉里的人，正走在刀刃上——%s",
	},
	toneWarm: {
		"祖魂含笑：%s",
		"香火亮了亮：%s",
		"先祖很是欣慰：%s",
		"风里像有谁轻轻应了一声：%s",
	},
	toneConnective: {
		"这事，牵动着她挂心的人：%s",
		"风从她在乎的那个方向吹来：%s",
		"血脉是连着的：%s",
		"她放不下的那些，又起了波澜：%s",
	},
}

// 待决策（升级到命运收件箱、需玩家定夺）的外层包裹模板；%s 落在句末，包裹已框定的 body。
var pendingFrames = []string{
	"有件关乎血脉的事，在等你拿个主意——%s",
	"该由你来定夺了：%s",
	"祖魂不能替她做主，这一程交给你：%s",
}

const fallbackSummary = "她在乎的人那边，出了点事。"

// Beat 把一条事实摘要渲染成祖魂语气的命运卡。
//
//	reasonCode  事件 reason-code（如 EMOTION_TRAUMA / ECONOMY_LOOT / RELEVANCE_MATCH），决定基调倾向。
//	valence     情绪效价：>0 暖、<0 沉、≈0 取决于 reason-code（牵连类→connective，否则中性）。
//	pending     是否升级为待决策（需玩家定夺），决定是否套用 pending 外层。
//	summary     事实摘要（已是一句中文）；为空时用兜底。
//	seed        打散种子（0 时按 summary 派生），保证不同事件取不同变体、可复现。
func Beat(reasonCode string, valence float64, pending bool, summary string, seed uint64) string {
	if summary == "" {
		summary = fallbackSummary
	}
	if seed == 0 {
		seed = hashSeed(reasonCode + "\x00" + summary)
	}
	t := classify(reasonCode, valence)
	bank := beatBanks[t]
	body := sprintf1(bank[seed%uint64(len(bank))], summary)
	if pending {
		return sprintf1(pendingFrames[seed%uint64(len(pendingFrames))], body)
	}
	return body
}

// classify 由 reason-code 与效价定基调：效价优先（暖/沉），否则按 reason-code 归类牵连/中性。
func classify(reasonCode string, valence float64) tone {
	if valence > 0.15 {
		return toneWarm
	}
	if valence < -0.15 {
		return toneGrave
	}
	switch reasonCode {
	case "COMBAT_DOWN", "EMOTION_TRAUMA", "RELATION_BETRAYAL", "COMBAT_HIT", "SURVIVAL_HUNGER", "SURVIVAL_MARCH_EXHAUST":
		return toneGrave
	case "EMOTION_REWARD", "ECONOMY_LOOT", "ECONOMY_REWARD", "RELATION_RESCUED":
		return toneWarm
	case "RELEVANCE_MATCH", "INBOX_HIGHLIGHT", "PENDING_DECISION":
		return toneConnective
	default:
		return toneNeutral
	}
}

func hashSeed(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	v := h.Sum64()
	if v == 0 {
		return 1 // 0 是「未指定」的哨兵，避免误触
	}
	return v
}

// sprintf1 等价于 fmt.Sprintf(tmpl, arg)，但只支持单个 %s，避免引入 fmt 依赖与额外分配语义。
func sprintf1(tmpl string, arg string) string {
	for i := 0; i+1 < len(tmpl); i++ {
		if tmpl[i] == '%' && tmpl[i+1] == 's' {
			return tmpl[:i] + arg + tmpl[i+2:]
		}
	}
	return tmpl
}
