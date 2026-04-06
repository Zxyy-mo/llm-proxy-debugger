package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	listenAddr     = flag.String("listen", "0.0.0.0:12337", "监听地址")
	targetAddr     = flag.String("target", "http://127.0.0.1:28000", "转发目标地址")
	logDir         = flag.String("logdir", "log", "日志目录")
	maxBodyLogSize = flag.Int("maxbody", 10240, "最大记录的 body 大小（字节），超过则截断")
)

var logger *zap.Logger

// RequestLog 请求日志结构
type RequestLog struct {
	Time         string            `json:"time"`
	Type         string            `json:"type"`
	ClientIP     string            `json:"client_ip"`
	Method       string            `json:"method"`
	Path         string            `json:"path"`
	Query        string            `json:"query,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	RequestBody  string            `json:"request_body,omitempty"`
	StatusCode   int               `json:"status_code"`
	ResponseBody string            `json:"response_body,omitempty"`
	Duration     float64           `json:"duration_ms"`
	UserAgent    string            `json:"user_agent"`
	Error        string            `json:"error,omitempty"`
}

func main() {
	flag.Parse()

	// 初始化日志
	initLogger(*logDir)
	defer logger.Sync()

	targetURL, err := url.Parse(*targetAddr)
	if err != nil {
		logger.Fatal("解析目标地址失败", zap.Error(err))
	}

	logger.Info("🚀 代理服务启动中...")
	logger.Info("📡 监听地址", zap.String("addr", *listenAddr))
	logger.Info("🎯 转发目标", zap.String("target", *targetAddr))
	logger.Info("📁 日志目录", zap.String("dir", *logDir))
	logger.Info("📦 最大 Body 记录", zap.Int("bytes", *maxBodyLogSize))

	// 同时输出到控制台
	fmt.Println("🚀 代理服务启动中...")
	fmt.Printf("📡 监听地址: %s\n", *listenAddr)
	fmt.Printf("🎯 转发目标: %s\n", *targetAddr)
	fmt.Printf("📁 日志目录: %s\n", *logDir)
	fmt.Printf("📦 最大 Body 记录: %d bytes\n", *maxBodyLogSize)

	// 创建反向代理
	proxy := createReverseProxy(targetURL)

	// 创建 HTTP 服务器
	server := &http.Server{
		Addr:         *listenAddr,
		Handler:      proxy,
		ReadTimeout:  0, // SSE 需要长连接，不设超时
		WriteTimeout: 0,
		IdleTimeout:  120 * time.Second,
	}

	logger.Info("✅ 代理服务已启动，等待连接...")
	fmt.Println("✅ 代理服务已启动，等待连接...")

	if err := server.ListenAndServe(); err != nil {
		logger.Fatal("服务器启动失败", zap.Error(err))
	}
}

// initLogger 初始化 zap 日志，按日期切片
func initLogger(logDir string) {
	// 确保日志目录存在
	if err := os.MkdirAll(logDir, 0755); err != nil {
		panic(fmt.Sprintf("创建日志目录失败: %v", err))
	}

	// 日志文件路径
	logFile := filepath.Join(logDir, "proxy.log")

	// 配置 lumberjack 进行日志切片
	lumberJackLogger := &lumberjack.Logger{
		Filename:   logFile,
		MaxSize:    100,  // 每个日志文件最大 100MB
		MaxBackups: 30,   // 保留最近 30 个备份
		MaxAge:     30,   // 保留 30 天
		Compress:   true, // 压缩旧日志
		LocalTime:  true, // 使用本地时间
	}

	// 编码器配置
	encoderConfig := zapcore.EncoderConfig{
		TimeKey:        "time",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		FunctionKey:    zapcore.OmitKey,
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.LowercaseLevelEncoder,
		EncodeTime:     zapcore.TimeEncoderOfLayout("2006-01-02 15:04:05.000"),
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// 文件输出使用 JSON 格式
	fileEncoder := zapcore.NewJSONEncoder(encoderConfig)

	// 控制台输出使用彩色格式
	consoleEncoderConfig := encoderConfig
	consoleEncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderConfig)

	// 多输出：文件 + 控制台
	core := zapcore.NewTee(
		zapcore.NewCore(fileEncoder, zapcore.AddSync(lumberJackLogger), zapcore.InfoLevel),
		zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), zapcore.InfoLevel),
	)

	logger = zap.New(core, zap.AddCaller())
}

// asyncLog 异步记录详细日志
func asyncLog(reqLog RequestLog) {
	go func() {
		logger.Info("📝 请求详情",
			zap.String("time", reqLog.Time),
			zap.String("type", reqLog.Type),
			zap.String("client_ip", reqLog.ClientIP),
			zap.String("method", reqLog.Method),
			zap.String("path", reqLog.Path),
			zap.String("query", reqLog.Query),
			zap.Any("headers", reqLog.Headers),
			zap.String("request_body", reqLog.RequestBody),
			zap.Int("status_code", reqLog.StatusCode),
			zap.String("response_body", reqLog.ResponseBody),
			zap.Float64("duration_ms", reqLog.Duration),
			zap.String("user_agent", reqLog.UserAgent),
			zap.String("error", reqLog.Error),
		)
	}()
}

// createReverseProxy 创建一个支持 SSE、WebSocket 的反向代理
func createReverseProxy(target *url.URL) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()
		clientIP := getClientIP(r)

		// 检测是否为 WebSocket 升级请求
		if isWebSocketRequest(r) {
			logger.Info("🔌 [WebSocket] 升级连接",
				zap.String("client_ip", clientIP),
				zap.String("path", r.URL.Path),
			)
			handleWebSocket(w, r, target)
			return
		}

		// 处理普通 HTTP 请求（包括 SSE）
		handleHTTP(w, r, target, startTime, clientIP)
	})
}

// getClientIP 获取客户端真实 IP
func getClientIP(r *http.Request) string {
	// 优先从 X-Forwarded-For 获取
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}
	// 其次从 X-Real-IP 获取
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	// 最后从连接地址获取
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// isWebSocketRequest 检测是否为 WebSocket 请求
func isWebSocketRequest(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket"
}

// truncateBody 截断过长的 body
func truncateBody(body []byte, maxSize int) string {
	if len(body) <= maxSize {
		return string(body)
	}
	return string(body[:maxSize]) + fmt.Sprintf("... [truncated, total %d bytes]", len(body))
}

// extractHeaders 提取重要的请求头
func extractHeaders(h http.Header) map[string]string {
	headers := make(map[string]string)
	importantHeaders := []string{
		"Content-Type",
		"Authorization",
		"X-API-Key",
		"Accept",
		"Origin",
		"Referer",
	}
	for _, key := range importantHeaders {
		if v := h.Get(key); v != "" {
			// 对敏感信息进行脱敏
			if key == "Authorization" || key == "X-API-Key" {
				if len(v) > 20 {
					headers[key] = v[:10] + "****" + v[len(v)-6:]
				} else {
					headers[key] = "****"
				}
			} else {
				headers[key] = v
			}
		}
	}
	return headers
}

// responseRecorder 响应记录器
type responseRecorder struct {
	http.ResponseWriter
	statusCode   int
	body         *bytes.Buffer
	isSSE        bool
	wroteHeader  bool
}

func newResponseRecorder(w http.ResponseWriter) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           &bytes.Buffer{},
	}
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.statusCode = code
		r.wroteHeader = true
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	// SSE 流不记录完整响应，只记录开头部分
	if !r.isSSE && r.body.Len() < *maxBodyLogSize {
		remaining := *maxBodyLogSize - r.body.Len()
		if len(b) <= remaining {
			r.body.Write(b)
		} else {
			r.body.Write(b[:remaining])
		}
	}
	return r.ResponseWriter.Write(b)
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// handleHTTP 处理普通 HTTP 请求和 SSE
func handleHTTP(w http.ResponseWriter, r *http.Request, target *url.URL, startTime time.Time, clientIP string) {
	// 读取并缓存请求体
	var requestBody []byte
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil {
			requestBody = bodyBytes
			// 重新设置 body 以供后续读取
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}
	}

	// 创建响应记录器
	recorder := newResponseRecorder(w)

	// 创建自定义 Transport，支持流式传输
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    true, // 禁用压缩以支持流式传输
	}

	var proxyErr error

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			// 保留原始路径和查询参数
			if target.Path != "" && target.Path != "/" {
				req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			}

			// 移除可能导致问题的 header
			req.Header.Del("Accept-Encoding")
		},
		Transport: transport,
		ModifyResponse: func(resp *http.Response) error {
			// 检测 SSE 响应
			contentType := resp.Header.Get("Content-Type")
			if strings.Contains(contentType, "text/event-stream") {
				recorder.isSSE = true
				logger.Info("📺 [SSE] 检测到流式响应",
					zap.String("client_ip", clientIP),
					zap.String("path", r.URL.Path),
				)
				// 移除可能干扰流式传输的 header
				resp.Header.Del("Content-Length")
			}

			// 对于非 SSE 响应，尝试读取并记录响应体
			if !recorder.isSSE && resp.Body != nil {
				respBody, err := io.ReadAll(resp.Body)
				if err == nil {
					// 处理 gzip 压缩的响应
					if resp.Header.Get("Content-Encoding") == "gzip" {
						if reader, err := gzip.NewReader(bytes.NewReader(respBody)); err == nil {
							decompressed, _ := io.ReadAll(reader)
							reader.Close()
							recorder.body.Write(decompressed)
						}
					} else {
						if len(respBody) <= *maxBodyLogSize {
							recorder.body.Write(respBody)
						} else {
							recorder.body.Write(respBody[:*maxBodyLogSize])
						}
					}
					// 重新设置响应体
					resp.Body = io.NopCloser(bytes.NewReader(respBody))
				}
			}

			return nil
		},
		FlushInterval: -1, // 立即 flush，对 SSE 至关重要
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyErr = err
			logger.Error("❌ 代理请求失败",
				zap.String("client_ip", clientIP),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.Error(err),
			)
			http.Error(w, "代理请求失败", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(recorder, r)

	// 计算耗时
	duration := time.Since(startTime)

	// 构建请求日志
	reqType := "HTTP"
	if recorder.isSSE {
		reqType = "SSE"
	}

	reqLog := RequestLog{
		Time:        startTime.Format("2006-01-02 15:04:05.000"),
		Type:        reqType,
		ClientIP:    clientIP,
		Method:      r.Method,
		Path:        r.URL.Path,
		Query:       r.URL.RawQuery,
		Headers:     extractHeaders(r.Header),
		RequestBody: truncateBody(requestBody, *maxBodyLogSize),
		StatusCode:  recorder.statusCode,
		Duration:    float64(duration.Milliseconds()),
		UserAgent:   r.UserAgent(),
	}

	// SSE 响应不记录完整 body
	if recorder.isSSE {
		reqLog.ResponseBody = "[SSE Stream]"
	} else {
		reqLog.ResponseBody = truncateBody(recorder.body.Bytes(), *maxBodyLogSize)
	}

	if proxyErr != nil {
		reqLog.Error = proxyErr.Error()
	}

	// 异步记录日志
	asyncLog(reqLog)

	// 控制台简要输出
	fmt.Printf("➡️ [%s] %s %s %s -> %d (%dms)\n",
		reqType, clientIP, r.Method, r.URL.Path, recorder.statusCode, duration.Milliseconds())
}

// handleWebSocket 处理 WebSocket 连接
func handleWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL) {
	clientIP := getClientIP(r)
	startTime := time.Now()

	// 构建目标地址
	targetHost := target.Host

	// 连接到目标 WebSocket 服务器
	targetConn, err := net.DialTimeout("tcp", targetHost, 30*time.Second)
	if err != nil {
		logger.Error("❌ [WebSocket] 连接目标失败",
			zap.String("client_ip", clientIP),
			zap.String("target", targetHost),
			zap.Error(err),
		)
		http.Error(w, "无法连接到目标服务器", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	// 劫持客户端连接
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		logger.Error("❌ [WebSocket] 不支持 Hijacker", zap.String("client_ip", clientIP))
		http.Error(w, "不支持 WebSocket", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		logger.Error("❌ [WebSocket] Hijack 失败",
			zap.String("client_ip", clientIP),
			zap.Error(err),
		)
		return
	}
	defer clientConn.Close()

	// 构建并发送升级请求到目标服务器
	upgradeReq := buildUpgradeRequest(r, target)
	if _, err := targetConn.Write([]byte(upgradeReq)); err != nil {
		logger.Error("❌ [WebSocket] 发送升级请求失败",
			zap.String("client_ip", clientIP),
			zap.Error(err),
		)
		return
	}

	logger.Info("✅ [WebSocket] 连接已建立",
		zap.String("client_ip", clientIP),
		zap.String("path", r.URL.Path),
		zap.Any("headers", extractHeaders(r.Header)),
	)

	// 双向转发数据
	done := make(chan struct{}, 2)

	go func() {
		io.Copy(targetConn, clientConn)
		done <- struct{}{}
	}()

	go func() {
		io.Copy(clientConn, targetConn)
		done <- struct{}{}
	}()

	<-done

	duration := time.Since(startTime)

	// 异步记录 WebSocket 日志
	go func() {
		logger.Info("🔌 [WebSocket] 连接已关闭",
			zap.String("client_ip", clientIP),
			zap.String("path", r.URL.Path),
			zap.Float64("duration_ms", float64(duration.Milliseconds())),
		)
	}()

	fmt.Printf("🔌 [WebSocket] %s %s closed (%dms)\n", clientIP, r.URL.Path, duration.Milliseconds())
}

// buildUpgradeRequest 构建 WebSocket 升级请求
func buildUpgradeRequest(r *http.Request, target *url.URL) string {
	var builder strings.Builder

	path := r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}

	builder.WriteString(r.Method + " " + path + " HTTP/1.1\r\n")
	builder.WriteString("Host: " + target.Host + "\r\n")

	// 复制重要的 header
	for key, values := range r.Header {
		for _, value := range values {
			builder.WriteString(key + ": " + value + "\r\n")
		}
	}

	builder.WriteString("\r\n")
	return builder.String()
}

// singleJoiningSlash 合并路径
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
