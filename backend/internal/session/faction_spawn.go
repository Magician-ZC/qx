package session

// 文件说明：阵营开放世界 F1——在出生据点播种「公共同阵营 NPC」。
//
// 把旧「20 人私人村庄网」（村民与玩家一开局就有 relations 行）改为「三阵营开放世界」的出生点 NPC：
//   - NPC 带该阵营道德基准（faction.Baseline + 确定性小扰动）、阵营主题化的名字/出身。
//   - **公共而非私人**：绝不建玩家↔NPC 的 relations 行——关系靠游历相遇后天结成（区别于 SeedVillage 的出生即结仇/结亲）。
//   - 落进出生据点 region（worldID 在场时 join 入世界 + 标记 region 作用域），供命运层/游历相遇时被发现。
//
// 与 village.go 的关系：参照其确定性生成与落库底座（BootstrapRecord + units.Save + world.Join），
// 但**不**织 Bonds、**不**写 applyRelationShift、**不**建锚——只放一批有名有姓、带阵营底色的公共角色在世界里。
// 幂等：同 session 同据点已播种则不重复（靠 NPC 的阵营指纹 Lineage 前缀识别）。

import (
	"context"
	"fmt"
	"hash/fnv"
	"log"
	"strings"

	"qunxiang/backend/internal/faction"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/runtimeconfig"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

// factionSpawnAnchor 是公共 NPC 散布的锚点（=主角降生坐标，见 mainworld.go bootstrapBattleUnit Coord{1,3}）。
// NPC 围绕该锚点在地图内确定性铺开，让「身边一上来就有同阵营的人」在 hex 地图上看得见。
var factionSpawnAnchor = world.Coord{Q: 1, R: 3}

// factionSpawnMaxScatterRadius 是 NPC 相对锚点的最大铺开半径（hex 距离），控制 NPC 不会散得太远（留在主角视野/出生据点附近）。
const factionSpawnMaxScatterRadius = 4

// factionSpawnPlacementAttempts 是单个 NPC 找空位的最大尝试次数（确定性递增哈希探测）；用尽仍冲突则跳过落点
// （NPC 仍落库，只是坐标留默认——避免叠在主角/彼此身上而不是硬塞一个非法格）。
const factionSpawnPlacementAttempts = 64

// scatterFactionNPCCoord 为第 idx 个公共 NPC 在地图 snapshot 内、围绕 anchor 确定性挑一个合法空格：
//   - 合法：在地图边界内、非锚点格、未被 occupied 集合占用（避叠放，含主角与已落点的同阵营 NPC）。
//   - 确定性：据 (sessionID, factionID, regionID, seed, idx) 的 FNV 哈希派生候选半径/方向，探测多次直到找到空格。
//   - 找不到（地图太小/太挤）：返回 ok=false，调用方据此保留默认坐标、不强塞非法格。
//
// occupied 是已占用坐标的字符串集合（coordString），找到落点后调用方应把新坐标并入，保证后续 NPC 互不叠放。
func scatterFactionNPCCoord(
	snapshot world.MapSnapshot,
	anchor world.Coord,
	occupied map[string]struct{},
	hashFor func(sub string) uint64,
) (world.Coord, bool) {
	for attempt := 0; attempt < factionSpawnPlacementAttempts; attempt++ {
		prefix := fmt.Sprintf("pos%d", attempt)
		// 半径 1..maxRadius（避开 anchor 自身），方向取六邻向之一并按半径步进展开。
		radius := 1 + int(hashFor(prefix+":rad")%uint64(factionSpawnMaxScatterRadius))
		dirs := axialNeighbors(world.Coord{Q: 0, R: 0})
		dir := dirs[hashFor(prefix+":dir")%uint64(len(dirs))]
		// 在选定方向上走 radius 步，再叠一个小的横向抖动（让落点不全挤在六条射线上）。
		cand := world.Coord{Q: anchor.Q + dir.Q*radius, R: anchor.R + dir.R*radius}
		jitter := axialNeighbors(world.Coord{Q: 0, R: 0})[hashFor(prefix+":jit")%6]
		if hashFor(prefix+":jon")%2 == 0 {
			cand.Q += jitter.Q
			cand.R += jitter.R
		}
		if !inBounds(snapshot, cand) {
			continue
		}
		if cand == anchor {
			continue
		}
		if _, taken := occupied[coordString(cand)]; taken {
			continue
		}
		return cand, true
	}
	return world.Coord{}, false
}

// factionNPCLineagePrefix 是公共阵营 NPC 的出身原型前缀（阵营指纹）：SeedFactionSpawn 把每个 NPC 的
// Identity.Lineage 置为「<前缀><阵营主题原型>」（如「同阵·律所书记」），使幂等守卫可据此识别已播种的阵营 NPC，
// 且与村民/玩家主角的 Lineage 指纹不冲突。
const factionNPCLineagePrefix = "同阵·"

// factionSpawnMinNPC / factionSpawnMaxNPC 是单据点公共 NPC 的人数区间（8–12，据 seed 确定性取数）。
const (
	factionSpawnMinNPC = 8
	factionSpawnMaxNPC = 12
)

// factionSpawnBirthTurn 是公共 NPC 落库的回合标签（与村庄出生同口径，挂第 1 回合）。
const factionSpawnBirthTurn = 1

// 公共 NPC 道德轴相对阵营基准的最大扰动幅度（默认 ±6.0，让同阵营 NPC 道德轴≈baseline 而非全同）现由
// runtimeconfig "faction.moral_jitter" 提供（默认值在 catalog 注册）；faction_spawn.go 建 NPC 与
// faction_conflict.go 生成冲突对手两处读取站点共用此名。

// factionSurnames 是跨阵营共用的姓氏池（中文，确定性挑选）。
var factionSurnames = []string{"江", "墨", "苏", "燕", "卓", "凌", "云", "纪", "商", "厉", "宁", "霍", "薛", "尉", "钟", "唐"}

// factionGivenF / factionGivenM 是性别区分的名字池。
var factionGivenF = []string{"昭", "鸢", "拂雪", "岚生", "未央", "听澜", "疏影", "南音", "倦", "知微"}
var factionGivenM = []string{"决", "孤舟", "明诚", "执戈", "无锋", "守正", "破阵", "怀沙", "长缨", "拙言"}

// factionArchetypes 把三阵营各自的出身原型主题化（名字/出身呼应该阵营道德底色）。
var factionArchetypes = map[string][]string{
	faction.IDFreedom: {"浪迹游侠", "断弦琴师", "草原牧人", "逃籍野客", "孤鹰猎手", "卖唱说书人"},
	faction.IDOrder:   {"律所书记", "巡夜执法", "铁律教谕", "府衙小吏", "城防卫长", "刑名讼师"},
	faction.IDChaos:   {"废墟拾荒", "黑市掮客", "断桥赌徒", "纵火乱党", "走私船工", "蛊惑巫祝"},
}

// factionLifeThemes 把三阵营各自的人生底色（写进 NPC 传记，定调其行事动机）。
var factionLifeThemes = map[string][]string{
	faction.IDFreedom: {"只想随心所欲走遍天下，谁也别想拴住她。", "受够了别人替她做主，宁可流落也要自己说了算。", "把自由看得比命还重，最恨有人对她指手画脚。"},
	faction.IDOrder:   {"信奉律法能护住弱者，愿一生守着这份规矩。", "见过没有规矩的世道有多乱，立志以法度安人心。", "把秩序当成信仰，容不得半点逾矩。"},
	faction.IDChaos:   {"觉得旧秩序早该砸烂，乱中才有她的活路。", "在废墟里讨生活，信奉强者自取、弱者自弃。", "视一切规矩为枷锁，只认眼前的痛快。"},
}

// FactionSpawnResult 是一次出生据点播种的结果摘要。
type FactionSpawnResult struct {
	FactionID string   // 阵营 ID（freedom/order/chaos）
	RegionID  string   // 出生据点 region
	UnitIDs   []string // 落库的公共 NPC 单位 ID
}

// pickFromPool 据哈希在字符串池里确定性选一个。
func pickFromPool(pool []string, h uint64) string {
	if len(pool) == 0 {
		return ""
	}
	return pool[h%uint64(len(pool))]
}

// fnvSpawn 用项目约定的 FNV-64a 哈希据 (sessionID, factionID, regionID, seed, tag) 取确定性随机数。
func fnvSpawn(sessionID, factionID, regionID string, seed int64, tag string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fmt.Sprintf("factionspawn|%s|%s|%s|%d|%s", sessionID, factionID, regionID, seed, tag)))
	return h.Sum64()
}

