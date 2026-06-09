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

	// 大世界 region-runner（M7.3-real-1，默认关闭、纯影子）：按真实时钟低频唤醒离线单位决策。
	// QUNXIANG_REGION_RUNNER_ENABLED=true 才启动；QUNXIANG_REGION_TICK_SECONDS 设逻辑 tick 的秒长（默认 30）。
	// Apply=false 时纯 shadow（只记日志，real-1）；QUNXIANG_REGION_RUNNER_APPLY=true 才真应用 L1 决策（real-2 灰度）。
	regionRunnerEnabled := envBool("QUNXIANG_REGION_RUNNER_ENABLED")
	regionThreatsEnabled := envBool("QUNXIANG_REGION_RUNNER_THREATS")
	regionRunner := regionrunner.New(db, regionrunner.Config{
		Enabled:     regionRunnerEnabled,
		Apply:       envBool("QUNXIANG_REGION_RUNNER_APPLY"),
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
	// PvE（默认关）：QUNXIANG_REGION_RUNNER_THREATS 开即 roll 威胁——注入 AnchorDensity 做锚加权（PvE-4，威胁扎堆她在乎处）；
	// 再开 QUNXIANG_REGION_RUNNER_THREATS_APPLY 才注入真 elite 遭遇结算（PvE-2，命中→改 HP/钱包、分赃/惩罚、命运卡），仅 THREATS=shadow。
	// ⚠️ 威胁 roll 还依赖 QUNXIANG_REGION_RUNNER_APPLY=true（roll 在 applyAmbientL1 内）——完整真打需 ENABLED+APPLY+THREATS+THREATS_APPLY 四者皆开。
	// region-runner 专用的长生命 Service（造人/战斗/锚查询用同一 db）。遭遇前 maybeEncounterThreat 已查让位收窄并发窗口。
	if regionThreatsEnabled {
		threatSvc := session.NewService(db, aiService)
		regionRunner.SetAnchorDensityProvider(threatSvc.AnchorDensity)
		if envBool("QUNXIANG_REGION_RUNNER_THREATS_APPLY") {
			regionRunner.SetThreatHandler(func(ctx context.Context, sessionID, unitID string) error {
				_, err := threatSvc.TriggerEliteEncounter(ctx, sessionID, unitID)
				return err
			})
		}
	}
	// real-3 HOT-LLM（默认关）：QUNXIANG_REGION_RUNNER_LLM=true 才给 HOT 单位上 LLM 离线决策；
	// 进程级成本上限 QUNXIANG_REGION_LLM_BUDGET_USD（默认 1.0 USD），沿用 session 同一单价表估算。
	if envBool("QUNXIANG_REGION_RUNNER_LLM") {
		budgetUSD := envFloatDefault("QUNXIANG_REGION_LLM_BUDGET_USD", 1.0)
		if budgetUSD <= 0 { // 配置笔误 0/负数不得让烧钱护栏失效（SetLLMClient 还会再夹一层做防御纵深）。
			logger.Warn("QUNXIANG_REGION_LLM_BUDGET_USD non-positive; using default", "got", budgetUSD, "default", 1.0)
			budgetUSD = 1.0
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
		// region-runner 启用时，建局/组队才把玩家单位 seed 进离线调度（M7.3-real-4b，默认关→零成本）。
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

	// 命运世界自动 tick（flag QUNXIANG_FATE_AUTOTICK 默认关，关时每次唤醒只查一次 flag 即返回、零行为）：
	// 开启时低频（默认 60s，QUNXIANG_FATE_AUTOTICK_SECONDS 可调）扫 world_default 活跃主世界角色，各推一拍自治生活。
	// 专用长生命 Service：异步执行（与生产 router 一致，AdvanceFateWorld 才会起后台执行一轮）+ 广播器（生活 beat/快照 WS 实时推送）+
	// 归因强制（与生产一致）。成本：每拍 1 次 LLM 自治决策，低频 + best-effort + flag 默认关 控成本。随 ctx 取消优雅退出。
	fateTickDone := make(chan struct{})
	go func() {
		fateSvc := session.NewServiceWithColdStore(db, aiService, coldStore)
		fateSvc.SetAsyncExecution(true)
		fateSvc.SetBroadcaster(hub)
		fateSvc.SetAttributionEnforcement(true)
		fateSvc.SetReflexShortCircuit(envBoolDefault("QUNXIANG_REFLEX_SHORTCIRCUIT", true))
		interval := time.Duration(envIntDefault("QUNXIANG_FATE_AUTOTICK_SECONDS", 60)) * time.Second
		fateSvc.RunFateAutoTickLoop(ctx, interval)
		close(fateTickDone)
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
