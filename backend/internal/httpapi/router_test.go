package httpapi

// 文件说明：路由注册与命运/遭遇端点的 httptest 测试（无需起真实服务进程）。
// 同时验证新增路由不与既有路由冲突（NewRouter 注册时不 panic）。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

func TestWorldRoutesRoundTrip(t *testing.T) {
	router := newTestRouter(t)

	// 建世界 → 取回 world_id。
	w := httptest.NewRecorder()
	router.ServeHTTP(w, jsonReq(http.MethodPost, "/api/worlds", `{"name":"无尽之环"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("建世界应 200，得到 %d (%s)", w.Code, w.Body.String())
	}
	worldID := extractField(t, w.Body.String(), "world_id")
	if worldID == "" {
		t.Fatalf("应返回 world_id：%s", w.Body.String())
	}

	// 记录一次跨玩家交互（世界时钟发号 → 写入总线）→ 取回 event_id。
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, jsonReq(http.MethodPost, "/api/worlds/"+worldID+"/interactions",
		`{"actor_id":"a_from_shard_1","target_id":"b_from_shard_2","kind":"CROSS_RESCUE","importance":7}`))
	if w2.Code != http.StatusOK {
		t.Fatalf("记录交互应 200，得到 %d (%s)", w2.Code, w2.Body.String())
	}
	if !contains(w2.Body.String(), `"event_id"`) {
		t.Fatalf("应返回 event_id：%s", w2.Body.String())
	}

	// 列出活跃世界应至少含刚建的世界。
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, httptest.NewRequest(http.MethodGet, "/api/worlds", nil))
	if w3.Code != http.StatusOK || !contains(w3.Body.String(), worldID) {
		t.Fatalf("列出世界应含 %s：%d %s", worldID, w3.Code, w3.Body.String())
	}
}

func jsonReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// extractField 从 JSON 串里粗取一个字符串字段值（仅供测试，避免引入解析依赖）。
func extractField(t *testing.T, jsonStr, field string) string {
	t.Helper()
	marker := `"` + field + `":"`
	i := strings.Index(jsonStr, marker)
	if i < 0 {
		return ""
	}
	rest := jsonStr[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}
