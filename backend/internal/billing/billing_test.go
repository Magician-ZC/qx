// 文件说明：billing 商业化层的聚焦单元测试。起临时 SQLite（仿 socialobject_test），覆盖三件事：
//
//	①flag QUNXIANG_BILLING_ENABLED 关时 CheckQuota 恒放行（向后兼容、零行为变化）；
//	②flag 开时 SKU 目录/购买/配额闭环（ListSKUs→Purchase→entitlement+charge+iap_receipt→AddSpend→CheckQuota 拦截）；
//	③收据 stub 校验恒通过（stubVerifier），并验证「校验失败」时只落 failed 流水、不授权益（用一个拒绝 verifier）。
//
// 测试名带 billing 前缀避免与并行 agent 的测试撞名。
package billing

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newBillingDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "billing.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return context.Background(), db
}

// TestBillingCheckQuotaDisabledAlwaysAllows 验证 flag 关时 CheckQuota 恒放行——即便配额表里已超额。
func TestBillingCheckQuotaDisabledAlwaysAllows(t *testing.T) {
	ctx, db := newBillingDB(t)
	// 显式不设 QUNXIANG_BILLING_ENABLED（t.Setenv 复原由框架保证）→ enabled=false。
	t.Setenv("QUNXIANG_BILLING_ENABLED", "")
	svc := NewService(db)
	if svc.Enabled() {
		t.Fatalf("flag 空时应 disabled")
	}

	// 即使硬塞一条「已花爆」的配额，flag 关也应放行。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO account_llm_quota (account_id, period_bucket, spent_micro_usd, cap_micro_usd, updated_at) VALUES (?,?,?,?,?)`,
		"acc-disabled", "2026-06", int64(9_999_999), int64(1), "2026-06-01 00:00:00"); err != nil {
		t.Fatalf("塞配额失败: %v", err)
	}
	allowed, err := svc.CheckQuota(ctx, "acc-disabled")
	if err != nil {
		t.Fatalf("CheckQuota 失败: %v", err)
	}
	if !allowed {
		t.Fatalf("flag 关时 CheckQuota 应恒放行，得到 allowed=false")
	}
}

