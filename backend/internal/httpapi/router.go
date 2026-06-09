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
	"sync"
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
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/item"
	"qunxiang/backend/internal/liveops"
	"qunxiang/backend/internal/runtimeconfig"
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

// healthzPlayerOOCTTL 是 /healthz 玩家口径 OOC（FateOOCDualChannel 的 product_events 覆盖扫描）结果的进程级缓存有效期。
// 取 30s：健康检查常被秒级高频轮询，对 product_events 的 O(N) 覆盖索引扫描若每次都跑会放大 DB 负载；
// 缓存窗口内多次 /healthz 复用上次扫描结果即可。纯观测读、不进任何结算/latch，30s 陈旧度对运营观测无影响。
const healthzPlayerOOCTTL = 30 * time.Second

// healthzPlayerOOCCache 是「玩家主观三键 OOC」DB 扫描结果的进程级 TTL 缓存。
// 只缓存昂贵的玩家口径（PlayerOOCRate/PlayerSamples，来自 product_events 扫描）；
// 机器口径（AttributionStats 等内存读）由调用方每次现读不走此缓存，故 attribution/机器 OOC 永远是最新值。
type healthzPlayerOOCCache struct {
	mu        sync.Mutex
	fetchedAt time.Time
	rate      float64
	samples   int
}

// playerOOC 返回玩家主观 OOC 率与样本数：缓存命中（距上次扫描 < ttl）直接复用，否则用 fetch 重扫并刷新缓存。
// fetch 失败/为 0 也照常写缓存（短路后续重试，避免热路径上对失败查询反复打 DB）；缓存为空时 fetchedAt 零值必然过期，首访必扫。
func (c *healthzPlayerOOCCache) playerOOC(now time.Time, ttl time.Duration, fetch func() (float64, int)) (float64, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < ttl {
		return c.rate, c.samples
	}
	rate, samples := fetch()
	c.rate = rate
	c.samples = samples
	c.fetchedAt = now
	return rate, samples
}

