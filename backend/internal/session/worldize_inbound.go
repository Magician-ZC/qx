package session

// 文件说明：双向世界化的**入向探针**（设计 docs/事件耦合与跨玩家关联.md §1.3/§1.5）。
// 出向已落（WorldizeDeath/血仇传播把「她做下的事」扇出在乎者）；本文补上对称的另一半——
// 当玩家角色做出 worldizing 白名单事件（背叛/救援/倒地/社交/债务），**反查「谁的锚会被这件事点亮」**：
// 遍历可能在乎 actor/target 的角色，按 engine/relevance.Score 评分，≥0.35 者落一条 PROPAGATION_INBOUND 探针并
// SurfaceFateEvent 投进其命运收件箱；出向源头留痕 WORLDIZE_OUTBOUND。
//
// 三条红线（设计 §1.3 + 风险登记）：
//   - **付费无特权**：传播半径/强度只由 relevance.Score（锚权重·相对重要度·时间衰减·跳保真）决定，绝不含 wallet/billing。
//   - **冷却防洪泛**：同 (actor,target,reasonCode) 每自然日 ≤1（按 occurred_at 日窗去重，确定性、不依赖 SQL 日期函数）。
//   - **预算截断**：单角色每日入向探针 ≤ inboundDailyProbeBudget（超出按 Relevance 已是降序，自然丢弃尾部），绝不轰炸她。
//
// 全程 best-effort：任何一步失败只吞错/记 log，绝不阻断主结算循环（与命运/血仇旁路同款）。整条 flag-gated
// （QUNXIANG_WORLDIZE_INBOUND，默认关），与既有 region-runner/dungeon flag 风格一致。

import (
	"context"
	"os"
	"strings"
	"time"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/unit"
)

const (
	// inboundDailyProbeBudget 是单角色每日入向探针配额上限（设计 §1.5：≤12，超出按 Relevance 截断丢弃，世界仍在 events 里
	// 发生，只是不打扰她）。本切片在「每 actor 事件的候选反查」层先按候选数封顶，命运层另有 fatePendingDailyBudget 兜底。
	inboundDailyProbeBudget = 12

	// inboundCandidateScanLimit 是单次入向反查最多检视的「可能在乎者」候选数（防全表扫；按锚权重/关系强度降序取头部）。
	inboundCandidateScanLimit = 64

	// inboundHopFidelity 是入向探针对「直接在乎者」的传播保真度=1.0（hop-0 直击）。语义对齐既有出向（WorldizeDeath
	// 经 SurfaceFateEvent 以 hopFidelity=1.0 直接惊动在乎逝者的人）：直接以 actor/target 为锚的旁人是 hop-0，不衰减——
	// 这是「她的密友的事→我的命运」的直链。**多跳传播的 0.6^hop 单调衰减是 §1.4 Propagate 的独立切片**（friends-of-friends），
	// 不在本入向切片内；本切片只做「直接在乎者」的对称扇出，绝不放大玩家影响力（半径仍由锚相关性闸死、付费无关）。
	inboundHopFidelity = 1.0
)

// worldizeInboundEnabled 读 QUNXIANG_WORLDIZE_INBOUND（true/1/yes/on 视为开），默认关 → 入向扇出 no-op、零行为变化。
func worldizeInboundEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_WORLDIZE_INBOUND"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// worldizingInboundCodes 是触发入向反查的白名单 reason-code（设计 §1.3：背叛/救援/倒地/社交/债务）。
// 只有玩家角色做出这些「会在别人心里激起涟漪」的事，才反查谁的锚被点亮；日常起居/纯生存消耗不入向（不产噪音）。
var worldizingInboundCodes = map[events.ReasonCode]bool{
	events.ReasonRelationBetray:   true, // 背叛/叛变
	events.ReasonRelationRescue:   true, // 救援
	events.ReasonCombatDown:       true, // 倒地濒死
	events.ReasonAmbientSocialize: true, // 社交攀谈
	events.ReasonSocialObjectBind: true, // 撮合入局（社会客体绑定）
}

