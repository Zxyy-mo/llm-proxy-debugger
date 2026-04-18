package main

import (
	"flag"
	"net/http"
	"net/url"
	"os"
	"time"

	"go.uber.org/zap"
)

func main() {
	flag.Parse()

	initLogger(*logDir)
	defer logger.Sync()

	// 尝试加载路由配置文件
	if err := LoadRoutesFromFile(*routesFile); err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("加载路由配置文件失败", zap.String("file", *routesFile), zap.Error(err))
		}
	} else {
		logger.Info("✅ 成功从配置文件加载路由映射", zap.String("file", *routesFile))
	}

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
	mux.HandleFunc("/api/routes", apiRoutesHandler) // 添加路由配置 API

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
