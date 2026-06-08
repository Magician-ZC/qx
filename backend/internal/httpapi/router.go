package httpapi

// 文件说明：HTTP API 路由总装入口，连接会话服务、账户服务、地形接口与实时推送广播。

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"log/slog"

	"qunxiang/backend/internal/account"
	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/analytics"
	"qunxiang/backend/internal/billing"
	"qunxiang/backend/internal/combat"
	"qunxiang/backend/internal/compliance"
	"qunxiang/backend/internal/config"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/engine/status"
	"qunxiang/backend/internal/engine/turns"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/session"
	"qunxiang/backend/internal/socialobject"
	"qunxiang/backend/internal/storage/dbdialect"
	"qunxiang/backend/internal/unit"
	"qunxiang/backend/internal/world"
	"qunxiang/backend/internal/worldbus"
	"qunxiang/backend/internal/ws"
)

// advancePhasePlaySeconds 是一次成功推进阶段累计进防沉迷时长的粗略估算秒数（一个回合约 1 分钟）。
const advancePhasePlaySeconds int64 = 60

// Dependencies 聚合 Router 初始化所需的外部依赖。
type Dependencies struct {
	Config              config.Config
	Logger              *slog.Logger
	Hub                 *ws.Hub
	Store               *sql.DB
	AI                  *ai.Service
	ColdStore           *sql.DB
	Accounts            *account.Service
	RegionRunner        RegionRunnerStats // 可空：大世界 region-runner 遥测（/healthz 暴露）
	RegionRunnerEnabled bool              // region-runner 是否启用：决定建局/组队是否把单位 seed 进离线调度（M7.3-real-4b）
	ReflexShortCircuit  bool              // 反射真短路是否启用（降本，QUNXIANG_REFLEX_SHORTCIRCUIT）
}

// RegionRunnerStats 是 region-runner 暴露遥测的最小接口（避免 httpapi 依赖 regionrunner 包）。
type RegionRunnerStats interface {
	Stats() map[string]any
}

// billingEntitlementAdapter 把 *billing.Service 适配成 session.EntitlementChecker（叙事密度 perk 用）：
// 取账户全部权益、过滤 Status=active、返回 SKUID 列表（基元 []string，session 侧据 SKUID 命名约定判 perk）。
// 这样 session 包无需 import billing（避免循环依赖），与 SpendRecorder 注入同纪律。
type billingEntitlementAdapter struct{ svc *billing.Service }

func (a billingEntitlementAdapter) ActiveEntitlementSKUs(ctx context.Context, accountID string) ([]string, error) {
	if a.svc == nil {
		return nil, nil
	}
	ents, err := a.svc.ListEntitlements(ctx, accountID)
	if err != nil {
		return nil, err
	}
	skus := make([]string, 0, len(ents))
	for _, e := range ents {
		if strings.EqualFold(strings.TrimSpace(e.Status), "active") {
			skus = append(skus, e.SKUID)
		}
	}
	return skus, nil
}