// SeedFactionSpawn 在出生据点 regionID 为阵营 factionID 播种 8–12 个公共同阵营 NPC（参照 SeedVillage 的确定性生成）。
//   - 每个 NPC：阵营主题化名字/出身、Faction=factionID、MoralAlignment≈baseline+小扰动、落第 1 回合传记。
//   - **坐标上图**：围绕主角降生锚点 factionSpawnAnchor 在地图内确定性散布（避越界/避叠放），写 Status.PositionQ/R，
//     使「身边一上来就有同阵营的人」在 hex 地图上看得见（NPC 进 AmbientUnits 快照，**绝不进执行 order**，零 LLM）。
//   - **公共非私人**：绝不建玩家↔NPC 的 relations 行；也不织 NPC 间 Bonds、不建锚。关系靠后天游历相遇结成。
//   - worldID 在场时把 NPC join 入世界（inhabitant）并标记 region 作用域为出生据点（供调度/相遇定位）；worldID 空则只落本局。
//
// 幂等守卫：同 session 该据点已播种过阵营 NPC（按阵营指纹 Lineage 前缀识别）则直接返回空结果，不重复造人。
// 返回 FactionSpawnResult（含落库 NPC 的 UnitIDs，供调用方 append 进 state.AmbientUnitIDs）与可能的错误
// （中途出错也返回已落库的 UnitIDs，best-effort 调用方据此处理）。
func (service *Service) SeedFactionSpawn(ctx context.Context, sessionID string, factionID string, regionID string, seed int64, mapSnapshot world.MapSnapshot) (FactionSpawnResult, error) {
	result := FactionSpawnResult{FactionID: faction.Normalize(factionID), RegionID: strings.TrimSpace(regionID)}
	if service == nil || service.units == nil || service.db == nil {
		return result, fmt.Errorf("seed faction spawn: missing dependencies")
	}
	fid := faction.Normalize(factionID)
	if fid == "" {
		return result, fmt.Errorf("seed faction spawn: invalid faction %q", factionID)
	}
	regionID = strings.TrimSpace(regionID)
	if regionID == "" {
		return result, fmt.Errorf("seed faction spawn: empty region")
	}

	// 幂等守卫：本局该据点已有阵营 NPC 则不重复播种（确定性、零 LLM，仅一次 ListBySession + 内存比对）。
	if service.factionSpawnAlreadySeeded(ctx, sessionID, regionID) {
		return result, nil
	}

	def, _ := faction.Get(fid)
	worldID := service.worldIDForSession(ctx, sessionID)

	// 散布所需的地图快照：优先用调用方传入的内存 state.Map（降生入口此时 state 尚未落库，绕 DB 读会读到空）。
	// 传入为空（Width==0，如某些测试/admin 路径）时再 best-effort 从 DB 读兜底。读不到则 scatter 恒返回 false，
	// NPC 仍落库、只是坐标留默认——不阻断播种（map 缺失只丢「上图」不丢「造人」）。
	if mapSnapshot.Width == 0 {
		mapSnapshot = service.spawnMapSnapshot(ctx, sessionID)
	}
	occupied := map[string]struct{}{coordString(factionSpawnAnchor): {}} // 锚点（主角降生格）预占，避 NPC 叠到主角身上。

	// 人数：8–12，据 (session, faction, region, seed) 确定性取。
	span := uint64(factionSpawnMaxNPC - factionSpawnMinNPC + 1)
	count := factionSpawnMinNPC + int(fnvSpawn(sessionID, fid, regionID, seed, "count")%span)

	archetypes := factionArchetypes[fid]
	lifeThemes := factionLifeThemes[fid]

	for i := 0; i < count; i++ {
		tag := fmt.Sprintf("npc%d", i)
		h := func(sub string) uint64 { return fnvSpawn(sessionID, fid, regionID, seed, tag+":"+sub) }

		gender := "female"
		if h("g")%2 == 0 {
			gender = "male"
		}
		given := factionGivenF
		if gender == "male" {
			given = factionGivenM
		}
		name := pickFromPool(factionSurnames, h("sur")) + pickFromPool(given, h("giv"))
		archetype := pickFromPool(archetypes, h("arch"))
		theme := pickFromPool(lifeThemes, h("theme"))

		// 复用 BootstrapRecord 底座（确定性 seed 派生属性/人格），再覆写阵营字段。
		npcSeed := seed + int64(i)*2671
		rec := unit.BootstrapRecord(npcSeed, sessionID, factionID, name)
		rec.Identity.Gender = gender
		rec.Identity.Age = 18 + int(h("age")%40)
		// 阵营指纹 Lineage（幂等守卫据此识别，且与玩家/村民指纹不冲突）。
		rec.Identity.Lineage = factionNPCLineagePrefix + archetype
		rec.Identity.Biography = fmt.Sprintf("%s（%s阵营）。%s", archetype, def.NameZH, theme)
		// 阵营 + 道德轴（非保护字段，直接写——不走 Mutator）：道德轴≈baseline + 确定性小扰动。
		rec.Faction = fid
		rec.MoralAlignment = faction.PerturbBaseline(fid, npcSeed, tag, runtimeconfig.GetFloat("faction.moral_jitter"))
		// 阵营底色精化六维野心（出身/传记关键词命中即偏置）。
		rec.Ambition = unit.DeriveAmbition(npcSeed, rec.Identity.Lineage, rec.Identity.Biography)
		// 坐标上图：围绕锚点确定性散布到合法空格（PositionQ/R 非受保护字段，直接写——不走 Mutator）。
		if coord, ok := scatterFactionNPCCoord(mapSnapshot, factionSpawnAnchor, occupied, h); ok {
			rec.Status.PositionQ = coord.Q
			rec.Status.PositionR = coord.R
			occupied[coordString(coord)] = struct{}{}
		}

		if err := service.units.Save(ctx, rec); err != nil {
			return result, fmt.Errorf("save faction npc %d: %w", i, err)
		}
		result.UnitIDs = append(result.UnitIDs, rec.ID)

		// 入世界 + 标记出生据点作用域（best-effort：失败不打断播种；worldID 空则只落本局，不入世界）。
		if worldID != "" {
			_ = world.Join(ctx, service.db, worldID, rec.ID, "inhabitant", dbdialect.For(service.db))
			if err := service.units.SetUnitScope(ctx, rec.ID, worldID, regionID); err != nil {
				log.Printf("seed faction spawn: set scope npc=%s region=%s: %v", rec.ID, regionID, err)
			}
		}

		// 公共角色的「自我底色」沉淀为可检索/可衰减/可闪回的真实记忆（best-effort）。
		// 注意：**只写自我记忆，绝不写玩家↔NPC 关系**——公共非私人。
		service.rememberFactionNPCSelf(ctx, &rec, theme)
	}
	return result, nil
}

