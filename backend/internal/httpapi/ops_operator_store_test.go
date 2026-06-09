package httpapi

// 文件说明：ops 操作者存储的关键运营路径测试——令牌轮换（同名换 token）与角色枚举校验（防自锁）。
// 这两条是对抗评审确证的 medium 修复点：原 ON CONFLICT(token_hash) 让「同名换 token」撞 name UNIQUE 失败、
// 原 Upsert 不校验 role 让拼错角色制造永久自锁。

import (
	"context"
	"path/filepath"
	"testing"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

// newOpsStoreTest 起一个全 schema 的临时 SQLite（含 ops_operators/ops_audit_log，由 DesignClosureTables 建），返回 store。
func newOpsStoreTest(t *testing.T) *OpsOperatorStore {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewOpsOperatorStore(db)
}

// TestOpsOperatorTokenRotation 验证「同名换 token」即令牌轮换：旧 token 失效、新 token 生效、role 同步更新、不新增行。
func TestOpsOperatorTokenRotation(t *testing.T) {
	s := newOpsStoreTest(t)
	ctx := context.Background()

	if err := s.Upsert(ctx, "alice", "operator", "tok-1", "test"); err != nil {
		t.Fatalf("首次建操作者失败: %v", err)
	}
	// 同名换 token（运营轮换某人的令牌）：原 ON CONFLICT(token_hash) 会在此撞 name UNIQUE 报错。
	if err := s.Upsert(ctx, "alice", "admin", "tok-2", "test"); err != nil {
		t.Fatalf("令牌轮换应成功（同名换 token），却失败: %v", err)
	}
	if _, ok, _ := s.Resolve(ctx, "tok-1"); ok {
		t.Fatalf("轮换后旧 token 应失效")
	}
	op, ok, err := s.Resolve(ctx, "tok-2")
	if err != nil || !ok {
		t.Fatalf("新 token 应能解析: ok=%v err=%v", ok, err)
	}
	if op.Name != "alice" || op.Role != RoleAdmin {
		t.Fatalf("轮换后应是 alice/admin，得到 %+v", op)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Fatalf("轮换不应新增行，Count=%d want 1", n)
	}
}

// TestOpsOperatorRoleValidation 验证 role 归一小写 + 枚举校验：大小写容错、非法角色拒绝（防全表无 admin 的自锁）。
func TestOpsOperatorRoleValidation(t *testing.T) {
	s := newOpsStoreTest(t)
	ctx := context.Background()

	// 大小写容错：Admin → admin（否则 roleRank=0、无人具足够权限）。
	if err := s.Upsert(ctx, "bob", "Admin", "tok-b", "test"); err != nil {
		t.Fatalf("大小写错的 Admin 应被归一接受: %v", err)
	}
	if op, _, _ := s.Resolve(ctx, "tok-b"); op.Role != RoleAdmin {
		t.Fatalf("role 应归一为 admin，得到 %q", op.Role)
	}
	// 非法 role 拒绝。
	if err := s.Upsert(ctx, "carol", "superuser", "tok-c", "test"); err == nil {
		t.Fatalf("非法 role superuser 应被拒绝（防自锁）")
	}
	// 空 role 默认 viewer（最小权，安全保守）。
	if err := s.Upsert(ctx, "dave", "", "tok-d", "test"); err != nil {
		t.Fatalf("空 role 应默认 viewer: %v", err)
	}
	if op, _, _ := s.Resolve(ctx, "tok-d"); op.Role != RoleViewer {
		t.Fatalf("空 role 应默认 viewer，得到 %q", op.Role)
	}
}
