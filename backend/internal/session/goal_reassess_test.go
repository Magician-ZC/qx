package session

// 文件说明：goal_reassess.go 与 ambition.go 的回归测试。
//   ① ambitionBias —— 纯函数确定性/单调性/标签映射/Dominant 平手稳定；
//   ② reassessGoalIfDue —— 周期门控（off-cadence no-op）、到点 fallback 落「高显著度记忆」、空野心兜底文案。
// 用 goalReassess_ 前缀避免与既有 helper 撞名；DB 集成测试复用 newThreatTestService（service.llm==nil → 走规则 fallback，确定性）。

import (
	"context"
	"database/sql"
	"testing"

	"qunxiang/backend/internal/unit"
)

// ── ① ambitionBias 纯函数 ──────────────────────────────────────────────

// TestGoalReassess_AmbitionBiasZeroIsNeutral 全 0 野心（旧存档/无野心）→ 空引力表，BiasFor 一律返回 floor（1.0 中性）。
func TestGoalReassess_AmbitionBiasZeroIsNeutral(t *testing.T) {
	bias := ambitionBias(unit.Ambition{})
	if len(bias) != 0 {
		t.Fatalf("全 0 野心应得空引力表，实际 %v", bias)
	}
	for _, tag := range []string{"conquer", "revenge", "hoard", "nurture", "train", "explore", "bond", "不存在"} {
		if got := bias.BiasFor(tag); got != ambitionFloor {
			t.Fatalf("空引力表 BiasFor(%q)=%v，应为中性 floor %v", tag, got, ambitionFloor)
		}
	}
	tag, w := bias.Dominant()
	if tag != "" || w != ambitionFloor {
		t.Fatalf("空引力表 Dominant 应为 (\"\", floor)，实际 (%q,%v)", tag, w)
	}
}

// TestGoalReassess_AmbitionBiasDeterministic 同输入恒同输出（无随机/无时间依赖）。
func TestGoalReassess_AmbitionBiasDeterministic(t *testing.T) {
	amb := unit.Ambition{Power: 0.8, Vengeance: 0.3, Wealth: 0.6, Mastery: 0.9}
	a := ambitionBias(amb)
	b := ambitionBias(amb)
	if len(a) != len(b) {
		t.Fatalf("两次调用长度不同：%d vs %d", len(a), len(b))
	}
	for k, v := range a {
		if b[k] != v {
			t.Fatalf("两次调用 %q 不一致：%v vs %v", k, v, b[k])
		}
	}
}

// TestGoalReassess_AmbitionBiasMonotonic 引力对对应野心分量单调不减；分量越大引力越大，且落在 [floor, floor+gain]。
func TestGoalReassess_AmbitionBiasMonotonic(t *testing.T) {
	prev := -1.0
	for _, v := range []float64{0.1, 0.3, 0.5, 0.7, 1.0} {
		bias := ambitionBias(unit.Ambition{Wealth: v})
		w := bias.BiasFor("hoard")
		if w+1e-12 < prev {
			t.Fatalf("引力应随野心单调不减：wealth=%v hoard=%v < prev=%v", v, w, prev)
		}
		if w < ambitionFloor || w > ambitionFloor+ambitionGain+1e-9 {
			t.Fatalf("引力应落在 [%v,%v]，实际 %v", ambitionFloor, ambitionFloor+ambitionGain, w)
		}
		prev = w
	}
	// 满野心恰好到上限。
	if got := ambitionBias(unit.Ambition{Wealth: 1}).BiasFor("hoard"); got != ambitionFloor+ambitionGain {
		t.Fatalf("wealth=1 时 hoard 引力应=%v，实际 %v", ambitionFloor+ambitionGain, got)
	}
}

