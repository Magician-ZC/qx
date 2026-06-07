package decision

// 文件说明：「意外但合理」的代码强制（设计宪法 §5、设计原则 4 的工程落地）。
// 每个自治选择必须随意图产出结构化「归因」——指向真实存在的前因（人格/记忆/红线/关系/现实压力/玩家回响）。
// 归因校验器纯代码、零 LLM：任一规则不过即判 OOC（无源戏剧性意外），调用方应回退到 safeFallback。
// 另含「门控意外表」：突然恋爱/卖传家宝/叛变这类高代价转折，没有专属前因时连选项都不生成。
// 这让「她做了 X，因为 Y」中的 Y 必须是数据里真实存在的因，而不是 LLM 觉得有戏剧性。

import "strings"

// CauseKind 是归因前因的六个合法类别。
type CauseKind string

const (
	CausePersonaTrait CauseKind = "persona_trait" // 8 维人格 | 6 维野心
	CauseMemory       CauseKind = "memory"        // 记忆条目 ID
	CauseRedline      CauseKind = "redline"        // 离线宪章红线条目 ID
	CauseRelation     CauseKind = "relation"       // 目标 unitID + 关系四轴
	CausePressure     CauseKind = "pressure"       // 现实压力位（饥饿/威胁/债务/受伤/疲劳）
	CauseOrderEcho    CauseKind = "order_echo"     // 上一条玩家 directive/接管 event（回响用）
)

// CauseRef 是一条归因前因引用。
type CauseRef struct {
	Kind      CauseKind `json:"kind"`
	RefID     string    `json:"ref_id"`
	Weight    float64   `json:"weight"` // 0–1
	SnippetZH string    `json:"snippet_zh,omitempty"`
}

// Attribution 随决断层意图一并产出，是「为什么她要这么做」的可校验凭证。
type Attribution struct {
	Primary       CauseRef   `json:"primary"`
	Supporting    []CauseRef `json:"supporting,omitempty"`
	SurpriseLevel int        `json:"surprise_level"` // 0–3，越高越「意外」
	NarrativeZH   string     `json:"narrative_zh"`   // ≤40 字给玩家看的因果句
}

// MemoryMeta 是单条记忆的可校验元数据。
type MemoryMeta struct {
	Importance    int
	EmotionWeight float64
	Salience      float64
	Summary       string
}

// RelationAxes 是对某目标的关系四轴。
type RelationAxes struct{ Trust, Affection, Fear, Loyalty float64 }

// PressureFlags 是当前是否越过 L1 护栏阈值的压力位。
type PressureFlags struct{ Hunger, Threat, Debt, Injury, Fatigue bool }

// Snapshot 是校验所需的「角色当前可解析前因」集合，纯数据，由 session 层从单位快照构造。
type Snapshot struct {
	Traits               map[string]float64    // 人格/野心维 → 当前值[0,1]
	Memories             map[string]MemoryMeta // 记忆 ID → 元数据
	Redlines             map[string]string     // 红线条目 ID → 原文
	Relations            map[string]RelationAxes
	Pressure             PressureFlags
	PlayerActionEventIDs map[string]struct{} // 可被 order_echo 引用的玩家动作 event
}

// 校验阈值（与设计宪法 §5.1 一致）。
const (
	traitSignificance = 0.25 // |value-0.5| ≥ 此值才算显著
	memImportanceMin  = 6
	memEmotionMin     = 0.5
	memSalienceMin    = 0.15
	relAxisMin        = 0.3
	strongCauseWeight = 0.5
)

// OOC 判定原因码。
const (
	OOCNone              = ""
	OOCNarrativeEmpty    = "NARRATIVE_EMPTY"
	OOCCauseNotFound     = "CAUSE_NOT_FOUND"
	OOCCauseTooWeak      = "CAUSE_TOO_WEAK"
	OOCSurpriseUngrounded = "SURPRISE_UNGROUNDED"
	OOCSnippetFabricated = "SNIPPET_FABRICATED"
)

// Verdict 是归因校验结论。
type Verdict struct {
	OK     bool
	Reason string // OOCNone 表示通过，否则为某条 OOC 原因码
}

