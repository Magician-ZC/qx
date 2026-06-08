// 文件说明：Apple/Google 真实收据校验网关的聚焦单元测试。
// 用 httptest mock server 注入端点 + 注入 http client / token source 做确定性断言，覆盖：
//
//	Apple：status==0 → ok=true+receiptRef(transaction_id)；status!=0 → ok=false；
//	       21007（沙盒收据发到生产端点）→ 自动改投 sandbox 端点重试；非 2xx → err。
//	Google：purchaseState==0 → ok=true+receiptRef(orderId)；purchaseState!=0 → ok=false；
//	        404 → ok=false；401/5xx → err；receiptBlob 非法 JSON → err。
//	选择器：platformVerifier 命中 apple/google 真实网关；未知平台走 fallback（denyVerifier 安全拒绝）。
//	NewService：QUNXIANG_IAP_REAL 开 → 装真实选择器；默认 → stub。
//
// 测试名带 iapVerifier 前缀避免与并行 agent / 既有 billing_test 撞名。
package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Apple ---

func TestIAPVerifierAppleValidReceipt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 断言请求体含 receipt-data 与 password。
		var body appleVerifyRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解析 apple 请求体失败: %v", err)
		}
		if body.ReceiptData != "RAW_APPLE_RECEIPT" {
			t.Errorf("receipt-data 应原样透传，得到 %q", body.ReceiptData)
		}
		if body.Password != "shh-secret" {
			t.Errorf("password 应为注入的 shared secret，得到 %q", body.Password)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      0,
			"environment": "Production",
			"latest_receipt_info": []map[string]any{
				{"transaction_id": "1000000111", "product_id": "sku-month"},
				{"transaction_id": "1000000222", "product_id": "sku-month"},
			},
		})
	}))
	defer srv.Close()

	v := NewAppleReceiptVerifier(srv.Client(), srv.URL, srv.URL, "shh-secret")
	ok, ref, err := v.Verify(context.Background(), "apple", "RAW_APPLE_RECEIPT")
	if err != nil {
		t.Fatalf("有效收据不应返回 err: %v", err)
	}
	if !ok {
		t.Fatalf("status==0 应 ok=true")
	}
	// 取 latest_receipt_info 最后一条的 transaction_id。
	if ref != "apple:1000000222" {
		t.Fatalf("receiptRef 应取最新 transaction_id，得到 %q", ref)
	}
}

func TestIAPVerifierAppleInvalidStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 21002}) // 21002 = receipt-data 格式错。
	}))
	defer srv.Close()

	v := NewAppleReceiptVerifier(srv.Client(), srv.URL, srv.URL, "")
	ok, ref, err := v.Verify(context.Background(), "apple", "BLOB")
	if err != nil {
		t.Fatalf("无效 status 应返回 ok=false 而非 err: %v", err)
	}
	if ok {
		t.Fatalf("status!=0 应 ok=false")
	}
	if ref != "" {
		t.Fatalf("无效收据 receiptRef 应空，得到 %q", ref)
	}
}

// TestIAPVerifierAppleMissingStatusNotAllowed 验证 2xx 但 body 缺 status 字段（代理/CDN 改写）时**绝不静默放行**
// （评审 load-bearing 修复：Status 裸 int 缺字段会解码为 0=OK）。
func TestIAPVerifierAppleMissingStatusNotAllowed(t *testing.T) {
	for _, body := range []map[string]any{{}, {"environment": "Production"}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(body)
		}))
		v := NewAppleReceiptVerifier(srv.Client(), srv.URL, srv.URL, "")
		ok, ref, err := v.Verify(context.Background(), "apple", "BLOB")
		srv.Close()
		if err != nil {
			t.Fatalf("缺 status 应 ok=false 而非 err: %v", err)
		}
		if ok || ref != "" {
			t.Fatalf("缺 status 字段绝不应放行，得到 ok=%v ref=%q（body=%v）", ok, ref, body)
		}
	}
}

func TestIAPVerifierAppleSandboxRetry(t *testing.T) {
	var prodHits, sandboxHits int
	// 生产端点：返回 21007（这是沙盒收据）。
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prodHits++
		_ = json.NewEncoder(w).Encode(map[string]any{"status": 21007})
	}))
	defer prod.Close()
	// 沙盒端点：返回 status 0 + 交易。
	sandbox := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sandboxHits++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":      0,
			"environment": "Sandbox",
			"latest_receipt_info": []map[string]any{
				{"transaction_id": "sandbox-tx-77"},
			},
		})
	}))
	defer sandbox.Close()

	v := NewAppleReceiptVerifier(prod.Client(), prod.URL, sandbox.URL, "")
	ok, ref, err := v.Verify(context.Background(), "apple", "SANDBOX_BLOB")
	if err != nil {
		t.Fatalf("21007 自动重试沙盒不应 err: %v", err)
	}
	if !ok {
		t.Fatalf("沙盒重试 status==0 应 ok=true")
	}
	if ref != "apple:sandbox-tx-77" {
		t.Fatalf("应取沙盒交易 id，得到 %q", ref)
	}
	if prodHits != 1 || sandboxHits != 1 {
		t.Fatalf("应先打生产 1 次再打沙盒 1 次，得到 prod=%d sandbox=%d", prodHits, sandboxHits)
	}
}

func TestIAPVerifierAppleNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	v := NewAppleReceiptVerifier(srv.Client(), srv.URL, srv.URL, "")
	ok, _, err := v.Verify(context.Background(), "apple", "BLOB")
	if err == nil {
		t.Fatalf("非 2xx 应返回 err")
	}
	if ok {
		t.Fatalf("err 时 ok 应为 false")
	}
}

func TestIAPVerifierAppleWrongPlatform(t *testing.T) {
	v := NewAppleReceiptVerifier(http.DefaultClient, "http://unused", "http://unused", "")
	_, _, err := v.Verify(context.Background(), "google", "BLOB")
	if err == nil {
		t.Fatalf("apple verifier 收到非 apple 平台应返回 err")
	}
}

func TestIAPVerifierAppleEmptyBlob(t *testing.T) {
	v := NewAppleReceiptVerifier(http.DefaultClient, "http://unused", "http://unused", "")
	_, _, err := v.Verify(context.Background(), "apple", "   ")
	if err == nil {
		t.Fatalf("空收据应返回 err（不发请求）")
	}
}

// --- Google ---

func googleBlob(t *testing.T, pkg, product, token string) string {
	t.Helper()
	b, err := json.Marshal(googleReceiptBlob{Package: pkg, ProductID: product, Token: token})
	if err != nil {
		t.Fatalf("构造 google blob 失败: %v", err)
	}
	return string(b)
}

func TestIAPVerifierGoogleValidPurchase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 断言路径与 Bearer。
		wantPath := "/androidpublisher/v3/applications/com.qx.game/purchases/products/sku-month/tokens/tok-abc"
		if r.URL.Path != wantPath {
			t.Errorf("路径不符:\n want %s\n got  %s", wantPath, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-access-token" {
			t.Errorf("Authorization 应带注入 token，得到 %q", auth)
		}
		state := googlePurchaseStatePurchased
		_ = json.NewEncoder(w).Encode(map[string]any{
			"purchaseState":      state,
			"orderId":            "GPA.1234-5678",
			"purchaseTimeMillis": "1700000000000",
		})
	}))
	defer srv.Close()

	v := NewGoogleReceiptVerifier(srv.Client(), srv.URL, StaticTokenSource("test-access-token"))
	ok, ref, err := v.Verify(context.Background(), "google", googleBlob(t, "com.qx.game", "sku-month", "tok-abc"))
	if err != nil {
		t.Fatalf("有效购买不应 err: %v", err)
	}
	if !ok {
		t.Fatalf("purchaseState==0 应 ok=true")
	}
	if ref != "google:GPA.1234-5678" {
		t.Fatalf("receiptRef 应取 orderId，得到 %q", ref)
	}
}

func TestIAPVerifierGoogleCanceledPurchase(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := 1 // 已取消。
		_ = json.NewEncoder(w).Encode(map[string]any{"purchaseState": state, "orderId": "GPA.x"})
	}))
	defer srv.Close()

	v := NewGoogleReceiptVerifier(srv.Client(), srv.URL, StaticTokenSource("tok"))
	ok, _, err := v.Verify(context.Background(), "google", googleBlob(t, "p", "sku", "t"))
	if err != nil {
		t.Fatalf("已取消购买应 ok=false 而非 err: %v", err)
	}
	if ok {
		t.Fatalf("purchaseState!=0 应 ok=false")
	}
}

func TestIAPVerifierGoogleNotFoundIsInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	v := NewGoogleReceiptVerifier(srv.Client(), srv.URL, StaticTokenSource("tok"))
	ok, _, err := v.Verify(context.Background(), "google", googleBlob(t, "p", "sku", "t"))
	if err != nil {
		t.Fatalf("404 应 ok=false 而非 err: %v", err)
	}
	if ok {
		t.Fatalf("404（token 不存在）应 ok=false")
	}
}

func TestIAPVerifierGoogleUnauthorizedIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	v := NewGoogleReceiptVerifier(srv.Client(), srv.URL, StaticTokenSource("tok"))
	ok, _, err := v.Verify(context.Background(), "google", googleBlob(t, "p", "sku", "t"))
	if err == nil {
		t.Fatalf("401（凭据失效）应返回 err，不静默放行")
	}
	if ok {
		t.Fatalf("err 时 ok 应为 false")
	}
}