// TestGoalReassess_AmbitionBiasMultiDriverTakesMax 同一标签被多维驱动时取最大引力（不叠加越界）。
// "bond" 同时被 power 与 lineage 驱动：取两者中更强的一维。
func TestGoalReassess_AmbitionBiasMultiDriverTakesMax(t *testing.T) {
	bias := ambitionBias(unit.Ambition{Power: 0.2, Lineage: 0.9})
	wantBond := ambitionFloor + ambitionGain*0.9 // 取 lineage(0.9) 这一更强驱动
	if got := bias.BiasFor("bond"); absFloat(got-wantBond) > 1e-9 {
		t.Fatalf("bond 应取最强驱动 lineage：期望 %v，实际 %v", wantBond, got)
	}
	// power 也驱动 conquer，lineage 驱动 nurture，各自独立。
	if got := bias.BiasFor("conquer"); absFloat(got-(ambitionFloor+ambitionGain*0.2)) > 1e-9 {
		t.Fatalf("conquer 应由 power(0.2) 驱动，实际 %v", got)
	}
}

// TestGoalReassess_AmbitionDominant 取引力最高标签；平手取字典序最小，确定性。
func TestGoalReassess_AmbitionDominant(t *testing.T) {
	// mastery 最高 → 主导标签应在 {train, explore} 中（两者同引力，字典序取 "explore"）。
	bias := ambitionBias(unit.Ambition{Power: 0.3, Mastery: 0.9})
	tag, w := bias.Dominant()
	if tag != "explore" {
		t.Fatalf("mastery 最高且 train/explore 平手应取字典序最小 'explore'，实际 %q", tag)
	}
	if absFloat(w-(ambitionFloor+ambitionGain*0.9)) > 1e-9 {
		t.Fatalf("主导引力应=%v，实际 %v", ambitionFloor+ambitionGain*0.9, w)
	}
}

// TestGoalReassess_AmbitionBiasOfReadsRecord AmbitionBiasOf 从 Record.Ambition 取向量。
func TestGoalReassess_AmbitionBiasOfReadsRecord(t *testing.T) {
	rec := unit.Record{Ambition: unit.Ambition{Vengeance: 1.0}}
	if got := AmbitionBiasOf(rec).BiasFor("revenge"); got != ambitionFloor+ambitionGain {
		t.Fatalf("AmbitionBiasOf 应读 Record.Ambition：revenge 引力期望 %v，实际 %v", ambitionFloor+ambitionGain, got)
	}
}

// ── ② reassessGoalIfDue 周期门控 + fallback 落记忆 ──────────────────────

// TestGoalReassess_NotDueIsNoop 非到点（turn 非 cadence 整数倍 / turn<=0）→ no-op，不写记忆。
func TestGoalReassess_NotDueIsNoop(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(7, "s1", "player", "阿采")
	actor.Ambition = unit.Ambition{Wealth: 0.9}
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}

	for _, turn := range []int{0, 1, 12, 23, 25, goalReassessCadence - 1} {
		if err := service.reassessGoalIfDue(ctx, &state, &actor, turn); err != nil {
			t.Fatalf("turn=%d reassess: %v", turn, err)
		}
	}
	if n := memoryRowCount(t, db, actor.ID); n != 0 {
		t.Fatalf("非到点不应写记忆，实际写了 %d 条", n)
	}
}

// TestGoalReassess_DueWritesHighSalienceMemory 到点（cadence 整数倍）→ 经 fallback 写一条目标记忆，source 标 GOAL_REASSESS、内容贴合野心。
func TestGoalReassess_DueWritesHighSalienceMemory(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(11, "s1", "player", "阿采")
	actor.Ambition = unit.Ambition{Wealth: 0.95} // 主导 hoard → fallback 应是「囤钱粮」模板
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}

	if err := service.reassessGoalIfDue(ctx, &state, &actor, goalReassessCadence); err != nil {
		t.Fatalf("reassess: %v", err)
	}
	if n := memoryRowCount(t, db, actor.ID); n != 1 {
		t.Fatalf("到点应写 1 条目标记忆，实际 %d 条", n)
	}
	summary, source := latestMemory(t, db, actor.ID)
	if source != goalReassessSource {
		t.Fatalf("记忆 source 应为 %q，实际 %q", goalReassessSource, source)
	}
	if summary == "" {
		t.Fatalf("目标记忆不应为空")
	}
	// fallback 模板：wealth 主导 → 含「钱粮」字样（goalReassessFallbackTemplates["hoard"]）。
	if !containsAny(summary, "钱粮", "囤") {
		t.Fatalf("wealth 主导的 fallback 目标应含囤钱粮意涵，实际 %q", summary)
	}
	// 经 importanceBoost 应高显著度：metadata importance 应被抬高（>= 默认 event 基准 4）。
	if imp := latestMemoryImportance(t, db, actor.ID); imp < 4+1 {
		t.Fatalf("目标记忆应高显著度（importance 经 boost 抬高），实际 importance=%d", imp)
	}
}

