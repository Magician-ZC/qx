package session

// 文件说明：obedience.go，服从度评估与偏离判定，决定单位对玩家命令的执行/抗命状态。

import (
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"time"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/runtimeconfig"
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
	// HasPlayerDirective 标记本次决策处于「玩家方针/任务/即时令」管辖下（actor 属玩家阵营且
	// directiveContextForActor 非空）。忠诚闭环（loyalty_loop.go）据此判定是否应结算忠诚——
	// 无玩家指令时单位的行为是纯自主，不触发「越按越不听 / 顺其本心」的任何忠诚反馈。
	HasPlayerDirective bool
	// ForcedByImmediateOrder 标记本次顺从是被玩家即时令（order，已扣指挥力的付费动作）强制压下来的：
	// 单位本回合收到点名即时令、走 activeImmediateOrderForUnit 早返回路径（无掷骰、无抗命空间）。
	ForcedByImmediateOrder bool
	// WouldDefyUnderForce 仅在 ForcedByImmediateOrder=true 时有意义：表示「若没有这道即时令、
	// 单位本会被自身倾向驱使抗命」（按与主路径一致的抗命概率算出的影子判定 ≥ 强令阈值）。
	// 这是「越按越不听」忠诚负反馈的触发位——被强令做了违心的事才扣忠诚，强令做了它本来也想做的
	// 事则不扣。确定性：复用 directiveOrderRisk + 各 reject modifier，无随机。
	WouldDefyUnderForce bool
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
	resolution.HasPlayerDirective = true
	if forcedOrder, ok := activeImmediateOrderForUnit(state, actor.ID); ok {
		resolution.Note = fmt.Sprintf("收到即时令：%s", strings.TrimSpace(forcedOrder.Text))
		resolution.ForcedByImmediateOrder = true
		// 影子判定「若没有这道强令、单位本会不会抗命」：复用主路径的风险+抗命概率口径，
		// 但即时令路径无掷骰（强制压下来），故用「抗命概率是否过强令阈值」作为「越按越不听」的触发位。
		shadowRisk := directiveOrderRisk(state, byID, actor, decision, directiveText)
		resolution.RiskScore = shadowRisk
		if shadowRisk >= 0.9 {
			shadowReject := directiveRejectProbability(state, byID, actor, decision, shadowRisk)
			resolution.RejectProbability = shadowReject
			// 影子抗命概率达到此阈即认定「本会违心抗命」。取与主路径 reluctant/refused 同量级的保守阈值，
			// 避免把「略有顾虑也照做」误判成被强压。阈值走 runtimeconfig（"obedience.forced_defiance_threshold"，默认 0.3）。
			resolution.WouldDefyUnderForce = shadowReject >= runtimeconfig.GetFloat("obedience.forced_defiance_threshold")
		}
		return resolution
	}

	risk := directiveOrderRisk(state, byID, actor, decision, directiveText)
	resolution.RiskScore = risk
	if risk < 0.9 {
		return resolution
	}

	rejectProbability := directiveRejectProbability(state, byID, actor, decision, risk)
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

// 「被即时令强压时，影子抗命概率达到此阈即认定单位本会违心抗命」的判定阈，默认 0.3：与主路径
// band<0.40 进入 reluctant（违心执行）同量级，保守地只把「明显本会抗命」的强令算作「越按越不听」的
// 扣忠诚触发，避免把「略有顾虑仍照做」误伤为被强压。现已迁入 runtimeconfig（"obedience.forced_defiance_threshold"）。

// directiveRejectProbability 按与主路径完全一致的口径计算单位对当前指令的抗命概率。
// 抽取自 resolveDirectiveComplianceWithRoll 主分支，供主分支与「即时令强压影子判定」共用，
// 保证「越按越不听」的判定与真实抗命判定同源。纯函数、确定性（仅依赖人格/状态/记忆/忠诚修正与风险）。
func directiveRejectProbability(
	state State,
	byID map[string]*unit.Record,
	actor *unit.Record,
	decision unitDecisionPayload,
	risk float64,
) float64 {
	personalityMod := personalityRejectModifier(*actor)
	statusMod := statusRejectModifier(*actor)
	memoryMod := memoryRejectModifier(state, byID, actor, decision)
	loyaltyMod := loyaltyRejectModifier(*actor)
	// 自治胆量曲线（沙盘 §5.2「越久越自主但越保守」）：离线越久，对高风险/不可逆动作越谨慎。
	// 仅与既有人格/状态/记忆/忠诚信号**叠加**（乘进同一连乘式，不替代任一），flag 关时恒 1.0（中性、零行为）。
	courageMod := offlineCourageRejectModifier(state, decision)
	// 野心契合度（③ 野心进服从）：指令目标动作若契合该单位野心（复仇心重→攻伐、逐利→交易…），更乐意执行 → 降低抗命概率；
	// 与胆量曲线同为**乘项叠加**（非替代），flag QUNXIANG_AMBITION_SCORING 关时恒 1.0（中性、零行为）。
	ambitionMod := ambitionComplianceRejectModifier(actor, decision)

	rejectProbability := clamp01(0.05 * personalityMod * statusMod * risk * memoryMod * loyaltyMod * courageMod * ambitionMod)
	if rejectProbability > 0.85 {
		rejectProbability = 0.85
	}
	return rejectProbability
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

// ──────────────────────────────────────────────────────────────────────────
// 自治胆量曲线（沙盘 §5.2「风险/广度双曲线：越久越自主但越保守」）
//
// 设计宪法：离线越久 = ①风险偏好↓（对高风险/不可逆动作越谨慎，更可能抗命冒险指令）；
// ②自治广度↑（短期只放行日常领域，长期才解锁更自主的领域；但高代价领域始终更保守）。
// 两条曲线**独立可调**、确定性（log/分档纯函数，无随机、time 仅墙钟比较）、付费不进
// （任何评分/门槛不含 wallet/billing）、与既有人格/记忆/红线/状态/忠诚信号**叠加不替代**。
//
// flag QUNXIANG_COURAGE_CURVE **默认关** → offlineCourageRejectModifier 恒返回 1.0
// （中性，对既有抗命概率逐位一致、零行为变化），仅在显式置 flag 后才灰度启用。
// ──────────────────────────────────────────────────────────────────────────

// courageCurveFlagEnv 是自治胆量曲线的灰度开关环境变量名。默认关 → 抗命修正恒 1.0（中性）。
const courageCurveFlagEnv = "QUNXIANG_COURAGE_CURVE"

// courageCurveEnabled 读 QUNXIANG_COURAGE_CURVE（默认关）。开时离线时长才进抗命概率与广度门。
// 自包含解析，对齐 ambition_scoring.go / auto_match.go 的 flag idiom。
func courageCurveEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride(courageCurveFlagEnv))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// autonomyDomain 表示一个「可自决领域」标签——广度曲线按离线时长分档解锁/限制的最小单位。
type autonomyDomain string

// 常量定义区：自治领域标签（与 §5.2 的 {move,gather,...}/{trade,build,...}/{ally,romance,migrate} 三档对应）。
const (
	autonomyDomainDaily    autonomyDomain = "daily"    // 日常：移动/采集/进食/对话/防御——0h 起即解锁
	autonomyDomainCivic    autonomyDomain = "civic"    // 经营：交易/生产/建造/结交——离线 ≥6h 解锁
	autonomyDomainHighRisk autonomyDomain = "highrisk" // 高代价：结盟/恋爱/家庭/迁徙/拆除——离线 >24h 才解锁，且始终更保守
)

// 自治广度的两个分档边界（小时）。直接落 §5.2：0–6h 仅日常；6–24h 解锁经营；>24h 解锁高代价。
const (
	autonomyCivicUnlockHours    = 6.0
	autonomyHighRiskUnlockHours = 24.0
)

// decisionAutonomyDomain 把一个决策动作归类到自治领域标签。
//
// 纯函数、确定性：仅依赖 decision.Action（与 §5.3 冻结清单的「高代价不可逆动作」口径对齐）。
// 未登记动作（攻击/重击/冲锋等战斗动作不在自治广度管辖——它们由风险曲线 directiveOrderRisk 直接处理）
// 归 daily，避免广度门误伤战斗服从判定。
func decisionAutonomyDomain(action DecisionAction) autonomyDomain {
	switch action {
	case DecisionActionRomance, DecisionActionFamily, DecisionActionDemolish:
		// 恋爱/家庭/拆除：不可逆或高情感代价，属高代价领域（始终更保守）。
		return autonomyDomainHighRisk
	case DecisionActionTrade, DecisionActionBuild, DecisionActionForge, DecisionActionUpgrade:
		// 交易/建造/锻造/升级：经营类领域，离线 ≥6h 才解锁自决。
		return autonomyDomainCivic
	default:
		// move/gather/eat/dialogue/defend/observe/assist/pickup/equip/hold/say/skill/attack… → 日常（0h 即放行）。
		return autonomyDomainDaily
	}
}

// offlineCaution 实现 §5.2 风险曲线的核心标定：offlineCaution = 1 + 0.4·log2(1+offlineHours)，
// 非线性饱和（log 增速递减），并夹一个保守上限避免极长离线把谨慎度推到失控。
//
// 纯函数、确定性：仅依赖 offlineHours（≥0）。offlineHours≤0 → 1.0（无离线惩罚，与在线一致）。
// 该上限只夹「胆量曲线这一项」的乘数；最终抗命概率仍由 directiveRejectProbability 统一夹 0.85。
func offlineCaution(offlineHours float64) float64 {
	if offlineHours <= 0 {
		return 1.0
	}
	caution := 1.0 + 0.4*math.Log2(1.0+offlineHours)
	// offlineCautionCeiling：单项乘数上限，仅防御「极长离线（远超任何真实托管时长）把谨慎度推到失控」。
	// 默认 4.5：约在 offlineHours≈430h（≈18 天）才触顶，故真实离线区间（小时～两周量级）内「越久越谨慎」
	// 保持严格单调；触顶后仍封住无界增长——长期托管学会「绝不拿命赌大的」而非把抗命乘数推到无穷。
	// 该上限只夹「胆量曲线这一项」；最终抗命概率仍由 directiveRejectProbability 统一夹 0.85。
	// 现已迁入 runtimeconfig（"obedience.offline_caution_ceiling"，默认即此值）。
	offlineCautionCeiling := runtimeconfig.GetFloat("obedience.offline_caution_ceiling")
	if caution > offlineCautionCeiling {
		caution = offlineCautionCeiling
	}
	return caution
}

// domainBreadthCaution 实现 §5.2 广度曲线：按离线时长分档「解锁/限制自治领域」。
//
// 语义：领域**未解锁**（离线不够久）→ 该领域的激进自决应更可能被判为不执行（额外谨慎乘数 >1）；
// 已解锁 → 1.0（广度放行，风险偏好交回风险曲线处理）；高代价领域即使解锁也始终额外更保守（乘数恒 >1）。
// 纯函数、确定性：仅依赖 domain + offlineHours，无随机。
func domainBreadthCaution(domain autonomyDomain, offlineHours float64) float64 {
	switch domain {
	case autonomyDomainDaily:
		// 日常领域 0h 起即完全自主，广度永不额外加谨慎。
		return 1.0
	case autonomyDomainCivic:
		if offlineHours >= autonomyCivicUnlockHours {
			return 1.0 // 已解锁经营自决，广度放行。
		}
		// 短期离线（<6h）尚不该自作主张做经营决策 → 更可能上交/不执行（额外谨慎）。
		return 1.5
	case autonomyDomainHighRisk:
		if offlineHours >= autonomyHighRiskUnlockHours {
			// 解锁后仍是高代价不可逆领域：始终更保守（恒 >1），呼应「绝不拿命赌大的」。
			return 1.3
		}
		// 未解锁高代价领域：强谨慎，几乎一律不自作主张（与冻结清单 §5.3 同向，但此处只抬抗命概率不冻结）。
		return 2.0
	default:
		return 1.0
	}
}

// offlineCourageRejectModifier 是自治胆量曲线进 directiveRejectProbability 的统一乘权桥：
// 把①风险曲线（offlineCaution）与②广度曲线（domainBreadthCaution）合成一个抗命概率乘数。
//
// flag QUNXIANG_COURAGE_CURVE 关（默认）→ 恒 1.0（中性，零行为）。开时：
//
//	modifier = offlineCaution(offlineHours) · domainBreadthCaution(domain, offlineHours)
//
// 离线越久 → offlineCaution 单调升 → 对高风险动作越谨慎；领域未解锁/高代价 → 广度项额外加谨慎。
//
// 确定性修复（LOW②）：原先用 time.Now() 经 offlineHoursFromState 算离线时长，墙钟会随真实时间漂移，
// 致同 (sessionID,turn,actor) 重放发散（破坏确定性不变量）。改用**确定性回合差**派生离线时长——
// 离线时长 = (当前回合 - 离线起始回合) × 每回合等效小时（offlineHoursFromTurns）。脱墙钟、纯回合函数，
// 故同 (sessionID,turn,actor) 重放逐位复现（time 仅在墙钟超时处用，决策概率链零墙钟）。
func offlineCourageRejectModifier(state State, decision unitDecisionPayload) float64 {
	if !courageCurveEnabled() {
		return 1.0
	}
	offlineHours := offlineHoursFromTurns(state)
	domain := decisionAutonomyDomain(decision.Action)
	return offlineCaution(offlineHours) * domainBreadthCaution(domain, offlineHours)
}

// offlineTurnEquivHours 是「一个部署回合 ≈ 多少小时离线/托管时长」的确定性换算常量。
// 取 6.0：使 1 回合≈刚好触发经营领域解锁门（autonomyCivicUnlockHours=6），4 回合≈触发高代价门（24），
// 让回合差喂进的曲线分档与小时曲线（§5.2）的语义对齐——既「越久越谨慎」单调，又脱墙钟、可确定性重放。
const offlineTurnEquivHours = 6.0

// offlineHoursFromTurns 从会话状态**确定性**派生「离线已持续多少（等效）小时」——脱墙钟、纯回合函数。
//
// 离线起始回合锚（§5.2「用离线起始 turn 推算」）：State 由 types.go 别处定义、本文件不可加字段，
// 故不存独立的 offlineStartTurn，而用 state 现有可得的确定性回合信息推算——以本局起始回合（turn=1，
// turns.NewState 钦定）为离线锚起点，回合差 = max(0, 当前回合 - 1)，再 × offlineTurnEquivHours 等效成小时。
// 语义：玩家不在场时每过一个部署回合即累积一段自治时长（越久越保守），与墙钟离线同向且严格单调。
//
// 确定性：仅依赖 state.TurnState.Turn（同 (sessionID,turn,actor) 重放该回合恒定）→ 逐位复现，无随机、无 time.Now。
func offlineHoursFromTurns(state State) float64 {
	turnsElapsed := state.TurnState.Turn - 1 // 起始回合(1)为锚：第 1 回合视作 0 离线（在线），其后逐回合累积。
	if turnsElapsed <= 0 {
		return 0
	}
	return float64(turnsElapsed) * offlineTurnEquivHours
}

// ambitionComplianceRejectModifier 把野心契合度接进服从（③）：指令目标动作越契合该单位的野心，越乐意执行 → 抗命概率乘数 <1.0。
//
// 语义：用 OnlineAmbitionActionWeight(record, tag) ∈ [1.0, 1.6]（1.0=中性，>1.0=契合，越契合越高）取其倒数当抗命乘数——
//   - weight==1.0（中性/未登记动作/flag 关）→ 乘数 1.0（不调整，逐位一致、零行为）；
//   - weight>1.0（野心契合，如复仇心重→conquer 攻伐、逐利→hoard 交易）→ 乘数 1/weight ∈ [0.625,1.0)，按比例降低抗命概率。
//
// 仅降不升（契合只让她更愿意听，不契合不额外加抗命——避免与既有人格/记忆/忠诚信号双重惩罚同一动作）；
// 与胆量曲线**乘项叠加**（同一连乘式里各占一项，互不替代）。纯函数、确定性、付费不进；actor 为 nil 时退 1.0（失败安全）。
// flag QUNXIANG_AMBITION_SCORING 关（默认）→ OnlineAmbitionActionWeight 恒 1.0 → 本乘数恒 1.0（零行为）。
func ambitionComplianceRejectModifier(actor *unit.Record, decision unitDecisionPayload) float64 {
	if actor == nil {
		return 1.0
	}
	weight := OnlineAmbitionActionWeight(*actor, OnlineActionAmbitionTag(decision.Action))
	if weight <= 1.0 {
		return 1.0 // 中性/未登记/flag 关：不调整（逐位一致）。
	}
	return 1.0 / weight // 契合：按比例降低抗命概率（仅降不升）。
}

// offlineHoursFromState 从会话状态的**墙钟**锚（UpdatedAt vs 传入 now）派生离线小时数。
//
// 【已不在决策概率链上】LOW② 确定性修复后，胆量曲线改由 offlineHoursFromTurns（纯回合差）喂数，
// 本函数不再被 offlineCourageRejectModifier 调用——保留它仅为：①暴露「墙钟离线时长」这一可读量给
// 非决策路径（如运维/调试展示，墙钟漂移在这些场景无害）；②文档化「为何不能把墙钟喂进决策」。
// **切勿**把本函数接回任何影响 directiveRejectProbability / 决策落地的链路，否则重新引入墙钟发散。
//
// 注入 now 而非内部直接 time.Now()，是为了让调用方/测试可注入固定墙钟、可复现。
// UpdatedAt 为零值（旧存档/未落库）→ 0；负差（时钟回拨）→ 0（失败安全）。
func offlineHoursFromState(state State, now time.Time) float64 {
	if state.UpdatedAt.IsZero() {
		return 0
	}
	delta := now.Sub(state.UpdatedAt)
	if delta <= 0 {
		return 0
	}
	return delta.Hours()
}

// DomainUnlockedAt 报告某自治领域在给定离线时长下是否已解锁（广度曲线的对外可读判定）。
//
// 供上层（如冻结清单 §5.3 / 决策前置过滤）按「当前广度」判断某领域决策是否应自决落地。
// 纯函数、确定性：日常恒解锁；经营 ≥6h；高代价 >24h（取 ≥，边界含）。flag 不在此判定内——
// 解锁判定是纯曲线定义，是否启用由调用方按 courageCurveEnabled 决定。
func DomainUnlockedAt(domain autonomyDomain, offlineHours float64) bool {
	switch domain {
	case autonomyDomainDaily:
		return true
	case autonomyDomainCivic:
		return offlineHours >= autonomyCivicUnlockHours
	case autonomyDomainHighRisk:
		return offlineHours >= autonomyHighRiskUnlockHours
	default:
		return true
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
