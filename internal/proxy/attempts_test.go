package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestHTTPAttemptsKeepEachDestinationAndReplayGetsNewRun(t *testing.T) {
	t.Setenv("ATTEMPT_PRIMARY_KEY", "primary-attempt-secret")
	t.Setenv("ATTEMPT_BACKUP_KEY", "backup-attempt-secret")
	var hits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer backup-attempt-secret" {
			t.Error("备用上游鉴权错误")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"attempt-%d","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`, n)
	}))
	defer backup.Close()
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer primary-attempt-secret" {
			t.Error("主上游鉴权错误")
		}
		w.WriteHeader(503)
	}), 1024)
	config := provider.Config{Providers: []provider.Provider{
		{ID: "first", BaseURL: proxy.target.String() + "/first/v1", Protocol: "passthrough", KeyEnv: "ATTEMPT_PRIMARY_KEY"},
		{ID: "backup", BaseURL: backup.URL + "/backup/v1", Protocol: "passthrough", KeyEnv: "ATTEMPT_BACKUP_KEY"},
	}, Routes: []provider.Route{{ID: "r", Model: "alias", TargetModel: "actual", ProviderID: "first", Failover: []string{"backup"}}}}
	if err := proxy.providers.Set(config); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"alias","metadata":{"run_id":"turn-one"},"seed":9007199254740993,"messages":[{"role":"user","content":"investigate"}]}`
	trace, reply := send(t, proxy, "POST", "/v1/chat/completions", body, map[string]string{"X-Session-ID": "four-layers", "X-Run-ID": "turn-one"})
	log := logOf(t, storage, trace)
	if reply.Code != 200 || log.RunID == "" || reply.Header().Get("X-Gateway-Run-ID") != log.RunID || len(log.Route.Attempts) != 2 {
		t.Fatalf("请求与分层记录不完整：%d %+v", reply.Code, log)
	}
	first, second := log.Route.Attempts[0], log.Route.Attempts[1]
	if first.ID == "" || first.ID == second.ID || first.Sequence != 1 || second.Sequence != 2 || first.Status != "abandoned" || second.Status != "done" {
		t.Fatalf("尝试身份或结果错误：%+v", log.Route.Attempts)
	}
	for _, attempt := range log.Route.Attempts {
		if attempt.TraceID != trace || attempt.Source != "gateway" || attempt.StartedAt == "" || attempt.EndedAt == "" || attempt.HeadersAt == "" || attempt.TotalDuration == nil || *attempt.TotalDuration < attempt.Duration {
			t.Fatalf("尝试计时不完整：%+v", attempt)
		}
		detail, ok := storage.AttemptSnapshot(trace, attempt.ID)
		if !ok || detail.Request.Unavailable != "" || gjson.Get(detail.Request.Body, "model").String() != "actual" || !strings.Contains(detail.Request.Body, "9007199254740993") {
			t.Fatalf("逐次出站快照错误：%+v", detail)
		}
		if !strings.Contains(detail.Request.URL, "/"+attempt.ProviderID+"/v1/chat/completions") {
			t.Fatalf("早期尝试被最后一次出站覆盖：%s", detail.Request.URL)
		}
		raw, _ := json.Marshal(detail)
		if strings.Contains(string(raw), "attempt-secret") {
			t.Fatal("独立尝试捕获泄漏凭证")
		}
		status, downloaded := api(t, storage.AttemptsHandler, "GET", "/api/attempts/"+trace+"/"+attempt.ID+"/body", nil)
		if status != 200 || string(downloaded) != detail.Request.Body {
			t.Fatalf("尝试正文下载错误：%d %s", status, downloaded)
		}
	}
	capture, _ := storage.RequestSnapshot(trace)
	if capture.Outgoing.URL != second.RequestURL {
		t.Fatal("兼容 outgoing 未指向最后一次尝试")
	}
	replayed := createReplay(t, proxy, map[string]any{"trace_id": trace, "source": "outgoing", "idempotency_key": "run-replay"}, 202)
	replayed = finishedReplay(t, proxy, replayed.ID)
	result := logOf(t, storage, replayed.TraceID)
	if result.RunID == "" || result.RunID == log.RunID || result.Run.State != "replay" || result.Replay.Of != trace || len(result.Route.Attempts) != 1 {
		t.Fatalf("重放继承了源任务或多发请求：%+v", result)
	}
	replayedCapture, _ := storage.RequestSnapshot(replayed.TraceID)
	if replayedCapture.Original.Body != capture.Outgoing.Body || replayedCapture.Original.Headers["X-Run-Id"] != capture.Outgoing.Headers["X-Run-Id"] {
		t.Fatal("为分配新任务而改变了精确重放报文")
	}
	createReplay(t, proxy, map[string]any{"trace_id": trace, "source": "outgoing", "idempotency_key": "run-replay"}, 200)
	if hits.Load() != 2 {
		t.Fatalf("同一幂等键重复执行：%d", hits.Load())
	}
	// 出站重放固定到备用上游，旧有上游/凭证作用域仍然隔离会话；全局任务视图保留两笔执行。
	runs, ok := storage.RunsSnapshot("")
	if !ok || len(runs.Runs) != 2 || len(runs.UnassociatedTraceIDs) != 0 {
		t.Fatalf("任务汇总未区分重放：%+v", runs)
	}
	scoped, _ := storage.RunsSnapshot(log.SessionID)
	if result.SessionID == log.SessionID || len(scoped.Runs) != 1 {
		t.Fatal("为了任务分组而破坏了原有上游作用域隔离")
	}
}

// awaitHeaderAttempt 等待真实上游头部已经到达，用于在正文尚未结束时检查观测状态。
func awaitHeaderAttempt(t *testing.T, storage *store.Store) (string, store.RouteAttempt) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, session := range storage.SessionsSnapshot() {
			for _, log := range session.Logs {
				if log.Route != nil && len(log.Route.Attempts) > 0 && log.Route.Attempts[0].HeadersAt != "" {
					return log.TraceID, log.Route.Attempts[0]
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("未观察到上游响应头")
	return "", store.RouteAttempt{}
}

func TestAttemptRemainsRunningUntilResponseBodyEnds(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			io.WriteString(w, `{"choices":[{"message":{"content":"finished"}}]}`)
		case <-r.Context().Done():
		}
	}), 1024)
	done := make(chan struct{})
	go func() {
		defer close(done)
		proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)))
	}()
	trace, attempt := awaitHeaderAttempt(t, storage)
	if attempt.Status != "running" || attempt.TotalDuration != nil || attempt.EndedAt != "" {
		t.Fatalf("将响应头误记为完整响应：%+v", attempt)
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("响应未结束")
	}
	final := logOf(t, storage, trace).Route.Attempts[0]
	if final.Status != "done" || final.TotalDuration == nil || final.EndedAt == "" || *final.TotalDuration < final.Duration {
		t.Fatalf("完整响应未更新尝试：%+v", final)
	}
}

func TestAttemptCancellationAndResponseErrors(t *testing.T) {
	t.Run("取消流", func(t *testing.T) {
		proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}), 1024)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`)).WithContext(ctx)
			proxy.ServeHTTP(httptest.NewRecorder(), req)
		}()
		trace, _ := awaitHeaderAttempt(t, storage)
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("取消没有结束流")
		}
		log := logOf(t, storage, trace)
		if log.Status != "canceled" || log.Route.Attempts[0].Status != "canceled" || log.Route.Attempts[0].TotalDuration == nil {
			t.Fatalf("取消状态丢失：%+v", log)
		}
	})
	for _, mode := range []string{"正文截断", "SSE 缺失完成事件", "上游 HTTP 错误"} {
		t.Run(mode, func(t *testing.T) {
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "正文截断":
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Content-Length", "1000")
					io.WriteString(w, `{"short":true}`)
				case "SSE 缺失完成事件":
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				default:
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"message":"rate limited"}}`)
				}
			}), 1024)
			trace, _ := send(t, proxy, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`, nil)
			log := logOf(t, storage, trace)
			if log.Status != "error" || len(log.Route.Attempts) != 1 || log.Route.Attempts[0].Status != "error" || log.Route.Attempts[0].Error == "" {
				t.Fatalf("上游错误未保留：%+v", log)
			}
		})
	}
}