// TestBillingSKUPurchaseQuotaLoop 验证 flag 开时 SKU 目录 / 购买 / 配额的完整闭环。
func TestBillingSKUPurchaseQuotaLoop(t *testing.T) {
	ctx, db := newBillingDB(t)
	t.Setenv("QUNXIANG_BILLING_ENABLED", "true")
	svc := NewService(db)
	if !svc.Enabled() {
		t.Fatalf("flag=true 时应 enabled")
	}

	// 灌目录：一上架月卡 + 一下架旧 SKU（后者不应出现在 ListSKUs）。
	if _, err := svc.UpsertSKU(ctx, SKU{ID: "sku-month", Kind: "subscription", Name: "天命月卡", PriceCents: 3000, Period: "P1M", Active: true}); err != nil {
		t.Fatalf("UpsertSKU 月卡失败: %v", err)
	}
	if _, err := svc.UpsertSKU(ctx, SKU{ID: "sku-old", Kind: "subscription", Name: "旧卡", PriceCents: 100, Period: "P1M", Active: false}); err != nil {
		t.Fatalf("UpsertSKU 旧卡失败: %v", err)
	}

	skus, err := svc.ListSKUs(ctx)
	if err != nil {
		t.Fatalf("ListSKUs 失败: %v", err)
	}
	if len(skus) != 1 {
		t.Fatalf("ListSKUs 只应返回 1 个上架 SKU，得到 %d", len(skus))
	}
	if skus[0].ID != "sku-month" || skus[0].PriceCents != 3000 || !skus[0].Active {
		t.Fatalf("上架 SKU 字段不符: %+v", skus[0])
	}

	// 购买：stub verifier 恒过 → 应写 entitlement(active) + charge(captured) + iap_receipt(verified)。
	charge, err := svc.Purchase(ctx, "acc-1", "sku-month", "apple", "RAW_RECEIPT_BLOB_DATA_1234567890")
	if err != nil {
		t.Fatalf("Purchase 失败: %v", err)
	}
	if charge.Status != "captured" {
		t.Fatalf("成功购买应 status=captured，得到 %q", charge.Status)
	}
	if charge.AmountCents != 3000 {
		t.Fatalf("charge 金额应取自 SKU=3000，得到 %d", charge.AmountCents)
	}
	if charge.ReceiptRef == "" {
		t.Fatalf("stub 校验应回填 receipt_ref，得到空")
	}

	ents, err := svc.ListEntitlements(ctx, "acc-1")
	if err != nil {
		t.Fatalf("ListEntitlements 失败: %v", err)
	}
	if len(ents) != 1 || ents[0].SKUID != "sku-month" || ents[0].Status != "active" {
		t.Fatalf("购买后应有 1 条 active 权益，得到 %+v", ents)
	}

	// 收据存证：iap_receipts 应有一条 verified=1。
	var receiptVerified int64
	if err := db.QueryRowContext(ctx,
		`SELECT verified FROM iap_receipts WHERE account_id = ? AND platform = ?`, "acc-1", "apple").
		Scan(&receiptVerified); err != nil {
		t.Fatalf("查 iap_receipts 失败: %v", err)
	}
	if receiptVerified != 1 {
		t.Fatalf("stub 校验过应 verified=1，得到 %d", receiptVerified)
	}

	// 流水：billing_charges 应有一条 captured。
	var chargeStatus string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM billing_charges WHERE id = ?`, charge.ID).Scan(&chargeStatus); err != nil {
		t.Fatalf("查 billing_charges 失败: %v", err)
	}
	if chargeStatus != "captured" {
		t.Fatalf("captured 流水应落库，得到 status=%q", chargeStatus)
	}

	// 配额闭环：新账户无配额行 → 放行；累加到超过 cap 后 → 拦截。
	allowed, err := svc.CheckQuota(ctx, "acc-quota")
	if err != nil {
		t.Fatalf("CheckQuota 初始失败: %v", err)
	}
	if !allowed {
		t.Fatalf("无配额行时应放行")
	}

	// 累加到刚好不到默认 cap（月卡×40%≈1_666_667）→ 仍放行。
	if err := svc.AddSpend(ctx, "acc-quota", "2026-06", DefaultCapMicroUSD-1); err != nil {
		t.Fatalf("AddSpend 失败: %v", err)
	}
	allowed, err = svc.CheckQuota(ctx, "acc-quota")
	if err != nil {
		t.Fatalf("CheckQuota 临界失败: %v", err)
	}
	if !allowed {
		t.Fatalf("spent(cap-1) < cap 应仍放行")
	}

	// 再加 2 micro → spent = cap+1 ≥ cap → 拦截。
	if err := svc.AddSpend(ctx, "acc-quota", "2026-06", 2); err != nil {
		t.Fatalf("AddSpend 第二次失败: %v", err)
	}
	allowed, err = svc.CheckQuota(ctx, "acc-quota")
	if err != nil {
		t.Fatalf("CheckQuota 超额失败: %v", err)
	}
	if allowed {
		t.Fatalf("spent >= cap 应拦截，得到 allowed=true")
	}

	// 验证累加确实落库（spent = cap+1）。
	var spent int64
	if err := db.QueryRowContext(ctx,
		`SELECT spent_micro_usd FROM account_llm_quota WHERE account_id = ?`, "acc-quota").Scan(&spent); err != nil {
		t.Fatalf("查 account_llm_quota 失败: %v", err)
	}
	if spent != DefaultCapMicroUSD+1 {
		t.Fatalf("累加后 spent 应为 cap+1=%d，得到 %d", DefaultCapMicroUSD+1, spent)
	}
}

// TestBillingQuotaResetsAcrossPeriod 验证跨计费周期 spent 重置（评审修复：勿把「每期上限」退化成「终生上限」）。
func TestBillingQuotaResetsAcrossPeriod(t *testing.T) {
	ctx, db := newBillingDB(t)
	t.Setenv("QUNXIANG_BILLING_ENABLED", "true")
	svc := NewService(db)

	// 5 月花掉一大笔，6 月再花一小笔——6 月的 spent 应只含 6 月 delta，不累加 5 月。
	if err := svc.AddSpend(ctx, "acc-period", "2026-05", 900_000); err != nil {
		t.Fatalf("5 月 AddSpend 失败: %v", err)
	}
	if err := svc.AddSpend(ctx, "acc-period", "2026-06", 30_000); err != nil {
		t.Fatalf("6 月 AddSpend 失败: %v", err)
	}
	var spent int64
	var bucket string
	if err := db.QueryRowContext(ctx,
		`SELECT spent_micro_usd, period_bucket FROM account_llm_quota WHERE account_id = ?`, "acc-period").
		Scan(&spent, &bucket); err != nil {
		t.Fatalf("查 account_llm_quota 失败: %v", err)
	}
	if bucket != "2026-06" {
		t.Fatalf("period_bucket 应更新为 2026-06，得到 %q", bucket)
	}
	if spent != 30_000 {
		t.Fatalf("跨周期应重置 spent 为本期 delta 30000，得到 %d（=900000+30000 说明未重置→终生上限）", spent)
	}
	// 同周期内继续累加。
	if err := svc.AddSpend(ctx, "acc-period", "2026-06", 5_000); err != nil {
		t.Fatalf("6 月二次 AddSpend 失败: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT spent_micro_usd FROM account_llm_quota WHERE account_id = ?`, "acc-period").Scan(&spent); err != nil {
		t.Fatalf("查 spent 失败: %v", err)
	}
	if spent != 35_000 {
		t.Fatalf("同周期应累加为 35000，得到 %d", spent)
	}
}

