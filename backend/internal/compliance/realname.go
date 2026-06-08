package compliance

// 文件说明：实名认证网关（PRD §5/§9：强制实名前置门）。
//
// 背景 / 为什么存在：service.go 原有的 VerifyRealname(accountID, verified bool) 只把
// 客户端传入的 bool 原样落库——等于「客户端自报实名就算实名」，对出海/版号双闸门形同虚设。
// 本文件提供一个可注入的实名核验抽象 RealnameVerifier，把「真姓名+身份证号 → 是否匹配」的
// 判定从客户端手里拿走、交给真实可配的第三方核验网关（如公安/运营商二要素核验）。
//
// 边界声明（重要）：
//   - stubRealnameVerifier 恒返回 matched=true，仅供本地/测试默认链路使用，是「零行为变化」
//     向后兼容兜底，**生产环境必须用 NewHTTPRealnameVerifierFromEnv 注入真实网关**，否则等于没实名。
//   - HTTPRealnameVerifier 是真实代码路径：真实 POST 到可配 endpoint、真实解析响应，不是 return true 的占位。
//     生产需配真实 endpoint/credentials（QUNXIANG_REALNAME_ENDPOINT / QUNXIANG_REALNAME_TOKEN）。
//
// PII 安全（合规硬约束）：
//   - 真实姓名 + 身份证号是高敏感 PII。本网关只在「请求体」中携带它们，
//     传输层要求 HTTPS（生产 endpoint 必须是 https://，加密信道）；
//   - 绝不把姓名/身份证号写入任何日志（本文件不打印请求体）；
//   - 调用方（service.go）只落库 realname_verified 结果位 + 可选脱敏 ref，绝不落明文 PII。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// RealnameVerifier 抽象实名二要素核验：给定真实姓名与身份证号，返回是否匹配。
// ref 是核验机构返回的脱敏引用/流水号（可空），供审计追溯——不含明文 PII。
type RealnameVerifier interface {
	// Verify 执行一次实名核验。
	// matched=true 表示「姓名与身份证号一致且为真实有效身份」；err 非空表示核验过程本身失败
	// （网络/网关错误），调用方应视为「未通过」处理而非「通过」。
	Verify(ctx context.Context, name, idNumber string) (matched bool, ref string, err error)
}

// ErrRealnameMismatch 表示核验网关明确判定姓名与身份证号不匹配（非系统错误）。
var ErrRealnameMismatch = errors.New("compliance: 实名核验未通过（姓名与身份证号不匹配）")

// ErrInvalidIDNumber 表示身份证号未通过本地基本格式校验（前置拦截，不发起远程请求）。
var ErrInvalidIDNumber = errors.New("compliance: 身份证号格式非法")

// ErrEmptyRealnameInput 表示姓名或身份证号为空。
var ErrEmptyRealnameInput = errors.New("compliance: 姓名/身份证号不能为空")

// ---------------------------------------------------------------------------
// stub 实现：默认兜底，恒通过。仅本地/测试用，生产必换。
// ---------------------------------------------------------------------------

// stubRealnameVerifier 是默认实名核验器——恒返回 matched=true。
// 边界：这是「客户端自报即过」语义的等价物，仅为向后兼容/本地开发存在。
// 生产环境若未注入 HTTPRealnameVerifier，则实名门形同虚设——务必通过 env 配置真实网关。
type stubRealnameVerifier struct{}

// Verify 恒判定通过（stub）。生产必须替换为 HTTPRealnameVerifier。
func (stubRealnameVerifier) Verify(ctx context.Context, name, idNumber string) (bool, string, error) {
	return true, "stub", nil
}

// ---------------------------------------------------------------------------
// HTTP 实现：真实可配的远程二要素核验网关。
// ---------------------------------------------------------------------------

const (
	// defaultRealnameTimeout 实名核验远程调用默认超时。
	defaultRealnameTimeout = 8 * time.Second
	// minRealnameTimeout / maxRealnameTimeout 把超时夹到合理区间，避免配错导致挂死或瞬断。
	minRealnameTimeout = 2 * time.Second
	maxRealnameTimeout = 30 * time.Second
)

// realnameRequestBody 是发往核验网关的请求体（敏感 PII，仅在请求体内携带，不入日志）。
type realnameRequestBody struct {
	Name     string `json:"name"`
	IDNumber string `json:"id_number"`
}

