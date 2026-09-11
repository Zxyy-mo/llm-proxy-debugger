package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

var privateEmailToken = regexp.MustCompile(`\[PRIVATE_email_[a-f0-9]{20}\]`)

func firstPrivateToken(t *testing.T, text string) string {
	t.Helper()
	token := privateEmailToken.FindString(text)
	if token == "" {
		t.Fatalf("no private email token in %q", text)
	}
	return token
}

func requestStartLogs(t *testing.T, events *hub.Hub) map[string]store.RequestLog {
	t.Helper()
	logs := map[string]store.RequestLog{}
	for {
		select {
		case event := <-events.Broadcast:
			raw, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(raw, "event").String() == "request_start" {
				var log store.RequestLog
				if err := json.Unmarshal([]byte(gjson.GetBytes(raw, "log").Raw), &log); err != nil {
					t.Fatal(err)
				}
				logs[log.TraceID] = log
			}
		default:
			return logs
		}
	}
}

func TestHTTPPrivacyKeepsRawClientHistoryAssociated(t *testing.T) {
	for _, retain := range []bool{true, false} {
		t.Run(fmt.Sprintf("retain=%t", retain), func(t *testing.T) {
			received := make(chan string, 3)
			var hits atomic.Int32
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				received <- string(body)
				answer := "reply " + gjson.GetBytes(body, "messages.0.content").String()
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprintf("private-%d", hits.Add(1)), "choices": []any{map[string]any{"message": map[string]string{"role": "assistant", "content": answer}}}})
			}), 4096)
			proxy.hub = hub.New(zap.NewNop())
			policy := storage.Privacy.Policy()
			policy.Record, policy.Outbound, policy.RetainRaw = true, true, retain
			if err := storage.Privacy.Set(policy); err != nil {
				t.Fatal(err)
			}
			const request = `{"messages":[{"role":"user","content":"raw-client@example.com"}]}`
			firstTrace, firstReply := send(t, proxy, "POST", "/v1/chat/completions", request, nil)
			firstWire := <-received
			firstToken := firstPrivateToken(t, firstWire)
			if strings.Contains(firstWire, "raw-client@example.com") {
				t.Fatal("outbound privacy did not substitute")
			}
			answer := gjson.Get(firstReply.Body.String(), "choices.0.message.content").String()
			if answer != "reply "+firstToken {
				t.Fatal("response forwarding changed the actual assistant reply")
			}
			body, _ := json.Marshal(map[string]any{"messages": []any{
				map[string]string{"role": "user", "content": "raw-client@example.com"},
				map[string]string{"role": "assistant", "content": answer},
				map[string]string{"role": "user", "content": "raw-client@example.com again"},
			}})
			childTrace, reply := send(t, proxy, "POST", "/v1/chat/completions", string(body), nil)
			childWire := <-received
			if reply.Code != 200 || firstPrivateToken(t, childWire) != firstToken {
				t.Fatal("same conversation changed outbound placeholders")
			}
			first, child := logOf(t, storage, firstTrace), logOf(t, storage, childTrace)
			if child.Correlation.ParentTraceID != firstTrace || child.SessionID != first.SessionID {
				t.Fatalf("raw client history lost association with privacy enabled: %+v", child.Correlation)
			}
			otherTrace, _ := send(t, proxy, "POST", "/v1/chat/completions", request, nil)
			otherWire := <-received
			if firstPrivateToken(t, otherWire) == firstToken || logOf(t, storage, otherTrace).SessionID == first.SessionID {
				t.Fatal("unrelated identical prompts shared conversation privacy")
			}
			starts := requestStartLogs(t, proxy.hub)
			for _, trace := range []string{firstTrace, childTrace} {
				if firstPrivateToken(t, starts[trace].RequestBody) != firstToken {
					t.Fatal("request_start projected before its conversation was resolved")
				}
				code, capture := api(t, proxy.RequestsHandler, "GET", "/api/requests/"+trace, nil)
				if code != 200 || firstPrivateToken(t, string(capture)) != firstToken || strings.Contains(string(capture), "raw-client@example.com") {
					t.Fatalf("request capture differs from admitted privacy namespace: %s", capture)
				}
			}
		})
	}
}

