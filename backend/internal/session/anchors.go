package session

// 文件说明：相关性锚的持久层（设计 耦合 §1.1）。把「她在乎什么」做成可 upsert 的持久集合，
// 喂 engine/relevance.Score。关系锚仍由 relations 表实时派生；目标/红线/债仇爱/血脉这些非关系锚，
// 只有 relevance_anchors 这张表能存——这正是 fate.go 原先缺的那一半。

import (
	"context"
	"fmt"
	"math"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/relevance"
	"qunxiang/backend/internal/runtimeconfig"
	"qunxiang/backend/internal/storage/dbdialect"
)

// anchorDefaultHalfLifeDays 的默认半衰（14 天）现已迁入 runtimeconfig（"anchor.default_half_life_days"）；
// 仅在 halfLifeDays<0「未指定」时作回落默认（见 UpsertAnchor），故直接在读取站点取 runtimeconfig 即可。

// geoAnchorHalfLifeDays 是「所在地」geo 锚的半衰期（设计 §1.1：地理牵挂随离开而淡，3 天半衰）。
// 现已迁入 runtimeconfig（"anchor.geo_half_life_days"，默认即此值）；此处保留为冻结默认基线（worldize_inbound_test 仍引用）。
const geoAnchorHalfLifeDays = 3.0

// geoAnchorDefaultWeight 是「她所在的地方」geo 锚的默认权重（接入世界时落锚用）。geo 是相关性里最轻的一类
// （relevance.RelativeImportance(Geo) 已大幅折扣），故 weight 取一个「确实身在此处」的中高值而非满值——
// 让地理牵挂在世界事件聚焦里有分量但不压过关系/血脉/红线这些更重的锚。
const geoAnchorDefaultWeight = 0.6

// legacyAnchorHalfLife<=0 表示「血脉/传家物」legacy 锚永不衰减（与 relevance.Anchor 约定一致：传承是恒久的弦）。
const legacyAnchorHalfLife = 0.0