// TestGoalReassess_NoAmbitionFallbackIsGeneric 全 0 野心 → 走通用兜底目标文案（仍写一条记忆，绝不中断）。
func TestGoalReassess_NoAmbitionFallbackIsGeneric(t *testing.T) {
	db, repo, service := newThreatTestService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(13, "s1", "player", "无名")
	// 不设 Ambition → 全 0。
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("save: %v", err)
	}
	state := State{ID: "s1"}

	if err := service.reassessGoalIfDue(ctx, &state, &actor, goalReassessCadence*2); err != nil {
		t.Fatalf("reassess: %v", err)
	}
	if n := memoryRowCount(t, db, actor.ID); n != 1 {
		t.Fatalf("到点应写 1 条目标记忆，实际 %d 条", n)
	}
	summary, _ := latestMemory(t, db, actor.ID)
	if summary == "" {
		t.Fatalf("无野心兜底也应产出非空目标")
	}
}

// TestGoalReassess_NilGuards nil/边界守卫安全返回。
func TestGoalReassess_NilGuards(t *testing.T) {
	_, _, service := newThreatTestService(t)
	ctx := context.Background()
	state := State{ID: "s1"}
	rec := unit.BootstrapRecord(1, "s1", "player", "x")

	if err := service.reassessGoalIfDue(ctx, nil, &rec, goalReassessCadence); err != nil {
		t.Fatalf("nil state 应安全 no-op，实际 %v", err)
	}
	if err := service.reassessGoalIfDue(ctx, &state, nil, goalReassessCadence); err != nil {
		t.Fatalf("nil record 应安全 no-op，实际 %v", err)
	}
	var nilService *Service
	if err := nilService.reassessGoalIfDue(ctx, &state, &rec, goalReassessCadence); err != nil {
		t.Fatalf("nil service 应安全 no-op，实际 %v", err)
	}
}

// ── 测试小工具 ────────────────────────────────────────────────────────

func memoryRowCount(t *testing.T, db *sql.DB, unitID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE unit_id = ?`, unitID).Scan(&n); err != nil {
		t.Fatalf("统计 memories 失败: %v", err)
	}
	return n
}

func latestMemory(t *testing.T, db *sql.DB, unitID string) (summary string, source string) {
	t.Helper()
	var metaJSON string
	if err := db.QueryRow(
		`SELECT summary, metadata_json FROM memories WHERE unit_id = ? ORDER BY created_at DESC LIMIT 1`,
		unitID,
	).Scan(&summary, &metaJSON); err != nil {
		t.Fatalf("读取最近 memory 失败: %v", err)
	}
	return summary, decodeMemoryMetadata(metaJSON).Source
}

func latestMemoryImportance(t *testing.T, db *sql.DB, unitID string) int {
	t.Helper()
	var metaJSON string
	if err := db.QueryRow(
		`SELECT metadata_json FROM memories WHERE unit_id = ? ORDER BY created_at DESC LIMIT 1`,
		unitID,
	).Scan(&metaJSON); err != nil {
		t.Fatalf("读取最近 memory metadata 失败: %v", err)
	}
	return decodeMemoryMetadata(metaJSON).Importance
}