// worldizingInboundCodeList 是 worldizingInboundCodes 的**确定性物化切片**（显式列举、与 map 一一对齐）。
// 用途：把白名单**下推进 SQL**（recentWorldizingEvents 的 `reason_code IN (...)`），使「LIMIT 在白名单过滤之前」的
// 标定缺陷消失——避免高噪声日内（大量 combat_damage/survival_consumption 行）把当日白名单事件挤出窗口而静默不扇出。
// map 顺序非确定，故这里显式列序（确定性、双驱动通用、占位符按本序拼接）。新增/删除白名单 code 须同步改 map 与本切片。
var worldizingInboundCodeList = []events.ReasonCode{
	events.ReasonRelationBetray,
	events.ReasonRelationRescue,
	events.ReasonCombatDown,
	events.ReasonAmbientSocialize,
	events.ReasonSocialObjectBind,
}

// IsWorldizingInboundCode 判定一条 reason-code 是否在入向白名单内（供主控在边界扫描时预筛，省掉无关事件的反查）。
func IsWorldizingInboundCode(code events.ReasonCode) bool {
	return worldizingInboundCodes[code]
}

// IsZeroAnchorSource 判定一条世界事件是否「零锚来源」（陌生人/新 region：没有任何角色以其 actor/target 为锚）。
// 供 fate.go 的破圈升档用（设计 §1.5 破圈预算：每日 ≥1 件零锚来源低权事件有资格进高光卡，作新锚的种子）——
// 本函数只提供**判定 helper**，破圈升档逻辑（强制 ≥1 件/日）在 fate.go 由集成方落。确定性、付费无关、best-effort（查错保守判 false=非零锚，不冒进破圈）。
func (service *Service) IsZeroAnchorSource(ctx context.Context, ev FateEvent) bool {
	if service == nil || service.db == nil {
		return false
	}
	// 任意一端（actor/target）被任何角色当作锚指向 → 非零锚来源（她的圈子里已有人在乎它）。
	for _, ref := range []string{ev.ActorID, ev.TargetID} {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		if service.AnchorDensityByRef(ctx, ref, "") > 0 {
			return false
		}
	}
	return true
}