// realnameResponseBody 是核验网关的响应体。
// match 为核验结论；code/message 供错误码透传与审计；ref 为脱敏流水号（不含明文 PII）。
type realnameResponseBody struct {
	Match   bool   `json:"match"`
	Code    string `json:"code"`    // 可选：网关错误码（非空且非成功码视为业务错误）
	Message string `json:"message"` // 可选：网关返回的提示（不应含明文 PII，仅审计）
	Ref     string `json:"ref"`     // 可选：脱敏引用/流水号
}

// HTTPRealnameVerifier 是真实可配的实名核验网关客户端。
// endpoint/token 经构造参数或 env 注入；httpClient 可注入以便 httptest 确定性测试。
//
// 生产部署：endpoint 必须是 https://（PII 传输加密），token 走机密管理，不入代码/日志。
type HTTPRealnameVerifier struct {
	endpoint   string       // 远程核验 endpoint（生产须 https）
	token      string       // 可选鉴权令牌（作 Authorization: Bearer）
	httpClient *http.Client // 可注入；nil 时用带超时的默认 client
}

// HTTPRealnameOption 配置 HTTPRealnameVerifier。
type HTTPRealnameOption func(*HTTPRealnameVerifier)

// WithRealnameHTTPClient 注入自定义 http.Client（测试用 httptest server 的 client）。
func WithRealnameHTTPClient(client *http.Client) HTTPRealnameOption {
	return func(v *HTTPRealnameVerifier) {
		if client != nil {
			v.httpClient = client
		}
	}
}

// WithRealnameToken 注入鉴权令牌。
func WithRealnameToken(token string) HTTPRealnameOption {
	return func(v *HTTPRealnameVerifier) {
		v.token = strings.TrimSpace(token)
	}
}

// NewHTTPRealnameVerifier 构造一个 HTTP 实名核验器。
// endpoint 为空返回 nil（调用方据此回退 stub）。httpClient 默认带 defaultRealnameTimeout 超时。
func NewHTTPRealnameVerifier(endpoint string, opts ...HTTPRealnameOption) *HTTPRealnameVerifier {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil
	}
	v := &HTTPRealnameVerifier{
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: defaultRealnameTimeout},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(v)
		}
	}
	// 注入的 client 若未设超时，补一个合理上限，避免远程挂死拖垮请求。
	if v.httpClient == nil {
		v.httpClient = &http.Client{Timeout: defaultRealnameTimeout}
	} else if v.httpClient.Timeout <= 0 {
		clamped := *v.httpClient
		clamped.Timeout = defaultRealnameTimeout
		v.httpClient = &clamped
	} else if v.httpClient.Timeout < minRealnameTimeout {
		clamped := *v.httpClient
		clamped.Timeout = minRealnameTimeout
		v.httpClient = &clamped
	} else if v.httpClient.Timeout > maxRealnameTimeout {
		clamped := *v.httpClient
		clamped.Timeout = maxRealnameTimeout
		v.httpClient = &clamped
	}
	return v
}

// NewHTTPRealnameVerifierFromEnv 从环境变量构造 HTTP 核验器：
//   - QUNXIANG_REALNAME_ENDPOINT：核验 endpoint（必须；空则返回 nil → 调用方回退 stub）。
//   - QUNXIANG_REALNAME_TOKEN：可选鉴权令牌。
//
// 生产环境配齐这两项即可启用真实核验；不配则零行为变化（走 stub）。
func NewHTTPRealnameVerifierFromEnv() *HTTPRealnameVerifier {
	endpoint := strings.TrimSpace(os.Getenv("QUNXIANG_REALNAME_ENDPOINT"))
	if endpoint == "" {
		return nil
	}
	return NewHTTPRealnameVerifier(endpoint, WithRealnameToken(os.Getenv("QUNXIANG_REALNAME_TOKEN")))
}

