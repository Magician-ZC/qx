package session

// 文件说明：社会客体撮合（设计 docs/事件耦合与跨玩家关联.md §2.2）。把候选角色按 relevance.MatchScore 四因子打分、
// 过 PassesMatch 门，再用 arbitration.Resolve **确定性**择 slots 人（胜率∝Score、与频率/入队顺序无关、付费不进 Score——反 P2W），
// 绑进一个社会客体并留痕。撮合原语**通用**：四因子由调用方按情境算（组队/结盟/市集各有口径），本函数只管打分→择人→绑定。

import (
	"context"
	"fmt"
	"hash/fnv"

	"qunxiang/backend/internal/engine/arbitration"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/socialobject"
)

// MatchCandidate 是一名撮合候选 + 其四因子（地理临近/钩子契合/关系交集/密度调节，各 [0,1]）。
type MatchCandidate struct {
	UnitID            string
	GeoNear           float64
	HookFit           float64
	RelationIntersect float64
	DensityAdj        float64
}

// MatchIntoSocialObject 撮合候选进一个社会客体：打分→过门→arbitration 确定性择 slots 人→建客体+绑成员+留痕。
// 返回 (objectID, 选中的 unitIDs)。无人达标则不建客体、返回空。同一 (worldID,kind,label) 重复撮合更新同一客体（确定性 id）。
func (service *Service) MatchIntoSocialObject(ctx context.Context, worldID, kind, label string, candidates []MatchCandidate, slots int) (string, []string, error) {
	if service == nil || service.db == nil {
		return "", nil, fmt.Errorf("match: missing db")
	}
	if worldID == "" || kind == "" {
		return "", nil, fmt.Errorf("match: world_id/kind required")
	}

	contestants := make([]arbitration.Contestant, 0, len(candidates))
	scores := make(map[string]float64, len(candidates))
	for _, c := range candidates {
		if c.UnitID == "" {
			continue
		}
		s := relevance.MatchScore(c.GeoNear, c.HookFit, c.RelationIntersect, c.DensityAdj)
		if !relevance.PassesMatch(s) {
			continue // 未达撮合门 → 不参与
		}
		contestants = append(contestants, arbitration.Contestant{UnitID: c.UnitID, Score: s})
		scores[c.UnitID] = s
	}
	if len(contestants) == 0 {
		return "", nil, nil // 无人达标，不建客体
	}

	key := worldID + "|" + kind + "|" + label
	out := arbitration.Resolve(arbitration.Contest{Key: key, Resource: kind, Contestants: contestants})
	chosen := out.Ranking
	if slots > 0 && len(chosen) > slots {
		chosen = chosen[:slots]
	}

	objID, err := socialobject.Create(ctx, service.db, socialobject.SocialObject{
		ID: socialObjectID(key), WorldID: worldID, Kind: kind, Label: label,
	})
	if err != nil {
		return "", nil, fmt.Errorf("match create object: %w", err)
	}
	for _, uid := range chosen {
		if err := socialobject.AddMember(ctx, service.db, socialobject.Member{ObjectID: objID, UnitID: uid, Score: scores[uid]}); err != nil {
			return objID, chosen, fmt.Errorf("match add member: %w", err)
		}
		// 留痕（流程事件，非状态变更）：best-effort，绝不阻断撮合。RelatedUnitID 留空（默认回退 owner——
		// events.target_unit_id 有 units FK，社会客体非单位故不能放此，object_id 入 Payload）。
		_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
			SessionID: worldID, OwnerUnitID: uid,
			Code: events.ReasonSocialObjectBind, Category: events.CategoryFate,
			Payload: map[string]any{"object_id": objID, "kind": kind, "label": label, "score": scores[uid]},
			WorldID: worldID,
		})
	}
	return objID, chosen, nil
}

// socialObjectID 由 (world|kind|label) 派生确定性 id，使同一撮合上下文复用同一客体（幂等更新）。
func socialObjectID(key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("so_%016x", h.Sum64())
}
