package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
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

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/config"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/export"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/replay"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"go.uber.org/zap"
)

type upstreamHit struct {
	method, path, query, auth, key, accept, encoding string
	body                                             []byte
}

// echoUpstream records every request and answers with a Responses JSON body.
func echoUpstream(t *testing.T) (http.Handler, func() []upstreamHit) {
	t.Helper()
	var mu sync.Mutex
	var hits []upstreamHit
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, upstreamHit{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"),
			key: r.Header.Get("X-Api-Key"), accept: r.Header.Get("Accept"), encoding: r.Header.Get("Content-Encoding"), body: body,
		})
		count := len(hits)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp-%d","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok %d"}]}],"usage":{"input_tokens":3,"output_tokens":2}}`, count, count)
	})
	return handler, func() []upstreamHit {
		mu.Lock()
		defer mu.Unlock()
		return append([]upstreamHit(nil), hits...)
	}
}

func testProxyWithTarget(t *testing.T, upstream http.Handler, targetPath string) (*Server, *store.Store) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	h := hub.New(zap.NewNop())
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case event := <-h.Broadcast:
				json.Marshal(event)
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-finished })
	s := store.New()
	proxy, err := NewServer(&config.Config{TargetAddr: server.URL + targetPath, MaxBodyLogSize: 64, LogDir: t.TempDir(), ListenAddr: "0.0.0.0:12337"}, zap.NewNop(), h, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.transport.CloseIdleConnections)
	return proxy, s
}

func send(t *testing.T, proxy *Server, method, target, body string, headers map[string]string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	proxy.ServeHTTP(response, request)
	return response.Header().Get("X-Gateway-Trace-ID"), response
}

func api(t *testing.T, handler http.HandlerFunc, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	response := httptest.NewRecorder()
	handler(response, httptest.NewRequest(method, path, reader))
	return response.Code, response.Body.Bytes()
}

func createReplay(t *testing.T, proxy *Server, body map[string]any, status int) replay.Record {
	t.Helper()
	code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", body)
	if code != status {
		t.Fatalf("POST /api/replays: got %d, want %d: %s", code, status, raw)
	}
	var record replay.Record
	if code == http.StatusAccepted || code == http.StatusOK {
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
	}
	return record
}

func finishedReplay(t *testing.T, proxy *Server, id string) replay.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	record, err := proxy.replays.Wait(ctx, id)
	if err != nil || record.State == "running" {
		t.Fatalf("replay did not finish: %+v %v", record, err)
	}
	return record
}

func logOf(t *testing.T, s *store.Store, traceID string) store.RequestLog {
	t.Helper()
	log, ok := s.Log(traceID)
	if !ok {
		t.Fatalf("no log for %s", traceID)
	}
	return log
}

