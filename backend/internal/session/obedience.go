package session

// 文件说明：obedience.go，服从度评估与偏离判定，决定单位对玩家命令的执行/抗命状态。

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// obedienceState 类型定义用于统一该模块的数据表达。
type obedienceState string

// 常量定义区：集中声明该文件使用的共享配置。
const (
	obedienceSteady    obedienceState = "steady"
	obedienceConcerned obedienceState = "concerned"
	obedienceReluctant obedienceState = "reluctant"
	obedienceRefused   obedienceState = "refused"
)

// actionModifiers 结构体用于承载该模块的核心数据。
type actionModifiers struct {
	MoveMultiplier   float64
	AttackMultiplier float64
}

// 抗命溯源卡与成长旁白相关的对外约定（宪法 §3.3「抗命叙事化为她的成长」）。
//
// 设计：抗命（refused）时，决策的可见字段被加固为「祖魂身份」的灵魂——既给出
// 一张「她为什么没听你」的结构化溯源卡（归因落到原则 4 的五来源之一），又附上
// 固定成长旁白。因为 unitDecisionPayload（types.go）不能改、没有专门的结构化
// 槽位，故采用「现有可见字段 + 约定标记」的最小侵入式编码：
//   - Speak：放固定成长旁白原文（前端把它当作她的成长旁白直接渲染）。
//   - Reasoning：在原拒绝理由后，追加一段以 defianceTraceMarker 起头的单行
//     溯源卡，键值用 defianceTraceFieldSep（"|"）分隔、键名用 "=" 赋值，便于前端
//     正则/split 取值；当前字段：source（五来源标签）、phrase（一句「她为什么
//     没听你」短语）、narration（成长旁白，冗余携带便于纯文本面板直接取用）。
//
// 前端取值约定（最小渲染契约）：
//
//	找到 Reasoning 中以 "「她为什么没听你」溯源卡 ::" 起头的行，去掉标记后按
//	"|" 切片，每片再按首个 "=" 拆 key/value；source/phrase 渲染成溯源卡，
//	narration 与 Speak 一致，渲染成黄字成长旁白。无该标记则按普通拒绝理由处理。
const (
	// defianceGrowthNarration 是宪法 §3.3 钦定的固定成长旁白原文，逐字不可改。
	defianceGrowthNarration = "她第一次没有照你说的做。她在变成她自己。"
	// defianceTraceMarker 标识 Reasoning 中嵌入的溯源卡片段起头，供前端识别。
	defianceTraceMarker = "「她为什么没听你」溯源卡 ::"
	// defianceTraceFieldSep 分隔溯源卡内的多个键值。
	defianceTraceFieldSep = "|"
)

// defianceSourceLabel 表示抗命归因落点的五来源标签（原则 4：人格/记忆/红线/关系/压力）。
type defianceSourceLabel string

// 常量定义区：抗命溯源的五来源固定标签。
const (
	defianceSourcePersona  defianceSourceLabel = "人格"
	defianceSourceMemory   defianceSourceLabel = "记忆"
	defianceSourceRedline  defianceSourceLabel = "红线"
	defianceSourceRelation defianceSourceLabel = "关系"
	defianceSourcePressure defianceSourceLabel = "压力"
)

