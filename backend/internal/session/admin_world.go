package session

// 文件说明：GM 后台「世界配置服务」——把散落的 world / region / village 能力收口成 GM 可调的世界维度，
// 供独立 GM 管理后台（全 opsTokenGuard）按维度运营大世界：
//   - ListWorldsDetail：世界 + region + 人口概览（GM 总览页数据源）。
//   - SetRegionThreatLevel：把某 region 的威胁等级**绝对置位**（供「人工拉高某地威胁度做活动/演练」）。
//   - SeedWorldVillage：触发 SeedVillage 为某局/世界确定性织一张 20 人出生关系网（供 GM 手动播种世界人口）。
//
// 同时实现 featureflags.Store（运行时 flag override 的双驱动持久化后端），让 GM 设过的开关重启存活。
// 本服务只**复用**既有 world/region/village 能力、不重写结算；全程 best-effort 友好（导出函数返回错误供端点透传，
// 但绝不在内部吞错处偷改受保护状态/世界事实）。所有写都不触及 status.Mutator 守护的受保护字段。

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/region"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/world"
)

// ===== 世界配置服务（GM 可调维度） =====

// RegionDetail 是一个 region 的 GM 视图（活跃度档 / 威胁等级 / 逻辑时钟）。
type RegionDetail struct {
	ID           string `json:"id"`
	WorldID      string `json:"world_id"`
	ActivityTier string `json:"activity_tier"`
	ThreatLevel  int64  `json:"threat_level"`
	LastTick     int64  `json:"last_tick"`
}

// WorldDetail 是一个世界的 GM 总览（含 region 概览与人口数）。
type WorldDetail struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Tick          int            `json:"tick"`
	MaxPopulation int            `json:"max_population"`
	Population    int            `json:"population"` // 已接入成员数（world_members）
	Regions       []RegionDetail `json:"regions"`
}

// ListWorldsDetail 列出全部世界及其 region/人口概览（GM 总览页数据源）。
// limit<=0 时取 100（对齐 world.List 缺省）。best-effort：单个世界的 region/成员查询失败不影响其余世界，
// 只把该世界的对应字段留空——总览页绝不因一处子查询失败而整体 500。
func (service *Service) ListWorldsDetail(ctx context.Context, limit int) ([]WorldDetail, error) {
	if service == nil || service.db == nil {
		return nil, fmt.Errorf("list worlds detail: nil service or db")
	}
	worlds, err := world.List(ctx, service.db, "", limit)
	if err != nil {
		return nil, fmt.Errorf("list worlds detail: %w", err)
	}
	registry := region.New(service.db)
	out := make([]WorldDetail, 0, len(worlds))
	for _, w := range worlds {
		detail := WorldDetail{
			ID:            w.ID,
			Name:          w.Name,
			Status:        string(w.Status),
			Tick:          w.Tick,
			MaxPopulation: w.MaxPopulation,
		}
		// 人口 = 已接入世界的成员数（best-effort）。
		if members, mErr := world.Members(ctx, service.db, w.ID, 0); mErr == nil {
			detail.Population = len(members)
		}
		// region 概览：跨三档汇总该世界的 region（best-effort）。
		detail.Regions = service.listRegionsForWorld(ctx, registry, w.ID)
		out = append(out, detail)
	}
	return out, nil
}

// listRegionsForWorld best-effort 汇总某世界三档（hot/warm/cold）的 region 概览，去重、按 region.ID 稳定。
func (service *Service) listRegionsForWorld(ctx context.Context, registry *region.Registry, worldID string) []RegionDetail {
	seen := map[string]bool{}
	var out []RegionDetail
	for _, tier := range []region.Tier{region.TierHot, region.TierWarm, region.TierCold} {
		regions, err := registry.ListByTier(ctx, worldID, tier, 0)
		if err != nil {
			continue // best-effort：某档查询失败跳过，不影响其余档
		}
		for _, r := range regions {
			if seen[r.ID] {
				continue
			}
			seen[r.ID] = true
			out = append(out, RegionDetail{
				ID:           r.ID,
				WorldID:      r.WorldID,
				ActivityTier: string(r.ActivityTier),
				ThreatLevel:  r.ThreatLevel,
				LastTick:     r.LastTick,
			})
		}
	}
	return out
}

