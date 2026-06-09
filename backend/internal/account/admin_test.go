package account

// 文件说明：客户管理读侧（ListUsers/搜索/GetByID）+ 封禁链路（SetBanned 后 Login/CurrentUser 被拒）的集成测试。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newAccountSvc(t *testing.T) *Service {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "acct.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := NewService(db, time.Hour)
	if err := svc.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	return svc
}

func TestListAndSearchUsers(t *testing.T) {
	svc := newAccountSvc(t)
	ctx := context.Background()
	if _, err := svc.Register(ctx, "alice", "爱丽丝", "password1"); err != nil {
		t.Fatalf("reg alice: %v", err)
	}
	if _, err := svc.Register(ctx, "bob", "鲍勃", "password2"); err != nil {
		t.Fatalf("reg bob: %v", err)
	}
	all, err := svc.ListUsers(ctx, "", 100)
	if err != nil || len(all) != 2 {
		t.Fatalf("list all: n=%d err=%v", len(all), err)
	}
	// 搜索（按 username 模糊）。
	hit, _ := svc.ListUsers(ctx, "alic", 100)
	if len(hit) != 1 || hit[0].Username != "alice" {
		t.Fatalf("search alice: %+v", hit)
	}
	// 按中文 display_name 搜索。
	hit2, _ := svc.ListUsers(ctx, "鲍勃", 100)
	if len(hit2) != 1 || hit2[0].Username != "bob" {
		t.Fatalf("search 鲍勃: %+v", hit2)
	}
}

func TestBanBlocksLoginAndToken(t *testing.T) {
	svc := newAccountSvc(t)
	ctx := context.Background()
	u, err := svc.Register(ctx, "carol", "卡萝", "password3")
	if err != nil {
		t.Fatalf("reg: %v", err)
	}
	// 登录拿 token。
	login, err := svc.Login(ctx, "carol", "password3")
	if err != nil {
		t.Fatalf("login before ban: %v", err)
	}
	if _, err := svc.CurrentUser(ctx, login.Token); err != nil {
		t.Fatalf("current before ban: %v", err)
	}
	// 封禁。
	if err := svc.SetBanned(ctx, u.ID, true); err != nil {
		t.Fatalf("ban: %v", err)
	}
	// 既有 token 立即失效。
	if _, err := svc.CurrentUser(ctx, login.Token); err == nil {
		t.Fatalf("封禁后既有 token 应失效")
	}
	// 密码正确也不能再登录。
	if _, err := svc.Login(ctx, "carol", "password3"); err == nil {
		t.Fatalf("封禁后应拒绝登录")
	}
	// 解封后恢复。
	if err := svc.SetBanned(ctx, u.ID, false); err != nil {
		t.Fatalf("unban: %v", err)
	}
	if _, err := svc.Login(ctx, "carol", "password3"); err != nil {
		t.Fatalf("解封后应能登录: %v", err)
	}
	// GetByID 反映封禁态。
	got, err := svc.GetByID(ctx, u.ID)
	if err != nil || got.Banned {
		t.Fatalf("GetByID after unban: %+v err=%v", got, err)
	}
}