// defianceSource 把抗命理由文本归因到五来源之一，并给出一句「她为什么没听你」短语。
//
// 纯函数、确定性：仅依赖 reason 文本关键词匹配，无随机、无外部状态。判定优先级
// 按「越具体越优先」：红线 > 记忆 > 关系 > 压力 > 人格（人格兜底，因为任何抗命终归
// 是她这个人在选）。短语统一以「她」为主语，呼应宪法对外语气。
func defianceSource(reason string) (defianceSourceLabel, string) {
	text := strings.ToLower(strings.TrimSpace(reason))

	switch {
	case containsAny(text, "红线", "底线", "原则", "良心", "不肯杀", "下不去手", "不愿伤", "誓约", "信仰", "宁死"):
		return defianceSourceRedline, "这越过了她不肯让步的底线。"
	case containsAny(text, "埋伏", "陷阱", "上次", "记得", "教训", "断粮", "倒下", "受伤", "太险", "勉强", "见过", "经历过"):
		return defianceSourceMemory, "她记得上一次照做的下场。"
	// 注意：用多字关系短语而非裸单字『他』——『他』会子串误命中『其他/他们』，把非关系原因误标成关系（评审修复）。
	case containsAny(text, "队友", "同伴", "战友", "朋友", "护着", "护住", "保护", "舍不得", "不忍", "丢下", "抛下", "牵挂", "为了他", "放不下", "身边的人", "她在那"):
		return defianceSourceRelation, "她放不下身边那个人。"
	case containsAny(text, "太累", "疲惫", "疲劳", "撑不住", "饿", "断粮", "怕", "恐惧", "崩", "士气", "扛不住", "压力", "喘不过"):
		return defianceSourcePressure, "她已经被现实压得喘不过气。"
	default:
		return defianceSourcePersona, "这不是她会做的选择。"
	}
}

// composeDefianceTrace 把溯源来源、短语与成长旁白编码成可被前端解析的单行溯源卡。
func composeDefianceTrace(source defianceSourceLabel, phrase string) string {
	parts := []string{
		fmt.Sprintf("source=%s", source),
		fmt.Sprintf("phrase=%s", strings.TrimSpace(phrase)),
		fmt.Sprintf("narration=%s", defianceGrowthNarration),
	}
	return defianceTraceMarker + " " + strings.Join(parts, defianceTraceFieldSep)
}

// obedienceResolution 结构体用于承载该模块的核心数据。
type obedienceResolution struct {
	Requested         unitDecisionPayload
	Final             unitDecisionPayload
	State             obedienceState
	Note              string
	RejectProbability float64
	RiskScore         float64
	Modifiers         actionModifiers
}

// resolveDirectiveCompliance 根据方针风险与单位状态评估执行顺从度。
func resolveDirectiveCompliance(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) obedienceResolution {
	return resolveDirectiveComplianceWithRoll(
		state,
		byID,
		actor,
		decision,
		deterministicDirectiveRoll(state, actor, decision),
	)
}

// resolveDirectiveComplianceWithRoll 在指定随机掷骰下完成顺从度判定。
func resolveDirectiveComplianceWithRoll(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	roll float64,
) obedienceResolution {
	resolution := obedienceResolution{
		Requested: decision,
		Final:     decision,
		State:     obedienceSteady,
		Modifiers: actionModifiers{
			MoveMultiplier:   1,
			AttackMultiplier: 1,
		},
	}
	if actor == nil || actor.FactionID != state.PlayerFactionID {
		return resolution
	}

	directiveText := directiveContextForActor(state, actor.ID, actor.FactionID)
	if directiveText == "" {
		return resolution
	}
	if forcedOrder, ok := activeImmediateOrderForUnit(state, actor.ID); ok {
		resolution.Note = fmt.Sprintf("收到即时令：%s", strings.TrimSpace(forcedOrder.Text))
		return resolution
	}

	risk := directiveOrderRisk(state, byID, actor, decision, directiveText)
	resolution.RiskScore = risk
	if risk < 0.9 {
		return resolution
	}

	personalityMod := personalityRejectModifier(*actor)
	statusMod := statusRejectModifier(*actor)
	memoryMod := memoryRejectModifier(state, byID, actor, decision)
	loyaltyMod := loyaltyRejectModifier(*actor)

	rejectProbability := clamp01(0.05 * personalityMod * statusMod * risk * memoryMod * loyaltyMod)
	if rejectProbability > 0.85 {
		rejectProbability = 0.85
	}
	resolution.RejectProbability = rejectProbability
	if roll >= rejectProbability {
		return resolution
	}

	band := 0.5
	if rejectProbability > 0 {
		band = roll / rejectProbability
	}

	switch {
	case band < 0.10:
		resolution.State = obedienceRefused
		resolution.Note = strings.TrimSpace(firstNonEmptyText(
			decision.Reasoning,
			decision.NextAction,
			decision.Speak,
			decision.Memory,
		))
		reason := resolution.Note
		if reason == "" {
			reason = "按风险评估拒绝执行"
		}
		resolution.Final = refusedDecision(decision, reason)
	case band < 0.40:
		resolution.State = obedienceReluctant
		resolution.Note = strings.TrimSpace(firstNonEmptyText(
			decision.Reasoning,
			decision.NextAction,
			decision.Speak,
			decision.Memory,
		))
		resolution.Modifiers = actionModifiers{
			MoveMultiplier:   0.7,
			AttackMultiplier: 0.8,
		}
	default:
		resolution.State = obedienceConcerned
		resolution.Note = strings.TrimSpace(firstNonEmptyText(
			decision.Reasoning,
			decision.NextAction,
			decision.Speak,
			decision.Memory,
		))
	}

	return resolution
}