// WorldizeInbound 是入向反查的核心：当 actor（玩家角色）对 target 做出一桩 worldizing 白名单事件，
// 反查「谁的锚会被这件事点亮」并扇出。返回被实际惊动（进高光卡/待决策）的人数。flag 关 / 非白名单 / 冷却内 → 返回 0。
//
// 步骤：①白名单 + flag 闸；②冷却去重（同 actor,target,code 每日 ≤1）；③出向源头留痕 WORLDIZE_OUTBOUND；
// ④反查可能在乎者候选（以 actor/target 为锚的角色 + 与之有关系的角色），按 relevance.Score（×inboundHopFidelity）评分；
// ⑤≥RelevanceGate 者落 PROPAGATION_INBOUND 探针 + SurfaceFateEvent 投收件箱（预算截断 ≤ inboundDailyProbeBudget）。
func (service *Service) WorldizeInbound(ctx context.Context, state *State, actorID, targetID string, reasonCode events.ReasonCode) (int, error) {
	if service == nil || service.db == nil {
		return 0, nil
	}
	if !worldizeInboundEnabled() || !worldizingInboundCodes[reasonCode] {
		return 0, nil
	}
	actorID = strings.TrimSpace(actorID)
	if actorID == "" {
		return 0, nil
	}
	// 冷却：同 (actor,target,code) 每自然日 ≤1（防回声室洪泛，设计风险登记）。已触发过则静默跳过。
	if service.inboundCooldownActive(ctx, actorID, targetID, reasonCode) {
		return 0, nil
	}
	sessionID := ""
	if state != nil {
		sessionID = state.ID
	}
	// 出向源头留痕（WORLDIZE_OUTBOUND）：先写，使本日后续同源事件被冷却挡下（去重锚点）。best-effort。
	// RelatedUnitID 故意留空（回落为 owner=actor，恒真实单位）——target 可能是非单位引用（地名/物品/抽象目标），
	// 塞进 target_unit_id 会触发 events 表 FK 失败。target 完整写进 payload.target，冷却去重据此精确比对。
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: actorID,
		Code:        events.ReasonWorldizeOutbound,
		Category:    events.CategoryFate,
		Payload: map[string]any{
			"actor":  actorID,
			"target": targetID,
			"reason": string(reasonCode),
		},
	})

	// 反查候选「可能在乎者」：以 actor/target 为锚指向的角色 + 与 target 有强关系的角色（去重、排除 actor 自己）。
	candidates := service.inboundCandidates(ctx, actorID, targetID)
	if len(candidates) == 0 {
		return 0, nil
	}

	// 入向事件原型（用于评分与命运卡）：actor 是源头，target 是被作用对象；importance/情绪由 reason-code 定义派生。
	importance, emotion := inboundEventWeights(reasonCode)
	surfaced := 0
	probed := 0
	for _, candidateID := range candidates {
		if probed >= inboundDailyProbeBudget {
			break // 预算截断：单角色每日入向探针封顶，超出按 Relevance 已降序，自然丢弃尾部（设计 §1.5）。
		}
		// 评分：用候选者「在乎 actor/target」的锚集对这桩事件评分，×inboundHopFidelity 折减（玩家影响力衰减、不放大）。
		anchors := service.buildRelevanceAnchorsWithState(ctx, state, candidateID)
		probe := FateEvent{
			ActorID:       actorID,
			TargetID:      targetID,
			ReasonCode:    reasonCode,
			Importance:    importance,
			EmotionWeight: emotion,
			Summary:       inboundSummary(reasonCode),
		}
		rel := inboundScore(anchors, probe)
		if rel < relevance.RelevanceGate {
			continue // 未过相关性阈：这桩事不足以牵动她，世界仍在发生，只是不打扰她。
		}
		// per-recipient 当日入向闸（M3）：同一 carer 一天最多被惊动 inboundDailyProbeBudget 次——即便她被多桩涉同 hub 的
		// distinct 事件命中。与 per-event 的 probed 互补（probed 限「一桩事惊动几人」）。满额跳过她，不影响其余候选。
		if service.inboundProbeBudgetExhausted(ctx, candidateID) {
			continue
		}
		probed++
		// 落入向探针留痕（PROPAGATION_INBOUND）：旁人的一桩事恰好牵动了她在乎的人或物。best-effort。
		// RelatedUnitID 留空（回落为 owner=candidate，恒真实单位）——source_target 可能是非单位引用，写进 payload 即可，
		// 否则触发 events 表 FK 失败。
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID:   sessionID,
			OwnerUnitID: candidateID,
			Code:        events.ReasonPropagationInbound,
			Category:    events.CategoryFate,
			Payload: map[string]any{
				"source_actor":  actorID,
				"source_target": targetID,
				"reason":        string(reasonCode),
				"relevance":     rel,
			},
		})
		// 投进命运层（SurfaceFateEvent 自做三档路由 + 每日待决策预算兜底）。失败只吞错、不阻断其余候选。
		owner := unit.Record{ID: candidateID}
		routing, err := service.SurfaceFateEvent(ctx, sessionID, &owner, probe)
		if err != nil {
			continue
		}
		if routing.Route != relevance.RouteAutonomous {
			surfaced++
		}
	}
	return surfaced, nil
}

