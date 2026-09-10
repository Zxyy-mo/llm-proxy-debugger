package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func TestEncodedInterceptionPreservesOriginalBytes(t *testing.T) {
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	writer.Write([]byte(`{"model":"mock","input":"compressed"}`))
	writer.Close()
	original := encoded.Bytes()
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !bytes.Equal(body, original) || r.Header.Get("Content-Encoding") != "gzip" {
			t.Error("encoded request was reencoded or lost its encoding header")
		}
		io.WriteString(w, `{"ok":true}`)
	}), 64)
	storage.Rules = []store.Rule{{ID: "encoded", PathMatch: "/responses", Intercept: true, WaitSeconds: 30}}
	request := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(original))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	pending, _, done, _ := startPending(t, proxy, request)
	if pending.EditBlockedReason == "" {
		t.Fatal("encoded request offered JSON editing")
	}
	if pending.BodyEncoding != "base64" || pending.Body != base64.StdEncoding.EncodeToString(original) {
		t.Fatal("pending API source lost encoded bytes")
	}
	body := `{"model":"mock","input":"changed"}`
	base := "/api/interceptions/" + pending.TraceID
	control(t, proxy, "POST", base+"/release", intercept.Edit{Revision: 1, Body: &body}, 422)
	control(t, proxy, "POST", base+"/release", intercept.Edit{Revision: 1}, 200)
	waitRequest(t, done)
	capture, _ := storage.RequestSnapshot(pending.TraceID)
	if capture.Original.BodyEncoding != "base64" || capture.Outgoing == nil || capture.Outgoing.BodyEncoding != "base64" {
		t.Fatal("encoded captures were not identified")
	}
	decoded, err := base64.StdEncoding.DecodeString(capture.Original.Body)
	if err != nil || !bytes.Equal(decoded, original) || capture.Outgoing.Body != capture.Original.Body {
		t.Fatal("full encoded capture lost bytes")
	}
}

func startPending(t *testing.T, proxy *Server, request *http.Request) (intercept.Detail, *httptest.ResponseRecorder, chan struct{}, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(request.Context())
	response, done := httptest.NewRecorder(), make(chan struct{})
	go func() {
		defer close(done)
		proxy.ServeHTTP(response, request.WithContext(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("pending HTTP handler did not stop after cancellation")
		}
	})
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		for _, pending := range proxy.interceptions.Pending() {
			if pending.Path == request.URL.Path {
				detail, _ := proxy.interceptions.Get(pending.TraceID)
				return detail, response, done, cancel
			}
		}
		select {
		case <-deadline:
			t.Fatal("request did not become pending")
		case <-done:
			t.Fatal("request forwarded before interception")
		case <-tick.C:
		}
	}
}

func waitRequest(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
}

func control(t *testing.T, proxy *Server, method, path string, body any, status int) intercept.Detail {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	proxy.InterceptionsHandler(w, httptest.NewRequest(method, path, strings.NewReader(string(encoded))))
	if w.Code != status {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, w.Code, status, w.Body)
	}
	var envelope struct {
		Request intercept.Detail `json:"request"`
	}
	if status == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
	}
	return envelope.Request
}

func TestInterceptionSaveReleaseAndFullCaptures(t *testing.T) {
	type outgoing struct {
		body, auth, accept string
		length             int64
	}
	hits := make(chan outgoing, 2)
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- outgoing{string(body), r.Header.Get("Authorization"), r.Header.Get("Accept"), r.ContentLength}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp-edited","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	}), 32)
	storage.Rules = []store.Rule{{ID: "pause", PathMatch: "/responses", Intercept: true, WaitSeconds: 30, TimeoutAction: "forward"}}
	original := `{"model":"original","input":"` + strings.Repeat("context", 2500) + `"}`
	request := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(original))
	request.Header.Set("Authorization", "Bearer original-secret-credential")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	pending, response, done, _ := startPending(t, proxy, request)
	base := "/api/interceptions/" + pending.TraceID
	if pending.Body != original || pending.OriginalBody != original {
		t.Fatal("editor received a truncated source")
	}
	select {
	case <-hits:
		t.Fatal("upstream was reached while pending")
	default:
	}
	edited := `{"model":"edited","input":"changed once","stream":false}`
	edit := intercept.Edit{Revision: pending.Revision, Body: &edited, Headers: map[string]string{"content-type": "application/json", "accept": "application/json"}}
	saved := control(t, proxy, "PATCH", base, edit, 200)
	if saved.Revision != 2 || saved.Deadline != pending.Deadline || !saved.Modified {
		t.Fatalf("save changed deadline or lost revision: %+v", saved)
	}
	control(t, proxy, "POST", base+"/release", intercept.Edit{Revision: 1}, 409)
	control(t, proxy, "POST", base+"/validate", intercept.Edit{Revision: 2}, 200)
	control(t, proxy, "POST", base+"/release", intercept.Edit{Revision: 2}, 200)
	waitRequest(t, done)
	control(t, proxy, "POST", base+"/release", intercept.Edit{Revision: 2}, 409)
	hit := <-hits
	if hit.body != edited || hit.length != int64(len(edited)) || hit.auth != "Bearer original-secret-credential" || hit.accept != "application/json" {
		t.Fatalf("incorrect outgoing request: %+v", hit)
	}
	if len(hits) != 0 || response.Code != 200 {
		t.Fatal("request was not released exactly once")
	}
	log := onlyLog(t, storage)
	if log.Status != "done" || log.Model != "edited" || log.Interception.State != "released" || log.WaitDuration <= 0 || log.UpstreamDuration <= 0 || log.Duration < log.WaitDuration+log.UpstreamDuration {
		t.Fatalf("lifecycle or timing incorrect: %+v", log)
	}
	capture, ok := storage.RequestSnapshot(pending.TraceID)
	if !ok || capture.Original.Body != original || capture.Outgoing == nil || capture.Outgoing.Body != edited {
		t.Fatal("original and outgoing full bodies were not preserved")
	}
	encoded, _ := json.Marshal(capture)
	if strings.Contains(string(encoded), "original-secret-credential") {
		t.Fatal("capture API exposed original credentials")
	}
	if !strings.Contains(log.RequestBody, "truncated") {
		t.Fatal("full captures should not change the display limit")
	}
}

