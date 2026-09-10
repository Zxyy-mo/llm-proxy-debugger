package store_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func begin(s *store.Store, id, path, body string, headers http.Header) store.RequestLog {
	input := correlation.ExtractRequest(headers, path, []byte(body), "http://upstream.test")
	return s.Begin(store.RequestLog{TraceID: id, Method: "POST", Path: path}, input)
}

func complete(s *store.Store, log store.RequestLog, body string) store.RequestLog {
	capture := correlation.NewResponseCapture(log.Protocol)
	capture.JSON([]byte(body))
	log.StatusCode = http.StatusOK
	return s.Complete(log.TraceID, log, capture.Response())
}

func stored(t *testing.T, s *store.Store, traceID string) store.RequestLog {
	t.Helper()
	for _, session := range s.SessionsSnapshot() {
		for _, log := range session.Logs {
			if log.TraceID == traceID {
				return log
			}
		}
	}
	t.Fatalf("trace %s not found", traceID)
	return store.RequestLog{}
}

func TestIdentityPriorityAliasesAndIsolation(t *testing.T) {
	s := store.New()
	first := begin(s, "first", "/v1/responses", `{"conversation":{"id":"conv-1"},"input":"hello"}`, http.Header{"X-Session-Id": {"manual"}})
	if first.SessionID != "manual" || first.Correlation.SessionSource != "header:X-Session-ID" || first.Correlation.ConversationID != "conv-1" {
		t.Fatalf("manual identity lost: %+v", first)
	}
	second := begin(s, "second", "/v1/responses", `{"conversation":"conv-1","input":"another turn"}`, nil)
	if second.SessionID != first.SessionID || second.Correlation.ParentTraceID != "" {
		t.Fatalf("alias should group without inventing an edge: %+v", second)
	}
	other := begin(s, "other", "/v1/responses", `{"conversation":"conv-1"}`, http.Header{"Authorization": {"Bearer different-client"}})
	if other.SessionID == first.SessionID {
		t.Fatal("identities crossed credentials")
	}
	thread := begin(s, "thread", "/v1/threads/thread-1/runs", `{}`, nil)
	if thread.Correlation.ThreadID != "thread-1" || thread.Correlation.SessionSource != "path:thread_id" {
		t.Fatalf("thread path not recognized: %+v", thread.Correlation)
	}
	metadata := begin(s, "metadata", "/v1/messages", `{"metadata":{"session_id":"metadata-session"}}`, nil)
	if metadata.SessionID != "metadata-session" {
		t.Fatal("metadata session not recognized")
	}
}

func TestExplicitResponseBranchesAndRunningParent(t *testing.T) {
	s := store.New()
	parent := begin(s, "parent", "/v1/responses", `{"model":"test","input":"first"}`, nil)
	s.ObserveResponse(parent.TraceID, correlation.Response{ID: "resp-parent"})
	a := begin(s, "branch-a", "/v1/responses", `{"previous_response_id":"resp-parent","input":"A"}`, nil)
	b := begin(s, "branch-b", "/v1/responses", `{"previous_response_id":"resp-parent","input":"B"}`, nil)
	for _, child := range []store.RequestLog{a, b} {
		if child.SessionID != parent.SessionID || child.Correlation.ParentTraceID != "parent" || child.Correlation.Confidence != "exact" {
			t.Fatalf("wrong response branch: %+v", child)
		}
	}
	graph, ok := s.GraphSnapshot(parent.SessionID)
	if !ok || len(graph.Nodes) != 3 || len(graph.Edges) != 2 {
		t.Fatalf("bad running graph: %+v", graph)
	}
	for _, edge := range graph.Edges {
		if edge.Source != "parent" {
			t.Fatalf("siblings were chained: %+v", edge)
		}
	}
}

func TestLateParentMovesCompletedChildrenAndDescendants(t *testing.T) {
	s := store.New()
	child := begin(s, "child", "/v1/responses", `{"previous_response_id":"resp-late","input":"child"}`, nil)
	child = complete(s, child, `{"id":"resp-child","object":"response","output":[]}`)
	grandchild := begin(s, "grandchild", "/v1/responses", `{"previous_response_id":"resp-child","input":"grandchild"}`, nil)
	before, _ := s.GraphSnapshot(child.SessionID)
	if len(before.Nodes) != 3 || before.Nodes[2].Kind != "reference" {
		t.Fatalf("missing parent should be a reference: %+v", before)
	}
	parent := begin(s, "late-parent", "/v1/responses", `{"input":"parent"}`, nil)
	parent = complete(s, parent, `{"id":"resp-late","object":"response","output":[]}`)
	for _, id := range []string{"child", grandchild.TraceID} {
		log := stored(t, s, id)
		if log.SessionID != parent.SessionID || log.Revision <= child.Revision {
			t.Fatalf("descendant not moved: %+v", log)
		}
	}
	if stored(t, s, "child").Status != "done" {
		t.Fatal("completed child reverted to running")
	}
	if sessions := s.SessionsSnapshot(); len(sessions) != 1 || len(sessions[parent.SessionID].Logs) != 3 {
		t.Fatalf("moved traces must occur exactly once: %+v", sessions)
	}
	graph, _ := s.GraphSnapshot(parent.SessionID)
	for _, node := range graph.Nodes {
		if node.Kind != "request" {
			t.Fatal("resolved parent placeholder remained")
		}
	}
}

