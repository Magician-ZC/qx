// 文件说明：Google Play 收据校验真实网关（替换 service.go 的 stubVerifier 边界）。
//
// 走 Google Play Developer API（Purchases.products.get）：
//
//	GET {base}/androidpublisher/v3/applications/{package}/purchases/products/{productId}/tokens/{token}
//	Header: Authorization: Bearer <access token>
//	响应 JSON 的 purchaseState==0 为已购（valid）；1=已取消，2=待处理 → 视为无效。
//	receiptRef 取 orderId。
//
// receiptBlob 约定为 JSON：{"package":"com.x.y","product_id":"sku","token":"<purchaseToken>"}。
//
// 真实可配置：
//   - HTTP client 可注入（默认 http.DefaultClient 加 15s 超时夹合）。
//   - base 端点经构造参数或 env（GOOGLE_PLAY_API_BASE，默认官方 https://androidpublisher.googleapis.com）。
//   - access token 由可注入的 TokenSource 提供。装配优先级（NewGoogleReceiptVerifier 传 nil 时）：
//       1) 若配了 service-account JSON（env GOOGLE_PLAY_SA_JSON 或 GOOGLE_APPLICATION_CREDENTIALS 指向可读文件）
//          → 真 OAuth2：golang.org/x/oauth2/google 的 JWTConfigFromJSON(saJSON, androidpublisher scope)
//          得 *jwt.Config，其 TokenSource(ctx) 提供「JWT 自动签名 + 自动刷新 + 缓存复用」的 access token；
//       2) 否则回退 envTokenSource 从 env(GOOGLE_PLAY_ACCESS_TOKEN) 读静态 token（仅便于本地/测试）。
//     生产请配 service-account JSON；切勿在生产长用静态 token。**密钥/JSON 内容绝不落日志**（错误仅引用 env 名/路径）。
//
// 本文件是**真实代码路径**（真的发 HTTP GET、真的解析 purchaseState/orderId），非 return true 的占位。
package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// Google Play Developer API 官方基址（默认值；可经 env / 构造参数覆盖）。
const (
	googlePlayAPIBase = "https://androidpublisher.googleapis.com"

	// googlePurchaseStatePurchased 表示已购（有效）。1=已取消、2=待处理。
	googlePurchaseStatePurchased = 0
)

// TokenSource 提供 Google Play API 的 OAuth2 access token。可注入以便测试/生产差异化。
// 生产实现应封装 service-account OAuth2（自动刷新）；测试用静态/桩 token。
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// envTokenSource 是默认 token 源——从 env(GOOGLE_PLAY_ACCESS_TOKEN) 读静态 token。
// 仅便于本地/测试；生产请注入 service-account OAuth2 的 TokenSource（见文件头注释）。
type envTokenSource struct {
	envKey string
}

func (e envTokenSource) Token(_ context.Context) (string, error) {
	key := e.envKey
	if key == "" {
		key = "GOOGLE_PLAY_ACCESS_TOKEN"
	}
	tok := strings.TrimSpace(os.Getenv(key))
	if tok == "" {
		return "", fmt.Errorf("google verify: empty access token from env %s (生产应注入 service-account OAuth2 TokenSource)", key)
	}
	return tok, nil
}

// StaticTokenSource 返回一个恒返回固定 token 的 TokenSource（测试注入 / 简单部署用）。
func StaticTokenSource(token string) TokenSource {
	return staticTokenSource{token: token}
}

type staticTokenSource struct{ token string }

func (s staticTokenSource) Token(_ context.Context) (string, error) {
	if strings.TrimSpace(s.token) == "" {
		return "", fmt.Errorf("google verify: static token is empty")
	}
	return s.token, nil
}

// ServiceAccountTokenSource 是「生产 service-account OAuth2 token 源」接口（满足 TokenSource）。
// 现已有内建实现 serviceAccountTokenSource（见 NewServiceAccountTokenSource）；接口保留供外层差异化注入。
type ServiceAccountTokenSource interface {
	TokenSource
}

// ServiceAccountCredentialsPath 返回 service-account JSON 凭据路径。
// 读 env GOOGLE_PLAY_SA_JSON（本模块专用，优先）→ GOOGLE_APPLICATION_CREDENTIALS（Google SDK 通用）。
// 返回空表示未配置 → 装配处回退 envTokenSource（静态 token，仅限本地/测试）。
func ServiceAccountCredentialsPath() string {
	if p := strings.TrimSpace(os.Getenv("GOOGLE_PLAY_SA_JSON")); p != "" {
		return p
	}
	return strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
}

