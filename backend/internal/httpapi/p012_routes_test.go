package httpapi

// 文件说明：P0/P2 路由改造的聚焦 httptest 测试（特性前缀 p012，避免与既有测试撞名）。
// 覆盖三组改造：
//   ① 高危端点鉴权：audit / privacy-erase / privacy-purge / reports 在配置 QUNXIANG_OPS_TOKEN
//      后缺 token 必须 403；未配置时默认开放（不被本守卫拦）。
//   ② 商业化端点：flag QUNXIANG_BILLING_ENABLED 关时整组不注册（404）；开时已注册（非 404）。
//   ③ 合规端点：flag QUNXIANG_COMPLIANCE_ENABLED 关时整组不注册（404）；开时已注册（非 404）。
// 用 t.Setenv 隔离 env（结束自动恢复）；sqlite 临时库经 NewRouter 装配真实路由树。
// 不运行真实服务进程，纯 httptest 直驱。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/config"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

// p012Router 装配一个真实路由树（含本次改造的守卫与 flag-gated 端点）。
// 调用方应在 NewRouter 之前用 t.Setenv 设好 flag/token，因为注册期会读 env。
func p012Router(t *testing.T) http.Handler {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "p012.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRouter(Dependencies{Store: db, Config: config.Config{}})
}

// p012Status 发一个请求（可选 X-Ops-Token 头），返回状态码与响应体。
func p012Status(r http.Handler, method, path, token string) (int, string) {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("X-Ops-Token", token)
	}
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// ① 高危端点：配置 OPS_TOKEN 后缺 token → 403（证明守卫已套上，修复越权）。
func TestP012HighRiskEndpointsRequireOpsToken(t *testing.T) {
	t.Setenv("QUNXIANG_OPS_TOKEN", "p012-secret")
	r := p012Router(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"audit_bundle_exposes_llm_prompts", http.MethodGet, "/api/sessions/s1/audit"},
		{"privacy_erase_irreversible", http.MethodPost, "/api/sessions/s1/privacy/erase"},
		{"privacy_purge_bulk", http.MethodPost, "/api/privacy/purge"},
		{"moderation_report_write", http.MethodPost, "/api/sessions/s1/reports"},
	}
	for _, tc := range cases {
		// 缺 token → 403
		if code, body := p012Status(r, tc.method, tc.path, ""); code != http.StatusForbidden {
			t.Fatalf("%s 缺 token 应 403，得到 %d (%s)", tc.name, code, body)
		}
		// 错 token → 403
		if code, body := p012Status(r, tc.method, tc.path, "wrong"); code != http.StatusForbidden {
			t.Fatalf("%s 错 token 应 403，得到 %d (%s)", tc.name, code, body)
		}
	}
}

// ① 守卫的 403 必须来自 opsTokenGuard（响应体形状 {"error":"forbidden"}）。
func TestP012HighRiskForbiddenBodyShape(t *testing.T) {
	t.Setenv("QUNXIANG_OPS_TOKEN", "p012-secret")
	r := p012Router(t)

	code, body := p012Status(r, http.MethodGet, "/api/sessions/s1/audit", "")
	if code != http.StatusForbidden {
		t.Fatalf("audit 缺 token 应 403，得到 %d", code)
	}
	if !p012Contains(body, `"error":"forbidden"`) {
		t.Fatalf("403 体应是 opsTokenGuard 的 forbidden 形状，得到 %q", body)
	}
}

// ① 未配置 OPS_TOKEN 且无登记 operator 时的 RBAC 降级语义：
//   - 只读端点（audit，opsViewer）默认开放——不应因守卫 403/503（向后兼容原型语义）。
//   - 高危破坏性写端点（purge，opsAdmin）fail-closed——未配鉴权应返 503（拒绝服务），绝不默认放行。
// 注：handler 自身可能因路径不存在/请求非法返回其它码（400/404），故只读端点只断言「非 403/503」。
func TestP012HighRiskOpenWhenTokenUnset(t *testing.T) {
	t.Setenv("QUNXIANG_OPS_TOKEN", "")
	r := p012Router(t)

	// 只读 audit（viewer 档）：降级开放，不应被守卫拒（403/503）。
	if code, body := p012Status(r, http.MethodGet, "/api/sessions/missing/audit", ""); code == http.StatusForbidden || code == http.StatusServiceUnavailable {
		t.Fatalf("未配置 token 时只读 audit 不应被守卫拒，得到 %d (%s)", code, body)
	}
	// 高危 purge（admin 档）：未配鉴权 fail-closed，必须 503（不可默认放行做跨会话删数据）。
	if code, body := p012Status(r, http.MethodPost, "/api/privacy/purge", ""); code != http.StatusServiceUnavailable {
		t.Fatalf("未配置 token 时高危 purge 应 fail-closed 503，得到 %d (%s)", code, body)
	}
}

// ② 商业化 flag 关：整组端点不注册 → 404（gin 对未注册路由返 404）。
func TestP012BillingEndpointsAbsentWhenFlagOff(t *testing.T) {
	t.Setenv("QUNXIANG_BILLING_ENABLED", "")
	r := p012Router(t)

	for _, p := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/billing/skus"},
		{http.MethodPost, "/api/billing/purchase"},
		{http.MethodGet, "/api/billing/quota/acc1"},
	} {
		if code, body := p012Status(r, p.method, p.path, ""); code != http.StatusNotFound {
			t.Fatalf("flag 关时 %s 应未注册 404，得到 %d (%s)", p.path, code, body)
		}
	}
}

// ② 商业化 flag 开：端点已注册 → 非 404（GET skus/quota 应可直达 handler）。
func TestP012BillingEndpointsPresentWhenFlagOn(t *testing.T) {
	t.Setenv("QUNXIANG_BILLING_ENABLED", "on")
	r := p012Router(t)

	if code, body := p012Status(r, http.MethodGet, "/api/billing/skus", ""); code == http.StatusNotFound {
		t.Fatalf("flag 开时 GET /api/billing/skus 应已注册（非 404），得到 %d (%s)", code, body)
	}
	if code, body := p012Status(r, http.MethodGet, "/api/billing/quota/acc1", ""); code == http.StatusNotFound {
		t.Fatalf("flag 开时 GET /api/billing/quota/:accountId 应已注册（非 404），得到 %d (%s)", code, body)
	}
}

// ③ 合规 flag 关：整组端点不注册 → 404。
func TestP012ComplianceEndpointsAbsentWhenFlagOff(t *testing.T) {
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "")
	r := p012Router(t)

	for _, p := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/compliance/verify"},
		{http.MethodGet, "/api/compliance/gate/acc1"},
	} {
		if code, body := p012Status(r, p.method, p.path, ""); code != http.StatusNotFound {
			t.Fatalf("flag 关时 %s 应未注册 404，得到 %d (%s)", p.path, code, body)
		}
	}
}

// ③ 合规 flag 开：gate 端点已注册 → 非 404。
func TestP012ComplianceGatePresentWhenFlagOn(t *testing.T) {
	t.Setenv("QUNXIANG_COMPLIANCE_ENABLED", "yes")
	r := p012Router(t)

	if code, body := p012Status(r, http.MethodGet, "/api/compliance/gate/acc1", ""); code == http.StatusNotFound {
		t.Fatalf("flag 开时 GET /api/compliance/gate/:accountId 应已注册（非 404），得到 %d (%s)", code, body)
	}
}

// p012Contains 朴素子串检查（避免引入 json 解码依赖，且不与既有 contains 撞名）。
func p012Contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}