func TestIAPVerifierGoogleBadBlobIsError(t *testing.T) {
	v := NewGoogleReceiptVerifier(http.DefaultClient, "http://unused", StaticTokenSource("tok"))
	if _, _, err := v.Verify(context.Background(), "google", "not-json"); err == nil {
		t.Fatalf("非 JSON receiptBlob 应返回 err")
	}
	if _, _, err := v.Verify(context.Background(), "google", googleBlob(t, "", "sku", "t")); err == nil {
		t.Fatalf("缺 package 应返回 err")
	}
}

func TestIAPVerifierGoogleWrongPlatform(t *testing.T) {
	v := NewGoogleReceiptVerifier(http.DefaultClient, "http://unused", StaticTokenSource("tok"))
	if _, _, err := v.Verify(context.Background(), "apple", "{}"); err == nil {
		t.Fatalf("google verifier 收到非 google 平台应返回 err")
	}
}

func TestIAPVerifierGoogleMissingTokenSource(t *testing.T) {
	// envTokenSource 在 env 未设时应报错（不发请求）。
	t.Setenv("GOOGLE_PLAY_ACCESS_TOKEN", "")
	v := NewGoogleReceiptVerifier(http.DefaultClient, "http://unused", nil)
	if _, _, err := v.Verify(context.Background(), "google", googleBlob(t, "p", "sku", "t")); err == nil {
		t.Fatalf("无 access token 应返回 err（不静默放行）")
	}
}

// --- 选择器 / NewService ---

func TestIAPVerifierPlatformDispatch(t *testing.T) {
	// apple/google 命中真实网关（用 mock server）；未知平台走 fallback(denyVerifier) → ok=false。
	apple := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":              0,
			"latest_receipt_info": []map[string]any{{"transaction_id": "tx-apple"}},
		})
	}))
	defer apple.Close()

	pv := &platformVerifier{
		verifiers: map[string]ReceiptVerifier{
			"apple": NewAppleReceiptVerifier(apple.Client(), apple.URL, apple.URL, ""),
		},
		fallback: denyVerifier{},
	}

	ok, ref, err := pv.Verify(context.Background(), "apple", "BLOB")
	if err != nil || !ok || ref != "apple:tx-apple" {
		t.Fatalf("apple 分派应命中真实网关: ok=%v ref=%q err=%v", ok, ref, err)
	}

	// 未注册平台 → fallback denyVerifier → ok=false, 无 err。
	ok, _, err = pv.Verify(context.Background(), "unknownstore", "BLOB")
	if err != nil {
		t.Fatalf("未知平台走 denyVerifier 不应 err: %v", err)
	}
	if ok {
		t.Fatalf("未配网关的平台应安全拒绝（ok=false）")
	}
}

func TestIAPVerifierSelectVerifierByEnv(t *testing.T) {
	t.Setenv("QUNXIANG_IAP_REAL", "")
	if _, isStub := selectVerifier().(stubVerifier); !isStub {
		t.Fatalf("QUNXIANG_IAP_REAL 未设时应回退 stubVerifier")
	}

	t.Setenv("QUNXIANG_IAP_REAL", "1")
	v := selectVerifier()
	pv, ok := v.(*platformVerifier)
	if !ok {
		t.Fatalf("QUNXIANG_IAP_REAL=1 时应装真实 platformVerifier，得到 %T", v)
	}
	if _, has := pv.verifiers["apple"]; !has {
		t.Fatalf("真实选择器应注册 apple 网关")
	}
	if _, has := pv.verifiers["google"]; !has {
		t.Fatalf("真实选择器应注册 google 网关")
	}
	if _, isDeny := pv.fallback.(denyVerifier); !isDeny {
		t.Fatalf("真实选择器 fallback 应为 denyVerifier（安全拒绝），得到 %T", pv.fallback)
	}
}

func TestIAPVerifierWithFallbackStub(t *testing.T) {
	t.Setenv("QUNXIANG_IAP_REAL", "1")
	svc := (&Service{verifier: selectVerifier()}).WithFallback(stubVerifier{})
	pv, ok := svc.verifier.(*platformVerifier)
	if !ok {
		t.Fatalf("应仍是 platformVerifier")
	}
	if _, isStub := pv.fallback.(stubVerifier); !isStub {
		t.Fatalf("WithFallback(stub) 应替换 fallback 为 stubVerifier")
	}
	// 未知平台经 stub fallback → ok=true（仅测试/灰度语义）。
	ok2, _, err := pv.Verify(context.Background(), "anything", "blob-data-here")
	if err != nil || !ok2 {
		t.Fatalf("stub fallback 应放行未知平台: ok=%v err=%v", ok2, err)
	}
}

// TestIAPVerifierTruncateForErr 守护错误信息截断助手（避免超大 body 进日志）。
func TestIAPVerifierTruncateForErr(t *testing.T) {
	long := strings.Repeat("x", 1000)
	got := truncateForErr([]byte(long))
	if len(got) > 300 {
		t.Fatalf("应截断长 body，得到长度 %d", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断后应有省略号标记")
	}
}