// TestBillingCheckQuotaUnlocksAcrossPeriod 验证「被锁账户跨周期解封」的纯 CheckQuota 路径（评审修复 be-quota-lockout-1）。
// 真实锁死场景：账户在某历史周期超额 → CheckQuota 返 false → 后续 LLM 全走 budget_guardrail 兜底（成本=0）→
// recordLLMSpendBestEffort no-op → AddSpend 永不以正成本被调用 → 存量行的旧 period_bucket+over-cap spent 永远停在那里。
// 若 CheckQuota 不按当期 period_bucket 比较，新周期即使零消费也被持续拦截（终生封禁）。这里直接塞一条「历史周期已花爆」的行，
// 当期（time.Now wall-clock，绝不等于 "2000-01"）应放行。
func TestBillingCheckQuotaUnlocksAcrossPeriod(t *testing.T) {
	ctx, db := newBillingDB(t)
	t.Setenv("QUNXIANG_BILLING_ENABLED", "true")
	svc := NewService(db)
	if !svc.Enabled() {
		t.Fatalf("flag=true 时应 enabled")
	}

	// 历史周期 "2000-01" 已花爆（spent 远超 cap），且永不可能等于当期 UTC 月份。
	if _, err := db.ExecContext(ctx,
		`INSERT INTO account_llm_quota (account_id, period_bucket, spent_micro_usd, cap_micro_usd, updated_at) VALUES (?,?,?,?,?)`,
		"acc-stale", "2000-01", int64(9_999_999), DefaultCapMicroUSD, "2000-01-01 00:00:00"); err != nil {
		t.Fatalf("塞历史周期配额失败: %v", err)
	}
	allowed, err := svc.CheckQuota(ctx, "acc-stale")
	if err != nil {
		t.Fatalf("CheckQuota 失败: %v", err)
	}
	if !allowed {
		t.Fatalf("历史周期超额、当期零消费应放行（否则终生封禁），得到 allowed=false")
	}

	// 对照：同当期超额仍应拦截（确保没把闸彻底拆掉）。先按 AddSpend 走到当期 bucket，再花爆。
	if err := svc.AddSpend(ctx, "acc-stale", quotaCurBucketForTest(), DefaultCapMicroUSD+1); err != nil {
		t.Fatalf("当期 AddSpend 失败: %v", err)
	}
	allowed, err = svc.CheckQuota(ctx, "acc-stale")
	if err != nil {
		t.Fatalf("CheckQuota 失败: %v", err)
	}
	if allowed {
		t.Fatalf("当期超额应拦截，得到 allowed=true")
	}
}

// quotaCurBucketForTest 返回当期 UTC 月份键（与 CheckQuota / session.quotaPeriodBucket 同口径）。
func quotaCurBucketForTest() string {
	return time.Now().UTC().Format("2006-01")
}

// rejectVerifier 是一个恒拒绝的收据校验器，用于验证「校验失败」路径。
type rejectVerifier struct{}

func (rejectVerifier) Verify(_ context.Context, _ string, _ string) (bool, string, error) {
	return false, "", nil
}

