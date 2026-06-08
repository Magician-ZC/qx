package session

// 文件说明：出生关系网落库集成测试（对真实 SQLite）：20 人 + 入世界 + 织关系 + 可复现 + 出生仇怨进记忆链路。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/villageseed"
	"qunxiang/backend/internal/world"
)

// TestIsSeededVillagerRecord 锁定村庄幂等指纹：中文性别=村民；wanderer(玩家)/child(孩子)/空=非村民；出身原型=村民。
// 回归点：孩子 Lineage="child" 曾因 lineage!=wanderer 被误判为村民，会让「只生过孩子」的局永久跳过织村（对抗评审 low 修复）。
func TestIsSeededVillagerRecord(t *testing.T) {
	cases := []struct {
		name    string
		gender  string
		lineage string
		want    bool
	}{
		{"村民·中文性别", "女", "边境猎户", true},
		{"村民·中文性别男·空原型", "男", "", true},
		{"玩家主单位·英文性别·wanderer", "female", "wanderer", false},
		{"孩子·英文性别·child", "male", "child", false},
		{"普通单位·英文性别·空原型", "male", "", false},
		{"出身原型·英文性别", "male", "流亡铁匠", true},
	}
	for _, tc := range cases {
		rec := &unit.Record{}
		rec.Identity.Gender = tc.gender
		rec.Identity.Lineage = tc.lineage
		if got := isSeededVillagerRecord(rec); got != tc.want {
			t.Fatalf("%s: isSeededVillagerRecord(gender=%q,lineage=%q)=%v，期望 %v", tc.name, tc.gender, tc.lineage, got, tc.want)
		}
	}
	if isSeededVillagerRecord(nil) {
		t.Fatalf("nil 记录应判为非村民")
	}
}

func TestSeedVillagePersists(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()
	wid, err := world.Create(ctx, service.db, world.World{Name: "出生世界"})
	if err != nil {
		t.Fatalf("建世界失败: %v", err)
	}

	villagers, err := service.SeedVillage(ctx, "s1", "player", wid, 7)
	if err != nil {
		t.Fatalf("播种村庄失败: %v", err)
	}
	if len(villagers) != villageseed.VillageSize {
		t.Fatalf("应落库 %d 人，得到 %d", villageseed.VillageSize, len(villagers))
	}

	// 人确实落库，且人格/生平持久化。
	first := villagers[0]
	rec, err := repo.GetByID(ctx, first.UnitID)
	if err != nil {
		t.Fatalf("取村民失败: %v", err)
	}
	if rec.Identity.Name != first.Member.Name {
		t.Fatalf("姓名应一致：%q vs %q", rec.Identity.Name, first.Member.Name)
	}
	if rec.Personality.Courage != first.Member.Traits.Courage {
		t.Fatalf("人格应持久化：%v vs %v", rec.Personality.Courage, first.Member.Traits.Courage)
	}
	if rec.Identity.Biography == "" || rec.Identity.Lineage == "" {
		t.Fatalf("生平/出身应写入：%+v", rec.Identity)
	}

	// 全员入世界。
	members, _ := world.Members(ctx, service.db, wid, 0)
	if len(members) != villageseed.VillageSize {
		t.Fatalf("应有 %d 人入世界，得到 %d", villageseed.VillageSize, len(members))
	}

	// 关系网落库（relations 表非空）。
	var relCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations`).Scan(&relCount); err != nil {
		t.Fatalf("统计 relations 失败: %v", err)
	}
	if relCount == 0 {
		t.Fatalf("出生关系网应落 relations 行")
	}

	// 可复现：落库的人名序列应与纯生成器一致。
	gen := villageseed.Generate(wid, 7)
	for i, vv := range villagers {
		if vv.Member.Name != gen.Members[i].Name {
			t.Fatalf("第 %d 人应与生成器一致：%q vs %q", i, vv.Member.Name, gen.Members[i].Name)
		}
	}
}

// TestSeedVillageBestEffortReturnsCount 断言 onboarding 用的吞错包装在 worldID 为空（不入世界）时
// 仍落库满员 20 人并返回数量、织出关系网——这是 /api/units/bootstrap?with_village=1
// 兑现「身边二十人」的核心路径（worldID 空=不进世界，纯本局关系网）。
func TestSeedVillageBestEffortReturnsCount(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()

	n := service.SeedVillageBestEffort(ctx, "s-bootstrap", "player", "", 42)
	if n != villageseed.VillageSize {
		t.Fatalf("best-effort 应落库 %d 人，得到 %d", villageseed.VillageSize, n)
	}

	// worldID 为空时不入世界，但关系网仍落库（relations 表非空）。
	var relCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM relations`).Scan(&relCount); err != nil {
		t.Fatalf("统计 relations 失败: %v", err)
	}
	if relCount == 0 {
		t.Fatalf("出生关系网应落 relations 行")
	}
}

// TestSeedVillageBestEffortSwallowsError 断言依赖缺失时包装吞错返回 0 而非 panic，
// 保证 bootstrap 主路径（建主单位）永不被村庄附加体验拖垮。
func TestSeedVillageBestEffortSwallowsError(t *testing.T) {
	var nilService *Service
	if got := nilService.SeedVillageBestEffort(context.Background(), "s", "f", "", 1); got != 0 {
		t.Fatalf("依赖缺失应吞错返回 0，得到 %d", got)
	}
}

