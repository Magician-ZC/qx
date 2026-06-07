package session

// 文件说明：MySQL 跨驱动集成测试（默认跳过；设 QUNXIANG_TEST_MYSQL_DSN 才跑）。
// 专门覆盖 M0 改的方言分支：world.AdvanceTick 的 SELECT...FOR UPDATE 发号（并发单调）、
// world.Join 的 INSERT IGNORE 幂等、world_boss strikeTx 的 FOR UPDATE 扣血到死。

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"

	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/storage/dbdialect"
	mysqlstore "qunxiang/backend/internal/storage/mysql"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
)

func mysqlServiceOrSkip(t *testing.T) (*sql.DB, *unit.Repository, *Service) {
	t.Helper()
	dsn := os.Getenv("QUNXIANG_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("设 QUNXIANG_TEST_MYSQL_DSN 跑 MySQL 跨驱动集成测试")
	}
	db, err := mysqlstore.Open(dsn)
	if err != nil {
		t.Fatalf("打开 MySQL 失败: %v", err)
	}
	// 测试隔离：清掉相关表（顺序避开外键）。
	for _, tbl := range []string{"world_bosses", "cross_events", "world_members", "worlds", "events", "units"} {
		_, _ = db.ExecContext(context.Background(), "DELETE FROM "+tbl)
	}
	repo := unit.NewRepository(db)
	service := &Service{db: db, units: repo, mutator: status.NewMutator(db, repo)}
	t.Cleanup(func() { _ = db.Close() })
	return db, repo, service
}

func TestMySQLWorldClockMonotonicUnderConcurrency(t *testing.T) {
	_, _, service := mysqlServiceOrSkip(t)
	ctx := context.Background()
	wid, err := world.Create(ctx, service.db, world.World{Name: "mysql世界"})
	if err != nil {
		t.Fatalf("建世界失败: %v", err)
	}

	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = service.RecordCrossInteraction(ctx, wid, "a", "b", worldbus.KindGift, 1, nil)
		}()
	}
	wg.Wait()

	// 时钟必须恰好推进到 N（FOR UPDATE 行锁防撞号）。
	w, _ := world.Get(ctx, service.db, wid)
	if w.Tick != N {
		t.Fatalf("并发 %d 次发号后时钟应为 %d，得到 %d", N, N, w.Tick)
	}
	// 总线上 N 条事件、world_tick 必须是 1..N 全不同（无重号）。
	evs, _ := worldbus.ListByWorld(ctx, service.db, wid, 0)
	if len(evs) != N {
		t.Fatalf("应有 %d 条总线事件，得到 %d", N, len(evs))
	}
	seen := map[int]bool{}
	for _, e := range evs {
		if seen[e.WorldTick] {
			t.Fatalf("world_tick 撞号：%d 重复", e.WorldTick)
		}
		seen[e.WorldTick] = true
	}
	for i := 1; i <= N; i++ {
		if !seen[i] {
			t.Fatalf("缺号 world_tick=%d", i)
		}
	}
}

func TestMySQLJoinIdempotent(t *testing.T) {
	_, _, service := mysqlServiceOrSkip(t)
	ctx := context.Background()
	wid, _ := world.Create(ctx, service.db, world.World{Name: "聚落"})
	for i := 0; i < 3; i++ { // 重复接入同一人
		if err := world.Join(ctx, service.db, wid, "c1", "founder", dbdialect.DialectMySQL); err != nil {
			t.Fatalf("INSERT IGNORE 幂等接入应不报错: %v", err)
		}
	}
	_ = world.Join(ctx, service.db, wid, "c2", "", dbdialect.DialectMySQL)
	members, _ := world.Members(ctx, service.db, wid, 0)
	if len(members) != 2 {
		t.Fatalf("重复接入不应重复计数，应 2 人，得到 %d", len(members))
	}
}

func TestMySQLWorldBossStrikeToDeath(t *testing.T) {
	_, repo, service := mysqlServiceOrSkip(t)
	ctx := context.Background()
	wid, _ := world.Create(ctx, service.db, world.World{Name: "试炼"})
	bossID, err := service.SpawnWorldBoss(ctx, wid, "焚天古龙", 50, "")
	if err != nil {
		t.Fatalf("投放世界Boss失败: %v", err)
	}
	a := bossStriker(t, ctx, repo, 501, "甲", 30)
	b := bossStriker(t, ctx, repo, 502, "乙", 30)

	r1, err := service.StrikeWorldBoss(ctx, wid, bossID, a) // FOR UPDATE 扣血 50->20
	if err != nil {
		t.Fatalf("甲出手失败: %v", err)
	}
	if r1.Defeated || r1.HPRemaining != 20 {
		t.Fatalf("首击后应剩 20 血未死，得到 hp=%d defeated=%v", r1.HPRemaining, r1.Defeated)
	}
	r2, err := service.StrikeWorldBoss(ctx, wid, bossID, b) // 20->0 致命 + 结算
	if err != nil {
		t.Fatalf("乙出手失败: %v", err)
	}
	if !r2.Defeated || !r2.SettledByMe || r2.Participants != 2 {
		t.Fatalf("致命一击应判死+结算+2人，得到 %+v", r2)
	}
	// 已死再出手应被拒（FOR UPDATE 读到 status!=active）。
	if _, err := service.StrikeWorldBoss(ctx, wid, bossID, a); err == nil {
		t.Fatalf("对已讨平的 Boss 出手应被拒")
	}
}
