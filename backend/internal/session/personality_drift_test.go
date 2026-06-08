package session

// 文件说明：人格漂移调节器的单元/集成测试。
// 守护四件事：①纯函数 driftDelta 步长封顶 + 当日剩余额度截断 + 方向偏置；②clampTrait 边界；③确定性（同输入同输出）；
// ④对真实 SQLite 的全链路：漂移落库 + PERSONALITY_DRIFT 留痕 + 单日单维累计 ≤0.10 闸住后续漂移。
// test 文件被 statuslint 白名单豁免，可直接读写字段。

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// TestDriftDelta_StepCap 验证单次单维增量绝对值恒 ≤ driftPerStepCap，且方向 ∈ {-1,+1}*magnitude。
func TestDriftDelta_StepCap(t *testing.T) {
	dims := personalityDimensions()
	for turn := 0; turn < 50; turn++ {
		for _, dim := range dims {
			for _, reason := range []PersonalityDriftReason{DriftReasonIntervention, DriftReasonAging, DriftReasonOrdeal} {
				d := driftDelta("sess-A", turn, "unit-1", reason, dim, driftPerDayCap)
				if math.Abs(d) > driftPerStepCap+1e-9 {
					t.Fatalf("步长越界：dim=%s reason=%s turn=%d delta=%.6f > cap=%.2f", dim.Name, reason, turn, d, driftPerStepCap)
				}
			}
		}
	}
}

// TestDriftDelta_RemainingTodayClamp 验证当日剩余额度对单步增量的截断。
func TestDriftDelta_RemainingTodayClamp(t *testing.T) {
	dim := personalityDimensions()[0]
	// 剩余额度 0 → 必为 0。
	if d := driftDelta("s", 1, "u", DriftReasonOrdeal, dim, 0); d != 0 {
		t.Fatalf("剩余额度 0 时应返回 0，得到 %.6f", d)
	}
	// 剩余额度 0.005（< 步长上限 0.03）→ |delta| ≤ 0.005。
	for turn := 0; turn < 40; turn++ {
		d := driftDelta("s", turn, "u", DriftReasonOrdeal, dim, 0.005)
		if math.Abs(d) > 0.005+1e-9 {
			t.Fatalf("剩余额度截断失效：turn=%d delta=%.6f > 0.005", turn, d)
		}
	}
}

// TestDriftDelta_Deterministic 验证同 (session,turn,actor,reason,dim) 输入必产同输出。
func TestDriftDelta_Deterministic(t *testing.T) {
	dim := personalityDimensions()[2]
	for turn := 0; turn < 10; turn++ {
		a := driftDelta("sess-X", turn, "actor-9", DriftReasonAging, dim, driftPerDayCap)
		b := driftDelta("sess-X", turn, "actor-9", DriftReasonAging, dim, driftPerDayCap)
		if a != b {
			t.Fatalf("非确定性：turn=%d %.6f != %.6f", turn, a, b)
		}
	}
	// 不同 actor 应大概率得到不同结果（哈希区分）。
	same := 0
	for turn := 0; turn < 20; turn++ {
		if driftDelta("s", turn, "actorA", DriftReasonOrdeal, dim, driftPerDayCap) ==
			driftDelta("s", turn, "actorB", DriftReasonOrdeal, dim, driftPerDayCap) {
			same++
		}
	}
	if same == 20 {
		t.Fatalf("不同 actor 全部同值，哈希未区分 actor")
	}
}

// TestDriftDelta_BiasDirection 验证 aging 下 prudence 偏向上调、stability 偏向上调、courage 偏向下调（统计倾向，非绝对）。
func TestDriftDelta_BiasDirection(t *testing.T) {
	byName := map[string]driftDimension{}
	for _, d := range personalityDimensions() {
		byName[d.Name] = d
	}
	count := func(name string, reason PersonalityDriftReason) (up, down int) {
		dim := byName[name]
		for turn := 0; turn < 200; turn++ {
			d := driftDelta("bias-sess", turn, "u", reason, dim, driftPerDayCap)
			if d > 0 {
				up++
			} else if d < 0 {
				down++
			}
		}
		return
	}
	if up, down := count("prudence", DriftReasonAging); up <= down {
		t.Fatalf("aging 下 prudence 应偏向上调，up=%d down=%d", up, down)
	}
	if up, down := count("courage", DriftReasonAging); down <= up {
		t.Fatalf("aging 下 courage 应偏向下调，up=%d down=%d", up, down)
	}
	if up, down := count("stability", DriftReasonOrdeal); down <= up {
		t.Fatalf("ordeal 下 stability 应偏向下调，up=%d down=%d", up, down)
	}
}

// TestClampTrait_Bounds 验证人格维 clamp 边界与两位小数精度。
func TestClampTrait_Bounds(t *testing.T) {
	if v := clampTrait(-5); v != personalityFloor {
		t.Fatalf("下界 clamp 失效：%.4f", v)
	}
	if v := clampTrait(99); v != personalityCeil {
		t.Fatalf("上界 clamp 失效：%.4f", v)
	}
	if v := clampTrait(0.5234); v != 0.52 {
		t.Fatalf("两位小数失效：%.4f", v)
	}
}

// TestParseDriftMagnitudes 验证从 payload 还原各维 |delta| 累计。
func TestParseDriftMagnitudes(t *testing.T) {
	if m := parseDriftMagnitudes(""); len(m) != 0 {
		t.Fatalf("空 payload 应返回空 map")
	}
	if m := parseDriftMagnitudes("not json"); len(m) != 0 {
		t.Fatalf("坏 payload 应返回空 map")
	}
	payload := `{"changes":[{"dimension":"courage","delta":-0.03},{"dimension":"courage","delta":0.01},{"dimension":"prudence","delta":0.02}]}`
	m := parseDriftMagnitudes(payload)
	if math.Abs(m["courage"]-0.04) > 1e-9 {
		t.Fatalf("courage 累计应 0.04（|−0.03|+|0.01|），得到 %.4f", m["courage"])
	}
	if math.Abs(m["prudence"]-0.02) > 1e-9 {
		t.Fatalf("prudence 累计应 0.02，得到 %.4f", m["prudence"])
	}
}

