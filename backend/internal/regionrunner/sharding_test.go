package regionrunner

// 文件说明：region 租约管理器（LeaseManager）的确定性测试（注入式固定时钟 + 真实 SQLite region_leases 表）。
// 断言抢租互斥：未过期时同 region 二次抢占失败、过期后可抢、自己可重入续租、flag 关时恒成功（向后兼容）。
// 测试前缀 lease* 避免与其它 agent 的测试撞名。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

// newLeaseManager 起一个临时 SQLite（含 region_leases 表）并返回固定时钟的 LeaseManager 及其时钟引用。
func newLeaseManager(t *testing.T) (*LeaseManager, context.Context, *time.Time) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clk := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m := NewLeaseManager(db).withClock(func() time.Time { return clk })
	return m, context.Background(), &clk
}

// TestLeaseAcquireMutualExclusionWhenEnabled：flag 开 → 首抢成功、未过期时他人二次抢占失败、原 holder 可重入续租。
func TestLeaseAcquireMutualExclusionWhenEnabled(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "true")
	m, ctx, _ := newLeaseManager(t)
	ttl := 30 * time.Second

	// holder-A 首抢空区 → 成功。
	ok, err := m.AcquireLease(ctx, "region-1", "holder-A", ttl)
	if err != nil {
		t.Fatalf("holder-A 首抢出错: %v", err)
	}
	if !ok {
		t.Fatal("holder-A 首抢应成功（空区）")
	}

	// holder-B 在未过期时抢同区 → 失败（A 持租中）。
	ok, err = m.AcquireLease(ctx, "region-1", "holder-B", ttl)
	if err != nil {
		t.Fatalf("holder-B 抢占出错: %v", err)
	}
	if ok {
		t.Fatal("holder-B 在 A 未过期持租时不应抢到")
	}

	// holder-A 重入抢自己的租约（续租语义）→ 成功。
	ok, err = m.AcquireLease(ctx, "region-1", "holder-A", ttl)
	if err != nil {
		t.Fatalf("holder-A 重入出错: %v", err)
	}
	if !ok {
		t.Fatal("holder-A 重入抢自己的租约应成功")
	}

	// 不同 region 互不影响：holder-B 抢另一区 → 成功。
	ok, err = m.AcquireLease(ctx, "region-2", "holder-B", ttl)
	if err != nil {
		t.Fatalf("holder-B 抢 region-2 出错: %v", err)
	}
	if !ok {
		t.Fatal("holder-B 抢空的 region-2 应成功")
	}
}

// TestLeaseAcquireExpiredCanBeStolen：租约过期后（时钟前进过 ttl）他人可抢过来。
func TestLeaseAcquireExpiredCanBeStolen(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "true")
	m, ctx, clk := newLeaseManager(t)
	ttl := 30 * time.Second

	if ok, err := m.AcquireLease(ctx, "region-1", "holder-A", ttl); err != nil || !ok {
		t.Fatalf("holder-A 首抢应成功: ok=%v err=%v", ok, err)
	}

	// 时钟未过 ttl：holder-B 仍抢不到。
	*clk = clk.Add(10 * time.Second)
	if ok, err := m.AcquireLease(ctx, "region-1", "holder-B", ttl); err != nil || ok {
		t.Fatalf("ttl 内 holder-B 不应抢到: ok=%v err=%v", ok, err)
	}

	// 时钟越过 ttl（A 的租约过期）：holder-B 抢到。
	*clk = clk.Add(30 * time.Second) // 共 +40s > 30s ttl
	ok, err := m.AcquireLease(ctx, "region-1", "holder-B", ttl)
	if err != nil {
		t.Fatalf("过期后 holder-B 抢占出错: %v", err)
	}
	if !ok {
		t.Fatal("租约过期后 holder-B 应抢到")
	}

	// 此后 holder-A 想抢回（未过期）→ 失败（B 持租中）。
	if ok, err := m.AcquireLease(ctx, "region-1", "holder-A", ttl); err != nil || ok {
		t.Fatalf("B 持租期间 A 不应抢回: ok=%v err=%v", ok, err)
	}
}