// spawnMapSnapshot best-effort 读取某 session 当前地图快照（供 NPC 坐标散布）。读不到返回零值 MapSnapshot
// （Width/Height=0），此时 scatter 恒判越界返回 false，NPC 坐标留默认——只丢「上图」、不丢「造人」。
func (service *Service) spawnMapSnapshot(ctx context.Context, sessionID string) world.MapSnapshot {
	if service == nil || service.sessions == nil {
		return world.MapSnapshot{}
	}
	state, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		return world.MapSnapshot{}
	}
	return state.Map
}

// rememberFactionNPCSelf 把公共 NPC 的阵营底色写成其真实记忆（best-effort，失败不打断播种）。来源 faction_birth_self。
func (service *Service) rememberFactionNPCSelf(ctx context.Context, rec *unit.Record, theme string) {
	if service == nil || rec == nil {
		return
	}
	summary := strings.TrimSpace(theme)
	if summary == "" {
		return
	}
	service.rememberUnitWithSourceBestEffort(ctx, rec, factionSpawnBirthTurn, summary, "faction_birth_self", 1)
}

// factionSpawnAlreadySeeded 判定本局是否已播种过阵营 NPC（任一单位 Lineage 命中阵营指纹前缀即算已播种）。
// 确定性、零 LLM：一次 ListBySession + 内存比对。读 DB 失败时保守返回 false（宁可后续 best-effort 重试播种）。
func (service *Service) factionSpawnAlreadySeeded(ctx context.Context, sessionID string, regionID string) bool {
	if service == nil || service.units == nil {
		return false
	}
	records, err := service.units.ListBySession(ctx, sessionID)
	if err != nil {
		return false
	}
	for i := range records {
		if isFactionNPCRecord(&records[i]) {
			return true
		}
	}
	return false
}

