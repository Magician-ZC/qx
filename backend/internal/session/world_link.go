package session

// 文件说明：把一局会话接入某个世界，并在实战交互发生时自动写进世界总线（设计 耦合 §1 / 大世界 §8）。
// 现阶段当前对局是单人战棋（敌方是 NPC），自动写总线只在 state.WorldID 非空（接入多世界）时生效——
// 这是为「世界里有别的玩家角色」预备的管线：一旦多世界落地，击杀/救援/馈赠即自动成为跨玩家事实源。

import (
	"context"
	"errors"
	"hash/fnv"
	"log"
	"strings"

	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

// defaultWorldID 是共享主世界的稳定常量 ID（get-or-create 的固定锚）。
// 取固定字面量而非 uuid：唯一性约束 + 固定 ID 让「先 Get 再 Create」天然幂等，
// 并发下两个建局同时撞 Create 会触发主键冲突，由 EnsureDefaultWorld 兜底再 Get 一次。
const defaultWorldID = "world_default"

// 共享世界几何（Phase 1）常量：与旧私有副本（world_default）**物理隔离**的新世代世界。
//   - sharedWorldID：新世代固定 ID。旧 world_default 已降生角色零影响（它们仍走各自独立种子的私有世界）。
//   - sharedWorldGenesisSeed：RegionSeed 缺省时写入的确定性固定字符串根。共享世界的「同一张图」由它派生——
//     所有开 flag 降生的玩家用 deriveSharedSeed(RegionSeed) 得到同一个 int64 种子，GenerateWorld 逐格相同。
const (
	sharedWorldID          = "world_shared_v1"
	sharedWorldGenesisSeed = "shared-world-genesis"
)

// isSharedWorldID 判定某 worldID 是否共享世界世代（world_shared_v1）。
// Phase 2 的复合 region_id / 玩家相遇分叉**只对共享世界世代生效**——私有 world_default / per_session 世界一律走旧路径，零影响。
func isSharedWorldID(worldID string) bool {
	return strings.TrimSpace(worldID) == sharedWorldID
}

// inSharedWorld 判定本局是否「开 flag 的共享世界局」：flag 开 + state.WorldID==共享世代。
// 这是 Phase 2 所有跨玩家分叉（scope 复合 region_id、buildSnapshot 拉同区别玩家）的统一守门——
// 任一不满足都回退旧行为（私有档 region_id 维持 sessionID 口径、快照只含本 session 单位），保证 flag 关/私有档零影响。
func inSharedWorld(state *State) bool {
	return state != nil && sharedWorldEnabled() && isSharedWorldID(state.WorldID)
}

// sharedWorldEnabled 读 QUNXIANG_SHARED_WORLD，**默认开**（显式 false/0/no/off 可关 → 走旧私有副本路径）。
// 开时降生分叉到共享世界几何（同 RegionSeed 派生同种子、逐格相同的世界）；显式关是回退到旧 world_default 私有档的应急通道。
func sharedWorldEnabled() bool {
	return featureflags.EnabledWithDefault("QUNXIANG_SHARED_WORLD", true)
}

// deriveSharedSeed 把共享世界的 RegionSeed（字符串）确定性派生为 GenerateWorld 所需的 int64 种子。
// 用 FNV-64a（与全仓「确定性随机不依赖全局 rand」铁律同源）：同一 RegionSeed 永远派生同一 int64，
// 故所有开 flag 降生的玩家跑 world.GenerateWorld(同种子) 得到**逐格相同**的世界，可离线复算验证。
func deriveSharedSeed(regionSeed string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(regionSeed))
	return int64(h.Sum64())
}

// sharedRegionID 把（共享世界 worldID, zoneID）组成**复合 region_id**：`worldID#zoneID`（Phase 2「玩家相遇」）。
//
// 为什么是复合而非裸 zoneID：units 表的 region_id 列在分片关时默认 ==sessionID（ambient_scheduling.go:79），
// 大量代码按 region_id 写得「貌似 region-scoped 实则 session-scoped」。Phase 2 给共享世界角色另起一套**专属命名空间**——
//   - 加 worldID 前缀：确保跨世代世界（world_default / world_shared_v1 / 未来 v2）同名 zone（如都叫 zone_neutral_start）
//     绝不混进同一 region，跨世界角色永不互相浮现；
//   - 加 `#` 分隔符：zoneID 形如 `zone_neutral_start`、worldID 形如 `world_shared_v1`，都不含 `#`，故复合键无歧义、可反解。
//
// **只给共享世界角色用**（flag 开 + world_id==共享世代 + zoneID 非空）；私有档 region_id 维持 sessionID 口径、零影响。
// 这是 §5 风险 2「region_id 语义二义性」的规避：不全局改 region_id 语义，只在共享世界新命名空间里赋予其「真·地理子区」含义。
func sharedRegionID(worldID, zoneID string) string {
	worldID = strings.TrimSpace(worldID)
	zoneID = strings.TrimSpace(zoneID)
	if worldID == "" || zoneID == "" {
		return ""
	}
	return worldID + "#" + zoneID
}

