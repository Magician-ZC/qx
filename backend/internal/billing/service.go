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
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"qunxiang/backend/internal/storage/dbdialect"
)

// ErrPlatformUnsupported 表示某 verifier 收到了它不处理的平台（由 platformVerifier 选择器分派时不应发生）。
var ErrPlatformUnsupported = errors.New("billing: unsupported platform")

// ErrPurchaseStubInProd 表示 billing 已开启但收据校验仍是 stub（QUNXIANG_IAP_REAL 关或真实凭据缺失）——
// 此态下 Purchase 一律拒绝，绝不让伪造收据走 stubVerifier 的「恒通过」落 captured 流水/发真实权益（§5 高危刷单链）。
// 「默认拒绝优于静默发权益」：要放行真实购买必须显式 QUNXIANG_IAP_REAL=on 且至少配一平台凭据（见 ProductionReady）。
var ErrPurchaseStubInProd = errors.New("billing: purchase refused — receipt verifier is stub, not production-ready (set QUNXIANG_IAP_REAL=on with a platform credential)")

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
// 真实网关见 receipt_apple.go（AppleReceiptVerifier）与 receipt_google.go（GoogleReceiptVerifier），
// 经 platformVerifier 选择器分派；设 QUNXIANG_IAP_REAL=1 时 NewService 默认装真实选择器（见 selectVerifier）。
// 默认（未设）仍用本 stub 保持向后兼容——**stub 是明确边界，勿在生产开放购买时用**。
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

// platformVerifier 是按平台分派的收据校验选择器：apple→AppleReceiptVerifier、google→GoogleReceiptVerifier，
// 其余平台或对应凭据未配 → 走 fallback（安全降级语义见下）。
//
// 安全降级语义（**绝不静默放行未校验收据于生产**）：
//   - 已注册的平台（apple/google）：真实网关。校验失败返回 ok=false（落 failed 流水留痕），
//     网络/凭据错返回 err（Purchase 整体失败，不授权益）。
//   - 未注册/未知平台：派到 fallback。
//   - fallback 默认是 denyVerifier（恒 ok=false），即未配真实网关的平台一律拒绝——安全侧默认拒绝。
//     仅当显式 WithFallback(stubVerifier{}) 才回到「恒通过」（仅供测试/灰度，注释已警示）。
type platformVerifier struct {
	verifiers map[string]ReceiptVerifier // 按小写 platform 索引的真实网关。
	fallback  ReceiptVerifier            // 未命中时的降级校验器（默认 denyVerifier，安全侧拒绝）。
}

// Verify 按 platform 分派。命中真实网关则用之；未命中走 fallback。
func (p *platformVerifier) Verify(ctx context.Context, platform, receiptBlob string) (bool, string, error) {
	key := strings.ToLower(strings.TrimSpace(platform))
	if v, ok := p.verifiers[key]; ok && v != nil {
		return v.Verify(ctx, platform, receiptBlob)
	}
	if p.fallback != nil {
		return p.fallback.Verify(ctx, platform, receiptBlob)
	}
	// 无 fallback：安全侧拒绝（绝不放行未配网关的平台）。
	return false, "", fmt.Errorf("%w: no verifier for platform %q", ErrPlatformUnsupported, platform)
}

// denyVerifier 恒拒绝——platformVerifier 对未配真实网关平台的安全默认（绝不静默放行）。
type denyVerifier struct{}

func (denyVerifier) Verify(_ context.Context, _ string, _ string) (bool, string, error) {
	return false, "", nil
}

// newRealPlatformVerifier 装配真实平台选择器：注册 apple/google 真实网关，凭据/端点经 env 自动读取。
// fallback 默认 denyVerifier（未配网关的平台安全拒绝）。HTTP client 用各 verifier 的默认（带超时）。
func newRealPlatformVerifier() *platformVerifier {
	return &platformVerifier{
		verifiers: map[string]ReceiptVerifier{
			"apple":  NewAppleReceiptVerifier(nil, "", "", ""),
			"google": NewGoogleReceiptVerifier(nil, "", nil),
		},
		fallback: denyVerifier{},
	}
}

// WithFallback 替换平台选择器的降级校验器（仅当当前 verifier 是 platformVerifier 时生效）。
// 传 stubVerifier{} 可让未配网关平台恒通过——**仅供测试/灰度，生产勿用**。返回自身便于链式。
func (s *Service) WithFallback(fallback ReceiptVerifier) *Service {
	if pv, ok := s.verifier.(*platformVerifier); ok && fallback != nil {
		pv.fallback = fallback
	}
	return s
}

