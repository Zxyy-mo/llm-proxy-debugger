package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/config"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"go.uber.org/zap"
)

// Server 是反向代理的核心，实现 http.Handler。
type Server struct {
	target    *url.URL
	transport *http.Transport
	hub       *hub.Hub
	store     *store.Store
	logger    *zap.Logger
	cfg       *config.Config
}

// NewServer 解析目标地址并初始化 Server。
func NewServer(cfg *config.Config, log *zap.Logger, h *hub.Hub, s *store.Store) (*Server, error) {
	targetURL, err := url.Parse(cfg.TargetAddr)
	if err != nil {
		return nil, fmt.Errorf("解析目标地址失败: %w", err)
	}
	return &Server{
		target:    targetURL,
		transport: newTransport(),
		hub:       h,
		store:     s,
		logger:    log,
		cfg:       cfg,
	}, nil
}

// ServeHTTP 实现 http.Handler 接口，区分 WebSocket 和普通/SSE 请求。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	clientIP := getClientIP(r)

	if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" {
		s.logger.Info("🔌 [WebSocket] 升级连接",
			zap.String("client_ip", clientIP),
			zap.String("path", r.URL.Path),
		)
		s.handleWebSocket(w, r)
		return
	}

	s.handleHTTP(w, r, startTime, clientIP)
}

func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request, startTime time.Time, clientIP string) {
	traceID := uuid.New().String()
	sessionID := r.Header.Get("X-Session-ID")
	if sessionID == "" {
		sessionID = "default"
	}

	// 协议选择
	var handler protocol.Handler = &protocol.AnthropicHandler{}
	if strings.Contains(r.URL.Path, "openai") || strings.Contains(r.URL.Path, "chat/completions") {
		handler = &protocol.OpenAIHandler{}
	}

	// 确保会话存在
	s.store.Lock()
	if _, ok := s.store.Sessions[sessionID]; !ok {
		s.store.Sessions[sessionID] = &store.Session{
			ID:        sessionID,
			CreatedAt: time.Now(),
		}
	}
	s.store.Unlock()

	// 广播请求开始事件
	s.hub.Broadcast <- map[string]interface{}{
		"event":      "request_start",
		"trace_id":   traceID,
		"session_id": sessionID,
		"method":     r.Method,
		"path":       r.URL.Path,
		"time":       startTime.Format(time.RFC3339),
	}

	// 读取并缓存请求体（以便规则匹配和日志）
	var requestBody []byte
	if r.Body != nil {
		bodyBytes, err := io.ReadAll(r.Body)
		if err == nil {
			requestBody = bodyBytes
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}
	}

	recorder := newResponseRecorder(w, traceID, handler, s.hub, s.logger, s.cfg.MaxBodyLogSize, filepath.Join(s.cfg.LogDir, "sse"))
	defer recorder.close()
	target := s.target
	var proxyErr error

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			if target.Path != "" && target.Path != "/" {
				req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			}

			req.Header.Del("Accept-Encoding")

			// 动态规则注入
			s.store.RLock()
			rules := s.store.Rules
			s.store.RUnlock()

			for _, rule := range rules {
				if !strings.Contains(req.URL.Path, rule.PathMatch) {
					continue
				}
				if rule.BodyMatch != "" && !strings.Contains(string(requestBody), rule.BodyMatch) {
					continue
				}
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
		},
		Transport: s.transport,
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
						if len(respBody) <= s.cfg.MaxBodyLogSize {
							recorder.body.Write(respBody)
						} else {
							recorder.body.Write(respBody[:s.cfg.MaxBodyLogSize])
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
			s.logger.Error("❌ 代理请求失败", zap.String("trace_id", traceID), zap.Error(err))
			http.Error(w, "代理请求失败", http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(recorder, r)

	duration := time.Since(startTime)
	reqType := "HTTP"
	if recorder.isSSE {
		reqType = "SSE"
	}

	reqLog := store.RequestLog{
		Time:            startTime.Format("2006-01-02 15:04:05.000"),
		TraceID:         traceID,
		SessionID:       sessionID,
		Type:            reqType,
		ClientIP:        clientIP,
		Method:          r.Method,
		Path:            r.URL.Path,
		Query:           r.URL.RawQuery,
		Headers:         extractHeaders(r.Header),
		RequestBody:     truncateBody(requestBody, s.cfg.MaxBodyLogSize),
		StatusCode:      recorder.statusCode,
		Duration:        float64(duration.Milliseconds()),
		UserAgent:       r.UserAgent(),
		InputTokens:     recorder.accumulator.InputTokens,
		OutputTokens:    recorder.accumulator.OutputTokens,
		ThinkingTokens:  recorder.accumulator.ThinkingTokens,
		ThinkingContent: recorder.accumulator.ThinkingContent,
		ToolUseCount:    recorder.accumulator.ToolUseCount,
		IsThinkingLoop:  recorder.accumulator.IsThinkingLoop,
	}

	if recorder.isSSE {
		reqLog.ResponseBody = truncateBody([]byte(recorder.accumulator.OutputContent), s.cfg.MaxBodyLogSize)
	} else {
		reqLog.ResponseBody = truncateBody(recorder.body.Bytes(), s.cfg.MaxBodyLogSize)
	}

	if proxyErr != nil {
		reqLog.Error = proxyErr.Error()
	}

	// 存入会话
	s.store.Lock()
	if sess, ok := s.store.Sessions[sessionID]; ok {
		sess.Logs = append(sess.Logs, reqLog)
	}
	s.store.Unlock()

	// 广播请求结束事件
	s.hub.Broadcast <- map[string]interface{}{
		"event":    "request_end",
		"trace_id": traceID,
		"log":      reqLog,
	}

	s.asyncLog(reqLog)
}

func (s *Server) asyncLog(reqLog store.RequestLog) {
	go func() {
		s.logger.Info("📝 请求详情",
			zap.String("trace_id", reqLog.TraceID),
			zap.String("type", reqLog.Type),
			zap.String("method", reqLog.Method),
			zap.String("path", reqLog.Path),
			zap.Int("status_code", reqLog.StatusCode),
			zap.Int("input", reqLog.InputTokens),
			zap.Int("output", reqLog.OutputTokens),
			zap.Int("thinking", reqLog.ThinkingTokens),
			zap.String("thinking_content", truncateBody([]byte(reqLog.ThinkingContent), 500)),
			zap.Int("tools", reqLog.ToolUseCount),
			zap.Float64("duration_ms", reqLog.Duration),
			zap.Bool("loop", reqLog.IsThinkingLoop),
			zap.String("error", reqLog.Error),
		)
	}()
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)
	startTime := time.Now()
	targetHost := s.target.Host

	targetConn, err := net.DialTimeout("tcp", targetHost, 30*time.Second)
	if err != nil {
		s.logger.Error("❌ [WebSocket] 连接目标失败", zap.Error(err))
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

	upgradeReq := buildUpgradeRequest(r, s.target)
	targetConn.Write([]byte(upgradeReq)) //nolint:errcheck

	done := make(chan struct{}, 2)
	go func() { io.Copy(targetConn, clientConn); done <- struct{}{} }() //nolint:errcheck
	go func() { io.Copy(clientConn, targetConn); done <- struct{}{} }() //nolint:errcheck
	<-done

	duration := time.Since(startTime)
	fmt.Printf("🔌 [WebSocket] %s closed (%dms)\n", clientIP, duration.Milliseconds())
}

// buildUpgradeRequest 构建发往上游的 WebSocket 升级请求报文
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
