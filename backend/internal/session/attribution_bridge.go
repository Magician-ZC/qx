package session

// 文件说明：把 engine/decision 的归因校验（「意外但合理」的代码强制，见设计宪法 §5）接入会话决策链路。
// 接入策略：影子模式优先——决策附带的 attribution 会被校验并计入 OOC 遥测，但默认不阻断游戏决策；
// 仅当显式开启强制（SetAttributionEnforcement(true)）时，归因判 OOC 才回退到安全决策。
// 现阶段快照只填充「人格维(persona_trait)」与「现实压力位(pressure)」这两类 ref_id 稳定可知、
// LLM 无需暴露内部 ID 即可正确引用的前因；memory/relation/redline 的解析需另接记忆/关系/宪章上下文（后续）。

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"

	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/unit"
)

const (
	attributionMemoryWindow  = 30 // 记忆回溯窗口（回合）
	attributionMemoryLimit   = 6  // 暴露给 LLM 的可引用记忆 ID 上限
	attributionRelationLimit = 8  // 纳入快照的关系条数上限
	relationAxisScale        = 10.0

	// 自动降级护栏：累计样本达 oocDegradeMinSample 后，若 OOC 率超 oocDegradeThreshold，
	// 自动把强制降级为影子模式（latch，不抖动），并在 /healthz 标 degraded。
	oocDegradeThreshold = 0.15
	oocDegradeMinSample = 50
)

// 进程级归因遥测计数（跨所有会话/请求累计；每请求新建的 Service 不会重置它们）。
var (
	attributionTotal    atomic.Int64
	attributionOOC      atomic.Int64
	attributionDegraded atomic.Bool // 自动降级闩锁：true 时全局暂停强制（转影子）。
)

// AttributionStats 返回进程级累计：归因校验总数与判定为 OOC 的数量。
func AttributionStats() (total int64, ooc int64) {
	return attributionTotal.Load(), attributionOOC.Load()
}

// AttributionDegraded 返回是否已因 OOC 率过高自动降级为影子模式。
func AttributionDegraded() bool {
	return attributionDegraded.Load()
}

// ResetAttributionDegrade 清除自动降级闩锁，重新武装强制（供运维/测试使用）。
func ResetAttributionDegrade() {
	attributionDegraded.Store(false)
}

// recordAttributionResult 记一次归因校验结果并按需触发自动降级。
func recordAttributionResult(ok bool) {
	attributionTotal.Add(1)
	if !ok {
		attributionOOC.Add(1)
	}
	maybeAutoDegrade()
}

// maybeAutoDegrade 在样本充分且 OOC 率超阈时把强制降级为影子模式（一次性闩锁）。
func maybeAutoDegrade() {
	if attributionDegraded.Load() {
		return
	}
	total := attributionTotal.Load()
	if total < oocDegradeMinSample {
		return
	}
	rate := float64(attributionOOC.Load()) / float64(total)
	if rate > oocDegradeThreshold && attributionDegraded.CompareAndSwap(false, true) {
		slog.Warn("attribution enforcement auto-degraded to shadow mode",
			"ooc_rate", rate, "threshold", oocDegradeThreshold, "total", total)
	}
}

// causeRefPayload 是 attribution 中单条前因的线上结构（LLM 产出）。
type causeRefPayload struct {
	Kind      string  `json:"kind"`
	RefID     string  `json:"ref_id"`
	Weight    float64 `json:"weight"`
	SnippetZH string  `json:"snippet_zh,omitempty"`
}

// attributionPayload 是决策归因的线上结构（可选，LLM 产出，解释「她为什么这么选」）。
type attributionPayload struct {
	Primary       causeRefPayload   `json:"primary"`
	Supporting    []causeRefPayload `json:"supporting,omitempty"`
	SurpriseLevel int               `json:"surprise_level,omitempty"`
	NarrativeZH   string            `json:"narrative_zh,omitempty"`
}

