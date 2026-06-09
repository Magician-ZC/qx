package session

// 文件说明：blood_feud 世仇传播纯函数的单元测试（零 DB）：敌意继承的单调性/跳数衰减/不在乎者归零，
// 以及 flag 开关语义。验证「直系哀悼者敌意 > 远系」「纯敌视者不继承」「flag 关 no-op」三条核心契约。
// 另含黑吃黑 + 罗生门 echo + 衍生传播 + blood_feud 社会客体 的 DB 集成测试（对真实 SQLite）。

import (
	"context"
	"database/sql"
	"math"
	"os"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/socialobject"
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
	if !bloodFeudEnabled() {
		t.Fatalf("缺省（未设环境变量）应为默认开")
	}
	// 空串/纯空白现按默认开处理（视为未显式关）；非法值同样默认开。
	for _, on := range []string{"", "  ", "true", "1", "yes", "on", "TRUE", " On ", "garbage"} {
		os.Setenv("QUNXIANG_BLOOD_FEUD", on)
		if !bloodFeudEnabled() {
			t.Fatalf("值 %q 应判为开（默认开，仅 false/0/no/off 关）", on)
		}
	}
	for _, off := range []string{"false", "0", "no", "off", "FALSE", " Off "} {
		os.Setenv("QUNXIANG_BLOOD_FEUD", off)
		if bloodFeudEnabled() {
			t.Fatalf("值 %q 应判为关", off)
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
//	并验证全图去重（一个人只按最浅跳数记一次）与凶手/死者排除。
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

// ════════════════════════════════════════════════════════════════════════════
// 黑吃黑 + 罗生门 echo + 衍生传播 + blood_feud 社会客体 的集成测试
// ════════════════════════════════════════════════════════════════════════════

// seedFeudWorld 建一个标准黑吃黑场景：受害者 victim、背叛者 betrayer，及在乎 victim 的密友 f1(直系) / f2(二跳)。
// 各单位均已落库，关系链已建（确保过衍生继承门）。
func seedFeudWorld(t *testing.T, ctx context.Context, db *sql.DB, repo *unit.Repository) {
	t.Helper()
	for i, id := range []string{"victim", "betrayer", "f1", "f2"} {
		seedUnit(t, ctx, repo, int64(i)+1, id)
	}
	seedRelation(t, ctx, db, "f1", "victim", 8, 1, 9, 0) // f1 直系密友（在乎 victim）
	seedRelation(t, ctx, db, "f2", "f1", 7, 1, 8, 0)     // f2 在乎 f1（你信任的人的密友，hop=1）
}

// TestPropagateCrossDerived_HopDecayAndEcho 验证衍生传播：黑吃黑沿受害者关系图衍生到第三方（密友），
// hop 越远 echo 的 relevance/fate_score 越弱（衰减）；且每个第三方各写一条 cross_event_echo（视角化叙事 narrative_zh）。
func TestPropagateCrossDerived_HopDecayAndEcho(t *testing.T) {
	orig := os.Getenv("QUNXIANG_BLOOD_FEUD")
	defer os.Setenv("QUNXIANG_BLOOD_FEUD", orig)
	os.Setenv("QUNXIANG_BLOOD_FEUD", "true")

	ctx := context.Background()
	db, repo, service := newBloodFeudTestService(t)
	seedFeudWorld(t, ctx, db, repo)

	victim := unit.Record{ID: "victim"}
	victim.Identity.Name = "阿吴"
	owners := service.propagateCrossDerived(ctx, "s1", "", "xevt1", victim, "betrayer")

	// f1（直系）与 f2（二跳）都应被衍生到（在乎 victim / 在乎 f1）。
	got := map[string]bool{}
	for _, o := range owners {
		got[o] = true
	}
	if !got["f1"] || !got["f2"] {
		t.Fatalf("直系密友 f1 与二跳密友 f2 都应被衍生到，得 %v", owners)
	}
	if got["betrayer"] || got["victim"] {
		t.Fatalf("背叛者/受害者本人不应作衍生第三方，得 %v", owners)
	}

	// 各第三方各一条 echo（同 cross_event_id=xevt1，罗生门）。
	echoes, err := service.ListCrossEventEchoes(ctx, "xevt1")
	if err != nil {
		t.Fatalf("列 echo 失败: %v", err)
	}
	byOwner := map[string]CrossEventEcho{}
	for _, e := range echoes {
		byOwner[e.OwnerUnitID] = e
		if e.CrossEventID != "xevt1" {
			t.Fatalf("echo 应绑同一 cross_event_id=xevt1，得 %q", e.CrossEventID)
		}
		if e.NarrativeZH == "" {
			t.Fatalf("echo 应有视角化叙事 narrative_zh，owner=%s 为空", e.OwnerUnitID)
		}
	}
	if _, ok := byOwner["f1"]; !ok {
		t.Fatalf("f1 应有一条 echo")
	}
	if _, ok := byOwner["f2"]; !ok {
		t.Fatalf("f2 应有一条 echo")
	}
	// hop 衰减：直系 f1(hop=0) 的 relevance 应严格高于二跳 f2(hop=1)。
	if !(byOwner["f1"].Hop == 0 && byOwner["f2"].Hop == 1) {
		t.Fatalf("hop 应为 f1=0 / f2=1，得 f1=%d f2=%d", byOwner["f1"].Hop, byOwner["f2"].Hop)
	}
	if !(byOwner["f1"].Relevance > byOwner["f2"].Relevance) {
		t.Fatalf("衍生相关度应随跳数衰减：f1=%.4f f2=%.4f", byOwner["f1"].Relevance, byOwner["f2"].Relevance)
	}
}

// TestCrossEventEchoes_Rashomon 验证罗生门：同一 cross_event_id 在多个 owner/session 各有一条 echo（视角化叙事），
// 事实唯一——echo 仅视角层。本测试直接写两条不同 owner、同 cross_event_id 的 echo，验证多视角同 id 可被一起列出，且幂等去重。
func TestCrossEventEchoes_Rashomon(t *testing.T) {
	ctx := context.Background()
	_, _, service := newBloodFeudTestService(t)

	// 同一 cross_event_id=evtX，两个不同 owner（来自不同 session）各一条视角化 echo。
	service.writeCrossEventEcho(ctx, "sA", "ownerA", "evtX", 0.6, 0.6, "pending", "在 A 看来：是 B 背信弃义。", -0.7, 0)
	service.writeCrossEventEcho(ctx, "sB", "ownerB", "evtX", 0.4, 0.4, "highlight", "在 B 看来：不过是各为其主。", -0.3, 1)
	// 幂等：同一 (cross_event_id|session|owner) 重写应更新而非新增行。
	service.writeCrossEventEcho(ctx, "sA", "ownerA", "evtX", 0.65, 0.65, "pending", "在 A 看来：是 B 背信弃义。", -0.7, 0)

	echoes, err := service.ListCrossEventEchoes(ctx, "evtX")
	if err != nil {
		t.Fatalf("列 echo 失败: %v", err)
	}
	if len(echoes) != 2 {
		t.Fatalf("同一 cross_event_id 应有 2 条不同视角 echo（罗生门，幂等去重后），得 %d", len(echoes))
	}
	// 排序 (hop 升序)：ownerA(hop0) 在前，ownerB(hop1) 在后。
	if echoes[0].OwnerUnitID != "ownerA" || echoes[1].OwnerUnitID != "ownerB" {
		t.Fatalf("echo 应按 hop 升序：ownerA(hop0),ownerB(hop1)，得 %q,%q", echoes[0].OwnerUnitID, echoes[1].OwnerUnitID)
	}
	// 事实唯一：两条 echo 叙事不同（罗生门），但 cross_event_id 恒同。
	if echoes[0].NarrativeZH == echoes[1].NarrativeZH {
		t.Fatalf("两视角叙事应不同（罗生门）")
	}
	if echoes[0].CrossEventID != echoes[1].CrossEventID {
		t.Fatalf("两条 echo 必须绑同一 cross_event_id（事实唯一），得 %q vs %q", echoes[0].CrossEventID, echoes[1].CrossEventID)
	}
	// 幂等：ownerA 的 relevance 被更新为 0.65（最后一次写），非堆叠两行。
	if math.Abs(echoes[0].Relevance-0.65) > 1e-9 {
		t.Fatalf("幂等重写应更新 relevance 为 0.65，得 %.4f", echoes[0].Relevance)
	}
}

// TestSurfaceBetrayalVictimCard_VictimSideOnly 验证黑吃黑受害者卡：
//
//	① 受害者本侧关系恶化（victim 对 betrayer trust- / rivalry+）；
//	② 生成 debt_grudge_love 锚；
//	③ 受害者命运收件箱进一张 CROSS_BETRAYAL 回应卡；
//	④ **永不直写背叛者一侧**（betrayer→victim 无关系行被本路径写入）。
func TestSurfaceBetrayalVictimCard_VictimSideOnly(t *testing.T) {
	ctx := context.Background()
	db, repo, service := newBloodFeudTestService(t)
	for i, id := range []string{"victim", "betrayer"} {
		seedUnit(t, ctx, repo, int64(i)+1, id)
	}

	victim, err := repo.GetByID(ctx, "victim")
	if err != nil {
		t.Fatalf("读受害者失败: %v", err)
	}
	service.surfaceBetrayalVictimCard(ctx, "s1", victim, "betrayer", "xevt1", "她信过的人，临头反咬了一口。")

	// ① 受害者本侧关系恶化：victim→betrayer 应有 trust<0、rivalry>0 的关系行。
	var trust, rivalry float64
	if err := db.QueryRowContext(ctx,
		`SELECT trust, rivalry FROM relations WHERE source_unit_id='victim' AND target_unit_id='betrayer'`).
		Scan(&trust, &rivalry); err != nil {
		t.Fatalf("受害者本侧关系行应存在: %v", err)
	}
	if !(trust < 0 && rivalry > 0) {
		t.Fatalf("受害者对背叛者应 trust<0 且 rivalry>0，得 trust=%.2f rivalry=%.2f", trust, rivalry)
	}

	// ④ 永不直写背叛者一侧：betrayer→victim 关系行不应被本路径创建（跨玩家硬不变量）。
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relations WHERE source_unit_id='betrayer' AND target_unit_id='victim'`).Scan(&n); err != nil {
		t.Fatalf("查背叛者侧关系失败: %v", err)
	}
	if n != 0 {
		t.Fatalf("绝不直写背叛者一侧关系（跨玩家硬不变量），却发现 %d 行", n)
	}

	// ② debt_grudge_love 锚：victim 对 betrayer 应落一根 debt_grudge_love 锚。
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM relevance_anchors WHERE character_unit_id='victim' AND anchor_kind='debt_grudge_love' AND anchor_ref='betrayer'`).Scan(&n); err != nil {
		t.Fatalf("查 debt 锚失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("受害者应落 1 根 debt_grudge_love 锚，得 %d", n)
	}

	// ③ 受害者命运层进一张 CROSS_BETRAYAL 回应卡（高重要度 → 待决策入收件箱）。
	inbox, err := service.OpenFateInbox(ctx, "victim")
	if err != nil {
		t.Fatalf("打开命运收件箱失败: %v", err)
	}
	if len(inbox) == 0 {
		t.Fatalf("受害者应收到一张黑吃黑回应待决策卡")
	}
	if inbox[0].Narrative == "" {
		t.Fatalf("回应卡应有叙事文案")
	}
}

// TestBindBloodFeudAlliance_SocialObject 验证 blood_feud 社会客体生成：受害者 + 密友被绑成对抗背叛者的同盟
// （social_objects kind=blood_feud），成员含 victim 与 friends，且确定性 id 幂等（重复绑定不新建客体）。
func TestBindBloodFeudAlliance_SocialObject(t *testing.T) {
	ctx := context.Background()
	db, repo, service := newBloodFeudTestService(t)
	seedFeudWorld(t, ctx, db, repo)

	worldID := "w1"
	service.bindBloodFeudAlliance(ctx, worldID, "xevt1", "victim", "betrayer", []string{"f1", "f2"})

	// 应有一个 kind=blood_feud 的社会客体。
	objs, err := socialobject.ListByWorld(ctx, db, worldID)
	if err != nil {
		t.Fatalf("列社会客体失败: %v", err)
	}
	var feudObj *socialobject.SocialObject
	for i := range objs {
		if objs[i].Kind == bloodFeudSocialObjectKind {
			feudObj = &objs[i]
			break
		}
	}
	if feudObj == nil {
		t.Fatalf("应生成一个 kind=blood_feud 的社会客体，得 %d 个客体（无 blood_feud）", len(objs))
	}

	// 成员应含 victim + f1 + f2，且不含 betrayer。
	members, err := socialobject.ListMembers(ctx, db, feudObj.ID)
	if err != nil {
		t.Fatalf("列成员失败: %v", err)
	}
	memSet := map[string]bool{}
	for _, m := range members {
		memSet[m.UnitID] = true
	}
	if !(memSet["victim"] && memSet["f1"] && memSet["f2"]) {
		t.Fatalf("同盟成员应含 victim/f1/f2，得 %v", memSet)
	}
	if memSet["betrayer"] {
		t.Fatalf("背叛者不应入对抗自己的同盟")
	}

	// 幂等：同一 (worldID|crossEventID|betrayer) 重绑应复用同一客体 id（不新建第二个 blood_feud 客体）。
	service.bindBloodFeudAlliance(ctx, worldID, "xevt1", "victim", "betrayer", []string{"f1", "f2"})
	objs2, _ := socialobject.ListByWorld(ctx, db, worldID)
	feudCount := 0
	for i := range objs2 {
		if objs2[i].Kind == bloodFeudSocialObjectKind {
			feudCount++
		}
	}
	if feudCount != 1 {
		t.Fatalf("同一桩背叛应幂等复用单一 blood_feud 客体，得 %d 个", feudCount)
	}

	// 留痕：成员应有 SOCIAL_OBJECT_BIND 流程事件。
	var bindN int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE reason_code=? AND actor_unit_id IN ('victim','f1','f2')`,
		string(events.ReasonSocialObjectBind)).Scan(&bindN); err != nil {
		t.Fatalf("查 bind 留痕失败: %v", err)
	}
	if bindN == 0 {
		t.Fatalf("应有 SOCIAL_OBJECT_BIND 留痕事件")
	}

	// worldID 空时 no-op（无世界域不绑同盟）。
	service.bindBloodFeudAlliance(ctx, "", "xevt2", "victim", "betrayer", []string{"f1", "f2"})
	// 只受害者一人（无密友）时 no-op（不成同盟）。
	service.bindBloodFeudAlliance(ctx, worldID, "xevt3", "victim", "betrayer", nil)
	objs3, _ := socialobject.ListByWorld(ctx, db, worldID)
	feudCount3 := 0
	for i := range objs3 {
		if objs3[i].Kind == bloodFeudSocialObjectKind {
			feudCount3++
		}
	}
	if feudCount3 != 1 {
		t.Fatalf("worldID 空 / 无密友应 no-op，blood_feud 客体仍应只有 1 个，得 %d", feudCount3)
	}
}

// TestPropagateCrossBetrayal_FlagOffNoOp 验证 flag 关时 PropagateCrossBetrayal 整段 no-op（返回 0，不触碰 DB）。
func TestPropagateCrossBetrayal_FlagOffNoOp(t *testing.T) {
	orig := os.Getenv("QUNXIANG_BLOOD_FEUD")
	defer os.Setenv("QUNXIANG_BLOOD_FEUD", orig)

	svc := &Service{} // db 为 nil
	victim := unit.Record{ID: "victim"}
	ctx := context.Background()

	os.Setenv("QUNXIANG_BLOOD_FEUD", "false")
	if n := svc.PropagateCrossBetrayal(ctx, "s1", "w1", "evt", victim, "betrayer", ""); n != 0 {
		t.Fatalf("flag 关时应 no-op 返回 0，得 %d", n)
	}
	os.Setenv("QUNXIANG_BLOOD_FEUD", "true")
	// flag 开但 db=nil：在守卫处安全返回 0，仍不 panic。
	if n := svc.PropagateCrossBetrayal(ctx, "s1", "w1", "evt", victim, "betrayer", ""); n != 0 {
		t.Fatalf("db=nil 时应安全返回 0，得 %d", n)
	}
}