// TestBillingPurchaseRejectedReceipt 验证收据校验失败时：落 failed 流水、不授权益、返回错误。
func TestBillingPurchaseRejectedReceipt(t *testing.T) {
	ctx, db := newBillingDB(t)
	t.Setenv("QUNXIANG_BILLING_ENABLED", "true")
	svc := NewService(db).WithVerifier(rejectVerifier{})

	if _, err := svc.UpsertSKU(ctx, SKU{ID: "sku-x", Kind: "consumable", Name: "加量包", PriceCents: 600, Period: "", Active: true}); err != nil {
		t.Fatalf("UpsertSKU 失败: %v", err)
	}

	charge, err := svc.Purchase(ctx, "acc-bad", "sku-x", "google", "BAD_RECEIPT")
	if err == nil {
		t.Fatalf("校验失败应返回错误")
	}
	if charge.Status != "failed" {
		t.Fatalf("校验失败应 status=failed，得到 %q", charge.Status)
	}

	// 不应授权益。
	ents, err := svc.ListEntitlements(ctx, "acc-bad")
	if err != nil {
		t.Fatalf("ListEntitlements 失败: %v", err)
	}
	if len(ents) != 0 {
		t.Fatalf("校验失败不应授权益，得到 %d 条", len(ents))
	}

	// 失败流水应落库。
	var failedStatus string
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM billing_charges WHERE account_id = ?`, "acc-bad").Scan(&failedStatus); err != nil {
		t.Fatalf("查失败流水失败: %v", err)
	}
	if failedStatus != "failed" {
		t.Fatalf("应落 failed 流水，得到 %q", failedStatus)
	}

	// 收据原文仍存证（verified=0）。
	var v int64
	if err := db.QueryRowContext(ctx,
		`SELECT verified FROM iap_receipts WHERE account_id = ?`, "acc-bad").Scan(&v); err != nil {
		t.Fatalf("查收据失败: %v", err)
	}
	if v != 0 {
		t.Fatalf("校验失败收据应 verified=0，得到 %d", v)
	}
}

// TestBillingPurchaseUnknownSKU 验证购买不存在的 SKU 返回错误、无副作用。
func TestBillingPurchaseUnknownSKU(t *testing.T) {
	ctx, db := newBillingDB(t)
	t.Setenv("QUNXIANG_BILLING_ENABLED", "true")
	svc := NewService(db)
	if _, err := svc.Purchase(ctx, "acc-1", "no-such-sku", "apple", "x"); err == nil {
		t.Fatalf("购买未知 SKU 应返回错误")
	}
}

// TestBillingPurchaseIdempotent 验证同一 receipt_ref 重复 POST 不重复落账/不重复授权益。
// 用 StaticTokenSource 不相关；stub verifier 的 receipt_ref 含 blob 前缀 → 同 blob 必同 ref，构成幂等键。
func TestBillingPurchaseIdempotent(t *testing.T) {
	ctx, db := newBillingDB(t)
	t.Setenv("QUNXIANG_BILLING_ENABLED", "true")
	svc := NewService(db) // 默认 stubVerifier（flag QUNXIANG_IAP_REAL 未设）。

	if _, err := svc.UpsertSKU(ctx, SKU{ID: "sku-idem", Kind: "consumable", Name: "宝石包", PriceCents: 600, Period: "", Active: true}); err != nil {
		t.Fatalf("UpsertSKU 失败: %v", err)
	}

	const blob = "STABLE_RECEIPT_BLOB_FOR_IDEMPOTENCY"
	first, err := svc.Purchase(ctx, "acc-idem", "sku-idem", "apple", blob)
	if err != nil {
		t.Fatalf("首次 Purchase 失败: %v", err)
	}
	if first.Status != "captured" {
		t.Fatalf("首次应 captured，得到 %q", first.Status)
	}

	second, err := svc.Purchase(ctx, "acc-idem", "sku-idem", "apple", blob)
	if err != nil {
		t.Fatalf("重复 Purchase 不应报错（幂等），得到: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("重复 Purchase 应复用既有 charge.ID=%q，得到 %q", first.ID, second.ID)
	}

	// 只应有一条 captured 流水（不重复落账）。
	var chargeCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM billing_charges WHERE account_id = ? AND sku_id = ? AND status = 'captured'`,
		"acc-idem", "sku-idem").Scan(&chargeCount); err != nil {
		t.Fatalf("查 charge 数失败: %v", err)
	}
	if chargeCount != 1 {
		t.Fatalf("幂等应只落 1 条 captured 流水，得到 %d", chargeCount)
	}

	// 只应有一条权益。
	ents, err := svc.ListEntitlements(ctx, "acc-idem")
	if err != nil {
		t.Fatalf("ListEntitlements 失败: %v", err)
	}
	if len(ents) != 1 {
		t.Fatalf("幂等应只有 1 条权益，得到 %d", len(ents))
	}
}

