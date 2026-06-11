package session

// 文件说明：世界编年史的入史规则与读侧装配（分区大世界阶段4 §7）——把世界的纪元大事
// （区域 boss 讨平 / 任务解锁区域 / 传奇诞生陨落 / 阵营之战）按 §7.2 规则物化进 world_chronicle 表，
// 独立于单角色编年史（chronicle.go 是「她的人生」，本文件是「她所处时代的洪流」）。
//
// 分层：纯存储 + 纪元/容量逻辑在 internal/world/chronicle.go（确定性、可测）；本文件是 session 业务层——
//   - recordWorldChronicleBestEffort：统一写入口（算当前纪元 → 盖戳 → 写表 → 旁路流程事件 → 低频裁剪），全 best-effort。
//   - chronicleBossSlain / chronicleZoneUnlocked / chronicleHeroDied / chronicleHeroBorn：四个语义化入史 helper，
//     供实战触发点（zone_boss.go 讨平 / quest.go 解锁 / aging.go 老死 / romance 传奇诞生）调用。
//   - WorldChronicleFeed：读侧导出入口（router 唯一消费口），倒序读一页世界史。
//
// 入史经 EmitProcessEvent 旁路 + 写 world_chronicle，**绝不改保护状态字段、不走 status.Mutator**（§7.2 流程旁路）。
// 全链路 best-effort 吞错：世界史是叙事派生物，写不进去最多丢一笔记述，绝不中断主模拟循环。
// 决策用 LLM、结算用代码：入史判定/纪元推进/叙事模板全代码确定性；LLM 润色是可选增强（缺失走规则文案 fallback）。

import (
	"context"
	"fmt"
	"strings"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/world"
)

// 世界编年史入史类别（§7.2）。与 world.WorldChronicleEntry.Category 对齐，前端据此分类着色。
const (
	WorldChronicleBossSlain    = "boss_slain"    // 区域/世界 boss 被讨平
	WorldChronicleZoneUnlocked = "zone_unlocked" // 任务交付解锁了新区域
	WorldChronicleHeroBorn     = "hero_born"     // 传奇角色诞生（高 importance）
	WorldChronicleHeroDied     = "hero_died"     // 传奇角色陨落（含 mortal 高龄逝去 + 血脉传承联动）
	WorldChronicleFactionWar   = "faction_war"   // 阵营冲突/城镇易主
	WorldChronicleGenesis      = "genesis"       // 创世序章（每世界一条、卷首楔子，模块1）
	WorldChronicleBossArisen   = "boss_arisen"   // 凶煞自虚无凝形的「降世」前兆（历史→boss 反向链路，模块2 引用）
)

// 纪元名称表（§7.2 纪元更替）：按累计重大事件段数命名，叙事用、循环复用末名以防越界。
// 第 0 段=开拓纪元；每累计 worldEraEventThreshold 个重大事件进下一段。
var worldEraNames = []string{
	"开拓纪元",
	"群雄纪元",
	"烽烟纪元",
	"问鼎纪元",
	"残阳纪元",
	"新生纪元",
}

