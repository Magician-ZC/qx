// Package billing 是商业化层的最小持久 + 服务层（设计 docs/产品方案PRD.md §3「单位经济/天命配额/付费成本上限=月卡×40%」）。
// 职责三件：①SKU 目录（ListSKUs）；②购买（Purchase，经 ReceiptVerifier 校验收据后写 entitlement+charge+iap_receipt）；
// ③账户级 LLM 成本配额闸（CheckQuota，按 account_llm_quota 的 spent_micro_usd < cap_micro_usd 判放行，体现 PRD §3.1 成本封顶）。
//
// 全 flag QUNXIANG_BILLING_ENABLED 控制：默认关 → CheckQuota 恒 allowed=true、ListSKUs/Purchase 仍可读写（纯附加，对默认链路零行为变化）。
// 收据校验是**明确 stub 边界**：默认 stubVerifier 恒返回 true，真 Apple App Store / Google Play 网关待接（见 ReceiptVerifier）。
// 双驱动 SQLite/MySQL：经 dbdialect.IsMySQL 分支 ON CONFLICT vs ON DUPLICATE KEY，统一 ? 占位。
// 刻意不设 account/sku 外键：账户与角色可能跨分片，归属完整性由业务层负责；金额一律存最小货币单位（cents/micro_usd）。
package billing

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// 默认账户级 LLM 成本上限（micro_usd）。来源 PRD §3.1：付费用户成本上限 = 天命月卡价 × 40%。
// 月卡 ¥30 ÷ 汇率 7.2 ≈ $4.1667，× 40% ≈ $1.6667 → 1_666_667 micro_usd。
// account_llm_quota.cap_micro_usd 为 0（未显式设额）时按本默认值放行判定，把「付费越多越亏」封死在配额上。
const DefaultCapMicroUSD int64 = 1_666_667

// SKU 是一个售卖项目（订阅/一次性/消耗品由 Kind 区分）。PriceCents 为最小货币单位（分）。
type SKU struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	PriceCents int64  `json:"price_cents"`
	Period     string `json:"period"`
	Active     bool   `json:"active"`
	CreatedAt  string `json:"created_at"`
}

// Charge 是一条计费流水（append-only 审计）。AmountCents 为最小货币单位（分）。
type Charge struct {
	ID          string `json:"id"`
	AccountID   string `json:"account_id"`
	SKUID       string `json:"sku_id"`
	AmountCents int64  `json:"amount_cents"`
	Provider    string `json:"provider"`
	ReceiptRef  string `json:"receipt_ref"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`
}

// Entitlement 是某账户对某 SKU 的当前权益态（status=active/expired/...）。
type Entitlement struct {
	AccountID string `json:"account_id"`
	SKUID     string `json:"sku_id"`
	Status    string `json:"status"`
	GrantedAt string `json:"granted_at"`
	ExpiresAt string `json:"expires_at"`
}

// ReceiptVerifier 校验平台收据。默认 stubVerifier 恒返回 true；真 Apple/Google 网关在此实现替换。
type ReceiptVerifier interface {
	// Verify 校验某平台（apple/google/...）收据原文，返回是否有效与外部凭据引用（receipt_ref，留作 charge 关联）。
	Verify(ctx context.Context, platform, receiptBlob string) (ok bool, receiptRef string, err error)
}

// stubVerifier 是收据校验的占位实现——恒通过。
// TODO 接真实收据校验 API：Apple App Store Server API（verifyReceipt/已迁 App Store Server Notifications V2）
// 与 Google Play Developer API（purchases.subscriptions/products.get）。当前是**明确 stub 边界**，勿在生产用。
type stubVerifier struct{}

// Verify 恒返回 true，并用收据原文哈希位长充当占位 receipt_ref（仅用于流水可读，非真实凭据）。
func (stubVerifier) Verify(_ context.Context, platform, receiptBlob string) (bool, string, error) {
	ref := "stub:" + platform
	if blob := strings.TrimSpace(receiptBlob); blob != "" {
		// 占位引用：截前 16 字符，避免把超长原文塞进流水列；真实实现应填网关返回的 transaction_id。
		if len(blob) > 16 {
			blob = blob[:16]
		}
		ref = ref + ":" + blob
	}
	return true, ref, nil
}

