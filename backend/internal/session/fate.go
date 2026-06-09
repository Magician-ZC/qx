package session

// 文件说明：命运相关性与命运收件箱（设计宪法 §4 M1 的 session 落地）。
// 把「世界事件」按相关性翻译成「我的角色命运的一段」：构造角色的相关性锚（当前模型=关系锚，
// geo/redline/goal 待世界化后接入），用 engine/relevance 评分并三档路由（自治不打扰/高光卡/待决策），
// 待决策经 events.EmitProcessEvent 写入命运收件箱（流程事件，不改状态）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/analytics"
	"qunxiang/backend/internal/engine/encounter"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/narration"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

const (
	relationIntensityNorm  = 20.0 // 关系强度（四轴绝对值之和，[-10,10] 量级）归一化分母
	relationAnchorHalfLife = 14.0 // 关系锚半衰期（天）

	// redlineAnchorWeight 是宪法 §4.1 红线锚的权重：红线是「绝对禁区」，单根即应足够牵动她（取满权 1.0，
	// 经 relevance.RelativeImportance(Redline) 折算后仍是高位贡献）；severity 缺省也不下调（红线无小事）。
	redlineAnchorWeight = 1.0
	// redlineAnchorHalfLife<=0 表示红线锚不衰减（与 relevance.Anchor 约定一致：红线/传承是恒久的弦）。
	redlineAnchorHalfLife = 0.0

	// 命运分（FateScore）三因子的阻尼下限（设计宪法 §4.2）。
	// 不可逆度/情绪强度被建成「阻尼系数 ∈ [floor,1]」而非裸权重：高 stakes 事件取≈1（不衰减 careRelevance、
	// 保住既有路由），日常琐事才把分往下压。下限保证「单因子退化」不会让本该牵动她的事被误杀（回归来源）。
	fateIrreversibilityFloor = 0.70 // 不可逆度阻尼下限：再「可逆」的事也保留七成命运分
	fateEmotionFloor         = 0.70 // 情绪强度阻尼下限：情绪缺省（EmotionWeight=0）时不应清零命运

	// M2 防轰炸三条（设计宪法 §4.2「硬规则」）。
	fatePendingDailyBudget = 3              // 每自然日进「待决策」的命运节点上限：溢出降级高光卡，绝不轰炸玩家。
	fatePendingTTL         = 48 * time.Hour // 待决策倒计时 TTL：occurred_at + 此值即过期，到期按宪章兜底（§4.2 过期兜底）。

	// 过期兜底的缺省宪章动作（§4.2「过期兜底」）：玩家没回来 → 默认放手让她自己选，再补一张回响卡。
	fateExpiryDefaultResolve = "let_her"
)

// FateEvent 是一条可能与某角色命运相关的世界事件。
type FateEvent struct {
	ActorID       string
	TargetID      string
	ReasonCode    events.ReasonCode
	Importance    int
	EmotionWeight float64
	Summary       string // 事件一句话，用于命运卡
	// AttributionZH 是该事件/其 owner 决策的「归因因果句」（来自 decision.Attribution.NarrativeZH，≤40 字，
	// 形如「她突然抛下一切，因为她向往自由」）。非空时会被并入命运卡文本，让玩家一眼看到「凭什么会这样」。
	// 留空则跳过（不是所有世界事件都带归因）。
	AttributionZH string
	// SourceRegionID 是事件来源地（陌生 region 时驱动「破圈预算」升档，§1.5）。零值=未知/本地。
	SourceRegionID string
	// SourceIsStranger 标记事件来源是否为「陌生人/陌生地」（与 owner 无任何持久锚关联）。
	// 与「未命中任何锚」共同构成 isZeroAnchorEvent 的判定——为破圈预算挑选「素不相识却撞进她命里」的种子事件。
	SourceIsStranger bool
	// LegacyItemID 是「传家物升级」待决策卡所绑定的待升级装备 ID（§5 Clutch→Legacy 闭环）。非空且本事件路由到待决策时，
	// 随 payload.legacy_item_id 落库；玩家在该卡 confirm（urge 类）→ ResolveFateDecision 调 upgradeItemToLegacy 落 IsLegacy+SoulBound+Pinned。
	// 留空=非传家物升级卡（绝大多数命运事件）。
	LegacyItemID string
}

// FateRouting 是一条世界事件对某角色的命运路由结果。
type FateRouting struct {
	Route      relevance.FateRoute
	Relevance  float64
	DecisionID string // pending 时的待决策 ID（用于后续 resolve）
	Card       string // 祖魂语气命运卡
}

// FateInboxItem 是命运收件箱里的一条未决待决策。
type FateInboxItem struct {
	DecisionID string
	Narrative  string
	OccurredAt string
	// 过期倒计时（M2 防轰炸②，设计宪法 §4.2）：ExpiresAt = occurred_at + fatePendingTTL（RFC3339）。
	// CountdownHours 是「距过期还剩几小时」（向下取整、已过期为 0），纯函数派生、确定性，供前端渲染倒计时条。
	ExpiresAt      string `json:"expires_at"`
	CountdownHours int    `json:"countdown_hours"`
	// 情境化 Copilot 选项（§4.5）：入箱时按事件类型/红线/关系生成的情境化 choice（id+文案+后果类）。
	// 为空（旧存档/无情境）时前端回落旧三键（let_her/urge/acknowledge）。omitempty 保向后兼容。
	Choices []FateChoiceOut `json:"choices,omitempty"`
}

// FateChoiceOut 是回给前端的一个情境化选项：id 供 resolve、label 供渲染、resolve_class 标后果类（透明化）。
type FateChoiceOut struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	ResolveClass string `json:"resolve_class"`
}

// buildRelevanceAnchors 从角色的对外关系/持久锚构造相关性锚（关系锚 + relevance_anchors 表的目标/红线/债仇爱/血脉）。
// 无 State 上下文版本：宪法 §4.1 的「离线宪章红线」锚需要 State.UnitCharters，故由 buildRelevanceAnchorsWithState
// 在能拿到 state 时补上；此处传 nil，保持对 anchors.go/旧测试的向后兼容。
func (service *Service) buildRelevanceAnchors(ctx context.Context, unitID string) []relevance.Anchor {
	return service.buildRelevanceAnchorsWithState(ctx, nil, unitID)
}

// buildRelevanceAnchorsWithState 在 buildRelevanceAnchors 的关系锚 + 持久锚之上，叠加由 state.UnitCharters 派生的
// **离线宪章红线锚**（宪法 §4.1：红线是「绝对禁区」，应实打实牵动她的命运）。state 为 nil 时退化为旧行为（无红线锚）。
func (service *Service) buildRelevanceAnchorsWithState(ctx context.Context, state *State, unitID string) []relevance.Anchor {
	anchors := make([]relevance.Anchor, 0)
	if service == nil || service.db == nil {
		return anchors
	}
	seen := map[string]bool{}
	// 1) 持久锚（目标/红线/债仇爱/血脉——非关系锚，只有 relevance_anchors 表能存）。
	for _, a := range service.loadPersistentAnchors(ctx, unitID) {
		anchors = append(anchors, a)
		seen[string(a.Kind)+"|"+a.Ref] = true
	}
	// 2) 离线宪章红线锚（宪法 §4.1）：从该单位 charter 的每条红线生成一根 Redline 锚，让红线权重真正参与 FateScore。
	//    Ref 用稳定的红线 ID（fallbackRedlineID 派生过），半衰期 0=不衰减（红线恒久）。与持久锚去重（同 kind|ref 不重复）。
	for id := range charterRedlinesAsMap(state, unitID) {
		key := string(relevance.Redline) + "|" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		anchors = append(anchors, relevance.Anchor{
			Kind:         relevance.Redline,
			Ref:          id,
			Weight:       redlineAnchorWeight,
			HalfLifeDays: redlineAnchorHalfLife,
		})
	}
	// 3) 实时关系锚（由 relations 表派生；同 (kind,ref) 已有持久锚则不重复）。
	for _, r := range service.loadTopOutgoingRelations(ctx, unitID, 16) {
		key := string(relevance.Relation) + "|" + r.TargetUnitID
		if seen[key] {
			continue
		}
		weight := relationIntensity(r.Trust, r.Fear, r.Affection, r.Rivalry) / relationIntensityNorm
		if weight <= 0 {
			continue
		}
		if weight > 1 {
			weight = 1
		}
		anchors = append(anchors, relevance.Anchor{
			Kind:         relevance.Relation,
			Ref:          r.TargetUnitID,
			Weight:       weight,
			HalfLifeDays: relationAnchorHalfLife,
		})
	}
	return anchors
}

// eventRelevance 计算一条世界事件对某角色（其锚集）的相关性。
func eventRelevance(anchors []relevance.Anchor, ev FateEvent) float64 {
	score, _ := eventRelevanceWithAnchor(anchors, ev)
	return score
}

// eventRelevanceWithAnchor 返回相关性分，以及命中里权重最高的锚类别（用于翻译矩阵选「凭什么牵动她」的引子）。
func eventRelevanceWithAnchor(anchors []relevance.Anchor, ev FateEvent) (float64, string) {
	hits := make([]relevance.Hit, 0, len(anchors))
	topKind := ""
	topWeight := -1.0
	for _, a := range anchors {
		if !anchorHitByEvent(a, ev) {
			continue
		}
		hits = append(hits, relevance.Hit{Anchor: a})
		if a.Weight > topWeight {
			topWeight = a.Weight
			topKind = string(a.Kind)
		}
	}
	return relevance.Score(hits, 1.0), topKind
}

// anchorHitByEvent 判定一根锚是否被事件点亮。
//   - 红线锚（Ref=红线 ID，不是 unitID）：当事件 reason-code 属于「红线触线类」（背叛/叛变/势力崩塌/倒地/创伤
//     等覆水难收的越线事）即命中——这正是宪法 §4.1「红线被触碰理应实打实牵动她」的机制落地。
//   - 其余锚（关系/债仇爱/血脉/目标/地方…）：其 Ref 命中事件 actor/target 即命中（沿用原 ID 匹配）。
func anchorHitByEvent(a relevance.Anchor, ev FateEvent) bool {
	if a.Kind == relevance.Redline {
		return eventTripsRedlineClass(ev.ReasonCode)
	}
	return a.Ref != "" && (a.Ref == ev.ActorID || a.Ref == ev.TargetID)
}

// eventTripsRedlineClass 判定一条事件的 reason-code 是否属于「会触碰典型红线」的类别（覆水难收/越线的重大事）。
// 与 fateReasonIsIrreversibleClass 同源精神：背叛/势力崩塌/角色死亡/倒地濒死/创伤/被强令越线。确定性、纯函数。
func eventTripsRedlineClass(code events.ReasonCode) bool {
	switch code {
	case events.ReasonRelationBetray, // 背叛/叛变（最典型的红线触线）
		events.ReasonFactionCollapse, // 势力崩塌
		events.ReasonCharacterDied,   // 角色死亡
		events.ReasonCombatDown,      // 倒地濒死/阵亡
		events.ReasonEmotionTrauma,   // 创伤
		events.ReasonCommandForced,   // 被强令做了高风险/越线的事
		events.ReasonRedlineTrip:     // 显式判定的触线事件
		return true
	default:
		return false
	}
}