// toEngineAttribution 把线上归因结构映射为 engine/decision 的 Attribution；nil 时返回 false。
func toEngineAttribution(payload *attributionPayload) (decision.Attribution, bool) {
	if payload == nil {
		return decision.Attribution{}, false
	}
	convert := func(c causeRefPayload) decision.CauseRef {
		return decision.CauseRef{
			Kind:      decision.CauseKind(c.Kind),
			RefID:     c.RefID,
			Weight:    c.Weight,
			SnippetZH: c.SnippetZH,
		}
	}
	supporting := make([]decision.CauseRef, 0, len(payload.Supporting))
	for _, c := range payload.Supporting {
		supporting = append(supporting, convert(c))
	}
	return decision.Attribution{
		Primary:       convert(payload.Primary),
		Supporting:    supporting,
		SurpriseLevel: payload.SurpriseLevel,
		NarrativeZH:   payload.NarrativeZH,
	}, true
}

// buildAttributionSnapshot 从单位当前可廉价获取的前因构造校验快照（纯函数，无 DB）。
func buildAttributionSnapshot(actor *unit.Record) decision.Snapshot {
	traits := map[string]float64{}
	if actor != nil {
		p := actor.Personality
		traits = map[string]float64{
			"courage":     p.Courage,
			"loyalty":     p.Loyalty,
			"aggression":  p.Aggression,
			"prudence":    p.Prudence,
			"sociability": p.Sociability,
			"integrity":   p.Integrity,
			"stability":   p.Stability,
			"ambition":    p.Ambition,
		}
	}
	pressure := decision.PressureFlags{}
	if actor != nil {
		s := actor.Status
		pressure = decision.PressureFlags{
			Hunger:  s.Hunger < 30,
			Threat:  s.InCombat,
			Injury:  s.HP < 25 || len(s.Injuries) > 0,
			Fatigue: s.Fatigue >= 70,
			Debt:    s.Wallet == 0,
		}
	}
	return decision.Snapshot{
		Traits:               traits,
		Memories:             map[string]decision.MemoryMeta{},
		Redlines:             map[string]string{},
		Relations:            map[string]decision.RelationAxes{},
		Pressure:             pressure,
		PlayerActionEventIDs: map[string]struct{}{},
	}
}

// checkAttribution 用「人格+压力」基础快照校验归因；第二返回值表示是否存在可校验归因。
// 仅供单元测试与无 DB 场景使用；生产链路用 prepareAttribution（含记忆/关系上下文）。
func (service *Service) checkAttribution(actor *unit.Record, choice unitDecisionChoicePayload) (decision.Verdict, bool) {
	attr, ok := toEngineAttribution(choice.Attribution)
	if !ok {
		return decision.Verdict{}, false
	}
	return decision.ValidateAttribution(attr, buildAttributionSnapshot(actor)), true
}

// buildDecisionAttributionContext 构造「人格+压力+记忆+关系」完整快照，并返回可暴露给 LLM 的
// 「可引用记忆 ID」prompt 块。记忆/关系来自实时存储，使 memory/relation 类前因得以解析。
func (service *Service) buildDecisionAttributionContext(ctx context.Context, state State, actor *unit.Record) (decision.Snapshot, string) {
	snap := buildAttributionSnapshot(actor)
	if service == nil || service.db == nil || actor == nil {
		return snap, ""
	}

	// 记忆：取近窗口结构化记忆，按 importance 排序取前 N，建可引用 ID 集合 + prompt 块。
	turn := state.TurnState.Turn
	if turn <= 0 {
		turn = 1
	}
	startTurn := turn - attributionMemoryWindow
	if startTurn < 1 {
		startTurn = 1
	}
	if rows, err := service.loadRecentMemoriesForPrompt(ctx, actor.ID, startTurn, turn); err == nil && len(rows) > 0 {
		sort.SliceStable(rows, func(i, j int) bool {
			return rows[i].Metadata.Importance > rows[j].Metadata.Importance
		})
		var block strings.Builder
		count := 0
		for _, row := range rows {
			if count >= attributionMemoryLimit {
				break
			}
			salience := row.Metadata.BaseSalience
			if salience < 0.5 {
				// 被选入近窗的记忆视为仍鲜活，避免基础显著度过低误杀有效前因。
				salience = 0.5
			}
			snap.Memories[row.ID] = decision.MemoryMeta{
				Importance:    row.Metadata.Importance,
				EmotionWeight: row.EmotionWeight,
				Salience:      salience,
				Summary:       row.Summary,
			}
			fmt.Fprintf(&block, "  %s：%s\n", row.ID, limitTextRunes(row.Summary, 24))
			count++
		}
		if count > 0 {
			promptBlock := "可引用的记忆 ID（attribution.primary.kind=memory 时，ref_id 用下列之一，snippet_zh 用对应摘要原文片段）：\n" + block.String()
			snap = withRelations(ctx, service, actor.ID, snap)
			return snap, promptBlock
		}
	}

	return withRelations(ctx, service, actor.ID, snap), ""
}

