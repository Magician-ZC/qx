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
	"syscall"
	"time"

	"qunxiang/backend/internal/account"
	"qunxiang/backend/internal/ai"
	"qunxiang/backend/internal/config"
	"qunxiang/backend/internal/engine/events"
	"qunxiang/backend/internal/httpapi"
	"qunxiang/backend/internal/session"
	postgresstore "qunxiang/backend/internal/storage/postgres"
	sqlitestore "qunxiang/backend/internal/storage/sqlite"
	"qunxiang/backend/internal/ws"
)

// main 启动后端进程：装配存储、AI、路由与优雅关停信号。
func main() {
	cfg := config.Load()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	db, err := sqlitestore.Open(cfg.SQLitePath)
	if err != nil {
		logger.Error("open sqlite store", "error", err)
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
		logger.Info("sqlite account service enabled (QUNXIANG_POSTGRES_DSN is empty)")
	}
	router := httpapi.NewRouter(httpapi.Dependencies{
		Config:    cfg,
		Logger:    logger,
		Hub:       hub,
		Store:     db,
		AI:        aiService,
		ColdStore: coldStore,
		Accounts:  accountService,
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

	errCh := make(chan error, 1)
	go func() {
		logger.Info("backend listening", "addr", cfg.HTTPAddr, "sqlite_path", cfg.SQLitePath)
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

		logger.Info("backend stopped")
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server crashed", "error", err)
			os.Exit(1)
		}
	}
}
