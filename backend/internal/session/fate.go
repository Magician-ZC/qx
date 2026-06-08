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
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/analytics"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/narration"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/unit"
)

const (
	relationIntensityNorm  = 20.0 // 关系强度（四轴绝对值之和，[-10,10] 量级）归一化分母
	relationAnchorHalfLife = 14.0 // 关系锚半衰期（天）

	// 命运分（FateScore）三因子的阻尼下限（设计宪法 §4.2）。
	// 不可逆度/情绪强度被建成「阻尼系数 ∈ [floor,1]」而非裸权重：高 stakes 事件取≈1（不衰减 careRelevance、
	// 保住既有路由），日常琐事才把分往下压。下限保证「单因子退化」不会让本该牵动她的事被误杀（回归来源）。
	fateIrreversibilityFloor = 0.70 // 不可逆度阻尼下限：再「可逆」的事也保留七成命运分
	fateEmotionFloor         = 0.70 // 情绪强度阻尼下限：情绪缺省（EmotionWeight=0）时不应清零命运
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
}

// buildRelevanceAnchors 从角色的对外关系构造相关性锚（当前=关系锚；geo/redline/goal 待世界化接入）。
func (service *Service) buildRelevanceAnchors(ctx context.Context, unitID string) []relevance.Anchor {
	anchors := make([]relevance.Anchor, 0)
	if service == nil || service.db == nil {
		return anchors
	}
	// 1) 持久锚（目标/红线/债仇爱/血脉——非关系锚，只有 relevance_anchors 表能存）。
	seen := map[string]bool{}
	for _, a := range service.loadPersistentAnchors(ctx, unitID) {
		anchors = append(anchors, a)
		seen[string(a.Kind)+"|"+a.Ref] = true
	}
	// 2) 实时关系锚（由 relations 表派生；同 (kind,ref) 已有持久锚则不重复）。
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
		// 任何锚（关系/债仇爱/血脉…）只要其 Ref 命中事件的 actor/target/region，就算命中。
		if a.Ref != "" && (a.Ref == ev.ActorID || a.Ref == ev.TargetID) {
			hits = append(hits, relevance.Hit{Anchor: a})
			if a.Weight > topWeight {
				topWeight = a.Weight
				topKind = string(a.Kind)
			}
		}
	}
	return relevance.Score(hits, 1.0), topKind
}

// SurfaceFateEvent 把一条世界事件按相关性路由进某角色的命运层。
// 自治不打扰：返回 RouteAutonomous，不写流程事件（底层事件已留痕）；高光卡/待决策：写入命运收件箱。
func (service *Service) SurfaceFateEvent(ctx context.Context, sessionID string, owner *unit.Record, ev FateEvent) (FateRouting, error) {
	if service == nil || service.db == nil || owner == nil {
		return FateRouting{}, fmt.Errorf("surface fate event: missing dependencies")
	}
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
		// 发生在别人身上 → 经她的锚翻译牵挂相关度，并记下命中里最重的锚类别（翻译矩阵用）。
		rel, anchorKind = eventRelevanceWithAnchor(service.buildRelevanceAnchors(ctx, owner.ID), ev)
	}
	// FateScore 三因子（设计宪法 §4.2）：不可逆度 × 牵挂相关度 × 情绪强度，三者 ∈ [0,1]。
	// careRelevance=rel（上面算出的关系/重要度相关性）；irreversibility/emotion 是 [floor,1] 的阻尼系数，
	// 高 stakes 事件取≈1（不衰减 careRelevance、与单因子退化等价、保住既有路由），日常琐事才被压低降档。
	fateScore := fateIrreversibility(ev) * rel * fateEmotionIntensity(ev)
	route := relevance.RouteFor(fateScore)
	out := FateRouting{Route: route, Relevance: fateScore}
	if route == relevance.RouteAutonomous {
		return out, nil
	}

	out.Card = fateCard(ev, route, anchorKind)
	code := events.ReasonInboxHighlight
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
	if route == relevance.RoutePending {
		code = events.ReasonPendingDecision
		out.DecisionID = "fd_" + uuid.NewString()
		payload["decision_id"] = out.DecisionID
	}
	if _, err := events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:     sessionID,
		OwnerUnitID:   owner.ID,
		RelatedUnitID: ev.TargetID,
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