// currentWorldEra best-effort 算某世界当前所处纪元名（§7.2）：按已落库的重大事件总数分段。
// 读 DB 失败/未接多世界 → 回落首纪元（开拓纪元），绝不阻断写入。纯派生、确定性（同样本同结果）。
func (service *Service) currentWorldEra(ctx context.Context, worldID string) string {
	if service == nil || service.db == nil || strings.TrimSpace(worldID) == "" {
		return worldEraNames[0]
	}
	// 统计本世界所有纪元段累计的重大事件（era 维度在写入时盖戳，故这里按 world_id 全量数重大事件总数推段）。
	var major int
	if err := service.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM world_chronicle WHERE world_id = ? AND importance >= ?`,
		worldID, worldEraMajorImportanceForSession(),
	).Scan(&major); err != nil {
		return worldEraNames[0]
	}
	segment := major / worldEraEventThresholdForSession()
	if segment < 0 {
		segment = 0
	}
	if segment >= len(worldEraNames) {
		segment = len(worldEraNames) - 1
	}
	return worldEraNames[segment]
}

// worldEraEventThresholdForSession / worldEraMajorImportanceForSession 暴露 world 包内部纪元常量给本层
// （world 包常量未导出；这里硬编同值，单测 TestEraThresholdParity 守恒一致，改一处即报）。
func worldEraEventThresholdForSession() int  { return 12 }
func worldEraMajorImportanceForSession() int { return 7 }

// recordWorldChronicleBestEffort 是世界史统一写入口：算当前纪元 → 盖戳 → 写 world_chronicle → 旁路 WORLD_CHRONICLE_RECORD
// 流程事件（「回到那一刻」世界级锚点）→ 低频容量裁剪。worldID 为空（旧单图档/未接多世界）直接 no-op（世界史必挂世界）。
// 全 best-effort：任何失败都吞掉、不返错、不中断主循环。返回写入条目 id（no-op/失败为空串）。
func (service *Service) recordWorldChronicleBestEffort(ctx context.Context, worldID string, worldTick int, category, titleZH, narrativeZH string, importance int, actorRefs []string) string {
	if service == nil || service.db == nil {
		return ""
	}
	worldID = strings.TrimSpace(worldID)
	if worldID == "" {
		return "" // 旧单图档无世界归属：不写世界史（与公共 NPC 入世界同口径，gate 在 WorldID）
	}
	era := service.currentWorldEra(ctx, worldID)
	id, err := world.RecordWorldChronicle(ctx, service.db, dbdialect.IsMySQL(service.db), world.WorldChronicleEntry{
		WorldID:     worldID,
		WorldTick:   worldTick,
		Era:         era,
		Category:    category,
		TitleZH:     titleZH,
		NarrativeZH: narrativeZH,
		ActorRefs:   actorRefs,
		Importance:  importance,
	})
	if err != nil {
		return "" // 吞错：写表失败不影响主循环
	}
	// 旁路一条流程事件作「回到那一刻」世界级锚点（不改保护字段、不走 Mutator）。失败无所谓：条目已落库。
	// 刻意**不**把 actor_refs 填进 events.actor_unit_id/target_unit_id——events 那两列有指向 units(id) 的外键，
	// 而世界史 actor 可能是跨分片/已离线/已逝角色（world_chronicle 刻意无 units 外键，§7.1）。actor 已存于条目 actor_refs，
	// 这里只落「世界 + tick + 条目 id」锚点（owner/target 留空，避免外键约束误拒）。
	_, _ = events.EmitProcessEvent(ctx, service.db, events.ProcessEvent{
		Code:     events.ReasonWorldChronicleRecord,
		Category: events.CategoryLifecycle,
		WorldID:  worldID,
		Tick:     worldTick,
		Payload: map[string]any{
			"world_chronicle_id": id,
			"category":           category,
			"era":                era,
			"world_tick":         worldTick,
			"actor_refs":         actorRefs,
		},
	})
	// 低频容量裁剪（防无限膨胀，§11 风险）：超 capacity 才删，best-effort 吞错。
	_, _ = world.TrimWorldChronicle(ctx, service.db, worldID)
	return id
}

// ── 语义化入史 helper（§7.2 入史规则，供实战触发点调用，全 best-effort）──

// chronicleBossSlain 记一笔「区域 boss 讨平」入世界史（zone_boss.go ChallengeZoneBoss 胜利时调）。
// slayerID 是讨平者（=主角）；slayerName/bossName/zoneName 入叙事；worldTick=部署回合（state.TurnState.Turn）。
func (service *Service) chronicleBossSlain(ctx context.Context, worldID string, worldTick int, slayerID, slayerName, bossName, zoneName string) string {
	if strings.TrimSpace(bossName) == "" {
		bossName = "盘踞此地的霸主"
	}
	where := ""
	if strings.TrimSpace(zoneName) != "" {
		where = "在「" + zoneName + "」"
	}
	title := fmt.Sprintf("%s讨平", bossName)
	narrative := fmt.Sprintf("史载：%s%s讨平了「%s」，一方凶煞自此绝迹，往来之人得以稍安。", strings.TrimSpace(slayerName), where, bossName)
	return service.recordWorldChronicleBestEffort(ctx, worldID, worldTick, WorldChronicleBossSlain, title, narrative, 7, []string{slayerID})
}

// chronicleZoneUnlocked 记一笔「区域解锁」入世界史（quest.go TurnInQuest 的 UnlockZone 非空时调）。
func (service *Service) chronicleZoneUnlocked(ctx context.Context, worldID string, worldTick int, heroID, heroName, zoneID, zoneName string) string {
	name := strings.TrimSpace(zoneName)
	if name == "" {
		name = strings.TrimSpace(zoneID)
	}
	if name == "" {
		name = "一方新天地"
	}
	title := fmt.Sprintf("%s通途既开", name)
	narrative := fmt.Sprintf("史载：%s了结了一桩大事，通往「%s」的道路自此豁然贯通，世界的版图又向外延展了一隅。", strings.TrimSpace(heroName), name)
	return service.recordWorldChronicleBestEffort(ctx, worldID, worldTick, WorldChronicleZoneUnlocked, title, narrative, 6, []string{heroID})
}

// chronicleHeroDied 记一笔「角色陨落」入世界史（aging.go 高龄老死 / 其它传奇之死时调）。
// causeZH 是死因短语（如「寿终正寝」「战死沙场」），空则用「走到了生命尽头」。
func (service *Service) chronicleHeroDied(ctx context.Context, worldID string, worldTick int, deceasedID, deceasedName, causeZH string) string {
	name := strings.TrimSpace(deceasedName)
	if name == "" {
		name = "一位无名之人"
	}
	cause := strings.TrimSpace(causeZH)
	if cause == "" {
		cause = "走到了生命尽头"
	}
	title := fmt.Sprintf("%s陨落", name)
	narrative := fmt.Sprintf("史载：%s%s，一缕命途就此归于尘土，其名或随风传颂，或悄然湮没于岁月。", name, cause)
	return service.recordWorldChronicleBestEffort(ctx, worldID, worldTick, WorldChronicleHeroDied, title, narrative, 5, []string{deceasedID})
}

// chronicleHeroBorn 记一笔「传奇诞生」入世界史（romance/newborn 路径在高 importance 诞生时可调，本阶段提供 helper）。
func (service *Service) chronicleHeroBorn(ctx context.Context, worldID string, worldTick int, newbornID, newbornName string) string {
	name := strings.TrimSpace(newbornName)
	if name == "" {
		name = "一个新生命"
	}
	title := fmt.Sprintf("%s降生", name)
	narrative := fmt.Sprintf("史载：%s降生于世，新的命途自此起笔，世界的新陈代谢又添一笔。", name)
	return service.recordWorldChronicleBestEffort(ctx, worldID, worldTick, WorldChronicleHeroBorn, title, narrative, 5, []string{newbornID})
}

// ── 读侧导出（router / 前端世界编年史面板唯一消费口）──

// WorldChronicleFeed 是装配好的一页世界编年史（倒序：纪元/tick 由近及远）。
type WorldChronicleFeed struct {
	WorldID string                      `json:"world_id"`
	Entries []world.WorldChronicleEntry `json:"entries"`
	Limit   int                         `json:"limit"`
}

// WorldChronicleFeedForSession 按 sessionID 解出 worldID 后读回该世界的编年史（router 端点用）。
// 会话归属由调用方鉴权后再调；本方法只载 state 取 WorldID。worldID 空（旧单图档）→ 返回空 Feed（前端提示「这方世界尚未与大世界相连」）。
func (service *Service) WorldChronicleFeedForSession(ctx context.Context, sessionID string, limit int) (WorldChronicleFeed, error) {
	feed := WorldChronicleFeed{Entries: make([]world.WorldChronicleEntry, 0)}
	if service == nil || service.db == nil || service.sessions == nil {
		return feed, fmt.Errorf("world chronicle feed: service unavailable")
	}
	state, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		return feed, fmt.Errorf("world chronicle feed (load session): %w", err)
	}
	worldID := strings.TrimSpace(state.WorldID)
	feed.WorldID = worldID
	if limit <= 0 || limit > world.MaxWorldChronicleList {
		limit = world.MaxWorldChronicleList
	}
	feed.Limit = limit
	if worldID == "" {
		return feed, nil // 旧单图档无世界：空 Feed，非错误
	}
	entries, err := world.ListWorldChronicle(ctx, service.db, worldID, limit)
	if err != nil {
		return feed, fmt.Errorf("world chronicle feed (list): %w", err)
	}
	feed.Entries = entries
	return feed, nil
}

// WorldChronicleFeedByWorldID 直接按 worldID 读世界编年史（worlds/:worldId/chronicle 端点用，不经 session）。
func (service *Service) WorldChronicleFeedByWorldID(ctx context.Context, worldID string, limit int) (WorldChronicleFeed, error) {
	feed := WorldChronicleFeed{WorldID: strings.TrimSpace(worldID), Entries: make([]world.WorldChronicleEntry, 0)}
	if service == nil || service.db == nil {
		return feed, fmt.Errorf("world chronicle feed: service unavailable")
	}
	if feed.WorldID == "" {
		return feed, fmt.Errorf("world chronicle feed: empty world id")
	}
	if limit <= 0 || limit > world.MaxWorldChronicleList {
		limit = world.MaxWorldChronicleList
	}
	feed.Limit = limit
	entries, err := world.ListWorldChronicle(ctx, service.db, feed.WorldID, limit)
	if err != nil {
		return feed, fmt.Errorf("world chronicle feed (list): %w", err)
	}
	feed.Entries = entries
	return feed, nil
}
