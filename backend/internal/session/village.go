package session

// 文件说明：把 villageseed 生成的「出生即 20 人关系网」持久化进本局（设计宪法 §4.5）。
// 复用既有底座：unit.BootstrapRecord 建人、units.Save 落库、world.Join 入世界、applyRelationShift 织关系。
// 出身/种子记忆/人生目标写进 Biography（决策 prompt 会读到）；秘密与目标作为锚由 M2.3 的 relevance_anchors 接手。

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"qunxiang/backend/internal/engine/relevance"
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

	// 织出生关系网（四轴按种子的关系类型一次性落到位），并把强关系沉淀为持久「债仇爱」锚。
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
		// 强烈的爱/仇沉淀为持久 debt_grudge_love 锚（即使关系行后续变动，这份「在乎」也留痕）。
		intensity := absFloat(b.Affection) + absFloat(b.Rivalry) + absFloat(b.Fear)
		if intensity >= 8 {
			_ = service.UpsertAnchor(ctx, src.ID, relevance.DebtGrudgeLove, tgt.ID, clampFloat(intensity/24.0, 0, 1), b.Kind+"："+tgt.Identity.Name, relationAnchorHalfLife)
		}
	}

	// 每人的人生目标沉淀为持久 goal 锚（M3/事件标注目标后即可命中；现在先建好）。
	for i, m := range v.Members {
		_ = service.UpsertAnchor(ctx, records[i].ID, relevance.Goal, "goal:"+records[i].ID, clampFloat(0.5+m.Traits.Ambition*0.5, 0, 1), m.LifeGoal, 0)
	}
	return out, nil
}

// SeedVillageBestEffort 是 onboarding 用的吞错包装：调 SeedVillage 生成 20 人关系网，
// 失败只记日志、返回已落库人数，绝不让上层（/api/units/bootstrap）失败——村庄是附加体验。
// 返回值是「实际落库的村民数」（即便中途出错，前面已 Save 的人也算数）。
func (service *Service) SeedVillageBestEffort(ctx context.Context, sessionID string, factionID string, worldID string, seed int64) int {
	villagers, err := service.SeedVillage(ctx, sessionID, factionID, worldID, seed)
	if err != nil {
		log.Printf("seed village best-effort failed (session=%s faction=%s): %v; persisted %d", sessionID, factionID, err, len(villagers))
	}
	return len(villagers)
}

// mainVillageEnabled 读 QUNXIANG_MAIN_VILLAGE（true/1/yes/on 视为开），默认关 → seedVillageForSession no-op。
// 主战局默认不播种 20 人关系网：避免对所有存量/默认建局链路强行造人（每局多落库 20 行 + 关系/锚），
// 只有显式开启才在主局兑现命运开盒「她身边已有二十个有名有姓的人」承诺。与 onboarding 的 with_village 查询参数互不影响。
func mainVillageEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_MAIN_VILLAGE"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// seedVillageForSession 把「主战局是否在玩家身边织 20 人出生关系网」的策略集中于一处：建局/组队的两条主局路径
// （createSinglePlayerWithMapScript 非 draft 路径与 ApplyOpeningDraft）各调一行即可，避免漏播/口径漂移。
// 仿 seedAmbientForNewUnit 的集中化 idiom：单人局只为玩家阵营织本局关系网（worldID 传空=不入世界，安全）。
//
// 纪律与不变量：
//  1. flag-gated：QUNXIANG_MAIN_VILLAGE 关时（默认）整方法 no-op、零行为变化、零 DB 写——对默认建局链路无成本，
//     也避免对存量局/重连重复造人。
//  2. best-effort：复用 SeedVillageBestEffort 的吞错包装，任何失败只记日志，**绝不**中断或影响建局/组队。
//  3. 确定性：seed 由建局 RandomSeed 派生（state.RandomSeed+1，与 onboarding /api/units/bootstrap?with_village=1 同口径），
//     避免与玩家主单位撞种子；同一局重复调用是确定性一致的，但会新建行，故调用方须只在建局/组队各调一次。
//
// 注意：调用点应放在「玩家单位刚落库、紧邻 seedAmbientForUnits」处，让村民与玩家在同一建局事务边界内成形。
func (service *Service) seedVillageForSession(ctx context.Context, state *State) {
	if service == nil || state == nil || !mainVillageEnabled() {
		return
	}
	// worldID 传空：单人局当前无世界，只织本局关系网（不强行 world.Create）。
	_ = service.SeedVillageBestEffort(ctx, state.ID, state.PlayerFactionID, "", state.RandomSeed+1)
}
