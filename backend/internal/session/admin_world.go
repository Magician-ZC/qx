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
// region 不存在时先按 worldID 幂等登记（UpsertRegion，默认 cold/零威胁），再原子绝对置位到目标值。
// 用单条原子 UPDATE 直写（region.SetThreatLevelAbsolute）而非「读当前值 + Bump(目标−当前)」：
// 后者在 Get 与 Bump 两步之间存在 TOCTOU 竞态，并发两次绝对置位会互相串扰成错误终值，破坏「绝对置位」语义。
// 返回置位后的实际威胁值。level<0 视为 0（威胁度非负，由 SetThreatLevelAbsolute 内部夹钳）。
func (service *Service) SetRegionThreatLevel(ctx context.Context, worldID, regionID string, level int64) (int64, error) {
	if service == nil || service.db == nil {
		return 0, fmt.Errorf("set region threat: nil service or db")
	}
	regionID = strings.TrimSpace(regionID)
	if regionID == "" {
		return 0, fmt.Errorf("set region threat: empty region id")
	}
	registry := region.New(service.db)
	// 幂等登记：region 不存在时建档（已存在则只刷 world_id/updated_at，不抹已积累威胁）。
	if err := registry.UpsertRegion(ctx, regionID, strings.TrimSpace(worldID)); err != nil {
		return 0, fmt.Errorf("set region threat (upsert): %w", err)
	}
	// 原子绝对置位：单条 UPDATE 直写目标值（内部夹 MAX(0,level)），无 read-then-delta 的 TOCTOU 窗口。
	newLevel, err := registry.SetThreatLevelAbsolute(ctx, regionID, level)
	if err != nil {
		return 0, fmt.Errorf("set region threat (set absolute): %w", err)
	}
	return newLevel, nil
}

// SeedWorldVillage 为某局（sessionID）确定性织一张 20 人出生关系网并接入 worldID（worldID 可空=不入世界）。
// GM 手动播种世界人口的入口：复用既有 SeedVillage（建人/落库/入世界/织关系/写记忆全套）。
// factionID 缺省取 sessionID（单人局阵营 ID 常与 sessionID 同源，调用方可显式覆盖）。
// 返回实际落库的村民数。
//
// 幂等守卫：起始处复用 service.sessionAlreadyHasVillage（与建局自动织村 seedVillageForSession 同一守卫），
// 若本局已织过村则直接返回既有村民数、**不重复造人**——杜绝 GM 对同一局重复 POST 时每次凭空新建 20 单位的翻倍 bug。
// admin 场景无玩家主角，村庄仅播种村民、不含主角（与 onboarding 的 with_village 同口径，village 本就不含主角）。
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
	// 幂等：本局已织过村则返回既有村民数、不新建（防 GM 同局重复 POST 翻倍造人）。
	// 守卫与既有村民计数共用一次 ListBySession 内存比对，确定性、零额外 LLM。
	if existing, ok := service.countSeededVillagers(ctx, sessionID); ok && existing > 0 {
		return existing, nil
	}
	villagers, err := service.SeedVillage(ctx, sessionID, factionID, strings.TrimSpace(worldID), seed)
	if err != nil {
		return 0, fmt.Errorf("seed world village: %w", err)
	}
	return len(villagers), nil
}

// countSeededVillagers 数本局已落库的「村民」单位数（isSeededVillagerRecord 指纹命中）。
// ok=false 表示读 DB 失败：保守按「未织村」处理（与 sessionAlreadyHasVillage 同口径，宁可重试播种也不永久跳过）。
// 既作 SeedWorldVillage 的幂等守卫、又给已织村的局返回既有村民数（免再起一次 SeedVillage）。
func (service *Service) countSeededVillagers(ctx context.Context, sessionID string) (int, bool) {
	if service == nil || service.units == nil {
		return 0, false
	}
	records, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		return 0, false
	}
	count := 0
	for i := range records {
		if isSeededVillagerRecord(&records[i]) {
			count++
		}
	}
	return count, true
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
