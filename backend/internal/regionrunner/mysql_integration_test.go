package regionrunner

// 文件说明：region-runner 全栈 MySQL 驱动集成验证（M7.3-real-5）。**仅当设置 QUNXIANG_MYSQL_DSN 时运行**，否则 t.Skip——
// 故常规 `go test ./...`（无 MySQL）不受影响，但代码恒被编译检查。覆盖三条 MySQL 专有方言路径（SQLite 之外）：
//   ① agentqueue 唤醒队列 ON DUPLICATE KEY upsert；
//   ② ClaimNextJob 的「内部事务 SELECT…FOR UPDATE + UPDATE」并发认领（行锁防双认领，对照 SQLite 的 UPDATE…RETURNING）；
//   ③ unit Save 的 ON DUPLICATE KEY + SaveOptimistic 的 version 守护条件写（RowsAffected 判冲突）。
// 并跑通 schedulePass→processOne→觅食 全链路，证明引擎在 MySQL 下与 SQLite 行为一致。
//
// 起一个本地容器即可验证：
//   docker run -d --name qx-mysql-test -e MYSQL_ROOT_PASSWORD=root -e MYSQL_DATABASE=qxtest -p 13306:3306 mysql:8.0
//   QUNXIANG_MYSQL_DSN='root:root@tcp(127.0.0.1:13306)/qxtest?parseTime=true' go test -race ./internal/regionrunner/ -run MySQL

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"qunxiang/backend/internal/agentqueue"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	mysqlstore "qunxiang/backend/internal/storage/mysql"
	"qunxiang/backend/internal/unit"
)

func mysqlDSNOrSkip(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("QUNXIANG_MYSQL_DSN")
	if dsn == "" {
		t.Skip("设置 QUNXIANG_MYSQL_DSN 才跑 MySQL 集成验证（real-5）")
	}
	return dsn
}