// NewRouter 组装 HTTP 路由、会话服务与实时推送链路。
func NewRouter(deps Dependencies) *gin.Engine {
	router := gin.New()
	router.Use(gin.Recovery(), gin.Logger())
	router.Use(func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Session-Role-Token, X-Ops-Token")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	})
	_ = router.SetTrustedProxies(nil)

	hub := deps.Hub
	if hub == nil {
		hub = ws.NewHub(deps.Logger)
	}
	duelAuth := newDuelSessionAuthStoreWithDB(deps.Store)

	// envFlag 解析进程级开关：默认关 → 零行为变化；true/1/yes/on（不分大小写）视为开。
	// 商业化 / 合规端点经此 flag-gate（未开启即整组不注册）。
	envFlag := func(name string) bool {
		switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	}

	// 商业化 / 合规服务上提到顶部，供 newSessionService 注入 SpendRecorder（账户级 LLM 成本闭环）
	// 与 complianceGate 前置中间件复用。两者构造均轻量（仅持有 *sql.DB）；端点注册仍各自 flag-gate。
	// billingEnabled 开时才构造 billingSvc 并注入 SpendRecorder（关时 nil → 账户级记账/配额全链路 no-op）。
	// complianceSvc 无条件构造：compliance.Gate 内部已 flag 兜底（QUNXIANG_COMPLIANCE_ENABLED 关时恒放行），
	// 故 complianceGate 中间件可无条件注册，关 flag 时零行为变化（绝不误伤匿名/未实名玩家）。
	billingEnabled := envFlag("QUNXIANG_BILLING_ENABLED")
	var billingSvc *billing.Service
	if billingEnabled {
		billingSvc = billing.NewService(deps.Store)
		// 播种默认 SKU 目录（幂等，best-effort）——否则 ListSKUs 永远空、充值面板无商品。仅 flag 开时播种，零行为变化。
		_ = billingSvc.SeedDefaultSKUs(context.Background())
	}
	complianceSvc := compliance.NewService(deps.Store)

	debugSnapshotRequested := func(c *gin.Context) bool {
		if c == nil {
			return false
		}
		value := strings.ToLower(strings.TrimSpace(c.Query("debug")))
		if value == "1" || value == "true" || value == "yes" {
			return true
		}
		value = strings.ToLower(strings.TrimSpace(c.GetHeader("X-Qunxiang-Debug")))
		return value == "1" || value == "true" || value == "yes"
	}
	publicForRequest := func(c *gin.Context, snapshot session.Snapshot) session.Snapshot {
		return session.PublicSnapshot(snapshot, debugSnapshotRequested(c))
	}
	broadcastSessionSnapshot := func(reason string, snapshot session.Snapshot, extra map[string]any) {
		if strings.TrimSpace(snapshot.ID) == "" {
			return
		}
		publicSnapshot := session.PublicSnapshot(snapshot, false)
		payload := map[string]any{
			"reason":  strings.TrimSpace(reason),
			"session": publicSnapshot,
		}
		for key, value := range extra {
			payload[key] = value
		}
		hub.BroadcastSessionEvent(snapshot.ID, "session_snapshot", payload)

		interactions := publicSnapshot.LLMInteractions
		if len(interactions) > 4 {
			interactions = interactions[len(interactions)-4:]
		}
		for _, interaction := range interactions {
			hub.BroadcastSessionEvent(snapshot.ID, "llm_interaction", interaction)
		}

		if len(snapshot.Logs) > 0 {
			hub.BroadcastSessionEvent(snapshot.ID, "session_log", snapshot.Logs[len(snapshot.Logs)-1])
		}
	}
	broadcastSessionProgress := func(reason string, snapshot session.Snapshot, extra map[string]any) {
		if strings.TrimSpace(snapshot.ID) == "" {
			return
		}
		publicSnapshot := session.PublicSnapshot(snapshot, false)
		payload := map[string]any{
			"reason":  strings.TrimSpace(reason),
			"session": publicSnapshot,
		}
		for key, value := range extra {
			payload[key] = value
		}
		hub.BroadcastSessionEvent(snapshot.ID, "session_snapshot", payload)
		if len(snapshot.Logs) > 0 {
			hub.BroadcastSessionEvent(snapshot.ID, "session_log", snapshot.Logs[len(snapshot.Logs)-1])
		}
	}
	newSessionService := func() *session.Service {
		service := session.NewServiceWithColdStore(deps.Store, deps.AI, deps.ColdStore)
		service.SetAsyncExecution(true)
		service.SetProgressReporter(broadcastSessionProgress)
		service.SetBroadcaster(hub) // 命运收件箱/回响卡的 WS 实时推送
		// 开启归因强制：无源戏剧性自治选择优雅回退安全决策（设计宪法 §5）。
		// 遥测见 Service.AttributionStats()；若线上 OOC 率过高可改回 false。
		service.SetAttributionEnforcement(true)
		// region-runner 启用时，建局/组队把玩家单位 seed 进离线调度（默认关→零成本，见 ambient_scheduling.go）。
		service.SetAmbientSchedulingEnabled(deps.RegionRunnerEnabled)
		service.SetReflexShortCircuit(deps.ReflexShortCircuit) // 降本：日常安静 tick 反射短路跳过 LLM（默认关）
		// 账户级 LLM 成本闭环：billing 开启时注入 SpendRecorder（*billing.Service 结构满足 session.SpendRecorder）。
		// newSessionService 是每请求新建 Service 的闭包，故每次新建都注入（billingSvc 提到闭包可捕获作用域）。
		// billingSvc 为 nil（未开 flag）时不注入 → 账户级记账/配额前置拦截整体 no-op（nil 安全）。
		if billingSvc != nil {
			service.SetSpendRecorder(billingSvc)
			// 叙事密度 perk：注入账户权益查询器（适配 *billing.Service→session.EntitlementChecker，避免 session import billing）。
			service.SetEntitlementChecker(billingEntitlementAdapter{svc: billingSvc})
		}
		return service
	}
	resolveCommanderFaction := func(c *gin.Context, sessionID string, fallbackFactionID string) (string, bool) {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			return "", false
		}
		if !duelAuth.requiresToken(c.Request.Context(), sessionID) {
			return strings.TrimSpace(fallbackFactionID), true
		}

		roleToken := strings.TrimSpace(c.GetHeader("X-Session-Role-Token"))
		if roleToken == "" {
			roleToken = strings.TrimSpace(c.Query("role_token"))
		}
		role, ok := duelAuth.resolveRole(c.Request.Context(), sessionID, roleToken)
		if !ok {
			return "", false
		}
		switch role {
		case duelRoleEnemy:
			return "enemy", true
		default:
			return "player", true
		}
	}

	// complianceGate 是出海合规前置门（P0 硬门槛）。挂在建局 / 推进阶段端点前：
	//   1) softAccountID 软取账户——无 token / 解析失败 → 放行（匿名无法门控，绝不误伤原型默认开放）；
	//   2) 非空账户 → complianceSvc.Gate 裁决：!Allowed → 403 + reason，并 best-effort 埋点 compliance_blocked；
	//   3) Allowed && MinorMode → c.Set("minor_mode",true) 供下游分级（关闭恋爱生育 / 降暴力）。
	// flag 关（QUNXIANG_COMPLIANCE_ENABLED）/ Gate 出错一律放行——compliance.Gate 内部已 flag 兜底（关时恒 Allowed）。
	complianceGate := func() gin.HandlerFunc {
		return func(c *gin.Context) {
			accountID := softAccountID(deps.Accounts, c)
			if accountID == "" {
				c.Next()
				return
			}
			verdict, err := complianceSvc.Gate(c.Request.Context(), accountID)
			if err != nil {
				// 门控出错绝不误伤：放行（错误已被吞，不影响主流程）。
				c.Next()
				return
			}
			if !verdict.Allowed {
				// best-effort 埋点：合规拦截事件（失败绝不影响拦截本身）。
				_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
					Stage: analytics.StageRetention,
					Name:  analytics.EventComplianceBlocked,
					Props: map[string]any{"account_id": accountID, "reason": verdict.Reason},
				})
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error":  "compliance gate blocked",
					"reason": verdict.Reason,
				})
				return
			}
			if verdict.MinorMode {
				c.Set("minor_mode", true)
			}
			c.Next()
		}
	}

	router.GET("/healthz", func(c *gin.Context) {
		aiStatus := map[string]any{
			"ready": false,
		}
		if deps.AI != nil {
			aiStatus = deps.AI.Status()
		}
		attrTotal, attrOOC := session.AttributionStats()
		attrDegraded := session.AttributionDegraded()
		attrOOCRate := 0.0
		if attrTotal > 0 {
			attrOOCRate = float64(attrOOC) / float64(attrTotal)
		}

		reflexTotal, reflexCouldSkip, reflexShortCircuited := session.ReflexStats()
		reflexSkipRate := 0.0
		if reflexTotal > 0 {
			reflexSkipRate = float64(reflexCouldSkip) / float64(reflexTotal)
		}

		// 合规/红线遥测：内容安全双向审核计数 + 突然戏剧性动作的硬前因门控计数。
		safetyChecked, safetyBlocked := session.ContentSafetyStats()
		gateTotal, gateBlocked := session.SurpriseGateStats()

		status := gin.H{
			"status":                     "ok",
			"service":                    "qunxiang-backend",
			"client_count":               hub.ClientCount(),
			"room_count":                 hub.RoomCount(),
			"queue_count":                hub.MatchmakingQueueCount(),
			"session_subscription_count": hub.SessionSubscriptionCount(),
			"sqlite_path":                deps.Config.SQLitePath,
			"timestamp":                  time.Now().UTC().Format(time.RFC3339),
			"ai":                         aiStatus,
			"accounts":                   deps.Accounts != nil,
			"cold_storage":               deps.ColdStore != nil,
			"attribution": gin.H{
				"enforced": true,
				"degraded": attrDegraded,
				"active":   !attrDegraded,
				"total":    attrTotal,
				"ooc":      attrOOC,
				"ooc_rate": attrOOCRate,
			},
			// 反射层遥测：could_skip=本可被反射层短路省下的 LLM；short_circuited=真短路实际省下的（QUNXIANG_REFLEX_SHORTCIRCUIT 开启时）。
			"reflex_shadow": gin.H{
				"total":           reflexTotal,
				"could_skip":      reflexCouldSkip,
				"skip_rate":       reflexSkipRate,
				"short_circuited": reflexShortCircuited,
				"enabled":         deps.ReflexShortCircuit,
			},
			// 内容安全双向审核遥测（合规硬门槛，QUNXIANG_CONTENT_SAFETY 默认关→恒放行、checked 仍计）。
			"content_safety": gin.H{
				"checked": safetyChecked,
				"blocked": safetyBlocked,
			},
			// 突然戏剧性动作（恋爱/卖传家宝/叛变）的硬前因门控遥测：无源前因被回退安全决策的次数。
			"surprise_gate": gin.H{
				"total":   gateTotal,
				"blocked": gateBlocked,
			},
		}

		// 大世界 region-runner 遥测（M7.3-real-1）。注入了 runner 即暴露（含 enabled:false 表未启用）；未注入（如测试）则不出本块。
		if deps.RegionRunner != nil {
			status["region_runner"] = deps.RegionRunner.Stats()
		}

		if err := deps.Store.PingContext(c.Request.Context()); err != nil {
			status["status"] = "degraded"
			status["error"] = err.Error()
			c.JSON(http.StatusServiceUnavailable, status)
			return
		}

		c.JSON(http.StatusOK, status)
	})

	// 运营成本/单位经济仪表盘（只读聚合，产品验证）：跨会话真实 LLM 成本 + MAU 代理 + 单位经济。?days=N 限窗口（默认 30，0=全量）。
	router.GET("/api/ops/cost-dashboard", opsTokenGuard(), func(c *gin.Context) {
		days := 30
		if raw := strings.TrimSpace(c.Query("days")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				days = v
			}
		}
		data, err := newSessionService().CostDashboard(c.Request.Context(), days, time.Now())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, data)
	})

	// 产品漏斗只读聚合（P0 通电：让 product_events 富埋点从 write-only 变可消费）。days<=0/缺省=全量。
	router.GET("/api/ops/product-funnel", opsTokenGuard(), func(c *gin.Context) {
		days := 0
		if raw := strings.TrimSpace(c.Query("days")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				days = v
			}
		}
		report, err := analytics.FunnelCounts(c.Request.Context(), deps.Store, days)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, report)
	})

	// 北极星指标只读聚合（D2 收件箱处理率 / 分享 / 付费 / 回访）。days<=0/缺省=全量。
	router.GET("/api/ops/north-star", opsTokenGuard(), func(c *gin.Context) {
		days := 0
		if raw := strings.TrimSpace(c.Query("days")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				days = v
			}
		}
		report, err := analytics.NorthStar(c.Request.Context(), deps.Store, days)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, report)
	})

	// A/B 实验漏斗：按 ab_bucket 分组拆分对比（SH2.3 红线 A/B、卖点 A/B/C、服从 vs 违背）。key 仅回显，桶名本身编码实验。
	router.GET("/api/ops/experiment", opsTokenGuard(), func(c *gin.Context) {
		days := 0
		if raw := strings.TrimSpace(c.Query("days")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				days = v
			}
		}
		report, err := analytics.ExperimentFunnel(c.Request.Context(), deps.Store, strings.TrimSpace(c.Query("key")), days)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, report)
	})

	// 假门预实验留资端点（W0 验证）：POST /api/leads + GET /api/ops/leads-funnel。
	// 漏斗端点是 ops 敏感只读聚合，套 opsTokenGuard；POST /api/leads 是 landing 公开提交，保持公开（不守卫）。
	// 漏斗路由在 leads.go 内注册，这里用路径作用域的前置中间件守卫，避免影响公开的 /api/leads。
	leadsFunnelGuard := opsTokenGuard()
	router.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/api/ops/leads-funnel" {
			leadsFunnelGuard(c)
			return
		}
		c.Next()
	})
	registerLeadEndpoints(router, deps.Store)

	// 社会客体撮合（跨玩家，§2.2）：POST 用 MatchScore 四因子 + arbitration 确定性择人绑进社会客体；GET 列出某世界的社会客体。
	router.POST("/api/worlds/:worldId/social-objects", opsTokenGuard(), func(c *gin.Context) {
		worldID := strings.TrimSpace(c.Param("worldId"))
		var body struct {
			Kind       string                   `json:"kind"`
			Label      string                   `json:"label"`
			Slots      int                      `json:"slots"`
			Candidates []session.MatchCandidate `json:"candidates"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		objectID, chosen, err := newSessionService().MatchIntoSocialObject(c.Request.Context(), worldID, body.Kind, body.Label, body.Candidates, body.Slots)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"object_id": objectID, "chosen": chosen})
	})
	router.GET("/api/worlds/:worldId/social-objects", func(c *gin.Context) {
		objs, err := socialobject.ListByWorld(c.Request.Context(), deps.Store, strings.TrimSpace(c.Param("worldId")))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"social_objects": objs})
	})

	// 七种交互（跨玩家，§2.3）：POST 记录并按 consent 档路由（单方立即应用/高后果建待决同意请求）。
	router.POST("/api/worlds/:worldId/seven-interactions", opsTokenGuard(), func(c *gin.Context) {
		var body struct {
			ActorID     string `json:"actor_id"`
			TargetID    string `json:"target_id"`
			Interaction string `json:"interaction"`
			Importance  int    `json:"importance"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
			return
		}
		res, err := newSessionService().RecordSevenInteraction(c.Request.Context(), strings.TrimSpace(c.Param("worldId")),
			body.ActorID, body.TargetID, session.SevenInteraction(body.Interaction), body.Importance)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, res)
	})
	// consent_gate：列出某角色的待决同意请求 / 处理一条（接受=应用关系效果，拒绝=不应用）。
	router.GET("/api/consent/pending/:unitId", opsTokenGuard(), func(c *gin.Context) {
		reqs, err := newSessionService().ListPendingConsents(c.Request.Context(), strings.TrimSpace(c.Param("unitId")))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"pending": reqs})
	})
	router.POST("/api/consent/:reqId/resolve", opsTokenGuard(), func(c *gin.Context) {
		var body struct {
			Accept bool `json:"accept"`
		}
		_ = c.ShouldBindJSON(&body)
		req, err := newSessionService().ResolveConsentRequest(c.Request.Context(), strings.TrimSpace(c.Param("reqId")), body.Accept)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, req)
	})

	router.GET("/api/world/terrains", func(c *gin.Context) {
		if err := world.SeedTerrainCatalog(c.Request.Context(), deps.Store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		terrains, err := world.LoadTerrainCatalog(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"terrains": terrains})
	})

	router.GET("/api/world/map-scripts", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"map_scripts": session.BattlefieldScripts()})
	})
	router.GET("/api/world/map-sizes", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"map_sizes": session.BattlefieldSizes()})
	})

	router.GET("/api/events/reason-codes", func(c *gin.Context) {
		if err := events.SeedReasonCodeCatalog(c.Request.Context(), deps.Store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		definitions, err := events.LoadReasonCodeCatalog(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"reason_codes": definitions})
	})

	router.GET("/api/items/catalog", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"items": item.Catalog()})
	})

	parseUnitCount := func(c *gin.Context) (int, bool) {
		count := session.RecommendedOpeningUnitCount()
		if raw := strings.TrimSpace(c.Query("unit_count")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed <= 0 || parsed > session.MaxOpeningUnitCount() {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unit_count must be between 1 and %d", session.MaxOpeningUnitCount())})
				return 0, false
			}
			count = session.NormalizeOpeningUnitCount(parsed)
		}
		return count, true
	}
	parseMapSizeID := func(c *gin.Context) (string, bool) {
		sizeID := session.NormalizeBattlefieldSizeID(strings.TrimSpace(c.Query("map_size")))
		if raw := strings.TrimSpace(c.Query("map_size")); raw != "" && raw != sizeID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "map_size must be one of: small, medium, large"})
			return "", false
		}
		return sizeID, true
	}
	parseFogOfWarEnabled := func(c *gin.Context) (bool, bool) {
		raw := strings.TrimSpace(c.Query("fog_of_war"))
		if raw == "" {
			return false, true
		}
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "fog_of_war must be a boolean"})
			return false, false
		}
		return parsed, true
	}
	parseRandomEventsEnabled := func(c *gin.Context) (bool, bool) {
		raw := strings.TrimSpace(c.Query("random_events"))
		if raw == "" {
			return true, true
		}
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "random_events must be a boolean"})
			return false, false
		}
		return parsed, true
	}

	router.POST("/api/sessions/single-player", complianceGate(), func(c *gin.Context) {
		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}
		mapScriptID := strings.TrimSpace(c.Query("map_script_id"))
		if mapScriptID != "" && !session.IsBattlefieldScriptID(mapScriptID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "map_script_id is invalid"})
			return
		}
		unitCount, ok := parseUnitCount(c)
		if !ok {
			return
		}
		mapSizeID, ok := parseMapSizeID(c)
		if !ok {
			return
		}
		fogOfWarEnabled, ok := parseFogOfWarEnabled(c)
		if !ok {
			return
		}
		randomEventsEnabled, ok := parseRandomEventsEnabled(c)
		if !ok {
			return
		}

		// 软解析账户：有 Bearer token → 归账（成本闭环 / 配额拦截）；无 token / 失败 → 匿名空串（建局照常）。
		accountID := softAccountID(deps.Accounts, c)
		// minor_mode 由 complianceGate() 前置中间件按账户实名生日裁定置位（flag 关时恒 false）；落 State 持久化，advance 时从 state 取。
		snapshot, err := newSessionService().CreateSinglePlayerDraftWithMapScriptSizeUnitCountFogRandomEventsAndAccount(c.Request.Context(), seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, randomEventsEnabled, accountID, c.GetBool("minor_mode"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("session_created", snapshot, nil)

		// 漏斗埋点（best-effort，失败绝不影响建局）：会话创建 = 获客阶段事件。
		_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
			Stage:     analytics.StageAcquisition,
			Name:      analytics.EventSessionCreated,
			SessionID: snapshot.ID,
		})

		c.JSON(http.StatusCreated, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/duel", complianceGate(), func(c *gin.Context) {
		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}
		mapScriptID := strings.TrimSpace(c.Query("map_script_id"))
		if mapScriptID != "" && !session.IsBattlefieldScriptID(mapScriptID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "map_script_id is invalid"})
			return
		}
		unitCount, ok := parseUnitCount(c)
		if !ok {
			return
		}
		mapSizeID, ok := parseMapSizeID(c)
		if !ok {
			return
		}
		fogOfWarEnabled, ok := parseFogOfWarEnabled(c)
		if !ok {
			return
		}
		randomEventsEnabled, ok := parseRandomEventsEnabled(c)
		if !ok {
			return
		}
		creatorRole := normalizeDuelRole(c.Query("creator_role"))

		// 软解析房主账户：有 Bearer token → 归账（成本闭环 / 配额拦截）；无 token / 失败 → 匿名空串（建局照常）。
		accountID := softAccountID(deps.Accounts, c)
		service := newSessionService()
		snapshot, err := service.CreateDuelWithMapScriptSizeUnitCountFogRandomEventsAndAccount(c.Request.Context(), seed, mapScriptID, mapSizeID, unitCount, fogOfWarEnabled, randomEventsEnabled, accountID, c.GetBool("minor_mode"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		playerToken := uuid.NewString()
		enemyToken := uuid.NewString()
		room, roomErr := duelAuth.register(c.Request.Context(), snapshot.ID, playerToken, enemyToken, creatorRole)
		if roomErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": roomErr.Error()})
			return
		}
		broadcastSessionSnapshot("session_created_duel", snapshot, map[string]any{
			"room_status": room.status(),
		})
		commanderFactionID := snapshot.PlayerFactionID
		if creatorRole == duelRoleEnemy {
			commanderFactionID = snapshot.EnemyFactionID
		}

		c.JSON(http.StatusCreated, gin.H{
			"session":              publicForRequest(c, snapshot),
			"mode":                 "duel",
			"room_code":            room.RoomCode,
			"player_role_token":    playerToken,
			"enemy_role_token":     enemyToken,
			"commander_faction_id": commanderFactionID,
			"room_status":          room.status(),
		})
	})

	router.POST("/api/sessions/duel/join", func(c *gin.Context) {
		var request struct {
			RoomCode      string `json:"room_code"`
			PreferredRole string `json:"preferred_role"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid duel join payload"})
			return
		}

		sessionID, roleToken, role, room, ok := duelAuth.joinByRoomCode(c.Request.Context(), request.RoomCode, request.PreferredRole)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "room_code is invalid"})
			return
		}
		snapshot, err := newSessionService().GetSnapshot(c.Request.Context(), sessionID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		commanderFactionID := snapshot.PlayerFactionID
		if role == duelRoleEnemy {
			commanderFactionID = snapshot.EnemyFactionID
		}
		broadcastSessionSnapshot("duel_room_joined", snapshot, map[string]any{
			"room_status": room.status(),
			"joined_role": role,
		})
		c.JSON(http.StatusOK, gin.H{
			"session":              publicForRequest(c, snapshot),
			"mode":                 "duel",
			"room_code":            room.RoomCode,
			"role":                 role,
			"role_token":           roleToken,
			"commander_faction_id": commanderFactionID,
			"room_status":          room.status(),
		})
	})

	router.POST("/api/accounts/register", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		var request struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid register payload"})
			return
		}
		user, err := deps.Accounts.Register(c.Request.Context(), request.Username, request.DisplayName, request.Password)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		login, err := deps.Accounts.Login(c.Request.Context(), user.Username, request.Password)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// 漏斗埋点（best-effort，失败绝不影响注册）：账户注册 = 获客阶段事件。
		_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
			Stage: analytics.StageAcquisition,
			Name:  analytics.EventAccountRegistered,
			Props: map[string]any{"account_id": user.ID},
		})

		c.JSON(http.StatusCreated, gin.H{"user": user, "auth": login})
	})

	router.POST("/api/accounts/login", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		var request struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid login payload"})
			return
		}
		login, err := deps.Accounts.Login(c.Request.Context(), request.Username, request.Password)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"auth": login, "user": login.User})
	})

	router.GET("/api/accounts/me", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		token := account.ExtractBearerToken(c.GetHeader("Authorization"))
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		user, err := deps.Accounts.CurrentUser(c.Request.Context(), token)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"user": user})
	})

	router.POST("/api/accounts/logout", func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service is unavailable"})
			return
		}
		token := account.ExtractBearerToken(c.GetHeader("Authorization"))
		if token == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
			return
		}
		if err := deps.Accounts.Logout(c.Request.Context(), token); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	router.GET("/api/sessions/:id", func(c *gin.Context) {
		snapshot, err := newSessionService().GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		// 注意：此处不可埋 return_visit。GET /api/sessions/:id 被前端按 1s/5s 高频轮询
		// （App.tsx 阶段轮询/执行快照轮询/重连刷新等），一次轮询=一行事件会把留存漏斗灌爆、指标失真。
		// 真实回访信号由前端 App.tsx 经 localStorage 去重后 trackFunnel("return_visit") → POST /api/leads
		// （按匿名 vid 去重）承载，语义正确，无需在被轮询的快照 GET 上重复埋点。
		commanderFactionID, ok := resolveCommanderFaction(c, snapshot.ID, snapshot.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}
		previousRoomStatus, hadPreviousRoomStatus := duelAuth.roomStatusForSession(c.Request.Context(), snapshot.ID)
		roomStatus, hasRoomStatus := duelAuth.markJoinedBySessionRole(c.Request.Context(), snapshot.ID, commanderFactionID)
		if !hasRoomStatus {
			roomStatus, _ = duelAuth.roomStatusForSession(c.Request.Context(), snapshot.ID)
		} else if !hadPreviousRoomStatus || previousRoomStatus.PlayerJoined != roomStatus.PlayerJoined || previousRoomStatus.EnemyJoined != roomStatus.EnemyJoined {
			broadcastSessionSnapshot("duel_room_joined", snapshot, map[string]any{
				"room_status": roomStatus,
				"joined_role": commanderFactionID,
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"session":              publicForRequest(c, snapshot),
			"room_code":            duelAuth.roomCodeForSession(c.Request.Context(), snapshot.ID),
			"commander_faction_id": commanderFactionID,
			"room_status":          roomStatus,
		})
	})

	router.GET("/api/sessions/:id/reconnect", func(c *gin.Context) {
		reconnect, err := newSessionService().GetReconnectSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		includeDebug := debugSnapshotRequested(c)
		reconnect.Session = session.PublicSnapshot(reconnect.Session, includeDebug)
		reconnect.BoundarySession = session.PublicSnapshot(reconnect.BoundarySession, includeDebug)

		c.JSON(http.StatusOK, gin.H{"reconnect": reconnect})
	})

	router.POST("/api/sessions/:id/directive", func(c *gin.Context) {
		var request struct {
			Text   string `json:"text"`
			Scope  string `json:"scope"`
			UnitID string `json:"unit_id"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid directive payload"})
			return
		}

		service := newSessionService()
		current, err := service.GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		commanderFactionID, ok := resolveCommanderFaction(c, current.ID, current.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}
		scope := strings.ToLower(strings.TrimSpace(request.Scope))
		var (
			snapshot session.Snapshot
		)
		switch scope {
		case "", string(session.DirectiveKindDoctrine):
			snapshot, err = service.SetFactionGlobalDirective(c.Request.Context(), c.Param("id"), commanderFactionID, request.Text)
		case string(session.DirectiveKindTask):
			snapshot, err = service.SetFactionDirective(
				c.Request.Context(),
				c.Param("id"),
				commanderFactionID,
				session.DirectiveKindTask,
				request.UnitID,
				request.Text,
			)
		case string(session.DirectiveKindOrder):
			snapshot, err = service.SetFactionDirective(
				c.Request.Context(),
				c.Param("id"),
				commanderFactionID,
				session.DirectiveKindOrder,
				request.UnitID,
				request.Text,
			)
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "directive scope must be doctrine/task/order"})
			return
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("directive_updated", snapshot, map[string]any{
			"scope": scope,
		})

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/:id/opening-draft", func(c *gin.Context) {
		var request struct {
			Units []unit.Record `json:"units"`
		}
		_ = c.ShouldBindJSON(&request)
		service := newSessionService()
		snapshot, err := service.ApplyOpeningDraft(c.Request.Context(), c.Param("id"), request.Units)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("opening_draft_confirmed", snapshot, nil)
		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/:id/faction-relation", func(c *gin.Context) {
		var request struct {
			LeftFactionID  string `json:"left_faction_id"`
			RightFactionID string `json:"right_faction_id"`
			State          string `json:"state"`
			Reason         string `json:"reason"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid faction relation payload"})
			return
		}

		snapshot, err := newSessionService().SetFactionRelation(
			c.Request.Context(),
			c.Param("id"),
			request.LeftFactionID,
			request.RightFactionID,
			session.FactionRelationState(request.State),
			request.Reason,
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("faction_relation_updated", snapshot, map[string]any{
			"left_faction_id":  strings.TrimSpace(request.LeftFactionID),
			"right_faction_id": strings.TrimSpace(request.RightFactionID),
			"state":            strings.ToLower(strings.TrimSpace(request.State)),
		})

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/sessions/:id/dialogue", func(c *gin.Context) {
		var request struct {
			UnitID  string `json:"unit_id"`
			Message string `json:"message"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid dialogue payload"})
			return
		}
		service := newSessionService()
		current, err := service.GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		commanderFactionID, ok := resolveCommanderFaction(c, current.ID, current.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}
		if strings.TrimSpace(request.UnitID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unit_id is required"})
			return
		}
		unitFactionID := ""
		for _, record := range current.PlayerUnits {
			if record.ID == request.UnitID {
				unitFactionID = record.FactionID
				break
			}
		}
		if unitFactionID == "" {
			for _, record := range current.EnemyUnits {
				if record.ID == request.UnitID {
					unitFactionID = record.FactionID
					break
				}
			}
		}
		if unitFactionID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unit_id is invalid"})
			return
		}
		if unitFactionID != commanderFactionID {
			c.JSON(http.StatusForbidden, gin.H{"error": "unit does not belong to commander faction"})
			return
		}

		snapshot, reply, err := service.TalkToUnit(
			c.Request.Context(),
			c.Param("id"),
			request.UnitID,
			request.Message,
		)
		if err != nil {
			body := gin.H{"error": err.Error()}
			if snapshot.ID != "" {
				body["session"] = publicForRequest(c, snapshot)
			}
			c.JSON(http.StatusBadRequest, body)
			return
		}
		broadcastSessionSnapshot("dialogue", snapshot, map[string]any{
			"reply": reply,
		})

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot), "reply": reply})
	})

	// 举报端点是治理敏感写接口（可任意构造举报，越权风险），套 opsTokenGuard（P2 安全修复）。
	// 鉴权语义待改进：玩家举报本应改用账户/会话角色鉴权，当前依赖 opsTokenGuard 默认放行——
	// 原型未配 QUNXIANG_OPS_TOKEN 时该 guard 放行，故前端无 ops token 的普通玩家仍可举报；
	// 本波次刻意保持此鉴权模型不变（以免破坏既有测试），账户级举报鉴权留待后续波次。
	router.POST("/api/sessions/:id/reports", opsTokenGuard(), func(c *gin.Context) {
		var request struct {
			Reporter string `json:"reporter"`
			UnitID   string `json:"unit_id"`
			Category string `json:"category"`
			Detail   string `json:"detail"`
		}
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid report payload"})
			return
		}

		service := newSessionService()
		snapshot, report, err := service.SubmitModerationReport(
			c.Request.Context(),
			c.Param("id"),
			request.Reporter,
			request.UnitID,
			request.Category,
			request.Detail,
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// WS 广播给该局全部订阅客户端 → 必须脱敏（抹去 Reporter/Detail）防对线报复/敏感原文泄露；
		// 与 PublicSnapshot 的快照脱敏同口径。点对点 HTTP 响应（下行返给提交者本人）保留原始 report。
		broadcastSessionSnapshot("moderation_report", snapshot, map[string]any{
			"report": session.RedactModerationReportForPublic(report),
		})

		c.JSON(http.StatusCreated, gin.H{"session": publicForRequest(c, snapshot), "report": report})
	})

	// 举报裁定闭环（运营动作，治理敏感写接口）：标记 Resolved + 按 action 对被举报单位经 StatusMutator 施加后果。
	// action ∈ resolve|warn|ban（缺省 resolve）；note 可空。套 opsTokenGuard（与其它运营/审计端点同级鉴权）。
	router.POST("/api/sessions/:id/reports/:reportId/resolve", opsTokenGuard(), func(c *gin.Context) {
		var request struct {
			Action string `json:"action"`
			Note   string `json:"note"`
		}
		_ = c.ShouldBindJSON(&request)

		snapshot, report, err := newSessionService().ResolveModerationReport(
			c.Request.Context(),
			c.Param("id"),
			c.Param("reportId"),
			request.Action,
			request.Note,
		)
		if err != nil {
			// report 不存在 → 404；其余（action 非法 / reportID 空 / 应用后果失败）→ 400。
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "was not found") {
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		// 同 submit：WS 广播脱敏（ops 裁定后仍会把 report 推给全部普通玩家订阅者，含举报人/详情→须抹去）；
		// 点对点 HTTP 响应（返给 ops 本人）保留原始 report。
		broadcastSessionSnapshot("moderation_resolution", snapshot, map[string]any{
			"report": session.RedactModerationReportForPublic(report),
		})
		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot), "report": report})
	})

	// 审计包内含完整 LLM prompt（含玩家指令/角色记忆等敏感内容），是高危只读端点，套 opsTokenGuard（P2 安全修复）。
	router.GET("/api/sessions/:id/audit", opsTokenGuard(), func(c *gin.Context) {
		limit := 80
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be an integer"})
				return
			}
			limit = parsed
		}

		audit, err := newSessionService().GetAuditBundle(c.Request.Context(), c.Param("id"), limit)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"audit": audit})
	})

	// 不可逆数据擦除是高危写接口（可销毁任意会话的对话/记忆/审计），套 opsTokenGuard（P2 安全修复）。
	router.POST("/api/sessions/:id/privacy/erase", opsTokenGuard(), func(c *gin.Context) {
		var request struct {
			EraseDialogue   bool `json:"erase_dialogue"`
			EraseLLMDetails bool `json:"erase_llm_details"`
			EraseAuditTrail bool `json:"erase_audit_trail"`
			EraseMemories   bool `json:"erase_memories"`
			EraseReports    bool `json:"erase_reports"`
		}
		_ = c.ShouldBindJSON(&request)

		service := newSessionService()
		snapshot, result, err := service.EraseSessionPrivateData(
			c.Request.Context(),
			c.Param("id"),
			session.PrivacyEraseOptions{
				EraseDialogue:   request.EraseDialogue,
				EraseLLMDetails: request.EraseLLMDetails,
				EraseAuditTrail: request.EraseAuditTrail,
				EraseMemories:   request.EraseMemories,
				EraseReports:    request.EraseReports,
			},
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		broadcastSessionSnapshot("privacy_erased", snapshot, map[string]any{
			"privacy_erase_result": result,
		})
		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot), "result": result})
	})

	// 批量过期清理是高危写接口（可跨会话删除数据），套 opsTokenGuard（P2 安全修复）。
	router.POST("/api/privacy/purge", opsTokenGuard(), func(c *gin.Context) {
		var request struct {
			RetentionDays int `json:"retention_days"`
			Limit         int `json:"limit"`
		}
		_ = c.ShouldBindJSON(&request)

		result, err := newSessionService().PurgeExpiredSessionData(
			c.Request.Context(),
			request.RetentionDays,
			request.Limit,
		)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/sessions/:id/advance-phase", complianceGate(), func(c *gin.Context) {
		service := newSessionService()
		current, err := service.GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		commanderFactionID, ok := resolveCommanderFaction(c, current.ID, current.PlayerFactionID)
		if !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}

		snapshot, advanced, err := service.RequestAdvancePhase(c.Request.Context(), c.Param("id"), commanderFactionID)
		if err != nil {
			body := gin.H{"error": err.Error()}
			if snapshot.ID != "" {
				body["session"] = publicForRequest(c, snapshot)
			}
			c.JSON(http.StatusBadRequest, body)
			return
		}
		reason := "phase_ready"
		if advanced {
			reason = "phase_advanced"
		}
		broadcastSessionSnapshot(reason, snapshot, nil)

		// 防沉迷时长累计（best-effort，失败绝不影响推进）：仅在真正推进了阶段且玩家已登录时累计。
		// 估算本回合约 60 秒（一个部署/执行回合的粗略时长）；compliance flag 关时 RecordPlaySeconds 内部 no-op。
		if advanced {
			if accountID := softAccountID(deps.Accounts, c); accountID != "" {
				_ = complianceSvc.RecordPlaySeconds(c.Request.Context(), accountID, advancePhasePlaySeconds)
			}
		}

		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot)})
	})

	router.POST("/api/units/bootstrap", func(c *gin.Context) {
		name := c.DefaultQuery("name", "Prototype Unit")
		sessionID := c.DefaultQuery("session_id", "prototype-session")
		factionID := c.DefaultQuery("faction_id", "prototype-faction")
		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}

		repository := unit.NewRepository(deps.Store)
		record := unit.BootstrapRecord(seed, sessionID, factionID, name)
		if raw := c.Query("q"); raw != "" {
			q, err := strconv.Atoi(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "q must be an integer"})
				return
			}
			record.Status.PositionQ = q
		}
		if raw := c.Query("r"); raw != "" {
			r, err := strconv.Atoi(raw)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "r must be an integer"})
				return
			}
			record.Status.PositionR = r
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// 漏斗埋点（best-effort，失败绝不影响 bootstrap）：角色创建 = 激活阶段事件。
		_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
			Stage:     analytics.StageActivation,
			Name:      analytics.EventCharacterCreated,
			SessionID: sessionID,
			UnitID:    record.ID,
		})
		// 捏人/契约完成（charter）：bootstrap 成功落地角色即视为该单位契约完成，best-effort 埋点（激活阶段）。
		_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
			Stage:     analytics.StageActivation,
			Name:      analytics.EventCharterCompleted,
			SessionID: sessionID,
			UnitID:    record.ID,
		})

		response := gin.H{"unit": record}
		// 兑现命运 onboarding「她身边已有二十个有名有姓的人」承诺：with_village 时附带 20 人关系网。
		// best-effort：村庄是附加体验，SeedVillage 失败不影响 bootstrap（吞错只记日志）。
		// worldID 传空=不入世界；seed+1 派生避免与主单位撞种子。
		if withVillage := c.Query("with_village"); withVillage == "1" || withVillage == "true" {
			villagers := newSessionService().SeedVillageBestEffort(c.Request.Context(), sessionID, factionID, "", seed+1)
			response["villagers"] = villagers
		}

		c.JSON(http.StatusCreated, response)
	})

	// 客户端漏斗埋点入口（无鉴权，best-effort）：让纯前端转化点（状态卡查看 / 分享发起）也能落 product_events。
	// 防滥用注水：仅白名单事件名落库，其余一律返回 ok 但忽略（不报错、不写库）。
	// 契约（给前端）：POST /api/analytics/client  body {name:string, props?:object}；返回 {"ok":true}。
	router.POST("/api/analytics/client", func(c *gin.Context) {
		var request struct {
			Name  string         `json:"name"`
			Props map[string]any `json:"props"`
			Vid   string         `json:"vid"` // 匿名访客 ID，供 A/B 后端分桶（分桶算法权威集中在后端，前端零变体知识）。
		}
		// 解析失败也返 ok：埋点端点对客户端永远成功，绝不暴露内部细节。
		_ = c.ShouldBindJSON(&request)
		// A/B 分桶（QUNXIANG_AB_EXPERIMENT 配了实验名才生效；默认空→不分桶、ab_bucket 留空，零行为变化）。
		// 桶名形如 <experiment>:a/<experiment>:b，本身编码实验，供 /api/ops/experiment 按桶拆分漏斗对比。
		abBucket := ""
		if exp := strings.TrimSpace(os.Getenv("QUNXIANG_AB_EXPERIMENT")); exp != "" && strings.TrimSpace(request.Vid) != "" {
			abBucket = exp + ":" + analytics.AssignBucket(exp, request.Vid, []string{"a", "b"})
		}

		// 白名单：事件名 → 漏斗阶段。仅这些客户端事件允许注入，其余忽略。
		stage, allowed := map[string]analytics.Stage{
			analytics.EventStatusCardViewed: analytics.StageActivation, // 状态卡查看 = 激活
			analytics.EventShareInitiated:   analytics.StageReferral,   // 分享发起 = 转介
			// 命运高光卡三键轻反馈（GDD §8 乐趣度量：惊喜命中率/OOC 率）= 留存内核心乐趣信号。
			analytics.EventFateReactExpected: analytics.StageRetention,
			analytics.EventFateReactSurprise: analytics.StageRetention,
			analytics.EventFateReactOoc:      analytics.StageRetention,
		}[request.Name]
		if allowed {
			props := request.Props
			if props == nil {
				props = map[string]any{}
			}
			_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
				Stage:    stage,
				Name:     request.Name,
				Props:    props,
				ABBucket: abBucket,
			})
		}

		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	router.POST("/api/units/:id/mutations", func(c *gin.Context) {
		field := status.Field(c.Query("field"))
		if field == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "field is required"})
			return
		}
		delta, err := strconv.ParseFloat(c.DefaultQuery("delta", "0"), 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "delta must be numeric"})
			return
		}
		turn, err := strconv.Atoi(c.DefaultQuery("turn", "1"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "turn must be an integer"})
			return
		}

		mutator := status.NewMutator(deps.Store, unit.NewRepository(deps.Store))
		result, err := mutator.Apply(c.Request.Context(), status.Mutation{
			UnitID:       c.Param("id"),
			Turn:         turn,
			Field:        field,
			Delta:        delta,
			ReasonCode:   events.ReasonCode(c.Query("reason_code")),
			ReasonText:   c.Query("reason_text"),
			Location:     c.Query("location"),
			EmotionalTag: c.Query("emotional_tag"),
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/units/:id/rewards/grant", func(c *gin.Context) {
		gold, err := strconv.ParseFloat(c.DefaultQuery("gold", "0"), 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "gold must be numeric"})
			return
		}
		turn, err := strconv.Atoi(c.DefaultQuery("turn", "1"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "turn must be an integer"})
			return
		}

		mutator := status.NewMutator(deps.Store, unit.NewRepository(deps.Store))
		result, err := mutator.Apply(c.Request.Context(), status.Mutation{
			UnitID:     c.Param("id"),
			Turn:       turn,
			Field:      status.FieldWallet,
			Delta:      gold,
			ReasonCode: events.ReasonEconomyReward,
			ReasonText: c.Query("reason_text"),
			Location:   c.Query("location"),
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/combat/loot/resolve", func(c *gin.Context) {
		killerID := c.Query("killer_id")
		victimID := c.Query("victim_id")
		if killerID == "" || victimID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "killer_id and victim_id are required"})
			return
		}

		service := combat.NewLootInheritor(deps.Store, unit.NewRepository(deps.Store), deps.AI)
		result, err := service.Resolve(c.Request.Context(), combat.ResolveRequest{
			KillerUnitID: killerID,
			VictimUnitID: victimID,
			Location:     c.DefaultQuery("location", "hex_0_0"),
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	router.POST("/api/units/:id/life/down", func(c *gin.Context) {
		repository := unit.NewRepository(deps.Store)
		record, err := repository.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := unit.ApplyFatalDamage(&record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"unit": record})
	})

	router.POST("/api/units/:id/life/rescue", func(c *gin.Context) {
		repository := unit.NewRepository(deps.Store)
		record, err := repository.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := unit.Rescue(&record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"unit": record})
	})

	router.POST("/api/units/:id/life/self-revive", func(c *gin.Context) {
		repository := unit.NewRepository(deps.Store)
		record, err := repository.GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := unit.SelfRevive(&record); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := repository.Save(c.Request.Context(), record); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"unit": record})
	})

	router.POST("/api/world/bootstrap", func(c *gin.Context) {
		if err := world.SeedTerrainCatalog(c.Request.Context(), deps.Store); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		seed := time.Now().UTC().UnixNano()
		if raw := c.Query("seed"); raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "seed must be an integer"})
				return
			}
			seed = parsed
		}

		snapshot := world.GenerateMap(seed, world.DefaultMapWidth, world.DefaultMapHeight)
		if err := world.SaveMap(c.Request.Context(), deps.Store, snapshot); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		turnState := turns.NewState(time.Now().UTC(), turns.DefaultBudgets())

		c.JSON(http.StatusCreated, gin.H{
			"map": gin.H{
				"id":             snapshot.ID,
				"seed":           snapshot.Seed,
				"width":          snapshot.Width,
				"height":         snapshot.Height,
				"generated_at":   snapshot.GeneratedAt,
				"tile_count":     len(snapshot.Tiles),
				"terrain_counts": snapshot.Counts,
			},
			"turn": turnState,
		})
	})

	router.GET("/api/world/maps/latest", func(c *gin.Context) {
		summary, err := world.LoadLatestMapSummary(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"map": summary})
	})

	router.GET("/api/world/maps/latest/fov", func(c *gin.Context) {
		snapshot, err := world.LoadLatestMapSnapshot(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}

		q, err := strconv.Atoi(c.DefaultQuery("q", "0"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "q must be an integer"})
			return
		}
		r, err := strconv.Atoi(c.DefaultQuery("r", "0"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "r must be an integer"})
			return
		}
		baseRange, err := strconv.Atoi(c.DefaultQuery("base_range", "5"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "base_range must be an integer"})
			return
		}

		visible, err := world.ComputeVisibleTiles(snapshot, world.Coord{Q: q, R: r}, baseRange)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"origin":        gin.H{"q": q, "r": r},
			"base_range":    baseRange,
			"visible_count": len(visible),
			"visible_tiles": visible,
		})
	})

	// 命运收件箱：读未决待决策 / 处理一条待决策（祖魂语气的命运层，设计宪法 §4.6）。
	router.GET("/api/fate/inbox/:unitId", func(c *gin.Context) {
		items, err := newSessionService().OpenFateInbox(c.Request.Context(), c.Param("unitId"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"inbox": items})
	})
	// 命运四槽首屏：某角色最近的高光/待决策/回响卡。
	router.GET("/api/fate/feed/:unitId", func(c *gin.Context) {
		items, err := newSessionService().OpenFateFeed(c.Request.Context(), c.Param("unitId"), 30)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"feed": items})
	})
	// 世仇清单：列出某角色当前怀有的强敌意关系（blood_feud 传播的可观测面，前端/调试用）。纯读。
	// 关系四轴敌意图是敏感读面：必须按 :id 会话作用域 + 指挥阵营鉴权（与其它 /api/sessions/:id 读一致），
	// 且校验 :unitId 确属该会话，否则任意调用方可拿任意 unitId 跨会话拉取其完整敌意网络（含对象名）。
	router.GET("/api/sessions/:id/units/:unitId/feuds", func(c *gin.Context) {
		snapshot, err := newSessionService().GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if _, ok := resolveCommanderFaction(c, snapshot.ID, snapshot.PlayerFactionID); !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return
		}
		// 校验 unitId 归属本会话：拒绝跨会话/任意单位读。
		unitID := strings.TrimSpace(c.Param("unitId"))
		rec, err := unit.NewRepository(deps.Store).GetByID(c.Request.Context(), unitID)
		if err != nil || strings.TrimSpace(rec.SessionID) != snapshot.ID {
			c.JSON(http.StatusNotFound, gin.H{"error": "unit not found in session"})
			return
		}
		entries, err := newSessionService().ListBloodFeuds(c.Request.Context(), unitID, 32)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"feuds": entries})
	})
	// 角色状态卡：读单个单位（命运四槽的「状态卡」用）。
	router.GET("/api/units/:id", func(c *gin.Context) {
		rec, err := unit.NewRepository(deps.Store).GetByID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"unit": rec})
	})

	// ---- 离线宪章（offline_charter）读写：玩家不在场时单位据此自治的三段长效授权 ----
	// 会话作用域 + 鉴权 + 单位归属校验（同 feuds 端点范式）：拒绝跨会话/任意 unitId 越权读写他人宪章。
	// 三段：long_term_goals（长期目标，驱动目标重估）、redlines（红线，喂归因校验/Freeze List 硬门）、social_mandates（社交授权）。
	//
	// resolveCharterUnit 解析并鉴权：返回 (snapshot.ID, 已校验属本会话的 unitID, true)；失败时已写好响应。
	resolveCharterUnit := func(c *gin.Context) (string, string, bool) {
		snapshot, err := newSessionService().GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return "", "", false
		}
		if _, ok := resolveCommanderFaction(c, snapshot.ID, snapshot.PlayerFactionID); !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return "", "", false
		}
		unitID := strings.TrimSpace(c.Param("unitId"))
		rec, err := unit.NewRepository(deps.Store).GetByID(c.Request.Context(), unitID)
		if err != nil || strings.TrimSpace(rec.SessionID) != snapshot.ID {
			c.JSON(http.StatusNotFound, gin.H{"error": "unit not found in session"})
			return "", "", false
		}
		return snapshot.ID, unitID, true
	}
	// 读：返回该单位当前的离线宪章（未设立时 charter 为空三段，exists=false）。
	router.GET("/api/sessions/:id/units/:unitId/charter", func(c *gin.Context) {
		sessionID, unitID, ok := resolveCharterUnit(c)
		if !ok {
			return
		}
		charter, exists, err := newSessionService().GetUnitCharterForSession(c.Request.Context(), sessionID, unitID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"charter": charter, "exists": exists})
	})
	// 写：设立/覆盖该单位的离线宪章（写入即 NormalizeCharter，落库并写 CHARTER_ACTIVATED/CHARTER_UPDATED 留痕）。
	router.PUT("/api/sessions/:id/units/:unitId/charter", func(c *gin.Context) {
		sessionID, unitID, ok := resolveCharterUnit(c)
		if !ok {
			return
		}
		var body session.OfflineCharter
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid charter payload"})
			return
		}
		stored, err := newSessionService().SetUnitCharterForSession(c.Request.Context(), sessionID, unitID, body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"charter": stored})
	})
	// 撤销：删除该单位的离线宪章（撤销长效授权，写 CHARTER_UPDATED 留痕）。
	router.DELETE("/api/sessions/:id/units/:unitId/charter", func(c *gin.Context) {
		sessionID, unitID, ok := resolveCharterUnit(c)
		if !ok {
			return
		}
		if err := newSessionService().ClearUnitCharterForSession(c.Request.Context(), sessionID, unitID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.POST("/api/fate/decisions/:decisionId/resolve", func(c *gin.Context) {
		var request struct {
			SessionID   string `json:"session_id"`
			UnitID      string `json:"unit_id"`
			ResolveType string `json:"resolve_type"`
		}
		if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.UnitID) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unit_id is required"})
			return
		}
		resolveType := strings.TrimSpace(request.ResolveType)
		if resolveType == "" {
			resolveType = "acknowledge"
		}
		if err := newSessionService().ResolveFateDecision(c.Request.Context(), request.SessionID, request.UnitID, c.Param("decisionId"), resolveType); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 触发一次单人精英怪遭遇（撞见→combat_roll 多回合→分赃/惩罚→命运收件箱）。真实动作。
	router.POST("/api/sessions/:id/units/:unitId/elite-encounter", func(c *gin.Context) {
		result, err := newSessionService().TriggerEliteEncounter(c.Request.Context(), c.Param("id"), c.Param("unitId"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"encounter": result})
	})

	// 玩家直接接管/嘱咐某角色一次（落成可被回响 order_echo 引用的真实事件）。真实动作。
	router.POST("/api/sessions/:id/units/:unitId/intervene", func(c *gin.Context) {
		var body struct {
			Summary string `json:"summary"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// 接管落事件 + best-effort 触发「intervention」成因人格漂移（玩家介入潜移默化改变她；漂移失败不影响接管）。
		id, err := newSessionService().RecordPlayerInterventionWithDrift(c.Request.Context(), c.Param("id"), c.Param("unitId"), body.Summary)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"event_id": id})
	})

	// 触发一次组队野外Boss遭遇（多回合消耗战→按贡献分赃含 epic 仲裁/失败各自分级惩罚→各自命运收件箱）。真实动作。
	router.POST("/api/sessions/:id/field-boss", func(c *gin.Context) {
		var body struct {
			UnitIDs []string `json:"unit_ids"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := newSessionService().TriggerFieldBoss(c.Request.Context(), c.Param("id"), body.UnitIDs)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"encounter": result})
	})

	// 触发一次多层副本（逐层 combat_roll→累计分赃/撤退保利/败北分级惩罚→各自命运收件箱）。真实动作，QUNXIANG_DUNGEON 默认关。
	router.POST("/api/sessions/:id/dungeon", func(c *gin.Context) {
		var body struct {
			UnitIDs []string `json:"unit_ids"`
			Floors  int      `json:"floors"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := newSessionService().RunDungeonForSession(c.Request.Context(), c.Param("id"), body.UnitIDs, body.Floors)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"dungeon": result})
	})

	// ---- 多世界 / 跨玩家世界总线 ----
	// 注册一个持久世界。
	router.POST("/api/worlds", func(c *gin.Context) {
		var body struct {
			Name          string `json:"name"`
			MaxPopulation int    `json:"max_population"`
			RegionSeed    string `json:"region_seed"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := world.Create(c.Request.Context(), deps.Store, world.World{
			Name: body.Name, MaxPopulation: body.MaxPopulation, RegionSeed: body.RegionSeed,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"world_id": id})
	})

	// 列出活跃世界。
	router.GET("/api/worlds", func(c *gin.Context) {
		worlds, err := world.List(c.Request.Context(), deps.Store, world.StatusActive, 0)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"worlds": worlds})
	})

	// 把一个角色接入世界（幂等）。
	router.POST("/api/worlds/:worldId/join", func(c *gin.Context) {
		var body struct {
			CharacterID string `json:"character_id"`
			Role        string `json:"role"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := world.Join(c.Request.Context(), deps.Store, c.Param("worldId"), body.CharacterID, body.Role, dbdialect.For(deps.Store)); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 记录一次跨玩家交互：世界时钟发号 → 写入不可篡改的世界总线。真实动作。
	router.POST("/api/worlds/:worldId/interactions", func(c *gin.Context) {
		var body struct {
			ActorID    string `json:"actor_id"`
			TargetID   string `json:"target_id"`
			Kind       string `json:"kind"`
			Importance int    `json:"importance"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := newSessionService().RecordCrossInteraction(
			c.Request.Context(), c.Param("worldId"), body.ActorID, body.TargetID,
			worldbus.EventKind(body.Kind), body.Importance, nil)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"event_id": id})
	})

	// 主动把世界总线上牵涉某角色的跨玩家事件投进她的命运收件箱（读出侧投递的手动触发口）。真实动作。
	// 先 GetByID 取该角色（sessionID 取自其落库的 SessionID），再调 SurfaceCrossEventsForCharacter，返回被惊动条数。
	router.POST("/api/worlds/:worldId/units/:unitId/cross-events/surface", func(c *gin.Context) {
		limit := 0
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				limit = v
			}
		}
		record, err := unit.NewRepository(deps.Store).GetByID(c.Request.Context(), c.Param("unitId"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		surfaced, err := newSessionService().SurfaceCrossEventsForCharacter(
			c.Request.Context(), record.SessionID, c.Param("worldId"), &record, limit)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"surfaced": surfaced})
	})

	// 投放一头世界Boss（全世界共享血池的协作目标）。
	router.POST("/api/worlds/:worldId/bosses", func(c *gin.Context) {
		var body struct {
			Name     string `json:"name"`
			HP       int    `json:"hp"`
			RegionID string `json:"region_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := newSessionService().SpawnWorldBoss(c.Request.Context(), c.Param("worldId"), body.Name, body.HP, body.RegionID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"boss_id": id})
	})

	// 对世界Boss出手一次（异步协作：原子扣血→记总线→血池清零则全员分赃）。真实动作。
	router.POST("/api/worlds/:worldId/bosses/:bossId/strike", func(c *gin.Context) {
		var body struct {
			AttackerID string `json:"attacker_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		svc := newSessionService()
		attacker, err := unit.NewRepository(deps.Store).GetByID(c.Request.Context(), body.AttackerID)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := svc.StrikeWorldBoss(c.Request.Context(), c.Param("worldId"), c.Param("bossId"), &attacker)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"strike": result})
	})

	// 商业化端点（P2，flag QUNXIANG_BILLING_ENABLED；默认关→整组不注册，零行为变化）。
	// SKU 目录只读；purchase 走 billing.Service（收据校验默认 stubVerifier）；quota 查 LLM 配额闸。
	// billingSvc 已在 NewRouter 顶部按 billingEnabled 构造（供 newSessionService 注入 SpendRecorder），此处复用。
	if billingEnabled {
		// 列出可售 SKU（会员/单品）——只读目录。
		router.GET("/api/billing/skus", func(c *gin.Context) {
			skus, err := billingSvc.ListSKUs(c.Request.Context())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"skus": skus})
		})

		// 购买（收据校验经 ReceiptVerifier）。§5 高危纵深防御：仅当 billing.ProductionReady()（IAP_REAL 开 + 至少一平台凭据存在）
		// 才注册 purchase 端点——否则 stubVerifier 恒通过会让任意伪造收据领真实权益（刷单/回放）。未就绪时端点根本不存在（404），
		// 配合 billing.Service.Purchase 内的前置闸（返回 ErrPurchaseStubInProd）双保险，即便误开 BILLING_ENABLED 上线也无法刷单。
		if billing.ProductionReady() {
			router.POST("/api/billing/purchase", func(c *gin.Context) {
				var body struct {
					SKUID    string `json:"sku_id"`
					Platform string `json:"platform"`
					Receipt  string `json:"receipt"`
				}
				if err := c.ShouldBindJSON(&body); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "invalid purchase payload"})
					return
				}
				// 账户 ID 取自鉴权 token（忽略客户端传入），防为他人账户伪造扣费/发放权益。
				accountID, ok := authedAccountID(deps.Accounts, c)
				if !ok {
					return
				}
				charge, err := billingSvc.Purchase(c.Request.Context(), accountID, body.SKUID, body.Platform, body.Receipt)
				if err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
				// 漏斗埋点（best-effort，失败绝不影响购买）：营收阶段事件。
				_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
					Stage: analytics.StageRevenue,
					Name:  analytics.EventPurchase,
					Props: map[string]any{
						"account_id":   accountID,
						"sku_id":       body.SKUID,
						"amount_cents": charge.AmountCents,
					},
				})
				c.JSON(http.StatusCreated, gin.H{"charge": charge})
			})
		} else if deps.Logger != nil {
			deps.Logger.Warn("billing enabled but not production-ready (QUNXIANG_IAP_REAL off or no platform credential); /api/billing/purchase route NOT registered to prevent stub receipt fraud")
		}

		// 查询本账号的 LLM 配额是否仍允许调用（true=未超额）。账户 ID 取自鉴权 token，忽略路径参数。
		router.GET("/api/billing/quota/:accountId", func(c *gin.Context) {
			accountID, ok := authedAccountID(deps.Accounts, c)
			if !ok {
				return
			}
			allowed, err := billingSvc.CheckQuota(c.Request.Context(), accountID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"allowed": allowed})
		})

		// 列出本账号已购权益（会员/单品）。账户 ID 取自鉴权 token，忽略路径参数防越权读他人权益。
		router.GET("/api/billing/entitlements/:accountId", func(c *gin.Context) {
			accountID, ok := authedAccountID(deps.Accounts, c)
			if !ok {
				return
			}
			entitlements, err := billingSvc.ListEntitlements(c.Request.Context(), accountID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"entitlements": entitlements})
		})
	}

	// 合规端点（P2，flag QUNXIANG_COMPLIANCE_ENABLED；默认关→整组不注册，零行为变化）。
	// verify 登记实名/生日；gate 做前置裁决（未实名/未成年宵禁/防沉迷时长超限→Allowed=false）。
	// complianceSvc 已在 NewRouter 顶部无条件构造（供 complianceGate 前置中间件复用），此处复用。
	if envFlag("QUNXIANG_COMPLIANCE_ENABLED") {
		// 登记实名（真实姓名+身份证号交 RealnameVerifier 核验，绝不信任客户端 bool）与生日（据生日刷新未成年模式）。
		// PII：name/id_number 仅用于核验、不落库、不入日志（VerifyRealnameWithIdentity 只落结果位）。
		router.POST("/api/compliance/verify", func(c *gin.Context) {
			var body struct {
				BirthDate string `json:"birth_date"`
				Name      string `json:"name"`
				IDNumber  string `json:"id_number"`
			}
			if err := c.ShouldBindJSON(&body); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid verify payload"})
				return
			}
			// 账户 ID 一律取自鉴权 token（忽略客户端传入），防越权为他人伪造实名/生日绕过合规门。
			accountID, ok := authedAccountID(deps.Accounts, c)
			if !ok {
				return
			}
			if bd := strings.TrimSpace(body.BirthDate); bd != "" {
				if err := complianceSvc.SetBirthDate(c.Request.Context(), accountID, bd); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
			}
			// 仅当提交了姓名+身份证号时才走实名核验（生日单独登记也允许）；核验不过/网关错→不置位、4xx。
			if strings.TrimSpace(body.Name) != "" || strings.TrimSpace(body.IDNumber) != "" {
				if err := complianceSvc.VerifyRealnameWithIdentity(c.Request.Context(), accountID, body.Name, body.IDNumber); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
					return
				}
			}
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})

		// 合规前置门裁决（未实名/宵禁/防沉迷）。账户 ID 取自鉴权 token，忽略路径参数防越权读他人状态。
		router.GET("/api/compliance/gate/:accountId", func(c *gin.Context) {
			accountID, ok := authedAccountID(deps.Accounts, c)
			if !ok {
				return
			}
			verdict, err := complianceSvc.Gate(c.Request.Context(), accountID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{
				"allowed":    verdict.Allowed,
				"minor_mode": verdict.MinorMode,
				"reason":     verdict.Reason,
			})
		})

		// 累计本账号防沉迷在线时长（客户端按心跳/会话时长上报）。账户 ID 取自鉴权 token，忽略客户端伪造。
		router.POST("/api/compliance/play-seconds", func(c *gin.Context) {
			var body struct {
				Seconds int64 `json:"seconds"`
			}
			if err := c.ShouldBindJSON(&body); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid play-seconds payload"})
				return
			}
			accountID, ok := authedAccountID(deps.Accounts, c)
			if !ok {
				return
			}
			if body.Seconds < 0 {
				body.Seconds = 0
			}
			if err := complianceSvc.RecordPlaySeconds(c.Request.Context(), accountID, body.Seconds); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
	}

	// C-15: client only sends input, server remains the authoritative state owner.
	router.GET("/ws", hub.Handle)

	return router
}