func TestLocalRejectionHasNoOutboundAttempt(t *testing.T) {
	for _, mode := range []string{"capability", "credential", "conversion"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			proxy, storage := testProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }), 1024)
			p := provider.Provider{ID: "p", BaseURL: proxy.target.String(), Protocol: "passthrough"}
			path, body := "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}]}`
			switch mode {
			case "capability":
				p.Capabilities.ChatCompletions = provider.CapabilityUnsupported
			case "credential":
				t.Setenv("ATTEMPT_MISSING_KEY", "")
				p.KeyEnv = "ATTEMPT_MISSING_KEY"
			case "conversion":
				p.Protocol = "openai"
				path, body = "/v1/responses", `{"model":"m","input":"x","previous_response_id":"remote"}`
			}
			if err := proxy.providers.Set(provider.Config{Providers: []provider.Provider{p}, Routes: []provider.Route{{ID: "r", Model: "m", ProviderID: "p"}}}); err != nil {
				t.Fatal(err)
			}
			trace, response := send(t, proxy, "POST", path, body, nil)
			attempts, ok := storage.AttemptsSnapshot(trace)
			capture, _ := storage.RequestSnapshot(trace)
			if !ok || len(attempts) != 0 || capture.Outgoing != nil || hits.Load() != 0 || response.Code < 400 {
				t.Fatalf("本地失败伪造出站：%d %+v", response.Code, attempts)
			}
		})
	}
}