// OpenFateInbox 返回某角色未被处理的待决策（命运收件箱）。
func (service *Service) OpenFateInbox(ctx context.Context, unitID string) ([]FateInboxItem, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("open fate inbox: missing db")
	}
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
			DecisionID string `json:"decision_id"`
			Narrative  string `json:"narrative"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID == "" || resolved[payload.DecisionID] {
			continue
		}
		items = append(items, FateInboxItem{DecisionID: payload.DecisionID, Narrative: payload.Narrative, OccurredAt: occurredAt})
	}
	return items, rows.Err()
}

// FateFeedItem 是命运四槽界面的一张卡（高光/待决策/回响）。
type FateFeedItem struct {
	Kind       string `json:"kind"`        // highlight / pending / echo
	DecisionID string `json:"decision_id"` // pending 时可处理
	Narrative  string `json:"narrative"`
	OccurredAt string `json:"occurred_at"`
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
		WHERE actor_unit_id = ? AND reason_code IN (?, ?, ?)
		ORDER BY occurred_at DESC LIMIT ?`,
		unitID, string(events.ReasonInboxHighlight), string(events.ReasonPendingDecision), string(events.ReasonEchoLink), limit)
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
			DecisionID string `json:"decision_id"`
			Narrative  string `json:"narrative"`
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
	ownerID, found, err := service.pendingDecisionOwner(ctx, decisionID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("resolve fate decision: decision %s not found or not pending", decisionID)
	}
	unitID = ownerID

	// 2) 原子抢占：唯一赢家继续；重复/并发者幂等 no-op 返回（不重复施加后果）。
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
	var consequenceErr error
	if service.mutator != nil {
		for _, c := range fateConsequencesFor(resolveType) {
			if _, err := service.mutator.Apply(ctx, status.Mutation{
				UnitID:     unitID,
				Turn:       0, // 待决策在回合循环外处理，turn 用 0 标记
				Field:      c.Field,
				Delta:      c.Delta,
				ReasonCode: c.ReasonCode,
				ReasonText: c.ReasonText,
				Actors:     []string{unitID},
			}); err != nil {
				consequenceErr = fmt.Errorf("apply fate consequence (%s/%s): %w", resolveType, c.Field, err)
				break
			}
		}
	}
	return consequenceErr
}

// pendingDecisionOwner 按 decisionID 查权威 PENDING_DECISION 事件，返回其归属单位（owner）。
// 用 payload_json LIKE 收窄候选（decisionID 为唯一 UUID），再在 Go 侧精确比对 decision_id——双驱动安全、不依赖 JSON 函数。
func (service *Service) pendingDecisionOwner(ctx context.Context, decisionID string) (string, bool, error) {
	rows, err := service.db.QueryContext(
		ctx,
		`SELECT actor_unit_id, payload_json FROM events WHERE reason_code = ? AND payload_json LIKE ?`,
		string(events.ReasonPendingDecision), "%"+decisionID+"%",
	)
	if err != nil {
		return "", false, fmt.Errorf("query pending decision owner: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var owner sql.NullString
		var payloadJSON string
		if err := rows.Scan(&owner, &payloadJSON); err != nil {
			return "", false, fmt.Errorf("scan pending decision owner: %w", err)
		}
		var payload struct {
			DecisionID string `json:"decision_id"`
		}
		_ = json.Unmarshal([]byte(payloadJSON), &payload)
		if payload.DecisionID == decisionID { // LIKE 可能因 _/% 通配略微过宽，以精确比对为准
			return owner.String, true, nil
		}
	}
	return "", false, rows.Err()
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
func (service *Service) WorldizeDeath(ctx context.Context, sessionID string, deceased unit.Record) (int, error) {
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
