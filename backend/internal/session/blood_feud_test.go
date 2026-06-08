package session

// 文件说明：blood_feud 世仇传播纯函数的单元测试（零 DB）：敌意继承的单调性/跳数衰减/不在乎者归零，
// 以及 flag 开关语义。验证「直系哀悼者敌意 > 远系」「纯敌视者不继承」「flag 关 no-op」三条核心契约。

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// 一个对死者怀有强烈正向羁绊（高好感高信任）的直系哀悼者。
func closeMourner(hop int) mournerBond {
	return mournerBond{
		MournerID: "m",
		Trust:     8,
		Affection: 9,
		Fear:      1,
		Rivalry:   0,
		Hop:       hop,
	}
}

func TestBloodFeudInheritance_CareRequired(t *testing.T) {
	// 纯敌视死者的人（好感/信任为负、仇/惧为主）不为死者继承血仇。
	hostile := mournerBond{MournerID: "h", Trust: -6, Affection: -7, Fear: 3, Rivalry: 8, Hop: 0}
	riv, fear := bloodFeudInheritance(hostile)
	if riv != 0 || fear != 0 {
		t.Fatalf("纯敌视死者者不应继承血仇，得到 rivalry=%.3f fear=%.3f", riv, fear)
	}

	// 在乎死者的人继承正向敌意。
	riv, fear = bloodFeudInheritance(closeMourner(0))
	if riv <= 0 || fear <= 0 {
		t.Fatalf("在乎死者的哀悼者应继承敌意，得到 rivalry=%.3f fear=%.3f", riv, fear)
	}
	if fear > riv {
		t.Fatalf("fear 增量应 ≤ rivalry 增量（敌意以仇为主），得到 fear=%.3f rivalry=%.3f", fear, riv)
	}
}

func TestBloodFeudInheritance_HopDecay(t *testing.T) {
	// 直系（hop=0）继承的敌意应严格大于远系（hop=1、hop=2）——可信度按 HopFidelity=0.6^hop 衰减。
	direct, _ := bloodFeudInheritance(closeMourner(0))
	oneHop, _ := bloodFeudInheritance(closeMourner(1))
	twoHop, _ := bloodFeudInheritance(closeMourner(2))
	if !(direct > oneHop && oneHop > twoHop) {
		t.Fatalf("敌意继承应随跳数严格递减：direct=%.4f oneHop=%.4f twoHop=%.4f", direct, oneHop, twoHop)
	}
	if twoHop <= 0 {
		t.Fatalf("两跳仍应有正继承（未到停传地板），得到 %.4f", twoHop)
	}
}

func TestBloodFeudInheritance_Monotonic(t *testing.T) {
	// 关系越亲密（好感越高）→ 继承敌意越强（对 weight 单调）。
	weak := mournerBond{MournerID: "w", Trust: 1, Affection: 1, Hop: 0}
	strong := mournerBond{MournerID: "s", Trust: 6, Affection: 8, Hop: 0}
	weakRiv, _ := bloodFeudInheritance(weak)
	strongRiv, _ := bloodFeudInheritance(strong)
	if !(strongRiv > weakRiv) {
		t.Fatalf("更亲密的哀悼者应继承更强敌意：weak=%.4f strong=%.4f", weakRiv, strongRiv)
	}
}

func TestCareRelevanceForDeceased_Bounds(t *testing.T) {
	// careRelevance 恒在 [0,1]；零关系 → 0。
	if r := careRelevanceForDeceased(mournerBond{MournerID: "z"}); r != 0 {
		t.Fatalf("零关系牵挂相关度应为 0，得到 %.4f", r)
	}
	r := careRelevanceForDeceased(closeMourner(0))
	if r < 0 || r > 1 {
		t.Fatalf("牵挂相关度应在 [0,1]，得到 %.4f", r)
	}
}

