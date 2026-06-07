package session

// 文件说明：把 engine/decision 的归因校验（「意外但合理」的代码强制，见设计宪法 §5）接入会话决策链路。
// 接入策略：影子模式优先——决策附带的 attribution 会被校验并计入 OOC 遥测，但默认不阻断游戏决策；
// 仅当显式开启强制（SetAttributionEnforcement(true)）时，归因判 OOC 才回退到安全决策。
// 现阶段快照只填充「人格维(persona_trait)」与「现实压力位(pressure)」这两类 ref_id 稳定可知、
// LLM 无需暴露内部 ID 即可正确引用的前因；memory/relation/redline 的解析需另接记忆/关系/宪章上下文（后续）。

import (
	"qunxiang/backend/internal/engine/decision"
	"qunxiang/backend/internal/unit"
)

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

// checkAttribution 校验决策附带的归因；第二返回值表示是否存在可校验归因。
func (service *Service) checkAttribution(actor *unit.Record, choice unitDecisionChoicePayload) (decision.Verdict, bool) {
	attr, ok := toEngineAttribution(choice.Attribution)
	if !ok {
		return decision.Verdict{}, false
	}
	return decision.ValidateAttribution(attr, buildAttributionSnapshot(actor)), true
}

// SetAttributionEnforcement 开关「归因判 OOC 即回退安全决策」的强制模式（默认关闭，仅影子遥测）。
func (service *Service) SetAttributionEnforcement(enabled bool) {
	service.attributionEnforced = enabled
}

// AttributionStats 返回归因校验的累计计数：总校验数与判定为 OOC 的数量。
func (service *Service) AttributionStats() (total int64, ooc int64) {
	return service.attrTotal.Load(), service.attrOOC.Load()
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