func TestCurlExportAPIAndBodyDownload(t *testing.T) {
	upstream, hits := echoUpstream(t)
	proxy, storage := testProxyWithTarget(t, upstream, "")
	storage.Rules = []store.Rule{{ID: "inject", PathMatch: "/messages", InjectSystem: "INJECTED"}}
	body := `{"model":"mock","max_tokens":5,"messages":[{"role":"user","content":"it's $HOME 你好 9007199254740993"}]}`
	trace, response := send(t, proxy, http.MethodPost, "http://gateway.test:12337/v1/messages?v=1&key=query-secret", body, map[string]string{
		"Authorization": "Bearer original-secret", "X-API-Key": "raw-secret", "Content-Type": "application/json", "Accept": "application/json",
	})
	if response.Code != 200 || len(hits()) != 1 {
		t.Fatalf("setup request failed: %d", response.Code)
	}
	if got := hits()[0]; got.query != "v=1&key=query-secret" || !strings.Contains(string(got.body), "INJECTED") {
		t.Fatalf("setup upstream request unexpected: %+v", got)
	}
	log := logOf(t, storage, trace)
	if log.Query != "v=1&key=****" {
		t.Fatalf("display query not redacted: %q", log.Query)
	}
	capture, _ := storage.RequestSnapshot(trace)
	if strings.Contains(capture.Original.URL, "query-secret") || strings.Contains(capture.Outgoing.URL, "query-secret") {
		t.Fatal("display URLs leaked the query secret")
	}
	encoded, _ := json.Marshal(capture)
	for _, secret := range []string{"original-secret", "raw-secret", "query-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("capture API leaked %q: %s", secret, encoded)
		}
	}
	if len(capture.Original.Credentials) != 3 || len(capture.Outgoing.Credentials) != 3 {
		t.Fatalf("credential names not recorded: %+v %+v", capture.Original.Credentials, capture.Outgoing.Credentials)
	}

	code, raw := api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+trace+"/curl", nil)
	var outgoing export.Export
	if code != 200 || json.Unmarshal(raw, &outgoing) != nil {
		t.Fatalf("default export failed: %d %s", code, raw)
	}
	if outgoing.Source != "outgoing" || outgoing.Destination.Kind != "upstream" || !strings.HasPrefix(outgoing.Destination.URL, "http://127.0.0.1:") || !strings.HasSuffix(outgoing.Destination.URL, "/v1/messages?v=1") {
		t.Fatalf("outgoing destination wrong: %+v", outgoing.Destination)
	}
	if !strings.Contains(outgoing.Command, "INJECTED") || strings.Contains(outgoing.Command, "-secret") || len(outgoing.Environment) != 3 {
		t.Fatalf("outgoing command unexpected: %s", outgoing.Command)
	}
	code, raw = api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+trace+"/curl?source=original", nil)
	var original export.Export
	if code != 200 || json.Unmarshal(raw, &original) != nil {
		t.Fatalf("original export failed: %d %s", code, raw)
	}
	if original.Destination.Kind != "gateway" || original.Destination.URL != "http://gateway.test:12337/v1/messages?v=1" || strings.Contains(original.Command, "INJECTED") {
		t.Fatalf("original export must target the gateway without injected content: %+v", original)
	}
	code, raw = api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+trace+"/body?source=original", nil)
	if code != 200 || string(raw) != body {
		t.Fatalf("original body download changed bytes: %d %s", code, raw)
	}
	code, raw = api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+trace+"/body", nil)
	if code != 200 || !bytes.Equal(raw, hits()[0].body) {
		t.Fatal("outgoing body download differs from what the upstream received")
	}
	for path, want := range map[string]int{
		"/api/requests/" + trace + "/curl?source=bogus": 400, "/api/requests/missing/curl": 404,
		"/api/requests/" + trace + "/unknown": 404, "/api/requests/" + trace: 200,
	} {
		if code, raw := api(t, proxy.RequestsHandler, http.MethodGet, path, nil); code != want {
			t.Fatalf("%s: got %d want %d: %s", path, code, want, raw)
		}
	}
	if code, _ := api(t, proxy.RequestsHandler, http.MethodPost, "/api/requests/"+trace+"/curl", nil); code != 405 {
		t.Fatal("export must be read-only")
	}

	// A request that never reached the upstream has no outgoing snapshot.
	storage.Rules = []store.Rule{{ID: "pause", PathMatch: "/responses", Intercept: true, WaitSeconds: 30}}
	pending, _, done, _ := startPending(t, proxy, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"mock","input":"never"}`)))
	control(t, proxy, http.MethodPost, "/api/interceptions/"+pending.TraceID+"/cancel", intercept.Edit{Revision: 1}, 200)
	waitRequest(t, done)
	if code, _ := api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+pending.TraceID+"/curl?source=outgoing", nil); code != 404 {
		t.Fatal("outgoing export of an unforwarded request must be explicit")
	}
	code, raw = api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+pending.TraceID+"/curl", nil)
	var fallback export.Export
	if code != 200 || json.Unmarshal(raw, &fallback) != nil || fallback.Source != "original" {
		t.Fatalf("default source must fall back to original: %d %s", code, raw)
	}
}

func TestReplayOutgoingSkipsRulesAndRecordsProvenance(t *testing.T) {
	upstream, hits := echoUpstream(t)
	proxy, storage := testProxyWithTarget(t, upstream, "")
	storage.Rules = []store.Rule{{ID: "pause", PathMatch: "/messages", InjectSystem: "INJECTED", Intercept: true, WaitSeconds: 30, TimeoutAction: "forward"}}
	body := `{"model":"mock","max_tokens":5,"messages":[{"role":"user","content":"first"}]}`
	pending, _, done, _ := startPending(t, proxy, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
	control(t, proxy, http.MethodPost, "/api/interceptions/"+pending.TraceID+"/release", intercept.Edit{Revision: 1}, 200)
	waitRequest(t, done)
	source := pending.TraceID
	first := hits()
	if len(first) != 1 || strings.Count(string(first[0].body), "INJECTED") != 1 {
		t.Fatalf("setup did not inject once: %+v", first)
	}

	record := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "outgoing-1"}, http.StatusAccepted)
	if record.Source != "outgoing" || record.Of != source || record.TraceID == source || record.Modified {
		t.Fatalf("record metadata wrong: %+v", record)
	}
	if len(proxy.interceptions.Pending()) != 0 {
		t.Fatal("replaying an outgoing snapshot must not enter the breakpoint again")
	}
	record = finishedReplay(t, proxy, record.ID)
	all := hits()
	if record.State != "done" || record.StatusCode != 200 || len(all) != 2 || !bytes.Equal(all[1].body, all[0].body) || all[1].path != "/v1/messages" {
		t.Fatalf("outgoing replay must resend identical bytes once: %+v %+v", record, all)
	}
	log := logOf(t, storage, record.TraceID)
	sourceLog := logOf(t, storage, source)
	if log.Replay == nil || log.Replay.Of != source || log.Replay.Source != "outgoing" || log.Replay.ID != record.ID || log.Status != "done" || log.ClientIP != "replay" {
		t.Fatalf("replay provenance missing from the log: %+v", log)
	}
	if log.SessionID != sourceLog.SessionID || log.Correlation.SessionSource != "replay" || log.Correlation.ParentTraceID != "" {
		t.Fatalf("replay must join the source session without inventing a parent: %+v", log.Correlation)
	}
	if log.Interception != nil {
		t.Fatal("replay must not inherit the source interception state")
	}
	graph, _ := storage.GraphSnapshot(sourceLog.SessionID)
	var replayEdges, parentEdges int
	for _, edge := range graph.Edges {
		if edge.Kind == "replay" && edge.Source == source && edge.Target == record.TraceID {
			replayEdges++
		} else {
			parentEdges++
		}
	}
	if replayEdges != 1 || parentEdges != 0 {
		t.Fatalf("graph should show exactly one replay edge: %+v", graph.Edges)
	}
	for _, node := range graph.Nodes {
		if node.TraceID == record.TraceID && (node.Replay == nil || node.Replay.Of != source) {
			t.Fatal("graph node lacks replay provenance")
		}
	}
	sourceCapture, _ := storage.RequestSnapshot(source)
	if sourceCapture.Original.Body != body || !bytes.Equal([]byte(sourceCapture.Outgoing.Body), all[0].body) {
		t.Fatal("source captures changed after replay")
	}
	replayCapture, _ := storage.RequestSnapshot(record.TraceID)
	if replayCapture.Original.Body != sourceCapture.Outgoing.Body || replayCapture.Outgoing == nil || replayCapture.Outgoing.Body != sourceCapture.Outgoing.Body {
		t.Fatal("replay captures must reflect the replayed snapshot")
	}
	sessions := storage.SessionsSnapshot()
	if len(sessions) != 1 || len(sessions[sourceLog.SessionID].Logs) != 2 {
		t.Fatalf("replay should appear next to its source: %d sessions", len(sessions))
	}

	// Replaying the client original goes through the pipeline like a new client
	// request: rules apply again and the breakpoint pauses it.
	storage.Rules = []store.Rule{{ID: "inject", PathMatch: "/messages", InjectSystem: "INJECTED"}}
	record = createReplay(t, proxy, map[string]any{"trace_id": source, "source": "original", "idempotency_key": "original-1"}, http.StatusAccepted)
	record = finishedReplay(t, proxy, record.ID)
	all = hits()
	if record.State != "done" || len(all) != 3 || strings.Count(string(all[2].body), "INJECTED") != 1 || !bytes.Equal(all[2].body, all[0].body) {
		t.Fatalf("original replay must apply rules exactly once: %+v", all)
	}
	log = logOf(t, storage, record.TraceID)
	if log.Replay == nil || log.Replay.Source != "original" {
		t.Fatalf("original replay provenance missing: %+v", log.Replay)
	}
	code, raw := api(t, proxy.ReplaysHandler, http.MethodGet, "/api/replays", nil)
	var records []replay.Record
	if code != 200 || json.Unmarshal(raw, &records) != nil || len(records) != 2 {
		t.Fatalf("replay listing wrong: %d %s", code, raw)
	}
}

func TestReplayIdempotencyAndTransientCredentials(t *testing.T) {
	upstream, hits := echoUpstream(t)
	proxy, storage := testProxyWithTarget(t, upstream, "")
	body := `{"model":"mock","input":"hello"}`
	source, _ := send(t, proxy, http.MethodPost, "/v1/responses?key=query-secret", body, map[string]string{"Authorization": "Bearer original-secret", "Content-Type": "application/json"})
	if len(hits()) != 1 {
		t.Fatal("setup request did not reach upstream")
	}
	code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source, "idempotency_key": "k1"})
	if code != 422 || !strings.Contains(string(raw), "header Authorization") || !strings.Contains(string(raw), "query key") {
		t.Fatalf("missing credentials must be listed: %d %s", code, raw)
	}
	if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source}); code != 400 || !strings.Contains(string(raw), "idempotency_key") {
		t.Fatalf("idempotency key must be required: %d %s", code, raw)
	}
	credentials := []map[string]string{
		{"kind": "header", "name": "Authorization", "value": "Bearer replay-secret"},
		{"kind": "query", "name": "key", "value": "query-replay"},
	}
	for _, bad := range [][]map[string]string{
		{{"kind": "header", "name": "Authorization", "value": "Bearer ori****cret"}},
		{{"kind": "header", "name": "Accept", "value": "application/json"}},
		{{"kind": "header", "name": "Authorization", "value": "Bearer a\r\nInjected: yes"}},
		{{"kind": "cookie", "name": "session", "value": "x"}},
		{{"kind": "query", "name": "model", "value": "x"}},
	} {
		if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source, "idempotency_key": "bad", "credentials": bad}); code != 422 {
			t.Fatalf("invalid credential accepted: %d %s (%v)", code, raw, bad)
		}
	}
	request := map[string]any{"trace_id": source, "idempotency_key": "k1", "credentials": credentials}
	record := createReplay(t, proxy, request, http.StatusAccepted)
	record = finishedReplay(t, proxy, record.ID)
	all := hits()
	if record.State != "done" || len(all) != 2 || all[1].auth != "Bearer replay-secret" || all[1].query != "key=query-replay" || !bytes.Equal(all[1].body, []byte(body)) {
		t.Fatalf("replay did not use transient credentials: %+v %+v", record, all)
	}
	// The same action retried returns the same record without another execution.
	again := createReplay(t, proxy, request, http.StatusOK)
	if again.ID != record.ID || len(hits()) != 2 {
		t.Fatal("retry with the same key executed twice")
	}
	edited := `{"model":"mock","input":"changed"}`
	if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source, "idempotency_key": "k1", "body": edited, "credentials": credentials}); code != 409 {
		t.Fatalf("reused key with different content must conflict: %d %s", code, raw)
	}
	second := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "k2", "credentials": credentials}, http.StatusAccepted)
	second = finishedReplay(t, proxy, second.ID)
	if second.ID == record.ID || second.TraceID == record.TraceID || len(hits()) != 3 {
		t.Fatal("a new key must execute a new replay")
	}

	// Nothing gateway-generated may retain the supplied secrets.
	for name, value := range map[string]any{
		"sessions": storage.SessionsSnapshot(), "replays": proxy.replays.List(),
		"capture": func() any { c, _ := storage.RequestSnapshot(record.TraceID); return c }(),
		"graph":   func() any { g, _ := storage.GraphSnapshot(""); return g }(),
	} {
		encoded, _ := json.Marshal(value)
		if strings.Contains(string(encoded), "replay-secret") || strings.Contains(string(encoded), "query-replay") || strings.Contains(string(encoded), "original-secret") {
			t.Fatalf("%s leaked a credential: %s", name, encoded)
		}
	}
	log := logOf(t, storage, record.TraceID)
	if log.Query != "key=****" || !strings.Contains(log.Headers["Authorization"], "****") {
		t.Fatalf("replay log did not redact credentials: %+v", log)
	}
	code, raw = api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+record.TraceID+"/curl", nil)
	if code != 200 || strings.Contains(string(raw), "replay-secret") || !strings.Contains(string(raw), "REPLAY_AUTHORIZATION") {
		t.Fatalf("replay export must also use placeholders: %d %s", code, raw)
	}
}

func TestReplayEditsAreValidatedAndSourceStaysImmutable(t *testing.T) {
	upstream, hits := echoUpstream(t)
	proxy, storage := testProxyWithTarget(t, upstream, "")
	body := `{"model":"mock","session_id":"s-1","messages":[{"role":"user","content":"hello"}]}`
	source, _ := send(t, proxy, http.MethodPost, "/v1/chat/completions", body, map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream"})
	for _, invalid := range []map[string]any{
		{"body": `{"model":`}, {"body": `[]`}, {"body": `{"model":"mock","messages":"bad"}`},
		{"body": `{"model":"mock","session_id":"changed","messages":[{"role":"user","content":"hello"}]}`},
		{"headers": map[string]string{"Authorization": "x"}}, {"headers": map[string]string{"Accept": "a\r\nb"}},
		{"source": "sideways"}, {"timeout_seconds": 100000},
	} {
		payload := map[string]any{"trace_id": source, "idempotency_key": "invalid"}
		for key, value := range invalid {
			payload[key] = value
		}
		if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", payload); code != 422 && code != 400 {
			t.Fatalf("invalid edit accepted: %d %s (%v)", code, raw, invalid)
		}
	}
	if len(hits()) != 1 || len(proxy.replays.List()) != 0 {
		t.Fatal("rejected replays must not execute or leave records")
	}
	edited := `{"model":"mock","session_id":"s-1","messages":[{"role":"user","content":"edited"}]}`
	code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays/validate", map[string]any{"trace_id": source, "body": edited, "headers": map[string]string{"Accept": "application/json"}})
	var validation struct {
		Valid    bool     `json:"valid"`
		Modified bool     `json:"modified"`
		Missing  []string `json:"missing_credentials"`
	}
	if code != 200 || json.Unmarshal(raw, &validation) != nil || !validation.Valid || !validation.Modified || len(validation.Missing) != 0 {
		t.Fatalf("validation response wrong: %d %s", code, raw)
	}
	record := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "edit", "body": edited, "headers": map[string]string{"Accept": "application/json"}}, http.StatusAccepted)
	record = finishedReplay(t, proxy, record.ID)
	all := hits()
	if record.State != "done" || !record.Modified || len(all) != 2 || string(all[1].body) != edited || all[1].accept != "application/json" {
		t.Fatalf("edited replay not sent as edited: %+v %+v", record, all)
	}
	sourceCapture, _ := storage.RequestSnapshot(source)
	replayCapture, _ := storage.RequestSnapshot(record.TraceID)
	if sourceCapture.Original.Body != body || sourceCapture.Outgoing.Body != body || replayCapture.Original.Body != edited {
		t.Fatal("source record changed or replay capture wrong")
	}
	sourceLog, replayLog := logOf(t, storage, source), logOf(t, storage, record.TraceID)
	if sourceLog.Replay != nil || sourceLog.Status != "done" || replayLog.Summary != "edited" || replayLog.SessionID != sourceLog.SessionID {
		t.Fatalf("source/replay logs wrong: %+v %+v", sourceLog, replayLog)
	}
}

func TestReplayCancelTimeoutAndUpstreamFailure(t *testing.T) {
	// Replays resend headers faithfully, so the fixture keys its behaviour off
	// the (editable) body instead of call order or custom headers.
	var inflight, slowStarted atomic.Int32
	proxy, storage := testProxyWithTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("fail")):
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		case bytes.Contains(body, []byte("slow")):
			slowStarted.Add(1)
			inflight.Add(1)
			defer inflight.Add(-1)
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"resp-1","output":[]}`)
		}
	}), "")
	source, _ := send(t, proxy, http.MethodPost, "/v1/responses", `{"model":"mock","input":"fast"}`, map[string]string{"Content-Type": "application/json"})
	if logOf(t, storage, source).Status != "done" {
		t.Fatal("setup request should have succeeded")
	}
	slow := `{"model":"mock","input":"slow"}`

	timeout := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "timeout", "timeout_seconds": 1, "body": slow}, http.StatusAccepted)
	timeout = finishedReplay(t, proxy, timeout.ID)
	log := logOf(t, storage, timeout.TraceID)
	if timeout.State != "error" || timeout.Reason != "timeout" || !timeout.Modified || log.Status != "error" || log.StatusCode != 504 || !strings.Contains(log.Error, "timed out") {
		t.Fatalf("timeout not reported: %+v %+v", timeout, log)
	}

	canceled := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "cancel", "body": slow}, http.StatusAccepted)
	deadline := time.Now().Add(2 * time.Second)
	for slowStarted.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if slowStarted.Load() < 2 {
		t.Fatal("cancel replay never reached the upstream")
	}
	code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays/"+canceled.ID+"/cancel", nil)
	if code != 200 || json.Unmarshal(raw, &canceled) != nil || canceled.State != "canceled" || canceled.Reason != "manual" {
		t.Fatalf("cancel failed: %d %s", code, raw)
	}
	log = logOf(t, storage, canceled.TraceID)
	if log.Status != "canceled" || log.StatusCode != 499 {
		t.Fatalf("cancel did not mark the replay canceled: %+v", log)
	}
	// The upstream sees the dropped connection asynchronously.
	for deadline = time.Now().Add(2 * time.Second); inflight.Load() != 0 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	if inflight.Load() != 0 {
		t.Fatal("cancel did not stop the upstream request")
	}
	if code, _ := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays/"+canceled.ID+"/cancel", nil); code != 409 {
		t.Fatal("cancelling a finished replay must conflict")
	}
	if code, _ := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays/nope/cancel", nil); code != 404 {
		t.Fatal("unknown replay must be 404")
	}
	code, raw = api(t, proxy.ReplaysHandler, http.MethodGet, "/api/replays/"+canceled.ID, nil)
	if code != 200 || !strings.Contains(string(raw), `"state":"canceled"`) {
		t.Fatalf("GET replay wrong: %d %s", code, raw)
	}
	if code, _ := api(t, proxy.ReplaysHandler, http.MethodPut, "/api/replays", nil); code != 405 {
		t.Fatal("unsupported method must be 405")
	}
	failed := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "upstream-failure", "body": `{"model":"mock","input":"fail"}`}, http.StatusAccepted)
	failed = finishedReplay(t, proxy, failed.ID)
	log = logOf(t, storage, failed.TraceID)
	if failed.State != "error" || failed.Reason != "" || failed.StatusCode != 500 || log.Status != "error" || log.StatusCode != 500 {
		t.Fatalf("upstream failure not reported: %+v %+v", failed, log)
	}
	graph, _ := storage.GraphSnapshot("")
	replayEdges := 0
	for _, edge := range graph.Edges {
		if edge.Kind == "replay" {
			replayEdges++
		}
	}
	if replayEdges != 3 {
		t.Fatalf("failed replays must remain visible in the graph: %+v", graph.Edges)
	}
}

