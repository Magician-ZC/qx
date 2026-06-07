package unit

// 文件说明：units 作用域/生命态调度列的集成测试（对真实 SQLite，modernc 纯 Go）。
// 覆盖：Save 双写 life_state 列、SetUnitScope/SetLifeState/TouchLastActiveTick、ListActiveByRegion/CountActiveByRegion。

import (
	"context"
	"path/filepath"
	"testing"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newUnitRepo(t *testing.T) (*Repository, context.Context) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "units.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRepository(db), context.Background()
}

func saveUnit(t *testing.T, repo *Repository, ctx context.Context, id, session, lifeState string) Record {
	t.Helper()
	rec := BootstrapRecord(1, session, "player", "测试单位")
	rec.ID = id
	rec.Status.LifeState = lifeState
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("保存单位失败: %v", err)
	}
	return rec
}

func TestSaveDoubleWritesLifeStateColumn(t *testing.T) {
	repo, ctx := newUnitRepo(t)
	saveUnit(t, repo, ctx, "u1", "s1", LifeStateDead)

	var lifeState string
	if err := repo.db.QueryRowContext(ctx, `SELECT life_state FROM units WHERE id='u1'`).Scan(&lifeState); err != nil {
		t.Fatalf("读 life_state 列失败: %v", err)
	}
	if lifeState != LifeStateDead {
		t.Fatalf("life_state 列应从 status 同步为 %q，得到 %q", LifeStateDead, lifeState)
	}

	// 空生命态归一为 active。
	saveUnit(t, repo, ctx, "u2", "s1", "")
	if err := repo.db.QueryRowContext(ctx, `SELECT life_state FROM units WHERE id='u2'`).Scan(&lifeState); err != nil {
		t.Fatalf("读 life_state 列失败: %v", err)
	}
	if lifeState != LifeStateActive {
		t.Fatalf("空生命态应归一为 active，得到 %q", lifeState)
	}
}