// SurfaceFateEvent 把一条世界事件按相关性路由进某角色的命运层。
// 自治不打扰：返回 RouteAutonomous，不写流程事件（底层事件已留痕）；高光卡/待决策：写入命运收件箱。
func (service *Service) SurfaceFateEvent(ctx context.Context, sessionID string, owner *unit.Record, ev FateEvent) (FateRouting, error) {
	if service == nil || service.db == nil || owner == nil {
		return FateRouting{}, fmt.Errorf("surface fate event: missing dependencies")
	}
	// best-effort 载入 state：用于 ① 离线宪章红线锚（§4.1）；② 真实在世天数（attachmentForUnit 内自取）；③ provenance。
	// 载入失败/sessions 未注入时退化为 nil（行为与旧路径一致，绝不阻断路由）。
	state := service.loadStateForFate(ctx, sessionID)
	var rel float64
	anchorKind := ""
	if ev.ActorID == owner.ID || ev.TargetID == owner.ID {
		// 直接发生在她身上 → 牵挂相关度由重要度/情绪强度决定（她自己的事一定相关，无外部锚）。
		rel = float64(ev.Importance) / 10.0
		if e := absFloat(ev.EmotionWeight); e > rel {
			rel = e
		}
		if rel > 1 {
			rel = 1
		}
	} else {
		// 发生在别人身上 → 经她的锚（含离线宪章红线锚）翻译牵挂相关度，并记下命中里最重的锚类别（翻译矩阵用）。
		rel, anchorKind = eventRelevanceWithAnchor(service.buildRelevanceAnchorsWithState(ctx, state, owner.ID), ev)
	}
	// §5「为什么会这样」对玩家可见：归因因果句缺省时，先走 §M1 data-driven 翻译层（按 reason-code × 锚类 查 DB
	// translation_templates 矩阵生成贴合个人的命运 beat，带内存缓存/三级回退）；命中则用更像「命运 beat」的措辞，
	// 未命中严格回落 deriveFateProvenance（与原行为逐字节一致）。调用方显式给了 AttributionZH 则尊重之。
	// forcePending 标志（密友倒地/密友被背叛/角色之死等「非看不可」组）由翻译矩阵携带，下方据此把路由硬抬到待决策。
	//
	// **仅在有命中锚（anchorKind != ""，即「别人身上经她的锚」）时查 DB 矩阵**——与原 translateFateBeat 的语义一致：
	// 自身事件（anchorKind=""，发生在她身上、无外部锚）不强行翻译，交还 deriveFateProvenance（无源不可逆事件由此保持
	// AttributionZH 为空，保住 §4.3「没有『为什么』时绝不悄悄落子」的归因强制不变量）。DB 矩阵的 anchor_kind='' 行是
	// 「命中锚但精确组缺表」的兜底，由 resolveTranslationTemplate 内部回退处理，不在此对自身事件误用。
	forcePending := false
	if anchorKind != "" {
		if beat, fp := translateFateBeatFromDB(ctx, service.db, ev, relevance.AnchorKind(anchorKind)); strings.TrimSpace(beat) != "" {
			forcePending = fp
			if strings.TrimSpace(ev.AttributionZH) == "" {
				ev.AttributionZH = beat
			}
		}
	}
	if strings.TrimSpace(ev.AttributionZH) == "" {
		ev.AttributionZH = deriveFateProvenance(ev, anchorKind)
	}
	// FateScore 三因子（设计宪法 §4.2）：不可逆度 × 牵挂相关度 × 情绪强度，三者 ∈ [0,1]。
	// careRelevance=rel（上面算出的关系/重要度相关性）；irreversibility/emotion 是 [floor,1] 的阻尼系数，
	// 高 stakes 事件取≈1（不衰减 careRelevance、与单因子退化等价、保住既有路由），日常琐事才被压低降档。
	fateScore := fateIrreversibility(ev) * rel * fateEmotionIntensity(ev)
	route := relevance.RouteFor(fateScore)
	// M2 防轰炸①：每自然日进「待决策」的命运节点 ≤ fatePendingDailyBudget（设计宪法 §4.2）。
	// 路由到待决策前查当天该 owner 的 PENDING_DECISION 计数，已达预算则**降级为高光卡**（RouteHighlight）而非
	// 待决策——绝不把溢出的命运节点继续堆给玩家。查不到/出错时保守放行（best-effort，不阻断既有路由）。
	if route == relevance.RoutePending && service.pendingBudgetExhausted(ctx, owner.ID) {
		route = relevance.RouteHighlight
	}
	// force_pending 硬抬（§1.2「force_pending 类必须停在玩家手里」）：翻译矩阵标了 force_pending 的组——密友倒地
	// COMBAT_DOWN×relation / 密友被背叛 RELATION_BETRAYAL×relation / 角色之死 CHARACTER_DIED×relation / 她所在地被劫
	// ECONOMY_LOOT×geo 等「非看不可」的命运节点——必须升到待决策，不能被自治悄悄吞掉或停在高光卡。
	// 放在防轰炸预算降级**之后**：这类覆水难收的节点是少数应当无条件打断玩家的事件，故凌驾于每日待决策预算之上。
	if forcePending && route != relevance.RoutePending {
		route = relevance.RoutePending
	}
	// 破圈预算（§1.5「破圈」）：每个自然日强制让 ≤1 件「零锚来源」低权事件升一档进高光卡，作为新锚的种子——
	// 让一处没去过的地方、一个素不相识的人有机会意外撞进她的命里，对冲「相关性闭环只反复触达老熟人/老地方」。
	// 仅在该事件本会被自治丢弃（RouteAutonomous）、且确属零锚来源、且当天破圈配额未用尽时触发；升一档后标
	// serendipity，落 ReasonSerendipityNewAnchor。flag 默认关（QUNXIANG_SERENDIPITY），关时零行为。
	// §8 OOC 收紧：一致性收紧态下破圈升档暂缓（serendipityPausedByTightening）——玩家正觉得「太离谱」时先稳住一致性，别再塞陌生意外。
	serendipity := false
	if route == relevance.RouteAutonomous && serendipityEnabled() && !serendipityPausedByTightening() &&
		isZeroAnchorEvent(ev, owner, anchorKind) && !service.serendipityBudgetExhausted(ctx, owner.ID) {
		route = relevance.RouteHighlight
		serendipity = true
	}
	out := FateRouting{Route: route, Relevance: fateScore}
	if route == relevance.RouteAutonomous {
		return out, nil
	}

	out.Card = fateCard(ev, route, anchorKind)
	code := events.ReasonInboxHighlight
	// 破圈升档的卡用 ReasonSerendipityNewAnchor 留痕（与普通高光区分；它是「新锚种子」，供后续锚化消费）。
	if serendipity {
		code = events.ReasonSerendipityNewAnchor
	}
	payload := map[string]any{
		"narrative":         out.Card,
		"relevance":         fateScore, // 命运分（三因子相乘后的最终分，用于路由）
		"care_relevance":    rel,       // 牵挂相关度因子（关系/重要度锚）
		"irreversibility":   fateIrreversibility(ev),
		"emotion_intensity": fateEmotionIntensity(ev),
		"source_actor":      ev.ActorID,
		"source_target":     ev.TargetID,
		"reason":            string(ev.ReasonCode),
	}
	// 破圈来源地随 payload 落库，供「新锚锚化」消费（哪个陌生 region/陌生人撞进了她的命里）。
	if serendipity {
		payload["serendipity"] = true
		if r := strings.TrimSpace(ev.SourceRegionID); r != "" {
			payload["source_region"] = r
		}
	}
	// 归因因果句随 payload 落库（§4.3 provenance 凭证 + 渲染管线）：非空才写，保持向后兼容（旧消费方忽略未知键）。
	if cause := strings.TrimSpace(ev.AttributionZH); cause != "" {
		payload["attribution_zh"] = cause
	}
	// 传家物升级待决策（§5 Clutch→Legacy 闭环）：绑定的待升级装备 ID 随 payload 落库，供 ResolveFateDecision 在 confirm 时反查并升级。
	if legacyItemID := strings.TrimSpace(ev.LegacyItemID); legacyItemID != "" {
		payload["legacy_item_id"] = legacyItemID
	}
	if route == relevance.RoutePending {
		code = events.ReasonPendingDecision
		out.DecisionID = "fd_" + uuid.NewString()
		payload["decision_id"] = out.DecisionID
		// 情境化 Copilot（§4.5）：按事件类型/红线/关系生成情境化选项，连同其后果类（resolve_class）写进 payload，
		// 供 ResolveFateDecision 在玩家选了某情境 id 时反查后果。仍保留旧三键（let_her/urge/acknowledge）始终可用。
		if choices := buildFateChoices(ev, anchorKind); len(choices) > 0 {
			payload["choices"] = fateChoicesToPayload(choices)
		}
	}
	// 关联单位：默认指向事件 target。但破圈事件的 target 是「陌生人」（不在 units 表）——直接落 target_unit_id 会
	// 触发 FK 约束失败；故破圈路径不设 RelatedUnitID（陌生人/陌生地信息已在 payload.source_actor/source_region），
	// 让 EmitProcessEvent 回落 owner，绝不让破圈写卡因外键失败而炸。
	relatedUnitID := ev.TargetID
	if serendipity {
		relatedUnitID = ""
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:     sessionID,
		OwnerUnitID:   owner.ID,
		RelatedUnitID: relatedUnitID,
		Code:          code,
		Category:      events.CategoryFate,
		Payload:       payload,
	}); err != nil {
		return out, err
	}
	// 漏斗埋点（best-effort）：待决策入箱是北极星「D2 收件箱处理率」的分母。
	if route == relevance.RoutePending {
		_ = analytics.Emit(ctx, service.db, analytics.Event{
			Stage: analytics.StageRetention, Name: analytics.EventDecisionPending,
			SessionID: sessionID, UnitID: owner.ID,
			Props: map[string]any{"decision_id": out.DecisionID, "relevance": fateScore},
		})
	}
	// 实时推送（best-effort）：让前端命运收件箱无需轮询即可即时看到新的高光/待决策卡。
	service.pushRealtime(sessionID, "fate_inbox", map[string]any{
		"unit_id":     owner.ID,
		"route":       string(route),
		"decision_id": out.DecisionID,
		"narrative":   out.Card,
		"relevance":   fateScore,
	})
	return out, nil
}

