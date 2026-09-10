package main

import (
	"net/http"
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

	s := store.New()

	srv, err := proxy.NewServer(cfg, log, h, s)
	if err != nil {
		log.Fatal("初始化代理服务失败", zap.Error(err))
	}

	log.Info("🚀 代理服务启动中...")
	log.Info("📡 监听地址", zap.String("addr", cfg.ListenAddr))
	log.Info("🎯 转发目标", zap.String("target", cfg.TargetAddr))

	mux := http.NewServeMux()
	mux.Handle("/", srv)
	mux.HandleFunc("/api/ws", h.ServeWS)
	mux.HandleFunc("/api/sessions", s.SessionsHandler)
	mux.HandleFunc("/api/graph", s.GraphHandler)
	mux.HandleFunc("/api/rules", s.RulesHandler)
	mux.HandleFunc("/api/rules/", s.RulesHandler)
	mux.HandleFunc("/api/requests/", srv.RequestsHandler)
	mux.HandleFunc("/api/interceptions", srv.InterceptionsHandler)
	mux.HandleFunc("/api/interceptions/", srv.InterceptionsHandler)
	mux.HandleFunc("/api/replays", srv.ReplaysHandler)
	mux.HandleFunc("/api/replays/", srv.ReplaysHandler)

	server := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	log.Info("✅ 代理服务及 API 已启动")
	if err := server.ListenAndServe(); err != nil {
		log.Fatal("服务器启动失败", zap.Error(err))
	}
}