func TestExactHistoryBranchesAndFalsePositiveGuards(t *testing.T) {
	s := store.New()
	firstBody := `{"model":"one","messages":[{"role":"system","content":"shared instruction"},{"role":"user","content":"unique first question"}]}`
	parent := begin(s, "parent", "/v1/chat/completions", firstBody, nil)
	parent = complete(s, parent, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"first answer"}}]}`)
	for _, name := range []string{"left", "right"} {
		body := fmt.Sprintf(`{"model":"two","messages":[{"role":"system","content":"shared instruction"},{"role":"user","content":"unique first question"},{"role":"assistant","content":[{"type":"text","text":"first answer"}]},{"role":"user","content":%q}]}`, name)
		child := begin(s, name, "/v1/chat/completions", body, nil)
		if child.Correlation.ParentTraceID != parent.TraceID || child.Correlation.Confidence != "inferred" || child.SessionID != parent.SessionID {
			t.Fatalf("replayed branch not matched: %+v", child)
		}
	}
	for name, body := range map[string]string{
		"same-initial-prompt": firstBody,
		"different-user":      `{"messages":[{"role":"system","content":"shared instruction"},{"role":"user","content":"another question"}]}`,
		"changed-assistant":   `{"messages":[{"role":"system","content":"shared instruction"},{"role":"user","content":"unique first question"},{"role":"assistant","content":"a different answer"},{"role":"user","content":"more"}]}`,
	} {
		child := begin(s, name, "/v1/chat/completions", body, nil)
		if child.SessionID == parent.SessionID || child.Correlation.ParentTraceID != "" {
			t.Fatalf("unrelated request was linked: %s", name)
		}
	}
	history := `{"messages":[{"role":"system","content":"shared instruction"},{"role":"user","content":"unique first question"},{"role":"assistant","content":"first answer"},{"role":"user","content":"more"}]}`
	for name, headers := range map[string]http.Header{
		"other-credential":       {"Authorization": {"Bearer another"}},
		"explicit-other-session": {"X-Session-Id": {"different"}},
	} {
		child := begin(s, name, "/v1/chat/completions", history, headers)
		if child.Correlation.ParentTraceID != "" {
			t.Fatalf("history crossed a boundary: %s", name)
		}
	}
}

func TestAmbiguousHistoryDoesNotChooseMostRecent(t *testing.T) {
	s := store.New()
	for _, id := range []string{"one", "two"} {
		log := begin(s, id, "/v1/chat/completions", `{"messages":[{"role":"user","content":"hello"}]}`, nil)
		complete(s, log, fmt.Sprintf(`{"id":%q,"choices":[{"message":{"role":"assistant","content":"hi"}}]}`, id))
	}
	child := begin(s, "child", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`, nil)
	if child.Correlation.ParentTraceID != "" || child.Correlation.Warning != "ambiguous_history" {
		t.Fatalf("ambiguous history was guessed: %+v", child.Correlation)
	}
}

func TestDuplicateResponseIDsUndoAnAmbiguousLink(t *testing.T) {
	s := store.New()
	a := begin(s, "a", "/v1/responses", `{}`, nil)
	s.ObserveResponse(a.TraceID, correlation.Response{ID: "duplicate"})
	child := begin(s, "child", "/v1/responses", `{"previous_response_id":"duplicate"}`, nil)
	if child.Correlation.ParentTraceID != a.TraceID {
		t.Fatal("initial unique ID should link")
	}
	b := begin(s, "b", "/v1/responses", `{}`, nil)
	s.ObserveResponse(b.TraceID, correlation.Response{ID: "duplicate"})
	child = stored(t, s, "child")
	if child.Correlation.ParentTraceID != "" || child.Correlation.Warning != "ambiguous_parent" || child.SessionID == a.SessionID {
		t.Fatalf("duplicate ID left a false association: %+v", child)
	}
}