// isFactionNPCRecord 判定一条单位记录是否为 SeedFactionSpawn 落库的公共阵营 NPC（Lineage 命中阵营指纹前缀）。
func isFactionNPCRecord(rec *unit.Record) bool {
	if rec == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(rec.Identity.Lineage), factionNPCLineagePrefix)
}

// worldIDForSession best-effort 读取某 session 当前 world_id（用于公共 NPC 入世界）。读不到/未接多世界返回空串。
func (service *Service) worldIDForSession(ctx context.Context, sessionID string) string {
	if service == nil || service.sessions == nil {
		return ""
	}
	state, err := service.sessions.Get(ctx, sessionID)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(state.WorldID)
}

// SeedFactionSpawnBestEffort 是降生入口用的吞错包装：调 SeedFactionSpawn 播种公共阵营 NPC，
// 失败只记日志、返回已落库 NPC 的 UnitIDs，绝不让降生失败——出生点 NPC 是附加体验（与 SeedVillageBestEffort 同模式）。
// 返回的 UnitIDs 供降生入口 append 进 state.AmbientUnitIDs（NPC 上图静态可见，**绝不进 WildUnitIDs/执行 order**）。
func (service *Service) SeedFactionSpawnBestEffort(ctx context.Context, sessionID string, factionID string, regionID string, seed int64, mapSnapshot world.MapSnapshot) []string {
	result, err := service.SeedFactionSpawn(ctx, sessionID, factionID, regionID, seed, mapSnapshot)
	if err != nil {
		log.Printf("seed faction spawn best-effort failed (session=%s faction=%s region=%s): %v; persisted %d",
			sessionID, factionID, regionID, err, len(result.UnitIDs))
	}
	return result.UnitIDs
}