// SetRegionThreatLevel 把某 region 的威胁等级**绝对置位**到 level（GM 人工拉高/清零某地威胁度做活动/演练）。
// region 不存在时先按 worldID 幂等登记（UpsertRegion，默认 cold/零威胁），再以「目标 − 当前」的 delta 累加到目标值。
// 用 delta 累加而非直写：复用 region.BumpThreatLevel 的并发安全 SQL（threat_level=threat_level+? 在 SQL 内完成、防读改写竞态）。
// 返回置位后的实际威胁值。level<0 视为 0（威胁度非负）。
func (service *Service) SetRegionThreatLevel(ctx context.Context, worldID, regionID string, level int64) (int64, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("set region threat: nil service or db")
	}
	regionID = strings.TrimSpace(regionID)
	if regionID == "" {
		return 0, fmt.Errorf("set region threat: empty region id")
	}
	if level < 0 {
		level = 0
	}
	registry := region.New(service.db)
	// 幂等登记：region 不存在时建档（已存在则只刷 world_id/updated_at，不抹已积累威胁）。
	if err := registry.UpsertRegion(ctx, regionID, strings.TrimSpace(worldID)); err != nil {
		return 0, fmt.Errorf("set region threat (upsert): %w", err)
	}
	current, err := registry.GetRegion(ctx, regionID)
	if err != nil {
		return 0, fmt.Errorf("set region threat (get current): %w", err)
	}
	delta := level - current.ThreatLevel
	if delta == 0 {
		return current.ThreatLevel, nil // 已是目标值，免一次写
	}
	newLevel, err := registry.BumpThreatLevel(ctx, regionID, delta)
	if err != nil {
		return 0, fmt.Errorf("set region threat (bump): %w", err)
	}
	return newLevel, nil
}

// SeedWorldVillage 为某局（sessionID）确定性织一张 20 人出生关系网并接入 worldID（worldID 可空=不入世界）。
// GM 手动播种世界人口的入口：复用既有 SeedVillage（建人/落库/入世界/织关系/写记忆全套）。
// factionID 缺省取 sessionID（单人局阵营 ID 常与 sessionID 同源，调用方可显式覆盖）。
// 返回实际落库的村民数。注意：同一 (worldID, seed) 重复调用人是确定性一致的，但会**新建行**——
// GM 应对同一局只播种一次，避免重复造人（这与建局自动织村的幂等守卫是两条独立路径）。
func (service *Service) SeedWorldVillage(ctx context.Context, sessionID, factionID, worldID string, seed int64) (int, error) {
	if service == nil || service.units == nil || service.db == nil {
		return 0, fmt.Errorf("seed world village: missing dependencies")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return 0, fmt.Errorf("seed world village: empty session id")
	}
	if strings.TrimSpace(factionID) == "" {
		factionID = sessionID
	}
	villagers, err := service.SeedVillage(ctx, sessionID, factionID, strings.TrimSpace(worldID), seed)
	if err != nil {
		return 0, fmt.Errorf("seed world village: %w", err)
	}
	return len(villagers), nil
}

// ===== featureflags.Store 实现（运行时 flag override 的双驱动持久化后端） =====

// FeatureFlagStore 把 featureflags 的 override 落 feature_flag_overrides 表（双驱动），使 GM 设过的开关重启存活。
// 实现 featureflags.Store 接口（在 featureflags 包内声明，本类型结构上满足，刻意不让 featureflags 依赖本包）。
type FeatureFlagStore struct {
	db      *sql.DB
	dialect dbdialect.Dialect
}

// NewFeatureFlagStore 构造持久化后端。dialect 由 db 推断（开新连接前应已 dbdialect.Register）。
func NewFeatureFlagStore(db *sql.DB) *FeatureFlagStore {
	return &FeatureFlagStore{db: db, dialect: dbdialect.For(db)}
}

// Upsert 幂等写一条 override（按 name 主键 upsert，双驱动 ON CONFLICT / ON DUPLICATE KEY）。
func (s *FeatureFlagStore) Upsert(ctx context.Context, name, value string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("feature flag store upsert: nil store")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("feature flag store upsert: empty name")
	}
	query := `
		INSERT INTO feature_flag_overrides (name, value, updated_by, updated_at)
		VALUES (?, ?, 'gm', CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`
	if s.dialect == dbdialect.DialectMySQL {
		query = `
			INSERT INTO feature_flag_overrides (name, value, updated_by, updated_at)
			VALUES (?, ?, 'gm', UTC_TIMESTAMP())
			ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = UTC_TIMESTAMP()`
	}
	if _, err := s.db.ExecContext(ctx, query, name, value); err != nil {
		return fmt.Errorf("feature flag store upsert: %w", err)
	}
	return nil
}

// Delete 删一条 override 的持久记录（ClearOverride 时同步）。
func (s *FeatureFlagStore) Delete(ctx context.Context, name string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("feature flag store delete: nil store")
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM feature_flag_overrides WHERE name = ?`, strings.TrimSpace(name)); err != nil {
		return fmt.Errorf("feature flag store delete: %w", err)
	}
	return nil
}

// Load 读全部已持久 override（启动期回灌）。
func (s *FeatureFlagStore) Load(ctx context.Context) (map[string]string, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("feature flag store load: nil store")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name, value FROM feature_flag_overrides`)
	if err != nil {
		return nil, fmt.Errorf("feature flag store load: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, fmt.Errorf("feature flag store load (scan): %w", err)
		}
		out[name] = value
	}
	return out, rows.Err()
}

// 编译期断言：FeatureFlagStore 满足 featureflags.Store 接口。
var _ featureflags.Store = (*FeatureFlagStore)(nil)
