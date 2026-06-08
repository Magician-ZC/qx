// 文件说明：socialobject 持久层的聚焦单元测试。起临时 SQLite（仿 worldbus_test），覆盖修复 4：
// Create/AddMember 显式写 created_at/joined_at（双驱动一致、定宽 UTC、字典序==时间序），
// 断言时间字段非空、ListByWorld 按 created_at 倒序稳定（MySQL 列默认 ” 不显式写会致排序失真）。
package socialobject

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "social.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return context.Background(), db
}

// TestCreateAndAddMemberStampTime 验证未显式给时间时，Create/AddMember 自动写入非空时间戳。
func TestCreateAndAddMemberStampTime(t *testing.T) {
	ctx, db := newDB(t)

	id, err := Create(ctx, db, SocialObject{WorldID: "w1", Kind: "alliance", Label: "北境同盟"})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}
	if id == "" {
		t.Fatalf("应返回客体 ID")
	}

	got, err := Get(ctx, db, id)
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	if got.CreatedAt == "" {
		t.Fatalf("created_at 应被显式写入、非空，得到空串")
	}
	if got.Status != "active" {
		t.Fatalf("默认 status 应为 active，得到 %q", got.Status)
	}

	if err := AddMember(ctx, db, Member{ObjectID: id, UnitID: "u1", Score: 0.8}); err != nil {
		t.Fatalf("AddMember 失败: %v", err)
	}
	members, err := ListMembers(ctx, db, id)
	if err != nil {
		t.Fatalf("ListMembers 失败: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("应有 1 名成员，得到 %d", len(members))
	}
	if members[0].JoinedAt == "" {
		t.Fatalf("joined_at 应被显式写入、非空，得到空串")
	}
}

// TestListByWorldOrderedByCreatedAtDesc 验证 ListByWorld 按 created_at 倒序稳定。
// 显式注入定宽时间串（字典序==时间序），即便同秒写入也能确定排序，等价于 MySQL 双驱动行为。
func TestListByWorldOrderedByCreatedAtDesc(t *testing.T) {
	ctx, db := newDB(t)

	// 乱序写入三个客体，时间由旧到新分别为 t1<t2<t3，最近优先应还原为 t3、t2、t1。
	cases := []struct {
		id        string
		createdAt string
	}{
		{"obj-b", "2026-01-02 09:00:00"}, // t2
		{"obj-a", "2026-01-01 08:00:00"}, // t1（最旧）
		{"obj-c", "2026-01-03 10:00:00"}, // t3（最新）
	}
	for _, c := range cases {
		if _, err := Create(ctx, db, SocialObject{
			ID: c.id, WorldID: "w1", Kind: "feud", CreatedAt: c.createdAt,
		}); err != nil {
			t.Fatalf("Create %s 失败: %v", c.id, err)
		}
	}
	// 别的世界不应串味。
	if _, err := Create(ctx, db, SocialObject{ID: "obj-z", WorldID: "w2", Kind: "feud"}); err != nil {
		t.Fatalf("Create obj-z 失败: %v", err)
	}

	list, err := ListByWorld(ctx, db, "w1")
	if err != nil {
		t.Fatalf("ListByWorld 失败: %v", err)
	}
	wantOrder := []string{"obj-c", "obj-b", "obj-a"} // created_at DESC
	if len(list) != len(wantOrder) {
		t.Fatalf("w1 应返回 %d 个客体，得到 %d", len(wantOrder), len(list))
	}
	for i, want := range wantOrder {
		if list[i].ID != want {
			t.Fatalf("最近优先：第 %d 个应为 %s，得到 %s（created_at=%q）", i, want, list[i].ID, list[i].CreatedAt)
		}
		if list[i].CreatedAt == "" {
			t.Fatalf("第 %d 个 created_at 不应为空（MySQL 下空串会致排序失真）", i)
		}
	}
}

// TestAddMemberKeepsFirstJoinedAt 验证重复绑定更新分数、但保留首次 joined_at。
func TestAddMemberKeepsFirstJoinedAt(t *testing.T) {
	ctx, db := newDB(t)
	id, err := Create(ctx, db, SocialObject{ID: "obj-1", WorldID: "w1", Kind: "party"})
	if err != nil {
		t.Fatalf("Create 失败: %v", err)
	}

	const firstJoin = "2026-02-01 12:00:00"
	if err := AddMember(ctx, db, Member{ObjectID: id, UnitID: "u1", Score: 0.5, JoinedAt: firstJoin}); err != nil {
		t.Fatalf("首次 AddMember 失败: %v", err)
	}
	// 重复绑定：更新分数、尝试改 joined_at（应被 UPDATE 分支忽略，保留首次）。
	if err := AddMember(ctx, db, Member{ObjectID: id, UnitID: "u1", Score: 0.9, JoinedAt: "2026-03-01 00:00:00"}); err != nil {
		t.Fatalf("重复 AddMember 失败: %v", err)
	}

	members, err := ListMembers(ctx, db, id)
	if err != nil {
		t.Fatalf("ListMembers 失败: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("幂等绑定应仍为 1 名成员，得到 %d", len(members))
	}
	if members[0].Score != 0.9 {
		t.Fatalf("重复绑定应更新分数为 0.9，得到 %v", members[0].Score)
	}
	if members[0].JoinedAt != firstJoin {
		t.Fatalf("重复绑定应保留首次 joined_at=%q，得到 %q", firstJoin, members[0].JoinedAt)
	}
}