// pendingBudgetExhausted 判断某 owner 当天（UTC 自然日）进「待决策」的命运节点是否已达 fatePendingDailyBudget。
// occurred_at 以 RFC3339Nano（UTC）写入，故同一 UTC 日的事件共享 "YYYY-MM-DD" 前缀；用 [dayStart, nextDayStart)
// 字符串区间过滤即可按日计数——双驱动安全、不依赖任何 SQL 日期函数、确定性。出错保守返回 false（放行）。
func (service *Service) pendingBudgetExhausted(ctx context.Context, ownerID string) bool {
	if service == nil || service.db == nil || ownerID == "" {
		return false
	}
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lo := dayStart.Format(time.RFC3339Nano)
	hi := dayStart.Add(24 * time.Hour).Format(time.RFC3339Nano)
	var count int
	if err := service.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events
		 WHERE actor_unit_id = ? AND reason_code = ? AND occurred_at >= ? AND occurred_at < ?`,
		ownerID, string(events.ReasonPendingDecision), lo, hi,
	).Scan(&count); err != nil {
		return false // best-effort：查询失败不阻断路由，保守放行
	}
	return count >= fatePendingDailyBudget
}

// ===== §1.5 破圈预算（serendipity）：每日强制 ≤1 件零锚来源事件升档进高光卡作新锚种子 =====

// serendipityDailyBudget 是每个自然日（每 owner）破圈升档的硬上限：≤1 件——只放一道窄缝，避免「破圈」反噬成噪声。
const serendipityDailyBudget = 1

// serendipityEnabled 读 QUNXIANG_SERENDIPITY（true/1/yes/on 视为开），默认关 → 破圈逻辑零行为（与既有 flag 风格一致）。
func serendipityEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_SERENDIPITY"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// isZeroAnchorEvent 判断一条事件对某 owner 是否为「零锚来源」种子候选（自包含、不跨 agent 依赖 worldize_inbound.go）：
//   - 未命中任何锚：anchorKind 为空（eventRelevanceWithAnchor 没点亮任何持久/关系/红线锚）；
//   - 来源是陌生人：事件 actor/target 都不是 owner 自己（她自己的事不是「破圈」），且事件显式标了 SourceIsStranger
//     或带了非空 SourceRegionID（陌生 region）——即「素不相识的人/没去过的地方」。
//
// 纯函数、确定性、可测。她自己的事（ActorID/TargetID==owner）一律不算破圈（那是自身命运，不是新锚）。
func isZeroAnchorEvent(ev FateEvent, owner *unit.Record, anchorKind string) bool {
	if owner == nil {
		return false
	}
	if ev.ActorID == owner.ID || ev.TargetID == owner.ID {
		return false // 自身事件不是「破圈」
	}
	if strings.TrimSpace(anchorKind) != "" {
		return false // 命中了某锚 → 非零锚
	}
	return ev.SourceIsStranger || strings.TrimSpace(ev.SourceRegionID) != ""
}

// serendipityBudgetExhausted 判断某 owner 当天（UTC 自然日）的破圈升档配额是否已用尽（≥serendipityDailyBudget）。
// 计数 ReasonSerendipityNewAnchor 当日事件数——与 pendingBudgetExhausted 同一套 [dayStart,nextDay) 字符串区间过滤，
// 确定性、双驱动安全、不依赖 SQL 日期函数。出错保守返回 true（不冒进升档，宁可少破圈、不噪声）。
func (service *Service) serendipityBudgetExhausted(ctx context.Context, ownerID string) bool {
	if service == nil || service.db == nil || ownerID == "" {
		return true
	}
	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	lo := dayStart.Format(time.RFC3339Nano)
	hi := dayStart.Add(24 * time.Hour).Format(time.RFC3339Nano)
	var count int
	if err := service.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events
		 WHERE actor_unit_id = ? AND reason_code = ? AND occurred_at >= ? AND occurred_at < ?`,
		ownerID, string(events.ReasonSerendipityNewAnchor), lo, hi,
	).Scan(&count); err != nil {
		return true // best-effort：查询失败时不冒进升档（与 pending 配额「放行」相反，破圈宁缺毋滥）
	}
	return count >= serendipityDailyBudget
}

// OpenFateInbox 返回某角色未被处理的待决策（命运收件箱），每条带过期倒计时（M2 防轰炸②）。
// 打开收件箱时还会做 M2 防轰炸③「过期兜底」：把**超期未决**的 PENDING 按宪章兜底（默认 let_her）自动关掉并补回响卡，
// best-effort、失败不阻断（详见 reclaimExpiredPending）。打开动作本身 best-effort 埋点 EventInboxOpened（留存阶段）。
func (service *Service) OpenFateInbox(ctx context.Context, unitID string) ([]FateInboxItem, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("open fate inbox: missing db")
	}
	// 埋点（best-effort）：「打开收件箱」是留存阶段的关键动作；EventInboxOpened 由 analytics 包统一登记。
	_ = analytics.Emit(ctx, service.db, analytics.Event{
		Stage: analytics.StageRetention, Name: analytics.EventInboxOpened,
		UnitID: unitID,
	})
	// M2 防轰炸③：先把超期未决的 PENDING 按宪章兜底关掉（best-effort，错误不阻断本次打开）。
	service.reclaimExpiredPending(ctx, unitID)

	resolved, err := service.resolvedDecisionIDs(ctx, unitID)
	if err != nil {
		return nil, err
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT payload_json, occurred_at FROM events
		 WHERE actor_unit_id = ? AND reason_code = ?
		 ORDER BY occurred_at DESC`,
		unitID, string(events.ReasonPendingDecision),
	)
	if err != nil {
		return nil, fmt.Errorf("query fate inbox: %w", err)
	}
	defer rows.Close()

	items := make([]FateInboxItem, 0)
	for rows.Next() {
		var payloadJSON, occurredAt string
		if err := rows.Scan(&payloadJSON, &occurredAt); err != nil {
			return nil, fmt.Errorf("scan fate inbox: %w", err)
		}
		var payload struct {
			DecisionID string          `json:"decision_id"`
			Narrative  string          `json:"narrative"`
			Choices    []payloadChoice `json:"choices"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID == "" || resolved[payload.DecisionID] {
			continue
		}
		items = append(items, FateInboxItem{
			DecisionID:     payload.DecisionID,
			Narrative:      payload.Narrative,
			OccurredAt:     occurredAt,
			ExpiresAt:      fateExpiresAt(occurredAt),
			CountdownHours: fateCountdownHours(occurredAt),
			Choices:        payloadChoicesToOut(payload.Choices),
		})
	}
	return items, rows.Err()
}

// FateFeedItem 是命运四槽界面的一张卡（高光/待决策/回响）。
type FateFeedItem struct {
	Kind       string `json:"kind"`        // highlight / pending / echo
	DecisionID string `json:"decision_id"` // pending 时可处理
	Narrative  string `json:"narrative"`
	OccurredAt string `json:"occurred_at"`
	// 仅 pending 卡有意义（M2 防轰炸②）：过期时间与剩余小时数，让首屏待决策卡显示倒计时。高光/回响卡置零。
	ExpiresAt      string `json:"expires_at,omitempty"`
	CountdownHours int    `json:"countdown_hours,omitempty"`
	// 仅 pending 卡有意义（§4.5）：情境化 Copilot 选项。空时前端回落旧三键。omitempty 保向后兼容。
	Choices []FateChoiceOut `json:"choices,omitempty"`
}