// TestBillingSubscriptionExpiry 验证订阅类 SKU（period 非空）授权益时写非空 expires_at，且按周期推算正确；
// 一次性/消耗品（period 空）expires_at 留空。
func TestBillingSubscriptionExpiry(t *testing.T) {
	ctx, db := newBillingDB(t)
	t.Setenv("QUNXIANG_BILLING_ENABLED", "true")
	svc := NewService(db)

	// 订阅月卡（P1M）。
	if _, err := svc.UpsertSKU(ctx, SKU{ID: "sku-sub", Kind: "subscription", Name: "月卡", PriceCents: 3000, Period: "P1M", Active: true}); err != nil {
		t.Fatalf("UpsertSKU 订阅失败: %v", err)
	}
	// 消耗品（无周期）。
	if _, err := svc.UpsertSKU(ctx, SKU{ID: "sku-con", Kind: "consumable", Name: "钻石", PriceCents: 100, Period: "", Active: true}); err != nil {
		t.Fatalf("UpsertSKU 消耗品失败: %v", err)
	}

	if _, err := svc.Purchase(ctx, "acc-sub", "sku-sub", "apple", "SUB_RECEIPT"); err != nil {
		t.Fatalf("订阅 Purchase 失败: %v", err)
	}
	if _, err := svc.Purchase(ctx, "acc-sub", "sku-con", "apple", "CON_RECEIPT"); err != nil {
		t.Fatalf("消耗品 Purchase 失败: %v", err)
	}

	// 订阅应有非空 expires_at。
	var subExpires string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(expires_at, '') FROM account_entitlements WHERE account_id = ? AND sku_id = ?`,
		"acc-sub", "sku-sub").Scan(&subExpires); err != nil {
		t.Fatalf("查订阅 expires_at 失败: %v", err)
	}
	if subExpires == "" {
		t.Fatalf("订阅类应写非空 expires_at，得到空")
	}

	// 消耗品 expires_at 应为空（永久）。
	var conExpires string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(expires_at, '') FROM account_entitlements WHERE account_id = ? AND sku_id = ?`,
		"acc-sub", "sku-con").Scan(&conExpires); err != nil {
		t.Fatalf("查消耗品 expires_at 失败: %v", err)
	}
	if conExpires != "" {
		t.Fatalf("消耗品应 expires_at 留空（永久），得到 %q", conExpires)
	}
}

// TestBillingParseISOPeriod 单测 ISO-8601 duration 子集解析（年/月/日/周组合 + 非法输入）。
func TestBillingParseISOPeriod(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		y, m, d int
	}{
		{"P1M", true, 0, 1, 0},
		{"P7D", true, 0, 0, 7},
		{"P1Y", true, 1, 0, 0},
		{"P2W", true, 0, 0, 14},
		{"P1Y2M10D", true, 1, 2, 10},
		{"", false, 0, 0, 0},
		{"1M", false, 0, 0, 0},   // 缺 P 前缀。
		{"P", false, 0, 0, 0},    // 空 body。
		{"P1", false, 0, 0, 0},   // 缺单位。
		{"PT1H", false, 0, 0, 0}, // 时间部分不支持。
		{"PM", false, 0, 0, 0},   // 单位前无数字。
	}
	for _, c := range cases {
		got, ok := parseISOPeriod(c.in)
		if ok != c.wantOK {
			t.Fatalf("parseISOPeriod(%q) ok=%v 期望 %v", c.in, ok, c.wantOK)
		}
		if ok && (got.years != c.y || got.months != c.m || got.days != c.d) {
			t.Fatalf("parseISOPeriod(%q)=%+v 期望 y=%d m=%d d=%d", c.in, got, c.y, c.m, c.d)
		}
	}
}