// withRelations 把单位的对外关系四轴（归一到 [-1,1]）填入快照，使 relation 类前因可解析。
func withRelations(ctx context.Context, service *Service, unitID string, snap decision.Snapshot) decision.Snapshot {
	for _, row := range service.loadTopOutgoingRelations(ctx, unitID, attributionRelationLimit) {
		snap.Relations[row.TargetUnitID] = decision.RelationAxes{
			Trust:     row.Trust / relationAxisScale,
			Affection: row.Affection / relationAxisScale,
			Fear:      row.Fear / relationAxisScale,
			Rivalry:   row.Rivalry / relationAxisScale,
		}
	}
	return snap
}

// prepareAttribution 在决策前准备归因上下文，返回 prompt 块与一个绑定了完整快照的校验闭包。
// 闭包让 llm.go 无需引入 engine/decision 即可校验：返回 (通过, 原因码, 是否存在归因)。
func (service *Service) prepareAttribution(ctx context.Context, state State, actor *unit.Record) (string, func(unitDecisionChoicePayload) (bool, string, bool)) {
	snap, block := service.buildDecisionAttributionContext(ctx, state, actor)
	validate := func(choice unitDecisionChoicePayload) (bool, string, bool) {
		attr, has := toEngineAttribution(choice.Attribution)
		if !has {
			return true, "", false
		}
		verdict := decision.ValidateAttribution(attr, snap)
		return verdict.OK, verdict.Reason, true
	}
	return block, validate
}

// oocFallbackDecision 是归因判 OOC 且强制开启时的优雅回退：不落地原意图，继续按兵不动（非乱码困惑态）。
func oocFallbackDecision() unitDecisionPayload {
	return unitDecisionPayload{
		Action:     DecisionActionHold,
		NextAction: "我再想想",
		Speak:      "我再想想。",
		Memory:     "我一时没理清自己的心思，先稳住。",
		Reasoning:  "归因未通过校验，谨慎起见先按兵不动。",
	}
}

// SetAttributionEnforcement 开关「归因判 OOC 即回退安全决策」的强制模式（默认关闭，仅影子遥测）。
func (service *Service) SetAttributionEnforcement(enabled bool) {
	service.attributionEnforced = enabled
}

// enforcementActive 返回当前是否实际执行强制：需本实例开启强制，且未触发全局自动降级。
func (service *Service) enforcementActive() bool {
	return service.attributionEnforced && !attributionDegraded.Load()
}

// AttributionStats 返回进程级归因校验计数（委托给包级 AttributionStats）。
func (service *Service) AttributionStats() (total int64, ooc int64) {
	return AttributionStats()
}

// attributionDecisionSchema 返回决策 schema 中可选 attribution 字段的子 schema（全字段可空、无必填，LLM 可整体省略）。
func attributionDecisionSchema(nullableString []string) map[string]any {
	causeRef := func() map[string]any {
		return map[string]any{
			"type": []string{"object", "null"},
			"properties": map[string]any{
				"kind":       map[string]any{"type": nullableString},
				"ref_id":     map[string]any{"type": nullableString},
				"weight":     map[string]any{"type": []string{"number", "null"}},
				"snippet_zh": map[string]any{"type": nullableString},
			},
			"additionalProperties": false,
		}
	}
	return map[string]any{
		"type": []string{"object", "null"},
		"properties": map[string]any{
			"primary": causeRef(),
			"supporting": map[string]any{
				"type":     []string{"array", "null"},
				"items":    causeRef(),
				"maxItems": 2,
			},
			"surprise_level": map[string]any{"type": []string{"integer", "null"}, "minimum": 0, "maximum": 3},
			"narrative_zh":   map[string]any{"type": nullableString, "maxLength": 40},
		},
		"additionalProperties": false,
	}
}