// OpenFateFeed 返回某角色命运四槽的最近卡片（高光 + 未决待决策 + 回响），按时间倒序。供前端首屏渲染。
func (service *Service) OpenFateFeed(ctx context.Context, unitID string, limit int) ([]FateFeedItem, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("open fate feed: missing db")
	}
	if limit <= 0 {
		limit = 30
	}
	resolved, err := service.resolvedDecisionIDs(ctx, unitID)
	if err != nil {
		return nil, err
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT reason_code, payload_json, occurred_at FROM events
		WHERE actor_unit_id = ? AND reason_code IN (?, ?, ?, ?)
		ORDER BY occurred_at DESC LIMIT ?`,
		unitID, string(events.ReasonInboxHighlight), string(events.ReasonPendingDecision),
		string(events.ReasonEchoLink), string(events.ReasonSerendipityNewAnchor), limit)
	if err != nil {
		return nil, fmt.Errorf("query fate feed: %w", err)
	}
	defer rows.Close()

	items := make([]FateFeedItem, 0)
	for rows.Next() {
		var code, payloadJSON, occurredAt string
		if err := rows.Scan(&code, &payloadJSON, &occurredAt); err != nil {
			return nil, fmt.Errorf("scan fate feed: %w", err)
		}
		var payload struct {
			DecisionID string          `json:"decision_id"`
			Narrative  string          `json:"narrative"`
			Choices    []payloadChoice `json:"choices"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		item := FateFeedItem{Narrative: payload.Narrative, OccurredAt: occurredAt}
		switch code {
		case string(events.ReasonPendingDecision):
			if payload.DecisionID == "" || resolved[payload.DecisionID] {
				continue // 已处理的待决策不再展示
			}
			item.Kind = "pending"
			item.DecisionID = payload.DecisionID
			item.ExpiresAt = fateExpiresAt(occurredAt) // M2②：仅待决策卡带倒计时
			item.CountdownHours = fateCountdownHours(occurredAt)
			item.Choices = payloadChoicesToOut(payload.Choices)
		case string(events.ReasonEchoLink):
			item.Kind = "echo"
		default:
			item.Kind = "highlight"
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// fateConsequence 描述一种待决策处理方式对该角色的真实后果（一条或多条经 Mutator 落地的状态变更）。
type fateConsequence struct {
	Field      status.Field
	Delta      float64
	ReasonCode events.ReasonCode
	ReasonText string
}

// fateConsequencesFor 把待决策的处理方式（resolveType）映射成可复算的真实后果（设计宪法 §4.6）。
// 只用既有 reason-code（不新增）：let_her→放手让她自主（被尊重，士气/忠诚小幅↑）；urge→玩家督促干预
// （短期更听话但有怨：忠诚↓、士气微↑）；acknowledge/缺省→只是知悉，轻微暖意（士气微↑）。确定性、纯函数。
func fateConsequencesFor(resolveType string) []fateConsequence {
	switch strings.ToLower(strings.TrimSpace(resolveType)) {
	case "let_her", "let-her", "let_go", "autonomy":
		return []fateConsequence{
			{Field: status.FieldMorale, Delta: +0.05, ReasonCode: events.ReasonEmotionReward, ReasonText: "你放手让她自己做主，她受到尊重"},
			{Field: status.FieldLoyalty, Delta: +0.03, ReasonCode: events.ReasonCommandAccepted, ReasonText: "你尊重她的选择，她更信你了"},
		}
	case "urge", "intervene", "push", "command":
		return []fateConsequence{
			{Field: status.FieldLoyalty, Delta: -0.04, ReasonCode: events.ReasonCommandForced, ReasonText: "你越界替她做了决定，她心生芥蒂"},
			{Field: status.FieldMorale, Delta: +0.02, ReasonCode: events.ReasonCommandForced, ReasonText: "被你督促后她暂时打起了精神"},
		}
	case "acknowledge", "ack", "noted", "":
		return []fateConsequence{
			{Field: status.FieldMorale, Delta: +0.01, ReasonCode: events.ReasonEmotionReward, ReasonText: "知道有人在远方惦记着，她心里暖了一下"},
		}
	default:
		// 未知处理方式：保守地只给一点暖意，绝不施加负面后果。
		return []fateConsequence{
			{Field: status.FieldMorale, Delta: +0.01, ReasonCode: events.ReasonEmotionReward, ReasonText: "这件事有了回应，她心里踏实了一点"},
		}
	}
}

// fateConsequenceLayer 是 M3 后果分级闸（设计宪法 §4.3/§4.6）：在 fateConsequencesFor 的基础后果上叠一层
// 「牵挂调节」——返回经 attachment（牵挂等级 [0,100]）幅度调节后的后果。**单调**：牵挂越高，「urge 越界干预」
// 的 loyalty 代价越大（祖魂替她做主，她在乎你越深、被越界时的芥蒂越深，对齐宪法「不可逆需牵挂建立后才开放」）。
// 正向后果（let_her/acknowledge 的暖意）不随牵挂膨胀，避免「越牵挂越能薅好处」；只放大「越界干预的代价」。
// 纯函数、确定性、可测；仍只输出既有 reason-code、由调用方经 status.Mutator 落地。
func fateConsequenceLayer(resolveType string, attachment float64) []fateConsequence {
	base := fateConsequencesFor(resolveType)
	// 牵挂归一到 [0,1]，并夹到 [0,100] 量级（ComputeAttachment 的值域）。
	a := attachment / 100.0
	if a < 0 {
		a = 0
	}
	if a > 1 {
		a = 1
	}
	// 越界干预（urge）的 loyalty 代价随牵挂线性放大：a=0 取 1.0×，a=1 取 fateUrgeCostMaxScale×（单调不减）。
	urgeScale := 1.0 + a*(fateUrgeCostMaxScale-1.0)
	out := make([]fateConsequence, len(base))
	copy(out, base)
	if isUrgeResolve(resolveType) {
		for i := range out {
			// 只放大「越界代价」那一项（loyalty 负向）；正向的「暂时打起精神」不放大。
			if out[i].Field == status.FieldLoyalty && out[i].Delta < 0 {
				out[i].Delta *= urgeScale
			}
		}
	}
	return out
}

const fateUrgeCostMaxScale = 2.0 // urge 越界 loyalty 代价在满牵挂时的最大放大倍数（a=1 → 2×，单调）。

// isUrgeResolve 判断处理方式是否属于「玩家越界督促/替她做主」一类（与 fateConsequencesFor 的 urge 分支同义词集对齐）。
func isUrgeResolve(resolveType string) bool {
	switch strings.ToLower(strings.TrimSpace(resolveType)) {
	case "urge", "intervene", "push", "command":
		return true
	default:
		return false
	}
}

// fateReasonIsIrreversibleClass 判断一条命运事件的 reason-code 是否属于「不可逆类」（死亡/叛变/pinned 永久丢失）。
// 对齐 §4.3 层3 与 encounter 的 D0-D3 硬锁不可逆精神：这类命运在高牵挂下**拒绝**自动 let_her 兜底，必须显式停在玩家手里。
func fateReasonIsIrreversibleClass(code events.ReasonCode) bool {
	switch code {
	case events.ReasonCombatDown, // 倒地濒死/阵亡
		events.ReasonCharacterDied,   // 角色死亡
		events.ReasonRelationBetray,  // 背叛/叛变
		events.ReasonFactionCollapse: // 势力崩塌（覆水难收的大变故）
		return true
	default:
		return false
	}
}

// fateRefusesAutoLetHer 判断「过期兜底」是否应**拒绝**对该 PENDING 自动 let_her——
// 不可逆类命运 且 牵挂达到不可逆解锁线（≥fateIrreversibleAttachmentGate）时为真：此时绝不替玩家放手，
// 而是把它继续留在收件箱，等玩家显式处理（对齐宪法 §4.3「不可逆需牵挂建立后开放」+ D0-D3 硬锁不可逆）。
func fateRefusesAutoLetHer(code events.ReasonCode, attachment float64) bool {
	return fateReasonIsIrreversibleClass(code) && attachment >= fateIrreversibleAttachmentGate
}

const fateIrreversibleAttachmentGate = 70.0 // 不可逆后果的牵挂解锁线（§4.3 层3 牵挂≥70），到线即拒绝自动兜底。

// ResolveFateDecision 处理一条待决策：先**校验归属 + 原子抢占去重**，唯一赢家才经 status.Mutator 对该角色
// 施加真实后果（可复算、留痕），再写 DECISION_RESOLVED 标记（让它出箱）。
//
// 安全/幂等（评审 load-bearing 修复）：① 按 decisionID 查权威 PENDING_DECISION 事件取其 owner，**忽略客户端传入的
// unitID**——杜绝伪造 decisionID 或跨单位 unitID 凭空刷 morale/loyalty；查不到（伪造/已非 pending）即拒绝、不施任何后果。
// ② 以 decision_id 为主键写 fate_decision_resolutions 去重表作为**原子抢占闸**：重复/并发 POST 中只有唯一 INSERT 赢家
// 施加后果，主键冲突者幂等 no-op——根除「同一命运决断被刷成多果」（设计宪法 §4.6 一事一果、可复算）。
func (service *Service) ResolveFateDecision(ctx context.Context, sessionID string, unitID string, decisionID string, resolveType string) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("resolve fate decision: missing db")
	}
	decisionID = strings.TrimSpace(decisionID)
	if decisionID == "" {
		return fmt.Errorf("resolve fate decision: empty decision id")
	}

	// 1) 归属校验：以权威 PENDING_DECISION 事件的 owner 为准（忽略客户端传入 unitID），不存在即拒绝。
	//    同时取回该 pending 的权威 payload（reason-code 类别 / 情境化 choices / provenance），供情境选项反查与不可逆守门。
	ownerID, meta, found, err := service.pendingDecisionMeta(ctx, decisionID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("resolve fate decision: decision %s not found or not pending", decisionID)
	}
	unitID = ownerID

	// 1.5) 情境化 Copilot（§4.5）：玩家可能传的是情境化选项 id（如 avenge / mourn / forbid…）。
	//      先把它映射回基础后果类（resolve_class ∈ {let_her,urge,acknowledge}）——映射不到则回落原值走旧三键。向后兼容。
	resolveClass := resolveFateChoiceClass(resolveType, meta.Choices)

	// 1.6) §4.3 provenance 强制（对齐 generateUnitDecision 的归因强制思路）：不可逆类后果（死亡/叛变/势力崩塌）
	//      要求该 pending 携带可解析前因（payload.attribution_zh 非空）。无源则**拒绝**自动兜底——绝不在没有「为什么」的
	//      情况下让一桩覆水难收的命运被悄悄落子。仅约束「越界干预(urge)」这种会施加不可逆负向后果的处理；
	//      let_her/acknowledge（放手/知悉）是安全降级路径，始终放行（与归因校验「不过则回退安全决策」同构）。
	if fateReasonIsIrreversibleClass(meta.ReasonCode) && isUrgeResolve(resolveClass) && strings.TrimSpace(meta.AttributionZH) == "" {
		return fmt.Errorf("resolve fate decision: irreversible consequence requires provenance (decision %s, reason %s)", decisionID, meta.ReasonCode)
	}

	// 2) 原子抢占：唯一赢家继续；重复/并发者幂等 no-op 返回（不重复施加后果）。落 resolveType 原文，保留玩家选的情境 id 供复盘。
	won, err := service.claimFateDecision(ctx, decisionID, unitID, resolveType)
	if err != nil {
		return err
	}
	if !won {
		return nil
	}

	// 3) 留痕：写 DECISION_RESOLVED 标记（即便后果失败也要写，保证收件箱能出箱、不卡死）。
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		Code:        events.ReasonDecisionResolved,
		Category:    events.CategoryFate,
		Payload:     map[string]any{"decision_id": decisionID, "resolve_type": resolveType},
	}); err != nil {
		return err
	}
	// 漏斗埋点（best-effort）：处理一条待决策 = 北极星留存动作。
	_ = analytics.Emit(ctx, service.db, analytics.Event{
		Stage: analytics.StageRetention, Name: analytics.EventDecisionResolved,
		SessionID: sessionID, UnitID: unitID,
		Props: map[string]any{"decision_id": decisionID, "resolve_type": resolveType},
	})

	// 4) 真后果：赢家才经 Mutator 改 morale/loyalty（字段级 clamp + 标准化事件留痕，可被复算）。
	// M3 后果分级闸：先算该角色的牵挂等级（含真实在世天数），再用 fateConsequenceLayer 据牵挂调节后果幅度
	// （urge 越界代价随牵挂单调放大）；用基础后果类 resolveClass（情境 id 已折算）取后果。
	attachment := service.attachmentForUnit(ctx, unitID)
	consequences := fateConsequenceLayer(resolveClass, attachment)
	// 4.5) D0-D3 真分级闸（§4.3 M3 逐条门槛）：把不可逆类的负向后果经 fatePenaltyCapPrecise 降级到该角色当前允许的最重层。
	//      不再用 encounter.DegradePenalty 的连续近似（care≥40||days≥3 笼统过层2、care≥70&&days≥7 即过层3），而是逐条判定：
	//      层2 需 care≥40 或 在世≥3 天（离乡/失盟类需 care≥50）；层3 需 care≥70 且 在世≥7 天 且 已发生≥1 次层2（三条 AND），
	//      且还须过 RequiresConsent（本地命运后果默认无跨玩家同意 → 层3 降层2）。萍水相逢/陪伴尚浅的角色被硬锁在「可恢复」。
	//      可逆类后果（日常暖意/小幅波动）不进闸，照原样落地。
	gateLandedLayer2 := false
	if fateReasonIsIrreversibleClass(meta.ReasonCode) {
		days := service.daysAliveForUnit(ctx, unitID)
		// 「已发生≥1 次层2」的前提：查该角色历史是否落过至少一次层2 高代价后果（M3 层3 第三条 AND）。
		priorLayer2 := service.fateHasPriorLayer2(ctx, unitID)
		// 本地命运后果闸不经跨玩家 consent 流程 → consentSatisfied=false（层3 须由 §2.5 consent_gate 的 RequiresConsent 路径解锁）。
		consequences, gateLandedLayer2 = gateFateConsequencesByPenaltyPrecise(consequences, meta.ReasonCode, attachment, days, priorLayer2, false)
	}
	var consequenceErr error
	if service.mutator != nil {
		for _, c := range consequences {
			if _, err := service.mutator.Apply(ctx, status.Mutation{
				UnitID:     unitID,
				Turn:       0, // 待决策在回合循环外处理，turn 用 0 标记
				Field:      c.Field,
				Delta:      c.Delta,
				ReasonCode: c.ReasonCode,
				ReasonText: c.ReasonText,
				Actors:     []string{unitID},
			}); err != nil {
				consequenceErr = fmt.Errorf("apply fate consequence (%s/%s): %w", resolveClass, c.Field, err)
				break
			}
		}
	}
	// 4.6) 层2 留痕（M3 层3 第三条 AND 的累计来源）：本闸实际落地了一次层2 高代价负向后果 → 补一条 ReasonDefeatScarred 标记，
	//      使「已发生≥1 次层2」可被后续 fateHasPriorLayer2 查到（层3 因此能在「先吃过一次高代价」后真正解锁）。best-effort、不阻断。
	if gateLandedLayer2 && consequenceErr == nil {
		service.recordFateLayer2Scar(ctx, sessionID, unitID, meta.ReasonCode)
	}

	// 5) 传家物升级闭环（§5 Clutch→Legacy）：本卡绑定了待升级装备 ID 且玩家选了「confirm（urge 类）」→ 把它刻成传家物
	//    （upgradeItemToLegacy 落 IsLegacy+SoulBound+Pinned + ReasonLegacyBequeathed 留痕；此后 GateSurprise(sell_pinned)→Reject）。
	//    let_her/acknowledge（婉拒/搁置）则不升级——是否刻成由玩家在此卡里点头。best-effort：升级失败只 log、不污染已落地的后果。
	if strings.TrimSpace(meta.LegacyItemID) != "" && isUrgeResolve(resolveClass) {
		if state := service.loadStateForFate(ctx, sessionID); state != nil {
			if _, err := service.upgradeItemToLegacy(ctx, state, unitID, meta.LegacyItemID); err != nil {
				slog.Warn("legacy upgrade on fate confirm failed (best-effort)", "unit", unitID, "item", meta.LegacyItemID, "err", err)
			}
		}
	}
	return consequenceErr
}

