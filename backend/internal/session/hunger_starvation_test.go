package session

// 文件说明：断粮致死收口测试（对真实 SQLite）——验证 applyTurnHungerUpkeep 把「饥饿归零致死」
// 统一走 status.Mutator + unit.ApplyFatalDamage：致死留痕（RecentEventIDs / events 落盘），
// 且生命状态机的多命语义被尊重（末命 -> Dead，多命 -> Down，绝不强制永久击杀）。

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/engine/status"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/unit"
)

// newHungerStarvationService 起一个临时 SQLite + 最小 Service（无 LLM，饥饿结算不触达 LLM）。
func newHungerStarvationService(t *testing.T) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hunger_starvation.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := unit.NewRepository(db)
	service := &Service{db: db, units: repo, mutator: status.NewMutator(db, repo)}
	return db, repo, service
}

// hungerStarvationHasHPEvent 检查 events 表里是否存在一条由本单位发起、改 hp 字段的事件，
// 用来证明断粮致死时 HP 归零确实经过了 status.Mutator（而非直写绕过）。
func hungerStarvationHasHPEvent(t *testing.T, db *sql.DB, unitID string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE actor_unit_id = ? AND payload_json LIKE '%"field":"hp"%'`,
		unitID,
	).Scan(&n); err != nil {
		t.Fatalf("查询 hp 事件失败: %v", err)
	}
	return n > 0
}

// TestHungerStarvation_LastLifeDiesThroughMutator 验证：末命单位断粮归零时死亡，
// 且死亡经 Mutator 留痕——HP=0 / LivesRemaining=0 / LifeState=Dead，RecentEventIDs 非空、events 表有 hp 事件。
func TestHungerStarvation_LastLifeDiesThroughMutator(t *testing.T) {
	db, repo, service := newHungerStarvationService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(2, "hs_sess", "player", "断粮者")
	actor.Status.Hunger = 0         // 已断粮
	actor.Status.HP = 100           // 起始满血，强制走「Mutator 把 HP 归零」分支
	actor.Status.LivesRemaining = 1 // 末命：致死即永久死亡
	actor.Status.LivesMax = 1
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}

	state := &State{ID: "hs_sess"}
	state.TurnState.Turn = 7

	if err := service.applyTurnHungerUpkeep(ctx, state, nil); err != nil {
		t.Fatalf("applyTurnHungerUpkeep 出错: %v", err)
	}

	got, err := repo.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("读回单位失败: %v", err)
	}

	if got.Status.LifeState != unit.LifeStateDead {
		t.Fatalf("末命断粮应永久死亡：LifeState=%q（期望 %q）", got.Status.LifeState, unit.LifeStateDead)
	}
	if got.Status.HP != 0 {
		t.Fatalf("致死后 HP 应为 0，实际 %d", got.Status.HP)
	}
	if got.Status.LivesRemaining != 0 {
		t.Fatalf("末命致死后剩余命数应为 0，实际 %d", got.Status.LivesRemaining)
	}
	if len(got.Memory.RecentEventIDs) == 0 {
		t.Fatalf("致死必须留痕：RecentEventIDs 不应为空")
	}
	if !hungerStarvationHasHPEvent(t, db, actor.ID) {
		t.Fatalf("致死时 HP 归零应经 status.Mutator 落一条 hp 事件，但 events 表未见")
	}
}

// TestHungerStarvation_MultiLifeGoesDownNotPermaKilled 验证：多命单位断粮归零时
// 只损一命转入 Down（unit.ApplyFatalDamage 语义），不再被旧的直写强制永久击杀。
func TestHungerStarvation_MultiLifeGoesDownNotPermaKilled(t *testing.T) {
	_, repo, service := newHungerStarvationService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(4, "hs_sess2", "player", "多命者")
	actor.Status.Hunger = 0
	actor.Status.HP = 100
	actor.Status.LivesRemaining = 3
	actor.Status.LivesMax = 3
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}

	state := &State{ID: "hs_sess2"}
	state.TurnState.Turn = 3

	if err := service.applyTurnHungerUpkeep(ctx, state, nil); err != nil {
		t.Fatalf("applyTurnHungerUpkeep 出错: %v", err)
	}

	got, err := repo.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("读回单位失败: %v", err)
	}

	if got.Status.LifeState == unit.LifeStateDead {
		t.Fatalf("多命单位断粮不应被强制永久击杀，LifeState=%q", got.Status.LifeState)
	}
	if got.Status.LifeState != unit.LifeStateDown {
		t.Fatalf("多命单位断粮致死应转入 Down，实际 %q", got.Status.LifeState)
	}
	if got.Status.LivesRemaining != 2 {
		t.Fatalf("多命单位断粮致死应只损一命（3->2），实际剩余 %d", got.Status.LivesRemaining)
	}
	if got.Status.HP != 0 {
		t.Fatalf("致死后 HP 应为 0，实际 %d", got.Status.HP)
	}
	if len(got.Memory.RecentEventIDs) == 0 {
		t.Fatalf("致死必须留痕：RecentEventIDs 不应为空")
	}
}

// TestHungerStarvation_FedUnitSurvives 反向哨兵：饥饿充足的单位走完 upkeep 不应死亡，
// 防止收口改动把正常存活单位误判致死。
func TestHungerStarvation_FedUnitSurvives(t *testing.T) {
	_, repo, service := newHungerStarvationService(t)
	ctx := context.Background()

	actor := unit.BootstrapRecord(6, "hs_sess3", "player", "饱腹者")
	actor.Status.Hunger = 90
	actor.Status.HP = 100
	actor.Status.LivesRemaining = 1
	actor.Status.LivesMax = 1
	if err := repo.Save(ctx, actor); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}

	state := &State{ID: "hs_sess3"}
	state.TurnState.Turn = 1

	if err := service.applyTurnHungerUpkeep(ctx, state, nil); err != nil {
		t.Fatalf("applyTurnHungerUpkeep 出错: %v", err)
	}

	got, err := repo.GetByID(ctx, actor.ID)
	if err != nil {
		t.Fatalf("读回单位失败: %v", err)
	}
	if got.Status.LifeState != unit.LifeStateActive {
		t.Fatalf("饱腹单位不应死亡，LifeState=%q", got.Status.LifeState)
	}
	if got.Status.HP <= 0 {
		t.Fatalf("饱腹单位 HP 不应归零，实际 %d", got.Status.HP)
	}
}
