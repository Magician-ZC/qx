// 文件说明：Apple App Store 收据校验真实网关（替换 service.go 的 stubVerifier 边界）。
//
// 走 Apple 旧版 verifyReceipt 流程（App Store Server API 的 /verifyReceipt 端点）：
//
//	POST {prodEndpoint}，body = {"receipt-data": <receiptBlob>, "password": <sharedSecret>, "exclude-old-transactions": true}
//	响应 JSON 的 status==0 为有效；status==21007 表示「这是沙盒收据但发到了生产端点」→ 自动改投 sandbox 端点重试一次。
//	receiptRef 取 latest_receipt_info[].transaction_id（无则回退 receipt.in_app[].transaction_id）。
//
// 真实可配置：
//   - HTTP client 可注入（默认 http.DefaultClient 加 15s 超时夹合，避免吊死主链路）。
//   - sharedSecret / 端点经构造参数或 env（APPLE_IAP_SHARED_SECRET / APPLE_IAP_VERIFY_ENDPOINT / APPLE_IAP_SANDBOX_ENDPOINT）。
//
// 生产须配真实 endpoint（默认即官方生产/沙盒地址）+ 真实 shared secret（自动续订订阅必填，
// 非自动续订/消耗品可空）；本文件是**真实代码路径**（真的发 HTTP、真的解析 status/transaction_id），
// 非 return true 的占位。注释处明确标注「生产需配真实凭据」。
package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Apple verifyReceipt 官方端点（默认值；可经 env / 构造参数覆盖）。
const (
	appleProdVerifyEndpoint    = "https://buy.itunes.apple.com/verifyReceipt"
	appleSandboxVerifyEndpoint = "https://sandbox.itunes.apple.com/verifyReceipt"

	// appleStatusOK 表示收据有效。
	appleStatusOK = 0
	// appleStatusSandboxReceipt（21007）：把沙盒收据发到了生产端点 → 应改投沙盒端点重试。
	appleStatusSandboxReceipt = 21007
)

// AppleReceiptVerifier 是 Apple App Store 收据校验器（真实 HTTP 网关）。
// 仅处理 platform=="apple"；其它平台返回 ErrPlatformUnsupported（由 platformVerifier 选择器分派）。
type AppleReceiptVerifier struct {
	httpClient      *http.Client
	prodEndpoint    string
	sandboxEndpoint string
	sharedSecret    string
}

// appleVerifyRequest 是 verifyReceipt 的请求体。
type appleVerifyRequest struct {
	ReceiptData            string `json:"receipt-data"`
	Password               string `json:"password,omitempty"`
	ExcludeOldTransactions bool   `json:"exclude-old-transactions"`
}

// appleVerifyResponse 是 verifyReceipt 的响应体（只取本流程关心的字段）。
type appleVerifyResponse struct {
	// Status 用 *int：缺字段时为 nil（而非裸 int 的零值 0=appleStatusOK），避免「2xx 但 body 无 status」被
	// 静默当有效收据放行（评审 load-bearing 修复，与 Google 端 PurchaseState *int 对齐）。
	Status            *int                 `json:"status"`
	Environment       string               `json:"environment"`
	LatestReceiptInfo []appleTransaction   `json:"latest_receipt_info"`
	Receipt           *appleReceiptPayload `json:"receipt"`
}

// appleReceiptPayload 是 receipt 字段（in_app 是首次购买/消耗品的交易列表）。
type appleReceiptPayload struct {
	InApp []appleTransaction `json:"in_app"`
}

// appleTransaction 是单条交易（只取 transaction_id 作 receiptRef）。
type appleTransaction struct {
	TransactionID         string `json:"transaction_id"`
	OriginalTransactionID string `json:"original_transaction_id"`
	ProductID             string `json:"product_id"`
}

