package dbmigrate

// 文件说明：加列迁移测试——模拟「现网旧库 events 缺世界双键列」，验证补列 + 幂等 + 可写。

import (
	"context"
	"database/sql"
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
