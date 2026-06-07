package status_test

// 文件说明：乐观并发 Mutator 测试（M7.3-real-3-0）——ApplyOptimistic 正常路径写入+落事件；
// 并发不变量：低优先写者（ApplyOptimistic，模拟 region-runner）绝不覆盖另一无条件写者（模拟战斗）对同一单位的写。

import (
	"context"
	"errors"
	"sync"
	"testing"

	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
)

func TestApplyOptimisticHappyPath(t *testing.T) {
	db, repo, mutator := newTestDB(t)
	rec := seedUnit(t, repo, 1, "测试")
	rec.Status.Hunger = 50
	_ = repo.Save(context.Background(), rec)

	before := countEvents(t, db)
	res, err := mutator.ApplyOptimistic(context.Background(), status.Mutation{
		UnitID: rec.ID, Field: status.FieldHunger, Delta: -10, ReasonCode: events.ReasonSurvivalHunger,
	})
	if err != nil {
		t.Fatalf("无并发时 ApplyOptimistic 应成功: %v", err)
	}
	if res.Record.Status.Hunger != 40 {
		t.Fatalf("hunger 应 50-10=40，得到 %d", res.Record.Status.Hunger)
	}
	if countEvents(t, db) != before+1 {
		t.Fatalf("成功应落一条事件")
	}
}

// TestApplyOptimisticNeverClobbersConcurrentWriter：高优先无条件写者（A，模拟战斗扣 HP）与低优先乐观写者
// （B，模拟 region-runner 觅食）并发改同一单位。不变量：A 的每一次 HP 扣减都不被 B 覆盖——最终 HP == 起始 - A 的写次数。
func TestApplyOptimisticNeverClobbersConcurrentWriter(t *testing.T) {
	_, repo, mutator := newTestDB(t)
	rec := seedUnit(t, repo, 1, "测试")
	rec.Status.HP = 100
	rec.Status.Hunger = 30
	_ = repo.Save(context.Background(), rec)

	const aWrites = 60
	var wg sync.WaitGroup

	// A：战斗——无条件 Save，每次 HP-1（共 aWrites 次）。A 是唯一 HP 写者。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < aWrites; i++ {
			cur, err := repo.GetByID(context.Background(), rec.ID)
			if err != nil {
				t.Errorf("A get: %v", err)
				return
			}
			cur.Status.HP -= 1
			if err := repo.Save(context.Background(), cur); err != nil {
				t.Errorf("A save: %v", err)
				return
			}
		}
	}()

	// B：region-runner——乐观觅食（hunger+5），与 A 并发；冲突时 ApplyOptimistic 返回 ErrConcurrentModification（退避）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < aWrites*3; i++ {
			_, err := mutator.ApplyOptimistic(context.Background(), status.Mutation{
				UnitID: rec.ID, Field: status.FieldHunger, Delta: 5, ReasonCode: events.ReasonAmbientForage,
			})
			if err != nil && !errors.Is(err, status.ErrConcurrentModification) {
				t.Errorf("B 非预期错误: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// 不变量：A 的 60 次 HP 扣减全部生效、未被 B 覆盖 → HP == 100-60=40。
	final, err := repo.GetByID(context.Background(), rec.ID)
	if err != nil {
		t.Fatalf("final get: %v", err)
	}
	if final.Status.HP != 100-aWrites {
		t.Fatalf("A 的 HP 扣减不应被 B 覆盖：期望 HP=%d，实际 %d（乐观并发硬化失效）", 100-aWrites, final.Status.HP)
	}
}
