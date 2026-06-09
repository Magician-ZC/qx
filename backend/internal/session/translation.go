package session

// 文件说明：data-driven「翻译层」（设计宪法/事件耦合 §M1）——把宏观世界事件按
// (事件语义类 × relevance.AnchorKind) 翻译成贴合个人的命运 beat 措辞，替代 fate.go 里
// crossSummary/fateCard/deriveFateProvenance 的「一刀切」措辞。
//
// 设计原则：纯函数、确定性、零 LLM、零副作用、零 DB。整张矩阵是静态表（fateTranslationTable），
// 决策用 LLM、结算/措辞用代码——这层只做「查表 + 占位渲染」，命中即用更贴合的 beat、未命中 matched=false
// 让调用方零行为变化地回落现有 deriveFateProvenance/Summary（严格向后兼容）。
//
// 矩阵 key 口径：
//   - 「事件语义类」由 FateEvent 现有可用字段判定——优先看 ReasonCode（如 ReasonRelationBetray→叛变/投靠语义），
//     再用关键词扫 Summary（征召/欠债/投靠等宏观事件没有专属 reason-code，只能从措辞里认）。详见 classifyFateEvent。
//   - 「锚类」直接用 eventRelevanceWithAnchor 返回的命中最重锚类别（relevance.AnchorKind）。
//   - 任一维缺省（语义类未识别 / 锚类为空）都会优雅回退：先退到「通用兜底 × 锚类」，再退到「未命中」。

import (
	"context"
	"database/sql"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
)

// fateEventClass 是一条世界事件的「语义类」——翻译矩阵的第一维。
// 与 worldbus.EventKind / events.ReasonCode 解耦：后两者是「机制类型」，这里是「叙事语义」，
// 由 classifyFateEvent 从 ReasonCode + Summary 关键词归一得来（一个语义类可由多种机制触发）。
type fateEventClass string

const (
	classConscription fateEventClass = "conscription" // 战争征召：被卷入/征召入伍/开赴前线
	classDebt         fateEventClass = "debt"         // 贸易欠债：欠下/赊借/债务缠身
	classDefection    fateEventClass = "defection"    // 势力投靠/倒戈：改换门庭、归顺他方、背主
	classGeneric      fateEventClass = ""             // 通用兜底（未识别出具体宏观语义）
)

// translationKey 是翻译矩阵的复合键：(事件语义类 × 锚类)。
type translationKey struct {
	class  fateEventClass
	anchor relevance.AnchorKind
}

// fateTranslationTable 是 data-driven 翻译矩阵：把 (事件语义类 × 锚类) 翻译成贴合个人的命运 beat 模板。
// 模板含占位 {target}（事件中与她相关的另一方，缺省「那个人」）与 {event}（事件一句话摘要 Summary）。
// 覆盖设计点名的三类宏观事件（征召/欠债/投靠）× 关键锚类，外加每类的通用锚兜底（anchor=Relation 视角默认）。
// 未在表里的 (class, anchor) 组合由 translateFateBeat 的多级回退处理（见该函数）。
var fateTranslationTable = map[translationKey]string{
	// ===== 战争征召（conscription）：把「某地征召/某人被卷入战事」翻成贴个人的命运 beat =====
	{classConscription, relevance.Relation}:       "{target}被卷进了那场征召，刀兵将至——而她在乎的人，正站在风口上。",
	{classConscription, relevance.Redline}:        "战火要征走{target}了。她曾立誓绝不让这样的事发生，如今红线就在眼前。",
	{classConscription, relevance.Goal}:           "一纸征召打乱了一切：{target}被卷入战事，她一直想做成的事，悬了。",
	{classConscription, relevance.DebtGrudgeLove}: "{target}被征召上了前线——这一去，她与{target}之间那桩未了的情债，再难有了结的时候。",
	{classConscription, relevance.Legacy}:         "征召的号角响到了门前，{target}被卷入战事，她血脉里的牵系也被一并扯动。",
	{classConscription, relevance.Geo}:            "她所在的地方落进了征召的版图，{target}被卷入战事——战线，离她只剩一步。",

	// ===== 贸易欠债（debt）：把「某人欠下/赊借/债务缠身」翻成贴个人的命运 beat =====
	{classDebt, relevance.Relation}:       "{target}欠下了一身还不清的债。她在乎的人陷在债里，她没法当作没看见。",
	{classDebt, relevance.Redline}:        "{target}为了还债，正要越过她绝不容许的那条线。",
	{classDebt, relevance.Goal}:           "{target}的这笔债像块石头压下来，她想做成的事，被它拖住了脚。",
	{classDebt, relevance.DebtGrudgeLove}: "债又添了一笔：{target}欠下的，连同她与{target}旧日的债仇情，越缠越紧。",
	{classDebt, relevance.Legacy}:         "为了{target}的债，连传家的东西都被摆上了台面——她血脉里的东西在隐隐作痛。",
	{classDebt, relevance.Geo}:            "债务的风声传到她脚下的这片地方：{target}欠下的窟窿，迟早会烧到她身边。",

	// ===== 势力投靠/倒戈（defection）：把「改换门庭/归顺他方/背主」翻成贴个人的命运 beat =====
	{classDefection, relevance.Relation}:       "{target}改换了门庭，投到了别处。她在乎的人，从此站到了另一边。",
	{classDefection, relevance.Redline}:        "{target}倒戈投了过去——背主，正是她立下死也不碰的那条红线。",
	{classDefection, relevance.Goal}:           "{target}的临阵投靠，把她苦心经营的局搅了个底朝天，她想成的事悬于一线。",
	{classDefection, relevance.DebtGrudgeLove}: "{target}转投他方，把她与{target}之间那笔债仇情，硬生生劈成了敌我两端。",
	{classDefection, relevance.Legacy}:         "{target}背了旧主另投他处——连同她血脉所系的那一脉，也被这一投扯裂。",
	{classDefection, relevance.Geo}:            "她脚下这片地方的人心倒向了别处：{target}的投靠，是变天的头一阵风。",
}