func TestRegionScopeAndActiveQuery(t *testing.T) {
	repo, ctx := newUnitRepo(t)
	saveUnit(t, repo, ctx, "u1", "s1", LifeStateActive)
	saveUnit(t, repo, ctx, "u2", "s1", LifeStateActive)
	saveUnit(t, repo, ctx, "u3", "s1", LifeStateDead)

	// 未分配 region 时按 region 查应为空。
	if list, err := repo.ListActiveByRegion(ctx, "r1"); err != nil || len(list) != 0 {
		t.Fatalf("未分配 region 应查不到，得到 %d (err=%v)", len(list), err)
	}

	// 分配 region。
	for _, id := range []string{"u1", "u2", "u3"} {
		if err := repo.SetUnitScope(ctx, id, "w1", "r1"); err != nil {
			t.Fatalf("SetUnitScope 失败: %v", err)
		}
	}

	// 只应返回 active 的 u1/u2（u3 已 dead）。
	list, err := repo.ListActiveByRegion(ctx, "r1")
	if err != nil {
		t.Fatalf("ListActiveByRegion 失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("region r1 应有 2 个 active 单位，得到 %d", len(list))
	}
	got := map[string]bool{list[0].ID: true, list[1].ID: true}
	if !got["u1"] || !got["u2"] {
		t.Fatalf("应返回 u1/u2，得到 %+v", got)
	}
	// 记录应被完整解码（身份/属性/生命态）。
	if list[0].Identity.Name == "" || list[0].Status.LifeState == "" {
		t.Fatalf("active 单位记录应完整解码，得到 %+v", list[0].Identity)
	}

	count, err := repo.CountActiveByRegion(ctx, "r1")
	if err != nil || count != 2 {
		t.Fatalf("CountActiveByRegion 应为 2，得到 %d (err=%v)", count, err)
	}
}

func TestSetLifeStateColumnFastPath(t *testing.T) {
	repo, ctx := newUnitRepo(t)
	saveUnit(t, repo, ctx, "u1", "s1", LifeStateActive)
	if err := repo.SetUnitScope(ctx, "u1", "w1", "r1"); err != nil {
		t.Fatalf("SetUnitScope 失败: %v", err)
	}
	if c, _ := repo.CountActiveByRegion(ctx, "r1"); c != 1 {
		t.Fatalf("前置：应有 1 个 active，得到 %d", c)
	}

	// 调度层标记 dormant（这里用 dead 代表非 active）——列内快速翻转，无需整记录 Save。
	if err := repo.SetLifeState(ctx, "u1", LifeStateDead); err != nil {
		t.Fatalf("SetLifeState 失败: %v", err)
	}
	if c, _ := repo.CountActiveByRegion(ctx, "r1"); c != 0 {
		t.Fatalf("翻转 dead 后 active 应为 0，得到 %d", c)
	}
}

func TestSetLifeStateRevertedByNextSave(t *testing.T) {
	// 契约文档：life_state 列是 status_json.LifeState 的去规范化只读索引，Save 从 blob 单向覆盖写。
	// 故 SetLifeState 的列内翻转会被下一次整记录 Save 还原——任何持久生命态变更必须改 Status.LifeState 再 Save。
	repo, ctx := newUnitRepo(t)
	rec := saveUnit(t, repo, ctx, "u1", "s1", LifeStateActive)

	// 只翻列，不改 blob。
	if err := repo.SetLifeState(ctx, "u1", LifeStateDead); err != nil {
		t.Fatalf("SetLifeState 失败: %v", err)
	}
	var col string
	_ = repo.db.QueryRowContext(ctx, `SELECT life_state FROM units WHERE id='u1'`).Scan(&col)
	if col != LifeStateDead {
		t.Fatalf("翻转后列应为 dead，得到 %q", col)
	}

	// 任意整记录 Save（这里改 HP）会把列从 blob（仍是 active）单向还原。
	rec.Status.HP = 80
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	_ = repo.db.QueryRowContext(ctx, `SELECT life_state FROM units WHERE id='u1'`).Scan(&col)
	if col != LifeStateActive {
		t.Fatalf("Save 应从 blob 还原 life_state 列为 active（契约：blob 赢），得到 %q", col)
	}

	// 正确做法：改 blob 再 Save，列随之持久为 dead。
	rec.Status.LifeState = LifeStateDead
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	_ = repo.db.QueryRowContext(ctx, `SELECT life_state FROM units WHERE id='u1'`).Scan(&col)
	if col != LifeStateDead {
		t.Fatalf("改 blob 后 Save，列应持久为 dead，得到 %q", col)
	}
}

func TestTouchLastActiveTickOrdering(t *testing.T) {
	repo, ctx := newUnitRepo(t)
	saveUnit(t, repo, ctx, "u1", "s1", LifeStateActive)
	saveUnit(t, repo, ctx, "u2", "s1", LifeStateActive)
	_ = repo.SetUnitScope(ctx, "u1", "w1", "r1")
	_ = repo.SetUnitScope(ctx, "u2", "w1", "r1")

	// u1 更晚活跃，应排在 u2 之后（ListActiveByRegion 按 last_active_tick ASC）。
	if err := repo.TouchLastActiveTick(ctx, "u2", 5); err != nil {
		t.Fatalf("TouchLastActiveTick 失败: %v", err)
	}
	if err := repo.TouchLastActiveTick(ctx, "u1", 10); err != nil {
		t.Fatalf("TouchLastActiveTick 失败: %v", err)
	}
	list, err := repo.ListActiveByRegion(ctx, "r1")
	if err != nil || len(list) != 2 {
		t.Fatalf("应有 2 个 active，得到 %d (err=%v)", len(list), err)
	}
	if list[0].ID != "u2" || list[1].ID != "u1" {
		t.Fatalf("应按 last_active_tick 升序（u2 先），得到 %s,%s", list[0].ID, list[1].ID)
	}
}

func TestSaveDoesNotResetScopeColumns(t *testing.T) {
	repo, ctx := newUnitRepo(t)
	rec := saveUnit(t, repo, ctx, "u1", "s1", LifeStateActive)
	if err := repo.SetUnitScope(ctx, "u1", "w1", "r1"); err != nil {
		t.Fatalf("SetUnitScope 失败: %v", err)
	}
	if err := repo.TouchLastActiveTick(ctx, "u1", 42); err != nil {
		t.Fatalf("TouchLastActiveTick 失败: %v", err)
	}

	// 整记录 Save 不应把调度层赋的 world_id/region_id/last_active_tick 覆盖回默认。
	rec.Status.HP = 50
	if err := repo.Save(ctx, rec); err != nil {
		t.Fatalf("再次保存失败: %v", err)
	}
	var worldID, regionID string
	var tick int64
	if err := repo.db.QueryRowContext(ctx, `SELECT world_id, region_id, last_active_tick FROM units WHERE id='u1'`).Scan(&worldID, &regionID, &tick); err != nil {
		t.Fatalf("读作用域列失败: %v", err)
	}
	if worldID != "w1" || regionID != "r1" || tick != 42 {
		t.Fatalf("Save 不应覆盖作用域列，得到 world=%q region=%q tick=%d", worldID, regionID, tick)
	}
}