// Service 是商业化服务。零依赖外部网关（收据校验经 verifier，默认 stub）。
type Service struct {
	db       *sql.DB
	verifier ReceiptVerifier
	enabled  bool
}

// NewService 构造商业化服务。flag QUNXIANG_BILLING_ENABLED 默认关 → CheckQuota 恒放行（向后兼容）。
func NewService(db *sql.DB) *Service {
	return &Service{
		db:       db,
		verifier: stubVerifier{},
		enabled:  billingEnabled(),
	}
}

// WithVerifier 替换收据校验器（接真实 Apple/Google 网关时用）。返回自身便于链式。
func (s *Service) WithVerifier(v ReceiptVerifier) *Service {
	if v != nil {
		s.verifier = v
	}
	return s
}

// Enabled 暴露 flag 态（供 router/healthz 自省）。
func (s *Service) Enabled() bool { return s.enabled }

// billingEnabled 读 QUNXIANG_BILLING_ENABLED（true/1/yes/on 视为开），默认关 → 零行为变化。
func billingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_BILLING_ENABLED"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// nowTimestamp 返回与 SQLite CURRENT_TIMESTAMP 同格式（UTC、定宽 "YYYY-MM-DD HH:MM:SS"）的时间串。
// 定宽 ⇒ 字典序 == 时间序，双驱动 ORDER BY 一致；MySQL 列默认 ”，不显式写会致排序失真，故插入一律显式写本值。
func nowTimestamp() string {
	return time.Now().UTC().Format("2006-01-02 15:04:05")
}