// worldBindingMode 读 QUNXIANG_WORLD_BINDING，归一为三档绑定策略（大小写不敏感、去空白）：
//   - shared（默认，未设/非法/缺省皆归此）：所有 session 接入同一个共享主世界 world_default，
//     是「跨玩家关联」（设计问题 B）的前置——不同 session 的角色须共享世界才能相互浮现/撮合/血仇传播。
//   - per_session：每局接入各自专属世界 world_sess_<sessionID>，跨玩家整层在域内但天然不与他局相交（隔离演练用）。
//   - off：不接入任何世界（WorldID 留空），等价旧行为——worldbus/cross-event/七交互/world-boss/auto_match 全早返回。
//
// 默认 shared 安全：relevance 阈值（0.35）+ 锚机制保证无跨会话关系时不会乱浮现，
// 共享世界只是把跨玩家管线「通电」，而非强制每局都互相干预。
func worldBindingMode() string {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_WORLD_BINDING"))) {
	case "per_session":
		return "per_session"
	case "off":
		return "off"
	default:
		return "shared"
	}
}

// EnsureDefaultWorld get-or-create 共享主世界 world_default 并返回其 ID（确定性、幂等）。
// 先 world.Get(defaultWorldID)：命中即返回；ErrNotFound 才 world.Create 固定 ID 的主世界。
// 并发兜底：两个建局同时 Create 会有一方撞唯一键报错，此时再 Get 一次拿到已存在的世界——
// 因此无论谁赢得创建竞争，所有调用方最终都收敛到同一个 world_default。
func (service *Service) EnsureDefaultWorld(ctx context.Context) (string, error) {
	if service == nil || service.db == nil {
		return "", errors.New("ensure default world: nil service or db")
	}
	if _, err := world.Get(ctx, service.db, defaultWorldID); err == nil {
		return defaultWorldID, nil
	} else if !errors.Is(err, world.ErrNotFound) {
		return "", err
	}
	if _, err := world.Create(ctx, service.db, world.World{
		ID:            defaultWorldID,
		Name:          "主世界",
		Status:        world.StatusActive,
		MaxPopulation: 100000,
	}); err != nil {
		// 并发竞争：另一建局先 Create 成功导致主键冲突 → 再 Get 一次兜底；仍失败才真报错。
		if _, getErr := world.Get(ctx, service.db, defaultWorldID); getErr == nil {
			return defaultWorldID, nil
		}
		return "", err
	}
	return defaultWorldID, nil
}

// EnsureSharedWorld get-or-create 共享世界几何世代 world_shared_v1，返回 (worldID, regionSeed, err)。确定性、幂等。
//
// 与 EnsureDefaultWorld 同模式（先 Get 命中即返回、ErrNotFound 才 Create 固定 ID、并发撞唯一键再 Get 兜底），
// 额外保证 **RegionSeed 非空**：这是共享世界「同一张图」的根——
//   - 新建时直接以 sharedWorldGenesisSeed 落库（Create 带 RegionSeed）。
//   - 已存在但历史行 RegionSeed 为空（如被别的代码路径先建）→ 补一个确定性固定值并持久化（UpdateRegionSeed）。
//
// 返回的 regionSeed 供降生路径 deriveSharedSeed → GenerateWorld。worldID 恒为 sharedWorldID。
// 与旧 world_default 物理隔离：旧私有档零影响（它们的 world_id 仍是 world_default，不在本世代）。
func (service *Service) EnsureSharedWorld(ctx context.Context) (string, string, error) {
	if service == nil || service.db == nil {
		return "", "", errors.New("ensure shared world: nil service or db")
	}
	if w, err := world.Get(ctx, service.db, sharedWorldID); err == nil {
		seed := strings.TrimSpace(w.RegionSeed)
		if seed == "" {
			// 历史行缺种子：补确定性固定值，保证后续降生玩家拿到逐格相同的世界。best-effort——
			// 补种失败仍回退用 genesis 常量（GenerateWorld 不依赖 DB 行的 seed 列、只依赖传入字符串），不阻断。
			seed = sharedWorldGenesisSeed
			if updErr := world.UpdateRegionSeed(ctx, service.db, sharedWorldID, seed); updErr != nil {
				log.Printf("ensure shared world: backfill region_seed best-effort failed (world=%s): %v", sharedWorldID, updErr)
			}
		}
		return sharedWorldID, seed, nil
	} else if !errors.Is(err, world.ErrNotFound) {
		return "", "", err
	}
	if _, err := world.Create(ctx, service.db, world.World{
		ID:            sharedWorldID,
		Name:          "共享世界",
		Status:        world.StatusActive,
		MaxPopulation: 100000,
		RegionSeed:    sharedWorldGenesisSeed,
	}); err != nil {
		// 并发竞争：另一降生先 Create 成功导致主键冲突 → 再 Get 一次兜底（与 EnsureDefaultWorld 同模式）。
		if w, getErr := world.Get(ctx, service.db, sharedWorldID); getErr == nil {
			seed := strings.TrimSpace(w.RegionSeed)
			if seed == "" {
				seed = sharedWorldGenesisSeed
			}
			return sharedWorldID, seed, nil
		}
		return "", "", err
	}
	return sharedWorldID, sharedWorldGenesisSeed, nil
}