func TestInterceptionInvalidEditsDoNotLosePendingRequest(t *testing.T) {
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid or canceled request reached upstream")
	}), 1024)
	storage.Rules = []store.Rule{{ID: "pause", PathMatch: "/chat/completions", Intercept: true, WaitSeconds: 30}}
	original := `{"model":"mock","messages":[{"role":"user","content":"hello"}]}`
	pending, response, done, _ := startPending(t, proxy, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(original)))
	base := "/api/interceptions/" + pending.TraceID
	for _, body := range []string{
		`{"model":`, `[]`, `{"model":"mock","messages":"invalid"}`,
		`{"model":"mock","messages":[{"role":"tool","tool_call_id":"missing","content":"result"}]}`,
		`{"model":"mock","messages":[{"role":"user","content":"hello"}],"session_id":"changed"}`,
	} {
		control(t, proxy, "POST", base+"/release", intercept.Edit{Revision: 1, Body: &body}, 422)
	}
	for _, headers := range []map[string]string{
		{"Authorization": "replace"}, {"Content-Length": "1"}, {"Accept": "ok\r\nInjected: true"}, {"Accept": "one", "accept": "two"},
	} {
		control(t, proxy, "PATCH", base, intercept.Edit{Revision: 1, Headers: headers}, 422)
	}
	current := control(t, proxy, "GET", base, nil, 200)
	if current.Revision != 1 || current.Body != original || current.State != "pending" {
		t.Fatal("invalid edit changed the pending request")
	}
	control(t, proxy, "POST", base+"/cancel", intercept.Edit{Revision: 1}, 200)
	waitRequest(t, done)
	log := onlyLog(t, storage)
	capture, _ := storage.RequestSnapshot(pending.TraceID)
	if response.Code != 409 || log.Status != "canceled" || capture.Outgoing != nil || log.UpstreamDuration != 0 {
		t.Fatalf("manual cancellation was not terminal: %+v", log)
	}
}

func TestInterceptionTimeoutAndDisconnect(t *testing.T) {
	for _, mode := range []string{"forward", "cancel", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			hits := make(chan string, 1)
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				hits <- string(body)
				io.WriteString(w, `{"ok":true}`)
			}), 1024)
			storage.Rules = []store.Rule{{ID: "pause", PathMatch: "/responses", Intercept: true, WaitSeconds: 1, TimeoutAction: mode}}
			original := `{"model":"mock","input":"timeout"}`
			pending, response, done, cancel := startPending(t, proxy, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(original)))
			if mode == "disconnect" {
				cancel()
			}
			waitRequest(t, done)
			log := onlyLog(t, storage)
			capture, _ := storage.RequestSnapshot(pending.TraceID)
			if mode == "forward" {
				if response.Code != 200 || <-hits != original || log.Interception.Reason != "timeout" || log.WaitDuration < 900 {
					t.Fatalf("timeout did not forward the saved request: %+v", log)
				}
			} else {
				if len(hits) != 0 || log.Status != "canceled" || capture.Outgoing != nil {
					t.Fatalf("canceled request reached upstream: %+v", log)
				}
				if mode == "cancel" && response.Code != 504 {
					t.Fatal("timeout cancellation must return 504")
				}
				if mode == "disconnect" && (log.StatusCode != 499 || log.Interception.Reason != "client_disconnected") {
					t.Fatalf("client disconnect not captured: %+v", log)
				}
			}
			control(t, proxy, "POST", "/api/interceptions/"+pending.TraceID+"/release", intercept.Edit{Revision: 1}, 409)
		})
	}
}

func TestRulesComposeBeforeInterception(t *testing.T) {
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), 100)
	storage.Rules = []store.Rule{
		{ID: "disabled", PathMatch: "/messages", Disabled: true, Intercept: true},
		{ID: "first", PathMatch: "/messages", BodyMatch: "original", InjectSystem: "one", Intercept: true, WaitSeconds: 20, TimeoutAction: "cancel"},
		{ID: "second", PathMatch: "/messages", BodyMatch: "original", InjectSystem: "two", Intercept: true, WaitSeconds: 40},
	}
	before := `{"model":"mock","max_tokens":10,"system":[{"type":"text","text":"original"}],"large":9007199254740993,"messages":[{"role":"user","content":"hi"}]}`
	body, rule := proxy.prepareRequest("/v1/messages", []byte(before))
	if rule == nil || rule.ID != "first" || rule.WaitSeconds != 20 || rule.TimeoutAction != "cancel" {
		t.Fatal("first enabled interception did not determine policy")
	}
	if !strings.Contains(string(body), `9007199254740993`) || !strings.Contains(string(body), `"original"`) || !strings.Contains(string(body), `"one"`) || !strings.Contains(string(body), `"two"`) {
		t.Fatalf("injections did not compose without losing original data: %s", body)
	}
}
