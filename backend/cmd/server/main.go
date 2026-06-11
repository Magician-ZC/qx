package main

// 文件说明：后端启动入口，负责装配存储、AI 服务、HTTP 路由、WebSocket Hub 与优雅关停。

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"qunxiang/backend/internal/account"
	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/config"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/featureflags"
	"qunxiang/backend/internal/httpapi"
	"qunxiang/backend/internal/region"
	"qunxiang/backend/internal/regionrunner"
	"qunxiang/backend/internal/runtimeconfig"
	"qunxiang/backend/internal/session"
	mysqlstore "qunxiang/backend/internal/storage/mysql"
	postgresstore "qunxiang/backend/internal/storage/postgres"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/ws"
)

// main 启动后端进程：装配存储、AI、路由与优雅关停信号。
func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := openPrimaryStore(cfg)
	if err != nil {
		logger.Error("open primary store", "error", err)
		os.Exit(1)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			logger.Error("close sqlite store", "error", closeErr)
		}
	}()

	if err := events.SeedReasonCodeCatalog(context.Background(), db); err != nil {
		logger.Error("seed reason code catalog", "error", err)
		os.Exit(1)
	}

	// M1 翻译矩阵启动 seed：把内置 (reason_code × anchor_kind) 命运 beat 模板幂等 upsert 进 translation_templates 表，
	// 供 SurfaceFateEvent 的 translateFateBeatFromDB 消费。best-effort：失败只记日志、绝不阻断启动
	//（loadTranslationTemplate 查库失败会优雅回退内置矩阵 / DefaultReasonText，命运路由仍可用）。
	if err := session.SeedTranslationTemplates(context.Background(), db); err != nil {
		logger.Error("seed translation templates", "error", err)
	}

	// GM 后台运行时 flag 持久化：注入双驱动 Store 后从 feature_flag_overrides 表回灌已存 override，
	// 让 GM 在后台设过的开关重启存活。best-effort：失败只记日志、绝不阻断启动（override 缺省回退环境变量/默认值）。
	featureflags.SetStore(session.NewFeatureFlagStore(db))
	if n, err := featureflags.LoadFromStore(context.Background()); err != nil {
		logger.Error("load feature flag overrides", "error", err)
	} else if n > 0 {
		logger.Info("loaded feature flag overrides", "count", n)
	}

	// GM 后台类型化运行时配置（数值/阈值/LLM 热切）持久化：注入双驱动 Store 后从 runtime_config_overrides
	// 表回灌已存 override，使 GM 设过的参数重启存活。best-effort：失败只记日志、绝不阻断启动（override 缺省回退
	// catalog 注册的默认值）。回灌时已下线/已非法（范围改过）的历史 override 被 runtimeconfig 侧丢弃。
	runtimeconfig.SetStore(session.NewRuntimeConfigStore(db))
	if n, err := runtimeconfig.LoadFromStore(context.Background()); err != nil {
		logger.Error("load runtime config overrides", "error", err)
	} else if n > 0 {
		logger.Info("loaded runtime config overrides", "count", n)
	}

	hub := ws.NewHub(logger)
	aiService := ai.NewService(cfg, logger)

	// 大世界 region-runner（M7.3-real-1，默认开、全功率）：按真实时钟低频唤醒离线单位决策。
	// QUNXIANG_REGION_RUNNER_ENABLED 默认开（显式设 false/0/no/off 仍能关）；QUNXIANG_REGION_TICK_SECONDS 设逻辑 tick 的秒长（默认 30）。
	// Apply 默认开即真应用 L1 决策（real-2）；显式 QUNXIANG_REGION_RUNNER_APPLY=false 才退回纯 shadow（只记日志，real-1）。
	regionRunnerEnabled := envBoolDefault("QUNXIANG_REGION_RUNNER_ENABLED", true)
	regionThreatsEnabled := envBoolDefault("QUNXIANG_REGION_RUNNER_THREATS", true)
	regionRunner := regionrunner.New(db, regionrunner.Config{
		Enabled:     regionRunnerEnabled,
		Apply:       envBoolDefault("QUNXIANG_REGION_RUNNER_APPLY", true),
		Threats:     regionThreatsEnabled,
		TickSeconds: int64(envIntDefault("QUNXIANG_REGION_TICK_SECONDS", 30)),
	}, logger)
	regionRunner.SetExecutionGuard(session.IsExecutionRunning) // 让位聚焦战斗：在战会话的单位由战斗循环管
	// region 真分片（§11.3，默认关）：QUNXIANG_REGION_SHARDING=true 才注入 region.Registry——schedulePass 改按活跃度档
	// （HOT/WARM）优先调度活跃区、推进 per-region 逻辑时钟、威胁经 BumpThreatLevel 扎堆。注入即装配（与 regionrunner 并列）；
	// 实际生效还需 flag 开（SetRegistry 注入后，shardingEnabled() 关时 schedulePass 仍走 DistinctWakeRegions 旧路径）。
	if envBool("QUNXIANG_REGION_SHARDING") {
		regionRunner.SetRegistry(region.New(db))
	}
	// PvE（默认开）：QUNXIANG_REGION_RUNNER_THREATS 默认开即 roll 威胁——注入 AnchorDensity 做锚加权（PvE-4，威胁扎堆她在乎处）；
	// QUNXIANG_REGION_RUNNER_THREATS_APPLY 默认开即注入真 elite 遭遇结算（PvE-2，命中→改 HP/钱包、分赃/惩罚、命运卡），显式设 false 才仅 THREATS=shadow。
	// ⚠️ 威胁 roll 还依赖 QUNXIANG_REGION_RUNNER_APPLY=true（roll 在 applyAmbientL1 内）——四者皆默认开即完整真打；显式关任一即收窄。
	// region-runner 专用的长生命 Service（造人/战斗/锚查询用同一 db）。遭遇前 maybeEncounterThreat 已查让位收窄并发窗口。
	if regionThreatsEnabled {
		threatSvc := session.NewService(db, aiService)
		regionRunner.SetAnchorDensityProvider(threatSvc.AnchorDensity)
		if envBoolDefault("QUNXIANG_REGION_RUNNER_THREATS_APPLY", true) {
			regionRunner.SetThreatHandler(func(ctx context.Context, sessionID, unitID string) error {
				_, err := threatSvc.TriggerEliteEncounter(ctx, sessionID, unitID)
				return err
			})
		}
	}
	// real-3 HOT-LLM（默认开）：默认给 HOT 单位上 LLM 离线决策；显式 QUNXIANG_REGION_RUNNER_LLM=false 才关。
	// 进程级成本上限 QUNXIANG_REGION_LLM_BUDGET_USD 默认 100.0 USD（全功率默认开下若仍用 1.0 会秒烧到 $1 即 latch 降级、名不副实；
	// 这是进程级累计上限，按部署规模上调/下调即可），沿用 session 同一单价表估算。
	if envBoolDefault("QUNXIANG_REGION_RUNNER_LLM", true) {
		budgetUSD := envFloatDefault("QUNXIANG_REGION_LLM_BUDGET_USD", 100.0)
		if budgetUSD <= 0 { // 配置笔误 0/负数不得让烧钱护栏失效（SetLLMClient 还会再夹一层做防御纵深）。
			logger.Warn("QUNXIANG_REGION_LLM_BUDGET_USD non-positive; using default", "got", budgetUSD, "default", 100.0)
			budgetUSD = 100.0
		}
		regionRunner.SetLLMClient(aiService, session.EstimateLLMCostUSD, budgetUSD)
	}
	accountService := account.NewService(db, cfg.AuthTokenTTL)
	if err := accountService.EnsureSchema(context.Background()); err != nil {
		logger.Error("ensure account schema on sqlite", "error", err)
		os.Exit(1)
	}
	var coldStore *sql.DB
	if cfg.PostgresDSN != "" {
		postgresDB, err := postgresstore.Open(cfg.PostgresDSN)
		if err != nil {
			logger.Error("open postgres store", "error", err)
			os.Exit(1)
		}
		defer func() {
			if closeErr := postgresDB.Close(); closeErr != nil {
				logger.Error("close postgres store", "error", closeErr)
			}
		}()

		accountService = account.NewService(postgresDB, cfg.AuthTokenTTL)
		if err := accountService.EnsureSchema(context.Background()); err != nil {
			logger.Error("ensure account schema on postgres", "error", err)
			os.Exit(1)
		}
		if err := session.EnsureColdStorageSchema(context.Background(), postgresDB); err != nil {
			logger.Error("ensure cold memory schema on postgres", "error", err)
			os.Exit(1)
		}
		coldStore = postgresDB
		logger.Info("postgres account service enabled")
	} else {
		logger.Info("primary account service enabled", "db_driver", cfg.DBDriver)
	}
	router := httpapi.NewRouter(httpapi.Dependencies{
		Config:       cfg,
		Logger:       logger,
		Hub:          hub,
		Store:        db,
		AI:           aiService,
		ColdStore:    coldStore,
		Accounts:     accountService,
		RegionRunner: regionRunner,
		// region-runner 启用时，建局/组队才把玩家单位 seed 进离线调度（M7.3-real-4b，默认开→默认 seed；显式关 ENABLED 即零成本）。
		RegionRunnerEnabled: regionRunnerEnabled,
		// 反射真短路（降本，默认开）：日常安静 tick 的单位决策由反射层零成本落地、跳过 LLM。
		// 短路面已被 reflexShortCircuitApplies 收窄到极保守子集（immediateOrder gate + NeedsLLM=false + hold/continue），
		// 默认开实现降本意图；紧急回退置 QUNXIANG_REFLEX_SHORTCIRCUIT=false 即关。
		ReflexShortCircuit: envBoolDefault("QUNXIANG_REFLEX_SHORTCIRCUIT", true),
	})

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 启动 region-runner（未 Enabled 时 Run 立即返回）；随关停信号 ctx 优雅退出。
	runnerDone := make(chan struct{})
	go func() {
		regionRunner.Run(ctx)
		close(runnerDone)
	}()

	// 私有世代双推进者运维告警（修 Phase5 major-2，docs §5 风险5）：region-runner 是 session-agnostic、纯按 region_id 调度，
	// 对单位属哪个世代无感——REGION_RUNNER_APPLY=true 时它会对 world_default 私有主角（region_id==sessionID）真应用 ambient L1，
	// 而 FATE_AUTOTICK=true 时 RunFateAutoTickLoop 又扫同一私有 session 各推一拍命运自治。Phase5「二选一」只硬隔离了共享世代
	// （sharedWorldDrivenByRegionRunner + 扫描排除），私有世代未硬隔离：同一私有主角会被两条 goroutine 各推一拍（觅食/休息 vs
	// 命运自治各写一次），仅靠 execGuard 在「两者时间窗恰好重叠」时软让位，非重叠窗口仍双驱动语义错位。私有世代是 fate-autotick
	// 的地盘，两者**不应同开 APPLY**。此处启动即检测同开并 Warn（最小运维契约；不拒启以免误伤 shadow/共享场景）。
	// 读 FATE_AUTOTICK 用 EnvOrOverride（与 fateAutoTickEnabled 同口径，含已回灌的 GM 持久 override）；APPLY 是纯环境变量。
	// 注：FATE_AUTOTICK 与 REGION_RUNNER_APPLY 现都默认开，故此 Warn 默认会打印——这是【预期】（私有遗留世代 world_default 的
	// 已知权衡；共享世代 world_shared_v1 已硬隔离无冲突）。保留 Warn 作运维契约，勿删。
	if envBoolDefault("QUNXIANG_REGION_RUNNER_APPLY", true) && fateAutoTickFlagOn() {
		logger.Warn("double-driver risk: REGION_RUNNER_APPLY and FATE_AUTOTICK both ON — " +
			"region-runner and fate-autotick will each advance the SAME world_default (private) protagonist per tick " +
			"(shared world is isolated, private world is NOT). Disable one of them for private generations. " +
			"See docs/共享世界改造方案-方向B-2026-06-10.md §5 risk 5.")
	}

	// 命运世界自动 tick（flag QUNXIANG_FATE_AUTOTICK 默认开，显式关时每次唤醒只查一次 flag 即返回、零行为）：
	// 默认低频（默认 60s，QUNXIANG_FATE_AUTOTICK_SECONDS 可调）扫 world_default 活跃主世界角色，各推一拍自治生活。
	// 专用长生命 Service：异步执行（与生产 router 一致，AdvanceFateWorld 才会起后台执行一轮）+ 广播器（生活 beat/快照 WS 实时推送）+
	// 归因强制（与生产一致）。成本：每拍 1 次 LLM 自治决策，低频 + best-effort 控成本；紧急回退置 QUNXIANG_FATE_AUTOTICK=false 即关。随 ctx 取消优雅退出。
	fateTickDone := make(chan struct{})
	go func() {
		fateSvc := session.NewServiceWithColdStore(db, aiService, coldStore)
		fateSvc.SetAsyncExecution(true)
		fateSvc.SetBroadcaster(hub)
		fateSvc.SetAttributionEnforcement(true)
		// 停双推进者纵深守门通电（修 Phase5 major-1）：fateSvc 必须与生产 HTTP Service（router.go 的 newSessionService）
		// 一样按 region-runner 是否启用武装 ambientSchedulingEnabled，否则 AdvanceFateWorld 入口的 sharedWorldDrivenByRegionRunner
		// 守门在「唯一会循环喂 session 给 AdvanceFateWorld 的生产路径=本 autotick loop」上恒为 false、永不让位——
		// 共享主角红线只剩 ListMainWorldSessionIDs 扫描排除单层兜底，注释自称的「防御纵深」名存实亡（async-boundary-hooks-trap
		// 同型坑：只接一路=纵深线上永不通电）。武装后：扫描排除 + 入口守门双层，任一被改/被绕都不会双推共享主角。
		fateSvc.SetAmbientSchedulingEnabled(regionRunnerEnabled)
		fateSvc.SetReflexShortCircuit(envBoolDefault("QUNXIANG_REFLEX_SHORTCIRCUIT", true))
		interval := time.Duration(envIntDefault("QUNXIANG_FATE_AUTOTICK_SECONDS", 60)) * time.Second
		fateSvc.RunFateAutoTickLoop(ctx, interval)
		close(fateTickDone)
	}()

	// 跨玩家 consent 超时 sweep（修复 major-3）：独立于 FATE_AUTOTICK 的全局低频清理，**恒开**（无 flag 门控）。
	// 只做全局安全操作——把超 72h TTL 的 pending 置 expired + 对层3 给发起方投回响卡（写发起方自己收件箱，不写他人 units/relations、不跑 LLM）——
	// 保证「真离线、从不上线」的 B 的层2/3 pending 也按 TTL 失效，不无限挂起。层2「据归因自治接受」仍由 target 自己 session 边界驱动。
	// 默认 10min 一拍（QUNXIANG_CONSENT_SWEEP_SECONDS 可调），随 ctx 取消优雅退出。
	consentSweepDone := make(chan struct{})
	go func() {
		sweepSvc := session.NewServiceWithColdStore(db, aiService, coldStore)
		interval := time.Duration(envIntDefault("QUNXIANG_CONSENT_SWEEP_SECONDS", 600)) * time.Second
		sweepSvc.RunConsentExpirySweepLoop(ctx, interval)
		close(consentSweepDone)
	}()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("backend listening", "addr", cfg.HTTPAddr, "db_driver", cfg.DBDriver, "sqlite_path", cfg.SQLitePath)
		errCh <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown http server", "error", err)
			os.Exit(1)
		}

		// 等 region-runner 跑完手头作业优雅退出（ctx 已取消，有界等待）。
		select {
		case <-runnerDone:
		case <-time.After(10 * time.Second):
			logger.Warn("region-runner did not stop in time")
		}

		// 等命运 tick 循环优雅退出（ctx 已取消，下一次 select 即返回；有界等待防卡死）。
		select {
		case <-fateTickDone:
		case <-time.After(10 * time.Second):
			logger.Warn("fate auto-tick did not stop in time")
		}

		// 等 consent 超时 sweep 循环优雅退出（ctx 已取消，下一次 select 即返回；有界等待防卡死）。
		select {
		case <-consentSweepDone:
		case <-time.After(10 * time.Second):
			logger.Warn("consent expiry sweep did not stop in time")
		}

		logger.Info("backend stopped")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server crashed", "error", err)
			os.Exit(1)
		}
	}
}