func TestBloodFeudEnabled_FlagSemantics(t *testing.T) {
	orig := os.Getenv("QUNXIANG_BLOOD_FEUD")
	defer os.Setenv("QUNXIANG_BLOOD_FEUD", orig)

	os.Unsetenv("QUNXIANG_BLOOD_FEUD")
	if bloodFeudEnabled() {
		t.Fatalf("缺省（未设环境变量）应为关")
	}
	for _, off := range []string{"", "false", "0", "no", "off", "  "} {
		os.Setenv("QUNXIANG_BLOOD_FEUD", off)
		if bloodFeudEnabled() {
			t.Fatalf("值 %q 应判为关", off)
		}
	}
	for _, on := range []string{"true", "1", "yes", "on", "TRUE", " On "} {
		os.Setenv("QUNXIANG_BLOOD_FEUD", on)
		if !bloodFeudEnabled() {
			t.Fatalf("值 %q 应判为开", on)
		}
	}
}

// TestPropagateBloodFeud_FlagOffNoOp 验证 flag 关时 propagateBloodFeud 整段 no-op、不触碰 DB。
// 用 nil-DB 的零值 Service：若 flag 关时提前 return，则不会因解引用 service.db 而 panic；
// 反证 flag 开时（仍 nil db）会在 service==nil/db==nil 守卫处安全返回——两路均不 panic 即通过。
func TestPropagateBloodFeud_FlagOffNoOp(t *testing.T) {
	orig := os.Getenv("QUNXIANG_BLOOD_FEUD")
	defer os.Setenv("QUNXIANG_BLOOD_FEUD", orig)

	svc := &Service{} // db 为 nil
	state := &State{ID: "s1"}
	deceased := unit.Record{ID: "victim"}
	ctx := context.Background()

	os.Setenv("QUNXIANG_BLOOD_FEUD", "false")
	// 不应 panic（flag 关直接 return）。byID 传 nil（非执行主循环路径）。
	svc.propagateBloodFeud(ctx, state, deceased, "killer", "", nil)

	os.Setenv("QUNXIANG_BLOOD_FEUD", "true")
	// flag 开但 db=nil：在 service.db==nil 守卫处安全返回，仍不 panic。
	svc.propagateBloodFeud(ctx, state, deceased, "killer", "", nil)
}

// TestApplyBloodFeudGrief_WritesBackInMemoryRecord 是 BF-1 回归：哀恸 morale 经 Mutator 落库后，
// 须同步回写到执行主循环持有的活指针（byID[mournerID]），否则后续 units.Save(*actor) 会用缺了悲恸的
// 旧内存态整列覆盖落库值，悲恸被静默回滚。本测试证明：传入 byID 时，内存指针的 Status.Morale 与落库值一致。
func TestApplyBloodFeudGrief_WritesBackInMemoryRecord(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "bloodfeud.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{db: db, units: repo, mutator: status.NewMutator(db, repo)}

	// 落一个 active 哀悼者（BootstrapRecord 默认 Morale=0.7）。
	mourner := unit.BootstrapRecord(1, "s1", "player", "她")
	if err := repo.Save(ctx, mourner); err != nil {
		t.Fatalf("保存哀悼者失败: %v", err)
	}
	// 死者也须落库：Mutator 落事件行时 actor_unit_id=死者 ID 有 FK 约束（events.actor_unit_id→units.id）。
	// 实战路径里死者本就在 units 表，此处对齐。
	deceasedRec := unit.BootstrapRecord(3, "s1", "player", "逝者")
	if err := repo.Save(ctx, deceasedRec); err != nil {
		t.Fatalf("保存死者失败: %v", err)
	}
	deceased := deceasedRec

	// 执行主循环持有的活指针映射：故意把内存态 morale 设成 stale 的 0.7（与落库初值一致）。
	live := mourner // 复制一份作为「主循环内存态」
	byID := map[string]*unit.Record{mourner.ID: &live}

	state := &State{ID: "s1"}
	if ok := service.applyBloodFeudGrief(ctx, state, mourner.ID, deceased, byID); !ok {
		t.Fatalf("applyBloodFeudGrief 应成功落地")
	}

	// ① 落库值：morale 应已下挫 bloodFeudGriefMorale（0.7 - 0.06 = 0.64）。
	reloaded, err := repo.GetByID(ctx, mourner.ID)
	if err != nil {
		t.Fatalf("重读哀悼者失败: %v", err)
	}
	wantMorale := 0.64
	if math.Abs(reloaded.Status.Morale-wantMorale) > 1e-9 {
		t.Fatalf("落库 morale 期望 %.2f，得到 %.4f", wantMorale, reloaded.Status.Morale)
	}

	// ② 内存活指针：必须被回写为落库后的值（BF-1 的核心）——否则后续 units.Save(live) 会回滚悲恸。
	if math.Abs(live.Status.Morale-reloaded.Status.Morale) > 1e-9 {
		t.Fatalf("内存活指针 morale (%.4f) 应与落库值 (%.4f) 一致——BF-1 回写丢失", live.Status.Morale, reloaded.Status.Morale)
	}
	// ③ RecentEventIDs 也应随回写带上（留痕事件 ID 进内存态），证明回写的是完整 result.Record 而非仅 morale。
	if len(live.Memory.RecentEventIDs) != len(reloaded.Memory.RecentEventIDs) {
		t.Fatalf("内存活指针 RecentEventIDs 应与落库一致，内存 %d / 落库 %d", len(live.Memory.RecentEventIDs), len(reloaded.Memory.RecentEventIDs))
	}

	// ④ byID==nil 时不 panic、仍落库（跨会话/离线哀悼者路径）。
	mourner2 := unit.BootstrapRecord(2, "s1", "player", "他")
	if err := repo.Save(ctx, mourner2); err != nil {
		t.Fatalf("保存哀悼者2失败: %v", err)
	}
	if ok := service.applyBloodFeudGrief(ctx, state, mourner2.ID, deceased, nil); !ok {
		t.Fatalf("applyBloodFeudGrief(byID=nil) 应成功落地")
	}
	reloaded2, err := repo.GetByID(ctx, mourner2.ID)
	if err != nil {
		t.Fatalf("重读哀悼者2失败: %v", err)
	}
	if math.Abs(reloaded2.Status.Morale-wantMorale) > 1e-9 {
		t.Fatalf("byID=nil 路径落库 morale 期望 %.2f，得到 %.4f", wantMorale, reloaded2.Status.Morale)
	}
}