// deterministicDirectiveRoll 生成与会话上下文绑定的稳定掷骰值。
func deterministicDirectiveRoll(state State, actor *unit.Record, decision unitDecisionPayload) float64 {
	if actor == nil {
		return 0.5
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(state.ID))
	_, _ = hasher.Write([]byte(actor.ID))
	_, _ = hasher.Write([]byte(directiveContextForActor(state, actor.ID, actor.FactionID)))
	_, _ = hasher.Write([]byte(fmt.Sprintf(
		"%d/%s/%s/%s/%d/%d",
		state.TurnState.Turn,
		state.TurnState.Phase,
		decision.Action,
		decision.TargetUnitID,
		decision.TargetQ,
		decision.TargetR,
	)))
	return float64(hasher.Sum32()%10000) / 10000
}

// directiveOrderRisk 评估当前指令对单位而言的执行风险分。
func directiveOrderRisk(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	directiveText string,
) float64 {
	intensity := directiveAggressionIntensity(directiveText)
	if intensity <= 0 {
		return 0.1
	}

	switch decision.Action {
	case DecisionActionAttack, DecisionActionHeavyAttack, DecisionActionCharge:
		risk := 1.4 * intensity
		if decision.Action == DecisionActionCharge {
			risk += 0.2
		}
		if decision.TargetUnitID != "" {
			target := byID[decision.TargetUnitID]
			if target != nil && actor.Status.HP <= target.Status.HP {
				risk += 0.25
			}
		} else if decision.StructureID != "" {
			risk -= 0.2
		}
		if actor.Status.HP <= 45 {
			risk += 0.2
		}
		return risk
	case DecisionActionMove:
		before, after := moveRiskDistance(state, byID, actor, decision)
		switch {
		case after < before && after <= 1:
			return 1.2 * intensity
		case after < before:
			return 0.8 * intensity
		default:
			return 0.2
		}
	default:
		return 0.1
	}
}

// directiveAggressionIntensity 从指令文本提取进攻强度。
func directiveAggressionIntensity(text string) float64 {
	text = strings.ToLower(strings.TrimSpace(text))
	switch {
	case containsAny(text, "不惜代价", "强攻", "硬冲", "扑上", "狠狠干", "不要后退", "死守", "顶住"):
		return 1.8
	case containsAny(text, "压上", "正面", "冲锋", "强推", "立刻拿下", "猛攻", "全体进攻", "全员进攻", "集火", "歼灭", "消灭", "击杀"):
		return 1.3
	case containsAny(text, "推进", "压制", "追击", "拿下", "逼近", "进攻", "攻击"):
		return 0.6
	default:
		return 0
	}
}

// moveRiskDistance 估算移动前后与最近敌军的距离变化。
func moveRiskDistance(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) (int, int) {
	targetIDs := opposingIDs(state, actor.FactionID)
	beforeTarget := nearestBattleReady(targetIDs, byID, actor)
	if beforeTarget == nil {
		return 0, 0
	}

	before := unit.HexDistance(
		actor.Status.PositionQ,
		actor.Status.PositionR,
		beforeTarget.Status.PositionQ,
		beforeTarget.Status.PositionR,
	)
	after := unit.HexDistance(
		decision.TargetQ,
		decision.TargetR,
		beforeTarget.Status.PositionQ,
		beforeTarget.Status.PositionR,
	)
	return before, after
}