func TestPrivateOriginalAndOutgoingReplayKeepCaptureIdentity(t *testing.T) {
	received := make(chan string, 4)
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- string(body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}), 4096)
	policy := storage.Privacy.Policy()
	policy.Record, policy.Outbound, policy.AllowReveal = true, true, true
	if err := storage.Privacy.Set(policy); err != nil {
		t.Fatal(err)
	}
	const request = `{"messages":[{"role":"user","content":"replay-private@example.com"}]}`
	trace, _ := send(t, proxy, "POST", "/v1/chat/completions", request, map[string]string{"Authorization": "Bearer original-identity"})
	wire := <-received
	token := firstPrivateToken(t, wire)
	for _, source := range []string{"original", "outgoing"} {
		record := createReplay(t, proxy, map[string]any{
			"trace_id": trace, "source": source, "idempotency_key": "privacy-" + source,
			"credentials": []any{map[string]string{"kind": "header", "name": "Authorization", "value": "Bearer original-identity"}},
		}, 202)
		record = finishedReplay(t, proxy, record.ID)
		replayed := <-received
		if record.State != "done" || firstPrivateToken(t, replayed) != token || (source == "outgoing" && replayed != wire) {
			t.Fatalf("%s replay changed captured privacy bytes: %s", source, replayed)
		}
		if firstPrivateToken(t, logOf(t, storage, record.TraceID).RequestBody) != token {
			t.Fatal("replay preview changed source token")
		}
	}
	changed := createReplay(t, proxy, map[string]any{
		"trace_id": trace, "source": "original", "idempotency_key": "privacy-other-identity",
		"credentials": []any{map[string]string{"kind": "header", "name": "Authorization", "value": "Bearer other-identity"}},
	}, 202)
	changed = finishedReplay(t, proxy, changed.ID)
	if changed.State != "done" || firstPrivateToken(t, <-received) == token {
		t.Fatal("changed-credential replay reused source namespace")
	}
	code, restored := api(t, storage.PrivacyHandler, "POST", "/api/privacy/restore", map[string]string{"trace_id": changed.TraceID, "body": token})
	if code != 200 || gjson.GetBytes(restored, "body").String() != token {
		t.Fatal("replay provenance granted cross-identity reveal")
	}
}

func TestPrivateWebSocketAdmissionAndResponseUseOneScope(t *testing.T) {
	secret := strings.Repeat("private-fragment", 12) + "@example.com"
	request := wsCreate("lane", secret, "")
	var count atomic.Int32
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if string(body) != request {
				t.Error("recording-only privacy changed client frame")
			}
			id := fmt.Sprintf("private-ws-%d", count.Add(1))
			if err := conn.WriteMessage(websocket.TextMessage, []byte(wsEvent("response.completed", "lane", id, secret))); err != nil {
				return
			}
		}
	}), 64)
	proxy.hub = hub.New(zap.NewNop())
	policy := storage.Privacy.Policy()
	policy.Record, policy.RetainRaw = true, false
	if err := storage.Privacy.Set(policy); err != nil {
		t.Fatal(err)
	}
	firstClient := connectTestWS(t, proxy, http.Header{"X-Session-Id": {"private-session"}})
	otherClient := connectTestWS(t, proxy, http.Header{"X-Session-Id": {"other-session"}})
	logs := []store.RequestLog{}
	for i, client := range []*websocket.Conn{firstClient, firstClient, otherClient} {
		if err := client.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			t.Fatal(err)
		}
		_, body, err := client.ReadMessage()
		id := fmt.Sprintf("private-ws-%d", i+1)
		if err != nil || string(body) != wsEvent("response.completed", "lane", id, secret) {
			t.Fatalf("recording privacy changed upstream frame: %s %v", body, err)
		}
		logs = append(logs, logByResponse(t, storage, id))
	}
	firstClient.Close()
	otherClient.Close()
	proxy.Close()
	starts := requestStartLogs(t, proxy.hub)
	tokens := []string{}
	for _, log := range logs {
		start := starts[log.TraceID]
		token := firstPrivateToken(t, start.RequestBody)
		tokens = append(tokens, token)
		if firstPrivateToken(t, log.ResponseBody) != token || strings.Contains(start.Summary, "private-fragment") {
			t.Fatal("WebSocket preview and response used different privacy identity")
		}
		detail := responseDetail(t, storage, "/api/responses/"+log.TraceID)
		if firstPrivateToken(t, detail.Body) != token || detail.Representation != "redacted-output" {
			t.Fatal("WebSocket recorder used credential scope")
		}
	}
	if tokens[0] != tokens[1] || tokens[0] == tokens[2] {
		t.Fatal("WebSocket conversation isolation is inconsistent")
	}
	if err := filepath.WalkDir(proxy.cfg.LogDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(body), "private-fragment") {
			t.Errorf("raw private fragment retained in %s", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(storage.Privacy.Snapshot().Values) != 0 {
		t.Fatal("no-retain WebSocket saved restoration mappings")
	}
}

func TestProviderHistoryPrivacyHasNoRetainedConversationMapping(t *testing.T) {
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"output":"history-only@example.com"}`)
	}), 4096)
	if err := proxy.providers.Set(provider.Config{Providers: []provider.Provider{{ID: "history", BaseURL: proxy.target.String(), Protocol: "passthrough", History: true}}}); err != nil {
		t.Fatal(err)
	}
	policy := storage.Privacy.Policy()
	policy.Record, policy.AllowReveal = true, true
	if err := storage.Privacy.Set(policy); err != nil {
		t.Fatal(err)
	}
	tokens := []string{}
	for _, id := range []string{"first-response", "first-response", "other-response"} {
		code, body := api(t, proxy.ProviderHistoryHandler, "POST", "/api/provider-history", map[string]string{"provider_id": "history", "response_id": id, "api_key": "history-identity"})
		if code != 200 || strings.Contains(string(body), "history-only@example.com") {
			t.Fatalf("history read failed or leaked text: %s", body)
		}
		tokens = append(tokens, firstPrivateToken(t, string(body)))
	}
	if tokens[0] != tokens[1] || tokens[0] == tokens[2] || len(storage.Privacy.Snapshot().Values) != 0 || len(storage.SessionsSnapshot()) != 0 || !storage.Privacy.Policy().RetainRaw {
		t.Fatal("provider-history reads retained a broad namespace or changed the global policy")
	}
}
