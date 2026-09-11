package store

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
)

func beginRun(s *Store, trace, session, run string) RequestLog {
	header := make(http.Header)
	if session != "" {
		header.Set("X-Session-ID", session)
	}
	if run != "" {
		header.Set("X-Run-ID", run)
	}
	body := []byte(`{"model":"mock","messages":[{"role":"user","content":"same prompt"}]}`)
	input := correlation.ExtractRequest(header, "/v1/chat/completions", body, "https://upstream.test")
	return s.BeginWithBody(RequestLog{TraceID: trace, Method: "POST", Path: "/v1/chat/completions"}, input, body, 4096)
}

func TestRunGroupingKeepsSessionsRoundsAndCausalitySeparate(t *testing.T) {
	s := New()
	a := beginRun(s, "a", "session-one", "run-1")
	b := beginRun(s, "b", "session-one", "run-1")
	c := beginRun(s, "c", "session-one", "run-2")
	d := beginRun(s, "d", "session-two", "run-1")
	missing := beginRun(s, "missing", "session-one", "")
	if a.RunID == "" || a.RunID != b.RunID || a.RunID == c.RunID || a.RunID == d.RunID || missing.RunID != "" {
		t.Fatalf("incorrect task scopes: %q %q %q %q %q", a.RunID, b.RunID, c.RunID, d.RunID, missing.RunID)
	}
	graph, _ := s.GraphSnapshot(a.SessionID)
	if len(graph.Edges) != 0 || len(graph.Nodes) != 4 {
		t.Fatalf("membership invented causality: %+v", graph)
	}
	runs, ok := s.RunsSnapshot(a.SessionID)
	if !ok || len(runs.Runs) != 2 || !slices.Equal(runs.UnassociatedTraceIDs, []string{"missing"}) {
		t.Fatalf("unexpected visible runs: %+v", runs)
	}
	first := runs.Runs[0]
	if first.ID != a.RunID || first.RequestCount != 2 || first.ActiveCount != 2 || first.SessionState != "known" || first.Partial {
		t.Fatalf("unexpected summary: %+v", first)
	}
	// 修改返回副本不能篡改存储里的证据或后续图快照。
	a.Run.Sources[0] = "caller-mutated"
	graph.Nodes[0].Run.Sources[0] = "graph-mutated"
	stored, _ := s.Log(a.TraceID)
	if stored.Run.Sources[0] != "header:X-Run-ID" {
		t.Fatalf("task provenance aliases returned snapshots: %+v", stored.Run)
	}
}

func TestUnscopedRunKeepsStableIdentityAndUncertainSession(t *testing.T) {
	s := New()
	a := beginRun(s, "a", "", "run-1")
	b := beginRun(s, "b", "", "run-1")
	if a.SessionID == b.SessionID || a.RunID != b.RunID {
		t.Fatal("task association changed independent session membership")
	}
	runs, _ := s.RunsSnapshot("")
	if len(runs.Runs) != 1 || runs.Runs[0].SessionState != "unknown" {
		t.Fatalf("missing session evidence was certified: %+v", runs)
	}
	s.ObserveResponse(a.TraceID, correlation.Response{ConversationID: "conversation-one"})
	s.ObserveResponse(b.TraceID, correlation.Response{ConversationID: "conversation-two"})
	runs, _ = s.RunsSnapshot(a.SessionID)
	if len(runs.Runs) != 1 || runs.Runs[0].SessionState != "conflict" || !runs.Runs[0].Partial || runs.Runs[0].RequestCount != 1 || len(runs.Runs[0].SessionIDs) != 2 {
		t.Fatalf("cross-session evidence disappeared in filtered summary: %+v", runs)
	}
	for _, trace := range []string{a.TraceID, b.TraceID} {
		log, _ := s.Log(trace)
		if log.RunID != a.RunID || log.Correlation.ParentTraceID != "" {
			t.Fatalf("late session evidence changed task or invented parent: %+v", log)
		}
	}
}

func TestRunSurvivesLateSessionMoveUpdatesAndReplay(t *testing.T) {
	s := New()
	const path = "/v1/responses"
	body := []byte(`{"metadata":{"run_id":"original-run"},"previous_response_id":"response-parent","input":"child"}`)
	input := correlation.ExtractRequest(nil, path, body, "https://upstream.test")
	child := s.BeginWithBody(RequestLog{TraceID: "child", Method: "POST", Path: path}, input, body, 4096)
	parent := beginRun(s, "parent", "explicit-session", "parent-run")
	s.ObserveResponse(parent.TraceID, correlation.Response{ID: "response-parent"})
	updated, _ := s.Log(child.TraceID)
	if updated.SessionID != parent.SessionID || updated.RunID != child.RunID {
		t.Fatalf("task ID followed movable session: %+v", updated)
	}
	changed := input
	changed.Run = correlation.RunEvidence{Value: "unexpected-update", Sources: []string{"body:metadata.run_id"}}
	s.UpdateInput(child.TraceID, changed)
	s.UpdateOutboundInput(child.TraceID, changed)
	child.StatusCode = 200
	completed := s.Complete(child.TraceID, child, correlation.Response{Complete: true})
	if completed.RunID != child.RunID || completed.Run.ExternalID != "original-run" || s.records[child.TraceID].input.Run.Value != "original-run" {
		t.Fatalf("input/completion replaced admission task evidence: %+v", completed.Run)
	}
	replayed := s.BeginWithBody(RequestLog{TraceID: "replayed", Method: "POST", Path: path, Replay: &ReplayInfo{ID: "manual-action", Of: child.TraceID, Source: "outgoing"}}, input, body, 4096)
	if replayed.RunID == child.RunID || replayed.RunID != "run:replay:manual-action" || replayed.Run.State != "replay" || replayed.Run.ExternalID != "" {
		t.Fatalf("replay inherited original task: %+v", replayed)
	}
	if replayed.RequestBody != string(body) {
		t.Fatal("assigning a new replay task changed captured bytes")
	}
}

