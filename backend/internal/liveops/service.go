// Package liveops 是 Live-Ops 平台层（设计 docs/产品方案PRD.md §8「无 live-ops 即慢性死亡」、docs/验证实验设计.md）。
// 三件事编排在一个服务里：① GM 世界事件注入（运营在后台往某世界投一条权威跨事件，留全量可仲裁审计）；
// ② 赛季骨架（创建/收尾一个绑定单世界的赛季，收尾时把存活角色回流名人堂、世界封存、广播 finalized）；
// ③ 零和监控审计（扫某回合区间的仲裁/争夺结局，按付费态分组算胜率，>60% 判反 P2W 红线）。
//
// 边界铁律：
//   - 所有写都走既有 append-only 表（cross_events / gm_events_audit / seasons / hall_of_fame_entries），永不改写历史。
//   - 审计**只观测**付费态有没有不公平地赢——付费态来自 billing，但**绝不进** arbitration.Resolve 的 Score（反 P2W 红线）。
//   - 传承归档**复用**既有名人堂/冷存路径，本包只调用、不重写结算。
//   - 全程 best-effort 友好：导出函数返回错误供运营端面，但被 service 边界调用时一律吞错只记 log，绝不阻断主结算循环。
package liveops

import (
	"context"
	"database/sql"
	"strings"

	"qunxiang/backend/internal/storage/dbdialect"
)

// HallArchiver 是把一个存活角色回流名人堂的能力（由 session.Service 提供，复用既有传承归档；本包只调用）。
// 赛季收尾时对每个存活成员调用一次；返回错误时 FinalizeSeason 记 log 继续（best-effort，不阻断封存）。
type HallArchiver interface {
	ArchiveCharacterToHall(ctx context.Context, worldID, characterID string) error
}

// PaidResolver 判定一个角色（actorID）当下是否处于付费态。
// **铁律**：返回值只用于审计分组（观测付费有没有不公平地赢），绝不回流进任何 Score/掉率/分赃。
// 由调用方注入 billing-backed 实现；未注入时默认全员非付费（审计退化为单组，不误报红线）。
type PaidResolver func(ctx context.Context, actorID string) bool

// Broadcaster 是把一条运营事件（赛季收尾/GM 注入）旁路广播给前端的能力（可选；nil 时静默跳过）。
// 复用既有 WebSocket Hub 的 BroadcastSessionEvent 语义由调用方适配；本包只发「世界级」轻通知，不携带敏感 payload。
type Broadcaster interface {
	BroadcastWorldNotice(worldID, kind, message string)
}

// LiveopsService 是 Live-Ops 平台层的服务句柄。db 必须非空；其余依赖均可选。
type LiveopsService struct {
	db          *sql.DB
	dialect     dbdialect.Dialect
	archiver    HallArchiver
	paid        PaidResolver
	broadcaster Broadcaster
}

// NewService 构造 LiveopsService。dialect 由 db 推断（开新连接前应已 dbdialect.Register）。
func NewService(db *sql.DB) *LiveopsService {
	return &LiveopsService{
		db:      db,
		dialect: dbdialect.For(db),
	}
}

// WithArchiver 注入名人堂归档能力（赛季收尾回流存活角色）。返回自身便于链式。
func (s *LiveopsService) WithArchiver(a HallArchiver) *LiveopsService {
	if s != nil {
		s.archiver = a
	}
	return s
}

// WithPaidResolver 注入付费态判定（仅供审计分组，绝不进 Score）。返回自身便于链式。
func (s *LiveopsService) WithPaidResolver(r PaidResolver) *LiveopsService {
	if s != nil {
		s.paid = r
	}
	return s
}

// WithBroadcaster 注入世界级轻通知广播（可选）。返回自身便于链式。
func (s *LiveopsService) WithBroadcaster(b Broadcaster) *LiveopsService {
	if s != nil {
		s.broadcaster = b
	}
	return s
}

// ready 返回服务是否可用（db 非空）。
func (s *LiveopsService) ready() bool {
	return s != nil && s.db != nil
}

// resolvePaid 在注入了 PaidResolver 时调用之，否则返回 false（审计退化为「无人付费」单组，保守不误报）。
func (s *LiveopsService) resolvePaid(ctx context.Context, actorID string) bool {
	if s == nil || s.paid == nil || strings.TrimSpace(actorID) == "" {
		return false
	}
	return s.paid(ctx, actorID)
}
