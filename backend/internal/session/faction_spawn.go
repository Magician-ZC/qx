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
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
)

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

// factionMoralJitter 是公共 NPC 道德轴相对阵营基准的最大扰动幅度（±factionMoralJitter，让同阵营 NPC 道德轴≈baseline 而非全同）。
const factionMoralJitter = 6.0

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
//   - **公共非私人**：绝不建玩家↔NPC 的 relations 行；也不织 NPC 间 Bonds、不建锚。关系靠后天游历相遇结成。
//   - worldID 在场时把 NPC join 入世界（inhabitant）并标记 region 作用域为出生据点（供调度/相遇定位）；worldID 空则只落本局。
//
// 幂等守卫：同 session 该据点已播种过阵营 NPC（按阵营指纹 Lineage 前缀识别）则直接返回 0，不重复造人。
// 返回实际落库的 NPC 数与可能的错误（中途出错也返回已落库数，best-effort 调用方据此处理）。
func (service *Service) SeedFactionSpawn(ctx context.Context, sessionID string, factionID string, regionID string, seed int64) (int, error) {
	if service == nil || service.units == nil || service.db == nil {
		return 0, fmt.Errorf("seed faction spawn: missing dependencies")
	}
	fid := faction.Normalize(factionID)
	if fid == "" {
		return 0, fmt.Errorf("seed faction spawn: invalid faction %q", factionID)
	}
	regionID = strings.TrimSpace(regionID)
	if regionID == "" {
		return 0, fmt.Errorf("seed faction spawn: empty region")
	}

	// 幂等守卫：本局该据点已有阵营 NPC 则不重复播种（确定性、零 LLM，仅一次 ListBySession + 内存比对）。
	if service.factionSpawnAlreadySeeded(ctx, sessionID, regionID) {
		return 0, nil
	}

	def, _ := faction.Get(fid)
	worldID := service.worldIDForSession(ctx, sessionID)

	// 人数：8–12，据 (session, faction, region, seed) 确定性取。
	span := uint64(factionSpawnMaxNPC - factionSpawnMinNPC + 1)
	count := factionSpawnMinNPC + int(fnvSpawn(sessionID, fid, regionID, seed, "count")%span)

	archetypes := factionArchetypes[fid]
	lifeThemes := factionLifeThemes[fid]

	saved := 0
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
		rec.MoralAlignment = faction.PerturbBaseline(fid, npcSeed, tag, factionMoralJitter)
		// 阵营底色精化六维野心（出身/传记关键词命中即偏置）。
		rec.Ambition = unit.DeriveAmbition(npcSeed, rec.Identity.Lineage, rec.Identity.Biography)

		if err := service.units.Save(ctx, rec); err != nil {
			return saved, fmt.Errorf("save faction npc %d: %w", i, err)
		}
		saved++

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
	return saved, nil
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
// 失败只记日志、返回已落库人数，绝不让降生失败——出生点 NPC 是附加体验（与 SeedVillageBestEffort 同模式）。
func (service *Service) SeedFactionSpawnBestEffort(ctx context.Context, sessionID string, factionID string, regionID string, seed int64) int {
	n, err := service.SeedFactionSpawn(ctx, sessionID, factionID, regionID, seed)
	if err != nil {
		log.Printf("seed faction spawn best-effort failed (session=%s faction=%s region=%s): %v; persisted %d",
			sessionID, factionID, regionID, err, n)
	}
	return n
}
