package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/adapter"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/config"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/export"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/observation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
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
	providers     *provider.Manager
	lifecycle     context.Context
	cancel        context.CancelFunc
	closeMu       sync.Mutex
	closed        bool
	websocketWG   sync.WaitGroup
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
	if (targetURL.Scheme != "http" && targetURL.Scheme != "https") || targetURL.Host == "" || targetURL.User != nil || targetURL.RawQuery != "" || targetURL.Fragment != "" {
		return nil, fmt.Errorf("target must be an http(s) base URL without credentials, query or fragment")
	}
	if err := s.SetCaptureRoot(cfg.LogDir); err != nil {
		return nil, err
	}
	server := &Server{
		target:        targetURL,
		transport:     newTransport(),
		hub:           h,
		store:         s,
		logger:        log,
		cfg:           cfg,
		interceptions: intercept.New(func() { h.Publish(map[string]any{"event": "interceptions_updated"}) }),
		providers:     provider.New(),
	}
	server.transport.TLSClientConfig.InsecureSkipVerify = cfg.Insecure
	server.lifecycle, server.cancel = context.WithCancel(context.Background())
	var providerConfig provider.Config
	if s.Setting("providers", &providerConfig) {
		if err := server.providers.Set(providerConfig); err != nil {
			return nil, fmt.Errorf("restore providers: %w", err)
		}
	}
	server.replays = replay.NewWithPersistence(func() {
		h.Publish(map[string]any{"event": "replays_updated"})
	}, func(saved []replay.Saved, durable bool) error {
		if durable {
			return s.SetSettingDurable("replays", saved)
		}
		s.SetSetting("replays", saved)
		return nil
	})
	var saved []replay.Saved
	if s.Setting("replays", &saved) {
		server.replays.Restore(saved)
	}
	return server, nil
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
	selection := s.selectProvider(input.Model)
	if replayOpts != nil && replayOpts.Selection != nil {
		selection = *replayOpts.Selection
	}
	scopeTarget := selection.Endpoints[0].BaseURL
	input = correlation.ExtractRequest(r.Header, r.URL.Path, requestBody, scopeTarget)
	requestPolicy := s.store.Privacy.Policy()
	var handler protocol.Handler = &protocol.AnthropicHandler{}
	switch input.Protocol {
	case "openai":
		handler = &protocol.OpenAIHandler{}
	case "responses":
		handler = &protocol.ResponsesHandler{}
	}
	initial := s.store.BeginWithBody(store.RequestLog{
		Time: startTime.Format(time.RFC3339Nano), TraceID: traceID,
		Type: "HTTP", ClientIP: clientIP, Method: r.Method, Path: r.URL.Path,
		Query: export.RedactQuery(r.URL.RawQuery), Headers: extractHeaders(r.Header),
		UserAgent: r.UserAgent(),
		Replay:    replayInfo,
	}, input, requestBody, s.cfg.MaxBodyLogSize, requestPolicy)
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
	policy, privacyScope := s.store.PrivacyContext(traceID)
	if policy.Record {
		recorder.private = true
		recorder.project = func(body []byte) []byte { return s.store.Privacy.JSON(policy, privacyScope, body) }
	}
	defer recorder.close()
	recorder.onTools = func(calls []observation.ToolCall) { s.store.ObserveTools(traceID, calls) }
	recorder.onResponse = func(response correlation.Response) {
		if s.store.ObserveResponse(traceID, response) {
			s.hub.Publish(map[string]interface{}{"event": "sessions_updated"})
		}
	}
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
		observationErr := recorder.sseParser.Err()
		if proxyErr == nil && response.Error != "" {
			proxyErr = fmt.Errorf("upstream: %s", response.Error)
		}
		if proxyErr == nil && observationErr == nil && recorder.isSSE && response.Recognized && !response.Complete {
			proxyErr = fmt.Errorf("SSE stream ended without a completion event")
		}
		log := initial
		if observationErr != nil {
			log.ObservationWarning = "SSE 事件超过 2 MiB 观测上限；文本、工具及指标观测不完整。响应字节继续转发，Token 与首内容时间记为未知。"
			response.Complete = false
			if recorder.response != nil {
				recorder.response.ObservationWarning = log.ObservationWarning
			}
		}
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
		log.TokenSources = recorder.accumulator.TokenSources
		if proxyErr != nil || log.StatusCode >= 400 || observationErr != nil {
			log.TokenSources = protocol.UnknownSources()
		} else if response.Recognized && !recorder.contentAt.IsZero() && !upstreamStarted.IsZero() {
			ttfb := float64(recorder.headersAt.Sub(upstreamStarted).Microseconds()) / 1000
			ttfc := float64(recorder.contentAt.Sub(upstreamStarted).Microseconds()) / 1000
			log.TTFB, log.TTFC = &ttfb, &ttfc
		}
		if saved := recorder.finishResponse(proxyErr == nil && !(observationErr != nil && policy.Record)); saved != nil {
			s.store.CaptureResponse(traceID, *saved)
			log.ResponseBytes, log.ResponseStored = saved.Bytes, saved.Stored
		}
		thinking, output := recorder.accumulator.ThinkingContent, recorder.accumulator.OutputContent
		if policy.Record {
			thinking = s.store.Privacy.Text(policy, privacyScope, thinking)
			output = s.store.Privacy.Text(policy, privacyScope, output)
		}
		log.ThinkingTokens, log.ThinkingContent = recorder.accumulator.ThinkingTokens, truncateBody([]byte(thinking), s.cfg.MaxBodyLogSize)
		log.ToolUseCount, log.IsThinkingLoop = recorder.accumulator.ToolUseCount, recorder.accumulator.IsThinkingLoop
		if recorder.isSSE {
			log.Type = "SSE"
			log.ResponseBody = truncateBody([]byte(output), s.cfg.MaxBodyLogSize)
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
	correlate := func(body []byte) correlation.Request {
		updated := correlation.ExtractRequest(r.Header, r.URL.Path, body, scopeTarget)
		if policy.Record {
			safe := s.store.Privacy.JSON(policy, privacyScope, body)
			updated.Summary = correlation.ExtractRequest(r.Header, r.URL.Path, safe, scopeTarget).Summary
		}
		return updated
	}
	if !bytes.Equal(outgoingBody, requestBody) {
		s.publishLog(s.store.UpdateInput(traceID, correlate(outgoingBody)))
	}
	preparedBody := outgoingBody
	outgoingBody = s.store.Outbound(traceID, preparedBody)
	if !bytes.Equal(outgoingBody, preparedBody) {
		s.publishLog(s.store.UpdateOutboundInput(traceID, correlate(outgoingBody)))
	}
	s.store.ObserveToolResults(traceID, outgoingBody)
	outgoingPath, convertedBody, conversion, conversionErr := adapter.Request(r.URL.Path, outgoingBody, selection.Endpoints[0].Protocol, selection.Model)
	if conversionErr != nil {
		proxyErr = conversionErr
		writeProtocolError(recorder, input.Protocol, 422, conversionErr.Error())
		return
	}
	outgoingBody = convertedBody
	outgoingRawPath := r.URL.RawPath
	if outgoingPath != r.URL.Path {
		outgoingRawPath = ""
	}
	routeInfo := store.RouteInfo{ID: selection.RouteID, ProviderID: selection.Endpoints[0].ID, Attempts: []store.RouteAttempt{}, OriginalModel: input.Model, TargetModel: selection.Model}
	if conversion != nil {
		routeInfo.Conversion = input.Protocol + " → openai"
	}
	s.store.SetRoute(traceID, routeInfo)
	if !bytes.Equal(outgoingBody, requestBody) {
		r.Body = io.NopCloser(bytes.NewReader(outgoingBody))
		r.ContentLength = int64(len(outgoingBody))
		r.GetBody, r.TransferEncoding, r.Trailer = nil, nil, nil
		r.Header.Del("Content-Length")
		r.Header.Del("Transfer-Encoding")
	}

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.Header.Del("Accept-Encoding")
		},
		Transport: routeTransport{base: s.transport, selection: selection, path: outgoingPath, rawPath: outgoingRawPath, body: outgoingBody, conversion: conversion, info: routeInfo,
			capture: func(req *http.Request, endpoint provider.Provider) {
				snapshot := snapshotRequest(req, outgoingBody)
				snapshot.Forwarding.ProviderID, snapshot.Forwarding.BaseURL = endpoint.ID, endpoint.BaseURL
				s.store.CaptureOutgoing(traceID, snapshot)
			}, started: func() {
				upstreamStarted = time.Now()
				recorder.upstreamAt = upstreamStarted
			}, update: func(info store.RouteInfo) { s.store.SetRoute(traceID, info) }, received: func(at time.Time) { recorder.headersAt = at }, upstream: func(resp *http.Response) *http.Response {
				return s.captureConversionUpstream(traceID, resp, policy.Record)
			}},
		ModifyResponse: func(resp *http.Response) error {
			contentType := resp.Header.Get("Content-Type")
			if strings.Contains(contentType, "text/event-stream") {
				recorder.isSSE = true
				resp.Header.Del("Content-Length")
			}
			recorder.beginResponse(resp)
			if conversion != nil {
				recorder.response.Representation = "converted-from-openai"
			}
			s.store.CaptureResponse(traceID, *recorder.response)

			if !recorder.isSSE && resp.Body != nil {
				respBody, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					return fmt.Errorf("read upstream response: %w", err)
				}
				decoded := respBody
				if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
					if reader, err := gzip.NewReader(bytes.NewReader(respBody)); err == nil {
						if data, err := io.ReadAll(reader); err == nil {
							decoded = data
							recorder.response.Decoded = true
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

func (s *Server) Close() {
	s.closeMu.Lock()
	s.closed = true
	s.cancel()
	s.closeMu.Unlock()
	s.replays.Shutdown()
	s.websocketWG.Wait()
	s.transport.CloseIdleConnections()
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
			zap.Any("token_sources", reqLog.TokenSources),
			zap.Any("ttfb_ms", reqLog.TTFB),
			zap.Any("ttfc_ms", reqLog.TTFC),
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
