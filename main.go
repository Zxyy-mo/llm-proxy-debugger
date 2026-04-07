package main

import (
	"flag"
	"net/http"
	"net/url"
	"time"

	"go.uber.org/zap"
)

func main() {
	flag.Parse()

	initLogger(*logDir)
	defer logger.Sync()

	go hub.Run()

	targetURL, err := url.Parse(*targetAddr)
	if err != nil {
		logger.Fatal("解析目标地址失败", zap.Error(err))
	}

	logger.Info("🚀 代理服务启动中...")
	logger.Info("📡 监听地址", zap.String("addr", *listenAddr))
	logger.Info("🎯 转发目标", zap.String("target", *targetAddr))

	proxy := createReverseProxy(targetURL)

	mux := http.NewServeMux()
	mux.Handle("/", proxy)
	mux.HandleFunc("/api/ws", serveWS)
	mux.HandleFunc("/api/sessions", apiSessionsHandler)
	mux.HandleFunc("/api/rules", apiRulesHandler)

	server := &http.Server{
		Addr:         *listenAddr,
		Handler:      mux,
		ReadTimeout:  0,
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	logger.Info("✅ 代理服务及 API 已启动")
	if err := server.ListenAndServe(); err != nil {
		logger.Fatal("服务器启动失败", zap.Error(err))
	}
}