// Verify 真实发起远程二要素核验：POST {name, id_number} 到 endpoint，解析 {match, code, ...}。
//
// 流程：
//  1. 前置校验（非空 + 身份证基本格式），不合法直接拒绝，不发起远程请求（省调用 + 早失败）。
//  2. 序列化请求体（仅请求体含 PII，不进日志），POST 到 endpoint（带可选 Bearer token）。
//  3. 解析响应：HTTP 非 2xx → 系统错误；业务 code 非空且非成功 → 业务错误；
//     match=false → matched=false（调用方据此拒绝置位）；match=true → 通过。
func (v *HTTPRealnameVerifier) Verify(ctx context.Context, name, idNumber string) (bool, string, error) {
	name = strings.TrimSpace(name)
	idNumber = strings.ToUpper(strings.TrimSpace(idNumber))
	if name == "" || idNumber == "" {
		return false, "", ErrEmptyRealnameInput
	}
	// 前置：身份证基本格式校验（18 位 + 校验位），不合法不发远程请求。
	if !ValidateChineseID(idNumber) {
		return false, "", ErrInvalidIDNumber
	}

	body, err := json.Marshal(realnameRequestBody{Name: name, IDNumber: idNumber})
	if err != nil {
		return false, "", fmt.Errorf("compliance: 序列化实名请求失败: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, bytes.NewReader(body))
	if err != nil {
		return false, "", fmt.Errorf("compliance: 构造实名请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if v.token != "" {
		req.Header.Set("Authorization", "Bearer "+v.token)
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		// 网络/超时错误：注意绝不在错误信息中回显请求体（PII）。
		return false, "", fmt.Errorf("compliance: 实名核验请求失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 限读响应体，避免恶意/异常大响应。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, "", fmt.Errorf("compliance: 读取实名响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, "", fmt.Errorf("compliance: 实名网关返回非成功状态 %d", resp.StatusCode)
	}

	var parsed realnameResponseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return false, "", fmt.Errorf("compliance: 解析实名响应失败: %w", err)
	}

	// 业务错误码透传：code 非空且非成功码（"0"/"OK"/"SUCCESS"，大小写不敏感）视为网关业务错误。
	if code := strings.TrimSpace(parsed.Code); code != "" && !isSuccessCode(code) {
		return false, parsed.Ref, fmt.Errorf("compliance: 实名网关业务错误 code=%s", code)
	}

	if !parsed.Match {
		// 明确不匹配：非系统错误，但返回 ErrRealnameMismatch 供调用方区分「不匹配」与「网关挂了」。
		return false, parsed.Ref, ErrRealnameMismatch
	}
	return true, parsed.Ref, nil
}

// isSuccessCode 判断网关 code 是否表示成功（容忍常见成功码形态）。
func isSuccessCode(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "0", "00", "000", "OK", "SUCCESS", "PASS":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// 身份证号基本格式校验（前置闸门，纯本地、确定性）。
// ---------------------------------------------------------------------------

// 加权因子（GB 11643-1999，前 17 位各位权重）。
var idWeights = [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}

// 校验码映射（余数 → 第 18 位字符）。
var idCheckCodes = [11]byte{'1', '0', 'X', '9', '8', '7', '6', '5', '4', '3', '2'}

// ValidateChineseID 校验 18 位中国大陆身份证号的基本格式 + 末位校验码（ISO 7064:1983, MOD 11-2）。
// 仅做「格式/校验位」前置校验——真实有效性（户籍/姓名匹配）由远程核验网关判定。
// 第 18 位允许 'X'（已大写）。
func ValidateChineseID(id string) bool {
	id = strings.TrimSpace(id)
	if len(id) != 18 {
		return false
	}
	sum := 0
	for i := 0; i < 17; i++ {
		c := id[i]
		if c < '0' || c > '9' {
			return false
		}
		sum += int(c-'0') * idWeights[i]
	}
	// 出生日期合法性（第 7..14 位 YYYYMMDD）做基本范围校验。
	if !validIDBirthDate(id[6:14]) {
		return false
	}
	last := id[17]
	if last >= 'a' && last <= 'z' {
		last -= 'a' - 'A' // 容错小写
	}
	expected := idCheckCodes[sum%11]
	return last == expected
}

// validIDBirthDate 校验身份证内嵌 8 位出生日期 YYYYMMDD 的基本合法性。
func validIDBirthDate(ymd string) bool {
	if len(ymd) != 8 {
		return false
	}
	t, err := time.Parse("20060102", ymd)
	if err != nil {
		return false
	}
	// 年份落在合理区间（1900 ~ 当前年），避免明显伪造。
	year := t.Year()
	if year < 1900 || year > time.Now().Year() {
		return false
	}
	return true
}
