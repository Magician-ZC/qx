package httpapi

// 文件说明：假门留资端点测试（POST /api/leads 落库 + GET /api/ops/leads-funnel 漏斗）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	sqlitestore "qunxiang/backend/internal/storage/sqlite"
)

func TestLeadEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "leads.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := gin.New()
	registerLeadEndpoints(r, db)

	post := func(body string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/leads", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w.Code
	}
	if code := post(`{"kind":"lead","vid":"v1","email":"a@b.com","utm_source":"x"}`); code != http.StatusCreated {
		t.Fatalf("POST lead 应 201，得 %d", code)
	}
	if code := post(`{"kind":"survey","vid":"v2"}`); code != http.StatusCreated {
		t.Fatalf("POST survey 应 201，得 %d", code)
	}
	if code := post(`not json`); code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400，得 %d", code)
	}

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/ops/leads-funnel", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("funnel 应 200，得 %d", w.Code)
	}
	var out struct {
		Total          int            `json:"total"`
		ByKind         map[string]int `json:"by_kind"`
		UniqueVisitors int            `json:"unique_visitors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析 funnel: %v", err)
	}
	if out.Total != 2 || out.ByKind["lead"] != 1 || out.ByKind["survey"] != 1 || out.UniqueVisitors != 2 {
		t.Fatalf("漏斗聚合不符: %+v", out)
	}

	// 超长字段（攻击面）应被夹长度、双驱动一致返回 201（不 500）——守评审 load-bearing。
	long := strings.Repeat("x", 5000)
	if code := post(`{"kind":"` + long + `","vid":"` + long + `","email":"` + long + `","source":"` + long + `"}`); code != http.StatusCreated {
		t.Fatalf("超长字段应被夹后 201，得 %d", code)
	}
}