// gateFateConsequencesByPenalty 是 §4.3 旧版「连续近似」后果闸（encounter.DegradePenalty 的 care≥40||days≥3 / care≥70&&days≥7 笼统门）。
// 保留供既有测试与回退；生产路径已升级为 gateFateConsequencesByPenaltyPrecise（逐条硬门槛 + 层3 三条 AND + consent 守门）。
// 把负向项按 |delta| 量级映射到候选层、降级到 PenaltyCap 允许的最重层，再按层重标 |delta|；正向后果原样保留。
func gateFateConsequencesByPenalty(consequences []fateConsequence, care float64, daysAlive int) []fateConsequence {
	out := make([]fateConsequence, len(consequences))
	copy(out, consequences)
	for i := range out {
		if out[i].Delta >= 0 {
			continue // 正向后果（暖意/打起精神）不进惩罚闸
		}
		candidate := penaltyLayerForMagnitude(absFloat(out[i].Delta))
		gated := encounter.DegradePenalty(candidate, care, daysAlive)
		out[i].Delta = -fatePenaltyMagnitudeForLayer(gated)
	}
	return out
}

// gateFateConsequencesByPenaltyPrecise 把一组命运后果里的「负向」项经 §4.3 M3 **逐条硬门槛**（fatePenaltyCapPrecise）映射到候选层，
// 再降级到该角色当前允许的最重层，并按降级后的层重标 |delta|——绝不让萍水相逢的角色挨到不该挨的不可逆重击
// （与 threat.go 的 defeatMoraleHitForLayer 同一套层→幅度映射）。正向后果原样保留。
// reasonCode 决定层2 是否走「离乡/失盟」更严门槛（care≥50）；priorLayer2/consentSatisfied 是层3 三条 AND + consent 守门的输入。
// 第二返回值 landedLayer2=本次是否实际落地了一条层2 量级负向后果（供调用方补 ReasonDefeatScarred 层2 标记，喂养层3 第三条 AND）。
func gateFateConsequencesByPenaltyPrecise(consequences []fateConsequence, reasonCode events.ReasonCode, care float64, daysAlive int, priorLayer2, consentSatisfied bool) ([]fateConsequence, bool) {
	out := make([]fateConsequence, len(consequences))
	copy(out, consequences)
	cap := fatePenaltyCapPrecise(reasonCode, care, daysAlive, priorLayer2, consentSatisfied)
	landedLayer2 := false
	for i := range out {
		if out[i].Delta >= 0 {
			continue // 正向后果（暖意/打起精神）不进惩罚闸
		}
		candidate := penaltyLayerForMagnitude(absFloat(out[i].Delta))
		gated := candidate
		if gated > cap { // 降级到逐条门槛允许的最重层（min(候选, cap)）；满足条件的候选层原样保留。
			gated = cap
		}
		if gated >= 2 {
			landedLayer2 = true
		}
		// 降级后按层重标负向幅度（层越低代价越轻）；同层则原样。
		out[i].Delta = -fatePenaltyMagnitudeForLayer(gated)
	}
	return out, landedLayer2
}

// fatePenaltyCapPrecise 是 §4.3 M3 后果分级闸的**逐条硬门槛**（取代 encounter.PenaltyCap 的连续近似）。确定性、纯函数、可测：
//   - 层1 可恢复：始终可达（不满足层2/层3 即落层1）。
//   - 层2 高代价：care≥40 或 在世≥3 天即解锁；但「离乡/失盟」类（背叛盟友/被迫离乡，fateReasonIsExileOrAllianceLoss）更严，需 care≥50。
//   - 层3 不可逆：三条 AND——care≥70 且 在世≥7 天 且 已发生≥1 次层2（priorLayer2）；再 AND 过 RequiresConsent
//     （consentSatisfied，本地命运后果默认 false → 层3 降层2，须由 §2.5 consent_gate 的 RequiresConsent 路径显式解锁）。
//
// 逐条（非整体 care 门）：层3 缺任一条即降层2；层2 不满足即降层1。无源/不满足绝不落不可逆（对齐宪法「不可逆需牵挂建立后才开放」）。
func fatePenaltyCapPrecise(reasonCode events.ReasonCode, care float64, daysAlive int, priorLayer2, consentSatisfied bool) int {
	// 层3：care≥70 ∧ 在世≥7 天 ∧ 已发生≥1 次层2 ∧ 过 RequiresConsent（四条全过才到层3）。
	if care >= fateLayer3CareGate && daysAlive >= fateLayer3DaysGate && priorLayer2 && consentSatisfied {
		return 3
	}
	// 层2：默认 care≥40 或 在世≥3 天；「离乡/失盟」类抬高 care 门槛到 ≥50（在世天数门不变）。
	care2Gate := fateLayer2CareGate
	if fateReasonIsExileOrAllianceLoss(reasonCode) {
		care2Gate = fateLayer2ExileCareGate
	}
	if care >= care2Gate || daysAlive >= fateLayer2DaysGate {
		return 2
	}
	return 1
}

const (
	fateLayer2CareGate      = 40.0 // 层2 高代价默认牵挂门槛（§4.3）。
	fateLayer2ExileCareGate = 50.0 // 离乡/失盟类层2 的更严牵挂门槛（§4.3：离乡/背叛盟友需 care≥50）。
	fateLayer2DaysGate      = 3    // 层2 在世天数门槛（§4.3：在世≥3 天）。
	fateLayer3CareGate      = 70.0 // 层3 不可逆牵挂门槛（§4.3）。
	fateLayer3DaysGate      = 7    // 层3 不可逆在世天数门槛（§4.3：在世≥7 天）。
)

// fateReasonIsExileOrAllianceLoss 判断一条命运 reason-code 是否属于「离乡/失盟」类——§4.3 层2 里需要更严牵挂门槛（care≥50）的子集：
// 背叛/被盟友背叛（失盟）、势力崩塌（被迫离乡的典型诱因）、跨玩家背刺/离队（被信任的人推开）。确定性、纯函数。
func fateReasonIsExileOrAllianceLoss(code events.ReasonCode) bool {
	switch code {
	case events.ReasonRelationBetray, // 遭遇背叛/失盟
		events.ReasonFactionCollapse, // 势力崩塌（被迫离乡）
		events.ReasonCrossBetrayal,   // 跨玩家背刺（信任的人反咬）
		events.ReasonCrossPartyLeave: // 离队（盟散）
		return true
	default:
		return false
	}
}

