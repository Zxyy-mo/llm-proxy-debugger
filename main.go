package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
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
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
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

// Session 会话记录
type Session struct {
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	Logs      []RequestLog `json:"logs"`
}

// Rule 动态干预规则
type Rule struct {
	ID           string `json:"id"`
	PathMatch    string `json:"path_match"`
	BodyMatch    string `json:"body_match"`
	InjectSystem string `json:"inject_system"`
	Intercept    bool   `json:"intercept"`
}

// GlobalStore 全局存储
type GlobalStore struct {
	sync.RWMutex
	Sessions map[string]*Session
	Rules    []Rule
}

var store = &GlobalStore{
	Sessions: make(map[string]*Session),
	Rules: []Rule{
		{ID: "default-compact", PathMatch: "/messages", BodyMatch: "", InjectSystem: "[System Interjection] Be concise and direct.", Intercept: false},
	},
}

// Hub WebSocket 中心
type Hub struct {
	clients    map[*websocket.Conn]bool
	broadcast  chan interface{}
	register   chan *websocket.Conn
	unregister chan *websocket.Conn
	mu         sync.Mutex
}

var hub = &Hub{
	clients:    make(map[*websocket.Conn]bool),
	broadcast:  make(chan interface{}),
	register:   make(chan *websocket.Conn),
	unregister: make(chan *websocket.Conn),
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				client.Close()
			}
			h.mu.Unlock()
		case message := <-h.broadcast:
			h.mu.Lock()
			for client := range h.clients {
				err := client.WriteJSON(message)
				if err != nil {
					client.Close()
					delete(h.clients, client)
				}
			}
			h.mu.Unlock()
		}
	}
}

// RequestLog 请求日志结构
type RequestLog struct {
	Time           string            `json:"time"`
	TraceID        string            `json:"trace_id,omitempty"`
	SessionID      string            `json:"session_id,omitempty"`
	Type           string            `json:"type"`
	ClientIP       string            `json:"client_ip"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Query          string            `json:"query,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	RequestBody    string            `json:"request_body,omitempty"`
	StatusCode     int               `json:"status_code"`
	ResponseBody   string            `json:"response_body,omitempty"`
	Duration       float64           `json:"duration_ms"`
	UserAgent      string            `json:"user_agent"`
	Error          string            `json:"error,omitempty"`
	InputTokens    int               `json:"input_tokens,omitempty"`
	OutputTokens   int               `json:"output_tokens,omitempty"`
	ThinkingTokens int               `json:"thinking_tokens,omitempty"`
	ToolUseCount   int               `json:"tool_use_count,omitempty"`
	IsThinkingLoop bool              `json:"is_thinking_loop,omitempty"`
}

// SSEEventAccumulator Anthropic SSE 实时累加器
type SSEEventAccumulator struct {
	TraceID        string `json:"trace_id"`
	InputTokens    int    `json:"input_tokens"`
	OutputTokens   int    `json:"output_tokens"`
	ThinkingTokens int    `json:"thinking_tokens"`
	TextTokens     int    `json:"text_tokens"`
	ToolUseCount   int    `json:"tool_use_count"`
	LastEventType  string `json:"last_event_type"`
	StartTime      time.Time
	IsThinkingLoop bool
	thinkingStreak int
}

func NewSSEAccumulator(traceID string) *SSEEventAccumulator {
	return &SSEEventAccumulator{
		TraceID:   traceID,
		StartTime: time.Now(),
	}
}

func (acc *SSEEventAccumulator) Accumulate(data []byte) {
	if len(data) == 0 {
		return
	}
	jsonStr := string(data)
	eventType := gjson.Get(jsonStr, "type").String()
	acc.LastEventType = eventType

	switch eventType {
	case "message_start":
		acc.InputTokens = int(gjson.Get(jsonStr, "message.usage.input_tokens").Int())
	case "thinking_delta":
		delta := len([]rune(gjson.Get(jsonStr, "thinking").String()))
		acc.ThinkingTokens += delta
		acc.OutputTokens += delta
		acc.thinkingStreak++
		if acc.thinkingStreak > 15 {
			acc.IsThinkingLoop = true
		}
	case "content_block_delta":
		if gjson.Get(jsonStr, "delta.type").String() == "text" {
			delta := len([]rune(gjson.Get(jsonStr, "delta.text").String()))
			acc.TextTokens += delta
			acc.OutputTokens += delta
			acc.thinkingStreak = 0
		}
	case "content_block_start":
		if gjson.Get(jsonStr, "content_block.type").String() == "tool_use" {
			acc.ToolUseCount++
			acc.thinkingStreak = 0
		}
	case "message_delta":
		if usage := gjson.Get(jsonStr, "usage"); usage.Exists() {
			acc.OutputTokens = int(usage.Get("output_tokens").Int())
		}
	}
}