// pickFeudSeed 在若干种子里挑一个「至少含一条仇怨关系」的种子，让仇怨记忆断言一定有料可验。
func pickFeudSeed(t *testing.T, worldID string) (seed int64, feud villageseed.Bond) {
	t.Helper()
	for s := int64(1); s <= 200; s++ {
		gen := villageseed.Generate(worldID, s)
		for _, b := range gen.Bonds {
			if _, isFeud := birthBondMemorySource(b.Kind); isFeud {
				return s, b
			}
		}
	}
	t.Fatalf("1..200 种子内未找到任何仇怨关系，内容池或生成器有变")
	return 0, villageseed.Bond{}
}

// TestSeedVillageWritesFeudMemory 断言「出生即结仇」被写成可检索/可衰减/可闪回的真实记忆：
//  1. 源村民的 memories 表里有一条 village_birth_feud 来源、含对方名字的记忆（可被记忆链路检索关联）；
//  2. 该仇怨记忆带较高重要度（importance>=6 → metadata.Permanent=true，更难衰减、可触发闪回）；
//  3. 每名村民的「定调种子记忆」也写成 village_birth_self 真实记忆（最早人生底色）。
func TestSeedVillageWritesFeudMemory(t *testing.T) {
	db, _, service := newThreatTestService(t)
	ctx := context.Background()
	const worldID = "" // 本局关系网，不入世界，足够验证记忆。

	seed, feud := pickFeudSeed(t, worldID)
	gen := villageseed.Generate(worldID, seed)

	villagers, err := service.SeedVillage(ctx, "s-feud", "player", worldID, seed)
	if err != nil {
		t.Fatalf("播种村庄失败: %v", err)
	}
	if len(villagers) != villageseed.VillageSize {
		t.Fatalf("应落库 %d 人，得到 %d", villageseed.VillageSize, len(villagers))
	}

	srcID := villagers[feud.From].UnitID
	tgtName := gen.Members[feud.To].Name

	// 1) 仇怨记忆落了 memories 表，来源 village_birth_feud，且精确引用这条 (From→To) 仇怨对方的名字。
	//    （源村民可能有多条仇怨，故按对方名字精确定位，而不是取最新一条。）
	var summary, category, metaJSON string
	var emotionWeight float64
	err = db.QueryRowContext(ctx, `
		SELECT summary, category, emotion_weight, metadata_json
		FROM memories
		WHERE unit_id = ? AND metadata_json LIKE '%village_birth_feud%' AND summary LIKE ?
		ORDER BY created_at DESC LIMIT 1`, srcID, "%"+tgtName+"%",
	).Scan(&summary, &category, &emotionWeight, &metaJSON)
	if err != nil {
		t.Fatalf("应能查到源村民对 %q 的出生仇怨记忆: %v", tgtName, err)
	}
	if !strings.Contains(summary, tgtName) {
		t.Fatalf("仇怨记忆应含对方名字 %q，得到 %q", tgtName, summary)
	}

	// 2) 重要度足够高（importance>=6 → Permanent=true），保证能扛过衰减并可闪回。
	var meta memoryMetadata
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("解析 metadata 失败: %v (%s)", err, metaJSON)
	}
	if meta.Source != "village_birth_feud" {
		t.Fatalf("记忆来源应为 village_birth_feud，得到 %q", meta.Source)
	}
	if meta.Importance < 6 {
		t.Fatalf("出生仇怨重要度应 >=6（可闪回/抗衰减），得到 %d", meta.Importance)
	}
	if !meta.Permanent {
		t.Fatalf("高重要度仇怨记忆应标记 Permanent，得到 %+v", meta)
	}

	// 3) 每名村民的种子记忆也落成 village_birth_self 真实记忆。
	var selfCount int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM memories WHERE metadata_json LIKE '%village_birth_self%'`,
	).Scan(&selfCount); err != nil {
		t.Fatalf("统计 village_birth_self 记忆失败: %v", err)
	}
	if selfCount == 0 {
		t.Fatalf("应至少落一条 village_birth_self 种子记忆")
	}
}

// TestBirthBondMemoryDeterministic 断言同一 (worldID,seed,bond) 永远渲染同一句仇怨记忆，
// 不同 bond 间能区分——这是「确定性随机用 FNV 哈希、可复现」铁律在村庄仇怨上的体现。
func TestBirthBondMemoryDeterministic(t *testing.T) {
	const worldID = "w-det"
	const seed = int64(7)
	b := villageseed.Bond{From: 3, To: 9, Kind: "宿敌"}

	a1 := birthBondMemoryText(worldID, seed, b, "林采薇")
	a2 := birthBondMemoryText(worldID, seed, b, "林采薇")
	if a1 != a2 {
		t.Fatalf("同参数应渲染同一句：%q vs %q", a1, a2)
	}
	if !strings.Contains(a1, "林采薇") {
		t.Fatalf("文本应含对方名字，得到 %q", a1)
	}
	// 仇怨文本应携带高情绪关键词（仇/恨/怵/背叛之一），以便下游正确加权。
	if !containsAny(a1, "仇", "恨", "怵", "背叛", "戒心", "气") {
		t.Fatalf("仇怨文本应携带恩怨语义关键词，得到 %q", a1)
	}

	// 换个 seed 应有机会得到不同变体（同 bond 不同种子撞同变体概率低，但至少不应 panic 且确定）。
	other := birthBondMemoryText(worldID, seed+1, b, "林采薇")
	_ = other // 仅验证调用确定、不 panic；变体差异由哈希分布保证。
}