// AndroidPublisherScope 是 Google Play Developer API 所需的 OAuth2 scope。
// JWTConfigFromJSON 的 scope 参数引用此常量，避免散落硬编码。
const AndroidPublisherScope = "https://www.googleapis.com/auth/androidpublisher"

// serviceAccountTokenSource 是 service-account OAuth2 的真实 token 源——
// 用 google.JWTConfigFromJSON 得 *jwt.Config，其 TokenSource(ctx) 提供「JWT 自动签名 + 自动刷新 + 缓存复用」。
// 每次 Token(ctx) 调底层 ts.Token() 取最新 AccessToken；底层（oauth2.reuseTokenSource）只在过期时才真刷新。
type serviceAccountTokenSource struct {
	ts oauth2.TokenSource
}

// NewServiceAccountTokenSource 从 service-account JSON 字节构造 OAuth2 token 源（androidpublisher scope）。
// jsonKey 为下载的 service-account 凭据文件内容。失败（解析/scope）即返回 err，**绝不把 jsonKey 内容写进 err/日志**。
func NewServiceAccountTokenSource(jsonKey []byte) (ServiceAccountTokenSource, error) {
	cfg, err := google.JWTConfigFromJSON(jsonKey, AndroidPublisherScope)
	if err != nil {
		// err 来自 oauth2 库，仅含结构性信息（如缺字段），不回显凭据内容。
		return nil, fmt.Errorf("google verify: parse service-account JSON: %w", err)
	}
	// TokenSource 用 context.Background 的 HTTP client；每次 Token 调用仍传请求 ctx 控制超时（见 Token）。
	return &serviceAccountTokenSource{ts: cfg.TokenSource(context.Background())}, nil
}

// loadServiceAccountTokenSource 从凭据路径读 JSON 文件并构造 OAuth2 token 源。
// 路径读不到 / 内容非法 → 返回 err（**err 不含文件内容，仅含路径与原因**）。
func loadServiceAccountTokenSource(path string) (ServiceAccountTokenSource, error) {
	raw, err := os.ReadFile(path) // #nosec G304 — 路径来自受控运维 env，非用户输入。
	if err != nil {
		return nil, fmt.Errorf("google verify: read service-account JSON at %q: %w", path, err)
	}
	return NewServiceAccountTokenSource(raw)
}

// Token 取最新 access token；底层 oauth2.TokenSource 自动复用未过期 token、过期才刷新。
// 注意：oauth2 的 TokenSource.Token() 不收 ctx（刷新用构造时绑定的 background HTTP client）；
// ctx 在此用于「取消/超时早退」语义对齐 TokenSource 接口（刷新失败即返回 err，不静默放行）。
func (s *serviceAccountTokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("google verify: service-account token ctx: %w", err)
	}
	tok, err := s.ts.Token()
	if err != nil {
		// err 来自 OAuth2 token 端点（如 401/网络），不含私钥内容。
		return "", fmt.Errorf("google verify: service-account oauth2 token: %w", err)
	}
	access := strings.TrimSpace(tok.AccessToken)
	if access == "" {
		return "", fmt.Errorf("google verify: service-account oauth2 returned empty access token")
	}
	return access, nil
}

// resolveDefaultTokenSource 决定 NewGoogleReceiptVerifier 传 nil 时的默认 token 源：
//   - 若配了 service-account JSON 且可读 → 真 OAuth2（serviceAccountTokenSource）；
//   - 否则（未配 / 读不到 / 解析失败）回退 envTokenSource，保持默认行为与既有测试不破坏。
//
// 确定性、零依赖时（无 env）仍走 envTokenSource。失败回退**不 panic、不阻断**——best-effort。
func resolveDefaultTokenSource() TokenSource {
	path := ServiceAccountCredentialsPath()
	if path == "" {
		return envTokenSource{}
	}
	ts, err := loadServiceAccountTokenSource(path)
	if err != nil {
		// 配了但加载失败：回退 env（不阻断构造）。错误不含凭据内容；此处不打印（装配阶段静默回退）。
		return envTokenSource{}
	}
	return ts
}

// GoogleReceiptVerifier 是 Google Play 收据校验器（真实 HTTP 网关）。
// 仅处理 platform=="google"；其它平台返回 ErrPlatformUnsupported（由 platformVerifier 选择器分派）。
type GoogleReceiptVerifier struct {
	httpClient  *http.Client
	apiBase     string
	tokenSource TokenSource
}

