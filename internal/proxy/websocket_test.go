package proxy

import (
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/gorilla/websocket"
)

func wsCreate(lane, input, previous string) string {
	value := map[string]any{"type": "response.create", "model": "mock", "stream_id": lane, "input": input}
	if previous != "" {
		value["previous_response_id"] = previous
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}
func wsEvent(kind, lane, id, text string) string {
	response := map[string]any{"id": id, "object": "response", "model": "mock", "status": "in_progress"}
	if kind == "response.completed" {
		response["status"] = "completed"
		response["output"] = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
		response["usage"] = map[string]int{"input_tokens": 3, "output_tokens": 2}
	}
	raw, _ := json.Marshal(map[string]any{"type": kind, "stream_id": lane, "response": response})
	return string(raw)
}
func connectTestWS(t *testing.T, proxy *Server, headers http.Header) *websocket.Conn {
	t.Helper()
	t.Cleanup(proxy.Close)
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	return conn
}
func logByResponse(t *testing.T, s *store.Store, id string) store.RequestLog {
	t.Helper()
	for _, session := range s.SessionsSnapshot() {
		for _, log := range session.Logs {
			if log.Correlation.ResponseID == id {
				return log
			}
		}
	}
	t.Fatalf("response log %s missing", id)
	return store.RequestLog{}
}

func TestWebSocketLanesFIFOBytesAndBranchEvidence(t *testing.T) {
	requests := []string{wsCreate("A", "first", ""), wsCreate("A", "second", ""), wsCreate("B", "other", ""), wsCreate("C", "branch", "resp-a1")}
	frames := []string{
		wsEvent("response.created", "A", "resp-a1", ""), wsEvent("response.created", "B", "resp-b", ""),
		`{ "type":"response.output_text.delta", "stream_id":"A", "delta":"first answer" }`,
		wsEvent("response.completed", "B", "resp-b", "other answer"), wsEvent("response.completed", "A", "resp-a1", "first answer"),
		wsEvent("response.created", "A", "resp-a2", ""), wsEvent("response.completed", "A", "resp-a2", "second answer"),
		wsEvent("response.created", "C", "resp-c", ""), wsEvent("response.completed", "C", "resp-c", "branch answer"),
	}
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ws-secret" {
			t.Error("WebSocket auth lost")
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for i := 0; i < 3; i++ {
			_, raw, err := conn.ReadMessage()
			if err != nil || string(raw) != requests[i] {
				t.Errorf("request frame changed: %s %v", raw, err)
				return
			}
		}
		for _, frame := range frames[:7] {
			if conn.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
				return
			}
		}
		_, raw, err := conn.ReadMessage()
		if err != nil || string(raw) != requests[3] {
			t.Errorf("branch frame changed: %s %v", raw, err)
			return
		}
		for _, frame := range frames[7:] {
			if conn.WriteMessage(websocket.TextMessage, []byte(frame)) != nil {
				return
			}
		}
		_, _, _ = conn.ReadMessage()
	}), 2048)
	client := connectTestWS(t, proxy, http.Header{"Authorization": {"Bearer ws-secret"}})
	for _, body := range requests[:3] {
		if err := client.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for i, frame := range frames {
		if i == 7 {
			if err := client.WriteMessage(websocket.TextMessage, []byte(requests[3])); err != nil {
				t.Fatal(err)
			}
		}
		_, raw, err := client.ReadMessage()
		if err != nil || string(raw) != frame {
			t.Fatalf("response frame changed: %s %v", raw, err)
		}
	}
	a1, a2, b, branch := logByResponse(t, storage, "resp-a1"), logByResponse(t, storage, "resp-a2"), logByResponse(t, storage, "resp-b"), logByResponse(t, storage, "resp-c")
	if a1.Status != "done" || a1.ResponseBody != "first answer" || a2.ResponseBody != "second answer" || b.ResponseBody != "other answer" || a1.WebSocket.Frames != 3 || a1.TokenSources.Output != "usage" || a1.TTFC == nil {
		t.Fatalf("lane metrics wrong: %+v %+v %+v", a1, a2, b)
	}
	if a2.Correlation.ParentTraceID != "" || b.Correlation.ParentTraceID != "" || a1.SessionID == a2.SessionID || branch.Correlation.ParentTraceID != a1.TraceID {
		t.Fatalf("lane mistaken for lineage: %+v %+v", a2.Correlation, branch.Correlation)
	}
	detail := responseDetail(t, storage, "/api/responses/"+a1.TraceID)
	if detail.Type != "websocket" || detail.Representation != "websocket-frames" || !detail.Complete {
		t.Fatalf("frame storage metadata: %+v", detail)
	}
	lines := strings.Split(strings.TrimSpace(detail.Body), "\n")
	if len(lines) != 3 {
		t.Fatal(detail.Body)
	}
	for i, index := range []int{0, 2, 4} {
		var envelope struct{ Type, Data string }
		if json.Unmarshal([]byte(lines[i]), &envelope) != nil || envelope.Data != frames[index] {
			t.Fatal("stored frame lost original whitespace")
		}
	}
	raw, _ := json.Marshal(storage.SessionsSnapshot())
	if strings.Contains(string(raw), "ws-secret") {
		t.Fatal("WebSocket credentials leaked")
	}
}