// 锚密度饱和速度（默认 1.5）现已迁入 runtimeconfig（"anchor.density_saturation"）：正向 AnchorDensity 与
// 反向 AnchorDensityByRef 同口径共用同一饱和参数（量纲一致，让「她在乎多少」与「多少人在乎她」可比）——
// Σ(weight·RelativeImportance) 达此值时密度≈0.63、约 2 倍时≈0.86。

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
	return 1 - math.Exp(-sum/runtimeconfig.GetFloat("anchor.density_saturation"))
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
	// halfLifeDays 语义：**0 = 永不衰减**（红线/血脉/目标这类恒久的弦，与 relevance.Anchor「HalfLifeDays<=0 不衰减」一致）；
	// 仅**负值**视为「未指定」回落默认半衰。此前把 0 也当未指定折成默认 14，会让本该恒久的锚悄悄衰减——已修正为保留 0。
	if halfLifeDays < 0 {
		halfLifeDays = runtimeconfig.GetFloat("anchor.default_half_life_days")
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

// ===== §1.3 锚自动 upsert：业务自然时刻的「牵挂落定」生产调用方（主控在对应业务点调用）=====
//
// 这四个导出 helper 把 UpsertAnchor 包成「带语义 + 留痕」的业务钩子：在关系/目标/债务/地理/血脉变更的自然时刻
// （贸易成交/结盟/进入新 region/血脉绑定）由主控调用，落锚的同时写一条 ReasonAnchorLit 流程事件留痕——
// 让「某个人、某件事自此成了她心上的一根线」可审计、可被入向反查命中。**best-effort**：upsert 失败即返回错误
// 由调用方决定是否记 log，但 emitAnchorLit 留痕失败绝不回滚已落的锚（留痕是辅助）。付费不进（纯关系/事件驱动）。

// UpsertGoalAnchor 在「目标确立/推进」时落一根 goal 锚（设计 §1.1：当前目标，半衰由调用方按目标时效给，默认不衰减）。
// ref 约定用 "goal:"+characterID（与 SeedVillage 一致，保证幂等覆盖同一人的当前目标）。
func (service *Service) UpsertGoalAnchor(ctx context.Context, sessionID, characterID, goalRef string, weight float64, label string) error {
	if goalRef == "" {
		goalRef = "goal:" + characterID
	}
	if err := service.UpsertAnchor(ctx, characterID, relevance.Goal, goalRef, weight, label, 0); err != nil {
		return err
	}
	service.emitAnchorLit(ctx, sessionID, characterID, relevance.Goal, goalRef, label)
	return nil
}

// UpsertDebtAnchor 在「贸易成交/结盟/欠下人情或仇怨」时落一根 debt_grudge_love 锚（设计 §1.3 白名单的债务源）。
// halfLife 默认走关系锚半衰（relationAnchorHalfLife），债/仇/情会随时间淡去但比泛泛关系更久驻心。
func (service *Service) UpsertDebtAnchor(ctx context.Context, sessionID, characterID, counterpartID string, weight float64, label string) error {
	if err := service.UpsertAnchor(ctx, characterID, relevance.DebtGrudgeLove, counterpartID, weight, label, relationAnchorHalfLife); err != nil {
		return err
	}
	service.emitAnchorLit(ctx, sessionID, characterID, relevance.DebtGrudgeLove, counterpartID, label)
	return nil
}

// UpsertGeoAnchor 在「进入新 region」时落一根 geo 锚（设计 §1.1：所在地，半衰 3 天——离开后地理牵挂渐淡）。
// ref 用 regionID；重复进同一区幂等刷新权重与 updated_at（让 TimeDecay 从最近一次驻留起算）。
func (service *Service) UpsertGeoAnchor(ctx context.Context, sessionID, characterID, regionID string, weight float64, label string) error {
	if err := service.UpsertAnchor(ctx, characterID, relevance.Geo, regionID, weight, label, runtimeconfig.GetFloat("anchor.geo_half_life_days")); err != nil {
		return err
	}
	service.emitAnchorLit(ctx, sessionID, characterID, relevance.Geo, regionID, label)
	return nil
}

// UpsertLegacyAnchor 在「血脉绑定/继承传家物」时落一根 legacy 锚（设计 §1.1：传家物/血脉，永不衰减）。
// ref 用血脉/传家物的稳定 ID（如后代 unitID 或物品 ID）。这是恒久的弦，halfLife=0。
func (service *Service) UpsertLegacyAnchor(ctx context.Context, sessionID, characterID, legacyRef string, weight float64, label string) error {
	if err := service.UpsertAnchor(ctx, characterID, relevance.Legacy, legacyRef, weight, label, legacyAnchorHalfLife); err != nil {
		return err
	}
	service.emitAnchorLit(ctx, sessionID, characterID, relevance.Legacy, legacyRef, label)
	return nil
}

// emitAnchorLit 写一条 ReasonAnchorLit 流程事件留痕（「牵挂落定」）。best-effort：失败只吞错、绝不回滚已落的锚、不阻断业务。
func (service *Service) emitAnchorLit(ctx context.Context, sessionID, characterID string, kind relevance.AnchorKind, ref, label string) {
	if service == nil || service.db == nil || characterID == "" {
		return
	}
	// RelatedUnitID 故意留空（EmitProcessEvent 会回落为 owner=characterID，一定是真实单位）——anchor_ref 可能是
	// goal:xxx / regionID / legacyRef 这类**非单位**引用，直接塞进 target_unit_id 会触发 events 表的 FK 约束失败。
	// ref 完整写进 payload.anchor_ref 即可被检索，不丢信息。
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		SessionID:   sessionID,
		OwnerUnitID: characterID,
		Code:        events.ReasonAnchorLit,
		Category:    events.CategoryFate,
		Payload: map[string]any{
			"anchor_kind": string(kind),
			"anchor_ref":  ref,
			"label":       label,
		},
	})
}

// AnchorDensityByRef 反向查「多少角色以 ref 为锚（且为指定 kind）」的归一密度 ∈ [0,1]（设计 §1.5 NPC 锚加权预算的输入）。
// 用反向索引 idx_relevance_anchors_ref(anchor_ref, anchor_kind) 聚合所有指向 ref 的锚权重，按 relevance.RelativeImportance
// 加权求和，再经饱和函数压进 [0,1]。语义：一个 NPC/地方被越多角色当作「她在乎的人/地方」指向，密度越高——威胁/事件
// 越该聚焦到她身上（§1.5「世界事件自然聚焦在她在乎的人和地方」）。kind 为空时跨所有类别聚合（任意指向 ref 的锚都算）。
// best-effort：db 缺失/查询失败返回 0（退化为「无人在乎」=噪声世界自消化，保守不夸大）。确定性、付费无关。
func (service *Service) AnchorDensityByRef(ctx context.Context, ref string, kind relevance.AnchorKind) float64 {
	if service == nil || service.db == nil || ref == "" {
		return 0
	}
	query := `SELECT anchor_kind, weight FROM relevance_anchors WHERE anchor_ref = ?`
	args := []any{ref}
	if kind != "" {
		query += ` AND anchor_kind = ?`
		args = append(args, string(kind))
	}
	rows, err := service.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var sum float64
	for rows.Next() {
		var k string
		var weight float64
		if err := rows.Scan(&k, &weight); err != nil {
			return 0
		}
		if weight <= 0 {
			continue
		}
		if weight > 1 {
			weight = 1
		}
		sum += weight * relevance.RelativeImportance(relevance.AnchorKind(k))
	}
	if err := rows.Err(); err != nil {
		return 0
	}
	if sum <= 0 {
		return 0
	}
	return 1 - math.Exp(-sum/runtimeconfig.GetFloat("anchor.density_saturation"))
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