// fateHasPriorLayer2 判断某角色历史上是否已落过至少一次「层2 高代价」后果（M3 层3 第三条 AND「已发生≥1 次层2」）。
// 口径：复用既有的层2 标记 reason-code ReasonDefeatScarred（PVE_DEFEAT_D2，threat.go 落层2 后果时即用），凡存在 ≥1 条即为真。
// 本命运闸落地一次层2 负向后果时也补一条该标记（recordFateLayer2Scar）——「一败再败」与「命运重创」同源累计层2 历史。
// 不新增 reason-code（spine 已登记）。best-effort：db 缺失/查询失败 → 返回 false（保守，绝不凭空让层3 解锁条件成立）。确定性、双驱动安全。
func (service *Service) fateHasPriorLayer2(ctx context.Context, unitID string) bool {
	if service == nil || service.db == nil || unitID == "" {
		return false
	}
	var count int
	if err := service.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events
		 WHERE actor_unit_id = ? AND reason_code = ?`,
		unitID, string(events.ReasonDefeatScarred),
	).Scan(&count); err != nil {
		return false // best-effort：查询失败不冒进让层3 条件成立（保守）
	}
	return count >= 1
}

// recordFateLayer2Scar 在命运闸实际落地一次「层2 高代价」负向后果时，补一条 ReasonDefeatScarred 层2 标记（流程事件，不改状态）。
// 这让「已发生≥1 次层2」可被 fateHasPriorLayer2 后续查到——层3 三条 AND 的第三条因此能在「先吃过一次高代价」后真正解锁。
// best-effort 旁路：失败只吞错、绝不阻断已落地的后果（与全文件 best-effort 纪律一致）。
func (service *Service) recordFateLayer2Scar(ctx context.Context, sessionID, unitID string, reasonCode events.ReasonCode) {
	if service == nil || service.db == nil || unitID == "" {
		return
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: unitID,
		Code:        events.ReasonDefeatScarred,
		Category:    events.CategoryFate,
		Payload:     map[string]any{"source": "fate_consequence_gate", "reason": string(reasonCode)},
	}); err != nil {
		slog.Warn("record fate layer2 scar failed (best-effort)", "unit", unitID, "err", err)
	}
}

// penaltyLayerForMagnitude 把一个负向 |delta| 量级映射到 D0-D3 候选层（与 fatePenaltyMagnitudeForLayer 互逆，阈值同源）。
func penaltyLayerForMagnitude(mag float64) int {
	switch {
	case mag >= 0.30:
		return 3
	case mag >= 0.08:
		return 2
	default:
		return 1
	}
}

// fatePenaltyMagnitudeForLayer 把降级后的 D0-D3 层映射回负向后果幅度（层越高代价越重）。确定性、纯函数。
func fatePenaltyMagnitudeForLayer(layer int) float64 {
	switch {
	case layer >= 3:
		return 0.30
	case layer == 2:
		return 0.08
	default:
		return 0.02
	}
}

// attachmentForUnit 估算某角色当前的牵挂等级 [0,100]（M3 后果分级闸的输入）。best-effort：
// 载入单位取其忠诚（共鸣项）+ 真实在世天数喂 ComputeAttachment；载入失败返回 0（无放大，退回基础后果，保守）。
// daysAlive 由 daysAliveForUnit 真实化（state.TurnState.Turn − unit.Social.BornTurn），不再硬传 0。
func (service *Service) attachmentForUnit(ctx context.Context, unitID string) float64 {
	if service == nil || service.units == nil || unitID == "" {
		return 0
	}
	rec, err := service.units.GetByID(ctx, unitID)
	if err != nil {
		return 0
	}
	days := service.daysAliveForUnit(ctx, unitID)
	return service.ComputeAttachment(ctx, unitID, rec.Status.Loyalty, days)
}

// daysAliveForUnit 估算某角色的真实在世天数（回合代理，与 threat.go 用 state.TurnState.Turn 同口径）。
// 真实化（修原 daysAlive 硬传 0 的注释自承缺陷）：从该单位所在会话载入 state，取 state.TurnState.Turn − 该单位
// 的 Social.BornTurn（不为负）。无法定位 state/会话（如 sessions 未注入的单元测试）或 BornTurn 未记 → 退回 0
// （最接近可得量：牵挂仍由忠诚/共创驱动，绝不夸大、保守）。best-effort、确定性、不阻断主流程。
func (service *Service) daysAliveForUnit(ctx context.Context, unitID string) int {
	if service == nil || service.units == nil || unitID == "" {
		return 0
	}
	rec, err := service.units.GetByID(ctx, unitID)
	if err != nil {
		return 0
	}
	state := service.loadStateForFate(ctx, rec.SessionID)
	if state == nil {
		return 0
	}
	days := state.TurnState.Turn - rec.Social.BornTurn
	if days < 0 {
		return 0
	}
	return days
}

// loadStateForFate 在命运路径上 best-effort 载入某会话的 state（供红线锚/真实在世天数/归因 provenance）。
// sessions 仓库未注入（单元测试）或会话不存在/解码失败 → 返回 nil；调用方据此优雅退化到无 state 行为，绝不阻断。
func (service *Service) loadStateForFate(ctx context.Context, sessionID string) *State {
	if service == nil || service.sessions == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	state, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		return nil
	}
	return &state
}

// expiredPendingRow 是一条「超期未决」的待决策，过期兜底据此自动关掉它。
type expiredPendingRow struct {
	SessionID  string
	DecisionID string
	ReasonCode events.ReasonCode
	OccurredAt string
}

// reclaimExpiredPending 是 M2 防轰炸③「过期兜底」（设计宪法 §4.2）：把某 owner 超过 fatePendingTTL 仍未处理的 PENDING
// 按宪章兜底（默认 let_her）自动关掉，并补一张「你没回来，于是她自己做了选择」回响卡。**全程 best-effort**——任何一步
// 失败都跳过该条、绝不阻断收件箱打开。M3 守门：不可逆类命运（死亡/叛变/pinned 丢失）在高牵挂下**拒绝**自动 let_her，
// 留在收件箱等玩家显式处理（对齐 D0-D3 硬锁不可逆）。
func (service *Service) reclaimExpiredPending(ctx context.Context, unitID string) {
	if service == nil || service.db == nil || unitID == "" {
		return
	}
	now := time.Now().UTC()
	resolved, err := service.resolvedDecisionIDs(ctx, unitID)
	if err != nil {
		return // 查不到已处理集合则不冒进兜底（保守）
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT session_id, payload_json, occurred_at FROM events
		 WHERE actor_unit_id = ? AND reason_code = ?`,
		unitID, string(events.ReasonPendingDecision),
	)
	if err != nil {
		return
	}
	expired := make([]expiredPendingRow, 0)
	for rows.Next() {
		var sessionID sql.NullString
		var payloadJSON, occurredAt string
		if err := rows.Scan(&sessionID, &payloadJSON, &occurredAt); err != nil {
			rows.Close()
			return
		}
		if !fateIsExpired(occurredAt, now) {
			continue
		}
		var payload struct {
			DecisionID string `json:"decision_id"`
			Reason     string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID == "" || resolved[payload.DecisionID] {
			continue // 空 ID 或已处理：跳过
		}
		expired = append(expired, expiredPendingRow{
			SessionID:  sessionID.String,
			DecisionID: payload.DecisionID,
			ReasonCode: events.ReasonCode(payload.Reason),
			OccurredAt: occurredAt,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return
	}
	if len(expired) == 0 {
		return
	}

	// M3 守门用的牵挂只算一次（同一 owner 共用）。
	attachment := service.attachmentForUnit(ctx, unitID)
	for _, p := range expired {
		// 不可逆类 + 高牵挂：拒绝自动兜底，留在收件箱等玩家显式处理（绝不替玩家放手不可逆的命运）。
		if fateRefusesAutoLetHer(p.ReasonCode, attachment) {
			continue
		}
		// 按宪章兜底：默认 let_her（你没回来 → 放手让她自己选）。失败则跳过该条（best-effort）。
		if err := service.ResolveFateDecision(ctx, p.SessionID, unitID, p.DecisionID, fateExpiryDefaultResolve); err != nil {
			continue
		}
		// 补回响卡：「你没回来，于是她自己做了选择」。锚定刚写下的 DECISION_RESOLVED 事件——它是一条真实存在、
		// 可被 loadRecentPlayerActions 引用的玩家动作事件（兜底也算「为她拿了主意」），满足 SurfaceEcho 的真实前因约束。
		priorEventID, ok := service.resolvedDecisionEventID(ctx, unitID, p.DecisionID)
		if !ok {
			continue
		}
		_, _ = service.SurfaceEcho(ctx, p.SessionID, unitID, priorEventID, "她没有等到你回来，于是自己做了选择。", -0.2)
	}
}

// resolvedDecisionEventID 按 (unitID, decisionID) 查该决策刚写下的 DECISION_RESOLVED 事件 id（过期兜底回响卡的锚点）。
// 该事件类型在 loadRecentPlayerActions 的引用集合内，故可被 SurfaceEcho 合法引用。双驱动安全、不依赖 JSON 函数。
func (service *Service) resolvedDecisionEventID(ctx context.Context, unitID, decisionID string) (string, bool) {
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT id, payload_json FROM events WHERE actor_unit_id = ? AND reason_code = ? AND payload_json LIKE ?`,
		unitID, string(events.ReasonDecisionResolved), "%"+decisionID+"%",
	)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	for rows.Next() {
		var id, payloadJSON string
		if err := rows.Scan(&id, &payloadJSON); err != nil {
			return "", false
		}
		var payload struct {
			DecisionID string `json:"decision_id"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID == decisionID {
			return id, true
		}
	}
	return "", false
}

// pendingDecisionMeta 是一条权威 PENDING_DECISION 事件的命运元数据，供 ResolveFateDecision 做情境选项反查与不可逆守门。
type pendingDecisionMeta struct {
	ReasonCode    events.ReasonCode // 底层事件 reason-code（判定是否不可逆类）
	AttributionZH string            // 归因因果句（§4.3 provenance 强制凭证；空=无可解析前因）
	Choices       []fateChoice      // 入箱时生成的情境化选项（id→后果类映射）
	LegacyItemID  string            // 传家物升级卡绑定的待升级装备 ID（§5；空=非升级卡）
}

// pendingDecisionMeta 按 decisionID 查权威 PENDING_DECISION 事件，返回其归属单位（owner）及命运元数据。
// 用 payload_json LIKE 收窄候选（decisionID 为唯一 UUID），再在 Go 侧精确比对 decision_id——双驱动安全、不依赖 JSON 函数。
func (service *Service) pendingDecisionMeta(ctx context.Context, decisionID string) (string, pendingDecisionMeta, bool, error) {
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT actor_unit_id, payload_json FROM events WHERE reason_code = ? AND payload_json LIKE ?`,
		string(events.ReasonPendingDecision), "%"+decisionID+"%",
	)
	if err != nil {
		return "", pendingDecisionMeta{}, false, fmt.Errorf("query pending decision meta: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner sql.NullString
		var payloadJSON string
		if err := rows.Scan(&owner, &payloadJSON); err != nil {
			return "", pendingDecisionMeta{}, false, fmt.Errorf("scan pending decision meta: %w", err)
		}
		var payload struct {
			DecisionID    string          `json:"decision_id"`
			Reason        string          `json:"reason"`
			AttributionZH string          `json:"attribution_zh"`
			Choices       []payloadChoice `json:"choices"`
			LegacyItemID  string          `json:"legacy_item_id"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID == decisionID { // LIKE 可能因 _/% 通配略微过宽，以精确比对为准
			meta := pendingDecisionMeta{
				ReasonCode:    events.ReasonCode(payload.Reason),
				AttributionZH: payload.AttributionZH,
				Choices:       payloadChoicesToFateChoices(payload.Choices),
				LegacyItemID:  payload.LegacyItemID,
			}
			return owner.String, meta, true, nil
		}
	}
	return "", pendingDecisionMeta{}, false, rows.Err()
}

// claimFateDecision 以 decision_id 为主键原子抢占 fate_decision_resolutions：INSERT 成功=唯一赢家(true)；
// 主键冲突（已被处理/并发输家）=幂等 no-op(false)。失败时复核该行是否已存在以区分「主键冲突」与「真 DB 错误」（不依赖驱动错误串）。
func (service *Service) claimFateDecision(ctx context.Context, decisionID, unitID, resolveType string) (bool, error) {
	resolvedAt := time.Now().UTC().Format("2006-01-02 15:04:05")
	if _, err := service.db.ExecContext(
		ctx,
		`INSERT INTO fate_decision_resolutions (decision_id, unit_id, resolve_type, resolved_at) VALUES (?, ?, ?, ?)`,
		decisionID, unitID, resolveType, resolvedAt,
	); err != nil {
		var existing string
		if qerr := service.db.QueryRowContext(
			ctx, `SELECT decision_id FROM fate_decision_resolutions WHERE decision_id = ?`, decisionID,
		).Scan(&existing); qerr == nil && existing == decisionID {
			return false, nil // 已被抢占：幂等 no-op
		}
		return false, fmt.Errorf("claim fate decision %s: %w", decisionID, err)
	}
	return true, nil
}