// ListSKUs 列出全部上架（active）的售卖项目（按 created_at 倒序、id 升序稳定）。
func (s *Service) ListSKUs(ctx context.Context) ([]SKU, error) {
	if s.db == nil {
		return nil, fmt.Errorf("billing: nil db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, price_cents, period, active, created_at
		   FROM billing_skus WHERE active = 1 ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SKU, 0)
	for rows.Next() {
		var sku SKU
		var active int64
		if err := rows.Scan(&sku.ID, &sku.Kind, &sku.Name, &sku.PriceCents, &sku.Period, &active, &sku.CreatedAt); err != nil {
			return out, err
		}
		sku.Active = active != 0
		out = append(out, sku)
	}
	return out, rows.Err()
}

// UpsertSKU 落库/更新一个售卖项目（id 空则生成）。供运营/测试灌目录用；不受 enabled flag 限制（纯目录写）。
func (s *Service) UpsertSKU(ctx context.Context, sku SKU) (string, error) {
	if s.db == nil {
		return "", fmt.Errorf("billing: nil db")
	}
	if sku.ID == "" {
		sku.ID = uuid.NewString()
	}
	if sku.CreatedAt == "" {
		sku.CreatedAt = nowTimestamp()
	}
	active := int64(0)
	if sku.Active {
		active = 1
	}
	if dbdialect.IsMySQL(s.db) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO billing_skus (id, kind, name, price_cents, period, active, created_at) VALUES (?,?,?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE kind=VALUES(kind), name=VALUES(name), price_cents=VALUES(price_cents), period=VALUES(period), active=VALUES(active)`,
			sku.ID, sku.Kind, sku.Name, sku.PriceCents, sku.Period, active, sku.CreatedAt)
		return sku.ID, err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO billing_skus (id, kind, name, price_cents, period, active, created_at) VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, name=excluded.name, price_cents=excluded.price_cents, period=excluded.period, active=excluded.active`,
		sku.ID, sku.Kind, sku.Name, sku.PriceCents, sku.Period, active, sku.CreatedAt)
	return sku.ID, err
}

// getSKU 读单个 SKU（Purchase 用以取金额）。
func (s *Service) getSKU(ctx context.Context, skuID string) (SKU, error) {
	var sku SKU
	var active int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, name, price_cents, period, active, created_at FROM billing_skus WHERE id = ?`, skuID).
		Scan(&sku.ID, &sku.Kind, &sku.Name, &sku.PriceCents, &sku.Period, &active, &sku.CreatedAt)
	sku.Active = active != 0
	return sku, err
}

// Purchase 执行一次购买：经 ReceiptVerifier 校验收据 → 校验过才写 iap_receipt(verified=1)+entitlement(active)+charge(captured)。
// 收据校验**不通过**则只落 charge=failed 流水（审计留痕），返回错误。stub 默认恒通过（见 stubVerifier）。
// receiptBlob 是平台收据原文（Apple/Google）；platform∈{apple,google,...}。金额取自 SKU.price_cents。
func (s *Service) Purchase(ctx context.Context, accountID, skuID, platform, receiptBlob string) (Charge, error) {
	if s.db == nil {
		return Charge{}, fmt.Errorf("billing: nil db")
	}
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(skuID) == "" {
		return Charge{}, fmt.Errorf("billing purchase: account_id/sku_id required")
	}
	sku, err := s.getSKU(ctx, skuID)
	if err != nil {
		if err == sql.ErrNoRows {
			return Charge{}, fmt.Errorf("billing purchase: unknown sku %q", skuID)
		}
		return Charge{}, err
	}

	now := nowTimestamp()
	ok, receiptRef, verifyErr := s.verifier.Verify(ctx, platform, receiptBlob)
	if verifyErr != nil {
		return Charge{}, fmt.Errorf("billing purchase: receipt verify: %w", verifyErr)
	}

	// 收据存证（无论是否通过都留原文供复核/补验；verified 记校验闩）。
	if err := s.insertReceipt(ctx, accountID, platform, receiptBlob, ok, now); err != nil {
		return Charge{}, err
	}

	charge := Charge{
		ID:          uuid.NewString(),
		AccountID:   accountID,
		SKUID:       skuID,
		AmountCents: sku.PriceCents,
		Provider:    platform,
		ReceiptRef:  receiptRef,
		Status:      "captured",
		CreatedAt:   now,
	}
	if !ok {
		charge.Status = "failed"
		// 失败流水仍落库（审计），但不授权益。
		if insErr := s.insertCharge(ctx, charge); insErr != nil {
			return Charge{}, insErr
		}
		return charge, fmt.Errorf("billing purchase: receipt verification failed for platform %q", platform)
	}

	// 校验通过：先授权益（active），再落 captured 流水。
	if err := s.grantEntitlement(ctx, accountID, skuID, sku.Period, now); err != nil {
		return Charge{}, err
	}
	if err := s.insertCharge(ctx, charge); err != nil {
		return Charge{}, err
	}
	return charge, nil
}

// insertReceipt 写一条 IAP 收据存证。
func (s *Service) insertReceipt(ctx context.Context, accountID, platform, blob string, verified bool, now string) error {
	v := int64(0)
	if verified {
		v = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO iap_receipts (id, account_id, platform, receipt_blob, verified, created_at) VALUES (?,?,?,?,?,?)`,
		uuid.NewString(), accountID, platform, blob, v, now)
	return err
}

// insertCharge 写一条计费流水（append-only）。
func (s *Service) insertCharge(ctx context.Context, c Charge) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO billing_charges (id, account_id, sku_id, amount_cents, provider, receipt_ref, status, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		c.ID, c.AccountID, c.SKUID, c.AmountCents, c.Provider, c.ReceiptRef, c.Status, c.CreatedAt)
	return err
}

// grantEntitlement 幂等授予/续期账户对某 SKU 的权益（status=active）。expires_at 由调用方按 period 计；此处留空（订阅续期逻辑接真网关时补）。
func (s *Service) grantEntitlement(ctx context.Context, accountID, skuID, _period, now string) error {
	if dbdialect.IsMySQL(s.db) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO account_entitlements (account_id, sku_id, status, granted_at, expires_at) VALUES (?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE status=VALUES(status), granted_at=VALUES(granted_at)`,
			accountID, skuID, "active", now, nil)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_entitlements (account_id, sku_id, status, granted_at, expires_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(account_id, sku_id) DO UPDATE SET status=excluded.status, granted_at=excluded.granted_at`,
		accountID, skuID, "active", now, nil)
	return err
}