// 单个公共 NPC 每个回合边界挪一格的概率（默认 0.30，低概率让舞台「活」而不喧宾夺主）现由
// runtimeconfig "faction.ambient_wander_move_chance" 提供（默认值在 catalog 注册），读取站点在 advanceAmbientWander 热循环外。

// ambientWanderEnabled 判定出生点公共 NPC 的轻量游走是否开启（QUNXIANG_AMBIENT_WANDER，默认关）。
// 关时 wanderAmbientUnits 直接 no-op，NPC 静态站在出生散布点；开后才在回合边界做确定性微游走。
func ambientWanderEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_AMBIENT_WANDER"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ambientWanderRoll 据 (sessionID, turn, npcID, salt) 的 FNV-32a 取确定性 [0,1) 随机值（同输入同序，可复现）。
// 与 combatActionRoll/weather 同口径——模拟逻辑不用全局 rand，保证回放一致。
func ambientWanderRoll(sessionID string, turn int, npcID string, salt string) float64 {
	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte("ambientwander"))
	_, _ = hasher.Write([]byte(sessionID))
	_, _ = hasher.Write([]byte(npcID))
	_, _ = hasher.Write([]byte(salt))
	_, _ = hasher.Write([]byte(fmt.Sprintf("%d", turn)))
	return float64(hasher.Sum32()%10000) / 10000
}

