package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/config"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/export"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/replay"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Server 是反向代理的核心，实现 http.Handler。
type Server struct {
	target        *url.URL
	transport     *http.Transport
	hub           *hub.Hub
	store         *store.Store
	logger        *zap.Logger
	cfg           *config.Config
	interceptions *intercept.Manager
	replays       *replay.Manager
}

// NewServer 解析目标地址并初始化 Server。
func NewServer(cfg *config.Config, log *zap.Logger, h *hub.Hub, s *store.Store) (*Server, error) {
	if cfg.MaxBodyLogSize < 0 {
		return nil, fmt.Errorf("maxbody must be non-negative")
	}
	targetURL, err := url.Parse(cfg.TargetAddr)
	if err != nil {
		return nil, fmt.Errorf("解析目标地址失败: %w", err)
	}
	return &Server{
		target:        targetURL,
		transport:     newTransport(),
		hub:           h,
		store:         s,
		logger:        log,
		cfg:           cfg,
		interceptions: intercept.New(func() { h.Publish(map[string]any{"event": "interceptions_updated"}) }),
		replays:       replay.New(func() { h.Publish(map[string]any{"event": "replays_updated"}) }),
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
	replayOpts := replayFrom(r.Context())
	traceID := uuid.New().String()
	var replayInfo *store.ReplayInfo
	if replayOpts != nil {
		traceID = replayOpts.TraceID
		info := replayOpts.Info
		replayInfo = &info
	}
	var requestBody []byte
	var requestReadErr error
	if r.Body != nil {
		requestBody, requestReadErr = io.ReadAll(r.Body)
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(requestBody))
	}
	input := correlation.ExtractRequest(r.Header, r.URL.Path, requestBody, s.target.String())
	var handler protocol.Handler = &protocol.AnthropicHandler{}
	switch input.Protocol {
	case "openai":
		handler = &protocol.OpenAIHandler{}
	case "responses":
		handler = &protocol.ResponsesHandler{}
	}
	initial := s.store.Begin(store.RequestLog{
		Time: startTime.Format(time.RFC3339Nano), TraceID: traceID,
		Type: "HTTP", ClientIP: clientIP, Method: r.Method, Path: r.URL.Path,
		Query: export.RedactQuery(r.URL.RawQuery), Headers: extractHeaders(r.Header),
		RequestBody: truncateBody(requestBody, s.cfg.MaxBodyLogSize), UserAgent: r.UserAgent(),
		Replay: replayInfo,
	}, input)
	s.store.CaptureOriginal(traceID, snapshotRequest(r, requestBody))
	if replayOpts != nil {
		replayOpts.markStarted()
	}
	w.Header().Set("X-Gateway-Trace-ID", traceID)
	s.hub.Publish(map[string]interface{}{
		"event": "request_start", "trace_id": traceID, "session_id": initial.SessionID,
		"method": r.Method, "path": r.URL.Path, "time": initial.Time, "log": initial,
	})

	recorder := newResponseRecorder(w, traceID, handler, s.hub, s.logger, s.cfg.MaxBodyLogSize, filepath.Join(s.cfg.LogDir, "sse"))
	defer recorder.close()
	recorder.onResponse = func(response correlation.Response) {
		if s.store.ObserveResponse(traceID, response) {
			s.hub.Publish(map[string]interface{}{"event": "sessions_updated"})
		}
	}
	target := s.target
	var proxyErr error
	var upstreamStarted time.Time
	defer func() {
		panicValue := recover()
		if panicValue == http.ErrAbortHandler {
			proxyErr = fmt.Errorf("response stream interrupted")
		} else if panicValue != nil {
			panic(panicValue)
		}
		response := recorder.capture.Response()
		if proxyErr == nil && response.Error != "" {
			proxyErr = fmt.Errorf("upstream: %s", response.Error)
		}
		if proxyErr == nil && recorder.isSSE && response.Recognized && !response.Complete {
			proxyErr = fmt.Errorf("SSE stream ended without a completion event")
		}
		log := initial
		log.StatusCode = recorder.statusCode
		log.Duration = float64(time.Since(startTime).Microseconds()) / 1000
		if !upstreamStarted.IsZero() {
			log.UpstreamDuration = float64(time.Since(upstreamStarted).Microseconds()) / 1000
		}
		if status, err := s.interrupted(r, traceID); err != nil {
			proxyErr = err
			log.StatusCode = status
		}
		log.InputTokens, log.OutputTokens = recorder.accumulator.InputTokens, recorder.accumulator.OutputTokens
		log.ThinkingTokens, log.ThinkingContent = recorder.accumulator.ThinkingTokens, recorder.accumulator.ThinkingContent
		log.ToolUseCount, log.IsThinkingLoop = recorder.accumulator.ToolUseCount, recorder.accumulator.IsThinkingLoop
		if recorder.isSSE {
			log.Type = "SSE"
			log.ResponseBody = truncateBody([]byte(recorder.accumulator.OutputContent), s.cfg.MaxBodyLogSize)
		} else {
			log.ResponseBody = truncateBody(recorder.body.Bytes(), s.cfg.MaxBodyLogSize)
		}
		if proxyErr != nil {
			log.Error = proxyErr.Error()
		}
		log = s.store.Complete(traceID, log, response)
		s.hub.Publish(map[string]interface{}{"event": "request_end", "trace_id": traceID, "log": log})
		s.hub.Publish(map[string]interface{}{"event": "sessions_updated"})
		s.asyncLog(log)
		if panicValue != nil {
			panic(panicValue)
		}
	}()
	if requestReadErr != nil {
		proxyErr = requestReadErr
		http.Error(recorder, "读取请求体失败", http.StatusBadRequest)
		return
	}

	outgoingBody, breakpoint := requestBody, (*store.Rule)(nil)
	if replayOpts == nil || !replayOpts.SkipRules {
		// Outgoing snapshots already contain rule injections; replaying them must
		// not inject or pause a second time.
		outgoingBody, breakpoint = s.prepareRequest(r.URL.Path, requestBody)
	}
	editBlockedReason := ""
	if encoding := r.Header.Get("Content-Encoding"); (encoding != "" && !strings.EqualFold(encoding, "identity")) || !utf8.Valid(requestBody) {
		editBlockedReason = "压缩或二进制请求只支持原样放行或取消；完整字节可在原始 / 出站视图中以 Base64 查看。"
		outgoingBody = requestBody
	}
	if breakpoint != nil {
		originalHeaders := intercept.EditableHeaders(r.Header)
		pendingBody, pendingOriginal, bodyEncoding := string(outgoingBody), string(requestBody), ""
		if editBlockedReason != "" {
			pendingBody = base64.StdEncoding.EncodeToString(outgoingBody)
			pendingOriginal = base64.StdEncoding.EncodeToString(requestBody)
			bodyEncoding = "base64"
		}
		pending := s.interceptions.Add(r.Context(), intercept.Detail{
			Summary: intercept.Summary{
				Metadata: intercept.Metadata{RuleID: breakpoint.ID, TimeoutAction: breakpoint.TimeoutAction},
				TraceID:  traceID, Method: r.Method, Path: r.URL.Path, Model: input.Model,
			},
			Body: pendingBody, Headers: originalHeaders,
			OriginalBody: pendingOriginal, OriginalHeaders: originalHeaders,
			EditBlockedReason: editBlockedReason,
			BodyEncoding:      bodyEncoding,
		}, time.Duration(breakpoint.WaitSeconds)*time.Second)
		s.publishLog(s.store.SetInterception(traceID, pending.Metadata))
		decision := s.interceptions.Wait(r.Context(), traceID)
		s.publishLog(s.store.SetInterception(traceID, decision.Metadata))
		if decision.State == "canceled" {
			proxyErr = fmt.Errorf("request canceled: %s", decision.Reason)
			status := http.StatusConflict
			if decision.Reason == "timeout" {
				status = http.StatusGatewayTimeout
			}
			if interruptedStatus, err := s.interrupted(r, traceID); err != nil {
				proxyErr, recorder.statusCode = err, interruptedStatus
			} else {
				http.Error(recorder, proxyErr.Error(), status)
			}
			return
		}
		if decision.EditBlockedReason == "" {
			outgoingBody = []byte(decision.Body)
		}
		if !maps.Equal(originalHeaders, decision.Headers) {
			for name := range originalHeaders {
				r.Header.Del(name)
			}
			for name, value := range decision.Headers {
				r.Header.Set(name, value)
			}
		}
	}
	if status, err := s.interrupted(r, traceID); err != nil {
		proxyErr = err
		recorder.statusCode = status
		return
	}
	if !bytes.Equal(outgoingBody, requestBody) {
		r.Body = io.NopCloser(bytes.NewReader(outgoingBody))
		r.ContentLength = int64(len(outgoingBody))
		r.GetBody, r.TransferEncoding, r.Trailer = nil, nil, nil
		r.Header.Del("Content-Length")
		r.Header.Del("Transfer-Encoding")
		s.publishLog(s.store.UpdateInput(traceID, correlation.ExtractRequest(r.Header, r.URL.Path, outgoingBody, s.target.String())))
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			if target.Path != "" && target.Path != "/" {
				req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			}

			req.Header.Del("Accept-Encoding")
		},
		Transport: captureTransport{base: s.transport, capture: func(req *http.Request) {
			upstreamStarted = time.Now()
			s.store.CaptureOutgoing(traceID, snapshotRequest(req, outgoingBody))
		}},
		ModifyResponse: func(resp *http.Response) error {
			contentType := resp.Header.Get("Content-Type")
			if strings.Contains(contentType, "text/event-stream") {
				recorder.isSSE = true
				resp.Header.Del("Content-Length")
			}

			if !recorder.isSSE && resp.Body != nil {
				respBody, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return fmt.Errorf("read upstream response: %w", err)
				}
				decoded := respBody
				if resp.Header.Get("Content-Encoding") == "gzip" {
					if reader, err := gzip.NewReader(bytes.NewReader(respBody)); err == nil {
						if data, err := io.ReadAll(reader); err == nil {
							decoded = data
						}
						reader.Close()
					}
				}
				recorder.captureJSON(decoded)
				resp.Body = io.NopCloser(bytes.NewReader(respBody))
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			proxyErr = err
			if status, interruptedErr := s.interrupted(r, traceID); interruptedErr != nil {
				proxyErr = interruptedErr
				recorder.statusCode = status
				return
			}
			s.logger.Error("❌ 代理请求失败", zap.String("trace_id", traceID), zap.Error(err))
			http.Error(w, "代理请求失败", http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(recorder, r)
}

func (s *Server) asyncLog(reqLog store.RequestLog) {
	go func() {
		s.logger.Info("📝 请求详情",
			zap.String("trace_id", reqLog.TraceID),
			zap.String("session_id", reqLog.SessionID),
			zap.String("parent_trace_id", reqLog.Correlation.ParentTraceID),
			zap.String("link_source", reqLog.Correlation.LinkSource),
			zap.String("response_id", reqLog.Correlation.ResponseID),
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
			zap.Float64("wait_duration_ms", reqLog.WaitDuration),
			zap.Float64("upstream_duration_ms", reqLog.UpstreamDuration),
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
