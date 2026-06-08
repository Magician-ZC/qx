package session

// 文件说明：把「牵挂等级」(engine/attachment) 接到真实数据上（设计宪法 §4.4 + §4.7 后果分级闸）。
// 共鸣=角色对你的忠诚（[0,1]）；共创=你为她处理待决策/接管的次数（events 表里数）；
// 在世天数由调用方给（回合数代理）；回访=你专程回来看这个角色的次数（best-effort 查询，无数据时退回 DefaultReturnVisits=0）。
// 牵挂只喂 encounter.PenaltyCap 的 care——它是「能否对她造成更重后果」的钥匙，不可付费购买。

import (
	"context"

	"qunxiang/backend/internal/engine/attachment"
	"qunxiang/backend/internal/engine/events"
)

// ComputeAttachment 估算某角色的牵挂等级 [0,100]。loyalty 为该角色对你的忠诚 [0,1]；daysAlive 为在世天数（可用回合代理）。
// 回访计数走 returnVisitsForActor best-effort 查询喂进 attachment.ComputeWithSignals（替代旧的硬编码 0），
// 真实回访按角色聚合的数据源若尚未埋通则自动退回 attachment.DefaultReturnVisits，该维度恒为 0、不破坏单调性与值域。
func (service *Service) ComputeAttachment(ctx context.Context, actorID string, loyalty float64, daysAlive int) float64 {
	coCreations := 0
	if service != nil && service.db != nil {
		// 共创 = 你为她处理过的待决策 + 直接接管次数（events 表 append-only 留痕）。
		_ = service.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code IN (?, ?)`,
			actorID, string(events.ReasonDecisionResolved), string(events.ReasonPlayerIntervention),
		).Scan(&coCreations)
	}
	return attachment.ComputeWithSignals(loyalty, daysAlive, service.returnVisitsForActor(ctx, actorID), coCreations)
}

// returnVisitsForActor best-effort 查询「玩家专程回来看这个角色」的次数（牵挂的回访维度，不可付费）。
//
// 现状：return_visit 漏斗事件仅按匿名访客 vid 落在 fake_door_leads（见 httpapi/leads.go），未按 actor/玩家维度聚合，
// 故此处暂无可信的「按角色回访」数据源 → 返回 attachment.DefaultReturnVisits（0），该维度恒不贡献、保守退化。
// 一旦 return_visit 埋点带上 actor/session 维度（建议落 events 表的专属 reason_code 或 leads.payload_json.actor_id），
// 本函数改为对该维度做 COUNT 即可让回访真正参与牵挂——调用方与 attachment 包签名都无需再改（已对齐 ComputeWithSignals）。
func (service *Service) returnVisitsForActor(ctx context.Context, actorID string) int {
	_ = ctx
	_ = actorID
	return attachment.DefaultReturnVisits
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
