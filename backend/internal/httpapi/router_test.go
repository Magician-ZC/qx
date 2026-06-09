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

func TestCharterRoutesRegistered(t *testing.T) {
	router := newTestRouter(t)
	// GET：路由已注册且 handler 运行 —— 对不存在的会话取宪章应 404（GetSnapshot 失败），而非 405/未注册。
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/sessions/nope/units/ghost/charter", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("不存在会话取宪章应 404（证明路由已注册 + 鉴权/会话守卫生效），得到 %d (%s)", w.Code, w.Body.String())
	}
	// PUT：同样对不存在会话写宪章应 404（先过 GetSnapshot 会话守卫）。
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, jsonReq(http.MethodPut, "/api/sessions/nope/units/ghost/charter", `{"long_term_goals":["守土"]}`))
	if w2.Code != http.StatusNotFound {
		t.Fatalf("不存在会话写宪章应 404（会话守卫先于落库），得到 %d (%s)", w2.Code, w2.Body.String())
	}
	// DELETE：撤销同理 404。
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, httptest.NewRequest(http.MethodDelete, "/api/sessions/nope/units/ghost/charter", nil))
	if w3.Code != http.StatusNotFound {
		t.Fatalf("不存在会话撤销宪章应 404，得到 %d (%s)", w3.Code, w3.Body.String())
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

func TestAdminFlagsWriteFailClosed(t *testing.T) {
	// 未配 QUNXIANG_OPS_TOKEN 时，GM 写端点 POST /api/admin/flags 必须 fail-closed 返 503，
	// 而非默认放行——否则任何人可开关游戏 flag（反射关键开关）。守卫在请求时读 env，
	// t.Setenv 设空即可覆盖全路由（即便 NewRouter 注册早于本测试）。
	t.Setenv("QUNXIANG_OPS_TOKEN", "")
	router := newTestRouter(t)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, jsonReq(http.MethodPost, "/api/admin/flags", `{"name":"x","value":"true"}`))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("未配 OPS_TOKEN 时写 flag 应 503 fail-closed，得到 %d (%s)", w.Code, w.Body.String())
	}

	// 只读 GET 端点仍走宽松守卫：未配 token 默认放行 200（原型语义不变）。
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, "/api/admin/flags", nil))
	if w2.Code != http.StatusOK {
		t.Fatalf("未配 OPS_TOKEN 时只读 GET flags 应仍 200（宽松守卫），得到 %d (%s)", w2.Code, w2.Body.String())
	}
}

func TestAdminFlagsWritePassesWithToken(t *testing.T) {
	// 配了 OPS_TOKEN 后，带正确 X-Ops-Token 才放行写端点（过守卫，进 handler）。
	// 用未知 flag 名让 handler 返 400「unknown flag」——证明已过守卫进入业务逻辑（非 403/503）。
	t.Setenv("QUNXIANG_OPS_TOKEN", "s3cret-token")
	router := newTestRouter(t)

	// 缺 token → 403（守卫拦截）。
	wNoTok := httptest.NewRecorder()
	router.ServeHTTP(wNoTok, jsonReq(http.MethodPost, "/api/admin/flags", `{"name":"x","value":"true"}`))
	if wNoTok.Code != http.StatusForbidden {
		t.Fatalf("配了 token 但缺 token 头写 flag 应 403，得到 %d (%s)", wNoTok.Code, wNoTok.Body.String())
	}

	// 带对 token → 过守卫；未知 flag 名 → handler 返 400（证明已穿过守卫）。
	req := jsonReq(http.MethodPost, "/api/admin/flags", `{"name":"__nope_unknown_flag__","value":"true"}`)
	req.Header.Set("X-Ops-Token", "s3cret-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("带对 token 写未知 flag 应过守卫并由 handler 返 400，得到 %d (%s)", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "unknown flag") {
		t.Fatalf("应返回 unknown flag 错误（证明已进入业务逻辑）：%s", w.Body.String())
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
