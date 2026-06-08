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
//   - access token 由可注入的 TokenSource 提供；默认 envTokenSource 从 env(GOOGLE_PLAY_ACCESS_TOKEN) 读。
//     **生产应换成 service-account OAuth2**（golang.org/x/oauth2/google 的 JWTConfigFromJSON →
//     androidpublisher scope → ts.Token()），env token 仅便于本地/测试注入；切勿在生产长用静态 token。
//     本模块刻意不引入 oauth2 依赖（保持依赖面最小），仅提供 ServiceAccountTokenSource 扩展点接口
//     与 ServiceAccountCredentialsPath/AndroidPublisherScope 约定，由外层装配处实现并注入（见下）。
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

// ServiceAccountTokenSource 是「生产 service-account OAuth2 token 源」的扩展点接口。
//
// 为何是接口扩展点而非内建实现：本模块**刻意不引入** golang.org/x/oauth2/google 依赖（保持依赖面最小、
// 离线可构建）。生产部署应在外层（cmd/server 装配处）用 service-account 实现本接口并经
// NewGoogleReceiptVerifier 的 tokenSource 参数 / Service.WithVerifier 注入，从而获得「JWT 自动签名 + 自动刷新」。
//
// 推荐的外层实现骨架（伪代码，需在引入 golang.org/x/oauth2 后落地）：
//
//	import (
//	    "golang.org/x/oauth2"
//	    "golang.org/x/oauth2/google"
//	)
//	// 1) 从 env(GOOGLE_APPLICATION_CREDENTIALS) 读 service-account JSON 路径并加载；
//	// 2) google.JWTConfigFromJSON(saJSON, "https://www.googleapis.com/auth/androidpublisher")；
//	// 3) cfg.TokenSource(ctx) → oauth2.ReuseTokenSource 缓存 + 自动刷新；
//	// 4) 适配为本接口：Token(ctx) 调 ts.Token() 取 AccessToken。
//
// 适配为本模块 TokenSource：因签名兼容（均 Token(ctx)(string,error)），实现本接口即可直接当 TokenSource 传入。
type ServiceAccountTokenSource interface {
	TokenSource
}

// ServiceAccountCredentialsPath 返回生产应读取的 service-account JSON 凭据路径（env GOOGLE_APPLICATION_CREDENTIALS）。
// 供外层装配 ServiceAccountTokenSource 时统一取值；本模块自身不解析凭据（不引入 oauth2 依赖），仅暴露约定。
// 返回空表示未配置 → 外层应回退 envTokenSource（静态 token，仅限本地/测试）。
func ServiceAccountCredentialsPath() string {
	return strings.TrimSpace(os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
}

// AndroidPublisherScope 是 Google Play Developer API 所需的 OAuth2 scope。
// 供外层装配 service-account TokenSource 时引用（JWTConfigFromJSON 的 scope 参数），避免散落硬编码。
const AndroidPublisherScope = "https://www.googleapis.com/auth/androidpublisher"

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
//   - tokenSource 为 nil → envTokenSource（读 env GOOGLE_PLAY_ACCESS_TOKEN）。生产请注入 service-account OAuth2。
func NewGoogleReceiptVerifier(httpClient *http.Client, apiBase string, tokenSource TokenSource) *GoogleReceiptVerifier {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(apiBase) == "" {
		apiBase = firstNonEmpty(os.Getenv("GOOGLE_PLAY_API_BASE"), googlePlayAPIBase)
	}
	apiBase = strings.TrimRight(apiBase, "/")
	if tokenSource == nil {
		tokenSource = envTokenSource{}
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