func (service *Service) resolvedDecisionIDs(ctx context.Context, unitID string) (map[string]bool, error) {
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT payload_json FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		unitID, string(events.ReasonDecisionResolved),
	)
	if err != nil {
		return nil, fmt.Errorf("query resolved decisions: %w", err)
	}
	defer rows.Close()
	resolved := map[string]bool{}
	for rows.Next() {
		var payloadJSON string
		if err := rows.Scan(&payloadJSON); err != nil {
			return nil, err
		}
		var payload struct {
			DecisionID string `json:"decision_id"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID != "" {
			resolved[payload.DecisionID] = true
		}
	}
	return resolved, rows.Err()
}

// WorldizeDeath 把一个角色之死，按相关性路由进「在乎她的每个人」的命运收件箱（双向耦合）。
// 返回被实际惊动（进高光卡/待决策）的人数。这正是「她的密友死了→我的命运」的机制落地。
// WorldizeDeath 把一个角色之死按相关性路由进「在乎她的人」的命运收件箱。
// killerAttributionZH 可选：凶手本次 LLM 决策的归因因果句（§5.4），非空则作为死亡卡的「为什么」尾注，
// 让哀悼者看到的是当次决策因果而非启发式模板；空则由 SurfaceFateEvent 回落翻译层/启发式。
func (service *Service) WorldizeDeath(ctx context.Context, sessionID string, deceased unit.Record, killerAttributionZH string) (int, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("worldize death: missing db")
	}
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT source_unit_id FROM relations
		 WHERE target_unit_id = ?
		 ORDER BY (ABS(trust) + ABS(fear) + ABS(affection) + ABS(rivalry)) DESC
		 LIMIT 64`,
		deceased.ID,
	)
	if err != nil {
		return 0, fmt.Errorf("query mourners: %w", err)
	}
	mourners := make([]string, 0)
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan mourner: %w", err)
		}
		if source != "" && source != deceased.ID {
			mourners = append(mourners, source)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	summary := deceased.DisplayName() + " 倒下了，再也没能起来。"
	surfaced := 0
	for _, source := range mourners {
		owner := unit.Record{ID: source}
		routing, err := service.SurfaceFateEvent(ctx, sessionID, &owner, FateEvent{
			ActorID:       deceased.ID,
			TargetID:      deceased.ID,
			ReasonCode:    events.ReasonCombatDown,
			Importance:    8,
			EmotionWeight: -0.6,
			Summary:       summary,
			AttributionZH: killerAttributionZH, // 非空=凶手当次 LLM 因果句；空则 SurfaceFateEvent 回落翻译层/启发式
		})
		if err != nil {
			return surfaced, err
		}
		if routing.Route != relevance.RouteAutonomous {
			surfaced++
		}
	}
	return surfaced, nil
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// parseOccurredAt 把 events.occurred_at（RFC3339Nano，偶有秒级 RFC3339）解析成 time.Time。
// 解析失败返回零值 + false（调用方据此跳过倒计时/兜底，绝不 panic）。纯函数、确定性。
func parseOccurredAt(occurredAt string) (time.Time, bool) {
	occurredAt = strings.TrimSpace(occurredAt)
	if occurredAt == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, occurredAt); err == nil {
		return t.UTC(), true
	}
	// 兜底：某些写入路径用 "2006-01-02 15:04:05"（claimFateDecision 等），也认一下。
	if t, err := time.Parse("2006-01-02 15:04:05", occurredAt); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// fateExpiresAt 计算一条待决策的过期时刻 = occurred_at + fatePendingTTL（RFC3339Nano）。
// occurred_at 不可解析时返回空串（前端据此不显示倒计时）。纯函数、确定性（设计宪法 §4.2 倒计时）。
func fateExpiresAt(occurredAt string) string {
	t, ok := parseOccurredAt(occurredAt)
	if !ok {
		return ""
	}
	return t.Add(fatePendingTTL).Format(time.RFC3339Nano)
}

// fateCountdownHours 计算「距过期还剩几小时」（相对 now，向下取整、已过期或不可解析为 0）。
// 纯逻辑核心抽到 fateCountdownHoursAt 便于测试注入固定 now。
func fateCountdownHours(occurredAt string) int {
	return fateCountdownHoursAt(occurredAt, time.Now().UTC())
}

// fateCountdownHoursAt 是 fateCountdownHours 的可注入 now 版本（测试用），确定性。
func fateCountdownHoursAt(occurredAt string, now time.Time) int {
	t, ok := parseOccurredAt(occurredAt)
	if !ok {
		return 0
	}
	remaining := t.Add(fatePendingTTL).Sub(now.UTC())
	if remaining <= 0 {
		return 0
	}
	return int(remaining.Hours())
}

// fateIsExpired 判断一条待决策是否已超过 fatePendingTTL（相对 now）。occurred_at 不可解析 → 视为未过期（保守不兜底）。
func fateIsExpired(occurredAt string, now time.Time) bool {
	t, ok := parseOccurredAt(occurredAt)
	if !ok {
		return false
	}
	return !now.UTC().Before(t.Add(fatePendingTTL))
}

// fateIrreversibility 估一条事件的「不可逆度」∈ [fateIrreversibilityFloor,1]（设计宪法 §4.2 因子一）。
// 纯函数、确定性、可测：死亡/背叛/卖传家宝等「覆水难收」的事取高位（≈1），日常可逆琐事取下限。
// 这是 [floor,1] 的阻尼系数而非裸权重——高 stakes 不衰减命运分（保住既有路由），日常才被往下压。
func fateIrreversibility(ev FateEvent) float64 {
	base := fateReasonIrreversibility(ev.ReasonCode)
	// 重要度作微调：高 importance 抬一点，低 importance 压一点（围绕 0.85 锚点 ±0.15）。
	if ev.Importance > 0 {
		base += (float64(ev.Importance)/10.0 - 0.85) * 0.15
	}
	return clampFateFactor(base, fateIrreversibilityFloor)
}

// fateReasonIrreversibility 给每类 reason-code 一个不可逆基线（缺省按 emotion/command/中性归类，未知取中位）。
func fateReasonIrreversibility(code events.ReasonCode) float64 {
	switch code {
	case events.ReasonCombatDown:
		return 1.0 // 倒下/阵亡：覆水难收
	case events.ReasonEmotionTrauma:
		return 0.95 // 创伤：难抹去
	case events.ReasonCommandForced:
		return 0.90 // 被强令做了高风险的事
	case events.ReasonCombatHit:
		return 0.85 // 受创：可恢复但留痕
	case events.ReasonEmotionReward, events.ReasonCommandAccepted:
		return 0.80 // 奖励/建议被采纳：偏正向、可逆性中等
	default:
		return 0.82 // 未登记原因取中位偏上（宁可多牵动，不误杀）
	}
}

// fateEmotionIntensity 把事件情绪强度归一为 [fateEmotionFloor,1] 的阻尼系数（设计宪法 §4.2 因子三）。
// 由 |EmotionWeight| 派生；缺省（EmotionWeight=0）不清零命运（取下限），强情绪事件趋近 1。确定性、可测。
func fateEmotionIntensity(ev FateEvent) float64 {
	return clampFateFactor(absFloat(ev.EmotionWeight), fateEmotionFloor)
}

// clampFateFactor 把一个原始因子映射到 [floor,1]：v≤0 取 floor，v≥1 取 1，中间线性插值到 [floor,1]。
func clampFateFactor(v float64, floor float64) float64 {
	if v <= 0 {
		return floor
	}
	if v >= 1 {
		return 1
	}
	return floor + v*(1-floor)
}

// fateCard 把世界事件渲染成祖魂语气的命运卡（engine/narration，确定性、无 LLM、按事件打散变体）。
// anchorKind 非空时走翻译矩阵：在祖魂语气外再加一句「凭什么这牵动她」（因她在乎的人/她的目标/她划的红线…）。
// 若事件带「归因因果句」（ev.AttributionZH，来自 decision.Attribution.NarrativeZH），再追加一句「为什么会这样」。
func fateCard(ev FateEvent, route relevance.FateRoute, anchorKind string) string {
	card := narration.BeatWithAnchor(
		string(ev.ReasonCode),
		anchorKind,
		ev.EmotionWeight,
		route == relevance.RoutePending,
		ev.Summary,
		0, // 种子按 reason-code+摘要派生，保证编年史不重复
	)
	if cause := strings.TrimSpace(ev.AttributionZH); cause != "" {
		card += "（" + cause + "）"
	}
	return card
}

// ===== §5 provenance 派生（让「为什么会这样」对玩家可见）=====

// deriveFateProvenance 在调用方未显式给出归因因果句时，用「可解析前因」（命中锚类别 + 事件摘要）派生一句 ≤40 字的
// 因果句。**可解析**=句子里的每一节都来自真实数据（锚类别由 eventRelevanceWithAnchor 命中得来、摘要是事件原文），
// 绝不凭空编戏剧性意外（对齐 §5「意外但合理」的代码强制）。确定性、纯函数、无 LLM。无可用前因时返回空串（跳过尾注）。
func deriveFateProvenance(ev FateEvent, anchorKind string) string {
	prefix := provenancePrefixForAnchor(anchorKind)
	summary := strings.TrimSpace(ev.Summary)
	switch {
	case prefix != "" && summary != "":
		return prefix + "：" + summary
	case prefix != "":
		return prefix
	case summary != "":
		// 自身事件（无外部锚命中）：直接以事件原文作前因句（她自己的事一定相关，因果即事件本身）。
		return summary
	default:
		return ""
	}
}

// provenancePrefixForAnchor 把命中里最重的锚类别翻译成「凭什么牵动她」的引子（§4.1 六类锚 → 因果引子）。
func provenancePrefixForAnchor(anchorKind string) string {
	switch relevance.AnchorKind(anchorKind) {
	case relevance.Relation:
		return "因为这事关她在乎的人"
	case relevance.Redline:
		return "因为这触到了她划下的红线"
	case relevance.Goal:
		return "因为这关乎她一直想做成的事"
	case relevance.DebtGrudgeLove:
		return "因为这桩债/仇/情未了"
	case relevance.Geo:
		return "因为这就发生在她所在的地方"
	case relevance.Legacy:
		return "因为这牵动她的血脉/传家之物"
	default:
		return ""
	}
}

// ===== §4.5 情境化 Copilot：按事件类型/红线/关系生成情境化待决策选项 =====

// fateChoice 是一个情境化待决策选项：玩家可见的 id/label，以及它折算到的基础后果类（resolveClass）。
type fateChoice struct {
	ID           string // 情境化选项 id（如 avenge / mourn / forbid…），resolve 时回传
	Label        string // 玩家可见文案
	ResolveClass string // 折算到的基础后果类 ∈ {let_her, urge, acknowledge}
}

// payloadChoice 是 fateChoice 的落库/反序列化镜像（写入 PENDING_DECISION 的 payload.choices）。
type payloadChoice struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	ResolveClass string `json:"resolve_class"`
}

// buildFateChoices 按事件类型/命中锚类别生成情境化选项（§4.5）。每个情境选项都显式映射到一个基础后果类，
// 既给玩家「贴合此刻」的措辞，又保证后果可复算、与旧三键同构。确定性、纯函数、无 LLM。
//   - 跨玩家回应卡（ReasonCrossConsentPending，事件耦合 §2.5）：给字面「还手/求和/认账」三选（urge/let_her/acknowledge），后果对称。
//   - 不可逆类（死亡/叛变/势力崩塌）：给「为TA复仇/送TA最后一程/由她去」三选，分别 urge/acknowledge/let_her。
//   - 触红线类（红线锚命中或触线事件）：给「严令她止步/默许她的选择」两选（urge/let_her）。
//   - 其余（日常牵挂）：给「叮嘱她/由她做主/只是知悉」三选（urge/let_her/acknowledge）。
func buildFateChoices(ev FateEvent, anchorKind string) []fateChoice {
	switch {
	case ev.ReasonCode == events.ReasonLegacyBequeathed && strings.TrimSpace(ev.LegacyItemID) != "":
		// 传家物升级卡（§5 Clutch→Legacy）：confirm（刻成传家物）→ urge（驱动 upgradeItemToLegacy）；婉拒/暂搁→ let_her/acknowledge。
		return []fateChoice{
			{ID: "engrave", Label: "把它刻成传家之物", ResolveClass: "urge"},
			{ID: "not_yet", Label: "暂且不必", ResolveClass: "let_her"},
			{ID: "acknowledge", Label: "只是知悉", ResolveClass: "acknowledge"},
		}
	case ev.ReasonCode == events.ReasonCrossConsentPending:
		// 跨玩家高后果交互回应卡（事件耦合 §2.5「被影响=她的命遇到另一缕命」）：另一缕命撞上了她的命，祖魂只能给「还手/求和/认账」三选——
		// 后果对称（§2.4 层2 CONTESTED）。还手=驱动她回击（越界干预，倾向对抗，后果：关系更僵、可能升级冲突）→urge；
		// 求和=放手让她自己缓和（倾向和解，后果：让一步、保住转圜余地）→let_her；认账=咽下这口气只是知悉（倾向隐忍，后果：暂避锋芒、心气受些挫）→acknowledge。
		// 替代此前回落的通用三键（叮嘱/由她/知悉），用贴合「被撞上」此刻的字面措辞。
		return []fateChoice{
			{ID: "strike_back", Label: "还手——叫她讨回这口气", ResolveClass: "urge"},
			{ID: "seek_peace", Label: "求和——由她自己去缓和", ResolveClass: "let_her"},
			{ID: "swallow", Label: "认账——这口气先咽下", ResolveClass: "acknowledge"},
		}
	case fateReasonIsIrreversibleClass(ev.ReasonCode):
		return []fateChoice{
			{ID: "avenge", Label: "为TA讨一个公道", ResolveClass: "urge"},
			{ID: "mourn", Label: "送TA最后一程", ResolveClass: "acknowledge"},
			{ID: "let_her", Label: "由她自己面对", ResolveClass: "let_her"},
		}
	case relevance.AnchorKind(anchorKind) == relevance.Redline || eventTripsRedlineClass(ev.ReasonCode):
		return []fateChoice{
			{ID: "forbid", Label: "严令她止步于此", ResolveClass: "urge"},
			{ID: "allow", Label: "默许她自己的抉择", ResolveClass: "let_her"},
		}
	default:
		return []fateChoice{
			{ID: "urge", Label: "叮嘱她按你的意思办", ResolveClass: "urge"},
			{ID: "let_her", Label: "放手让她自己做主", ResolveClass: "let_her"},
			{ID: "acknowledge", Label: "只是知悉，不干预", ResolveClass: "acknowledge"},
		}
	}
}

