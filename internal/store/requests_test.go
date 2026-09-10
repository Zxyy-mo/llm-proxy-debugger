package store_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func TestEditedHistoryUsesOutgoingTranscript(t *testing.T) {
	s := store.New()
	path := "/v1/chat/completions"
	parent := begin(s, "parent", path, `{"model":"mock","messages":[{"role":"user","content":"hello"}]}`, nil)
	complete(s, parent, `{"id":"parent-response","choices":[{"message":{"role":"assistant","content":"answer"}}]}`)
	child := begin(s, "child", path, `{"model":"mock","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow"}]}`, nil)
	if child.Correlation.ParentTraceID != "parent" {
		t.Fatal("fixture should initially infer the original history")
	}
	edited := `{"model":"mock","messages":[{"role":"user","content":"changed"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow"}]}`
	updated := s.UpdateInput("child", correlation.ExtractRequest(nil, path, []byte(edited), "http://upstream.test"))
	if updated.Correlation.ParentTraceID != "" || updated.SessionID == parent.SessionID {
		t.Fatalf("edited history retained a false edge: %+v", updated)
	}
	complete(s, updated, `{"id":"child-response","choices":[{"message":{"role":"assistant","content":"next"}}]}`)
	next := begin(s, "next", path, `{"model":"mock","messages":[{"role":"user","content":"changed"},{"role":"assistant","content":"answer"},{"role":"user","content":"follow"},{"role":"assistant","content":"next"},{"role":"user","content":"last"}]}`, nil)
	if next.Correlation.ParentTraceID != "child" {
		t.Fatalf("outgoing transcript was not indexed: %+v", next)
	}
}