// openMySQL 打开 MySQL 并清掉本测试命名空间的残留行（容器持久，重跑需自洁）。
func openMySQL(t *testing.T, sessionID string) *sql.DB {
	t.Helper()
	db, err := mysqlstore.Open(mysqlDSNOrSkip(t))
	if err != nil {
		t.Fatalf("打开 MySQL 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	for _, q := range []string{
		`DELETE FROM agent_decision_jobs WHERE session_id = ?`,
		`DELETE FROM agent_wake_queue WHERE session_id = ?`,
		`DELETE FROM units WHERE session_id = ?`,
	} {
		if _, err := db.ExecContext(ctx, q, sessionID); err != nil {
			t.Fatalf("清理残留失败 %q: %v", q, err)
		}
	}
	return db
}

func mysqlRunner(db *sql.DB, cfg Config) *Runner {
	fixed := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	return New(db, cfg, nil).withClock(func() time.Time { return fixed })
}

// TestRegionRunnerMySQLEndToEnd：MySQL 下 seed→唤醒→入队→认领→觅食 全链路（ON DUPLICATE KEY + FOR UPDATE + version 写齐活）。
func TestRegionRunnerMySQLEndToEnd(t *testing.T) {
	const sid = "mysql-it-e2e"
	db := openMySQL(t, sid)
	ctx := context.Background()
	r := mysqlRunner(db, Config{TickSeconds: 1, Apply: true})
	tick := r.currentTick()

	repo := unit.NewRepository(db)
	rec := unit.BootstrapRecord(1, sid, "player", "她")
	rec.ID = "mysql-it-u1"
	rec.Status.Hunger = 20 // 饿 → 觅食
	rec.Status.LifeState = unit.LifeStateActive
	if err := repo.Save(ctx, rec); err != nil { // ON DUPLICATE KEY upsert
		t.Fatalf("MySQL Save: %v", err)
	}
	if err := agentqueue.EnqueueWake(ctx, db, agentqueue.WakeEntry{
		UnitID: rec.ID, SessionID: sid, RegionID: sid, WakeAtTick: tick,
	}); err != nil { // ON DUPLICATE KEY upsert
		t.Fatalf("MySQL EnqueueWake: %v", err)
	}

	if _, err := r.schedulePass(ctx); err != nil { // DistinctWakeRegions→ListDueWakes→PromoteWakeToJob
		t.Fatalf("MySQL schedulePass: %v", err)
	}
	if worked, err := r.processOne(ctx); err != nil || !worked { // ClaimNextJob(FOR UPDATE)→觅食(SaveOptimistic+Mutator)→重排→完成
		t.Fatalf("MySQL processOne: worked=%v err=%v", worked, err)
	}

	if h, _, _, err := unitHungerMySQL(ctx, repo, rec.ID); err != nil || h != 20+forageGain {
		t.Fatalf("MySQL 下觅食应把 hunger 抬到 %d，得到 %d err=%v", 20+forageGain, h, err)
	}
	// 觅食是主动 → HOT 重排到下一 tick。
	if due, _ := agentqueue.ListDueWakes(ctx, db, "", sid, tick+1, 10); len(due) != 1 || due[0].Tier != "hot" {
		t.Fatalf("MySQL 觅食后应 HOT 重排，得到 %+v", due)
	}
}

func unitHungerMySQL(ctx context.Context, repo *unit.Repository, id string) (int, string, int64, error) {
	rec, err := repo.GetByID(ctx, id)
	if err != nil {
		return 0, "", 0, err
	}
	return rec.Status.Hunger, rec.Status.LifeState, rec.Version, nil
}

// TestAgentQueueMySQLNoDoubleClaim：MySQL FOR UPDATE 行锁下，8 个并发 worker 抢同一批作业，每条恰被认领一次（无双认领）。
func TestAgentQueueMySQLNoDoubleClaim(t *testing.T) {
	const sid = "mysql-it-claim"
	db := openMySQL(t, sid)
	ctx := context.Background()

	const jobs = 40
	for i := 0; i < jobs; i++ {
		if _, err := agentqueue.EnqueueJob(ctx, db, agentqueue.DecisionJob{
			UnitID: "u" + strconv.Itoa(i), SessionID: sid, RegionID: sid,
		}); err != nil {
			t.Fatalf("EnqueueJob: %v", err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := agentqueue.ClaimNextJob(ctx, db)
				if err != nil {
					t.Errorf("ClaimNextJob: %v", err)
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				seen[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != jobs {
		t.Fatalf("应认领 %d 条不同作业，得到 %d", jobs, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("作业 %s 被认领 %d 次（FOR UPDATE 行锁双认领防护失效）", id, n)
		}
	}
}

// TestSaveOptimisticMySQLNoClobber：MySQL 下 version 守护条件写——无条件写者（战斗扣 HP）与乐观写者（觅食）并发，
// 战斗的 HP 扣减永不被覆盖（终值 HP==100-aWrites），验证 SaveOptimistic 的 WHERE version=? + RowsAffected 在 MySQL 语义下成立。
func TestSaveOptimisticMySQLNoClobber(t *testing.T) {
	const sid = "mysql-it-opt"
	db := openMySQL(t, sid)
	ctx := context.Background()
	repo := unit.NewRepository(db)
	mutator := status.NewMutator(db, repo)

	rec := unit.BootstrapRecord(1, sid, "player", "她")
	rec.ID = "mysql-it-opt-u1"
	rec.Status.HP = 100
	rec.Status.Hunger = 30
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	const aWrites = 40
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // A：战斗——无条件 Save，每次 HP-1（唯一 HP 写者）
		defer wg.Done()
		for i := 0; i < aWrites; i++ {
			cur, err := repo.GetByID(ctx, rec.ID)
			if err != nil {
				t.Errorf("A get: %v", err)
				return
			}
			cur.Status.HP -= 1
			if err := repo.Save(ctx, cur); err != nil {
				t.Errorf("A save: %v", err)
				return
			}
		}
	}()
	wg.Add(1)
	go func() { // B：region-runner——乐观觅食，冲突即退避（不覆盖）
		defer wg.Done()
		for i := 0; i < aWrites*3; i++ {
			if _, err := mutator.ApplyOptimistic(ctx, status.Mutation{
				UnitID: rec.ID, Field: status.FieldHunger, Delta: 5, ReasonCode: events.ReasonAmbientForage,
			}); err != nil && !errors.Is(err, status.ErrConcurrentModification) {
				t.Errorf("B 非预期错误: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	final, err := repo.GetByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if final.Status.HP != 100-aWrites {
		t.Fatalf("MySQL 下 A 的 HP 扣减不应被 B 覆盖：期望 %d，实际 %d（version 守护失效）", 100-aWrites, final.Status.HP)
	}
}