// personalityRejectModifier 基于人格特征计算抗命倾向修正。
func personalityRejectModifier(actor unit.Record) float64 {
	modifier := 1.0
	switch {
	case actor.Personality.Courage >= 0.75:
		modifier *= 0.6
	case actor.Personality.Courage <= 0.35:
		modifier *= 1.4
	}
	if actor.Personality.Prudence >= 0.7 {
		modifier *= 1.2
	}
	if actor.Personality.Stability <= 0.35 {
		modifier *= 1.15
	}
	return modifier
}

// statusRejectModifier 基于当前状态（疲劳/士气/血量等）计算抗命修正。
func statusRejectModifier(actor unit.Record) float64 {
	modifier := 1.0
	if actor.Status.Fatigue >= 70 {
		modifier *= 1.3
	}
	if actor.Status.Morale <= 0.35 {
		modifier *= 1.4
	}
	if actor.Status.HP <= 50 {
		modifier *= 1.4
	}
	if actor.Status.Hunger >= 70 {
		modifier *= 1.15
	}
	return modifier
}

// memoryRejectModifier 基于近期记忆与战场迹象计算抗命修正。
func memoryRejectModifier(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
) float64 {
	modifier := 1.0
	dangerKeywords := []string{"埋伏", "危险", "倒下", "太险", "受伤", "濒死", "不对劲"}

	for index := len(state.DialogueHistory) - 1; index >= 0 && index >= len(state.DialogueHistory)-8; index-- {
		entry := state.DialogueHistory[index]
		if entry.UnitID != actor.ID {
			continue
		}
		text := strings.ToLower(entry.Message)
		if containsAny(text, dangerKeywords...) {
			modifier *= 1.3
			break
		}
	}

	for index := len(state.Logs) - 1; index >= 0 && index >= len(state.Logs)-10; index-- {
		entry := state.Logs[index]
		if entry.ActorUnitID != actor.ID && entry.TargetUnitID != actor.ID {
			continue
		}
		text := strings.ToLower(entry.Message)
		if containsAny(text, "倒下", "失去战斗力", "造成", "伤害", "逼近") {
			modifier *= 1.25
			break
		}
	}
	if memoryContainsAny(*actor, "太险", "埋伏", "断粮", "倒下", "受伤", "陷阱", "勉强推进") {
		// 情绪扭曲：近期负面记忆会把威胁评估放大到 1.3x。
		modifier *= 1.3
	}

	if decision.Action == DecisionActionAttack || decision.Action == DecisionActionHeavyAttack || decision.Action == DecisionActionCharge {
		target := byID[decision.TargetUnitID]
		if target != nil && hasNearbyAllies(state, byID, actor, world.Coord{Q: target.Status.PositionQ, R: target.Status.PositionR}) {
			modifier *= 0.85
		}
	}

	return modifier
}

// hasNearbyAllies 判断目标点附近是否存在可战斗友军。
func hasNearbyAllies(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	target world.Coord,
) bool {
	for _, alliedID := range alliedIDs(state, actor.FactionID) {
		if alliedID == actor.ID {
			continue
		}
		ally, ok := byID[alliedID]
		if !ok || !isBattleReady(*ally) {
			continue
		}
		if unit.HexDistance(ally.Status.PositionQ, ally.Status.PositionR, target.Q, target.R) <= 1 {
			return true
		}
	}
	return false
}

// loyaltyRejectModifier 根据忠诚阈值计算抗命倍率。
func loyaltyRejectModifier(actor unit.Record) float64 {
	loyalty := math.Max(actor.Status.Loyalty, actor.Personality.Loyalty)
	switch {
	case loyalty >= 0.8:
		return 0.3
	case loyalty <= 0.3:
		return 2.5
	case loyalty <= 0.5:
		return 1.4
	default:
		return 1.0
	}
}

