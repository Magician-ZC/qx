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
	"qunxiang/backend/internal/httpapi"
	"qunxiang/backend/internal/regionrunner"
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

	hub := ws.NewHub(logger)
	aiService := ai.NewService(cfg, logger)

	// 大世界 region-runner（M7.3-real-1，默认关闭、纯影子）：按真实时钟低频唤醒离线单位决策。
	// QUNXIANG_REGION_RUNNER_ENABLED=true 才启动；QUNXIANG_REGION_TICK_SECONDS 设逻辑 tick 的秒长（默认 30）。
	// Apply=false 时纯 shadow（只记日志，real-1）；QUNXIANG_REGION_RUNNER_APPLY=true 才真应用 L1 决策（real-2 灰度）。
	regionRunnerEnabled := envBool("QUNXIANG_REGION_RUNNER_ENABLED")
	regionRunner := regionrunner.New(db, regionrunner.Config{
		Enabled:     regionRunnerEnabled,
		Apply:       envBool("QUNXIANG_REGION_RUNNER_APPLY"),
		TickSeconds: int64(envIntDefault("QUNXIANG_REGION_TICK_SECONDS", 30)),
	}, logger)
	regionRunner.SetExecutionGuard(session.IsExecutionRunning) // 让位聚焦战斗：在战会话的单位由战斗循环管
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
