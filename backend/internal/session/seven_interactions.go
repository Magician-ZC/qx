package session

// 文件说明：七种跨玩家交互的统一协议（设计 docs/事件耦合与跨玩家关联.md §2.3）：结识/结盟/交易/联姻/反目/复仇/开战。
// 每种交互 = (worldbus 事件类型 + 后果层 + actor→target 四轴关系增量)。统一管线 RecordSevenInteraction：
// 先把交互记进世界总线（append-only 事实源），再按后果层经 relevance.ConsentTierFor 路由——
// 单方成立(unilateral)立即应用关系效果；需对方角色自治同意(contested/requires_consent)则建 consent_request 待 resolve（见 consent_gate.go）。

import (
	"context"
	"fmt"

	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/worldbus"
)

// SevenInteraction 是七种跨玩家交互之一。
type SevenInteraction string

const (
	InteractionAcquaint  SevenInteraction = "acquaint"  // 结识
	InteractionAlliance  SevenInteraction = "alliance"  // 结盟
	InteractionTrade     SevenInteraction = "trade"     // 交易
	InteractionMarriage  SevenInteraction = "marriage"  // 联姻
	InteractionFallout   SevenInteraction = "fallout"   // 反目
	InteractionVengeance SevenInteraction = "vengeance" // 复仇
	InteractionWar       SevenInteraction = "war"       // 开战
)

type interactionTemplate struct {
	Kind             worldbus.EventKind
	ConsequenceLayer int           // 喂 ConsentTierFor：1=unilateral 2=contested 3=requires_consent
	Delta            relationDelta // actor→target 四轴增量（accept/unilateral 时应用，clamp 到 [-10,10]）
	Reason           string
}

// sevenTemplates 七种交互→(总线类型, 后果层, 关系增量)。后果越重→越需对方同意（联姻/复仇/开战=层3）。
var sevenTemplates = map[SevenInteraction]interactionTemplate{
	InteractionAcquaint:  {worldbus.KindAcquaint, 1, relationDelta{Trust: 1}, "结识"},
	InteractionTrade:     {worldbus.KindGift, 1, relationDelta{Trust: 1, Affection: 0.5}, "交易"},
	InteractionAlliance:  {worldbus.KindAlliance, 2, relationDelta{Trust: 3, Affection: 2}, "结盟"},
	InteractionFallout:   {worldbus.KindBetrayal, 2, relationDelta{Trust: -3, Rivalry: 3}, "反目"},
	InteractionMarriage:  {worldbus.KindMarriage, 3, relationDelta{Affection: 5, Trust: 3}, "联姻"},
	InteractionVengeance: {worldbus.KindVengeance, 3, relationDelta{Fear: 4, Rivalry: 4, Affection: -3}, "复仇"},
	InteractionWar:       {worldbus.KindAttack, 3, relationDelta{Fear: 3, Rivalry: 4, Trust: -2}, "开战"},
}

// SevenInteractionResult 是一次交互的结果：事件 id、同意档、是否立即应用、需同意时的请求 id。
type SevenInteractionResult struct {
	EventID          string `json:"event_id"`
	Interaction      string `json:"interaction"`
	Tier             string `json:"tier"`
	Applied          bool   `json:"applied"`
	ConsentRequestID string `json:"consent_request_id,omitempty"`
}

// RecordSevenInteraction 记录并按 consent 档路由一次七种交互之一。
func (service *Service) RecordSevenInteraction(ctx context.Context, worldID, actorID, targetID string, interaction SevenInteraction, importance int) (SevenInteractionResult, error) {
	tmpl, ok := sevenTemplates[interaction]
	if !ok {
		return SevenInteractionResult{}, fmt.Errorf("unknown interaction %q", interaction)
	}
	if service == nil || service.db == nil || service.units == nil {
		return SevenInteractionResult{}, fmt.Errorf("seven interaction: missing deps")
	}
	if worldID == "" || actorID == "" || targetID == "" {
		return SevenInteractionResult{}, fmt.Errorf("seven interaction: world/actor/target required")
	}

	// 1) 先把交互记进世界总线（事实源，append-only，无论同意与否「谁先动手」都留痕）。
	eventID, err := service.RecordCrossInteraction(ctx, worldID, actorID, targetID, tmpl.Kind, importance, map[string]any{"interaction": string(interaction)})
	if err != nil {
		return SevenInteractionResult{}, err
	}

	tier := relevance.ConsentTierFor(tmpl.ConsequenceLayer)
	res := SevenInteractionResult{EventID: eventID, Interaction: string(interaction), Tier: string(tier)}

	// 2) 按档路由关系效果。
	if tier == relevance.Unilateral {
		if err := service.applySevenEffect(ctx, actorID, targetID, tmpl); err != nil {
			return res, err
		}
		res.Applied = true
		return res, nil
	}
	// contested / requires_consent：建 pending 同意请求，关系效果待 ResolveConsentRequest。
	reqID, err := service.createConsentRequest(ctx, worldID, actorID, targetID, interaction, tier, eventID)
	if err != nil {
		return res, err
	}
	res.ConsentRequestID = reqID
	return res, nil
}

// applySevenEffect 加载 actor/target 并应用四轴关系增量（unilateral 或 consent accept 时）。
// **best-effort 跨分片安全**：actor/target 任一不在本库（跨分片/远端角色）→ 关系效果本地不应用、返回 nil
// （世界总线事件已是跨分片可仲裁事实；relations 表有 units FK，远端角色本就无法落本地关系行）。
// 仅真实 DB 写失败才返错。这样七种交互对「目标在别分片」的设计场景不再整体崩坏（评审 load-bearing）。
func (service *Service) applySevenEffect(ctx context.Context, actorID, targetID string, tmpl interactionTemplate) error {
	actor, err := service.units.GetByID(ctx, actorID)
	if err != nil {
		return nil // actor 不在本库 → 跳过本地关系效果（best-effort）
	}
	target, err := service.units.GetByID(ctx, targetID)
	if err != nil {
		return nil // target 跨分片/远端 → 跳过本地关系效果（best-effort）
	}
	if _, err := service.applyRelationShift(ctx, nil, &actor, &target, tmpl.Delta, "七种交互·"+tmpl.Reason); err != nil {
		return fmt.Errorf("seven effect apply relation: %w", err)
	}
	return nil
}