// refusedDecision 生成“拒绝执行后转为 hold”的替代决策。
//
// 抗命（refused）是祖魂身份的灵魂时刻：此处把决策的可见字段加固为
//   - Speak：固定成长旁白原文（宪法 §3.3，逐字钦定）——前端渲染为黄字成长旁白；
//   - Reasoning：原拒绝理由 + 一行可解析的「她为什么没听你」溯源卡，归因落到
//     原则 4 的五来源之一（人格/记忆/红线/关系/压力）。
//
// 仅抗命路径触发；服从路径完全不经过本函数，零影响。确定性：溯源来源仅由 reason
// 文本经纯函数 defianceSource 判定，不引入随机。
func refusedDecision(decision unitDecisionPayload, reason string) unitDecisionPayload {
	nextAction := limitTextRunes(firstNonEmptyText(
		decision.NextAction,
		decision.Speak,
		decision.Memory,
		decision.Reasoning,
	), 12)
	memory := limitTextRunes(firstNonEmptyText(
		decision.Memory,
		decision.Speak,
		decision.NextAction,
		decision.Reasoning,
	), 18)

	trimmedReason := strings.TrimSpace(reason)
	source, phrase := defianceSource(trimmedReason)
	trace := composeDefianceTrace(source, phrase)

	// 拒绝理由 = 原因 + 换行 + 溯源卡（前端按 defianceTraceMarker 切出溯源卡）。
	reasoning := trace
	if trimmedReason != "" {
		reasoning = trimmedReason + "\n" + trace
	}

	return unitDecisionPayload{
		Action:     DecisionActionHold,
		NextAction: nextAction,
		Memory:     memory,
		Knowledge:  strings.TrimSpace(decision.Knowledge),
		// Speak 直接承载固定成长旁白原文（不截断，保证逐字呈现）。
		Speak:     defianceGrowthNarration,
		Reasoning: reasoning,
	}
}

// firstNonEmptyText 返回首个非空候选文本。
func firstNonEmptyText(candidates ...string) string {
	for _, candidate := range candidates {
		if value := strings.TrimSpace(candidate); value != "" {
			return value
		}
	}
	return ""
}

// describeDecisionTarget 生成决策目标的人类可读摘要。
func describeDecisionTarget(byID map[string]*unit.Record, decision unitDecisionPayload) string {
	switch decision.Action {
	case DecisionActionAttack:
		if target, ok := byID[decision.TargetUnitID]; ok {
			return fmt.Sprintf("进攻 %s", target.DisplayName())
		}
		if decision.StructureType != "" {
			return fmt.Sprintf("攻击 %s", structureDisplayName(decision.StructureType))
		}
		return "进攻目标"
	case DecisionActionCharge:
		if target, ok := byID[decision.TargetUnitID]; ok {
			return fmt.Sprintf("冲锋接敌 %s", target.DisplayName())
		}
		if decision.StructureType != "" {
			return fmt.Sprintf("冲锋拆除 %s", structureDisplayName(decision.StructureType))
		}
		return "冲锋目标"
	case DecisionActionHeavyAttack:
		if target, ok := byID[decision.TargetUnitID]; ok {
			return fmt.Sprintf("重击 %s", target.DisplayName())
		}
		if decision.StructureType != "" {
			return fmt.Sprintf("重击 %s", structureDisplayName(decision.StructureType))
		}
		return "重击目标"
	case DecisionActionDefend:
		return "防御待击"
	case DecisionActionObserve:
		return "观察校准"
	case DecisionActionAssist:
		if target, ok := byID[decision.TargetUnitID]; ok {
			return fmt.Sprintf("援助 %s", target.DisplayName())
		}
		return "援助队友"
	case DecisionActionDialogue:
		if target, ok := byID[decision.TargetUnitID]; ok {
			return fmt.Sprintf("与 %s 交谈", target.DisplayName())
		}
		return "与邻近单位交谈"
	case DecisionActionTrade:
		if target, ok := byID[decision.TargetUnitID]; ok {
			switch decision.TradeKind {
			case TradeActionKindGift:
				return fmt.Sprintf("把 %s 交给 %s", displayItemName(decision.ItemID), target.DisplayName())
			case TradeActionKindGold:
				return fmt.Sprintf("向 %s 调拨 %d 金", target.DisplayName(), decision.GoldAmount)
			case TradeActionKindSell:
				return fmt.Sprintf("向 %s 开价 %d 金卖 %s", target.DisplayName(), decision.Price, displayItemName(decision.ItemID))
			}
			return fmt.Sprintf("与 %s 交易", target.DisplayName())
		}
		return "交易物资"
	case DecisionActionRomance:
		if target, ok := byID[decision.TargetUnitID]; ok {
			return fmt.Sprintf("向 %s 表露心意", target.DisplayName())
		}
		return "表露心意"
	case DecisionActionFamily:
		if target, ok := byID[decision.TargetUnitID]; ok {
			return fmt.Sprintf("与 %s 商量家庭", target.DisplayName())
		}
		return "商量家庭"
	case DecisionActionBuild:
		return fmt.Sprintf("建造 %s", structureDisplayName(decision.StructureType))
	case DecisionActionDemolish:
		return fmt.Sprintf("拆除 %s", structureDisplayName(decision.StructureType))
	case DecisionActionGather:
		return productionActivityDisplayName(decision.Activity)
	case DecisionActionMove:
		return fmt.Sprintf("向 %d,%d 推进", decision.TargetQ, decision.TargetR)
	default:
		return "原地等待"
	}
}