// ScanAndWorldizeInbound 是部署边界钩子（主控在 service.go 边界调）：扫最近的 worldizing 白名单事件，
// 逐个 WorldizeInbound 扇出。units 是本会话当前在场单位（用于判定 actor 是否玩家角色——只有玩家阵营角色的事入向，§1.3
// 「玩家角色的行为也被世界化」）。返回被实际惊动的总人数。flag 关 → no-op 返回 0。全程 best-effort。
func (service *Service) ScanAndWorldizeInbound(ctx context.Context, state *State, units []unit.Record) (int, error) {
	if service == nil || service.db == nil || state == nil {
		return 0, nil
	}
	if !worldizeInboundEnabled() {
		return 0, nil
	}
	// 玩家阵营角色集合（只有他们的事入向；§1.3「我的角色的行为也被世界化」）。
	playerSet := map[string]bool{}
	for _, u := range units {
		if u.FactionID != "" && u.FactionID == state.PlayerFactionID {
			playerSet[u.ID] = true
		}
	}
	if len(playerSet) == 0 {
		return 0, nil
	}
	pending := service.recentWorldizingEvents(ctx, state.ID, playerSet)
	total := 0
	for _, e := range pending {
		n, err := service.WorldizeInbound(ctx, state, e.ActorID, e.TargetID, e.ReasonCode)
		if err != nil {
			continue // best-effort：单条失败不拖垮整批。
		}
		total += n
	}
	return total, nil
}

// inboundWorldEvent 是一条待入向扇出的 worldizing 事件（边界扫描取出的原型）。
type inboundWorldEvent struct {
	ActorID    string
	TargetID   string
	ReasonCode events.ReasonCode
}

