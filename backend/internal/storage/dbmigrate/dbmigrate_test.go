package dbmigrate

// 文件说明：加列迁移测试——模拟「现网旧库 events 缺世界双键列」，验证补列 + 幂等 + 可写。

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"qunxiang/backend/internal/storage/dbdialect"
)

func TestEnsureColumnsAddsAndIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存 sqlite 失败: %v", err)
	}
	defer db.Close()
	dbdialect.Register(db, dbdialect.DialectSQLite)
	ctx := context.Background()

	// 旧库：events 没有世界双键列。
	if _, err := db.ExecContext(ctx, `CREATE TABLE events (id TEXT PRIMARY KEY, session_id TEXT)`); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}

	// 补列。
	if err := EnsureColumns(ctx, db, "events", EventScopeColumns); err != nil {
		t.Fatalf("补列失败: %v", err)
	}
	cols, _ := existingColumns(ctx, db, "events")
	for _, c := range []string{"world_id", "region_id", "tick"} {
		if !cols[c] {
			t.Fatalf("应已补上列 %s", c)
		}
	}

	// 幂等：再补一次不报错、不重复加。
	if err := EnsureColumns(ctx, db, "events", EventScopeColumns); err != nil {
		t.Fatalf("重复补列应幂等不报错: %v", err)
	}

	// 新列可写。
	if _, err := db.ExecContext(ctx, `INSERT INTO events (id, session_id, world_id, region_id, tick) VALUES ('e1','s1','w1','r1',5)`); err != nil {
		t.Fatalf("写新列失败: %v", err)
	}
	var tick int
	var worldID string
	if err := db.QueryRowContext(ctx, `SELECT world_id, tick FROM events WHERE id = 'e1'`).Scan(&worldID, &tick); err != nil {
		t.Fatalf("读新列失败: %v", err)
	}
	if worldID != "w1" || tick != 5 {
		t.Fatalf("新列值不符：world_id=%q tick=%d", worldID, tick)
	}
}

// TestEnsureIndexAddsAndIsIdempotent 验证 EnsureIndex 在 SQLite 上建复合索引且幂等（重复调用不报错）。
func TestEnsureIndexAddsAndIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存 sqlite 失败: %v", err)
	}
	defer db.Close()
	dbdialect.Register(db, dbdialect.DialectSQLite)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE sess (id TEXT PRIMARY KEY, account_id TEXT, world_id TEXT)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 首次建索引。
	if err := EnsureIndex(ctx, db, "sess", "idx_sess_account_world", "account_id", "world_id"); err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	// 幂等：再建一次不报错（CREATE INDEX IF NOT EXISTS）。
	if err := EnsureIndex(ctx, db, "sess", "idx_sess_account_world", "account_id", "world_id"); err != nil {
		t.Fatalf("重复建索引应幂等不报错: %v", err)
	}
	// 索引确已存在。
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sess_account_world'`,
	).Scan(&name); err != nil {
		t.Fatalf("索引应已存在: %v", err)
	}
	// 无列时报错（防误用）。
	if err := EnsureIndex(ctx, db, "sess", "idx_empty"); err == nil {
		t.Fatalf("无列的 EnsureIndex 应报错")
	}
}

// TestDropIndexIsIdempotent 验证 DropIndex 在 SQLite 上删存量索引且幂等（不存在时不报错）。
func TestDropIndexIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存 sqlite 失败: %v", err)
	}
	defer db.Close()
	dbdialect.Register(db, dbdialect.DialectSQLite)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE units (id TEXT PRIMARY KEY, region_id TEXT, life_state TEXT)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := EnsureIndex(ctx, db, "units", "idx_to_drop", "region_id", "life_state"); err != nil {
		t.Fatalf("建索引失败: %v", err)
	}
	// 删除存量索引。
	if err := DropIndex(ctx, db, "units", "idx_to_drop"); err != nil {
		t.Fatalf("删索引失败: %v", err)
	}
	var name string
	if err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_to_drop'`,
	).Scan(&name); err == nil {
		t.Fatalf("索引应已被删除")
	}
	// 幂等：删不存在的索引不报错（DROP INDEX IF EXISTS）。
	if err := DropIndex(ctx, db, "units", "idx_to_drop"); err != nil {
		t.Fatalf("重复删除应幂等不报错: %v", err)
	}
}

// TestUnitsRegionIndexRenameUsesIndex 复现并锁定 major 修复：旧索引 idx_units_world_region(world_id,...) 因前导
// world_id 不在 ListActiveByRegion 的 WHERE（只过滤 region_id+life_state）里而**完全失效**（全表扫描）；
// store.go Open 的「DropIndex 旧名 + EnsureIndex 新名(region_id, life_state, last_active_tick)」迁移序列后，
// 同一查询的 EXPLAIN QUERY PLAN 必须用上新索引（SEARCH ... USING INDEX）而非 SCAN。
func TestUnitsRegionIndexRenameUsesIndex(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("打开内存 sqlite 失败: %v", err)
	}
	defer db.Close()
	dbdialect.Register(db, dbdialect.DialectSQLite)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `CREATE TABLE units (
		id TEXT PRIMARY KEY, session_id TEXT, world_id TEXT, region_id TEXT,
		life_state TEXT NOT NULL DEFAULT 'active', last_active_tick INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	// 模拟存量库：已建列序错的旧索引（109c2bb Phase 2 部署后的形态）。
	if err := EnsureIndex(ctx, db, "units", "idx_units_world_region", "world_id", "region_id", "life_state"); err != nil {
		t.Fatalf("建旧索引失败: %v", err)
	}

	planUsesIndex := func() (string, bool) {
		rows, err := db.QueryContext(ctx,
			`EXPLAIN QUERY PLAN SELECT id FROM units WHERE region_id = ? AND life_state = ? ORDER BY last_active_tick ASC, id`,
			"w#z", "active")
		if err != nil {
			t.Fatalf("EXPLAIN QUERY PLAN 失败: %v", err)
		}
		defer rows.Close()
		var detail string
		usesIndex := false
		for rows.Next() {
			var id, parent, notused int
			var d string
			if err := rows.Scan(&id, &parent, &notused, &d); err != nil {
				t.Fatalf("scan 计划失败: %v", err)
			}
			detail += d + "; "
			if strings.Contains(d, "USING INDEX") {
				usesIndex = true
			}
		}
		return detail, usesIndex
	}

	// 前置断言：旧索引下查询走全表扫描（不命中索引）——锁住「失效」这个被修复的缺陷本身。
	if detail, usesIndex := planUsesIndex(); usesIndex {
		t.Fatalf("前置：旧索引(world_id 前导)本应失效全表扫描，却命中了索引：%s", detail)
	}

	// 应用修复迁移序列（与 store.go Open 一致）。
	if err := DropIndex(ctx, db, "units", "idx_units_world_region"); err != nil {
		t.Fatalf("删旧索引失败: %v", err)
	}
	if err := EnsureIndex(ctx, db, "units", "idx_units_region_active", "region_id", "life_state", "last_active_tick"); err != nil {
		t.Fatalf("建新索引失败: %v", err)
	}

	// 修复后：同一查询必须命中新索引。
	if detail, usesIndex := planUsesIndex(); !usesIndex {
		t.Fatalf("修复后查询应命中 idx_units_region_active（SEARCH USING INDEX），实得：%s", detail)
	}
}