// logDirectiveCompliance 把顺从度结果写入 defiance 系列日志。
func logDirectiveCompliance(
	state *State,
	actor *unit.Record,
	byID map[string]*unit.Record,
	resolution obedienceResolution,
) {
	if state == nil || actor == nil || resolution.State == obedienceSteady {
		return
	}

	targetSummary := describeDecisionTarget(byID, resolution.Requested)
	message := directiveComplianceMessage(*actor, resolution, targetSummary)
	switch resolution.State {
	case obedienceConcerned:
		appendLog(
			state,
			"defiance_concerned",
			message,
			actor.ID,
			resolution.Requested.TargetUnitID,
		)
	case obedienceReluctant:
		appendLog(
			state,
			"defiance_reluctant",
			message,
			actor.ID,
			resolution.Requested.TargetUnitID,
		)
	case obedienceRefused:
		appendLog(
			state,
			"defiance_refused",
			message,
			actor.ID,
			resolution.Requested.TargetUnitID,
		)
	}
}

// directiveComplianceMessage 生成顺从度事件的展示文案。
func directiveComplianceMessage(_ unit.Record, resolution obedienceResolution, targetSummary string) string {
	requested := firstNonEmptyText(
		resolution.Requested.NextAction,
		resolution.Requested.Speak,
		resolution.Requested.Memory,
		resolution.Requested.Reasoning,
	)
	final := firstNonEmptyText(
		resolution.Final.NextAction,
		resolution.Final.Speak,
		resolution.Final.Memory,
		resolution.Final.Reasoning,
	)
	if requested == "" {
		requested = strings.TrimSpace(targetSummary)
	}
	if final == "" {
		final = strings.TrimSpace(targetSummary)
	}

	if requested != "" && final != "" && requested != final {
		return fmt.Sprintf("%s；%s", requested, final)
	}
	if final != "" {
		return final
	}
	if requested != "" {
		return requested
	}

	note := strings.TrimSpace(resolution.Note)
	if note != "" {
		return note
	}
	return strings.TrimSpace(targetSummary)
}

// effectiveMoveRange 按倍率缩放可移动步数并保证下限。
func effectiveMoveRange(base int, multiplier float64) int {
	if base <= 0 {
		return 0
	}
	if multiplier <= 0 {
		return 0
	}

	effective := int(math.Floor(float64(base) * multiplier))
	if effective < 1 {
		effective = 1
	}
	return effective
}

// scaledDamage 按倍率缩放伤害并保证最小伤害为 1。
func scaledDamage(base int, multiplier float64) int {
	if base <= 0 {
		return 0
	}
	if multiplier <= 0 {
		return 1
	}
	damage := int(math.Round(float64(base) * multiplier))
	if damage < 1 {
		damage = 1
	}
	return damage
}

// clamp01 将浮点值裁剪到 [0,1] 区间。
func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}
