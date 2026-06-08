package session

// 文件说明：blood_feud 世仇传播纯函数的单元测试（零 DB）：敌意继承的单调性/跳数衰减/不在乎者归零，
// 以及 flag 开关语义。验证「直系哀悼者敌意 > 远系」「纯敌视者不继承」「flag 关 no-op」三条核心契约。

import (
	"context"
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