func TestReplayDestinationRulesAndEndpointRestrictions(t *testing.T) {
	upstream, hits := echoUpstream(t)
	proxy, storage := testProxyWithTarget(t, upstream, "/base/")
	body := `{"model":"mock","input":"prefixed"}`
	source, _ := send(t, proxy, http.MethodPost, "/v1/responses", body, map[string]string{"Content-Type": "application/json"})
	first := hits()
	if len(first) != 1 || first[0].path != "/base/v1/responses" {
		t.Fatalf("setup path wrong: %+v", first)
	}
	code, raw := api(t, proxy.RequestsHandler, http.MethodGet, "/api/requests/"+source+"/curl", nil)
	var exported export.Export
	if code != 200 || json.Unmarshal(raw, &exported) != nil || !strings.HasSuffix(exported.Destination.URL, "/base/v1/responses") {
		t.Fatalf("outgoing export must keep the upstream prefix once: %s", raw)
	}
	record := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "prefix"}, http.StatusAccepted)
	record = finishedReplay(t, proxy, record.ID)
	all := hits()
	if record.State != "done" || len(all) != 2 || all[1].path != "/base/v1/responses" || !bytes.Equal(all[1].body, []byte(body)) {
		t.Fatalf("outgoing replay must not duplicate the prefix: %+v", all)
	}
	if log := logOf(t, storage, record.TraceID); log.Path != "/v1/responses" {
		t.Fatalf("replay log path should be the gateway-relative path: %q", log.Path)
	}

	// A capture addressed to a different upstream must never be redirected to
	// the configured target silently.
	capture, _ := storage.RequestSnapshot(source)
	foreign := *capture.Outgoing
	foreign.Forwarding.Host = "other.example:443"
	foreign.Forwarding.Scheme = "https"
	storage.CaptureOutgoing(source, foreign)
	if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source, "idempotency_key": "foreign"}); code != 409 || !strings.Contains(string(raw), "other.example") {
		t.Fatalf("destination mismatch must be rejected: %d %s", code, raw)
	}
	if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source, "idempotency_key": "foreign-original", "source": "original"}); code != 202 {
		t.Fatalf("the original snapshot still targets the gateway: %d %s", code, raw)
	}

	listing, _ := send(t, proxy, http.MethodGet, "/v1/models", "", nil)
	if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": listing, "idempotency_key": "models"}); code != 422 {
		t.Fatalf("non-generating requests must not be replayed: %d %s", code, raw)
	}
	if code, _ := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": "missing", "idempotency_key": "x"}); code != 404 {
		t.Fatal("unknown source must be 404")
	}
	storage.Rules = []store.Rule{{ID: "pause", PathMatch: "/messages", Intercept: true, WaitSeconds: 30}}
	pending, _, done, _ := startPending(t, proxy, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"mock","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`)))
	control(t, proxy, http.MethodPost, "/api/interceptions/"+pending.TraceID+"/cancel", intercept.Edit{Revision: 1}, 200)
	waitRequest(t, done)
	if code, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": pending.TraceID, "idempotency_key": "never", "source": "outgoing"}); code != 422 || !strings.Contains(string(raw), "never forwarded") {
		t.Fatalf("unforwarded outgoing replay must be explicit: %d %s", code, raw)
	}
}

