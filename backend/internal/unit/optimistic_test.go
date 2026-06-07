package unit

// 文件说明：乐观并发版本号测试（M7.3-real-3-0）——Save 单调 +1；SaveOptimistic 仅版本匹配才写、否则返回 false。

import "testing"

func TestSaveIncrementsVersion(t *testing.T) {
	repo, ctx := newUnitRepo(t)
	rec := BootstrapRecord(1, "s1", "player", "测试")
	rec.ID = "u1"
	if err := repo.Save(ctx, rec); err != nil { // INSERT → version 默认 0
		t.Fatalf("save: %v", err)
	}
	if got, _ := repo.GetByID(ctx, "u1"); got.Version != 0 {
		t.Fatalf("首次插入 version 应为 0，得到 %d", got.Version)
	}
	if err := repo.Save(ctx, rec); err != nil { // UPDATE → version+1
		t.Fatalf("save2: %v", err)
	}
	if got, _ := repo.GetByID(ctx, "u1"); got.Version != 1 {
		t.Fatalf("二次保存 version 应 +1=1，得到 %d", got.Version)
	}
}

func TestSaveOptimisticVersionGuard(t *testing.T) {
	repo, ctx := newUnitRepo(t)
	rec := BootstrapRecord(1, "s1", "player", "测试")
	rec.ID = "u1"
	_ = repo.Save(ctx, rec)

	cur, _ := repo.GetByID(ctx, "u1") // version 0
	// 版本匹配 → 写入成功，version → 1。
	applied, err := repo.SaveOptimistic(ctx, cur)
	if err != nil || !applied {
		t.Fatalf("版本匹配应写入：applied=%v err=%v", applied, err)
	}
	if got, _ := repo.GetByID(ctx, "u1"); got.Version != 1 {
		t.Fatalf("SaveOptimistic 成功应 version+1=1，得到 %d", got.Version)
	}

	// 用陈旧版本（仍为 0）再写 → 应被拒（false），不改库。
	cur.Version = 0
	applied, err = repo.SaveOptimistic(ctx, cur)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if applied {
		t.Fatalf("陈旧版本应被拒（applied=false），实际写入了")
	}
	if got, _ := repo.GetByID(ctx, "u1"); got.Version != 1 {
		t.Fatalf("被拒的写不应改库，version 应仍 1，得到 %d", got.Version)
	}
}