func TestWebSocketCreatesHaveSeparateAttemptsWithinExplicitRun(t *testing.T) {
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for i := 1; i <= 2; i++ {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			_ = conn.WriteMessage(websocket.TextMessage, []byte(wsEvent("response.completed", "", fmt.Sprintf("ws-attempt-%d", i), "ok")))
		}
		_, _, _ = conn.ReadMessage()
	}), 1024)
	client := connectTestWS(t, proxy, http.Header{"X-Session-Id": {"ws-session"}, "X-Run-Id": {"ws-run"}})
	var previous store.RequestLog
	for i := 1; i <= 2; i++ {
		if err := client.WriteMessage(websocket.TextMessage, []byte(wsCreate("", "x", ""))); err != nil {
			t.Fatal(err)
		}
		if _, _, err := client.ReadMessage(); err != nil {
			t.Fatal(err)
		}
		log := logByResponse(t, storage, fmt.Sprintf("ws-attempt-%d", i))
		if len(log.Route.Attempts) != 1 || log.RunID == "" || log.Correlation.ParentTraceID != "" {
			t.Fatalf("WS 调用层次错误：%+v", log)
		}
		attempt := log.Route.Attempts[0]
		if attempt.ID == "" || attempt.Transport != "websocket" || attempt.Status != "done" || attempt.StatusCode != 101 || attempt.TotalDuration == nil || attempt.HeadersAt != "" {
			t.Fatalf("WS 帧冒充 HTTP 握手或未结束：%+v", attempt)
		}
		if i == 2 && (log.RunID != previous.RunID || attempt.ID == previous.Route.Attempts[0].ID) {
			t.Fatal("同轮 WS 请求归属或独立 Attempt 身份错误")
		}
		previous = log
	}
}

func TestWebSocketDisconnectKeepsFailureAndHandshakeHasNoModelAttempt(t *testing.T) {
	for _, handshakeFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("handshake_failure_%t", handshakeFailure), func(t *testing.T) {
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if handshakeFailure {
					w.WriteHeader(503)
					return
				}
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_, _, _ = conn.ReadMessage()
				// 真实上游在收到模型帧后断开，不能被网关清理 context 的动作改写为取消。
			}), 1024)
			client := connectTestWS(t, proxy, nil)
			if err := client.WriteMessage(websocket.TextMessage, []byte(wsCreate("", "disconnect", ""))); err != nil {
				t.Fatal(err)
			}
			_, _, _ = client.ReadMessage()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				for _, session := range storage.SessionsSnapshot() {
					for _, log := range session.Logs {
						if log.Status == "running" {
							continue
						}
						attempts := log.Route.Attempts
						if log.Status != "error" || log.StatusCode != 502 {
							t.Fatalf("断连应记为失败：%+v", log)
						}
						if handshakeFailure {
							if len(attempts) != 0 {
								t.Fatal("连接握手冒充已发送模型帧")
							}
						} else if len(attempts) != 1 || attempts[0].Status != "error" || attempts[0].TotalDuration == nil {
							t.Fatalf("上游断连被记成取消：%+v", attempts)
						}
						return
					}
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("断开的模型调用未收敛到终态")
		})
	}
}

func TestWebSocketControlFrameCannotBypassProviderCapabilities(t *testing.T) {
	for _, mode := range []string{"responses_disabled", "websocket_disabled", "conversion_mode"} {
		t.Run(mode, func(t *testing.T) {
			var hits atomic.Int32
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(503)
			}), 1024)
			p := provider.Provider{ID: "p", BaseURL: proxy.target.String(), Protocol: "passthrough", WebSocket: true}
			switch mode {
			case "responses_disabled":
				p.Capabilities.Responses = provider.CapabilityUnsupported
			case "websocket_disabled":
				p.WebSocket = false
			case "conversion_mode":
				p.Protocol = "openai"
			}
			if err := proxy.providers.Set(provider.Config{Providers: []provider.Provider{p}, Routes: []provider.Route{{ID: "r", Model: "m", ProviderID: "p"}}}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(proxy.Close)
			server := httptest.NewServer(proxy)
			t.Cleanup(server.Close)
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses?model=m", nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_ = client.SetReadDeadline(time.Now().Add(3 * time.Second))
			if err := client.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.cancel"}`)); err != nil {
				t.Fatal(err)
			}
			_, response, err := client.ReadMessage()
			if err != nil || gjson.GetBytes(response, "type").String() != "error" {
				t.Fatalf("控制帧拒绝缺少明确原因：%s %v", response, err)
			}
			if hits.Load() != 0 || len(storage.SessionsSnapshot()) != 0 {
				t.Fatal("非创建帧绕过了接口声明或伪造模型执行记录")
			}
		})
	}
}
