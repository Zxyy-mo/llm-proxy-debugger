package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/config"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/logger"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/proxy"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func main() {
	cfg := config.Load()

	log := logger.New(cfg.LogDir)
	defer log.Sync() //nolint:errcheck

	h := hub.New(log)
	go h.Run()

	s, err := store.Open(cfg.DataFile)
	if err != nil {
		log.Fatal("无法打开历史数据库", zap.Error(err))
	}
	defer func() {
		if err := s.Close(); err != nil {
			log.Error("保存历史失败", zap.Error(err))
		}
	}()

	srv, err := proxy.NewServer(cfg, log, h, s)
	if err != nil {
		log.Fatal("初始化代理服务失败", zap.Error(err))
	}
	defer srv.Close()

	log.Info("🚀 代理服务启动中...")
	log.Info("📡 监听地址", zap.String("addr", cfg.ListenAddr))
	log.Info("🎯 转发目标", zap.String("target", cfg.TargetAddr))

	mux := http.NewServeMux()
	mux.Handle("/", srv)
	if cfg.WebDir != "" && cfg.WebDir != "-" {
		mux.Handle("/ui/", http.StripPrefix("/ui/", http.FileServer(http.Dir(cfg.WebDir))))
		log.Info("🖥️ 管理页面", zap.String("path", "/ui/"), zap.String("assets", cfg.WebDir))
	}
	mux.HandleFunc("/api/ws", h.ServeWS)
	mux.HandleFunc("/api/sessions", s.SessionsHandler)
	mux.HandleFunc("/api/graph", s.GraphHandler)
	mux.HandleFunc("/api/runs", s.RunsHandler)
	mux.HandleFunc("/api/attempts/", s.AttemptsHandler)
	mux.HandleFunc("/api/rules", s.RulesHandler)
	mux.HandleFunc("/api/rules/", s.RulesHandler)
	mux.HandleFunc("/api/requests/", srv.RequestsHandler)
	mux.HandleFunc("/api/responses/", s.ResponsesHandler)
	mux.HandleFunc("/api/context-diff/", s.ContextHandler)
	mux.HandleFunc("/api/privacy", s.PrivacyHandler)
	mux.HandleFunc("/api/privacy/", s.PrivacyHandler)
	mux.HandleFunc("/api/history", s.HistoryHandler)
	mux.HandleFunc("/api/history/", s.HistoryHandler)
	mux.HandleFunc("/api/tool-spans", func(w http.ResponseWriter, r *http.Request) {
		s.ToolSpansHandler(w, r)
		h.Publish(map[string]any{"event": "sessions_updated"})
	})
	mux.HandleFunc("/api/providers", srv.ProvidersHandler)
	mux.HandleFunc("/api/provider-presets", srv.ProviderPresetsHandler)
	mux.HandleFunc("/api/provider-models", srv.ProviderModelsHandler)
	mux.HandleFunc("/api/provider-history", srv.ProviderHistoryHandler)
	mux.HandleFunc("/api/interceptions", srv.InterceptionsHandler)
	mux.HandleFunc("/api/interceptions/", srv.InterceptionsHandler)
	mux.HandleFunc("/api/replays", srv.ReplaysHandler)
	mux.HandleFunc("/api/replays/", srv.ReplaysHandler)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadTimeout:       0,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 15 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	log.Info("✅ 代理服务及 API 已启动")
	failures := make(chan error, 1)
	go func() { failures <- server.ListenAndServe() }()
	select {
	case err := <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("服务器启动失败", zap.Error(err))
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
		}
	}
}