// recentWorldizingEvents 扫某会话最近的 worldizing 白名单事件（只取玩家阵营角色作为 actor 的）。
// 限当天窗口（与冷却同口径）+ 头部 N 条，避免边界扫描扫全历史。best-effort：查错返回空。
func (service *Service) recentWorldizingEvents(ctx context.Context, sessionID string, playerSet map[string]bool) []inboundWorldEvent {
	if service == nil || service.db == nil || len(playerSet) == 0 {
		return nil
	}
	dayLo := dayStartUTC().Format(time.RFC3339Nano)
	// 白名单**下推进 SQL**（M2 修复）：`reason_code IN (?,...)` 让 LIMIT 只截断「白名单内」的行，
	// 而非先 LIMIT 再过滤——否则高噪声日内的 combat_damage/survival_consumption 会把当日白名单事件挤出窗口而静默不扇出。
	// args 顺序：sessionID, dayLo, codes..., scanLimit（与下方占位符顺序严格对齐）。双驱动通用、无需方言分支。
	placeholders := strings.Repeat("?,", len(worldizingInboundCodeList))
	placeholders = strings.TrimSuffix(placeholders, ",")
	args := make([]any, 0, len(worldizingInboundCodeList)+3)
	args = append(args, sessionID, dayLo)
	for _, code := range worldizingInboundCodeList {
		args = append(args, string(code))
	}
	args = append(args, inboundCandidateScanLimit)
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT actor_unit_id, target_unit_id, reason_code FROM events
		 WHERE session_id = ? AND occurred_at >= ? AND reason_code IN (`+placeholders+`)
		 ORDER BY occurred_at DESC LIMIT ?`,
		args...,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]inboundWorldEvent, 0)
	seen := map[string]bool{}
	for rows.Next() {
		var actor, target, code string
		if err := rows.Scan(&actor, &target, &code); err != nil {
			return out
		}
		rc := events.ReasonCode(code)
		if !worldizingInboundCodes[rc] || !playerSet[actor] {
			continue
		}
		key := actor + "|" + target + "|" + code
		if seen[key] {
			continue // 同 (actor,target,code) 边界内只扇一次（冷却另有日窗去重）。
		}
		seen[key] = true
		out = append(out, inboundWorldEvent{ActorID: actor, TargetID: target, ReasonCode: rc})
	}
	return out
}

// inboundCandidates 反查「可能在乎 actor/target 的角色」：①以 actor/target 为锚指向的角色（relevance_anchors 反向索引）；
// ②与 target 有强关系的角色（relations 表）。去重、排除 actor 自己、按头部 N 截断。best-effort：查错返回已收集到的。
func (service *Service) inboundCandidates(ctx context.Context, actorID, targetID string) []string {
	if service == nil || service.db == nil {
		return nil
	}
	seen := map[string]bool{actorID: true} // 排除 actor 自己（她不对自己的事入向）。
	out := make([]string, 0)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	// ① 以 actor/target 为锚指向的角色（反向索引 idx_relevance_anchors_ref）。
	for _, ref := range []string{actorID, targetID} {
		if strings.TrimSpace(ref) == "" {
			continue
		}
		rows, err := service.db.QueryContext(
			ctx,
			`SELECT character_unit_id FROM relevance_anchors WHERE anchor_ref = ?
			 ORDER BY weight DESC LIMIT ?`,
			ref, inboundCandidateScanLimit,
		)
		if err != nil {
			continue
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				break
			}
			add(id)
		}
		rows.Close()
	}
	// ② 与 target 有关系的角色（关系也是一种锚；relations.source_unit_id 派生）。
	if strings.TrimSpace(targetID) != "" {
		rows, err := service.db.QueryContext(
			ctx,
			`SELECT source_unit_id FROM relations WHERE target_unit_id = ?
			 ORDER BY (ABS(trust) + ABS(fear) + ABS(affection) + ABS(rivalry)) DESC LIMIT ?`,
			targetID, inboundCandidateScanLimit,
		)
		if err == nil {
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					break
				}
				add(id)
			}
			rows.Close()
		}
	}
	if len(out) > inboundCandidateScanLimit {
		out = out[:inboundCandidateScanLimit]
	}
	return out
}

// inboundCooldownActive 判定同 (actor,target,code) 是否当天已触发过入向（已写过 WORLDIZE_OUTBOUND 留痕）。
// 用 occurred_at 日窗 + payload LIKE 收窄（payload 含 actor/target/reason），双驱动安全、不依赖 SQL JSON 函数、确定性。
// best-effort：查错保守返回 false（放行，不漏扇——宁可多扇一次也不静默吞掉应有的命运牵连，冷却是软抑制）。
func (service *Service) inboundCooldownActive(ctx context.Context, actorID, targetID string, reasonCode events.ReasonCode) bool {
	if service == nil || service.db == nil || actorID == "" {
		return false
	}
	dayLo := dayStartUTC().Format(time.RFC3339Nano)
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT payload_json FROM events
		 WHERE actor_unit_id = ? AND reason_code = ? AND occurred_at >= ?`,
		actorID, string(events.ReasonWorldizeOutbound), dayLo,
	)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var payloadJSON string
		if err := rows.Scan(&payloadJSON); err != nil {
			return false
		}
		// 精确比对 target+reason（payload 是确定性 JSON，含这两键）。target 可空，空对空也算同一桩。
		if strings.Contains(payloadJSON, `"reason":"`+string(reasonCode)+`"`) &&
			inboundPayloadTargetMatches(payloadJSON, targetID) {
			return true
		}
	}
	return false
}

