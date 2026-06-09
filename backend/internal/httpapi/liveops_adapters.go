package httpapi

// 文件说明：Live-Ops 平台层（internal/liveops）的三个依赖适配器，把 httpapi 已有的 session / ws.Hub 能力
// 适配进 liveops 的注入接口（HallArchiver / PaidResolver / Broadcaster）。与 billingEntitlementAdapter 同纪律：
// 适配器薄、不持业务逻辑，避免 liveops 反向 import session/ws/billing 造成循环依赖。

import (
	"context"
	"log"

	"qunxiang/backend/internal/liveops"
	"qunxiang/backend/internal/session"
	"qunxiang/backend/internal/ws"
)

// liveopsHallArchiver 把 session.Service.ArchiveCharacterToHall 适配成 liveops.HallArchiver。
// 复用既有名人堂归档原语（hall_of_fame_entries upsert + L3 冷存同步），不重写结算。
// newServiceFn 是每次新建一个 *session.Service 的工厂（与 newSessionService 闭包同源），避免持有跨请求的长生命 Service。
type liveopsHallArchiver struct {
	newServiceFn func() *session.Service
}

func (a liveopsHallArchiver) ArchiveCharacterToHall(ctx context.Context, worldID, characterID string) error {
	if a.newServiceFn == nil {
		return nil
	}
	return a.newServiceFn().ArchiveCharacterToHall(ctx, worldID, characterID)
}

// defaultPaidResolver 是付费态判定的保守默认实现：恒返回 false（审计退化为「无人付费」单组）。
//
// documented residual：unit(actorID)→single_player_sessions.account_id→billing 付费态的映射成本高且当前无稳定
// unitID→accountID 链路（单位归属随世界化/赛季流转，account_id 仅在 single_player_sessions 维度）。恒 false 是
// **安全保守值**：审计绝不会误把非付费战绩算进付费组、绝不误报 P2W 红线（只会在真有付费不公平时漏报，由后续
// unit→account 映射补全）。**铁律不变**：PaidResolver 仅用于审计观测分组，本就绝不进任何 Score/掉率/分赃。
func defaultPaidResolver() liveops.PaidResolver {
	return func(_ context.Context, _ string) bool {
		return false
	}
}

// liveopsBroadcaster 把 liveops 的世界级轻通知（赛季收尾/GM 注入）经 ws.Hub 旁路广播。
// best-effort：hub 为 nil 时仅 log.Printf；广播只发「世界级」轻通知，不携带敏感 payload（与 liveops.Broadcaster 约定一致）。
// 注意：ws.Hub 的订阅维度是 sessionID，这里以 worldID 作为「频道」复用 BroadcastSessionEvent——前端按需订阅 world 频道即可。
type liveopsBroadcaster struct {
	hub *ws.Hub
}

func (b liveopsBroadcaster) BroadcastWorldNotice(worldID, kind, message string) {
	if b.hub == nil {
		log.Printf("liveops world notice (no hub): world=%s kind=%s msg=%s", worldID, kind, message)
		return
	}
	// best-effort：BroadcastSessionEvent 内部对无订阅者静默；以 worldID 作频道键，kind 作事件类型。
	b.hub.BroadcastSessionEvent(worldID, "world_notice", map[string]any{
		"world_id": worldID,
		"kind":     kind,
		"message":  message,
	})
}
