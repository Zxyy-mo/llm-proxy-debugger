package main

import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

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
	ThinkingContent string           `json:"thinking_content,omitempty"` // 新增
	ToolUseCount   int               `json:"tool_use_count,omitempty"`
	IsThinkingLoop bool              `json:"is_thinking_loop,omitempty"`
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
	sseParser   *SSEParser
}

func newResponseRecorder(w http.ResponseWriter, traceID string, handler ProtocolHandler) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           &bytes.Buffer{},
		accumulator:    NewSSEAccumulator(traceID, handler),
		sseParser:      NewSSEParser(),
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
		events := r.sseParser.Feed(b)
		for _, evt := range events {
			if evt.Data == "" {
				continue
			}

			// SSE 的 data 通常是单行 JSON；这里对多行 data 也按原样尝试解析。
			r.accumulator.Accumulate([]byte(evt.Data))

			// 广播实时事件到 WebSocket
			hub.broadcast <- map[string]interface{}{
				"event":      "sse_delta",
				"trace_id":   r.accumulator.TraceID,
				"data":       evt.Data,
				"sse_event":  evt.Event,
				"sse_id":     evt.ID,
				"sse_retry":  evt.RetryMS,
				"sse_fields": evt.Fields,
				"metrics":    r.accumulator,
			}

			// 思考循环检测告警
			if r.accumulator.IsThinkingLoop {
				logger.Warn("⚠️ [Thinking Loop Detected]",
					zap.String("trace_id", r.accumulator.TraceID),
					zap.Int("thinking_tokens", r.accumulator.ThinkingTokens))
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
			zap.String("thinking_content", truncateBody([]byte(reqLog.ThinkingContent), 500)), // 记录部分推理内容
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

	// 协议选择 (SRP)
	var handler ProtocolHandler = &AnthropicHandler{}
	if strings.Contains(r.URL.Path, "openai") || strings.Contains(r.URL.Path, "chat/completions") {
		handler = &OpenAIHandler{}
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

	// 广播开始事件
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

	recorder := newResponseRecorder(w, traceID, handler)

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

			// 获取动态规则和模型路由映射
			store.RLock()
			rules := store.Rules
			routes := store.Routes
			store.RUnlock()

			// [新增] 动态路由解析逻辑：通过 body 里的 model 进行目标覆盖
			if len(requestBody) > 0 && req.Method == http.MethodPost {
				var payload map[string]interface{}
				// 如果不能解析或者没有匹配到，使用原有的默认 target (由 flag 提供)
				if err := json.Unmarshal(requestBody, &payload); err == nil {
					if modelName, ok := payload["model"].(string); ok {
						if routeTargetStr, exists := routes[modelName]; exists {
							if routeTarget, err := url.Parse(routeTargetStr); err == nil {
								// 使用匹配到的自定义后端覆盖代理请求
								req.URL.Scheme = routeTarget.Scheme
								req.URL.Host = routeTarget.Host
								req.Host = routeTarget.Host
							}
						}
					}
				}
			}

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
		InputTokens:     recorder.accumulator.InputTokens,
		OutputTokens:    recorder.accumulator.OutputTokens,
		ThinkingTokens:  recorder.accumulator.ThinkingTokens,
		ThinkingContent: recorder.accumulator.ThinkingContent, // 新增
		ToolUseCount:    recorder.accumulator.ToolUseCount,
		IsThinkingLoop:  recorder.accumulator.IsThinkingLoop,
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