// envBool 读布尔环境变量（true/1/yes/on 视为真，不区分大小写）。
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// fateAutoTickFlagOn 读 QUNXIANG_FATE_AUTOTICK，与 session.fateAutoTickEnabled 同口径（默认开，可显式关）——经
// featureflags.EnvOrOverride 读（含已回灌的 GM 持久 override，非仅环境变量），使启动期双推进者告警与运行期 ticker 的
// 实际开关判定一致。session.fateAutoTickEnabled 已转默认开，此处必须同步默认开，否则启动 Warn 与运行期 ticker 判定漂移。
func fateAutoTickFlagOn() bool {
	switch strings.ToLower(strings.TrimSpace(featureflags.EnvOrOverride("QUNXIANG_FATE_AUTOTICK"))) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

// envBoolDefault 读「默认开但可显式关」的布尔环境变量：未设/非法→返回 def；
// 仅当值为 false/0/no/off 才关。用于把已就绪的能力转默认开、同时保留紧急回退开关。
func envBoolDefault(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return def
	}
}

// envIntDefault 读整数环境变量，缺失/非法时回退默认值。
func envIntDefault(key string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil {
		return v
	}
	return def
}

// envFloatDefault 读浮点环境变量，缺失/非法时回退默认值。
func envFloatDefault(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64); err == nil {
		return v
	}
	return def
}

func openPrimaryStore(cfg config.Config) (*sql.DB, error) {
	switch cfg.DBDriver {
	case "", "sqlite":
		return sqlitestore.Open(cfg.SQLitePath)
	case "mysql":
		return mysqlstore.Open(cfg.MySQLDSN)
	default:
		return nil, errors.New("unsupported QUNXIANG_DB_DRIVER: " + cfg.DBDriver)
	}
}
