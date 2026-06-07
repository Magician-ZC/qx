package session

// 文件说明：把 villageseed 生成的「出生即 20 人关系网」持久化进本局（设计宪法 §4.5）。
// 复用既有底座：unit.BootstrapRecord 建人、units.Save 落库、world.Join 入世界、applyRelationShift 织关系。
// 出身/种子记忆/人生目标写进 Biography（决策 prompt 会读到）；秘密与目标作为锚由 M2.3 的 relevance_anchors 接手。

import (
	"context"
	"fmt"

	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/villageseed"
	"qunxiang/backend/internal/world"
)

// SeededVillager 是一名已落库的村民，连同其原始种子档案（供上层接锚/叙事）。
type SeededVillager struct {
	UnitID  string
	Member  villageseed.Member
	WorldID string
}

// SeedVillage 确定性地为某局生成并落库 20 人出生关系网，全部接入 worldID（worldID 可空=不入世界）。
// 返回每名村民及其原始档案。同一 (worldID, seed) 重复调用，人是确定性一致的（但会新建行——调用方应只在建局时调一次）。
func (service *Service) SeedVillage(ctx context.Context, sessionID string, factionID string, worldID string, seed int64) ([]SeededVillager, error) {
	if service == nil || service.units == nil || service.db == nil {
		return nil, fmt.Errorf("seed village: missing dependencies")
	}
	v := villageseed.Generate(worldID, seed)

	records := make([]unit.Record, len(v.Members))
	out := make([]SeededVillager, 0, len(v.Members))
	for i, m := range v.Members {
		rec := unit.BootstrapRecord(seed+int64(i)*1009, sessionID, factionID, m.Name)
		rec.Personality = unit.Personality{
			Courage:     m.Traits.Courage,
			Loyalty:     m.Traits.Loyalty,
			Aggression:  m.Traits.Aggression,
			Prudence:    m.Traits.Prudence,
			Sociability: m.Traits.Sociability,
			Integrity:   m.Traits.Integrity,
			Stability:   m.Traits.Stability,
			Ambition:    m.Traits.Ambition,
		}
		rec.Identity.Gender = m.Gender
		rec.Identity.Age = m.Age
		rec.Identity.Lineage = m.Archetype
		rec.Identity.Biography = fmt.Sprintf("%s出身。%s 心里一直惦记着一件事：%s。", m.Archetype, m.SeedMemory, m.LifeGoal)
		if err := service.units.Save(ctx, rec); err != nil {
			return out, fmt.Errorf("save villager %d: %w", i, err)
		}
		if worldID != "" {
			_ = world.Join(ctx, service.db, worldID, rec.ID, "inhabitant", dbdialect.For(service.db))
		}
		records[i] = rec
		out = append(out, SeededVillager{UnitID: rec.ID, Member: m, WorldID: worldID})
	}

	// 织出生关系网（四轴按种子的关系类型一次性落到位）。
	for _, b := range v.Bonds {
		if b.From < 0 || b.From >= len(records) || b.To < 0 || b.To >= len(records) {
			continue
		}
		src := records[b.From]
		tgt := records[b.To]
		if _, err := service.applyRelationShift(ctx, nil, &src, &tgt, relationDelta{
			Trust: b.Trust, Fear: b.Fear, Affection: b.Affection, Rivalry: b.Rivalry,
		}, "出生关系网·"+b.Kind); err != nil {
			return out, fmt.Errorf("seed bond %d->%d: %w", b.From, b.To, err)
		}
	}
	return out, nil
}
