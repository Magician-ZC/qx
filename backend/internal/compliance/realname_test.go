package compliance

// 文件说明：实名认证网关聚焦测试——用 httptest mock 实名 endpoint 做确定性核验。
// 断言：match=true → VerifyRealnameWithIdentity 成功且 realname_verified 落 1；
//       match=false → 返回 ErrRealnameMismatch 且不置位；
//       身份证格式非法 → 前置拒绝（不发起远程请求）；
//       stub 默认恒过；HTTP 网关解析真实响应（注入 mock client，确定性）。
// 复用 compliance_test.go 的 complianceTestDB / fixedClock / at 辅助。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 测试用合法身份证号（18 位 + 校验位通过 MOD 11-2，内嵌出生日期合法）。
// 由校验算法反推得到，仅用于格式校验通过——非真实身份。
const (
	validTestID   = "11010119900307002X" // 1990-03-07，末位 X 为正确校验码（验证 X 容错路径）
	validTestID2  = "110101199003072615" // 1990-03-07，末位数字 5 为正确校验码
	invalidTestID = "11010119900307002A" // 末位校验码错误（应被前置拒绝）
)

// realnameRoundTrip 是一个把所有请求路由到指定 handler 的 RoundTripper，
// 让我们能在不监听端口的前提下注入 mock（也可直接用 httptest.Server，这里两种都演示）。
type realnameRoundTrip struct {
	handler http.HandlerFunc
}

func (rt realnameRoundTrip) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.handler(rec, req)
	return rec.Result(), nil
}

func TestValidateChineseID(t *testing.T) {
	if !ValidateChineseID(validTestID) {
		t.Fatalf("合法身份证号 %s 应通过格式校验", validTestID)
	}
	if !ValidateChineseID(validTestID2) {
		t.Fatalf("合法身份证号 %s 应通过格式校验", validTestID2)
	}
	if ValidateChineseID(invalidTestID) {
		t.Fatalf("校验位错误的身份证号 %s 不应通过", invalidTestID)
	}
	if ValidateChineseID("12345") {
		t.Fatalf("长度不足应拒绝")
	}
	// 非数字字符（前 17 位含字母）应拒绝。
	if ValidateChineseID("1101011990030700AX") {
		t.Fatalf("前 17 位含非数字应拒绝")
	}
	// 非法出生月份（13 月）：构造一个 18 位串，让出生日期校验失败。
	if ValidateChineseID("11010119901307002X") {
		t.Fatalf("非法出生月份应拒绝")
	}
	if ValidateChineseID("") {
		t.Fatalf("空串应拒绝")
	}
	// 小写 x 容错。
	if !ValidateChineseID(strings.ToLower(validTestID)) {
		t.Fatalf("小写末位 x 应容错通过")
	}
}

func TestStubRealnameVerifierAlwaysPasses(t *testing.T) {
	var v RealnameVerifier = stubRealnameVerifier{}
	matched, ref, err := v.Verify(context.Background(), "任意", "任意")
	if err != nil || !matched {
		t.Fatalf("stub 应恒通过，得到 matched=%v err=%v", matched, err)
	}
	if ref != "stub" {
		t.Fatalf("stub ref 应为 stub，得到 %q", ref)
	}
}

func TestServiceDefaultVerifierIsStub(t *testing.T) {
	// 未配 env、未注入 → 默认 stub，VerifyRealnameWithIdentity 恒过并落位。
	t.Setenv("QUNXIANG_REALNAME_ENDPOINT", "")
	db := complianceTestDB(t)
	svc := NewService(db)
	ctx := context.Background()

	if err := svc.VerifyRealnameWithIdentity(ctx, "acc-stub", "张三", validTestID); err != nil {
		t.Fatalf("默认 stub 应通过，得到 err=%v", err)
	}
	assertRealnameVerified(t, db, "acc-stub", true)
}

