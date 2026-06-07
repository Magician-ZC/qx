package status_test

// 文件说明：ApplyBatch（批量状态变更）的集成测试，对真实 SQLite 跑通，验证：
// 与逐次 Apply 语义等价、结果按原序对齐、单事务事件落盘、未知原因码整批拒绝且零副作用。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

func newTestDB(t *testing.T) (*sql.DB, *unit.Repository, *status.Mutator) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	return db, repo, status.NewMutator(db, repo)
}

func seedUnit(t *testing.T, repo *unit.Repository, seed int64, name string) unit.Record {
	t.Helper()
	rec := unit.BootstrapRecord(seed, "sess1", "player", name)
	if err := repo.Save(context.Background(), rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
	return rec
}

func countEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		t.Fatalf("统计 events 失败: %v", err)
	}
	return n
}

func TestApplyBatch_BasicAndEventCount(t *testing.T) {
	db, repo, mut := newTestDB(t)
	u1 := seedUnit(t, repo, 2, "甲") // HP100 Hunger100 Morale0.7
	u2 := seedUnit(t, repo, 4, "乙")

	muts := []status.Mutation{
		{UnitID: u1.ID, Field: status.FieldHP, Delta: -10, ReasonCode: events.ReasonCombatHit},
		{UnitID: u1.ID, Field: status.FieldHunger, Delta: -5, ReasonCode: events.ReasonSurvivalHunger},
		{UnitID: u2.ID, Field: status.FieldMorale, Delta: 0.1, ReasonCode: events.ReasonEmotionReward},
	}
	results, err := mut.ApplyBatch(context.Background(), muts)
	if err != nil {
		t.Fatalf("ApplyBatch 出错: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("期望 3 条结果，得到 %d", len(results))
	}
	if results[0].Payload.Before != 100 || results[0].Payload.After != 90 {
		t.Fatalf("u1 HP 期望 100→90，得到 %v→%v", results[0].Payload.Before, results[0].Payload.After)
	}
	if results[1].Payload.Before != 100 || results[1].Payload.After != 95 {
		t.Fatalf("u1 Hunger 期望 100→95，得到 %v→%v", results[1].Payload.Before, results[1].Payload.After)
	}

	r1, _ := repo.GetByID(context.Background(), u1.ID)
	if r1.Status.HP != 90 || r1.Status.Hunger != 95 {
		t.Fatalf("u1 落库状态错误：HP=%d Hunger=%d", r1.Status.HP, r1.Status.Hunger)
	}
	r2, _ := repo.GetByID(context.Background(), u2.ID)
	if r2.Status.Morale != 0.8 {
		t.Fatalf("u2 Morale 期望 0.8，得到 %v", r2.Status.Morale)
	}

	if got := countEvents(t, db); got != 3 {
		t.Fatalf("期望落盘 3 条事件，得到 %d", got)
	}
}

// 等价性：ApplyBatch 与逐次 Apply 产生相同最终状态。
func TestApplyBatch_EquivalentToSequentialApply(t *testing.T) {
	_, repoA, mutA := newTestDB(t)
	_, repoB, mutB := newTestDB(t)
	ua := seedUnit(t, repoA, 6, "同")
	ub := seedUnit(t, repoB, 6, "同")

	makeMuts := func(id string) []status.Mutation {
		return []status.Mutation{
			{UnitID: id, Field: status.FieldHP, Delta: -10, ReasonCode: events.ReasonCombatHit},
			{UnitID: id, Field: status.FieldHP, Delta: -7, ReasonCode: events.ReasonCombatHit},
			{UnitID: id, Field: status.FieldHunger, Delta: -20, ReasonCode: events.ReasonSurvivalHunger},
			{UnitID: id, Field: status.FieldWallet, Delta: 50, ReasonCode: events.ReasonEconomyReward},
			{UnitID: id, Field: status.FieldMorale, Delta: 0.15, ReasonCode: events.ReasonEmotionReward},
		}
	}

	if _, err := mutA.ApplyBatch(context.Background(), makeMuts(ua.ID)); err != nil {
		t.Fatalf("ApplyBatch 出错: %v", err)
	}
	for _, m := range makeMuts(ub.ID) {
		if _, err := mutB.Apply(context.Background(), m); err != nil {
			t.Fatalf("Apply 出错: %v", err)
		}
	}

	ra, _ := repoA.GetByID(context.Background(), ua.ID)
	rb, _ := repoB.GetByID(context.Background(), ub.ID)
	if ra.Status.HP != rb.Status.HP || ra.Status.Hunger != rb.Status.Hunger ||
		ra.Status.Wallet != rb.Status.Wallet || ra.Status.Morale != rb.Status.Morale {
		t.Fatalf("ApplyBatch 与逐次 Apply 最终状态应一致：\n batch=%+v\n apply=%+v", ra.Status, rb.Status)
	}
	// 预期：HP 100-17=83，Hunger 100-20=80，Wallet 100+50=150，Morale 0.7+0.15=0.85
	if ra.Status.HP != 83 || ra.Status.Hunger != 80 || ra.Status.Wallet != 150 || ra.Status.Morale != 0.85 {
		t.Fatalf("最终状态数值不符预期：%+v", ra.Status)
	}
}

// 未知原因码：整批拒绝，且不产生任何副作用（事件零落盘、状态不变）。
func TestApplyBatch_UnknownReasonCodeRejectsAtomically(t *testing.T) {
	db, repo, mut := newTestDB(t)
	u := seedUnit(t, repo, 8, "丙")
	muts := []status.Mutation{
		{UnitID: u.ID, Field: status.FieldHP, Delta: -10, ReasonCode: events.ReasonCombatHit},
		{UnitID: u.ID, Field: status.FieldHP, Delta: -10, ReasonCode: events.ReasonCode("NO_SUCH_CODE")},
	}
	if _, err := mut.ApplyBatch(context.Background(), muts); err == nil {
		t.Fatalf("含未知原因码应整批返回错误")
	}
	if got := countEvents(t, db); got != 0 {
		t.Fatalf("整批拒绝应零副作用，但落盘了 %d 条事件", got)
	}
	r, _ := repo.GetByID(context.Background(), u.ID)
	if r.Status.HP != 100 {
		t.Fatalf("整批拒绝不应改动状态，HP=%d", r.Status.HP)
	}
}

func TestApplyBatch_Empty(t *testing.T) {
	_, _, mut := newTestDB(t)
	results, err := mut.ApplyBatch(context.Background(), nil)
	if err != nil || results != nil {
		t.Fatalf("空入参应返回 (nil,nil)，得到 results=%v err=%v", results, err)
	}
}
