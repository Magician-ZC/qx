package session

// 文件说明：记忆衰减/cap/闪回的确定性回归测试（最易回归的确定性逻辑）。
// 覆盖 memory_store.go：
//  ① computeMemorySalience —— exp 衰减(tau=120)、importance^2.5 加成、permanent 免衰减地板 0.8、单调性；
//  ② enforceMemoryCaps —— 按 salience 升序逐出、permanent 跳过、各类 cap 生效；
//  ③ selectFlashbackMemories —— 命中阈(memoryFlashbackMin=0.8)与排序。
// 仅新增测试，零生产改动；用 memDecay_ 前缀避免与既有 helper（newThreatTestService 等）撞名。

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/unit"
)

// ── ① computeMemorySalience ──────────────────────────────────────────────

// TestMemDecay_SalienceExactExpDecay 锁定衰减公式：score = base*emotion*exp(-elapsed / (tau*importance^alpha))。
func TestMemDecay_SalienceExactExpDecay(t *testing.T) {
	meta := memoryMetadata{Turn: 1, Importance: 4, BaseSalience: 1, Permanent: false}
	currentTurn := 121 // elapsed = 120
	got := computeMemorySalience(currentTurn, meta, 1.0)

	denom := memoryDecayTauTurns * math.Pow(4, memoryDecayAlpha) // 120 * 4^2.5
	want := 1.0 * 1.0 * math.Exp(-120.0/denom)
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("衰减公式不符：得到 %v，期望 %v（denom=%v）", got, want, denom)
	}
}

// TestMemDecay_SalienceImportanceBoost 验证 importance^2.5 加成：更高重要度衰减更慢（同 elapsed 下 salience 更高）。
func TestMemDecay_SalienceImportanceBoost(t *testing.T) {
	currentTurn := 200
	lowImp := computeMemorySalience(currentTurn, memoryMetadata{Turn: 1, Importance: 2, BaseSalience: 1}, 1.0)
	highImp := computeMemorySalience(currentTurn, memoryMetadata{Turn: 1, Importance: 9, BaseSalience: 1}, 1.0)
	if !(highImp > lowImp) {
		t.Fatalf("高重要度应衰减更慢：imp9=%v 应 > imp2=%v", highImp, lowImp)
	}
	// importance 对 salience 应单调不减。
	prev := -1.0
	for imp := 1; imp <= 10; imp++ {
		s := computeMemorySalience(currentTurn, memoryMetadata{Turn: 1, Importance: imp, BaseSalience: 1}, 1.0)
		if s+1e-12 < prev {
			t.Fatalf("salience 应随 importance 单调不减：imp=%d s=%v < prev=%v", imp, s, prev)
		}
		prev = s
	}
}

// TestMemDecay_SaliencePermanentFloor 验证 permanent 记忆免衰减且有 0.8 地板。
func TestMemDecay_SaliencePermanentFloor(t *testing.T) {
	// 极远 currentTurn 也不衰减：permanent 分支不看 elapsed。
	farFuture := computeMemorySalience(100000, memoryMetadata{Turn: 1, Importance: 4, BaseSalience: 1, Permanent: true}, 1.0)
	if farFuture != 1.0 {
		t.Fatalf("permanent(base=1,emotion=1) 应恒为 base*emotion=1，得到 %v", farFuture)
	}
	// 低 base*emotion 触发 0.8 地板。
	floored := computeMemorySalience(100000, memoryMetadata{Turn: 1, Importance: 4, BaseSalience: 0.1, Permanent: true}, 1.0)
	if math.Abs(floored-0.8) > 1e-9 {
		t.Fatalf("permanent 低分应被托到地板 0.8，得到 %v", floored)
	}
	if floored < memoryFlashbackMin-1e-9 {
		t.Fatalf("permanent 地板应≥%v，得到 %v", memoryFlashbackMin, floored)
	}
}