func TestWebSocketRoutesFirstFrameWithTLSAndPrefix(t *testing.T) {
	t.Setenv("GATEWAY_WS_KEY", "ws-provider-key")
	received := make(chan string, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mounted/v1/responses" || r.Header.Get("Authorization") != "Bearer ws-provider-key" {
			t.Errorf("handshake path/auth: %s", r.URL.Path)
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		received <- string(body)
		_ = conn.WriteMessage(websocket.TextMessage, []byte(wsEvent("response.completed", "tls", "tls-response", "verified")))
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	proxy, storage := testProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("used default target instead of first frame route") }), 2048)
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	proxy.transport.TLSClientConfig.RootCAs = pool
	_ = proxy.providers.Set(provider.Config{Providers: []provider.Provider{{ID: "tls", BaseURL: upstream.URL + "/mounted/v1", Protocol: "passthrough", KeyEnv: "GATEWAY_WS_KEY", WebSocket: true}}, Routes: []provider.Route{{ID: "ws-route", Model: "mock", TargetModel: "actual", ProviderID: "tls"}}})
	client := connectTestWS(t, proxy, nil)
	if err := client.WriteMessage(websocket.TextMessage, []byte(wsCreate("tls", "go", ""))); err != nil {
		t.Fatal(err)
	}
	_, _, err := client.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case body := <-received:
		if !strings.Contains(body, `"model":"actual"`) {
			t.Fatal("model alias missing")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive frame")
	}
	log := logByResponse(t, storage, "tls-response")
	if log.Route.ProviderID != "tls" || log.Status != "done" {
		t.Fatalf("route missing: %+v", log)
	}
	client.Close()
	proxy.Close()
}

func TestWebSocketCancellationAndDisconnectAreRecorded(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "cancel-frame"
		if disconnect {
			name = "disconnect"
		}
		t.Run(name, func(t *testing.T) {
			const cancellation = `{"type":"response.cancel","stream_id":"A"}`
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upgrader := websocket.Upgrader{}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				_, _, err = conn.ReadMessage()
				if err != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(wsEvent("response.created", "A", "pending", "")))
				_, raw, err := conn.ReadMessage()
				if disconnect {
					return
				}
				if err != nil || string(raw) != cancellation {
					t.Errorf("cancellation changed: %s %v", raw, err)
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.incomplete","stream_id":"A","response":{"id":"pending","status":"incomplete","incomplete_details":{"reason":"cancelled"}}}`))
				_, _, _ = conn.ReadMessage()
			}), 1024)
			client := connectTestWS(t, proxy, nil)
			_ = client.WriteMessage(websocket.TextMessage, []byte(wsCreate("A", "wait", "")))
			if _, _, err := client.ReadMessage(); err != nil {
				t.Fatal(err)
			}
			if disconnect {
				client.Close()
			} else {
				_ = client.WriteMessage(websocket.TextMessage, []byte(cancellation))
				if _, _, err := client.ReadMessage(); err != nil {
					t.Fatal(err)
				}
			}
			deadline := time.Now().Add(3 * time.Second)
			for {
				log := logByResponse(t, storage, "pending")
				if log.Status == "canceled" {
					if log.TTFC != nil || log.TokenSources.Output != "unknown" || log.Error == "" {
						t.Fatalf("cancellation metrics: %+v", log)
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("call still running: %+v", log)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestProviderHistoryDoesNotInventExecutions(t *testing.T) {
	t.Setenv("GATEWAY_HISTORY_KEY", "history-key")
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/base/v1/responses/resp_saved" || r.Header.Get("Authorization") != "Bearer history-key" {
			t.Errorf("history request wrong: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_saved","object":"response","output":[]}`))
	}), 1024)
	_ = proxy.providers.Set(provider.Config{Providers: []provider.Provider{{ID: "saved", BaseURL: proxy.target.String() + "/base/v1", Protocol: "passthrough", KeyEnv: "GATEWAY_HISTORY_KEY", History: true}}})
	status, body := api(t, proxy.ProviderHistoryHandler, "POST", "/api/provider-history", map[string]any{"provider_id": "saved", "response_id": "resp_saved"})
	if status != 200 || !strings.Contains(string(body), "resp_saved") || len(storage.SessionsSnapshot()) != 0 {
		t.Fatalf("history created model call: %d %s", status, body)
	}
	status, _ = api(t, proxy.ProviderHistoryHandler, "POST", "/api/provider-history", map[string]any{"provider_id": "saved", "response_id": "../escape"})
	if status != 400 {
		t.Fatal("invalid history ID accepted")
	}
}
