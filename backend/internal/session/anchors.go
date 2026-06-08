package session

// 文件说明：相关性锚的持久层（设计 耦合 §1.1）。把「她在乎什么」做成可 upsert 的持久集合，
// 喂 engine/relevance.Score。关系锚仍由 relations 表实时派生；目标/红线/债仇爱/血脉这些非关系锚，
// 只有 relevance_anchors 这张表能存——这正是 fate.go 原先缺的那一半。

import (
	"context"
	"fmt"
	"math"

	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/storage/dbdialect"
)

const anchorDefaultHalfLifeDays = 14.0

// anchorDensitySaturation 控制锚密度饱和速度：Σ(weight·RelativeImportance) 达此值时密度≈0.63、约 2 倍时≈0.86。
const anchorDensitySaturation = 1.5

// AnchorDensity 返回某角色「在乎程度」的归一密度 [0,1]——锚（目标/红线/债仇爱/血脉 + 实时关系）越多越强、密度越高。
// 供 region-runner 锚加权威胁刷新（威胁天然扎堆她在乎的地方，PvE-4）：注入式 provider，故放 session（relevance 域知识）。
// 每类锚按 relevance.RelativeImportance 加权（红线/血脉重于泛泛关系），Σ 经饱和函数压进 [0,1] 不溢出。
func (service *Service) AnchorDensity(ctx context.Context, unitID string) float64 {
	if service == nil {
		return 0
	}
	anchors := service.buildRelevanceAnchors(ctx, unitID)
	var sum float64
	for _, a := range anchors {
		sum += a.Weight * relevance.RelativeImportance(a.Kind)
	}
	if sum <= 0 {
		return 0
	}
	return 1 - math.Exp(-sum/anchorDensitySaturation)
}

// UpsertAnchor 写入/更新一条相关性锚（按 (character, kind, ref) 主键累不重复）。weight 夹到 [0,1]。
func (service *Service) UpsertAnchor(ctx context.Context, characterID string, kind relevance.AnchorKind, ref string, weight float64, label string, halfLifeDays float64) error {
	if service == nil || service.db == nil {
		return fmt.Errorf("upsert anchor: missing db")
	}
	if characterID == "" || kind == "" || ref == "" {
		return fmt.Errorf("upsert anchor: empty character/kind/ref")
	}
	if weight < 0 {
		weight = 0
	}
	if weight > 1 {
		weight = 1
	}
	if halfLifeDays <= 0 {
		halfLifeDays = anchorDefaultHalfLifeDays
	}
	query := `
		INSERT INTO relevance_anchors (character_unit_id, anchor_kind, anchor_ref, weight, label, half_life_days, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(character_unit_id, anchor_kind, anchor_ref) DO UPDATE SET
			weight = excluded.weight, label = excluded.label, half_life_days = excluded.half_life_days, updated_at = excluded.updated_at`
	args := []any{characterID, string(kind), ref, weight, label, halfLifeDays}
	if dbdialect.IsMySQL(service.db) {
		query = `
			INSERT INTO relevance_anchors (character_unit_id, anchor_kind, anchor_ref, weight, label, half_life_days, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '')
			ON DUPLICATE KEY UPDATE
				weight = VALUES(weight), label = VALUES(label), half_life_days = VALUES(half_life_days)`
	}
	if _, err := service.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("upsert anchor: %w", err)
	}
	return nil
}

// loadPersistentAnchors 读某角色已落库的相关性锚（含非关系锚）。
func (service *Service) loadPersistentAnchors(ctx context.Context, characterID string) []relevance.Anchor {
	anchors := make([]relevance.Anchor, 0)
	if service == nil || service.db == nil {
		return anchors
	}
	rows, err := service.db.QueryContext(ctx, `
		SELECT anchor_kind, anchor_ref, weight, half_life_days
		FROM relevance_anchors WHERE character_unit_id = ?
		ORDER BY weight DESC`, characterID)
	if err != nil {
		return anchors
	}
	defer rows.Close()
	for rows.Next() {
		var kind, ref string
		var weight, halfLife float64
		if err := rows.Scan(&kind, &ref, &weight, &halfLife); err != nil {
			return anchors
		}
		if weight <= 0 {
			continue
		}
		anchors = append(anchors, relevance.Anchor{
			Kind:         relevance.AnchorKind(kind),
			Ref:          ref,
			Weight:       weight,
			HalfLifeDays: halfLife,
		})
	}
	return anchors
}