// wanderAmbientUnits 在 Execution→Deployment 回合边界给出生点公共 NPC 做**轻量确定性微游走**（纯代码、零 LLM）。
//   - flag 门控：QUNXIANG_AMBIENT_WANDER 默认关，关时直接 no-op（NPC 静态站着）。
//   - 每个 NPC 据 FNV(sessionID+turn+npcID) 低概率（faction.ambient_wander_move_chance）决定本拍是否挪 1 格；
//     要挪则从六邻格里确定性挑一个**地图内、未被任何单位占用**的空格，写 Status.PositionQ/R（非受保护字段，直接写）并 Save。
//   - **best-effort**：单个 NPC 读不到/无空位/Save 失败都只跳过该 NPC，绝不阻断回合推进或其余 NPC。
//
// 设计约束：NPC 不自治、不进执行 order——本游走是唯一让其位置变化的途径，刻意做得低频、就近、确定性，
// 只为「让命运地图舞台活起来」，不引入任何决策/战斗/LLM 成本。occupied 集合含全体本局单位当前坐标（含玩家/NPC 互避）。
func (service *Service) wanderAmbientUnits(ctx context.Context, state *State, units []unit.Record) {
	if service == nil || service.units == nil || state == nil {
		return
	}
	if len(state.AmbientUnitIDs) == 0 || !ambientWanderEnabled() {
		return
	}
	snapshot := state.Map
	if snapshot.Width <= 0 || snapshot.Height <= 0 {
		return // 无地图（理论不应发生）则静态，不强塞坐标。
	}
	// 本局全体单位的当前占用坐标（含玩家/敌方/wild/其它 NPC），供 NPC 互避叠放。
	occupied := make(map[string]struct{}, len(units))
	ambientByID := make(map[string]*unit.Record, len(state.AmbientUnitIDs))
	ambientSet := make(map[string]struct{}, len(state.AmbientUnitIDs))
	for _, id := range state.AmbientUnitIDs {
		ambientSet[id] = struct{}{}
	}
	for index := range units {
		rec := &units[index]
		occupied[coordString(world.Coord{Q: rec.Status.PositionQ, R: rec.Status.PositionR})] = struct{}{}
		if _, ok := ambientSet[rec.ID]; ok {
			ambientByID[rec.ID] = rec
		}
	}

	turn := state.TurnState.Turn
	// 热循环外读一次存局部：游走概率每 tick 对全部 NPC 同值，避免内层每 NPC 一次 RLock。
	wanderMoveChance := runtimeconfig.GetFloat("faction.ambient_wander_move_chance")
	for _, npcID := range state.AmbientUnitIDs {
		rec := ambientByID[npcID]
		if rec == nil {
			continue
		}
		if ambientWanderRoll(state.ID, turn, npcID, "move") >= wanderMoveChance {
			continue // 本拍不挪（低概率游走）。
		}
		from := world.Coord{Q: rec.Status.PositionQ, R: rec.Status.PositionR}
		dest, ok := pickAmbientWanderStep(snapshot, from, occupied, func(sub string) uint64 {
			return uint64(ambientWanderRoll(state.ID, turn, npcID, sub) * 1e9)
		})
		if !ok {
			continue // 四周无合法空格则原地不动。
		}
		// 落点：更新占用集（腾出旧格、占新格），写坐标并持久化（best-effort）。
		delete(occupied, coordString(from))
		occupied[coordString(dest)] = struct{}{}
		rec.Status.PositionQ = dest.Q
		rec.Status.PositionR = dest.R
		if err := service.units.Save(ctx, *rec); err != nil {
			log.Printf("ambient wander: save npc=%s: %v", npcID, err)
			// 回滚占用集（落库失败则当作没挪，保后续 NPC 互避一致）。
			delete(occupied, coordString(dest))
			occupied[coordString(from)] = struct{}{}
			rec.Status.PositionQ = from.Q
			rec.Status.PositionR = from.R
		}
	}
}

// pickAmbientWanderStep 为一个 NPC 从当前格 from 的六邻格里确定性挑一个**地图内、未占用**的相邻空格。
// 按确定性哈希给六个方向打分排序后取第一个合法的，保证同输入同落点（可复现）。无合法空格返回 ok=false。
func pickAmbientWanderStep(
	snapshot world.MapSnapshot,
	from world.Coord,
	occupied map[string]struct{},
	hashFor func(sub string) uint64,
) (world.Coord, bool) {
	neighbors := axialNeighbors(from)
	// 确定性起点：据哈希选一个起始方向，再顺时针扫六邻——避免总是偏向同一方向。
	start := int(hashFor("dir") % uint64(len(neighbors)))
	for offset := 0; offset < len(neighbors); offset++ {
		cand := neighbors[(start+offset)%len(neighbors)]
		if !inBounds(snapshot, cand) {
			continue
		}
		if _, taken := occupied[coordString(cand)]; taken {
			continue
		}
		return cand, true
	}
	return world.Coord{}, false
}
