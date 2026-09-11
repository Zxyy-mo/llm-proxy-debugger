package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/adapter"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/export"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/observation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type wsMessage struct {
	kind int
	data []byte
	err  error
}
type wsCall struct {
	trace, lane          string
	initial              store.RequestLog
	recorder             *responseRecorder
	started, sent, first time.Time
	info                 store.WebSocketInfo
	cancelRequested      bool
	attempt              *upstreamAttempt
}

func readWS(ctx context.Context, conn *websocket.Conn, events chan<- wsMessage) {
	for {
		kind, data, err := conn.ReadMessage()
		select {
		case events <- wsMessage{kind: kind, data: data, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func writeWS(conn *websocket.Conn, kind int, data []byte) error {
	_ = conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return conn.WriteMessage(kind, data)
}

func wsCloseMessage(err error, fallback string) []byte {
	code, text := websocket.CloseInternalServerErr, fallback
	if closed, ok := err.(*websocket.CloseError); ok && closed.Code != websocket.CloseAbnormalClosure && closed.Code != websocket.CloseNoStatusReceived && closed.Code != websocket.CloseTLSHandshake {
		code, text = closed.Code, closed.Text
	}
	return websocket.FormatCloseMessage(code, text)
}

func (s *Server) dialWebSocket(ctx context.Context, r *http.Request, endpoint provider.Provider, subprotocols []string) (*websocket.Conn, *url.URL, error) {
	// 首帧不一定是 response.create；统一在拨号边界检查，防止取消/控制帧绕过能力声明。
	if err := endpoint.CheckEndpoint(r.URL.Path); err != nil {
		return nil, nil, err
	}
	if endpoint.ID != "" && !endpoint.WebSocket {
		return nil, nil, fmt.Errorf("Provider %s 未启用 WebSocket 能力", endpoint.ID)
	}
	if strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/responses") && endpoint.Protocol != "passthrough" {
		return nil, nil, fmt.Errorf("Responses WebSocket 需要原生透传 Provider")
	}
	target, err := url.Parse(endpoint.BaseURL)
	if err != nil {
		return nil, nil, err
	}
	target.Path, target.RawPath = provider.JoinURLPath(target, r.URL)
	target.RawQuery = r.URL.RawQuery
	if target.Scheme == "https" {
		target.Scheme = "wss"
	} else {
		target.Scheme = "ws"
	}
	headers := r.Header.Clone()
	for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol", "Host"} {
		headers.Del(key)
	}
	if endpoint.KeyEnv != "" {
		request := r.Clone(ctx)
		forwarding, _ := export.Classify(request)
		target.RawQuery = forwarding.Query
	}
	if err := endpoint.Authorize(headers); err != nil {
		return nil, nil, err
	}
	dialer := websocket.Dialer{Proxy: s.transport.Proxy, NetDialContext: s.transport.DialContext, TLSClientConfig: s.transport.TLSClientConfig.Clone(), HandshakeTimeout: 30 * time.Second, Subprotocols: subprotocols}
	conn, resp, err := dialer.DialContext(ctx, target.String(), headers)
	if err != nil {
		if resp != nil {
			if resp.Body != nil {
				resp.Body.Close()
			}
			return nil, nil, fmt.Errorf("WebSocket upstream handshake returned HTTP %d", resp.StatusCode)
		}
		return nil, nil, fmt.Errorf("WebSocket upstream connection failed: %w", err)
	}
	return conn, target, nil
}

func (s *Server) newWSCall(r *http.Request, connection, lane string, body []byte, selection provider.Selection) *wsCall {
	started := time.Now()
	trace := uuid.NewString()
	endpoint := selection.Endpoints[0]
	input := correlation.ExtractRequest(r.Header, r.URL.Path, body, endpoint.BaseURL)
	policy := s.store.Privacy.Policy()
	info := store.WebSocketInfo{ConnectionID: connection, StreamID: lane, EventID: gjson.GetBytes(body, "event_id").String()}
	initial := s.store.BeginWithBody(store.RequestLog{TraceID: trace, Time: started.Format(time.RFC3339Nano), Type: "WebSocket", Method: "WS", Path: r.URL.Path, Query: export.RedactQuery(r.URL.RawQuery), ClientIP: getClientIP(r), UserAgent: r.UserAgent(), Headers: extractHeaders(r.Header), WebSocket: &info}, input, body, s.cfg.MaxBodyLogSize, policy)
	snapshot := snapshotRequest(r, body)
	snapshot.Method = "WS"
	snapshot.ContentLength = int64(len(body))
	s.store.CaptureOriginal(trace, snapshot)
	recorder := newResponseRecorder(&replayWriter{header: make(http.Header)}, trace, &protocol.ResponsesHandler{}, s.hub, s.logger, s.cfg.MaxBodyLogSize, filepath.Join(s.cfg.LogDir, "sse"))
	recorder.isSSE = true
	if policy.Record {
		_, scope := s.store.PrivacyContext(trace)
		recorder.private = true
		recorder.project = func(body []byte) []byte { return s.store.Privacy.JSON(policy, scope, body) }
	}
	recorder.onTools = func(calls []observation.ToolCall) { s.store.ObserveTools(trace, calls) }
	recorder.onResponse = func(response correlation.Response) {
		if s.store.ObserveResponse(trace, response) {
			s.hub.Publish(map[string]any{"event": "sessions_updated"})
		}
	}
	s.hub.Publish(map[string]any{"event": "request_start", "trace_id": trace, "session_id": initial.SessionID, "method": "WS", "path": r.URL.Path, "time": initial.Time, "log": initial})
	s.store.SetRoute(trace, store.RouteInfo{ID: selection.RouteID, ProviderID: endpoint.ID, OriginalModel: input.Model, TargetModel: selection.Model, Attempts: []store.RouteAttempt{}})
	return &wsCall{trace: trace, lane: lane, initial: initial, recorder: recorder, started: started, info: info}
}

func (s *Server) wsFrame(call *wsCall, body []byte) {
	recorder := call.recorder
	if call.first.IsZero() {
		call.first = time.Now()
		recorder.headersAt = call.first
	}
	if recorder.response == nil {
		path, _ := filepath.Abs(filepath.Join(s.cfg.LogDir, "responses", call.trace+".ws.jsonl"))
		recorder.response = &store.ResponseSnapshot{TraceID: call.trace, Type: "websocket", ContentType: "application/x-ndjson", StatusCode: 101, Stored: "file", Receiving: true, Path: path, Representation: "websocket-frames"}
		if recorder.private {
			recorder.response.Stored = "missing"
			recorder.response.Reason = "记录脱敏已启用，结束后保存输出投影"
		}
		s.store.CaptureResponse(call.trace, *recorder.response)
	}
	call.info.Frames++
	call.info.ReceivedBytes += int64(len(body))
	// The JSONL envelope preserves each original text message, including its
	// whitespace and embedded newlines, without confusing message boundaries.
	raw, _ := json.Marshal(map[string]string{"type": "text", "data": string(body)})
	recorder.dumpRaw(append(raw, '\n'))
	if recorder.capture.Event(body) && recorder.onResponse != nil {
		recorder.onResponse(recorder.capture.Metadata())
	}
	recorder.accumulator.Accumulate(body)
	recorder.captureTools(body)
	recorder.markContent()
	metrics := *recorder.accumulator
	data := string(body)
	metrics.OutputContent = truncateBody([]byte(metrics.OutputContent), recorder.maxBodySize)
	metrics.ThinkingContent = truncateBody([]byte(metrics.ThinkingContent), recorder.maxBodySize)
	data = truncateBody([]byte(data), recorder.maxBodySize)
	if recorder.private {
		metrics.OutputContent, metrics.ThinkingContent, data = "", "", ""
	}
	s.hub.Publish(map[string]any{"event": "sse_delta", "trace_id": call.trace, "data": data, "metrics": metrics})
}

// finishWSCall 按模型终止事件或真实断连原因收敛请求和尝试；握手失败没有已发送模型帧。
func (s *Server) finishWSCall(call *wsCall, status int, failure error) {
	recorder := call.recorder
	response := recorder.capture.Response()
	log := call.initial
	if failure == nil && response.Error != "" {
		failure = fmt.Errorf("upstream: %s", response.Error)
	}
	if failure == nil && !response.Complete {
		failure = fmt.Errorf("WebSocket response ended without a completion event")
	}
	log.StatusCode = status
	log.Duration = float64(time.Since(call.started).Microseconds()) / 1000
	if !call.sent.IsZero() {
		log.UpstreamDuration = float64(time.Since(call.sent).Microseconds()) / 1000
	}
	log.InputTokens, log.OutputTokens, log.ThinkingTokens = recorder.accumulator.InputTokens, recorder.accumulator.OutputTokens, recorder.accumulator.ThinkingTokens
	log.TokenSources = recorder.accumulator.TokenSources
	if failure != nil || status >= 400 {
		log.TokenSources = protocol.UnknownSources()
	} else if !call.sent.IsZero() && !recorder.contentAt.IsZero() {
		first := float64(call.first.Sub(call.sent).Microseconds()) / 1000
		content := float64(recorder.contentAt.Sub(call.sent).Microseconds()) / 1000
		log.TTFB, log.TTFC = &first, &content
	}
	if saved := recorder.finishResponse(failure == nil); saved != nil {
		s.store.CaptureResponse(call.trace, *saved)
		log.ResponseBytes, log.ResponseStored = saved.Bytes, saved.Stored
	}
	policy, scope := s.store.PrivacyContext(call.trace)
	output, thinking := recorder.accumulator.OutputContent, recorder.accumulator.ThinkingContent
	if policy.Record {
		output = s.store.Privacy.Text(policy, scope, output)
		thinking = s.store.Privacy.Text(policy, scope, thinking)
	}
	log.ResponseBody, log.ThinkingContent = truncateBody([]byte(output), s.cfg.MaxBodyLogSize), truncateBody([]byte(thinking), s.cfg.MaxBodyLogSize)
	log.ToolUseCount, log.IsThinkingLoop = recorder.accumulator.ToolUseCount, recorder.accumulator.IsThinkingLoop
	info := call.info
	log.WebSocket = &info
	if status == 499 {
		s.store.MarkCanceled(call.trace)
	}
	if failure != nil {
		log.Error = failure.Error()
	}
	if status == 499 {
		call.attempt.finish("canceled", failure)
	} else if failure != nil {
		call.attempt.finish("error", failure)
	} else {
		call.attempt.finish("done", nil)
	}
	log = s.store.Complete(call.trace, log, response)
	s.hub.Publish(map[string]any{"event": "request_end", "trace_id": call.trace, "log": log})
	s.hub.Publish(map[string]any{"event": "sessions_updated"})
	s.asyncLog(log)
}

// handleWebSocket 固定连接上游，在各 lane 内关联可见帧；连接本身不证明会话、任务或因果关系。
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		http.Error(w, "gateway closed", 503)
		return
	}
	s.websocketWG.Add(1)
	s.closeMu.Unlock()
	defer s.websocketWG.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.lifecycle, cancel)
	defer cancel()
	defer stop()
	observed := strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "/responses")
	var upstream *websocket.Conn
	var destination *url.URL
	var pinned provider.Provider
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	if !observed {
		selection := s.selectProvider(r.URL.Query().Get("model"))
		pinned = selection.Endpoints[0]
		if pinned.ID != "" && !pinned.WebSocket {
			http.Error(w, "Provider WebSocket capability is disabled", 422)
			return
		}
		if err := pinned.CheckEndpoint(r.URL.Path); err != nil {
			http.Error(w, err.Error(), 422)
			return
		}
		var err error
		upstream, destination, err = s.dialWebSocket(ctx, r, pinned, websocket.Subprotocols(r))
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer upstream.Close()
		if upstream.Subprotocol() != "" {
			upgrader.Subprotocols = []string{upstream.Subprotocol()}
		}
	} else {
		upgrader.Subprotocols = websocket.Subprotocols(r)
	}
	client, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()
	// A per-message bound protects observability while still supporting large
	// context frames; no input is truncated and forwarded as a different body.
	client.SetReadLimit(64 << 20)
	clientEvents, upstreamEvents := make(chan wsMessage, 1), make(chan wsMessage, 1)
	go readWS(ctx, client, clientEvents)
	if upstream != nil {
		upstream.SetReadLimit(64 << 20)
		go readWS(ctx, upstream, upstreamEvents)
	}
	connection := uuid.NewString()
	lanes := map[string][]*wsCall{}
	seenLanes := map[string]bool{}
	responseOwners := map[string]*wsCall{}
	pending := 0
	endStatus := 502
	endReason := "WebSocket upstream closed before response completion"
	defer func() {
		// 先按真实退出原因记录终态，再取消内部读循环；清理不能把上游断连误记为用户取消。
		for _, queue := range lanes {
			for _, call := range queue {
				s.finishWSCall(call, endStatus, fmt.Errorf("%s", endReason))
			}
		}
		cancel()
		if upstream != nil {
			upstream.Close()
		}
	}()
	reject := func(call *wsCall, status int, message string) error {
		s.finishWSCall(call, status, fmt.Errorf("%s", message))
		value := map[string]any{"type": "error", "error": map[string]any{"type": "gateway_error", "code": "gateway_unsupported", "message": message, "trace_id": call.trace}}
		if call.lane != "" {
			value["stream_id"] = call.lane
		}
		if call.info.EventID != "" {
			value["event_id"] = call.info.EventID
		}
		raw, _ := json.Marshal(value)
		return writeWS(client, websocket.TextMessage, raw)
	}
	for {
		select {
		case <-ctx.Done():
			endStatus = 499
			endReason = "WebSocket connection canceled; requests are not automatically resent"
			return
		case message := <-clientEvents:
			if message.err != nil {
				endStatus = 499
				endReason = "WebSocket client disconnected; requests are not automatically resent"
				if upstream != nil {
					_ = upstream.WriteControl(websocket.CloseMessage, wsCloseMessage(message.err, "client closed"), time.Now().Add(time.Second))
				}
				return
			}
			body := message.data
			var call *wsCall
			selection := s.selectProvider(r.URL.Query().Get("model"))
			if observed && message.kind == websocket.TextMessage && gjson.ValidBytes(body) && gjson.GetBytes(body, "type").String() == "response.create" {
				model := gjson.GetBytes(body, "model").String()
				selection = s.selectProvider(model)
				laneValue := gjson.GetBytes(body, "stream_id")
				lane := laneValue.String()
				call = s.newWSCall(r, connection, lane, body, selection)
				failure := ""
				endpoint := selection.Endpoints[0]
				if laneValue.Exists() && (laneValue.Type != gjson.String || len(lane) > 128) {
					failure = "stream_id must be a string of at most 128 bytes"
				} else if pending >= 16 {
					failure = "at most 16 outstanding Responses are supported per connection"
				} else if lane != "" && !seenLanes[lane] && len(seenLanes) >= 32 {
					failure = "at most 32 named streams are supported per connection"
				} else if endpoint.Protocol != "passthrough" || (endpoint.ID != "" && !endpoint.WebSocket) {
					failure = "WebSocket requests need a passthrough Provider with WebSocket enabled"
				} else if err := endpoint.CheckEndpoint(r.URL.Path); err != nil {
					failure = err.Error()
				} else if upstream != nil && (pinned.ID != endpoint.ID || pinned.BaseURL != endpoint.BaseURL || pinned.KeyEnv != endpoint.KeyEnv) {
					failure = "this WebSocket is pinned to its first Provider; open another connection for a different route"
				}
				prepared, breakpoint := s.prepareRequest(r.URL.Path, body)
				if breakpoint != nil {
					failure = "WebSocket frame interception is unavailable; use HTTP Responses for breakpoint editing"
				}
				if failure == "" {
					var aliasErr error
					_, prepared, _, aliasErr = adapter.Request(r.URL.Path, prepared, "passthrough", selection.Model)
					if aliasErr != nil {
						failure = aliasErr.Error()
					}
				}
				if failure != "" {
					if reject(call, 422, failure) != nil {
						return
					}
					continue
				}
				correlate := func(body []byte) correlation.Request {
					updated := correlation.ExtractRequest(r.Header, r.URL.Path, body, endpoint.BaseURL)
					policy, scope := s.store.PrivacyContext(call.trace)
					if policy.Record {
						safe := s.store.Privacy.JSON(policy, scope, body)
						updated.Summary = correlation.ExtractRequest(r.Header, r.URL.Path, safe, endpoint.BaseURL).Summary
					}
					return updated
				}
				if !bytes.Equal(prepared, message.data) {
					s.publishLog(s.store.UpdateInput(call.trace, correlate(prepared)))
				}
				body = s.store.Outbound(call.trace, prepared)
				if !bytes.Equal(body, prepared) {
					s.publishLog(s.store.UpdateOutboundInput(call.trace, correlate(body)))
				}
				s.store.ObserveToolResults(call.trace, body)
			}
			if upstream == nil {
				pinned = selection.Endpoints[0]
				subprotocols := []string{}
				if client.Subprotocol() != "" {
					subprotocols = append(subprotocols, client.Subprotocol())
				}
				upstream, destination, err = s.dialWebSocket(ctx, r, pinned, subprotocols)
				if err != nil {
					if call != nil {
						_ = reject(call, 502, err.Error())
					} else {
						// 控制帧被拒绝时仍返回明确原因，不虚构一条模型请求来承载错误。
						raw, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "gateway_error", "message": err.Error()}})
						_ = writeWS(client, websocket.TextMessage, raw)
					}
					return
				}
				if client.Subprotocol() != upstream.Subprotocol() {
					if call != nil {
						_ = reject(call, 502, "upstream WebSocket subprotocol negotiation differs")
					}
					return
				}
				upstream.SetReadLimit(64 << 20)
				go readWS(ctx, upstream, upstreamEvents)
			}
			if call != nil {
				out := r.Clone(ctx)
				u := *destination
				out.URL = &u
				out.Host = u.Host
				out.Method = "WS"
				out.ContentLength = int64(len(body))
				_ = pinned.Authorize(out.Header)
				// 只有即将写出的 response.create 才拥有模型 Attempt；握手失败保持未发送。
				call.attempt = s.beginUpstreamAttempt(call.trace, out, pinned, body, "websocket")
				call.sent = time.Now()
				if call.attempt != nil {
					call.sent = call.attempt.started
				}
				call.recorder.upstreamAt = call.sent
				lanes[call.lane] = append(lanes[call.lane], call)
				if call.lane != "" {
					seenLanes[call.lane] = true
				}
				pending++
			}
			if observed && gjson.GetBytes(body, "type").String() == "response.cancel" {
				lane := gjson.GetBytes(body, "stream_id").String()
				if queue := lanes[lane]; len(queue) > 0 {
					queue[0].cancelRequested = true
				}
			}
			if err := writeWS(upstream, message.kind, body); err != nil {
				endReason = "WebSocket upstream write failed"
				return
			}
			if call != nil {
				call.attempt.websocketSent()
			}
		case message := <-upstreamEvents:
			if message.err != nil {
				_ = client.WriteControl(websocket.CloseMessage, wsCloseMessage(message.err, "upstream closed"), time.Now().Add(time.Second))
				return
			}
			if observed && message.kind == websocket.TextMessage && gjson.ValidBytes(message.data) {
				root := gjson.ParseBytes(message.data)
				lane := root.Get("stream_id").String()
				id := root.Get("response.id").String()
				if id == "" {
					id = root.Get("response_id").String()
				}
				var call *wsCall
				if id != "" {
					call = responseOwners[id]
				}
				if call == nil {
					if queue := lanes[lane]; len(queue) > 0 {
						call = queue[0]
					}
				}
				kind := root.Get("type").String()
				if call != nil && (strings.HasPrefix(kind, "response.") || kind == "error") {
					if id != "" {
						responseOwners[id] = call
					}
					s.wsFrame(call, message.data)
					terminal := kind == "response.completed" || kind == "response.failed" || kind == "response.incomplete" || kind == "response.cancelled" || kind == "response.canceled" || kind == "error"
					if terminal {
						status := 101
						var failure error
						if kind == "response.cancelled" || kind == "response.canceled" {
							status = 499
							failure = fmt.Errorf("WebSocket response canceled")
						}
						if call.cancelRequested && (kind == "response.incomplete" || kind == "error") {
							status = 499
						}
						s.finishWSCall(call, status, failure)
						queue := lanes[call.lane]
						for i, candidate := range queue {
							if candidate == call {
								lanes[call.lane] = append(queue[:i], queue[i+1:]...)
								break
							}
						}
						for responseID, owner := range responseOwners {
							if owner == call {
								delete(responseOwners, responseID)
							}
						}
						pending--
					}
				}
			}
			if err := writeWS(client, message.kind, message.data); err != nil {
				endStatus = 499
				endReason = "WebSocket client disconnected"
				return
			}
		}
	}
}