func TestExplicitTraceCyclesAndSessionBoundaries(t *testing.T) {
	s := store.New()
	a := begin(s, "a", "/v1/responses", `{}`, http.Header{"X-Parent-Trace-Id": {"b"}})
	b := begin(s, "b", "/v1/responses", `{}`, http.Header{"X-Parent-Trace-Id": {"a"}})
	a = stored(t, s, "a")
	if a.Correlation.ParentTraceID != "" || a.Correlation.Warning != "cycle" || b.Correlation.ParentTraceID != "a" {
		t.Fatalf("cycle was not rejected: a=%+v b=%+v", a.Correlation, b.Correlation)
	}
	c := begin(s, "c", "/v1/responses", `{}`, http.Header{"X-Session-Id": {"manual"}, "X-Parent-Trace-Id": {"b"}})
	if c.SessionID != "manual" || c.Correlation.ParentTraceID != "b" || c.Correlation.Warning != "session_boundary" {
		t.Fatalf("explicit grouping should be preserved: %+v", c)
	}
	graph, _ := s.GraphSnapshot(c.SessionID)
	if len(graph.Nodes) != 2 || graph.Nodes[1].Kind != "reference" || graph.Nodes[1].TraceID != "b" {
		t.Fatalf("cross-session parent reference missing: %+v", graph)
	}
}

func TestResponseConversationAliasFollowsSession(t *testing.T) {
	s := store.New()
	a := begin(s, "a", "/v1/responses", `{"input":"hello"}`, nil)
	s.ObserveResponse(a.TraceID, correlation.Response{ID: "resp-a", ConversationID: "conv-a"})
	b := begin(s, "b", "/v1/responses", `{"conversation":"conv-a","input":"next"}`, nil)
	if b.SessionID != a.SessionID {
		t.Fatal("response-provided conversation did not create an alias")
	}
}

func TestResponsePollingAndCancellationDoNotReplaceTheGeneratingRequest(t *testing.T) {
	s := store.New()
	parent := begin(s, "producer", "/v1/responses", `{"input":"hello"}`, nil)
	s.ObserveResponse(parent.TraceID, correlation.Response{ID: "resp-poll"})
	for _, action := range []struct{ id, method, path string }{
		{"poll", "GET", "/v1/responses/resp-poll"},
		{"cancel", "POST", "/v1/responses/resp-poll/cancel"},
	} {
		input := correlation.ExtractRequest(nil, action.path, nil, "http://upstream.test")
		s.Begin(store.RequestLog{TraceID: action.id, Method: action.method, Path: action.path}, input)
		s.ObserveResponse(action.id, correlation.Response{ID: "resp-poll"})
	}
	child := begin(s, "next", "/v1/responses", `{"previous_response_id":"resp-poll","input":"next"}`, nil)
	if child.Correlation.ParentTraceID != parent.TraceID || child.Correlation.Warning != "" {
		t.Fatalf("response retrieval corrupted the parent index: %+v", child.Correlation)
	}
}

func TestGraphAPIAndSnapshotImmutability(t *testing.T) {
	s := store.New()
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/graph", 200}, {"GET", "/api/graph?session_id=missing", 404}, {"POST", "/api/graph", 405},
	} {
		rr := httptest.NewRecorder()
		s.GraphHandler(rr, httptest.NewRequest(test.method, test.path, nil))
		if rr.Code != test.status || !strings.Contains(rr.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("bad API response: %d %s", rr.Code, rr.Body.String())
		}
		if test.status == 200 && (!strings.Contains(rr.Body.String(), `"nodes":[]`) || !strings.Contains(rr.Body.String(), `"edges":[]`)) {
			t.Fatal("empty graph arrays must not be null")
		}
		if test.status == 405 && rr.Header().Get("Allow") != "GET" {
			t.Fatal("405 requires Allow header")
		}
	}
	log := s.Begin(store.RequestLog{TraceID: "immutable", Headers: map[string]string{"Accept": "original"}}, correlation.Request{})
	log.Headers["Accept"] = "modified"
	snapshot := s.SessionsSnapshot()
	snapshot[log.SessionID].Logs[0].Headers["Accept"] = "modified again"
	if stored(t, s, log.TraceID).Headers["Accept"] != "original" {
		t.Fatal("snapshot caller mutated store state")
	}
}

func TestConcurrentCaptureAndSnapshots(t *testing.T) {
	s := store.New()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("request-%d", i)
			log := begin(s, id, "/v1/responses", `{"input":"hello"}`, nil)
			s.ObserveResponse(id, correlation.Response{ID: "resp-" + id})
			complete(s, log, fmt.Sprintf(`{"id":%q,"object":"response","output":[]}`, "resp-"+id))
			json.Marshal(s.SessionsSnapshot())
			graph, _ := s.GraphSnapshot("")
			json.Marshal(graph)
		}(i)
	}
	wg.Wait()
	graph, _ := s.GraphSnapshot("")
	if len(graph.Nodes) != 40 {
		t.Fatalf("lost concurrent requests: %d", len(graph.Nodes))
	}
}
