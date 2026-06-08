package httpapi

// 文件说明：假门预实验（W0 验证）的留资后端。landing 落地页把留资/问卷/事件 POST 到 /api/leads，append-only 落库；
// /api/ops/leads-funnel 给运营看转化漏斗（按 kind 计数 + 唯一访客）。先验证需求/单位成本，再大投入开发（PRD §11.6）。

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// registerLeadEndpoints 在 NewRouter 里注册假门留资与漏斗端点。store 为主库。
func registerLeadEndpoints(router *gin.Engine, store *sql.DB) {
	// POST /api/leads：接收 landing 的 {kind, vid, email?, utm_source?/source?, ...} JSON，整体存 payload_json。
	router.POST("/api/leads", func(c *gin.Context) {
		var body map[string]any
		if err := c.ShouldBindJSON(&body); err != nil || body == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		raw, err := json.Marshal(body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot encode body"})
			return
		}
		// 夹长度对齐 MySQL VARCHAR 宽度（避免攻击者超长串在 MySQL strict 模式 500、SQLite 201 的双驱动发散；payload_json 是 LONGTEXT/TEXT 不夹）。
		kind := clampStr(firstNonEmpty(asString(body["kind"]), "lead"), 32)
		vid := clampStr(asString(body["vid"]), 191)
		email := clampStr(asString(body["email"]), 255)
		source := clampStr(firstNonEmpty(asString(body["source"]), asString(body["utm_source"])), 191)
		if _, err := store.ExecContext(c.Request.Context(),
			`INSERT INTO fake_door_leads (id, kind, vid, email, source, payload_json, created_at) VALUES (?,?,?,?,?,?,?)`,
			uuid.NewString(), kind, vid, email, source, string(raw),
			time.Now().UTC().Format("2006-01-02 15:04:05"),
		); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"ok": true})
	})

	// GET /api/ops/leads-funnel：假门转化漏斗（按 kind 计数 + 唯一访客数）。
	router.GET("/api/ops/leads-funnel", func(c *gin.Context) {
		byKind := map[string]int{}
		rows, err := store.QueryContext(c.Request.Context(), `SELECT kind, COUNT(*) FROM fake_door_leads GROUP BY kind`)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		total := 0
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				_ = rows.Close()
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			byKind[k] = n
			total += n
		}
		_ = rows.Close()

		uniqueVisitors := 0
		_ = store.QueryRowContext(c.Request.Context(), `SELECT COUNT(DISTINCT vid) FROM fake_door_leads WHERE vid IS NOT NULL AND vid <> ''`).Scan(&uniqueVisitors)

		c.JSON(http.StatusOK, gin.H{
			"total":           total,
			"by_kind":         byKind,
			"unique_visitors": uniqueVisitors,
		})
	})
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// clampStr 按 rune 截断到 max 长度（避免超长串溢出 MySQL VARCHAR；按 rune 防多字节字符截半）。
func clampStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