// TestMemDecay_SalienceMonotoneInElapsed 验证非永久记忆 salience 随 elapsed（currentTurn 增大）单调不增，且有 0.01 下限。
func TestMemDecay_SalienceMonotoneInElapsed(t *testing.T) {
	meta := memoryMetadata{Turn: 1, Importance: 4, BaseSalience: 1, Permanent: false}
	prev := math.Inf(1)
	for currentTurn := 1; currentTurn <= 5000; currentTurn += 50 {
		s := computeMemorySalience(currentTurn, meta, 1.0)
		if s > prev+1e-12 {
			t.Fatalf("salience 应随 elapsed 单调不增：t=%d s=%v > prev=%v", currentTurn, s, prev)
		}
		if s < 0.01-1e-12 {
			t.Fatalf("salience 应有 0.01 下限，t=%d 得到 %v", currentTurn, s)
		}
		prev = s
	}
}

// ── ② enforceMemoryCaps ──────────────────────────────────────────────────

// memDecayInsertMemory 直接向 memories 表写一条记忆（绕过 LLM 入库链路，便于精确构造 cap 场景）。
func memDecayInsertMemory(t *testing.T, service *Service, unitID, category string, salience float64, permanent bool, turn int) string {
	t.Helper()
	id := uuid.NewString()
	meta := memoryMetadata{Turn: turn, Importance: 4, BaseSalience: 1, Permanent: permanent}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if _, err := service.db.ExecContext(
		context.Background(),
		`INSERT INTO memories (id, unit_id, category, summary, emotion_weight, salience, recall_count, metadata_json, created_at)
		 VALUES (?, ?, ?, ?, 1.0, ?, 0, ?, ?)`,
		id, unitID, category, fmt.Sprintf("记忆-%s-%.4f-%d", category, salience, turn),
		salience, string(metaJSON), time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	return id
}

func memDecayCountCategory(t *testing.T, service *Service, unitID, category string) int {
	t.Helper()
	n, err := loadMemoryCountByCategory(context.Background(), service.db, unitID, category)
	if err != nil {
		t.Fatalf("count category: %v", err)
	}
	return n
}

func memDecayMemoryExists(t *testing.T, service *Service, id string) bool {
	t.Helper()
	var n int
	if err := service.db.QueryRowContext(context.Background(),
		`SELECT COUNT(1) FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("exists query: %v", err)
	}
	return n > 0
}

// TestMemDecay_EnforceCapsEvictsLowestSalience 验证超出 cap 时按 salience 升序逐出最低者。
func TestMemDecay_EnforceCapsEvictsLowestSalience(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(2, "s1", "player", "记忆者")
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save unit: %v", err)
	}

	cap := memoryCategoryCaps[memoryCategoryRelation] // 150
	// 写 cap+3 条，salience 严格递增，全部非永久。最低的 3 条应被逐出。
	type rec struct {
		id       string
		salience float64
	}
	recs := make([]rec, 0, cap+3)
	for i := 0; i < cap+3; i++ {
		s := 0.05 + float64(i)*0.01 // 递增，互不相同
		id := memDecayInsertMemory(t, service, actor.ID, memoryCategoryRelation, s, false, 1)
		recs = append(recs, rec{id: id, salience: s})
	}

	if err := service.enforceMemoryCaps(ctx, actor.ID); err != nil {
		t.Fatalf("enforce caps: %v", err)
	}

	if got := memDecayCountCategory(t, service, actor.ID, memoryCategoryRelation); got != cap {
		t.Fatalf("裁剪后应剩 cap=%d 条，得到 %d", cap, got)
	}
	// 最低 salience 的 3 条（前 3 个插入）应被逐出。
	for i := 0; i < 3; i++ {
		if memDecayMemoryExists(t, service, recs[i].id) {
			t.Fatalf("最低 salience 第 %d 条应被逐出（salience=%.4f）", i, recs[i].salience)
		}
	}
	// 其余应保留。
	for i := 3; i < len(recs); i++ {
		if !memDecayMemoryExists(t, service, recs[i].id) {
			t.Fatalf("较高 salience 第 %d 条不应被逐出（salience=%.4f）", i, recs[i].salience)
		}
	}
}

// TestMemDecay_EnforceCapsSkipsPermanent 验证 cap 裁剪跳过永久记忆，即便它们 salience 更低。
func TestMemDecay_EnforceCapsSkipsPermanent(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(2, "s1", "player", "守忆者")
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save unit: %v", err)
	}

	cap := memoryCategoryCaps[memoryCategorySpatial] // 200
	// 2 条极低 salience 的永久记忆 + cap 条较高 salience 的非永久记忆 = cap+2 总数，溢出 2。
	permIDs := []string{
		memDecayInsertMemory(t, service, actor.ID, memoryCategorySpatial, 0.001, true, 1),
		memDecayInsertMemory(t, service, actor.ID, memoryCategorySpatial, 0.002, true, 1),
	}
	nonPermIDs := make([]string, 0, cap)
	for i := 0; i < cap; i++ {
		id := memDecayInsertMemory(t, service, actor.ID, memoryCategorySpatial, 0.5+float64(i)*0.001, false, 1)
		nonPermIDs = append(nonPermIDs, id)
	}

	if err := service.enforceMemoryCaps(ctx, actor.ID); err != nil {
		t.Fatalf("enforce caps: %v", err)
	}

	// 永久记忆不可被逐出，即便它们 salience 最低。
	for i, id := range permIDs {
		if !memDecayMemoryExists(t, service, id) {
			t.Fatalf("永久记忆第 %d 条不应被逐出", i)
		}
	}
	// 溢出 2 条只能从非永久里逐出最低的 2 条（前 2 个插入）。
	evicted := 0
	for i := 0; i < 2; i++ {
		if !memDecayMemoryExists(t, service, nonPermIDs[i]) {
			evicted++
		}
	}
	if evicted != 2 {
		t.Fatalf("溢出 2 条应从非永久最低 salience 中逐出 2 条，实际逐出 %d", evicted)
	}
	if got := memDecayCountCategory(t, service, actor.ID, memoryCategorySpatial); got != cap {
		t.Fatalf("裁剪后该分类应剩 cap=%d 条，得到 %d", cap, got)
	}
}

// TestMemDecay_EnforceCapsPerCategoryIndependent 验证 cap 按分类独立生效：未超 cap 的分类不被裁剪。
func TestMemDecay_EnforceCapsPerCategoryIndependent(t *testing.T) {
	_, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(2, "s1", "player", "分类者")
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save unit: %v", err)
	}

	// relation 分类塞满到 cap+5（应裁到 cap），spatial 分类只塞 10（应原样保留）。
	relationCap := memoryCategoryCaps[memoryCategoryRelation]
	for i := 0; i < relationCap+5; i++ {
		memDecayInsertMemory(t, service, actor.ID, memoryCategoryRelation, 0.1+float64(i)*0.001, false, 1)
	}
	for i := 0; i < 10; i++ {
		memDecayInsertMemory(t, service, actor.ID, memoryCategorySpatial, 0.1+float64(i)*0.001, false, 1)
	}

	if err := service.enforceMemoryCaps(ctx, actor.ID); err != nil {
		t.Fatalf("enforce caps: %v", err)
	}

	if got := memDecayCountCategory(t, service, actor.ID, memoryCategoryRelation); got != relationCap {
		t.Fatalf("relation 应被裁到 cap=%d，得到 %d", relationCap, got)
	}
	if got := memDecayCountCategory(t, service, actor.ID, memoryCategorySpatial); got != 10 {
		t.Fatalf("spatial 未超 cap，应原样保留 10 条，得到 %d", got)
	}
}

// ── ③ selectFlashbackMemories ────────────────────────────────────────────

// TestMemDecay_FlashbackThresholdAndOrder 验证闪回阈值（memoryFlashbackMin=0.8）与命中排序。
func TestMemDecay_FlashbackThresholdAndOrder(t *testing.T) {
	features := flashbackFeatures{
		Terrain:    "森林",           // +0.45
		Weather:    "暴雨",           // +0.25
		EnemyNames: []string{"黑鸦"}, // +0.30
	}

	// 满命中（terrain+weather+enemy = 1.0，含「伏击」再加但已 cap=1）。
	full := memoryRow{ID: "full", Summary: "在森林暴雨里被黑鸦伏击，几乎丧命"}
	// 三命中无伏击关键词（0.45+0.25+0.30=1.0，cap 后 1.0）—— 与 full 同分，用 Summary 字典序决定先后。
	// 仅 terrain+weather（0.70 < 0.8）—— 应被滤除。
	low := memoryRow{ID: "low", Summary: "森林里下着暴雨，平静无事"}
	// terrain+enemy（0.75 < 0.8）—— 应被滤除。
	low2 := memoryRow{ID: "low2", Summary: "森林中远远看到黑鸦"}
	// terrain+enemy+伏击（0.45+0.30+0.10=0.85 ≥ 0.8）—— 达阈，应入选。
	// 注意：terrain+weather+伏击=0.45+0.25+0.10 在 float64 下是 0.7999…< 0.8（裸边界不可靠），故用稳健的 0.85 combo。
	edge := memoryRow{ID: "edge", Summary: "森林中遭黑鸦伏击"}

	rows := []memoryRow{low, full, low2, edge}
	selected := selectFlashbackMemories(rows, features, 5)

	gotIDs := map[string]bool{}
	for _, r := range selected {
		gotIDs[r.ID] = true
	}
	if gotIDs["low"] {
		t.Fatalf("仅 terrain+weather(0.70) 应被阈值滤除")
	}
	if gotIDs["low2"] {
		t.Fatalf("仅 terrain+enemy(0.75) 应被阈值滤除")
	}
	if !gotIDs["full"] {
		t.Fatalf("满命中(1.0)应入选")
	}
	if !gotIDs["edge"] {
		t.Fatalf("达阈(0.85)的记忆应入选")
	}
	// full 分(1.0) > edge 分(0.85)，应排在 edge 之前。
	posFull, posEdge := -1, -1
	for i, r := range selected {
		if r.ID == "full" {
			posFull = i
		}
		if r.ID == "edge" {
			posEdge = i
		}
	}
	if posFull < 0 || posEdge < 0 || posFull > posEdge {
		t.Fatalf("高分 full 应排在 edge 之前：selected=%v", selected)
	}
}

// TestMemDecay_FlashbackLimitRespected 验证 limit 截断：候选多于 limit 时只取前 limit 个（按分降序）。
func TestMemDecay_FlashbackLimitRespected(t *testing.T) {
	features := flashbackFeatures{Terrain: "森林", Weather: "暴雨", EnemyNames: []string{"黑鸦"}}
	rows := []memoryRow{
		{ID: "m1", Summary: "森林暴雨黑鸦伏击 一"},
		{ID: "m2", Summary: "森林暴雨黑鸦伏击 二"},
		{ID: "m3", Summary: "森林暴雨黑鸦伏击 三"},
	}
	if got := selectFlashbackMemories(rows, features, 2); len(got) != 2 {
		t.Fatalf("limit=2 应只返回 2 条，得到 %d", len(got))
	}
	if got := selectFlashbackMemories(rows, features, 0); got != nil {
		t.Fatalf("limit=0 应返回 nil，得到 %v", got)
	}
	if got := selectFlashbackMemories(nil, features, 3); got != nil {
		t.Fatalf("空候选应返回 nil，得到 %v", got)
	}
}

// TestMemDecay_FlashbackScoreComponents 直接锁定 memoryFlashbackScore 的加权常量，防止评分回归。
func TestMemDecay_FlashbackScoreComponents(t *testing.T) {
	f := flashbackFeatures{Terrain: "森林", Weather: "暴雨", EnemyNames: []string{"黑鸦"}}

	cases := []struct {
		summary string
		want    float64
	}{
		{"无关文本", 0},
		{"只有森林", 0.45},
		{"只有暴雨", 0.25},
		{"看到黑鸦", 0.30},
		{"森林里有黑鸦", 0.75},
		{"森林暴雨", 0.70},
		{"森林暴雨遭遇伏击", 0.80}, // 0.45+0.25+0.10
		{"森林暴雨黑鸦", 1.0},    // 0.45+0.25+0.30=1.0（cap）
	}
	for _, c := range cases {
		got := memoryFlashbackScore(c.summary, f)
		if math.Abs(got-c.want) > 1e-9 {
			t.Fatalf("memoryFlashbackScore(%q)=%v，期望 %v", c.summary, got, c.want)
		}
	}
}