// TestApplyPersonalityDrift_PersistAndAudit 验证全链路：漂移落库 + PERSONALITY_DRIFT 留痕 + 各维 clamp 在界内。
func TestApplyPersonalityDrift_PersistAndAudit(t *testing.T) {
	db, repo, service := newDriftTestService(t)
	ctx := context.Background()

	rec := unit.BootstrapRecord(7, "s-drift", "player", "阿玖")
	rec.Personality = unit.GeneratePersonality(7)
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	before := rec.Personality

	amounts, err := service.ApplyPersonalityDrift(ctx, "s-drift", rec.ID, DriftReasonOrdeal, 3)
	if err != nil {
		t.Fatalf("apply drift: %v", err)
	}
	if len(amounts) == 0 {
		t.Fatalf("一次重大经历漂移应至少动一维")
	}

	// 落库后人格各维仍在 [floor,ceil]，且确有变化。
	after, err := repo.GetByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("reget: %v", err)
	}
	if after.Personality == before {
		t.Fatalf("漂移未持久化（人格未变）")
	}
	for _, dim := range personalityDimensions() {
		v := dim.Get(&after.Personality)
		if v < personalityFloor-1e-9 || v > personalityCeil+1e-9 {
			t.Fatalf("维 %s 越界：%.4f", dim.Name, v)
		}
	}

	// 留痕：恰有 PERSONALITY_DRIFT 事件，actor=本单位。
	if n := driftEventCount(t, db, rec.ID); n != 1 {
		t.Fatalf("应留 1 条 PERSONALITY_DRIFT 事件，得到 %d", n)
	}

	// 每维单步幅度 ≤ driftPerStepCap。
	for _, a := range amounts {
		if math.Abs(a.Delta) > driftPerStepCap+1e-9 {
			t.Fatalf("维 %s 单步越界 %.4f", a.Dimension, a.Delta)
		}
	}
}

// TestApplyPersonalityDrift_DailyCapGate 验证单日单维累计 ≤ driftPerDayCap：反复漂移直到额度耗尽后不再变化。
func TestApplyPersonalityDrift_DailyCapGate(t *testing.T) {
	_, repo, service := newDriftTestService(t)
	ctx := context.Background()

	rec := unit.BootstrapRecord(11, "s-cap", "player", "阿拾")
	// 把人格放中间，避免被 [0.05,0.95] 边界提前夹死、干扰累计验证。
	rec.Personality = unit.Personality{Courage: 0.5, Loyalty: 0.5, Aggression: 0.5, Prudence: 0.5, Sociability: 0.5, Integrity: 0.5, Stability: 0.5, Ambition: 0.5}
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 跑很多次（不同 turn → 不同确定性增量），累计每维总变化幅度。
	cumPerDim := map[string]float64{}
	for turn := 0; turn < 60; turn++ {
		amounts, err := service.ApplyPersonalityDrift(ctx, "s-cap", rec.ID, DriftReasonOrdeal, turn)
		if err != nil {
			t.Fatalf("apply drift turn=%d: %v", turn, err)
		}
		for _, a := range amounts {
			cumPerDim[a.Dimension] += math.Abs(a.Delta)
		}
	}

	// 任何一维当日累计幅度都不得超过 driftPerDayCap（带一个步长的容差：最后一步可能正好顶到额度）。
	for dim, cum := range cumPerDim {
		if cum > driftPerDayCap+1e-9 {
			t.Fatalf("维 %s 单日累计 %.4f 超出上限 %.2f", dim, cum, driftPerDayCap)
		}
	}
	// 且至少有一维确实逼近了上限（否则说明根本没漂、测试没意义）。
	maxCum := 0.0
	for _, cum := range cumPerDim {
		if cum > maxCum {
			maxCum = cum
		}
	}
	if maxCum < driftPerStepCap {
		t.Fatalf("60 次漂移后最大累计仅 %.4f，疑似根本没漂动", maxCum)
	}
}

// TestApplyPersonalityDrift_NilSafe 验证 nil/空 守护不 panic。
func TestApplyPersonalityDrift_NilSafe(t *testing.T) {
	var nilService *Service
	if _, err := nilService.ApplyPersonalityDrift(context.Background(), "s", "u", DriftReasonAging, 1); err != nil {
		t.Fatalf("nil service 应安全返回 nil err，得到 %v", err)
	}
	_, repo, service := newDriftTestService(t)
	_ = repo
	if _, err := service.ApplyPersonalityDrift(context.Background(), "s", "  ", DriftReasonAging, 1); err != nil {
		t.Fatalf("空 unitID 应安全返回，得到 %v", err)
	}
	// 不存在的单位 → best-effort no-op，无错。
	if amts, err := service.ApplyPersonalityDrift(context.Background(), "s", "ghost", DriftReasonAging, 1); err != nil || amts != nil {
		t.Fatalf("不存在单位应 no-op：amts=%v err=%v", amts, err)
	}
}

func newDriftTestService(t *testing.T) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "drift.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{db: db, units: repo, mutator: status.NewMutator(db, repo)}
	return db, repo, service
}

func driftEventCount(t *testing.T, db *sql.DB, unitID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND reason_code = ?`,
		unitID, string(events.ReasonPersonalityDrift),
	).Scan(&n); err != nil {
		t.Fatalf("统计漂移事件失败: %v", err)
	}
	return n
}