// iapReal 读 QUNXIANG_IAP_REAL（true/1/yes/on 视为开）→ 用真实平台选择器；默认关，保持 stub 向后兼容。
func iapReal() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_IAP_REAL"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// selectVerifier 按 env 选择默认收据校验器：QUNXIANG_IAP_REAL 开 → 真实平台选择器；否则 stub（向后兼容）。
func selectVerifier() ReceiptVerifier {
	if iapReal() {
		return newRealPlatformVerifier()
	}
	return stubVerifier{}
}

// firstNonEmpty 返回首个非空（trim 后）字符串；全空返回 ""。供各 verifier 取「构造参数→env→默认」优先级用。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// truncateForErr 把响应原文截到 256 字节并去空白，避免把超大/多行 body 塞进错误信息。供各 verifier 的非 2xx 错误用。
func truncateForErr(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 256 {
		s = s[:256] + "…"
	}
	return s
}

// Service 是商业化服务。零依赖外部网关（收据校验经 verifier，默认 stub）。
type Service struct {
	db       *sql.DB
	verifier ReceiptVerifier
	enabled  bool
}

// NewService 构造商业化服务。flag QUNXIANG_BILLING_ENABLED 默认关 → CheckQuota 恒放行（向后兼容）。
// 收据校验器按 env 选择（selectVerifier）：QUNXIANG_IAP_REAL 开 → 真实 Apple/Google 网关选择器；
// 默认（未设）→ stubVerifier（恒通过，向后兼容）。Purchase 调用点不变（仍走 s.verifier.Verify）。
func NewService(db *sql.DB) *Service {
	return &Service{
		db:       db,
		verifier: selectVerifier(),
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

// SeedDefaultSKUs 幂等播种一组默认售卖项目，修复「SKU 目录从未播种 → ListSKUs 永远空 → 充值面板无商品」缺口（审计 PRD §3）。
// 复用 UpsertSKU（INSERT...ON CONFLICT/DUPLICATE，天然幂等）：每个 SKU 用稳定 id（"sku_*"），重复调用只刷新字段、不产生重复行。
//
// 反 P2W（设计宪法 / PRD §3「绝不卖战斗或命运胜负优势」）：本目录只售
//   - 会员订阅（叙事/陪伴增量服务，不改战斗胜负或命运掷骰）；
//   - 一次性「叙事密度包 / 信鸽加速」等纯叙事节奏权益；
//   - 外观类（角色皮肤）。
//
// 严禁出现任何直接或间接增益战斗 Score / 命运仲裁胜率 / 掉落排他权的 SKU——后者由 arbitration（胜率∝Score、付费不进）与
// encounter（ContributionScore 付费不进）在机制层兜底，本目录在商品层先行自律。
// best-effort：仅运营/启动期灌目录用，不受 enabled flag 限制（与 UpsertSKU 同，纯目录写）。
func (s *Service) SeedDefaultSKUs(ctx context.Context) error {
	if s.db == nil {
		return fmt.Errorf("billing: nil db")
	}
	// 价格单位为分（cents）。订阅类带 ISO-8601 period（P1M/P3M/P1Y）→ grantEntitlement 据此算到期；
	// 一次性/消耗品/外观 period 留空 → 永久权益（subscriptionExpiry 返 nil）。
	defaults := []SKU{
		// —— 会员订阅（叙事陪伴增量，反 P2W：不触碰战斗/命运胜负）——
		{ID: "sku_member_monthly", Kind: "subscription", Name: "天命会员·月卡", PriceCents: 3000, Period: "P1M", Active: true},
		{ID: "sku_member_quarterly", Kind: "subscription", Name: "天命会员·季卡", PriceCents: 8000, Period: "P3M", Active: true},
		{ID: "sku_member_yearly", Kind: "subscription", Name: "天命会员·年卡", PriceCents: 29800, Period: "P1Y", Active: true},
		// —— 一次性叙事密度 / 节奏权益（consumable，period 空 → 永久领取，纯叙事不改胜负）——
		{ID: "sku_narrative_density_pack", Kind: "consumable", Name: "叙事密度包", PriceCents: 1800, Period: "", Active: true},
		{ID: "sku_pigeon_express", Kind: "consumable", Name: "信鸽加急·五连", PriceCents: 600, Period: "", Active: true},
		// —— 外观（entitlement，纯装饰，绝无属性）——
		{ID: "sku_cosmetic_banner_pack", Kind: "entitlement", Name: "阵营旗帜外观包", PriceCents: 1200, Period: "", Active: true},
	}
	for _, sku := range defaults {
		// CreatedAt 留空 → UpsertSKU 首次插入填 nowTimestamp；ON CONFLICT 路径不更新 created_at（保留首播时间）。
		if _, err := s.UpsertSKU(ctx, sku); err != nil {
			return fmt.Errorf("billing: seed sku %q: %w", sku.ID, err)
		}
	}
	return nil
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
//
// 幂等：同一账户+SKU+非空 receipt_ref 的 captured 流水若已存在，直接返回既有 charge（不重复落账、不重复 grant）。
// 防客户端重试 / 网关回放导致的重复扣账；receipt_ref 是平台返回的外部凭据（apple/google 的 transaction_id/orderId），天然唯一。
func (s *Service) Purchase(ctx context.Context, accountID, skuID, platform, receiptBlob string) (Charge, error) {
	if s.db == nil {
		return Charge{}, fmt.Errorf("billing: nil db")
	}
	// 根上堵死「§5 高危刷单链」：billing 开启但收据校验未就绪（stub / 凭据缺失）时，绝不让 Purchase 走
	// stubVerifier 的「恒通过」落 captured 流水 / 发真实权益。可选沙盒：QUNXIANG_IAP_SANDBOX_OK=on 才放行
	// stub 但强制 charge.Status="sandbox" 且不 grant 真实权益（见下方沙盒分支）。
	// 「默认拒绝优于静默发权益」：billing 关时（向后兼容）不拦——此时 Purchase 属测试/灌数据路径，非线上扣费。
	if s.enabled && !ProductionReady() && !iapSandboxOK() {
		return Charge{}, ErrPurchaseStubInProd
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

	// 沙盒分支：billing 开但未就绪（stub），仅当显式 QUNXIANG_IAP_SANDBOX_OK=on 才放行——但强制 status="sandbox"
	// 且**绝不 grant 真实权益**，只落沙盒流水留痕。这样灰度/联调能跑通购买链，又不让伪造收据领真实会员/单品（§5）。
	// 注意：ProductionReady() 为真时不会进此分支（前置闸已让真实校验路径正常 grant）。
	if s.enabled && !ProductionReady() && iapSandboxOK() {
		charge.Status = "sandbox"
		if insErr := s.insertCharge(ctx, charge); insErr != nil {
			return Charge{}, insErr
		}
		return charge, nil
	}

	// 幂等闸：非空 receipt_ref 已有 captured 流水 → 复用既有 charge，不重复落账/不重复 grant。
	// 空 receipt_ref（如某些 stub/降级路径）不是稳定幂等键，跳过去重照常落账。
	if strings.TrimSpace(receiptRef) != "" {
		if existing, found, dupErr := s.findCapturedChargeByReceipt(ctx, accountID, skuID, receiptRef); dupErr != nil {
			return Charge{}, dupErr
		} else if found {
			// 既有权益视为已授（grantEntitlement 本身幂等，但此处直接短路避免无谓写）。
			return existing, nil
		}
	}

	// 校验通过：先授权益（active），再落 captured 流水。
	if err := s.grantEntitlement(ctx, accountID, skuID, sku.Kind, sku.Period, now); err != nil {
		return Charge{}, err
	}
	if err := s.insertCharge(ctx, charge); err != nil {
		return Charge{}, err
	}
	return charge, nil
}

// findCapturedChargeByReceipt 查同一账户+SKU+receipt_ref 是否已有 captured 流水（购买幂等键）。
// 找到返回既有 charge 与 found=true；无则 found=false。receipt_ref 是平台外部凭据，天然唯一，作幂等键安全。
// 取最早一条（created_at ASC）以保持「首次成功购买」语义稳定。
func (s *Service) findCapturedChargeByReceipt(ctx context.Context, accountID, skuID, receiptRef string) (Charge, bool, error) {
	var c Charge
	err := s.db.QueryRowContext(ctx,
		`SELECT id, account_id, sku_id, amount_cents, provider, receipt_ref, status, created_at
		   FROM billing_charges
		  WHERE account_id = ? AND sku_id = ? AND receipt_ref = ? AND status = 'captured'
		  ORDER BY created_at ASC, id ASC LIMIT 1`,
		accountID, skuID, receiptRef).
		Scan(&c.ID, &c.AccountID, &c.SKUID, &c.AmountCents, &c.Provider, &c.ReceiptRef, &c.Status, &c.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return Charge{}, false, nil
		}
		return Charge{}, false, err
	}
	return c, true, nil
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

// grantEntitlement 幂等授予/续期账户对某 SKU 的权益（status=active）。
//
// expires_at 由本函数按 kind/period 计算：
//   - 订阅类（kind 含 "subscription"/"订阅" 或 period 非空）→ 从 now 起按 period 推算到期时间（subscriptionExpiry）；
//     续期（ON CONFLICT）时同步刷新 expires_at（按本次 now 续期，最简单可叠加策略由真网关续订逻辑后续接管）。
//   - 一次性/消耗品（period 空且非订阅 kind）→ expires_at 留 NULL（永久权益）。
//
// expiry 为 nil 时写 SQL NULL（沿用原永久语义）；非 nil 时写定宽时间串（与 nowTimestamp 同格式，字典序==时间序）。
func (s *Service) grantEntitlement(ctx context.Context, accountID, skuID, kind, period, now string) error {
	expiry := subscriptionExpiry(kind, period, now) // nil=永久（一次性/消耗品）。
	var expiresArg interface{}
	if expiry != nil {
		expiresArg = *expiry
	} // 否则保持 nil → SQL NULL。
	if dbdialect.IsMySQL(s.db) {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO account_entitlements (account_id, sku_id, status, granted_at, expires_at) VALUES (?,?,?,?,?)
			 ON DUPLICATE KEY UPDATE status=VALUES(status), granted_at=VALUES(granted_at), expires_at=VALUES(expires_at)`,
			accountID, skuID, "active", now, expiresArg)
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO account_entitlements (account_id, sku_id, status, granted_at, expires_at) VALUES (?,?,?,?,?)
		 ON CONFLICT(account_id, sku_id) DO UPDATE SET status=excluded.status, granted_at=excluded.granted_at, expires_at=excluded.expires_at`,
		accountID, skuID, "active", now, expiresArg)
	return err
}

// isSubscriptionSKU 判某 SKU 是否订阅类：kind 含 "subscription"/"订阅"，或 period 非空（有周期即按订阅计到期）。
func isSubscriptionSKU(kind, period string) bool {
	k := strings.ToLower(strings.TrimSpace(kind))
	if strings.Contains(k, "subscription") || strings.Contains(k, "订阅") || strings.Contains(k, "subscribe") {
		return true
	}
	return strings.TrimSpace(period) != ""
}

// subscriptionExpiry 按 kind/period 算订阅到期时间串（与 nowTimestamp 同格式）；非订阅或无法解析周期返回 nil（永久）。
//
// period 采用 ISO-8601 duration 子集（PRD 的 SKU.period 约定，如 "P1M"=1 月、"P7D"=7 天、"P1Y"=1 年、"P1W"=1 周）。
// 解析失败（未知/空周期）但 kind 为订阅类时，回退默认 1 个月（保守给到期，避免订阅退化成永久）。
func subscriptionExpiry(kind, period, now string) *string {
	if !isSubscriptionSKU(kind, period) {
		return nil
	}
	base, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(now))
	if err != nil {
		base = time.Now().UTC()
	}
	d, ok := parseISOPeriod(period)
	if !ok {
		// 订阅类但周期不可解析 → 保守默认 1 个月，绝不退化成永久。
		d = isoPeriod{months: 1}
	}
	exp := base.AddDate(d.years, d.months, d.days)
	out := exp.UTC().Format("2006-01-02 15:04:05")
	return &out
}

// isoPeriod 是 ISO-8601 duration 的本流程子集（仅年/月/日；周折算为 7 日）。
type isoPeriod struct {
	years  int
	months int
	days   int
}

// parseISOPeriod 解析 ISO-8601 duration 子集："P" 前缀 + 数字+单位（Y/M/W/D）。
// 支持组合（如 "P1Y2M10D"、"P2W"）；不支持时间部分（T...）。成功返回 (period, true)，否则 (零值, false)。
func parseISOPeriod(period string) (isoPeriod, bool) {
	p := strings.ToUpper(strings.TrimSpace(period))
	if len(p) < 2 || p[0] != 'P' {
		return isoPeriod{}, false
	}
	body := p[1:]
	var out isoPeriod
	var num int
	var hasNum, hasUnit bool
	for _, r := range body {
		switch {
		case r >= '0' && r <= '9':
			num = num*10 + int(r-'0')
			hasNum = true
		case r == 'Y':
			if !hasNum {
				return isoPeriod{}, false
			}
			out.years += num
			num, hasNum, hasUnit = 0, false, true
		case r == 'M':
			if !hasNum {
				return isoPeriod{}, false
			}
			out.months += num
			num, hasNum, hasUnit = 0, false, true
		case r == 'W':
			if !hasNum {
				return isoPeriod{}, false
			}
			out.days += num * 7
			num, hasNum, hasUnit = 0, false, true
		case r == 'D':
			if !hasNum {
				return isoPeriod{}, false
			}
			out.days += num
			num, hasNum, hasUnit = 0, false, true
		default:
			// 含时间部分(T) 或未知字符 → 不支持。
			return isoPeriod{}, false
		}
	}
	// 末尾残留未消费数字（无单位）或全程无任何单位 → 无效。
	if hasNum || !hasUnit {
		return isoPeriod{}, false
	}
	return out, true
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
	var bucket sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT spent_micro_usd, cap_micro_usd, period_bucket FROM account_llm_quota WHERE account_id = ?`, accountID).
		Scan(&spent, &capCol, &bucket)
	if err != nil {
		if err == sql.ErrNoRows {
			// 从未消耗 → 放行。
			return true, nil
		}
		return true, err
	}
	// 跨计费周期重置：存量 period_bucket 与当期不同时，该行 spent 属上一周期，本期视作 spent=0 放行。
	// 否则「每期上限」对超额账户退化成「终生封禁」——撞顶一次后新周期即使零消费也被持续拦截（AddSpend 在
	// 被锁账户走不到正成本累加，存量行的旧 bucket+over-cap spent 永远停在那里）。口径与 session.quotaPeriodBucket() 一致。
	curBucket := time.Now().UTC().Format("2006-01")
	if bucket.String != curBucket {
		return true, nil
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

// IAPReal 暴露「是否启用真实收据校验」flag（QUNXIANG_IAP_REAL，true/1/yes/on 视为开）。
// 供 router/healthz 自省，以及 ProductionReady 复用。与内部 iapReal() 同义，仅导出便于纵深防御调用方判断。
func IAPReal() bool { return iapReal() }

// ProductionReady 判断 billing 是否「线上就绪」——可安全放行真实购买（绝不让伪造收据走 stub 领权益）。
// 三层与门（任一不满足即未就绪 false）：
//  1. billingEnabled()：billing 必须开启（关时本判定无意义，调用方按「关」处理路由）；
//  2. iapReal()：必须显式 QUNXIANG_IAP_REAL=on（否则收据校验是 stubVerifier 恒通过）；
//  3. hasIAPCredential()：iapReal 开时还须至少配一平台凭据——否则真实网关无凭据校验任何收据都失败/无意义，
//     是「形式上开了真实但实质仍可被绕」的伪就绪态，一律判未就绪。
//
// 「默认拒绝优于静默发权益」：只有三层全满足，Purchase 才进真实校验路径；否则前置闸返 ErrPurchaseStubInProd。
func ProductionReady() bool {
	if !billingEnabled() {
		return false
	}
	if !iapReal() {
		return false
	}
	return hasIAPCredential()
}

// hasIAPCredential 判断是否至少配了一个平台收据校验凭据（os.Getenv 任一非空，trim 后）：
//   - APPLE_IAP_SHARED_SECRET：Apple 自动续订订阅校验必填（见 receipt_apple.go）；
//   - GOOGLE_PLAY_SA_JSON / GOOGLE_APPLICATION_CREDENTIALS：Google service-account JSON 路径（真 OAuth2，见 receipt_google.go）；
//   - GOOGLE_PLAY_ACCESS_TOKEN：Google 静态 access token（仅本地/测试回退，但视为已配凭据）。
//
// 全缺 → 即便 QUNXIANG_IAP_REAL=on 也判未就绪：真实网关没有任何可用凭据，开放购买是伪就绪高危态。
func hasIAPCredential() bool {
	for _, key := range []string{
		"APPLE_IAP_SHARED_SECRET",
		"GOOGLE_PLAY_SA_JSON",
		"GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_PLAY_ACCESS_TOKEN",
	} {
		if strings.TrimSpace(os.Getenv(key)) != "" {
			return true
		}
	}
	return false
}

// iapSandboxOK 读 QUNXIANG_IAP_SANDBOX_OK（true/1/yes/on 视为开），**默认关**（与 enabled flag 同向：未设即拒绝）。
// 仅当显式开启，未就绪态的 Purchase 才放行 stub 但强制 charge.Status="sandbox" 且不 grant 真实权益（见 Purchase 沙盒分支）。
func iapSandboxOK() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("QUNXIANG_IAP_SANDBOX_OK"))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}