// bindSessionWorld 按 worldBindingMode 把一局接入世界，best-effort：吞错只记日志、绝不阻断建局。
// 调用点须在玩家单位已落库之后（AssignSessionToWorld 要接入这些单位）、且在 ambient/村庄锚落库之前
// （使 state.WorldID 在那时已置位，跨玩家锚才会带正确世界域）。
//   - off：直接 return nil，WorldID 留空 = 旧行为。
//   - shared：worldID = EnsureDefaultWorld（共享主世界）。
//   - per_session：worldID = world_sess_<sessionID>，幂等 Create 一局专属世界。
func (service *Service) bindSessionWorld(ctx context.Context, state *State) error {
	if service == nil || service.db == nil || state == nil {
		return nil
	}
	var worldID string
	switch worldBindingMode() {
	case "off":
		return nil
	case "per_session":
		worldID = "world_sess_" + state.ID
		if _, err := world.Create(ctx, service.db, world.World{
			ID:            worldID,
			Name:          "对局世界 " + state.ID,
			Status:        world.StatusActive,
			MaxPopulation: 100000,
		}); err != nil {
			// 幂等：重连/重复建局撞已存在的同 ID 世界即视为已就绪，仍继续接入。
			if _, getErr := world.Get(ctx, service.db, worldID); getErr != nil {
				log.Printf("bind session world (per_session) best-effort failed (session=%s): %v", state.ID, err)
				return nil
			}
		}
	default: // shared
		id, err := service.EnsureDefaultWorld(ctx)
		if err != nil {
			log.Printf("bind session world (shared) best-effort failed (session=%s): %v", state.ID, err)
			return nil
		}
		worldID = id
	}
	if err := service.AssignSessionToWorld(ctx, state, worldID); err != nil {
		log.Printf("assign session to world best-effort failed (session=%s world=%s): %v", state.ID, worldID, err)
		return nil
	}
	return nil
}

// AssignSessionToWorld 把本局接入某世界：设 state.WorldID，并把本局所有单位接入该世界（幂等）。
func (service *Service) AssignSessionToWorld(ctx context.Context, state *State, worldID string) error {
	if service == nil || service.db == nil || state == nil {
		return nil
	}
	state.WorldID = worldID
	if worldID == "" {
		return nil
	}
	dialect := dbdialect.For(service.db)
	for _, id := range append(append([]string{}, state.PlayerUnitIDs...), state.EnemyUnitIDs...) {
		if id != "" {
			_ = world.Join(ctx, service.db, worldID, id, "inhabitant", dialect)
		}
	}
	// geo 锚（设计 §1.1「她所在的地方」）：把每个玩家单位当前所在 region 落成一根 geo 锚（半衰 3 天——
	// 离开后地理牵挂渐淡）。让「她所在的地方」成为相关性锚，使世界事件天然聚焦到她身上/脚下。
	// region 取单位去规范化的 region_id 列（service.regionOf），缺/查错回落 worldID 兜底（至少锚在世界域上）。
	// 全程 best-effort：UpsertGeoAnchor 内部失败不回滚、不阻断接入；仅玩家单位落锚（敌方 NPC 不进相关性锚池）。
	for _, id := range state.PlayerUnitIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		regionID := service.regionOf(ctx, id)
		if regionID == "" {
			regionID = worldID
		}
		_ = service.UpsertGeoAnchor(ctx, state.ID, id, regionID, geoAnchorDefaultWeight, "所在·"+regionID)
	}
	return nil
}

// recordWorldizedKill 在接入世界后，把一次击杀作为 CROSS_ATTACK 写进世界总线（best-effort，gate 在 WorldID）。
func (service *Service) recordWorldizedKill(ctx context.Context, state *State, attacker *unit.Record, victim *unit.Record) {
	if service == nil || state == nil || state.WorldID == "" || attacker == nil || victim == nil {
		return
	}
	_, _ = service.RecordCrossInteraction(ctx, state.WorldID, attacker.ID, victim.ID,
		worldbus.KindAttack, 8, map[string]any{"fatal": true})
}