// fateGenericAnchorTemplate 是「通用兜底 × 锚类」的回退模板（class 未识别、或具体 (class,anchor) 缺表时用）。
// 比 deriveFateProvenance 的「引子：摘要」更像一段命运 beat（带 {target}/{event} 画面），但仍只取自真实字段。
var fateGenericAnchorTemplate = map[relevance.AnchorKind]string{
	relevance.Relation:       "因为这事关她在乎的人，{event}",
	relevance.Redline:        "这触到了她亲手划下的红线：{event}",
	relevance.Goal:           "这关乎她一直想做成的事：{event}",
	relevance.DebtGrudgeLove: "这桩债、仇、情还没了断，{event}",
	relevance.Geo:            "这就发生在她脚下的地方：{event}",
	relevance.Legacy:         "这牵动着她的血脉与传家之物：{event}",
}

// translateFateBeat 把一条世界事件按其语义类与命中锚类翻译成贴合个人的命运 beat。
//
//   - 命中（matched=true）：返回填好占位（{target}/{event}）的 beat，调用方用它作更贴合的措辞。
//   - 未命中（matched=false）：返回空串，调用方**零行为变化**地回落现有 deriveFateProvenance/Summary（向后兼容）。
//
// 多级回退（越来越宽，任一级有模板即命中）：
//  1. 精确 (语义类 × 锚类) —— 最贴合；
//  2. 通用兜底 × 锚类 —— 语义类没识别出、或该具体组合缺表时，仍按「凭什么牵动她」给一段 beat；
//  3. 都没有（锚类为空且语义类未识别，如自身事件无外部锚）→ matched=false，交还给调用方。
//
// 纯函数、确定性（同输入恒同输出）、零副作用。
func translateFateBeat(ev FateEvent, anchorKind relevance.AnchorKind) (string, bool) {
	class := classifyFateEvent(ev)

	// 1) 精确组合：(语义类 × 锚类)。
	if class != classGeneric && anchorKind != "" {
		if tmpl, ok := fateTranslationTable[translationKey{class: class, anchor: anchorKind}]; ok {
			return renderFateTemplate(tmpl, ev), true
		}
	}

	// 2) 通用兜底 × 锚类：有锚类即可给一段 beat（含具体语义类但缺该组合时，也回落到这里——保证三类宏观事件
	//    至少能按锚类给措辞，而非直接掉回旧逻辑）。
	if anchorKind != "" {
		if tmpl, ok := fateGenericAnchorTemplate[anchorKind]; ok {
			return renderFateTemplate(tmpl, ev), true
		}
	}

	// 3) 无锚类（自身事件等）：不强行翻译，交还调用方走现有 deriveFateProvenance/Summary。
	return "", false
}

// classifyFateEvent 从 FateEvent 现有字段判定事件语义类——翻译矩阵的第一维。
// 口径（先 ReasonCode 后关键词，确定性）：
//   - ReasonCode 强信号优先：ReasonRelationBetray/ReasonFactionCollapse → 投靠/倒戈语义（defection）。
//   - 其余宏观语义（征召/欠债/投靠）没有专属 reason-code，从 Summary（必要时含 AttributionZH）关键词认：
//     征召/参军/入伍/开赴/前线/战火 → conscription；欠/债/赊/借贷/还不清 → debt；
//     投靠/归顺/倒戈/改换门庭/背主 → defection。
//   - 都不命中 → classGeneric（交给「通用兜底 × 锚类」处理）。
func classifyFateEvent(ev FateEvent) fateEventClass {
	// ReasonCode 强信号：背叛/势力崩塌天然属投靠/倒戈语义类。
	switch ev.ReasonCode {
	case events.ReasonRelationBetray, events.ReasonFactionCollapse:
		return classDefection
	}

	// 关键词扫（合并 Summary + AttributionZH，宏观事件的语义往往落在措辞里）。
	text := strings.TrimSpace(ev.Summary)
	if a := strings.TrimSpace(ev.AttributionZH); a != "" {
		text += " " + a
	}
	if text == "" {
		return classGeneric
	}
	// 复用 trade.go 的 containsAny(text, candidates ...string)（变参，内部按子串匹配；中文关键词大小写无关）。
	if containsAny(text, conscriptionKeywords...) {
		return classConscription
	}
	if containsAny(text, debtKeywords...) {
		return classDebt
	}
	if containsAny(text, defectionKeywords...) {
		return classDefection
	}
	return classGeneric
}