func TestRunConflictAndCredentialIsolation(t *testing.T) {
	s := New()
	base := beginRun(s, "base", "session", "run")
	for _, tc := range []struct{ trace, target, credential string }{
		{"credential", "https://upstream.test", "Bearer other"},
		{"upstream", "https://another.test", ""},
	} {
		header := make(http.Header)
		header.Set("X-Session-ID", "session")
		header.Set("X-Run-ID", "run")
		header.Set("Authorization", tc.credential)
		input := correlation.ExtractRequest(header, "/v1/responses", nil, tc.target)
		log := s.Begin(RequestLog{TraceID: tc.trace}, input)
		if log.RunID == base.RunID {
			t.Fatalf("run crossed %s boundary", tc.trace)
		}
	}
	header := make(http.Header)
	header.Set("X-Run-ID", "header-run")
	input := correlation.ExtractRequest(header, "/v1/responses", []byte(`{"metadata":{"run_id":"body-run"}}`), "https://upstream.test")
	log := s.Begin(RequestLog{TraceID: "conflict"}, input)
	if log.RunID != "" || log.Run.State != "conflict" || log.Run.Warning != "conflicting_run_identifier" {
		t.Fatalf("conflict was assigned to a task: %+v", log)
	}
}

func TestAmbiguousSessionEvidenceNeverJoinsAnExplicitRun(t *testing.T) {
	for _, tc := range []struct{ name, body, legacySession string }{
		{"duplicate-metadata-session", `{"metadata":{"run_id":"r","session_id":"A","session_id":"B"}}`, "A"},
		{"duplicate-root-session", `{"session_id":"A","session_id":"B","metadata":{"run_id":"r"}}`, "A"},
		{"duplicate-metadata-object", `{"metadata":{"session_id":"A"},"metadata":{"session_id":"B"}}`, "A"},
		{"duplicate-metadata-with-run", `{"metadata":{"run_id":"r","session_id":"A"},"metadata":{"session_id":"B"}}`, "A"},
		{"duplicate-conversation-object-id", `{"conversation":{"id":"A","id":"B"}}`, "conversation:A"},
		{"duplicate-thread", `{"metadata":{"thread_id":"A","thread_id":"B"}}`, "thread:A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			valid := beginRun(s, "valid", "A", "r")
			header := make(http.Header)
			header.Set("X-Run-ID", "r")
			input := correlation.ExtractRequest(header, "/v1/responses", []byte(tc.body), "https://upstream.test")
			log := s.BeginWithBody(RequestLog{TraceID: "ambiguous", Method: "POST", Path: "/v1/responses"}, input, []byte(tc.body), 4096)
			if log.RunID != "" || log.Run.State != "conflict" || log.Run.Warning == "" {
				t.Fatalf("parser-selected session certified a task: %+v", log.Run)
			}
			if log.SessionID != tc.legacySession || log.RequestBody != tc.body {
				t.Fatalf("task validation changed original session/body: %+v", log)
			}
			runs, _ := s.RunsSnapshot("")
			if len(runs.Runs) != 1 || runs.Runs[0].ID != valid.RunID || runs.Runs[0].RequestCount != 1 || !slices.Equal(runs.UnassociatedTraceIDs, []string{log.TraceID}) {
				t.Fatalf("ambiguous request polluted a valid task: %+v", runs)
			}
			graph, _ := s.GraphSnapshot(log.SessionID)
			for _, node := range graph.Nodes {
				if node.TraceID == log.TraceID && (node.RunID != "" || node.Run.State != "conflict") {
					t.Fatalf("graph certified rejected task scope: %+v", node)
				}
			}
		})
	}
}

func TestRunAPIsAndHistoryFilter(t *testing.T) {
	s := New()
	a := beginRun(s, "a", "session", "external-task")
	beginRun(s, "b", "session", "other-task")
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/runs", 200},
		{"GET", "/api/runs?session_id=missing", 404},
		{"GET", "/api/runs?session_id=one&session_id=two", 400},
		{"GET", "/api/runs?session_id=%XX", 400},
		{"POST", "/api/runs", 405},
	} {
		w := httptest.NewRecorder()
		s.RunsHandler(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	s.HistoryHandler(w, httptest.NewRequest("GET", "/api/history?run_id="+url.QueryEscape(a.RunID), nil))
	var history struct {
		Items []RequestLog `json:"items"`
	}
	if json.Unmarshal(w.Body.Bytes(), &history) != nil || len(history.Items) != 1 || history.Items[0].TraceID != "a" {
		t.Fatalf("run filter: %s", w.Body)
	}
	w = httptest.NewRecorder()
	New().RunsHandler(w, httptest.NewRequest("GET", "/api/runs", nil))
	if strings.Contains(w.Body.String(), "null") {
		t.Fatalf("empty arrays became null: %s", w.Body)
	}
}

func TestConcurrentTaskProjection(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := range 24 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			beginRun(s, string(rune('a'+i)), "session", "parallel-task")
			s.RunsSnapshot("")
			s.GraphSnapshot("")
		}(i)
	}
	wg.Wait()
	runs, _ := s.RunsSnapshot("")
	if len(runs.Runs) != 1 || runs.Runs[0].RequestCount != 24 {
		t.Fatalf("parallel requests lost membership: %+v", runs)
	}
}