// ValidateAttribution 对归因做硬判定，任一不过即判 OOC（调用方应回退 safeFallback 并写审计事件）。
func ValidateAttribution(attr Attribution, snap Snapshot) Verdict {
	if strings.TrimSpace(attr.NarrativeZH) == "" {
		return Verdict{false, OOCNarrativeEmpty}
	}
	// 规则 1：primary 必须解析成功。
	if !causeResolves(attr.Primary, snap) {
		return Verdict{false, OOCCauseNotFound}
	}
	// 规则 2：primary 必须越过显著阈值。
	if !causeSignificant(attr.Primary, snap) {
		return Verdict{false, OOCCauseTooWeak}
	}
	// 规则 3：明显意外(≥2)必须有一条强的 memory/relation/redline 前因支撑（纯 persona 不够）。
	if attr.SurpriseLevel >= 2 && !hasStrongGrounding(attr, snap) {
		return Verdict{false, OOCSurpriseUngrounded}
	}
	// 规则 5：memory/redline 的 primary 其 snippet 必须与真实文本存在性重合（防编造因果句）。
	if !snippetGrounded(attr.Primary, snap) {
		return Verdict{false, OOCSnippetFabricated}
	}
	return Verdict{true, OOCNone}
}

// causeResolves 判断该前因引用是否指向一个真实存在的记录。
func causeResolves(c CauseRef, snap Snapshot) bool {
	switch c.Kind {
	case CausePersonaTrait:
		_, ok := snap.Traits[c.RefID]
		return ok
	case CauseMemory:
		_, ok := snap.Memories[c.RefID]
		return ok
	case CauseRedline:
		_, ok := snap.Redlines[c.RefID]
		return ok
	case CauseRelation:
		_, ok := snap.Relations[c.RefID]
		return ok
	case CausePressure:
		return isKnownPressure(c.RefID)
	case CauseOrderEcho:
		_, ok := snap.PlayerActionEventIDs[c.RefID]
		return ok
	default:
		return false
	}
}

// causeSignificant 判断该前因是否足够「显著」可作为因（解析成功的前提下）。
func causeSignificant(c CauseRef, snap Snapshot) bool {
	switch c.Kind {
	case CausePersonaTrait:
		v := snap.Traits[c.RefID]
		return absf(v-0.5) >= traitSignificance
	case CauseMemory:
		m := snap.Memories[c.RefID]
		strongEnough := m.Importance >= memImportanceMin || absf(m.EmotionWeight) >= memEmotionMin
		return strongEnough && m.Salience > memSalienceMin
	case CauseRedline:
		return true // 红线存在即显著
	case CauseRelation:
		r := snap.Relations[c.RefID]
		return absf(r.Trust) >= relAxisMin || absf(r.Affection) >= relAxisMin ||
			absf(r.Fear) >= relAxisMin || absf(r.Loyalty) >= relAxisMin
	case CausePressure:
		return pressureActive(c.RefID, snap.Pressure)
	case CauseOrderEcho:
		return true // 已解析即有效
	default:
		return false
	}
}

// hasStrongGrounding 判断归因里是否存在一条「强的」memory/relation/redline 前因。
func hasStrongGrounding(attr Attribution, snap Snapshot) bool {
	all := append([]CauseRef{attr.Primary}, attr.Supporting...)
	for _, c := range all {
		switch c.Kind {
		case CauseMemory, CauseRelation, CauseRedline:
			if c.Weight >= strongCauseWeight && causeResolves(c, snap) && causeSignificant(c, snap) {
				return true
			}
		}
	}
	return false
}

// snippetGrounded 对 memory/redline 的 primary 做「文本存在性」校验，防 LLM 编造因果句。
func snippetGrounded(c CauseRef, snap Snapshot) bool {
	var text string
	switch c.Kind {
	case CauseMemory:
		text = snap.Memories[c.RefID].Summary
	case CauseRedline:
		text = snap.Redlines[c.RefID]
	default:
		return true // 其它类别不做 snippet 校验
	}
	snippet := normalizeText(c.SnippetZH)
	full := normalizeText(text)
	if snippet == "" || full == "" {
		return false
	}
	// 双向存在性：snippet 是原文子串，或原文是 snippet 子串（容许规范化摘要）。
	return strings.Contains(full, snippet) || strings.Contains(snippet, full)
}