// newBloodFeudTestService 起一个真实 SQLite 的 Service（含 mutator），供多跳/撮合的 DB 集成测试用。
func newBloodFeudTestService(t *testing.T) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "bloodfeud_integ.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{db: db, units: repo, mutator: status.NewMutator(db, repo)}
	return db, repo, service
}

// seedUnit 落一个 active 单位（多跳/撮合测试用真单位，relations 表对 source/target 有 units FK）。
func seedUnit(t *testing.T, ctx context.Context, repo *unit.Repository, seq int64, id string) {
	t.Helper()
	rec := unit.BootstrapRecord(seq, "s1", "player", id)
	rec.ID = id
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位 %s 失败: %v", id, err)
	}
}

// seedRelation 直接插一条 source→target 的关系四轴（绕过 applyRelationShift 的累加语义，直设绝对值）。
func seedRelation(t *testing.T, ctx context.Context, db *sql.DB, source, target string, trust, fear, affection, rivalry float64) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO relations (source_unit_id, target_unit_id, trust, fear, affection, rivalry, notes_json, updated_at)
		 VALUES (?,?,?,?,?,?, '{}', '2026-01-01 00:00:00')`,
		source, target, trust, fear, affection, rivalry); err != nil {
		t.Fatalf("插关系 %s->%s 失败: %v", source, target, err)
	}
}

// TestLoadMournerBonds_MultiHop 验证多跳哀悼者发现：
//
//	关系图：m0→死者(直系 hop=0)；m1→m0(在乎直系哀悼者 hop=1)；m2→m1(在乎二跳者 hop=2)；m3→m2(三跳，超 MaxHop 不应被发现)。
//	并验证全图去重（一个人只按最浅跳记一次）与凶手/死者排除。
func TestLoadMournerBonds_MultiHop(t *testing.T) {
	ctx := context.Background()
	db, repo, service := newBloodFeudTestService(t)

	for i, id := range []string{"victim", "killer", "m0", "m1", "m2", "m3"} {
		seedUnit(t, ctx, repo, int64(i)+1, id)
	}
	// 关系链（强正向羁绊，确保过继承门）。
	seedRelation(t, ctx, db, "m0", "victim", 8, 1, 9, 0) // hop=0
	seedRelation(t, ctx, db, "m1", "m0", 8, 1, 9, 0)     // hop=1
	seedRelation(t, ctx, db, "m2", "m1", 8, 1, 9, 0)     // hop=2
	seedRelation(t, ctx, db, "m3", "m2", 8, 1, 9, 0)     // hop=3（超 MaxHop，不应被发现）
	// 凶手也碰巧在乎死者——但凶手须被排除（不为自己杀的人哀悼）。
	seedRelation(t, ctx, db, "killer", "victim", 9, 0, 9, 0)

	bonds := service.loadMournerBonds(ctx, "victim", "killer", bloodFeudMournerLimit)

	got := map[string]int{} // mournerID → hop
	for _, b := range bonds {
		got[b.MournerID] = b.Hop
	}
	if hop, ok := got["m0"]; !ok || hop != 0 {
		t.Fatalf("m0 应作 hop=0 直系哀悼者被发现，得 hop=%d ok=%v", hop, ok)
	}
	if hop, ok := got["m1"]; !ok || hop != 1 {
		t.Fatalf("m1 应作 hop=1 二跳哀悼者被发现，得 hop=%d ok=%v", hop, ok)
	}
	if hop, ok := got["m2"]; !ok || hop != 2 {
		t.Fatalf("m2 应作 hop=2 三跳哀悼者被发现，得 hop=%d ok=%v", hop, ok)
	}
	if _, ok := got["m3"]; ok {
		t.Fatalf("m3 超 MaxHop(%d) 不应被发现", bloodFeudMaxHop)
	}
	if _, ok := got["killer"]; ok {
		t.Fatalf("凶手不应作哀悼者")
	}
	if _, ok := got["victim"]; ok {
		t.Fatalf("死者不应作哀悼者")
	}
	// FromUnit：hop=0 的来源应为死者；hop=1 的来源应为 m0。
	for _, b := range bonds {
		switch b.MournerID {
		case "m0":
			if b.FromUnit != "victim" {
				t.Fatalf("m0 的 FromUnit 应为 victim，得 %q", b.FromUnit)
			}
		case "m1":
			if b.FromUnit != "m0" {
				t.Fatalf("m1 的 FromUnit 应为 m0，得 %q", b.FromUnit)
			}
		case "m2":
			if b.FromUnit != "m1" {
				t.Fatalf("m2 的 FromUnit 应为 m1，得 %q", b.FromUnit)
			}
		}
	}
}

// TestLoadMournerBonds_Dedup 验证全图去重：一个人既直接在乎死者(hop0)又在乎直系哀悼者(hop1)，应只按最浅 hop=0 记一次。
func TestLoadMournerBonds_Dedup(t *testing.T) {
	ctx := context.Background()
	db, repo, service := newBloodFeudTestService(t)
	for i, id := range []string{"victim", "killer", "m0", "dup"} {
		seedUnit(t, ctx, repo, int64(i)+1, id)
	}
	seedRelation(t, ctx, db, "m0", "victim", 8, 1, 9, 0)  // m0 直系
	seedRelation(t, ctx, db, "dup", "victim", 7, 1, 8, 0) // dup 也直系（hop=0）
	seedRelation(t, ctx, db, "dup", "m0", 8, 1, 9, 0)     // dup 又在乎 m0（本应 hop=1，但已 hop=0 访问，须去重）

	bonds := service.loadMournerBonds(ctx, "victim", "killer", bloodFeudMournerLimit)
	count := 0
	var dupHop int
	for _, b := range bonds {
		if b.MournerID == "dup" {
			count++
			dupHop = b.Hop
		}
	}
	if count != 1 {
		t.Fatalf("dup 应只出现一次（去重），得 %d 次", count)
	}
	if dupHop != 0 {
		t.Fatalf("dup 应按最浅 hop=0 记录，得 hop=%d", dupHop)
	}
}

// TestLogBloodFeudPropagation_WritesRows 验证传播留痕写入 propagation_log，含 hop/fidelity=0.6^hop 与去重幂等。
func TestLogBloodFeudPropagation_WritesRows(t *testing.T) {
	ctx := context.Background()
	db, _, service := newBloodFeudTestService(t)

	service.logBloodFeudPropagation(ctx, "s1", "origin1", "victim", "m0", 0)
	service.logBloodFeudPropagation(ctx, "s1", "origin1", "m0", "m1", 1)
	service.logBloodFeudPropagation(ctx, "s1", "origin1", "m1", "m2", 2)
	// 同一边重复写应幂等（确定性行 id），不新增行。
	service.logBloodFeudPropagation(ctx, "s1", "origin1", "victim", "m0", 0)

	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM propagation_log WHERE origin_event_id = 'origin1'`).Scan(&n); err != nil {
		t.Fatalf("统计 propagation_log 失败: %v", err)
	}
	if n != 3 {
		t.Fatalf("应有 3 条传播留痕（幂等去重后），得 %d", n)
	}
	// 校验 hop=1 行的 fidelity ≈ 0.6，hop=2 ≈ 0.36。
	var fid1, fid2 float64
	if err := db.QueryRowContext(ctx, `SELECT fidelity FROM propagation_log WHERE to_unit='m1'`).Scan(&fid1); err != nil {
		t.Fatalf("读 m1 fidelity 失败: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT fidelity FROM propagation_log WHERE to_unit='m2'`).Scan(&fid2); err != nil {
		t.Fatalf("读 m2 fidelity 失败: %v", err)
	}
	if math.Abs(fid1-0.6) > 1e-9 {
		t.Fatalf("hop=1 fidelity 应为 0.6，得 %.4f", fid1)
	}
	if math.Abs(fid2-0.36) > 1e-9 {
		t.Fatalf("hop=2 fidelity 应为 0.36，得 %.4f", fid2)
	}
	// hop=0 行 from_unit 非空（死者）；验证可空字段写入正常。
	var fromUnit string
	if err := db.QueryRowContext(ctx, `SELECT from_unit FROM propagation_log WHERE to_unit='m0'`).Scan(&fromUnit); err != nil {
		t.Fatalf("读 m0 from_unit 失败: %v", err)
	}
	if fromUnit != "victim" {
		t.Fatalf("hop=0 from_unit 应为 victim，得 %q", fromUnit)
	}
}

