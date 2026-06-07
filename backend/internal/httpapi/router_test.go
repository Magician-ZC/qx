package httpapi

// 文件说明：路由注册与命运/遭遇端点的 httptest 测试（无需起真实服务进程）。
// 同时验证新增路由不与既有路由冲突（NewRouter 注册时不 panic）。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"qunxiang/backend/internal/config"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "router.db"))
	if err != nil {
		t.Fatalf("打开 sqlite 失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRouter(Dependencies{Store: db, Config: config.Config{}})
}

func TestHealthzHasAttributionBlock(t *testing.T) {
	router := newTestRouter(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/healthz 应 200，得到 %d", w.Code)
	}
	if !contains(w.Body.String(), `"attribution"`) || !contains(w.Body.String(), `"ooc_rate"`) {
		t.Fatalf("/healthz 应含 attribution 遥测块：%s", w.Body.String())
	}
}

func TestFateInboxRoute(t *testing.T) {
	router := newTestRouter(t)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/fate/inbox/nobody", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET 命运收件箱应 200，得到 %d (%s)", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), `"inbox"`) {
		t.Fatalf("应返回 inbox 字段：%s", w.Body.String())
	}
}

func TestEliteEncounterRouteRegistered(t *testing.T) {
	router := newTestRouter(t)
	// 路由已注册且 handler 运行：对不存在的单位触发应得到 400（GetByID 失败），而非 404。
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/sessions/s1/units/ghost/elite-encounter", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("不存在单位的遭遇触发应 400（证明路由已注册），得到 %d (%s)", w.Code, w.Body.String())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}
