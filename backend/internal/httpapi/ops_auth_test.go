package httpapi

// 文件说明：opt-in 操作者鉴权中间件 opsTokenGuard 的聚焦单元测试。
// 覆盖三态：未配置 env → 放行；配置后缺/错 token → 403；对 token → 放行。
// 用 t.Setenv 隔离 env（自动在用例结束恢复），gin.New()+httptest 直驱中间件。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newGuardedRouter 装一个挂了 opsTokenGuard 的最小路由，命中即返回 200{ok}。
func newGuardedRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/guarded", opsTokenGuard(), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

// doGuarded 发一个带可选 X-Ops-Token 头的请求，返回状态码。token 为空字符串时不带头。
func doGuarded(r *gin.Engine, token string, withHeader bool) int {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
	if withHeader {
		req.Header.Set("X-Ops-Token", token)
	}
	r.ServeHTTP(w, req)
	return w.Code
}

func TestOpsTokenGuard_EnvUnset_AllowsAll(t *testing.T) {
	// 不设 QUNXIANG_OPS_TOKEN：原型默认开放，无论带不带头都放行。
	t.Setenv("QUNXIANG_OPS_TOKEN", "")
	r := newGuardedRouter()

	if got := doGuarded(r, "", false); got != http.StatusOK {
		t.Fatalf("env unset, no header: want 200, got %d", got)
	}
	if got := doGuarded(r, "whatever", true); got != http.StatusOK {
		t.Fatalf("env unset, with header: want 200, got %d", got)
	}
}

func TestOpsTokenGuard_EnvSet_RequiresMatchingToken(t *testing.T) {
	t.Setenv("QUNXIANG_OPS_TOKEN", "s3cret-token")
	r := newGuardedRouter()

	// 缺 token → 403
	if got := doGuarded(r, "", false); got != http.StatusForbidden {
		t.Fatalf("env set, missing header: want 403, got %d", got)
	}
	// 错 token → 403
	if got := doGuarded(r, "wrong-token", true); got != http.StatusForbidden {
		t.Fatalf("env set, wrong token: want 403, got %d", got)
	}
	// 空字符串 token 头（显式提供空值）→ 403
	if got := doGuarded(r, "", true); got != http.StatusForbidden {
		t.Fatalf("env set, empty token header: want 403, got %d", got)
	}
	// 对 token → 放行
	if got := doGuarded(r, "s3cret-token", true); got != http.StatusOK {
		t.Fatalf("env set, correct token: want 200, got %d", got)
	}
}

func TestOpsTokenGuard_Forbidden_BodyShape(t *testing.T) {
	// 403 响应体形状必须是 {"error":"forbidden"}（调用方约定）。
	t.Setenv("QUNXIANG_OPS_TOKEN", "tok")
	r := newGuardedRouter()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", w.Code)
	}
	if body := w.Body.String(); body == "" || !containsForbidden(body) {
		t.Fatalf("want body to contain forbidden error, got %q", body)
	}
}

// containsForbidden 朴素子串检查，避免引入 json 解码依赖。
func containsForbidden(s string) bool {
	const needle = `"error":"forbidden"`
	for i := 0; i+len(needle) <= len(s); i++ {
		if s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