// --- 门控意外表（设计宪法 §5.3）---

// GatedAction 是需要专属前因才能发生的高代价转折动作。
type GatedAction string

const (
	ActionRomance    GatedAction = "romance"     // 表白/接受恋情
	ActionSellPinned GatedAction = "sell_pinned" // 卖/赠传家宝
	ActionDefect     GatedAction = "defect"      // 叛变/投敌
)

// GateDecision 是门控裁决。
type GateDecision string

const (
	GateAllow  GateDecision = "allow"  // 允许（自治或进候选池）
	GateFreeze GateDecision = "freeze" // 必须上交玩家（PENDING_DECISION），绝不自治
	GateReject GateDecision = "reject" // 前因不足，从候选中剔除
)

// GateResult 是门控结果。
type GateResult struct {
	Decision GateDecision
	Reason   string
}

// GateInput 是门控判定所需的角色侧前因（由 session 层填充）。
type GateInput struct {
	// romance
	TargetAffection     float64
	RelationMemoryCount int
	AccumulatedWindows  int // 与目标累积互动的自治窗口数
	// sell_pinned
	ItemIsPermanentAnchor bool // 父辈遗志类，绝不可卖
	HasDebtPressure       bool
	HasThreatPressure     bool
	// defect
	Loyalty                   float64
	LoyaltyThreshold          float64 // ≤0 时取默认 0.4
	HasNegativeMemory         bool
	HasRelationDecay          bool
	HasFactionDeclinePressure bool
}

// GateSurprise 对门控动作判定是否允许、需上交玩家、还是剔除。非门控动作一律放行。
func GateSurprise(action GatedAction, in GateInput) GateResult {
	switch action {
	case ActionRomance:
		// 需对目标已有 ≥2 条关系记忆 + affection≥0.4 + 跨 ≥3 个窗口累积，防一面之缘就告白。
		if in.TargetAffection >= 0.4 && in.RelationMemoryCount >= 2 && in.AccumulatedWindows >= 3 {
			return GateResult{GateAllow, ""}
		}
		return GateResult{GateReject, "ROMANCE_NO_PRIOR"}
	case ActionSellPinned:
		// 父辈遗志类绝不可卖。
		if in.ItemIsPermanentAnchor {
			return GateResult{GateReject, "PINNED_PERMANENT"}
		}
		// 有债务/威胁压力 → 可在重压下自治变卖；否则上交玩家，绝不自治。
		if in.HasDebtPressure || in.HasThreatPressure {
			return GateResult{GateAllow, ""}
		}
		return GateResult{GateFreeze, "SELL_PINNED_NEEDS_PLAYER"}
	case ActionDefect:
		threshold := in.LoyaltyThreshold
		if threshold <= 0 {
			threshold = 0.4
		}
		// 忠诚低 + 至少一条（负面记忆/关系恶化/势力衰退压力）才允许，纯野心高不够。
		if in.Loyalty < threshold && (in.HasNegativeMemory || in.HasRelationDecay || in.HasFactionDeclinePressure) {
			return GateResult{GateAllow, ""}
		}
		return GateResult{GateReject, "DEFECT_UNGROUNDED"}
	default:
		return GateResult{GateAllow, ""}
	}
}

// --- 工具 ---

func isKnownPressure(name string) bool {
	switch name {
	case "hunger", "threat", "debt", "injury", "fatigue":
		return true
	default:
		return false
	}
}

func pressureActive(name string, p PressureFlags) bool {
	switch name {
	case "hunger":
		return p.Hunger
	case "threat":
		return p.Threat
	case "debt":
		return p.Debt
	case "injury":
		return p.Injury
	case "fatigue":
		return p.Fatigue
	default:
		return false
	}
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// normalizeText 去掉空白用于子串存在性校验。
func normalizeText(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '　' {
			return -1
		}
		return r
	}, s)
}