// NewAppleReceiptVerifier 构造 Apple 校验器。空参回退 env / 官方默认端点。
//   - httpClient 为 nil → http.DefaultClient（加 15s 超时夹合）。
//   - prodEndpoint / sandboxEndpoint 空 → env(APPLE_IAP_VERIFY_ENDPOINT / APPLE_IAP_SANDBOX_ENDPOINT) → 官方默认。
//   - sharedSecret 空 → env(APPLE_IAP_SHARED_SECRET)。生产对自动续订订阅必填。
func NewAppleReceiptVerifier(httpClient *http.Client, prodEndpoint, sandboxEndpoint, sharedSecret string) *AppleReceiptVerifier {
	if httpClient == nil {
		// 注入点：测试用 httptest server 的 client；生产用带超时的默认 client。
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	if strings.TrimSpace(prodEndpoint) == "" {
		prodEndpoint = firstNonEmpty(os.Getenv("APPLE_IAP_VERIFY_ENDPOINT"), appleProdVerifyEndpoint)
	}
	if strings.TrimSpace(sandboxEndpoint) == "" {
		sandboxEndpoint = firstNonEmpty(os.Getenv("APPLE_IAP_SANDBOX_ENDPOINT"), appleSandboxVerifyEndpoint)
	}
	if strings.TrimSpace(sharedSecret) == "" {
		sharedSecret = os.Getenv("APPLE_IAP_SHARED_SECRET")
	}
	return &AppleReceiptVerifier{
		httpClient:      httpClient,
		prodEndpoint:    prodEndpoint,
		sandboxEndpoint: sandboxEndpoint,
		sharedSecret:    sharedSecret,
	}
}

// Verify 实现 ReceiptVerifier。platform 非 "apple" 返回 ErrPlatformUnsupported（由选择器分派，不应直达）。
// 先打生产端点；status==21007（沙盒收据）则自动改投沙盒端点重试一次。
func (v *AppleReceiptVerifier) Verify(ctx context.Context, platform, receiptBlob string) (bool, string, error) {
	if !strings.EqualFold(strings.TrimSpace(platform), "apple") {
		return false, "", fmt.Errorf("%w: apple verifier got platform %q", ErrPlatformUnsupported, platform)
	}
	if strings.TrimSpace(receiptBlob) == "" {
		return false, "", fmt.Errorf("apple verify: empty receipt blob")
	}

	resp, err := v.post(ctx, v.prodEndpoint, receiptBlob)
	if err != nil {
		return false, "", err
	}
	// 21007：沙盒收据发到了生产端点 → 改投沙盒重试（Apple 官方推荐流程：总是先打生产，遇 21007 再打沙盒）。
	if resp.Status != nil && *resp.Status == appleStatusSandboxReceipt {
		resp, err = v.post(ctx, v.sandboxEndpoint, receiptBlob)
		if err != nil {
			return false, "", err
		}
	}

	if resp.Status == nil || *resp.Status != appleStatusOK {
		// 缺 status（2xx 但 body 无该字段，如代理/CDN 改写）或非 0 状态码（21002 格式错/21003 不可认证/21004 secret 不符等）
		// 一律视为无效收据。返回 ok=false（非 err），让 Purchase 落 failed 流水留痕，绝不静默放行未校验收据。
		return false, "", nil
	}
	return true, v.extractReceiptRef(resp), nil
}

// post 发一次 verifyReceipt 调用并解析响应。非 2xx → err；JSON 解析失败 → err。
func (v *AppleReceiptVerifier) post(ctx context.Context, endpoint, receiptBlob string) (*appleVerifyResponse, error) {
	body, err := json.Marshal(appleVerifyRequest{
		ReceiptData:            receiptBlob,
		Password:               v.sharedSecret,
		ExcludeOldTransactions: true,
	})
	if err != nil {
		return nil, fmt.Errorf("apple verify: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("apple verify: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	httpResp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apple verify: http do: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20)) // 1 MiB 上限，防超大响应。
	if err != nil {
		return nil, fmt.Errorf("apple verify: read body: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("apple verify: unexpected http status %d: %s", httpResp.StatusCode, truncateForErr(raw))
	}

	var decoded appleVerifyResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("apple verify: decode response: %w", err)
	}
	return &decoded, nil
}

// extractReceiptRef 取最稳定的交易引用：优先 latest_receipt_info（订阅最新一条），回退 receipt.in_app（首购/消耗品）。
func (v *AppleReceiptVerifier) extractReceiptRef(resp *appleVerifyResponse) string {
	if resp == nil {
		return ""
	}
	if n := len(resp.LatestReceiptInfo); n > 0 {
		// latest_receipt_info 通常按时间升序；取最后一条作「最新交易」。
		last := resp.LatestReceiptInfo[n-1]
		if id := firstNonEmpty(last.TransactionID, last.OriginalTransactionID); id != "" {
			return "apple:" + id
		}
	}
	if resp.Receipt != nil {
		if n := len(resp.Receipt.InApp); n > 0 {
			last := resp.Receipt.InApp[n-1]
			if id := firstNonEmpty(last.TransactionID, last.OriginalTransactionID); id != "" {
				return "apple:" + id
			}
		}
	}
	return ""
}