// serveWS 处理 WebSocket 升级
func serveWS(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.Error("WebSocket upgrade failed", zap.Error(err))
		return
	}
	hub.register <- conn
}

// apiSessionsHandler 返回所有会话
func apiSessionsHandler(w http.ResponseWriter, r *http.Request) {
	store.RLock()
	defer store.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.Sessions)
}

// apiRulesHandler 管理规则
func apiRulesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		store.RLock()
		json.NewEncoder(w).Encode(store.Rules)
		store.RUnlock()
	} else if r.Method == http.MethodPost {
		var rule Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err == nil {
			store.Lock()
			rule.ID = uuid.New().String()
			store.Rules = append(store.Rules, rule)
			store.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(rule)
		}
	}
}

func main() {
	flag.Parse()

	// 初始化日志
	initLogger(*logDir)
	defer logger.Sync()

	// 启动 WebSocket Hub
	go hub.Run()

	targetURL, err := url.Parse(*targetAddr)
	if err != nil {
		logger.Fatal("解析目标地址失败", zap.Error(err))
	}

	logger.Info("🚀 代理服务启动中...")
	logger.Info("📡 监听地址", zap.String("addr", *listenAddr))
	logger.Info("🎯 转发目标", zap.String("target", *targetAddr))

	// 创建代理
	proxy := createReverseProxy(targetURL)

	// 设置路由
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
	statusCode  int
	body        *bytes.Buffer // 非 SSE 时用
	isSSE       bool
	wroteHeader bool
	accumulator *SSEEventAccumulator // 实时累加器
	sseBuffer   []byte               // 临时缓存当前 event data
}