// resolveFateChoiceClass 把玩家传入的 resolveType（可能是情境选项 id，也可能是旧三键）折算成基础后果类。
//   - 若它命中本 pending 携带的某个情境 choice 的 id → 用该 choice 的 ResolveClass。
//   - 否则原样返回（让 fateConsequencesFor/isUrgeResolve 的旧三键同义词集兜底）——向后兼容旧三键调用。
func resolveFateChoiceClass(resolveType string, choices []fateChoice) string {
	id := strings.TrimSpace(resolveType)
	for _, c := range choices {
		if c.ID == id && strings.TrimSpace(c.ResolveClass) != "" {
			return c.ResolveClass
		}
	}
	return resolveType
}

// fateChoicesToPayload 把 []fateChoice 转为可 JSON 落库的 []map（payload.choices）。
func fateChoicesToPayload(choices []fateChoice) []map[string]any {
	out := make([]map[string]any, 0, len(choices))
	for _, c := range choices {
		out = append(out, map[string]any{"id": c.ID, "label": c.Label, "resolve_class": c.ResolveClass})
	}
	return out
}

// payloadChoicesToFateChoices 把反序列化的 []payloadChoice 还原为 []fateChoice（供 resolveFateChoiceClass 反查）。
func payloadChoicesToFateChoices(in []payloadChoice) []fateChoice {
	out := make([]fateChoice, 0, len(in))
	for _, c := range in {
		out = append(out, fateChoice{ID: c.ID, Label: c.Label, ResolveClass: c.ResolveClass})
	}
	return out
}

// payloadChoicesToOut 把反序列化的 []payloadChoice 转为回前端的 []FateChoiceOut。
func payloadChoicesToOut(in []payloadChoice) []FateChoiceOut {
	if len(in) == 0 {
		return nil
	}
	out := make([]FateChoiceOut, 0, len(in))
	for _, c := range in {
		out = append(out, FateChoiceOut{ID: c.ID, Label: c.Label, ResolveClass: c.ResolveClass})
	}
	return out
}

// ===== §8 OOC 双口径闭环：玩家主观三键 OOC 与归因机器 OOC 交叉暴露，高玩家 OOC → 收紧一致性旋钮 =====
//
// 此前玩家三键反馈里的「太离谱(fate_react_ooc)」纯观测：落 product_events、surfaced 在北极星 ooc_rate，但**不驱动任何收紧**。
// 本块把它接成闭环——读玩家主观三键 OOC 率（窗口聚合），超阈值即 latch 一个进程级「一致性收紧」态，使 GateSurprise/
// 归因强制更严（突然戏剧性动作门槛抬高、破圈升档暂缓）；回落则解除。与归因机器 OOC（AttributionStats）两口径在遥测里交叉暴露。
// flag 默认关：QUNXIANG_CONSISTENCY_TIGHTEN 未开 → RefreshConsistencyTightening 整体零行为（永不 latch），与全仓 flag 风格一致。

const (
	// playerOOCTightenThreshold 是玩家主观三键 OOC 率触发「一致性收紧」的阈值（>15% 即收紧，与归因机器降级阈同口径）。
	playerOOCTightenThreshold = 0.15
	// playerOOCMinSample 是触发收紧前要求的最小三键反馈总样本（防早期小样本抖动 latch）。
	playerOOCMinSample = 20
	// playerOOCWindowDays 是读玩家 OOC 率的滚动窗口（天）：只看近窗口的主观反馈，过往噪声不应永久压住一致性。
	playerOOCWindowDays = 7
)

// consistencyTightened 是进程级「一致性收紧」闩（高玩家 OOC 触发）；与 attributionDegraded 平行，跨会话/请求共享。
// 区别：attributionDegraded 是「机器 OOC 太高 → 暂停强制（放松）」的安全阀；本闩是「玩家主观 OOC 太高 → 收紧一致性」的旋钮。
var consistencyTightened atomic.Bool

// ConsistencyTightened 返回当前是否处于「一致性收紧」态（供 GateSurprise/归因强制消费方收紧门槛；/healthz 暴露）。
func ConsistencyTightened() bool {
	return consistencyTightened.Load()
}

// SetConsistencyTightened 直接设置收紧闩（供运维/测试）；生产由 RefreshConsistencyTightening 据玩家 OOC 率自动驱动。
func SetConsistencyTightened(on bool) {
	consistencyTightened.Store(on)
}

// consistencyTightenEnabled 读 QUNXIANG_CONSISTENCY_TIGHTEN（true/1/yes/on 视为开），默认关 → 收紧闭环零行为。
func consistencyTightenEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_CONSISTENCY_TIGHTEN"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// TightenedSurpriseCap 是收紧态下「突然戏剧性动作」允许的最高 SurpriseLevel（消费方据此抬高门槛）：
// 收紧时把破圈/突变的惊喜上限从 3 压到 2（≥3 的大转折一律降级/暂缓），未收紧时返回 3（不约束）。确定性、纯函数。
// 导出供 llm.go 的 GateSurprise/归因强制路径调用（crossFileNeeds：消费在 llm.go，本函数只产出门槛）。
func TightenedSurpriseCap() int {
	if ConsistencyTightened() {
		return 2
	}
	return 3
}

// serendipityPausedByTightening 报告破圈升档是否应因「一致性收紧」暂缓（高玩家 OOC 时先稳住一致性，别再塞陌生意外）。
// 供 SurfaceFateEvent 的破圈分支与 llm.go 消费（本块在 SurfaceFateEvent 内已接入：见破圈门）。确定性、纯函数。
func serendipityPausedByTightening() bool {
	return ConsistencyTightened()
}

// RefreshConsistencyTightening 读玩家主观三键 OOC 率（近 playerOOCWindowDays 天 product_events 聚合），超阈即 latch 收紧、回落即解除。
// best-effort、确定性、不阻断主结算：flag 关 / db 缺失 / 查询失败 / 样本不足 → 不改变现态（保守，绝不凭空 latch）。
// 返回本次读到的玩家主观 OOC 率与样本数，供调用方（/healthz、运营）与机器 OOC 交叉暴露。
func (service *Service) RefreshConsistencyTightening(ctx context.Context) (playerOOCRate float64, samples int) {
	if !consistencyTightenEnabled() || service == nil || service.db == nil {
		return 0, 0
	}
	report, err := analytics.NorthStar(ctx, service.db, playerOOCWindowDays)
	if err != nil {
		return 0, 0 // 查询失败：保守不改现态
	}
	samples = report.FateReactExpected + report.FateReactSurprise + report.FateReactOoc
	playerOOCRate = report.OocRate
	if samples < playerOOCMinSample {
		return playerOOCRate, samples // 样本不足：不抖动 latch（保守保持现态）
	}
	if playerOOCRate > playerOOCTightenThreshold {
		if consistencyTightened.CompareAndSwap(false, true) {
			slog.Warn("consistency tightening engaged (high player OOC)",
				"player_ooc_rate", playerOOCRate, "threshold", playerOOCTightenThreshold, "samples", samples)
		}
	} else if consistencyTightened.CompareAndSwap(true, false) {
		slog.Info("consistency tightening released (player OOC fell back)",
			"player_ooc_rate", playerOOCRate, "samples", samples)
	}
	return playerOOCRate, samples
}

// OOCDualChannel 是「玩家主观 OOC」与「归因机器 OOC」两口径的交叉暴露（§8 双口径闭环），供 /healthz / 运营观测。
type OOCDualChannel struct {
	// 玩家主观口径（三键「太离谱」反馈，近 playerOOCWindowDays 天窗口）。
	PlayerOOCRate float64 `json:"player_ooc_rate"`
	PlayerSamples int     `json:"player_samples"`
	// 归因机器口径（ValidateAttribution 判 OOC 的进程级累计，全量）。
	MachineOOCRate float64 `json:"machine_ooc_rate"`
	MachineTotal   int64   `json:"machine_total"`
	MachineOOC     int64   `json:"machine_ooc"`
	// 两个旋钮的当前态：玩家高 OOC → 收紧；机器高 OOC → 降级（放松强制）。交叉暴露便于发现「主观/机器背离」。
	ConsistencyTightened bool `json:"consistency_tightened"`
	AttributionDegraded  bool `json:"attribution_degraded"`
}

// FateOOCDualChannel 读两口径 OOC 并打包交叉暴露：玩家主观（product_events 三键，窗口）+ 机器（AttributionStats，全量累计）。
// best-effort：玩家口径查询失败仅置 0，机器口径恒可读（进程内存）。不修改任何 latch（纯读，幂等）；驱动收紧请用 RefreshConsistencyTightening。
func (service *Service) FateOOCDualChannel(ctx context.Context) OOCDualChannel {
	out := OOCDualChannel{
		ConsistencyTightened: ConsistencyTightened(),
		AttributionDegraded:  AttributionDegraded(),
	}
	machineTotal, machineOOC := AttributionStats()
	out.MachineTotal = machineTotal
	out.MachineOOC = machineOOC
	if machineTotal > 0 {
		out.MachineOOCRate = float64(machineOOC) / float64(machineTotal)
	}
	if service != nil && service.db != nil {
		if report, err := analytics.NorthStar(ctx, service.db, playerOOCWindowDays); err == nil {
			out.PlayerOOCRate = report.OocRate
			out.PlayerSamples = report.FateReactExpected + report.FateReactSurprise + report.FateReactOoc
		}
	}
	return out
}