// TestPropagateBloodFeud_MultiHopEndToEnd 端到端（flag 开）：一桩死亡沿多跳传播，
// 直系/二跳/三跳哀悼者都对凶手继承敌意（rivalry+），且 hop 越远继承越弱；propagation_log 留痕齐全。
func TestPropagateBloodFeud_MultiHopEndToEnd(t *testing.T) {
	orig := os.Getenv("QUNXIANG_BLOOD_FEUD")
	defer os.Setenv("QUNXIANG_BLOOD_FEUD", orig)
	os.Setenv("QUNXIANG_BLOOD_FEUD", "true")

	ctx := context.Background()
	db, repo, service := newBloodFeudTestService(t)
	for i, id := range []string{"victim", "killer", "m0", "m1", "m2"} {
		seedUnit(t, ctx, repo, int64(i)+1, id)
	}
	seedRelation(t, ctx, db, "m0", "victim", 8, 1, 9, 0)
	seedRelation(t, ctx, db, "m1", "m0", 8, 1, 9, 0)
	seedRelation(t, ctx, db, "m2", "m1", 8, 1, 9, 0)

	deceased := unit.Record{ID: "victim"}
	deceased.Identity.Name = "逝者"
	state := &State{ID: "s1"}
	service.propagateBloodFeud(ctx, state, deceased, "killer", "", nil)

	rivalryOf := func(mourner string) float64 {
		var r float64
		if err := db.QueryRowContext(ctx,
			`SELECT rivalry FROM relations WHERE source_unit_id=? AND target_unit_id='killer'`, mourner).Scan(&r); err != nil {
			return 0 // 没有行 = 未继承
		}
		return r
	}
	r0, r1, r2 := rivalryOf("m0"), rivalryOf("m1"), rivalryOf("m2")
	if !(r0 > 0 && r1 > 0 && r2 > 0) {
		t.Fatalf("三跳哀悼者都应对凶手继承敌意：m0=%.3f m1=%.3f m2=%.3f", r0, r1, r2)
	}
	if !(r0 > r1 && r1 > r2) {
		t.Fatalf("继承敌意应随跳数严格递减：m0=%.3f m1=%.3f m2=%.3f", r0, r1, r2)
	}
	// propagation_log 应至少有 3 条（三跳的边）。
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM propagation_log WHERE session_id='s1'`).Scan(&n); err != nil {
		t.Fatalf("统计 propagation_log 失败: %v", err)
	}
	if n < 3 {
		t.Fatalf("应有 ≥3 条传播留痕（三跳边），得 %d", n)
	}
}

// TestBloodFeudMatchCandidates_CommonEnemy 验证血仇衍生撮合：两个对同一仇敌怀有血仇的角色被聚成 common_enemy 候选组；
// 仅被一人怀恨的仇敌不成组；flag 关时返回 nil。
func TestBloodFeudMatchCandidates_CommonEnemy(t *testing.T) {
	orig := os.Getenv("QUNXIANG_BLOOD_FEUD")
	defer os.Setenv("QUNXIANG_BLOOD_FEUD", orig)

	ctx := context.Background()
	db, repo, service := newBloodFeudTestService(t)
	for i, id := range []string{"a", "b", "c", "enemyX", "enemyY"} {
		seedUnit(t, ctx, repo, int64(i)+1, id)
	}
	// a、b 共同怀恨 enemyX（rivalry ≥ gate=4）；c 只怀恨 enemyY（独狼，不成共敌组）。
	seedRelation(t, ctx, db, "a", "enemyX", -2, 3, -3, 8)
	seedRelation(t, ctx, db, "b", "enemyX", -1, 2, -2, 6)
	seedRelation(t, ctx, db, "c", "enemyY", -1, 2, -2, 7)

	pool := []unit.Record{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	// flag 关：返回 nil。
	os.Setenv("QUNXIANG_BLOOD_FEUD", "false")
	if groups := service.BloodFeudMatchCandidates(ctx, "s1", pool); groups != nil {
		t.Fatalf("flag 关时应返回 nil，得 %d 组", len(groups))
	}

	// flag 开：enemyX 成共敌组（a、b），enemyY 不成组（只 c 一人）。
	os.Setenv("QUNXIANG_BLOOD_FEUD", "true")
	groups := service.BloodFeudMatchCandidates(ctx, "s1", pool)
	if len(groups) != 1 {
		t.Fatalf("应只产出 1 个共敌组（enemyX），得 %d 个", len(groups))
	}
	g := groups[0]
	if g.EnemyID != "enemyX" {
		t.Fatalf("共敌应为 enemyX，得 %q", g.EnemyID)
	}
	if g.Kind != bloodFeudMatchKind {
		t.Fatalf("社会客体类型应为 %q，得 %q", bloodFeudMatchKind, g.Kind)
	}
	if len(g.Candidates) != 2 {
		t.Fatalf("enemyX 共敌组应有 2 名候选(a,b)，得 %d", len(g.Candidates))
	}
	// 候选按 id 升序（确定性）：a 在前。
	if g.Candidates[0].UnitID != "a" || g.Candidates[1].UnitID != "b" {
		t.Fatalf("候选应按 id 升序 a,b，得 %q,%q", g.Candidates[0].UnitID, g.Candidates[1].UnitID)
	}
	// RelationIntersect = 世仇强度归一：a(rivalry 8)应高于 b(rivalry 6)。
	if !(g.Candidates[0].RelationIntersect > g.Candidates[1].RelationIntersect) {
		t.Fatalf("a 恨得更深，RelationIntersect 应更高：a=%.3f b=%.3f",
			g.Candidates[0].RelationIntersect, g.Candidates[1].RelationIntersect)
	}
	// HookFit 确定性：同输入再算应一致。
	groups2 := service.BloodFeudMatchCandidates(ctx, "s1", pool)
	if groups2[0].Candidates[0].HookFit != g.Candidates[0].HookFit {
		t.Fatalf("HookFit 应确定性可复现")
	}
}