func TestVerifyRealnameWithIdentityMatchTrueSetsFlag(t *testing.T) {
	db := complianceTestDB(t)
	ctx := context.Background()

	// mock endpoint：断言收到 PII 在 body 内、返回 match=true。
	var gotName, gotID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("应为 POST，得到 %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Name     string `json:"name"`
			IDNumber string `json:"id_number"`
		}
		_ = json.Unmarshal(body, &parsed)
		gotName, gotID = parsed.Name, parsed.IDNumber
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"match": true, "code": "0", "ref": "ref-***123"}`))
	}))
	t.Cleanup(server.Close)

	verifier := NewHTTPRealnameVerifier(server.URL, WithRealnameHTTPClient(server.Client()))
	svc := NewService(db).WithRealnameVerifier(verifier)

	if err := svc.VerifyRealnameWithIdentity(ctx, "acc-1", "李四", validTestID); err != nil {
		t.Fatalf("match=true 应成功，得到 err=%v", err)
	}
	if gotName != "李四" || gotID != validTestID {
		t.Fatalf("网关应收到 PII，得到 name=%q id=%q", gotName, gotID)
	}
	assertRealnameVerified(t, db, "acc-1", true)
}

func TestVerifyRealnameWithIdentityMatchFalseDoesNotSetFlag(t *testing.T) {
	db := complianceTestDB(t)
	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"match": false, "code": "0"}`))
	}))
	t.Cleanup(server.Close)

	verifier := NewHTTPRealnameVerifier(server.URL, WithRealnameHTTPClient(server.Client()))
	svc := NewService(db).WithRealnameVerifier(verifier)

	err := svc.VerifyRealnameWithIdentity(ctx, "acc-2", "王五", validTestID)
	if !errors.Is(err, ErrRealnameMismatch) {
		t.Fatalf("match=false 应返回 ErrRealnameMismatch，得到 %v", err)
	}
	// 不应建行/置位（账号此前无行 → load 返回零值，未触发 save）。
	assertRealnameVerified(t, db, "acc-2", false)
}

func TestVerifyRealnameWithIdentityInvalidIDRejectedPreflight(t *testing.T) {
	db := complianceTestDB(t)
	ctx := context.Background()

	// 远程被调用即测试失败——格式非法应在前置拦截，不发请求。
	called := false
	verifier := NewHTTPRealnameVerifier("https://realname.example/verify",
		WithRealnameHTTPClient(&http.Client{Transport: realnameRoundTrip{
			handler: func(w http.ResponseWriter, r *http.Request) {
				called = true
				_, _ = w.Write([]byte(`{"match": true}`))
			},
		}}))
	svc := NewService(db).WithRealnameVerifier(verifier)

	err := svc.VerifyRealnameWithIdentity(ctx, "acc-3", "赵六", invalidTestID)
	if !errors.Is(err, ErrInvalidIDNumber) {
		t.Fatalf("格式非法应返回 ErrInvalidIDNumber，得到 %v", err)
	}
	if called {
		t.Fatalf("格式非法不应发起远程请求")
	}
	assertRealnameVerified(t, db, "acc-3", false)
}

func TestVerifyRealnameWithIdentityGatewayErrorNotSet(t *testing.T) {
	db := complianceTestDB(t)
	ctx := context.Background()

	// 网关返回 5xx → 系统错误，不置位。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(server.Close)

	verifier := NewHTTPRealnameVerifier(server.URL, WithRealnameHTTPClient(server.Client()))
	svc := NewService(db).WithRealnameVerifier(verifier)

	if err := svc.VerifyRealnameWithIdentity(ctx, "acc-4", "钱七", validTestID); err == nil {
		t.Fatalf("网关 5xx 应返回错误")
	}
	assertRealnameVerified(t, db, "acc-4", false)
}