func newResponseRecorder(w http.ResponseWriter, traceID string) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           &bytes.Buffer{},
		accumulator:    NewSSEAccumulator(traceID),
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
	if r.isSSE {
		// SSE 实时解析逻辑
		r.sseBuffer = append(r.sseBuffer, b...)

		for {
			idx := bytes.Index(r.sseBuffer, []byte("\n\n"))
			if idx == -1 {
				break
			}
			event := r.sseBuffer[:idx]
			r.sseBuffer = r.sseBuffer[idx+2:]

			// 提取 data: 后面的 JSON
			if bytes.HasPrefix(event, []byte("data: ")) {
				data := bytes.TrimPrefix(event, []byte("data: "))
				data = bytes.TrimSpace(data)
				if len(data) > 0 && data[0] == '{' {
					r.accumulator.Accumulate(data)

					// 思考循环检测告警
					if r.accumulator.IsThinkingLoop && r.accumulator.thinkingStreak%10 == 0 {
						logger.Warn("⚠️ [Thinking Loop Detected]",
							zap.String("trace_id", r.accumulator.TraceID),
							zap.Int("thinking_tokens", r.accumulator.ThinkingTokens))
					}
				}
			}
		}
		// 原样转发
		return r.ResponseWriter.Write(b)
	}

	// 非 SSE 走原来逻辑
	if r.body.Len() < *maxBodyLogSize {
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

// asyncLog 异步记录详细日志
func asyncLog(reqLog RequestLog) {
	go func() {
		logger.Info("📝 请求详情",
			zap.String("trace_id", reqLog.TraceID),
			zap.String("type", reqLog.Type),
			zap.String("method", reqLog.Method),
			zap.String("path", reqLog.Path),
			zap.Int("status_code", reqLog.StatusCode),
			zap.Int("input", reqLog.InputTokens),
			zap.Int("output", reqLog.OutputTokens),
			zap.Int("thinking", reqLog.ThinkingTokens),
			zap.Int("tools", reqLog.ToolUseCount),
			zap.Float64("duration_ms", reqLog.Duration),
			zap.Bool("loop", reqLog.IsThinkingLoop),
			zap.String("error", reqLog.Error),
		)
	}()
}

// handleHTTP 处理普通 HTTP 请求和 SSE
func handleHTTP(w http.ResponseWriter, r *http.Request, target *url.URL, startTime time.Time, clientIP string) {
	traceID := uuid.New().String()
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = "default"
	}

	// 确保会话存在
	store.Lock()
	if _, ok := store.Sessions[sessionID]; !ok {
		store.Sessions[sessionID] = &Session{
			ID:        sessionID,
			CreatedAt: time.Now(),
		}
	}
	store.Unlock()

	// 广播请求开始事件
	hub.broadcast <- map[string]interface{}{
		"event":      "request_start",
		"trace_id":   traceID,
		"session_id": sessionID,
		"method":     r.Method,
		"path":       r.URL.Path,
		"time":       startTime.Format(time.RFC3339),
	}

	// 读取并缓存请求体
	var requestBody []byte
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil {
			requestBody = bodyBytes
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}
	}

	recorder := newResponseRecorder(w, traceID)

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
		DisableCompression:    true,
	}

	var proxyErr error

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			if target.Path != "" && target.Path != "/" {
				req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			}

			req.Header.Del("Accept-Encoding")

			// 动态规则匹配
			store.RLock()
			rules := store.Rules
			store.RUnlock()

			for _, rule := range rules {
				if strings.Contains(req.URL.Path, rule.PathMatch) {
					// 如果有 body 匹配要求
					if rule.BodyMatch != "" && !strings.Contains(string(requestBody), rule.BodyMatch) {
						continue
					}

					// 执行注入
					if req.Body != nil && rule.InjectSystem != "" {
						var payload map[string]any
						if err := json.Unmarshal(requestBody, &payload); err == nil {
							if sys, ok := payload["system"].(string); ok {
								payload["system"] = sys + "\n\n" + rule.InjectSystem
							} else {
								payload["system"] = rule.InjectSystem
							}
							newBody, _ := json.Marshal(payload)
							req.Body = io.NopCloser(bytes.NewBuffer(newBody))
							req.ContentLength = int64(len(newBody))
							req.Header.Set("Content-Length", fmt.Sprint(len(newBody)))
						}
					}
				}
			}
		},
		Transport: transport,
		ModifyResponse: func(resp *http.Response) error {
			contentType := resp.Header.Get("Content-Type")
			if strings.Contains(contentType, "text/event-stream") {
				recorder.isSSE = true
				resp.Header.Del("Content-Length")
			}

			if !recorder.isSSE && resp.Body != nil {
				respBody, err := io.ReadAll(resp.Body)
				if err == nil {
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
					resp.Body = io.NopCloser(bytes.NewReader(respBody))
				}
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyErr = err
			logger.Error("❌ 代理请求失败", zap.String("trace_id", traceID), zap.Error(err))
			http.Error(w, "代理请求失败", http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(recorder, r)

	duration := time.Since(startTime)
	reqType := "HTTP"
	if recorder.isSSE {
		reqType = "SSE"
	}

	reqLog := RequestLog{
		Time:           startTime.Format("2006-01-02 15:04:05.000"),
		TraceID:        traceID,
		SessionID:      sessionID,
		Type:           reqType,
		ClientIP:       clientIP,
		Method:         r.Method,
		Path:           r.URL.Path,
		Query:          r.URL.RawQuery,
		Headers:        extractHeaders(r.Header),
		RequestBody:    truncateBody(requestBody, *maxBodyLogSize),
		StatusCode:     recorder.statusCode,
		Duration:       float64(duration.Milliseconds()),
		UserAgent:      r.UserAgent(),
		InputTokens:    recorder.accumulator.InputTokens,
		OutputTokens:   recorder.accumulator.OutputTokens,
		ThinkingTokens: recorder.accumulator.ThinkingTokens,
		ToolUseCount:   recorder.accumulator.ToolUseCount,
		IsThinkingLoop: recorder.accumulator.IsThinkingLoop,
	}

	if recorder.isSSE {
		reqLog.ResponseBody = "[SSE Stream]"
	} else {
		reqLog.ResponseBody = truncateBody(recorder.body.Bytes(), *maxBodyLogSize)
	}

	if proxyErr != nil {
		reqLog.Error = proxyErr.Error()
	}

	// 存入会话
	store.Lock()
	if s, ok := store.Sessions[sessionID]; ok {
		s.Logs = append(s.Logs, reqLog)
	}
	store.Unlock()

	// 广播请求结束事件
	hub.broadcast <- map[string]interface{}{
		"event":    "request_end",
		"trace_id": traceID,
		"log":      reqLog,
	}

	asyncLog(reqLog)
}

// handleWebSocket 处理 WebSocket 连接
func handleWebSocket(w http.ResponseWriter, r *http.Request, target *url.URL) {
	clientIP := getClientIP(r)
	startTime := time.Now()
	targetHost := target.Host

	targetConn, err := net.DialTimeout("tcp", targetHost, 30*time.Second)
	if err != nil {
		logger.Error("❌ [WebSocket] 连接目标失败", zap.Error(err))
		http.Error(w, "无法连接到目标服务器", http.StatusBadGateway)
		return
	}
	defer targetConn.Close()

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持 WebSocket", http.StatusInternalServerError)
		return
	}

	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer clientConn.Close()

	upgradeReq := buildUpgradeRequest(r, target)
	targetConn.Write([]byte(upgradeReq))

	done := make(chan struct{}, 2)
	go func() { io.Copy(targetConn, clientConn); done <- struct{}{} }()
	go func() { io.Copy(clientConn, targetConn); done <- struct{}{} }()
	<-done

	duration := time.Since(startTime)
	fmt.Printf("🔌 [WebSocket] %s closed (%dms)\n", clientIP, duration.Milliseconds())
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
