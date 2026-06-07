package session

// 文件说明：把「牵挂等级」(engine/attachment) 接到真实数据上（设计宪法 §4.4 + §4.7 后果分级闸）。
// 共鸣=角色对你的忠诚（[0,1]）；共创=你为她处理待决策/接管的次数（events 表里数）；
// 在世天数由调用方给（回合数代理）；回访暂记 0（return_visit 埋点落地后接入）。
// 牵挂只喂 encounter.PenaltyCap 的 care——它是「能否对她造成更重后果」的钥匙，不可付费购买。

import (
	"context"

	"qunxiang/backend/internal/engine/attachment"
	"qunxiang/backend/internal/engine/events"
)

// ComputeAttachment 估算某角色的牵挂等级 [0,100]。loyalty 为该角色对你的忠诚 [0,1]；daysAlive 为在世天数（可用回合代理）。
func (service *Service) ComputeAttachment(ctx context.Context, actorID string, loyalty float64, daysAlive int) float64 {
	coCreations := 0
	if service != nil && service.db != nil {
		// 共创 = 你为她处理过的待决策 + 直接接管次数（events 表 append-only 留痕）。
		_ = service.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code IN (?, ?)`,
			actorID, string(events.ReasonDecisionResolved), string(events.ReasonPlayerIntervention),
		).Scan(&coCreations)
	}
	return attachment.Compute(attachment.Inputs{
		Resonance:    loyalty,
		DaysAlive:    daysAlive,
		ReturnVisits: 0,
		CoCreations:  coCreations,
	})
}

// defeatMoraleHitForLayer 把后果分级层映射为士气挫伤幅度：层越高代价越重（层3 只可能落在深牵挂+陪伴久的角色上）。
func defeatMoraleHitForLayer(layer int) float64 {
	switch {
	case layer >= 3:
		return 0.50
	case layer == 2:
		return 0.30
	default:
		return 0.15
	}
}