// 三类宏观事件的判定关键词（中文措辞集，确定性、可扩展）。刻意收窄、宁可漏判回落兜底，也不误判。
var (
	conscriptionKeywords = []string{"征召", "征兵", "参军", "入伍", "开赴", "上前线", "奔赴前线", "战火", "兵役", "应征"}
	debtKeywords         = []string{"欠债", "欠下", "债务", "赊", "借贷", "还不清", "讨债", "债主", "负债", "欠款"}
	defectionKeywords    = []string{"投靠", "归顺", "倒戈", "改换门庭", "背主", "易主", "叛投", "转投", "改投"}
)

// renderFateTemplate 把模板里的 {target}/{friend}/{region}/{event} 占位替换成事件的真实字段（确定性、纯函数、占位安全）。
//   - {target}/{friend}：与她相关的另一方（DB 矩阵用 {friend}，原内存矩阵用 {target}，二者同义、同一来源）。优先 ActorID
//     （事件发起方），缺省回落 TargetID；都空用「那个人」。注意：填的是稳定标识（ID 或缺省词），措辞层不展开为显示名
//     （跨分片角色可能无显示名，与 crossSummary 一致克制）。
//   - {region}：事件来源地 SourceRegionID，缺省用「她所在的地方」（绝不残留裸占位）。
//   - {event}：事件一句话摘要 Summary，缺省时占位整段连同其前导措辞被裁掉（避免出现空尾巴）。
func renderFateTemplate(tmpl string, ev FateEvent) string {
	target := strings.TrimSpace(ev.ActorID)
	if target == "" {
		target = strings.TrimSpace(ev.TargetID)
	}
	if target == "" {
		target = "那个人"
	}
	out := strings.ReplaceAll(tmpl, "{target}", target)
	out = strings.ReplaceAll(out, "{friend}", target)

	region := strings.TrimSpace(ev.SourceRegionID)
	if region == "" {
		region = "她所在的地方"
	}
	out = strings.ReplaceAll(out, "{region}", region)

	summary := strings.TrimSpace(ev.Summary)
	if summary != "" {
		out = strings.ReplaceAll(out, "{event}", summary)
	} else {
		// 无摘要：把「，{event}」「：{event}」「{event}」等占位连同其紧邻的引导标点一并裁掉，避免空尾巴。
		out = strings.ReplaceAll(out, "，{event}", "")
		out = strings.ReplaceAll(out, "：{event}", "")
		out = strings.ReplaceAll(out, "{event}", "")
		out = strings.TrimSpace(out)
	}
	return out
}

// translateFateBeatFromDB 是 §M1 翻译矩阵的 **DB 版消费入口**（SurfaceFateEvent 用它替代纯内存 translateFateBeat）：
// 按事件 reason-code × 命中锚类查 data-driven translation_templates 表（带内存缓存、三级回退），渲染成贴合个人的命运 beat。
//
//   - beat：填好占位（{friend}/{region}/{event}…）的命运 beat。命中精确组/通用兜底用专属模板；缺模板时回退 DefaultReasonText
//     （仍是一句可用的保守 beat，并已在 loadTranslationTemplate 里计入遥测便于排期补全）。
//   - forcePending：该 (reason_code × 锚类) 是否标了 force_pending（密友倒地/密友被背叛/她所在地被劫等）——调用方据此把路由
//     从高光卡硬抬到待决策（§1.2「force_pending 类必须有专属模板」的机制落地）。
//
// best-effort、确定性、零 LLM、零副作用（除遥测/缓存）。db 为 nil 时退内置矩阵，绝不阻断命运路由。
func translateFateBeatFromDB(ctx context.Context, db *sql.DB, ev FateEvent, anchorKind relevance.AnchorKind) (beat string, forcePending bool) {
	t := loadTranslationTemplate(ctx, db, ev.ReasonCode, anchorKind)
	if strings.TrimSpace(t.Narrative) == "" {
		return "", false
	}
	return renderFateTemplate(t.Narrative, ev), t.ForcePending
}