// healthzOOCCache 是 /healthz 玩家口径 OOC 的进程级缓存单例（每进程一份，跨 /healthz 请求共享）。
var healthzOOCCache = &healthzPlayerOOCCache{}

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

	// ops RBAC：多操作者 + 三档角色（viewer/operator/admin）+ 操作审计，建立在 ops_operators/ops_audit_log 两表上。
	// 三档守卫闭包替换旧的单 token opsTokenGuard()/opsTokenGuardStrict()：只读挂 opsViewer、写挂 opsWriter、
	// 高危破坏性（不可逆擦除/批量清理/操作者管理）挂 opsAdmin。ops_operators 表为空时优雅降级回旧单 token env 语义
	// （向后兼容：表空 + 配了 QUNXIANG_OPS_TOKEN → 该 token 视为 admin；表空 + 未配 → viewer 放行、operator+ fail-closed 503）。
	opsStore := NewOpsOperatorStore(deps.Store)
	opsViewer := opsRBACGuard(opsStore, RoleViewer)
	opsWriter := opsRBACGuard(opsStore, RoleOperator)
	opsAdmin := opsRBACGuard(opsStore, RoleAdmin)
	// 引导期种子：表空且配了 QUNXIANG_OPS_TOKEN 时，把该 env token 幂等写为 admin 操作者（name=env-admin）。
	// 否则运营用 env-admin 建首个 operator 后 Count>0、表权威切换会把 env-admin 永久踢出造成自锁。best-effort 吞错不阻断启动。
	if n, err := opsStore.Count(context.Background()); err == nil && n == 0 {
		if envTok := opsEnvToken(); envTok != "" {
			_ = opsStore.Upsert(context.Background(), "env-admin", string(RoleAdmin), envTok, "bootstrap")
		}
	}

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

	// Live-Ops 平台层（GM 世界事件注入 / 赛季骨架 / 零和审计）。无条件构造（仅持 *sql.DB）；端点套 opsTokenGuard。
	// 三个依赖经 httpapi 适配器注入：
	//   - HallArchiver：复用 session 既有名人堂归档（赛季收尾回流存活角色），每次新建轻量 Service。
	//   - PaidResolver：保守默认恒 false（审计退化为单组，绝不误报 P2W；unit→account 映射是 documented residual）。
	//   - Broadcaster：经 ws.Hub best-effort 广播世界级轻通知（hub 为 nil 时仅 log）。
	liveopsSvc := liveops.NewService(deps.Store).
		WithArchiver(liveopsHallArchiver{newServiceFn: newSessionService}).
		WithPaidResolver(defaultPaidResolver()).
		WithBroadcaster(liveopsBroadcaster{hub: hub})
	// 母题库幂等播种（修「season_content_themes 死表零播种」缺口）：通电默认母题骨架，GM 后台可续填/改。
	// best-effort：失败只记日志、绝不阻断启动（母题缺省不影响赛季创建——content_theme_id 仅是可选指针）。
	if err := liveopsSvc.SeedDefaultContentThemes(context.Background()); err != nil {
		deps.Logger.Error("seed default content themes", "error", err)
	}
	// 开发/演示：QUNXIANG_SEED_DEMO 开时幂等播种 demo 账号 + 主世界角色（自动 20 人村庄），
	// 登录 demo/demo1234 即可在全屏世界地图上看到主角 + 20 个村民。best-effort 不阻断启动。
	if envFlag("QUNXIANG_SEED_DEMO") {
		seedDemoCharacter(context.Background(), deps.Accounts, newSessionService, deps.Logger)
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

		// 合规/红线遥测：内容安全双向审核计数 + 突然戏剧性动作的硬前因门控计数 + 一致性收紧惊喜上限计数。
		safetyChecked, safetyBlocked := session.ContentSafetyStats()
		injChecked, injBlocked := session.InjectionSafetyStats()
		gateTotal, gateBlocked := session.SurpriseGateStats()
		surpriseCapTotal, surpriseCapDeferred := session.SurpriseCapStats()
		// §8 OOC 双口径交叉观测（玩家主观三键 OOC + 归因机器 OOC + 两旋钮态）。纯读、幂等（不驱动 latch）。
		// 机器口径 + 两旋钮态恒现读（内存读，零成本、永远最新，与 attribution 块同源）；
		// 玩家口径（PlayerOOCRate/PlayerSamples）走进程级 TTL 缓存——它是对 product_events 的 O(N) 覆盖扫描，
		// 高频健康轮询若每次都打 DB 会放大负载，故缓存 healthzPlayerOOCTTL，窗口内复用上次扫描结果。
		// 关键：把昂贵的 FateOOCDualChannel（含 NorthStar 扫描）只放进缓存 miss 分支，命中时根本不建 Service、不打 DB。
		oocDual := session.OOCDualChannel{
			MachineTotal:         attrTotal,
			MachineOOC:           attrOOC,
			MachineOOCRate:       attrOOCRate,
			ConsistencyTightened: session.ConsistencyTightened(),
			AttributionDegraded:  attrDegraded,
		}
		oocDual.PlayerOOCRate, oocDual.PlayerSamples = healthzOOCCache.playerOOC(
			time.Now(), healthzPlayerOOCTTL,
			func() (float64, int) {
				fresh := newSessionService().FateOOCDualChannel(c.Request.Context())
				return fresh.PlayerOOCRate, fresh.PlayerSamples
			},
		)

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
			// 输入侧越狱/prompt-injection 检测遥测（发行安全门，QUNXIANG_INPUT_INJECTION 默认开、独立于 content_safety）。
			"injection_guard": gin.H{
				"checked": injChecked,
				"blocked": injBlocked,
			},
			// 突然戏剧性动作（恋爱/卖传家宝/叛变）的硬前因门控遥测：无源前因被回退安全决策的次数。
			"surprise_gate": gin.H{
				"total":   gateTotal,
				"blocked": gateBlocked,
			},
			// §8 一致性收紧旋钮遥测：收紧态下命中惊喜上限（surprise_level ≥cap）总数 + 真被回退（deferred）数 + 当前上限。
			"surprise_cap": gin.H{
				"total":    surpriseCapTotal,
				"deferred": surpriseCapDeferred,
				"cap":      session.TightenedSurpriseCap(),
			},
			// §8 OOC 双口径交叉观测：玩家主观三键 OOC（窗口） + 归因机器 OOC（全量累计） + 两旋钮态（收紧/降级）。
			"ooc_dual_channel": gin.H{
				"player_ooc_rate":       oocDual.PlayerOOCRate,
				"player_samples":        oocDual.PlayerSamples,
				"machine_ooc_rate":      oocDual.MachineOOCRate,
				"machine_total":         oocDual.MachineTotal,
				"machine_ooc":           oocDual.MachineOOC,
				"consistency_tightened": oocDual.ConsistencyTightened,
				"attribution_degraded":  oocDual.AttributionDegraded,
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
	router.GET("/api/ops/cost-dashboard", opsViewer, func(c *gin.Context) {
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
	router.GET("/api/ops/product-funnel", opsViewer, func(c *gin.Context) {
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
	router.GET("/api/ops/north-star", opsViewer, func(c *gin.Context) {
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
	router.GET("/api/ops/experiment", opsViewer, func(c *gin.Context) {
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

	// ---- Live-Ops 运营手柄（GM 世界事件注入 / 赛季骨架 / 零和审计），全部 opsTokenGuard ----
	// 设计 docs/产品方案PRD.md §8。写均走 append-only 表、永不改写历史；审计只观测付费态、绝不进 Score。

	// GM 世界事件注入：往某活世界投一条权威跨事件（天灾/外敌/丰年…），全量留可仲裁审计。
	router.POST("/api/ops/worlds/:worldId/events", opsWriter, func(c *gin.Context) {
		var body struct {
			Kind       string         `json:"kind"`
			Importance int            `json:"importance"`
			ActorID    string         `json:"actor_id"`
			TargetID   string         `json:"target_id"`
			RegionID   string         `json:"region_id"`
			Payload    map[string]any `json:"payload"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// CreatedBy 用 ops 令牌指纹身份；当前 ops 鉴权不透出操作者身份，统一记 "ops"（审计可见「这是运营注入」）。
		createdBy := strings.TrimSpace(c.GetHeader(opsTokenHeader))
		if createdBy != "" {
			createdBy = "ops-token"
		} else {
			createdBy = "ops"
		}
		result, err := liveopsSvc.EmitWorldEvent(c.Request.Context(), liveops.GMEvent{
			WorldID:    c.Param("worldId"),
			Kind:       body.Kind,
			ActorID:    body.ActorID,
			TargetID:   body.TargetID,
			RegionID:   body.RegionID,
			Importance: body.Importance,
			CreatedBy:  createdBy,
			Payload:    body.Payload,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	// 创建赛季：建一个新世界 + 落 seasons 行（可选挂内容母题）。
	router.POST("/api/ops/seasons", opsWriter, func(c *gin.Context) {
		var body struct {
			Name           string `json:"name"`
			WorldName      string `json:"world_name"`
			ContentThemeID string `json:"content_theme_id"`
			MaxPopulation  int    `json:"max_population"`
			RegionSeed     string `json:"region_seed"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		season, err := liveopsSvc.CreateSeason(c.Request.Context(), liveops.CreateSeasonInput{
			Name:           body.Name,
			WorldName:      body.WorldName,
			ContentThemeID: body.ContentThemeID,
			MaxPopulation:  body.MaxPopulation,
			RegionSeed:     body.RegionSeed,
		})
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"season": season})
	})

	// 收尾赛季：存活角色回流名人堂 + 世界封存 + status=finalized + 广播。
	router.POST("/api/ops/seasons/:id/finalize", opsWriter, func(c *gin.Context) {
		result, err := liveopsSvc.FinalizeSeason(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	// 列赛季（SeasonPanel 列季数据源；修原 GET 列表路由缺失致前端只能展示本会话新建）。
	router.GET("/api/ops/seasons", opsViewer, func(c *gin.Context) {
		seasons, err := liveopsSvc.ListSeasons(c.Request.Context(), 0)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"seasons": seasons})
	})

	// 零和监控审计：扫某世界 [turn_start, turn_end] 区间的仲裁结局，按付费态分组算胜率、判 P2W 红线。
	router.GET("/api/ops/worlds/:worldId/arbitration-audit", opsViewer, func(c *gin.Context) {
		turnStart := 0
		turnEnd := 0
		if raw := strings.TrimSpace(c.Query("turn_start")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				turnStart = v
			}
		}
		if raw := strings.TrimSpace(c.Query("turn_end")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				turnEnd = v
			}
		}
		report, err := liveopsSvc.AuditArbitration(c.Request.Context(), c.Param("worldId"), turnStart, turnEnd)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"report": report})
	})

	// GM 注入审计清单：列出某世界的 GM 注入审计（最新在前），?limit= 限条数（缺省 100）。
	router.GET("/api/ops/worlds/:worldId/gm-audit", opsViewer, func(c *gin.Context) {
		limit := 0
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				limit = v
			}
		}
		entries, err := liveopsSvc.ListAudit(c.Request.Context(), c.Param("worldId"), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"entries": entries})
	})

	// ---- GM 管理后台手柄（运行时 flag 开关层 + 世界配置）----
	// 只读端点（GET flags/worlds-detail）套宽松 opsTokenGuard（未配 token 默认开放，原型语义）；
	// 写端点（POST/DELETE flags、POST threat、POST seed-village）套 fail-closed 的 opsTokenGuardStrict
	//（未配 token 返 503 拒绝）——这些是状态变更 + 反射关键 flag 开关，绝不能 fail-open。
	// flag override 经 featureflags 进程级状态（已注入双驱动 Store 持久化，main.go 启动回灌）；
	// 世界配置经 session 的 admin_world 服务（每请求新建轻量 Service）。供独立 AdminApp 前端 5 面板驱动。

	// 列出全部已知游戏 flag 的生效态（override / 环境变量 / 默认值三层合一）。
	router.GET("/api/admin/flags", opsViewer, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"flags": featureflags.SnapshotEffective()})
	})

	// 设一个 flag 的运行时 override（不重启即灰度）。先校验是已知游戏 flag，再 best-effort 落库持久化。
	// 写端点：反射关键 flag 开关，fail-closed（opsTokenGuardStrict）——未配 OPS_TOKEN 时返 503 拒绝。
	router.POST("/api/admin/flags", opsWriter, func(c *gin.Context) {
		var body struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if !featureflags.IsKnownGameplayFlag(body.Name) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown flag"})
			return
		}
		if err := featureflags.SetOverride(body.Name, body.Value); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "flag_set", body.Name+"="+body.Value)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 清一个 flag 的运行时 override（回退环境变量/默认值）。existed 透出该 flag 此前是否有 override。
	// AdminApp 走 DELETE /api/admin/flags?name=（query）；同时注册 /:name（path）便于直接 curl/兼容 spec。
	adminClearFlag := func(c *gin.Context) {
		name := strings.TrimSpace(c.Param("name"))
		if name == "" {
			name = strings.TrimSpace(c.Query("name"))
		}
		existed, err := featureflags.ClearOverride(name)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "flag_clear", name)
		c.JSON(http.StatusOK, gin.H{"existed": existed})
	}
	// 清 override 同属写端点，operator+ fail-closed（opsWriter）。
	router.DELETE("/api/admin/flags", opsWriter, adminClearFlag)       // AdminApp：?name=
	router.DELETE("/api/admin/flags/:name", opsWriter, adminClearFlag) // 兼容 path 形

	// 世界总览：列出全部世界及其 region/人口概览（GM 总览页数据源）。limit=0 取缺省。
	// 路径用 /worlds-detail 对齐 AdminApp WorldConfigPanel 的 listWorldsDetail（与已落地的基础列表 /api/worlds 区分）。
	adminWorldsDetail := func(c *gin.Context) {
		worlds, err := newSessionService().ListWorldsDetail(c.Request.Context(), 0)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"worlds": worlds})
	}
	router.GET("/api/admin/worlds-detail", opsViewer, adminWorldsDetail)
	router.GET("/api/admin/worlds", opsViewer, adminWorldsDetail) // 兼容别名

	// 三阵营 GM 只读概览（标识/中文名/道德信条/道德基准/出生据点/当前人口）。阵营开放世界 F3。
	// 与 worlds-detail 同口径只读守卫（opsTokenGuard：未配 token 放行，配了需正确 X-Ops-Token）。
	router.GET("/api/admin/factions", opsViewer, func(c *gin.Context) {
		details, err := newSessionService().ListFactionsDetail(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"factions": details})
	})

	// 绝对置位某 region 的威胁等级（GM 人工拉高/清零某地威胁度做活动/演练）。返回置位后的实际威胁值。
	// 写端点：状态变更，fail-closed（opsTokenGuardStrict）。
	router.POST("/api/admin/worlds/:worldId/regions/:regionId/threat", opsWriter, func(c *gin.Context) {
		var body struct {
			Level int64 `json:"level"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		lvl, err := newSessionService().SetRegionThreatLevel(c.Request.Context(), c.Param("worldId"), c.Param("regionId"), body.Level)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"threat_level": lvl})
	})

	// 手动为某局/世界确定性织一张 20 人出生关系网（GM 手动播种世界人口）。faction_id 缺省服务内取 session_id。
	// 写端点：凭空造人是状态变更，fail-closed（opsTokenGuardStrict）。
	router.POST("/api/admin/worlds/:worldId/seed-village", opsWriter, func(c *gin.Context) {
		var body struct {
			SessionID string `json:"session_id"`
			FactionID string `json:"faction_id"`
			Seed      int64  `json:"seed"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		n, err := newSessionService().SeedWorldVillage(c.Request.Context(), body.SessionID, body.FactionID, c.Param("worldId"), body.Seed)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"seeded": n})
	})

	// ===== 类型化运行时配置中心（internal/runtimeconfig：不重启即实时调玩法数值 / LLM 热切） =====
	// 只读快照挂 opsViewer；设/清 override 挂 opsWriter（fail-closed）。值经 runtimeconfig 类型 + 范围/枚举校验，非法 400。
	// 反 P2W：llm.* 是全局单值热切，不接 tier-routing 付费分档（付费买不到更强模型）。
	router.GET("/api/admin/config", opsViewer, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"params": runtimeconfig.SnapshotEffective()})
	})
	router.POST("/api/admin/config", opsWriter, func(c *gin.Context) {
		var body struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := runtimeconfig.SetOverride(body.Name, body.Value); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "config_set", body.Name+"="+body.Value)
		if p, ok := runtimeconfig.EffectiveByName(body.Name); ok {
			c.JSON(http.StatusOK, gin.H{"param": p})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	// 清 override 回落 catalog 默认值（AdminApp 走 ?name= query；同注册 /:name path 便于 curl）。
	adminClearConfig := func(c *gin.Context) {
		name := strings.TrimSpace(c.Param("name"))
		if name == "" {
			name = strings.TrimSpace(c.Query("name"))
		}
		existed, err := runtimeconfig.ClearOverride(name)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "config_clear", name)
		if p, ok := runtimeconfig.EffectiveByName(name); ok {
			c.JSON(http.StatusOK, gin.H{"param": p, "existed": existed})
			return
		}
		c.JSON(http.StatusOK, gin.H{"existed": existed})
	}
	router.DELETE("/api/admin/config", opsWriter, adminClearConfig)
	router.DELETE("/api/admin/config/:name", opsWriter, adminClearConfig)

	// ===== ops 操作者管理 + 操作审计（RBAC） =====
	// 当前登录态身份（viewer+）：返回 X-Ops-Token 对应 operator 的 name+role（表空降级返回 env-admin / anonymous）。
	router.GET("/api/admin/whoami", opsViewer, func(c *gin.Context) {
		if op, ok := currentOperator(c); ok {
			c.JSON(http.StatusOK, gin.H{"name": op.Name, "role": string(op.Role)})
			return
		}
		c.JSON(http.StatusOK, gin.H{"name": "anonymous", "role": string(RoleViewer)})
	})
	// 列操作者（脱敏，不含 token_hash）。viewer+。键名归一小写对齐前端。
	router.GET("/api/admin/operators", opsViewer, func(c *gin.Context) {
		rows, err := opsStore.List(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out := make([]gin.H, 0, len(rows))
		for _, o := range rows {
			out = append(out, gin.H{"name": o.Name, "role": o.Role, "created_at": o.CreatedAt})
		}
		c.JSON(http.StatusOK, gin.H{"operators": out})
	})
	// 新增/改操作者（admin+，fail-closed）。明文 token 仅此一次随请求传入、落库只存 sha256。
	router.POST("/api/admin/operators", opsAdmin, func(c *gin.Context) {
		var body struct {
			Name  string `json:"name"`
			Role  string `json:"role"`
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		createdBy := "admin"
		if op, ok := currentOperator(c); ok && op.Name != "" {
			createdBy = op.Name
		}
		if err := opsStore.Upsert(c.Request.Context(), body.Name, body.Role, body.Token, createdBy); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "operator_upsert", body.Name+":"+body.Role)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	// 删操作者（admin+，fail-closed）。
	router.DELETE("/api/admin/operators", opsAdmin, func(c *gin.Context) {
		name := strings.TrimSpace(c.Query("name"))
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name required"})
			return
		}
		if err := opsStore.Delete(c.Request.Context(), name); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "operator_delete", name)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	// 操作审计日志（viewer+：谁在何时做了什么运营动作）。键名归一小写对齐前端。
	router.GET("/api/admin/audit", opsViewer, func(c *gin.Context) {
		limit := 100
		if v := strings.TrimSpace(c.Query("limit")); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		rows, err := opsStore.ListAudit(c.Request.Context(), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		out := make([]gin.H, 0, len(rows))
		for _, r := range rows {
			out = append(out, gin.H{"operator": r.Operator, "role": r.Role, "action": r.Action, "target": r.Target, "created_at": r.CreatedAt})
		}
		c.JSON(http.StatusOK, gin.H{"audit": out})
	})

	// ===== 内容运营 CRUD（母题库 / 翻译模板 / SKU），让「配置页面内容」可实时增删改 =====
	// 母题库 season_content_themes（原死表）：GET 列(viewer) / POST 增改(writer) / DELETE 删(writer)。
	router.GET("/api/admin/content-themes", opsViewer, func(c *gin.Context) {
		themes, err := liveopsSvc.ListContentThemes(c.Request.Context())
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"themes": themes})
	})
	router.POST("/api/admin/content-themes", opsWriter, func(c *gin.Context) {
		var theme liveops.ContentTheme
		if err := c.ShouldBindJSON(&theme); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := liveopsSvc.UpsertContentTheme(c.Request.Context(), theme)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "content_theme_upsert", id)
		c.JSON(http.StatusOK, gin.H{"id": id})
	})
	router.DELETE("/api/admin/content-themes", opsWriter, func(c *gin.Context) {
		id := strings.TrimSpace(c.Query("id"))
		if id == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
			return
		}
		if err := liveopsSvc.DeleteContentTheme(c.Request.Context(), id); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "content_theme_delete", id)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 翻译模板 translation_templates：GET 列(viewer) / POST 增改(writer，写后即时生效) / DELETE 删(writer)。
	router.GET("/api/admin/translation-templates", opsViewer, func(c *gin.Context) {
		rows, err := session.ListTranslationTemplates(c.Request.Context(), deps.Store)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"templates": rows})
	})
	router.POST("/api/admin/translation-templates", opsWriter, func(c *gin.Context) {
		var row session.TranslationTemplateRow
		if err := c.ShouldBindJSON(&row); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := session.UpsertTranslationTemplate(c.Request.Context(), deps.Store, row); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "translation_upsert", row.ReasonCode+"|"+row.AnchorKind)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	router.DELETE("/api/admin/translation-templates", opsWriter, func(c *gin.Context) {
		reasonCode := strings.TrimSpace(c.Query("reason_code"))
		anchorKind := strings.TrimSpace(c.Query("anchor_kind"))
		if reasonCode == "" || anchorKind == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "reason_code and anchor_kind required"})
			return
		}
		if err := session.DeleteTranslationTemplate(c.Request.Context(), deps.Store, reasonCode, anchorKind); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "translation_delete", reasonCode+"|"+anchorKind)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// SKU 目录 admin 写路由（billing 开启时才可用；公开读在 /api/billing/skus）。POST 增改(writer)。
	router.POST("/api/admin/skus", opsWriter, func(c *gin.Context) {
		if billingSvc == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing disabled (QUNXIANG_BILLING_ENABLED off)"})
			return
		}
		var sku billing.SKU
		if err := c.ShouldBindJSON(&sku); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		id, err := billingSvc.UpsertSKU(c.Request.Context(), sku)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "sku_upsert", id)
		c.JSON(http.StatusOK, gin.H{"id": id})
	})

	// ===== 客户管理（玩家账户）：列表/搜索/聚合详情 + 封禁/擦除/退款 =====
	// 含 PII，读端套 opsWriter(operator+)；高危写(封禁/擦除/退款)套 opsAdmin(fail-closed)+auditOps。
	// 列/搜索账户。q 模糊匹配 username/display_name 或 id 精确；limit 默认 100。
	router.GET("/api/admin/clients", opsWriter, func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
			return
		}
		limit := 100
		if v := strings.TrimSpace(c.Query("limit")); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		users, err := deps.Accounts.ListUsers(c.Request.Context(), c.Query("q"), limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"clients": users})
	})

	// 单个客户聚合详情：账号 + 角色/命运进度 + 充值权益 + 实名/防沉迷状态。各子项 best-effort，单项失败不阻断整体。
	router.GET("/api/admin/clients/:id", opsWriter, func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
			return
		}
		id := c.Param("id")
		user, err := deps.Accounts.GetByID(c.Request.Context(), id)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		characters, _ := newSessionService().ListCharactersByAccount(c.Request.Context(), id)
		complianceStatus, _ := complianceSvc.GetStatus(c.Request.Context(), id)
		entitlements := []billing.Entitlement{}
		if billingSvc != nil {
			if ents, eErr := billingSvc.ListEntitlements(c.Request.Context(), id); eErr == nil {
				entitlements = ents
			}
		}
		c.JSON(http.StatusOK, gin.H{
			"account":      user,
			"characters":   characters,
			"entitlements": entitlements,
			"compliance":   complianceStatus,
		})
	})

	// 封禁/解封（admin+，fail-closed）。body {banned:bool}。封禁后该账户拒登录、既有 token 失效。
	router.POST("/api/admin/clients/:id/ban", opsAdmin, func(c *gin.Context) {
		if deps.Accounts == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "account service unavailable"})
			return
		}
		var body struct {
			Banned bool `json:"banned"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := deps.Accounts.SetBanned(c.Request.Context(), c.Param("id"), body.Banned); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		action := "client_unban"
		if body.Banned {
			action = "client_ban"
		}
		auditOps(opsStore, c, action, c.Param("id"))
		c.JSON(http.StatusOK, gin.H{"ok": true, "banned": body.Banned})
	})

	// 按账户数据擦除（admin+，fail-closed，不可逆）：擦该账户全部 session 的对话/记忆/审计/举报。
	router.POST("/api/admin/clients/:id/erase", opsAdmin, func(c *gin.Context) {
		erased, err := newSessionService().EraseAccountPrivateData(c.Request.Context(), c.Param("id"))
		if err != nil && erased == 0 {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "client_erase", c.Param("id"))
		c.JSON(http.StatusOK, gin.H{"erased_sessions": erased})
	})

	// 退款撤权益（admin+，fail-closed）：body {sku_id} 撤指定 SKU，空则撤该账户全部 active 权益。billing 关时 503。
	router.POST("/api/admin/clients/:id/refund", opsAdmin, func(c *gin.Context) {
		if billingSvc == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "billing disabled"})
			return
		}
		var body struct {
			SKUID string `json:"sku_id"`
		}
		_ = c.ShouldBindJSON(&body)
		n, err := billingSvc.RevokeEntitlement(c.Request.Context(), c.Param("id"), body.SKUID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		auditOps(opsStore, c, "client_refund", c.Param("id")+":"+body.SKUID)
		c.JSON(http.StatusOK, gin.H{"revoked": n})
	})

	// 假门预实验留资端点（W0 验证）：POST /api/leads + GET /api/ops/leads-funnel。
	// 漏斗端点是 ops 敏感只读聚合，套 opsTokenGuard；POST /api/leads 是 landing 公开提交，保持公开（不守卫）。
	// 漏斗路由在 leads.go 内注册，这里用路径作用域的前置中间件守卫，避免影响公开的 /api/leads。
	leadsFunnelGuard := opsViewer
	router.Use(func(c *gin.Context) {
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/api/ops/leads-funnel" {
			leadsFunnelGuard(c)
			return
		}
		c.Next()
	})
	registerLeadEndpoints(router, deps.Store)

	// 社会客体撮合（跨玩家，§2.2）：POST 用 MatchScore 四因子 + arbitration 确定性择人绑进社会客体；GET 列出某世界的社会客体。
	router.POST("/api/worlds/:worldId/social-objects", opsWriter, func(c *gin.Context) {
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
	router.POST("/api/worlds/:worldId/seven-interactions", opsWriter, func(c *gin.Context) {
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
	router.GET("/api/consent/pending/:unitId", opsViewer, func(c *gin.Context) {
		reqs, err := newSessionService().ListPendingConsents(c.Request.Context(), strings.TrimSpace(c.Param("unitId")))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"pending": reqs})
	})
	router.POST("/api/consent/:reqId/resolve", opsWriter, func(c *gin.Context) {
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

	// [DEPRECATED — 大世界页游转向] 单机选秀建局入口：玩家入口已改为「登录 → 捏人 → 降生主世界」
	// （GET/POST /api/me/character，见 session/mainworld.go）。本端点不再是默认入口，保留仅为向后兼容
	// （旧客户端 / 对局演练 / 关键战战棋机制验证）。其 draft 选秀分支（ApplyOpeningDraft）代码全部保留——
	// 战棋对局/战斗结算（battlefield/combat_roll/terrain_combat/executor_loop）是命运角色「手动接管关键战」的后端支撑，绝不删除。
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

	// 改密码（账号配置）：登录态(Bearer token 解析出权威账户 id) + 旧密码校验 → 写新密码 + 吊销全部会话。
	router.POST("/api/accounts/change-password", func(c *gin.Context) {
		accountID, ok := authedAccountID(deps.Accounts, c)
		if !ok {
			return // authedAccountID 已写 401/503
		}
		var body struct {
			OldPassword string `json:"old_password"`
			NewPassword string `json:"new_password"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := deps.Accounts.ChangePassword(c.Request.Context(), accountID, body.OldPassword, body.NewPassword); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 大世界页游入口（登录 → 捏人 → 降生主世界 world_default 的账号绑定持久角色）。
	// 鉴权用 authedAccountID（权威账户，忽略请求体里的任何 account_id，杜绝越权为他人降生/查角色）。

	// GET /api/me/character：resume 该账号在主世界的角色。幂等持久：同账号任何设备登录拿到同一角色。
	// 无角色 → {character:{has_character:false}}，前端据此进捏人。
	router.GET("/api/me/character", complianceGate(), func(c *gin.Context) {
		accountID, ok := authedAccountID(deps.Accounts, c)
		if !ok {
			return // authedAccountID 已写 401/503
		}
		character, err := newSessionService().ResumeMainWorldCharacter(c.Request.Context(), accountID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"character": character})
	})

	// POST /api/me/character：捏人降生（1 个玩家角色 + 20 人村庄 + 绑 world_default + 离线宪章落 desire/wound/redline）。
	// 幂等：若该账号已有 world_default 角色，返回既有的、绝不重复降生（防多设备/重复点击重复造人）。
	router.POST("/api/me/character", complianceGate(), func(c *gin.Context) {
		accountID, ok := authedAccountID(deps.Accounts, c)
		if !ok {
			return // authedAccountID 已写 401/503
		}
		var request struct {
			Name    string `json:"name"`
			Origin  string `json:"origin"`
			Desire  string `json:"desire"`
			Wound   string `json:"wound"`
			Redline string `json:"redline"`
			Faction string `json:"faction"` // 阵营开放世界 F1：玩家选阵营（freedom/order/chaos，空则据出身/夙愿启发选）
		}
		// 解析失败不致命：全字段可空（降生会用占位名兜底、阵营据启发选），故仅尽力绑定。
		_ = c.ShouldBindJSON(&request)
		character, err := newSessionService().CreateMainWorldCharacter(c.Request.Context(), accountID, session.MainWorldCharacterInput{
			Name:    request.Name,
			Origin:  request.Origin,
			Desire:  request.Desire,
			Wound:   request.Wound,
			Redline: request.Redline,
			Faction: request.Faction,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		// 漏斗埋点（best-effort，失败绝不影响降生）：仅本次真新降生才埋点（幂等命中既有不重复计角色创建/契约完成）。
		if character.Created {
			_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
				Stage:     analytics.StageActivation,
				Name:      analytics.EventCharacterCreated,
				SessionID: character.SessionID,
				UnitID:    character.UnitID,
				Props:     map[string]any{"account_id": accountID, "world_id": character.WorldID},
			})
			_ = analytics.Emit(c.Request.Context(), deps.Store, analytics.Event{
				Stage:     analytics.StageActivation,
				Name:      analytics.EventCharterCompleted,
				SessionID: character.SessionID,
				UnitID:    character.UnitID,
			})
		}

		status := http.StatusCreated
		if !character.Created {
			status = http.StatusOK // 幂等命中既有角色：200 而非 201（语义上未新建资源）
		}
		c.JSON(status, gin.H{"character": character})
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
	router.POST("/api/sessions/:id/reports/:reportId/resolve", opsWriter, func(c *gin.Context) {
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
	router.GET("/api/sessions/:id/audit", opsViewer, func(c *gin.Context) {
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
	router.POST("/api/sessions/:id/privacy/erase", opsAdmin, func(c *gin.Context) {
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
		auditOps(opsStore, c, "privacy_erase", c.Param("id"))
		c.JSON(http.StatusOK, gin.H{"session": publicForRequest(c, snapshot), "result": result})
	})

	// 批量过期清理是高危写接口（可跨会话删除数据），套 opsTokenGuard（P2 安全修复）。
	router.POST("/api/privacy/purge", opsAdmin, func(c *gin.Context) {
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
		auditOps(opsStore, c, "privacy_purge", strconv.Itoa(request.RetentionDays)+"d")
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

		// §8 一致性收紧闭环驱动（类比归因自动降级护栏）：阶段推进是低频自然触发点，借此读玩家主观三键 OOC 率，
		// 超阈则 latch 收紧（抬高 GateSurprise/惊喜上限门槛）、回落则解除。best-effort：flag 默认关时整体 no-op，绝不影响推进。
		if advanced {
			service.RefreshConsistencyTightening(c.Request.Context())
		}

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
		unitID := c.Param("unitId")
		items, err := newSessionService().OpenFateInbox(c.Request.Context(), unitID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// 牵挂回访埋点（修四维之一恒 0）：玩家专程回看某角色的命运收件箱是低频「回访」读路径。
		// best-effort：吞错只记日志，绝不阻断响应。**不可**挂在高频轮询的 GET /api/sessions/:id 上（会污染回访口径）。
		// userID 软解析（未登录传空串）；sessionID 此读路径无路径作用域，传空（actorID=unitID 已足够标定回访对象）。
		userID := softAccountID(deps.Accounts, c)
		if visitErr := analytics.EmitReturnVisit(c.Request.Context(), deps.Store, unitID, "", userID); visitErr != nil && deps.Logger != nil {
			deps.Logger.Debug("emit return visit failed", "unit_id", unitID, "err", visitErr)
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
	// 「让世界往前走」：推主世界 session 一拍（部署→异步执行一轮自治+边界结算+生活 beat）。供前端按钮/轮询。
	// best-effort：推进失败仍 200 返回 advancing=false（前端据此知道这拍没推动，可重试）；已在执行中返回 advancing=true。
	// 前端拿到 advancing=true 后轮询 GET /api/sessions/:id（ExecutionInProgress 翻 false=这拍跑完）+ 刷新 fate feed。
	router.POST("/api/fate/sessions/:sessionId/advance", func(c *gin.Context) {
		advancing, err := newSessionService().AdvanceFateWorld(c.Request.Context(), c.Param("sessionId"))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"advancing": false, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"advancing": advancing})
	})
	// 地图兴趣点：某会话地图的确定性 POI（地块特殊资源 + 野外 NPC 身上的事件）。纯读，供命运地图画徽标 + 点击查看。
	// 用 :sessionId（与同前缀的 advance 路由占位名一致，避免 gin 通配冲突 panic）。
	router.GET("/api/fate/sessions/:sessionId/map-pois", func(c *gin.Context) {
		pois, err := newSessionService().MapPOIs(c.Request.Context(), c.Param("sessionId"))
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"pois": []session.MapPOI{}, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"pois": pois})
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

	// 某角色对外四轴关系（命运客户端「她身边的人」关系面板用，不带敌意过滤）。与 GET /api/units/:id 同级只读。
	router.GET("/api/units/:id/relations", func(c *gin.Context) {
		rels, err := newSessionService().ListRelations(c.Request.Context(), c.Param("id"), 32)
		if err != nil {
			c.JSON(http.StatusOK, gin.H{"relations": []session.RelationView{}, "error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"relations": rels})
	})

	// ---- 编年史读侧（chronicle）：传记时间线 + 「回到那一刻」（C5 写侧已真写击杀/死亡/继承/升级，本段补读侧 HTTP）----
	// 低频读路径（传记面板 / 分享卡 / 命运 Copilot 取材），**绝不**塞进高频轮询的 GET /api/sessions/:id snapshot。
	// 会话作用域 + 指挥阵营鉴权（同 feuds 端点范式）：拒绝跨会话越权读他局编年史。?limit=&offset= 分页（缺省由 ChronicleFeed 夹默认上限）。
	//
	// chronicleLimitOffset 从 query 解析 limit/offset（缺省/非法归零，交由 ChronicleFeed 夹默认上限）。
	chronicleLimitOffset := func(c *gin.Context) (int, int) {
		limit, offset := 0, 0
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				limit = v
			}
		}
		if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
			if v, err := strconv.Atoi(raw); err == nil {
				offset = v
			}
		}
		return limit, offset
	}
	// chronicleAuth 解析并鉴权会话（指挥阵营 token），返回已校验的 sessionID。失败时已写好响应。
	chronicleAuth := func(c *gin.Context) (string, bool) {
		snapshot, err := newSessionService().GetSnapshot(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return "", false
		}
		if _, ok := resolveCommanderFaction(c, snapshot.ID, snapshot.PlayerFactionID); !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid role token"})
			return "", false
		}
		return snapshot.ID, true
	}
	// 单个单位的编年史时间线（传记 / 分享卡）：倒序分页，逐条带「回到那一刻」锚点。
	router.GET("/api/sessions/:id/units/:unitId/chronicle", func(c *gin.Context) {
		sessionID, ok := chronicleAuth(c)
		if !ok {
			return
		}
		limit, offset := chronicleLimitOffset(c)
		feed, err := newSessionService().ChronicleFeed(c.Request.Context(), sessionID, strings.TrimSpace(c.Param("unitId")), limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"feed": feed})
	})
	// 整局编年史总览（unitID 空）：跨单位的命运总线（编年史总览 / 命运 Copilot 取材）。
	router.GET("/api/sessions/:id/chronicle", func(c *gin.Context) {
		sessionID, ok := chronicleAuth(c)
		if !ok {
			return
		}
		limit, offset := chronicleLimitOffset(c)
		feed, err := newSessionService().ChronicleFeed(c.Request.Context(), sessionID, "", limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"feed": feed})
	})
	// 「回到那一刻」单条端点：按 chronicle_id 反查条目并定位锚点（前端点击某条编年史的「回到那一刻」时调）。
	router.GET("/api/sessions/:id/chronicle/:chronicleId/moment", func(c *gin.Context) {
		sessionID, ok := chronicleAuth(c)
		if !ok {
			return
		}
		view, found, err := newSessionService().ChronicleMomentByID(c.Request.Context(), sessionID, strings.TrimSpace(c.Param("chronicleId")))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "那一刻已无从追溯"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"moment": view})
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

	// 玩家亲自接管一场阵营冲突（F4 H3 接管侧）：把扫描期出可接管命运卡所造的对手接入 EnemyUnitIDs，
	// 仅此刻（玩家在场）才让对手进可致死战斗——绝不在玩家离线的异步执行里离线掷骰打死主角。
	// opponentId 取命运卡 battle.threat_id（阵营冲突对手 ID，fconflict_ 前缀）。真实动作，QUNXIANG_FACTION_PVE 门控的产物。
	router.POST("/api/sessions/:id/faction-conflict/:opponentId/takeover", func(c *gin.Context) {
		if err := newSessionService().ResolveFactionConflictTakeover(c.Request.Context(), c.Param("id"), c.Param("opponentId")); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
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

	// 玩家在线直接操作角色（命运混合模型：离线她自治，上线玩家可直接干预，与世界自治推进共存）。
	// 直接移动：把她移到目标格（校验在界内/非水山阻挡/归属本会话），位置非受保护字段直改+持久化。
	router.POST("/api/sessions/:id/units/:unitId/move", func(c *gin.Context) {
		var body struct {
			Q int `json:"q"`
			R int `json:"r"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		q, r, err := newSessionService().PlayerMoveUnit(c.Request.Context(), c.Param("id"), c.Param("unitId"), body.Q, body.R)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"q": q, "r": r})
	})
	// 直接穿装备：从她背包把某件装备穿上（复用 EquipBackpackItem，含重算派生攻防）。
	router.POST("/api/sessions/:id/units/:unitId/equip", func(c *gin.Context) {
		var body struct {
			ItemID string `json:"item_id"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if err := newSessionService().PlayerEquipItem(c.Request.Context(), c.Param("id"), c.Param("unitId"), body.ItemID); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
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

	// ---- 副本异步分段推进（设计 PvE威胁系统.md §3-5）。与同步 /dungeon 同鉴权档（无 ops_token）。 ----
	// flag QUNXIANG_DUNGEON 关时各入口返回 ErrDungeonDisabled → 路由统一当 409 透出（前端据「未启用」识别）。
	dungeonStatusFor := func(err error) int {
		if err == session.ErrDungeonDisabled {
			return http.StatusConflict
		}
		return http.StatusBadRequest
	}

	// 创建副本异步推进首段。
	router.POST("/api/sessions/:id/dungeon/segments", func(c *gin.Context) {
		var body struct {
			UnitIDs []string `json:"unit_ids"`
			Floors  int      `json:"floors"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		seg, err := newSessionService().StartDungeonAsync(c.Request.Context(), c.Param("id"), body.UnitIDs, body.Floors)
		if err != nil {
			c.JSON(dungeonStatusFor(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"segment": gin.H{
			"segment_id": seg.ID,
			"floors":     seg.Floors,
			"floor":      seg.Floor,
			"state":      seg.State,
		}})
	})

	// 推进当前段一层（按 segmentId 加载段并 RunDungeonSegment）。
	router.POST("/api/sessions/:id/dungeon/segments/:segmentId/run", func(c *gin.Context) {
		result, err := newSessionService().RunDungeonSegmentByID(c.Request.Context(), c.Param("id"), c.Param("segmentId"))
		if err != nil {
			c.JSON(dungeonStatusFor(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	// 玩家回来据选择续跑/见好就收（按 sessionID 载 state + ResumePausedDungeonSegment）。
	router.POST("/api/sessions/:id/dungeon/segments/:segmentId/resume", func(c *gin.Context) {
		var body struct {
			Choice string `json:"choice"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result, err := newSessionService().ResumeDungeonSegmentByID(c.Request.Context(), c.Param("id"), c.Param("segmentId"), body.Choice)
		if err != nil {
			c.JSON(dungeonStatusFor(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"result": result})
	})

	// 段状态视图（绝不裸露数值血量）。不存在 → 404。
	router.GET("/api/sessions/:id/dungeon/segments/:segmentId/status", func(c *gin.Context) {
		status, err := newSessionService().DungeonSegmentStatusView(c.Request.Context(), c.Param("id"), c.Param("segmentId"))
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if status == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "dungeon segment not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": status})
	})

	// 标记玩家离场（开始 charter 超时计时，供主控在玩家断线/挂起时调用）。
	router.POST("/api/sessions/:id/dungeon/segments/:segmentId/leave", func(c *gin.Context) {
		if err := newSessionService().MarkDungeonSegmentLeft(c.Request.Context(), c.Param("id"), c.Param("segmentId")); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
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