// TestLeaseRenewAndRelease：持租者可续租、非持租者续不到；释放后他人立刻可抢。
func TestLeaseRenewAndRelease(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "true")
	m, ctx, clk := newLeaseManager(t)
	ttl := 30 * time.Second

	if ok, err := m.AcquireLease(ctx, "region-1", "holder-A", ttl); err != nil || !ok {
		t.Fatalf("holder-A 首抢应成功: ok=%v err=%v", ok, err)
	}

	// 非持租者 holder-B 续不到。
	if ok, err := m.RenewLease(ctx, "region-1", "holder-B", ttl); err != nil || ok {
		t.Fatalf("非持租者 holder-B 不应续到: ok=%v err=%v", ok, err)
	}

	// 推进时钟 20s（仍在 ttl 内），holder-A 续租成功 → expires_at 顺延到 now+ttl。
	*clk = clk.Add(20 * time.Second)
	if ok, err := m.RenewLease(ctx, "region-1", "holder-A", ttl); err != nil || !ok {
		t.Fatalf("holder-A 续租应成功: ok=%v err=%v", ok, err)
	}

	// 再推进 20s（距首抢 40s，但续租把 expires 顶到首抢+20+30=50s，故此刻仍有效）：holder-B 抢不到。
	*clk = clk.Add(20 * time.Second)
	if ok, err := m.AcquireLease(ctx, "region-1", "holder-B", ttl); err != nil || ok {
		t.Fatalf("续租延长有效期内 holder-B 不应抢到: ok=%v err=%v", ok, err)
	}

	// holder-A 主动释放 → holder-B 立刻可抢。
	if err := m.ReleaseLease(ctx, "region-1", "holder-A"); err != nil {
		t.Fatalf("holder-A 释放出错: %v", err)
	}
	if ok, err := m.AcquireLease(ctx, "region-1", "holder-B", ttl); err != nil || !ok {
		t.Fatalf("释放后 holder-B 应抢到: ok=%v err=%v", ok, err)
	}

	// 释放只释放自己的：holder-B 持租时 holder-A 调 Release 不应清掉 B 的租约。
	if err := m.ReleaseLease(ctx, "region-1", "holder-A"); err != nil {
		t.Fatalf("holder-A 误释放出错: %v", err)
	}
	if ok, err := m.AcquireLease(ctx, "region-1", "holder-A", ttl); err != nil || ok {
		t.Fatalf("A 的无效释放不应清掉 B 的租约: ok=%v err=%v", ok, err)
	}
}

// TestLeaseDisabledFlagAlwaysSucceeds：flag 关（默认）→ 抢/续恒成功、释放 no-op、不触 DB（同 region 多 holder 都抢到）。
func TestLeaseDisabledFlagAlwaysSucceeds(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "") // 显式置空 = 关
	m, ctx, _ := newLeaseManager(t)
	ttl := 30 * time.Second

	if ok, err := m.AcquireLease(ctx, "region-1", "holder-A", ttl); err != nil || !ok {
		t.Fatalf("flag 关时 holder-A 应恒抢到: ok=%v err=%v", ok, err)
	}
	// flag 关 → 同 region 第二个 holder 也抢到（无互斥，单实例兼容）。
	if ok, err := m.AcquireLease(ctx, "region-1", "holder-B", ttl); err != nil || !ok {
		t.Fatalf("flag 关时 holder-B 也应抢到（无互斥）: ok=%v err=%v", ok, err)
	}
	if ok, err := m.RenewLease(ctx, "region-1", "holder-B", ttl); err != nil || !ok {
		t.Fatalf("flag 关时续租应恒成功: ok=%v err=%v", ok, err)
	}
	if err := m.ReleaseLease(ctx, "region-1", "holder-B"); err != nil {
		t.Fatalf("flag 关时释放应 no-op: %v", err)
	}
}

// TestLeaseEnabledNilDBErrors：flag 开但 db 为 nil → 抢租返回错误（而非静默 true），暴露误配置。
func TestLeaseEnabledNilDBErrors(t *testing.T) {
	t.Setenv(regionLeasesFlagEnv, "on")
	m := NewLeaseManager(nil)
	if _, err := m.AcquireLease(context.Background(), "region-1", "holder-A", time.Second); err == nil {
		t.Fatal("flag 开 + db nil 时抢租应返回错误")
	}
}