func TestVerifyRealnameWithIdentityBusinessErrorCode(t *testing.T) {
	db := complianceTestDB(t)
	ctx := context.Background()

	// HTTP 200 但业务 code 非成功 → 业务错误，不置位。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"match": false, "code": "E1001", "message": "id not found"}`))
	}))
	t.Cleanup(server.Close)

	verifier := NewHTTPRealnameVerifier(server.URL, WithRealnameHTTPClient(server.Client()))
	svc := NewService(db).WithRealnameVerifier(verifier)

	err := svc.VerifyRealnameWithIdentity(ctx, "acc-5", "孙八", validTestID)
	if err == nil {
		t.Fatalf("业务错误码应返回错误")
	}
	assertRealnameVerified(t, db, "acc-5", false)
}

func TestHTTPVerifierFromEnv(t *testing.T) {
	// 未配 endpoint → nil（调用方回退 stub）。
	t.Setenv("QUNXIANG_REALNAME_ENDPOINT", "")
	if v := NewHTTPRealnameVerifierFromEnv(); v != nil {
		t.Fatalf("未配 endpoint 应返回 nil")
	}
	// 配了 endpoint → 非 nil。
	t.Setenv("QUNXIANG_REALNAME_ENDPOINT", "https://realname.example/verify")
	t.Setenv("QUNXIANG_REALNAME_TOKEN", "tok-abc")
	v := NewHTTPRealnameVerifierFromEnv()
	if v == nil {
		t.Fatalf("配了 endpoint 应返回非 nil")
	}
	if v.token != "tok-abc" {
		t.Fatalf("token 应被注入，得到 %q", v.token)
	}
}

func TestHTTPVerifierSendsBearerToken(t *testing.T) {
	var gotAuth string
	verifier := NewHTTPRealnameVerifier("https://realname.example/verify",
		WithRealnameToken("secret-tok"),
		WithRealnameHTTPClient(&http.Client{Transport: realnameRoundTrip{
			handler: func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"match": true}`))
			},
		}}))
	matched, _, err := verifier.Verify(context.Background(), "周九", validTestID)
	if err != nil || !matched {
		t.Fatalf("应通过，得到 matched=%v err=%v", matched, err)
	}
	if gotAuth != "Bearer secret-tok" {
		t.Fatalf("应携带 Bearer token，得到 %q", gotAuth)
	}
}

func TestHTTPVerifierEmptyInputRejected(t *testing.T) {
	verifier := NewHTTPRealnameVerifier("https://realname.example/verify")
	if _, _, err := verifier.Verify(context.Background(), "", validTestID); !errors.Is(err, ErrEmptyRealnameInput) {
		t.Fatalf("空姓名应拒绝，得到 %v", err)
	}
	if _, _, err := verifier.Verify(context.Background(), "名", ""); !errors.Is(err, ErrEmptyRealnameInput) {
		t.Fatalf("空身份证号应拒绝，得到 %v", err)
	}
}

func TestNewHTTPRealnameVerifierEmptyEndpointNil(t *testing.T) {
	if v := NewHTTPRealnameVerifier("  "); v != nil {
		t.Fatalf("空 endpoint 应返回 nil")
	}
}

// assertRealnameVerified 读回 account_compliance 断言 realname_verified 位。
// 账号无行（从未置位）时视为 false。同时断言绝不落明文 PII（无 name/id_number 列）。
func assertRealnameVerified(t *testing.T, db *sql.DB, accountID string, want bool) {
	t.Helper()
	var realname sql.NullInt64
	err := db.QueryRowContext(context.Background(),
		`SELECT realname_verified FROM account_compliance WHERE account_id = ?`, accountID).
		Scan(&realname)
	if err == sql.ErrNoRows {
		if want {
			t.Fatalf("账号 %s 无行，但期望 realname_verified=1", accountID)
		}
		return
	}
	if err != nil {
		t.Fatalf("读回 realname_verified 报错: %v", err)
	}
	got := realname.Int64 != 0
	if got != want {
		t.Fatalf("账号 %s realname_verified 期望 %v，得到 %v", accountID, want, got)
	}
}
