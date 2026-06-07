package world

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/storage/dbdialect"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newWorldDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "world.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return context.Background(), db
}

func TestCreateGetList(t *testing.T) {
	ctx, db := newWorldDB(t)
	id, err := Create(ctx, db, World{Name: "无尽之环", MaxPopulation: 1000, RegionSeed: "seed-7"})
	if err != nil {
		t.Fatalf("创建世界失败: %v", err)
	}
	got, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("取世界失败: %v", err)
	}
	if got.Name != "无尽之环" || got.Status != StatusActive || got.MaxPopulation != 1000 || got.RegionSeed != "seed-7" {
		t.Fatalf("世界字段不符: %+v", got)
	}

	if _, err := Create(ctx, db, World{Name: "封存之城", Status: StatusSealed}); err != nil {
		t.Fatalf("创建第二个世界失败: %v", err)
	}
	active, err := List(ctx, db, StatusActive, 0)
	if err != nil {
		t.Fatalf("列出活跃世界失败: %v", err)
	}
	if len(active) != 1 || active[0].ID != id {
		t.Fatalf("应只有 1 个活跃世界且为 %s，得到 %+v", id, active)
	}
	all, _ := List(ctx, db, "", 0)
	if len(all) != 2 {
		t.Fatalf("应有 2 个世界，得到 %d", len(all))
	}
}

func TestGetNotFound(t *testing.T) {
	ctx, db := newWorldDB(t)
	if _, err := Get(ctx, db, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的世界应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := AdvanceTick(ctx, db, "nope", dbdialect.DialectSQLite); !errors.Is(err, ErrNotFound) {
		t.Fatalf("推进不存在世界的时钟应返回 ErrNotFound，得到 %v", err)
	}
}

func TestAdvanceTickMonotonic(t *testing.T) {
	ctx, db := newWorldDB(t)
	id, _ := Create(ctx, db, World{Name: "钟摆"})
	for want := 1; want <= 3; want++ {
		got, err := AdvanceTick(ctx, db, id, dbdialect.DialectSQLite)
		if err != nil {
			t.Fatalf("推进时钟失败: %v", err)
		}
		if got != want {
			t.Fatalf("世界时钟应单调推进到 %d，得到 %d", want, got)
		}
	}
	w, _ := Get(ctx, db, id)
	if w.Tick != 3 {
		t.Fatalf("持久化的 tick 应为 3，得到 %d", w.Tick)
	}
}

func TestJoinIdempotentAndWorldOf(t *testing.T) {
	ctx, db := newWorldDB(t)
	id, _ := Create(ctx, db, World{Name: "聚落"})

	// 跨分片角色（非本库 units）也能接入——无 FK。
	if err := Join(ctx, db, id, "char_from_shard_2", "founder", dbdialect.DialectSQLite); err != nil {
		t.Fatalf("接入失败: %v", err)
	}
	if err := Join(ctx, db, id, "char_from_shard_2", "founder", dbdialect.DialectSQLite); err != nil {
		t.Fatalf("重复接入应幂等不报错: %v", err)
	}
	if err := Join(ctx, db, id, "another_char", "", dbdialect.DialectSQLite); err != nil {
		t.Fatalf("接入第二人失败: %v", err)
	}

	members, err := Members(ctx, db, id, 0)
	if err != nil {
		t.Fatalf("列成员失败: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("重复接入不应重复计数，应有 2 名成员，得到 %d", len(members))
	}

	worldID, ok, err := WorldOf(ctx, db, "char_from_shard_2")
	if err != nil || !ok || worldID != id {
		t.Fatalf("应解析出角色归属世界 %s，得到 (%s, %v, %v)", id, worldID, ok, err)
	}
	if _, ok, _ := WorldOf(ctx, db, "nobody"); ok {
		t.Fatalf("无归属角色应 ok=false")
	}
}

func TestSeal(t *testing.T) {
	ctx, db := newWorldDB(t)
	id, _ := Create(ctx, db, World{Name: "将封"})
	if err := Seal(ctx, db, id); err != nil {
		t.Fatalf("封存失败: %v", err)
	}
	w, _ := Get(ctx, db, id)
	if w.Status != StatusSealed {
		t.Fatalf("应已封存，得到 %s", w.Status)
	}
	if err := Seal(ctx, db, "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("封存不存在世界应 ErrNotFound，得到 %v", err)
	}
}