func TestReplayCompressedBodiesAndStreams(t *testing.T) {
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	writer.Write([]byte(`{"model":"mock","input":"compressed"}`))
	writer.Close()
	original := encoded.Bytes()
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n"
	upstream, hits := echoUpstream(t)
	proxy, storage := testProxyWithTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") == "gzip" {
			upstream.ServeHTTP(w, r)
			return
		}
		io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}), "")
	source, _ := send(t, proxy, http.MethodPost, "/v1/responses", string(original), map[string]string{"Content-Type": "application/json", "Content-Encoding": "gzip"})
	edited := `{"model":"mock","input":"changed"}`
	if code, _ := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source, "idempotency_key": "edit-gzip", "body": edited}); code != 422 {
		t.Fatal("compressed bodies cannot be edited")
	}
	record := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "gzip"}, http.StatusAccepted)
	record = finishedReplay(t, proxy, record.ID)
	all := hits()
	if record.State != "done" || len(all) != 2 || !bytes.Equal(all[1].body, original) || all[1].encoding != "gzip" {
		t.Fatalf("compressed replay changed bytes: %+v", record)
	}
	capture, _ := storage.RequestSnapshot(record.TraceID)
	if capture.Original.BodyEncoding != "base64" || capture.Original.Body != base64.StdEncoding.EncodeToString(original) {
		t.Fatal("replay capture lost encoding")
	}

	streamed, _ := send(t, proxy, http.MethodPost, "/v1/responses", `{"model":"mock","input":"stream","stream":true}`, map[string]string{"Content-Type": "application/json"})
	record = createReplay(t, proxy, map[string]any{"trace_id": streamed, "idempotency_key": "sse"}, http.StatusAccepted)
	record = finishedReplay(t, proxy, record.ID)
	log := logOf(t, storage, record.TraceID)
	if record.State != "done" || log.Type != "SSE" || log.ResponseBody != "hello" || log.OutputTokens != 2 || log.Correlation.ResponseID != "resp-stream" {
		t.Fatalf("SSE replay was not recorded through the normal pipeline: %+v %+v", record, log)
	}
}