// ListEntitlements 列出某账户的全部权益（供 router/前端展示已购）。
func (s *Service) ListEntitlements(ctx context.Context, accountID string) ([]Entitlement, error) {
	if s.db == nil {
		return nil, fmt.Errorf("billing: nil db")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT account_id, sku_id, status, granted_at, COALESCE(expires_at, '')
		   FROM account_entitlements WHERE account_id = ? ORDER BY granted_at DESC, sku_id ASC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Entitlement, 0)
	for rows.Next() {
		var e Entitlement
		if err := rows.Scan(&e.AccountID, &e.SKUID, &e.Status, &e.GrantedAt, &e.ExpiresAt); err != nil {
			return out, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CheckQuota 判账户级 LLM 成本配额是否仍放行：spent_micro_usd < cap_micro_usd 即 allowed。
// flag QUNXIANG_BILLING_ENABLED 关时恒 allowed=true（向后兼容，零行为变化）。
// 无配额行（账户从未消耗过）视为放行。cap_micro_usd<=0（未显式设额）按 DefaultCapMicroUSD（月卡×40%）兜底，体现 PRD §3.1 成本封顶。
func (s *Service) CheckQuota(ctx context.Context, accountID string) (bool, error) {
	if !s.enabled {
		return true, nil
	}
	if s.db == nil {
		return true, nil
	}
	if strings.TrimSpace(accountID) == "" {
		return true, nil
	}
	var spent, capCol sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT spent_micro_usd, cap_micro_usd FROM account_llm_quota WHERE account_id = ?`, accountID).
		Scan(&spent, &capCol)
	if err != nil {
		if err == sql.ErrNoRows {
			// 从未消耗 → 放行。
			return true, nil
		}
		return true, err
	}
	capVal := capCol.Int64
	if capVal <= 0 {
		capVal = DefaultCapMicroUSD
	}
	return spent.Int64 < capVal, nil
}

// AddSpend 把一次 LLM 成本累加进 account_llm_quota（best-effort 升级会话级护栏到账户级）。
// flag 关时仍累计（纯审计），但 CheckQuota 在 flag 关时恒放行，故对默认链路无行为影响。
// cap_micro_usd 首次插入时取 DefaultCapMicroUSD（月卡×40%）；periodBucket 为计费周期键（如 "2026-06"）。
func (s *Service) AddSpend(ctx context.Context, accountID, periodBucket string, microUSD int64) error {
	if s.db == nil {
		return fmt.Errorf("billing: nil db")
	}
	if strings.TrimSpace(accountID) == "" {
		return fmt.Errorf("billing addspend: account_id required")
	}
	if microUSD < 0 {
		microUSD = 0
	}
	now := nowTimestamp()
	if dbdialect.IsMySQL(s.db) {
		// 跨计费周期重置 spent：仅当存量 period_bucket 与本次相同才累加，否则置为本次 delta（新周期从 0 起，
		// 否则「每期上限」退化成「终生上限」，评审修复）。spent 赋值在 period_bucket 之前 → IF 内引用旧 bucket。
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO account_llm_quota (account_id, period_bucket, spent_micro_usd, cap_micro_usd, updated_at) VALUES (?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE spent_micro_usd = IF(period_bucket = VALUES(period_bucket), spent_micro_usd + VALUES(spent_micro_usd), VALUES(spent_micro_usd)), period_bucket = VALUES(period_bucket), updated_at = VALUES(updated_at)`,
			accountID, periodBucket, microUSD, DefaultCapMicroUSD, now)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_llm_quota (account_id, period_bucket, spent_micro_usd, cap_micro_usd, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(account_id) DO UPDATE SET spent_micro_usd = CASE WHEN account_llm_quota.period_bucket = excluded.period_bucket THEN account_llm_quota.spent_micro_usd + excluded.spent_micro_usd ELSE excluded.spent_micro_usd END, period_bucket = excluded.period_bucket, updated_at = excluded.updated_at`,
		accountID, periodBucket, microUSD, DefaultCapMicroUSD, now)
	return err
}