func TestRequestCaptureSnapshotsDoNotAlias(t *testing.T) {
	s := store.New()
	begin(s, "capture", "/v1/responses", `{"model":"mock","input":"hi"}`, nil)
	snapshot := store.RequestSnapshot{Method: "POST", URL: "/v1/responses", Body: "original", Headers: map[string]string{"Accept": "original"}}
	s.CaptureOriginal("capture", snapshot)
	snapshot.Headers["Accept"] = "edited"
	snapshot.Body = "edited"
	s.CaptureOutgoing("capture", snapshot)
	s.SetInterception("capture", intercept.Metadata{State: "pending", Revision: 1})
	capture, _ := s.RequestSnapshot("capture")
	capture.Original.Headers["Accept"] = "mutated"
	capture.Outgoing.Headers["Accept"] = "mutated"
	log := stored(t, s, "capture")
	log.Interception.State = "mutated"
	again, _ := s.RequestSnapshot("capture")
	if again.Original.Headers["Accept"] != "original" || again.Outgoing.Headers["Accept"] != "edited" || stored(t, s, "capture").Interception.State != "pending" {
		t.Fatal("snapshot consumer mutated authoritative state")
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/requests/capture", 200}, {"POST", "/api/requests/capture", 405}, {"GET", "/api/requests/missing", 404},
	} {
		w := httptest.NewRecorder()
		s.RequestsHandler(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status || !json.Valid(w.Body.Bytes()) {
			t.Fatalf("request capture API: %d %s", w.Code, w.Body)
		}
	}
}

func TestRuleLifecycleAndValidation(t *testing.T) {
	s := store.New()
	call := func(method, path, body string, expected int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		s.RulesHandler(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		if w.Code != expected {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}
	for _, body := range []string{
		`null`, `{}`, `{"path_match":"/responses","wait_seconds":0}`, `{"path_match":"/responses","wait_seconds":3601}`,
		`{"path_match":"/responses","timeout_action":"unknown"}`, `{"path_match":"/responses"} {}`, `{"path_match":"/responses","typo":true}`,
	} {
		call("POST", "/api/rules", body, 400)
	}
	var rule store.Rule
	json.Unmarshal(call("POST", "/api/rules", `{"path_match":"/responses","intercept":true}`, 201), &rule)
	if rule.WaitSeconds != 30 || rule.TimeoutAction != "forward" || rule.Disabled || rule.ID == "" {
		t.Fatalf("rule defaults missing: %+v", rule)
	}
	path := "/api/rules/" + rule.ID
	call("PUT", path, `{"path_match":"/responses","intercept":true,"disabled":true,"wait_seconds":5,"timeout_action":"cancel"}`, 200)
	rules := s.RulesSnapshot()
	if !rules[len(rules)-1].Disabled || rules[len(rules)-1].TimeoutAction != "cancel" {
		t.Fatal("rule update was not saved")
	}
	rules[len(rules)-1].Disabled = false
	if !s.RulesSnapshot()[len(rules)-1].Disabled {
		t.Fatal("rule snapshots alias authoritative state")
	}
	call("DELETE", path, "", 204)
	call("DELETE", path, "", 404)
	call(http.MethodPatch, "/api/rules", "", 405)
}

func TestReplayProvenanceIsSeparateFromParentEvidence(t *testing.T) {
	s := store.New()
	path := "/v1/chat/completions"
	source := begin(s, "source", path, `{"model":"mock","messages":[{"role":"user","content":"hello"}]}`, nil)
	complete(s, source, `{"id":"source-response","choices":[{"message":{"role":"assistant","content":"answer"}}]}`)
	input := correlation.ExtractRequest(nil, path, []byte(`{"model":"mock","messages":[{"role":"user","content":"hello"}]}`), "http://upstream.test")
	replayed := s.Begin(store.RequestLog{TraceID: "replay", Method: "POST", Path: path, Replay: &store.ReplayInfo{ID: "r1", Of: "source", Source: "outgoing"}}, input)
	if replayed.SessionID != source.SessionID || replayed.Correlation.SessionSource != "replay" || replayed.Correlation.ParentTraceID != "" || replayed.Correlation.LinkSource != "" {
		t.Fatalf("replay must share the session without a parent edge: %+v", replayed)
	}
	// Forwarding metadata and credential names survive capture copies while the
	// public snapshot JSON exposes only credential names.
	s.CaptureOriginal("replay", store.RequestSnapshot{
		Method: "POST", URL: path, Body: "{}", Credentials: []store.Credential{{Kind: "header", Name: "Authorization", Scheme: "Bearer", Replayable: true}},
		Forwarding: &store.Forwarding{Scheme: "http", Host: "gateway:1", Path: path, Headers: http.Header{"Accept": {"a", "b"}}},
	})
	capture, _ := s.RequestSnapshot("replay")
	capture.Original.Forwarding.Headers.Set("Accept", "mutated")
	capture.Original.Credentials[0].Name = "mutated"
	again, _ := s.RequestSnapshot("replay")
	if again.Original.Forwarding.Headers.Get("Accept") != "a" || again.Original.Credentials[0].Name != "Authorization" {
		t.Fatal("capture copies alias forwarding metadata")
	}
	encoded, _ := json.Marshal(again)
	if strings.Contains(string(encoded), "gateway:1") || !strings.Contains(string(encoded), `"scheme":"Bearer"`) {
		t.Fatalf("forwarding profile must stay private and credential names public: %s", encoded)
	}
	done := complete(s, replayed, `{"id":"replay-response","choices":[{"message":{"role":"assistant","content":"again"}}]}`)
	if done.Replay == nil || done.Replay.Of != "source" || done.Status != "done" {
		t.Fatalf("completion dropped replay provenance: %+v", done)
	}
	log := stored(t, s, "replay")
	log.Replay.Of = "mutated"
	if stored(t, s, "replay").Replay.Of != "source" {
		t.Fatal("log copies alias replay provenance")
	}
	graph, ok := s.GraphSnapshot(source.SessionID)
	if !ok || len(graph.Edges) != 1 || graph.Edges[0].Kind != "replay" || graph.Edges[0].Source != "source" || graph.Edges[0].Target != "replay" || graph.Edges[0].Confidence != "exact" {
		t.Fatalf("graph must show one replay edge: %+v", graph.Edges)
	}
	for _, node := range graph.Nodes {
		if node.ID == "replay" && (node.Replay == nil || node.Replay.ID != "r1") {
			t.Fatal("graph node lost replay provenance")
		}
	}
	// A replay whose source lives elsewhere still references it exactly once.
	other := begin(s, "other", path, `{"model":"mock","session_id":"explicit","messages":[{"role":"user","content":"x"}]}`, nil)
	s.Begin(store.RequestLog{TraceID: "cross", Method: "POST", Path: path, Replay: &store.ReplayInfo{ID: "r2", Of: "other", Source: "original"}}, correlation.ExtractRequest(nil, path, []byte(`{"model":"mock","session_id":"explicit","messages":[{"role":"user","content":"x"}]}`), "http://upstream.test"))
	cross := stored(t, s, "cross")
	if cross.SessionID != other.SessionID || cross.Correlation.SessionSource == "replay" {
		t.Fatalf("explicit identifiers keep priority over replay membership: %+v", cross)
	}
	full, _ := s.GraphSnapshot("")
	references := 0
	for _, node := range full.Nodes {
		if node.Kind == "reference" {
			references++
		}
	}
	if references != 0 || len(full.Edges) != 2 {
		t.Fatalf("whole graph should not need reference nodes for replays: %d %+v", references, full.Edges)
	}
	if _, ok := s.Log("missing"); ok {
		t.Fatal("Log must report unknown traces")
	}
}