// googleReceiptBlob 是约定的 receiptBlob JSON 结构。
type googleReceiptBlob struct {
	Package   string `json:"package"`
	ProductID string `json:"product_id"`
	Token     string `json:"token"`
}

// googleProductPurchase 是 Purchases.products.get 的响应体（只取本流程关心的字段）。
type googleProductPurchase struct {
	PurchaseState      *int   `json:"purchaseState"`
	ConsumptionState   *int   `json:"consumptionState"`
	OrderID            string `json:"orderId"`
	PurchaseTimeMillis string `json:"purchaseTimeMillis"`
}

// NewGoogleReceiptVerifier 构造 Google 校验器。空参回退 env / 官方默认。
//   - httpClient 为 nil → http.DefaultClient（加 15s 超时夹合）。
//   - apiBase 空 → env(GOOGLE_PLAY_API_BASE) → 官方默认。
//   - tokenSource 为 nil → resolveDefaultTokenSource：配了 service-account JSON 走真 OAuth2，否则回退 envTokenSource。
func NewGoogleReceiptVerifier(httpClient *http.Client, apiBase string, tokenSource TokenSource) *GoogleReceiptVerifier {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(apiBase) == "" {
		apiBase = firstNonEmpty(os.Getenv("GOOGLE_PLAY_API_BASE"), googlePlayAPIBase)
	}
	apiBase = strings.TrimRight(apiBase, "/")
	if tokenSource == nil {
		tokenSource = resolveDefaultTokenSource()
	}
	return &GoogleReceiptVerifier{
		httpClient:  httpClient,
		apiBase:     apiBase,
		tokenSource: tokenSource,
	}
}

// Verify 实现 ReceiptVerifier。platform 非 "google" 返回 ErrPlatformUnsupported（由选择器分派，不应直达）。
func (v *GoogleReceiptVerifier) Verify(ctx context.Context, platform, receiptBlob string) (bool, string, error) {
	if !strings.EqualFold(strings.TrimSpace(platform), "google") {
		return false, "", fmt.Errorf("%w: google verifier got platform %q", ErrPlatformUnsupported, platform)
	}

	var blob googleReceiptBlob
	if err := json.Unmarshal([]byte(receiptBlob), &blob); err != nil {
		return false, "", fmt.Errorf("google verify: receiptBlob must be JSON {package,product_id,token}: %w", err)
	}
	if strings.TrimSpace(blob.Package) == "" || strings.TrimSpace(blob.ProductID) == "" || strings.TrimSpace(blob.Token) == "" {
		return false, "", fmt.Errorf("google verify: receiptBlob missing package/product_id/token")
	}

	token, err := v.tokenSource.Token(ctx)
	if err != nil {
		return false, "", fmt.Errorf("google verify: token source: %w", err)
	}

	endpoint := fmt.Sprintf("%s/androidpublisher/v3/applications/%s/purchases/products/%s/tokens/%s",
		v.apiBase,
		url.PathEscape(blob.Package),
		url.PathEscape(blob.ProductID),
		url.PathEscape(blob.Token),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, "", fmt.Errorf("google verify: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	httpResp, err := v.httpClient.Do(req)
	if err != nil {
		return false, "", fmt.Errorf("google verify: http do: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20)) // 1 MiB 上限。
	if err != nil {
		return false, "", fmt.Errorf("google verify: read body: %w", err)
	}
	// 404 = token/product 不存在（无效收据）→ ok=false（非 err），落 failed 流水留痕。
	if httpResp.StatusCode == http.StatusNotFound {
		return false, "", nil
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// 401/403（凭据失效）、5xx（网关错）等 → err，让上层不静默放行。
		return false, "", fmt.Errorf("google verify: unexpected http status %d: %s", httpResp.StatusCode, truncateForErr(raw))
	}

	var purchase googleProductPurchase
	if err := json.Unmarshal(raw, &purchase); err != nil {
		return false, "", fmt.Errorf("google verify: decode response: %w", err)
	}
	// purchaseState 缺失视为无效（不静默放行）。
	if purchase.PurchaseState == nil || *purchase.PurchaseState != googlePurchaseStatePurchased {
		return false, "", nil
	}
	ref := purchase.OrderID
	if strings.TrimSpace(ref) == "" {
		ref = blob.Token // orderId 缺失时回退 token（仍可关联，但应少见）。
	}
	return true, "google:" + ref, nil
}
