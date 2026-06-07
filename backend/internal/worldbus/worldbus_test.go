package worldbus

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newBus(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "bus.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return context.Background(), db
}

func TestAppendAndOrderByAuthority(t *testing.T) {
	ctx, db := newBus(t)
	// 乱序写入 world_tick 3、1、2，权威排序应还原为 1、2、3。
	for _, tick := range []int{3, 1, 2} {
		if _, err := Append(ctx, db, CrossEvent{
			WorldID: "w1", ActorID: "charA", TargetID: "charB",
			Kind: KindAttack, WorldTick: tick,
			Payload: map[string]any{"tick": tick},
		}); err != nil {
			t.Fatalf("append tick=%d 失败: %v", tick, err)
		}
	}
	events, err := ListByWorld(ctx, db, "w1", 0)
	if err != nil {
		t.Fatalf("ListByWorld 失败: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("应有 3 条事件，得到 %d", len(events))
	}
	for i, want := range []int{1, 2, 3} {
		if events[i].WorldTick != want {
			t.Fatalf("第 %d 条 world_tick 应为 %d（谁先动手在前），得到 %d", i, want, events[i].WorldTick)
		}
	}
}

func TestCrossShardActorNoForeignKey(t *testing.T) {
	ctx, db := newBus(t)
	// 关键属性：actor/target 都不是本库 units 行（来自别的玩家/分片/已离线），无 FK 应可写入。
	id, err := Append(ctx, db, CrossEvent{
		WorldID: "w1", ActorID: "ghost_from_shard_7", TargetID: "offline_hero_42",
		Kind: KindRescue, WorldTick: 5,
	})
	if err != nil {
		t.Fatalf("跨分片角色（非本库 units）应可写入世界总线，却失败: %v", err)
	}
	if id == "" {
		t.Fatalf("应返回事件 ID")
	}
}

func TestListForCharacterMatchesActorOrTarget(t *testing.T) {
	ctx, db := newBus(t)
	mustAppend(t, ctx, db, CrossEvent{WorldID: "w1", ActorID: "hero", TargetID: "villain", Kind: KindAttack, WorldTick: 1})
	mustAppend(t, ctx, db, CrossEvent{WorldID: "w1", ActorID: "ally", TargetID: "hero", Kind: KindRescue, WorldTick: 2})
	mustAppend(t, ctx, db, CrossEvent{WorldID: "w1", ActorID: "x", TargetID: "y", Kind: KindGift, WorldTick: 3}) // 与 hero 无关

	got, err := ListForCharacter(ctx, db, "w1", "hero", 0)
	if err != nil {
		t.Fatalf("ListForCharacter 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("hero 作为 actor 或 target 应牵涉 2 条，得到 %d", len(got))
	}
	// 不同世界互不串味。
	if other, _ := ListForCharacter(ctx, db, "w2", "hero", 0); len(other) != 0 {
		t.Fatalf("别的世界不应返回 hero 的事件，得到 %d", len(other))
	}
}

func TestAppendValidation(t *testing.T) {
	ctx, db := newBus(t)
	if _, err := Append(ctx, db, CrossEvent{Kind: KindAttack}); err == nil {
		t.Fatalf("空 world_id 应报错")
	}
	if _, err := Append(ctx, db, CrossEvent{WorldID: "w1"}); err == nil {
		t.Fatalf("空 event_kind 应报错")
	}
}

func mustAppend(t *testing.T, ctx context.Context, db *sql.DB, ev CrossEvent) {
	t.Helper()
	if _, err := Append(ctx, db, ev); err != nil {
		t.Fatalf("append 失败: %v", err)
	}
}