// inboundProbeBudgetExhausted 判断某 recipient（candidate）当天（UTC 自然日）**收到**的入向探针是否已达 inboundDailyProbeBudget。
// M3 修复：既有 per-event `probed` 计数器只约束「一桩事最多惊动几人」（per-source-event），无法约束「同一 carer 一天被多少桩
// 涉同 hub 的不同事件投卡」——热门 target 的一连串 distinct 事件会对同一 carer 反复投卡。本闸按 recipient 维度封顶
// （per-recipient ≤12，设计 §1.5），与 per-event 互补：前者限「一桩事惊动几人」，本闸限「一人一天被惊动几次」。
// PROPAGATION_INBOUND 以 OwnerUnitID=candidate 写入（→ actor_unit_id=recipient），故按 actor_unit_id 计数即「她收到的探针数」。
// occurred_at 以 RFC3339Nano（UTC）写入，用 [dayStart, nextDay) 字符串区间过滤——双驱动安全、不依赖 SQL 日期函数、确定性。
// 仿 fate.go 的 pendingBudgetExhausted；出错保守返回 true（宁少扇不轰炸——与冷却放行相反，因这是反洪泛的硬上限）。
func (service *Service) inboundProbeBudgetExhausted(ctx context.Context, recipientID string) bool {
	if service == nil || service.db == nil || recipientID == "" {
		return true // 无 recipient / 无库：保守不投（宁少扇不轰炸）。
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
		recipientID, string(events.ReasonPropagationInbound), lo, hi,
	).Scan(&count); err != nil {
		return true // best-effort：查错保守返回满额（宁少扇一张卡，不冒轰炸她的风险）。
	}
	return count >= inboundDailyProbeBudget
}

// inboundPayloadTargetMatches 判定一条 WORLDIZE_OUTBOUND payload 的 target 是否等于给定 targetID（含 target 为空的情形）。
func inboundPayloadTargetMatches(payloadJSON, targetID string) bool {
	return strings.Contains(payloadJSON, `"target":"`+targetID+`"`)
}

// dayStartUTC 返回当前 UTC 自然日 00:00:00（冷却/边界扫描的日窗起点，与 pendingBudgetExhausted 同口径）。
func dayStartUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// inboundScore 用候选者的锚集对一桩入向事件评分，并按入向跳保真度（inboundHopFidelity）折减。
// 复用 fate.go 的命中判定（anchorHitByEvent），但 hopFidelity 传 0.6 而非直击的 1.0——玩家影响力经「世界化」传到旁人是一跳，
// 单调衰减、绝不放大（对齐设计「每跳 importance 单调衰减」+「付费不增传播半径/强度」）。纯函数级确定性（只看锚与事件）。
func inboundScore(anchors []relevance.Anchor, ev FateEvent) float64 {
	hits := make([]relevance.Hit, 0, len(anchors))
	for _, a := range anchors {
		if anchorHitByEvent(a, ev) {
			hits = append(hits, relevance.Hit{Anchor: a})
		}
	}
	return relevance.Score(hits, inboundHopFidelity)
}

// inboundEventWeights 给一类入向 reason-code 一个 (importance, emotionWeight) 原型（喂 FateScore 三因子）。
// 取 reason-code 定义的 ImportanceMin 作保守下限（入向是「别人的事」，不夸大重要度）；情绪权重按事件性质定符号/量级。
func inboundEventWeights(code events.ReasonCode) (int, float64) {
	importance := 5
	if def, ok := events.Lookup(code); ok && def.ImportanceMin > 0 {
		importance = def.ImportanceMin
	}
	emotion := 0.0
	switch code {
	case events.ReasonRelationBetray:
		emotion = -0.6 // 背叛：强负向
	case events.ReasonCombatDown:
		emotion = -0.6 // 倒地濒死：强负向
	case events.ReasonRelationRescue:
		emotion = 0.5 // 救援：正向
	case events.ReasonAmbientSocialize, events.ReasonSocialObjectBind:
		emotion = 0.3 // 社交/撮合：温和正向
	default:
		emotion = 0.0
	}
	return importance, emotion
}

// inboundSummary 给一类入向 reason-code 一句确定性事件摘要（命运卡引子，无 LLM）。
func inboundSummary(code events.ReasonCode) string {
	switch code {
	case events.ReasonRelationBetray:
		return "有人背弃了曾经的同伴。"
	case events.ReasonCombatDown:
		return "有人在外倒下了。"
	case events.ReasonRelationRescue:
		return "有人在危难中被救了下来。"
	case events.ReasonAmbientSocialize:
		return "有人在外结下了新的交情。"
	case events.ReasonSocialObjectBind:
		return "几个人的命被牵到了一处。"
	default:
		return "外面发生了一件事。"
	}
}
