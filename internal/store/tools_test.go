package store

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/observation"
)

func TestToolResultsAndReportedDuration(t *testing.T) {
	s := New()
	input := correlation.Request{Scope: "same", Identities: []correlation.Identity{{Kind: "session", Value: "tools", Source: "header"}}}
	parent := s.Begin(RequestLog{TraceID: "parent"}, input)
	parent.StatusCode = 200
	s.Complete("parent", parent, correlation.Response{})
	s.ObserveTools("parent", []observation.ToolCall{{ID: "call-1", CallID: "call-1", Name: "lookup", Input: `{"query":"test"}`, Status: "requested", Source: "llm"}})
	input.ParentTraceID = "parent"
	s.Begin(RequestLog{TraceID: "child"}, input)
	s.ObserveToolResults("child", []byte(`{"messages":[{"role":"tool","tool_call_id":"call-1","content":"found"}]}`))
	log, _ := s.Log("parent")
	if len(log.Tools) != 1 || log.Tools[0].Output != "found" || log.Tools[0].Duration != nil || log.Tools[0].ResultTrace != "child" {
		t.Fatalf("tool result not linked: %+v", log.Tools)
	}
	body := `{"trace_id":"parent","span_id":"span-1","call_id":"call-1","name":"lookup","kind":"mcp","status":"done","started_at":"2026-09-10T10:00:00Z","ended_at":"2026-09-10T10:00:00.125Z","input":"{}","output":"found"}`
	w := httptest.NewRecorder()
	s.ToolSpansHandler(w, httptest.NewRequest("POST", "/api/tool-spans", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("span rejected: %d %s", w.Code, w.Body)
	}
	if json.Unmarshal(w.Body.Bytes(), &log) != nil || log.Tools[0].Duration == nil || *log.Tools[0].Duration != 125 || log.Tools[0].Source != "trace" {
		t.Fatalf("reported duration not retained: %s", w.Body)
	}
	graph, _ := s.GraphSnapshot("")
	tools := 0
	for _, node := range graph.Nodes {
		if node.Kind == "tool" {
			tools++
			if node.Tool.Input != "" || node.Tool.Output != "" {
				t.Fatal("graph leaked full tool payload")
			}
		}
	}
	if tools != 1 {
		t.Fatal("tool node absent")
	}
}

func TestDistinctExecutionsOfOneToolCallKeepDistinctSpans(t *testing.T) {
	s := New()
	s.Begin(RequestLog{TraceID: "parent"}, correlation.Request{})
	s.ObserveTools("parent", []observation.ToolCall{{ID: "call", CallID: "call", Name: "lookup", Source: "llm"}})
	for _, id := range []string{"first", "second", "first"} {
		body := fmt.Sprintf(`{"trace_id":"parent","span_id":%q,"call_id":"call","name":"lookup","kind":"mcp","status":"done","started_at":"2026-09-10T10:00:00Z","ended_at":"2026-09-10T10:00:00.125Z","input":"{}","output":%q}`, id, id)
		w := httptest.NewRecorder()
		s.ToolSpansHandler(w, httptest.NewRequest("POST", "/api/tool-spans", strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	log, _ := s.Log("parent")
	if len(log.Tools) != 2 || log.Tools[0].SpanID != "first" || log.Tools[1].SpanID != "second" || log.Tools[0].Output != "first" {
		t.Fatalf("execution spans were overwritten: %+v", log.Tools)
	}
}

func TestLateToolOutputRetainsOnlyReferencedAliasesAcrossCleanupAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	p := recordingPolicy(t, s)
	p.Outbound = false
	if err := s.Privacy.Set(p); err != nil {
		t.Fatal(err)
	}
	unrelated := privacyRequest(s, "unrelated", `{"input":"outside@example.com"}`, http.Header{"X-Session-Id": {"unrelated-session"}}, nil)
	unknownToken := placeholderIn(t, unrelated.RequestBody)
	output := "response-only@example.com " + unknownToken
	body := fmt.Sprintf(`{"previous_response_id":"late-parent-response","input":[{"type":"function_call_output","call_id":"tool-1","output":%q}],"metadata":{"note":"not-attached@example.com"}}`, output)
	child := privacyRequest(s, "result-first", body, nil, nil)
	s.CaptureOriginal(child.TraceID, RequestSnapshot{Body: body})
	s.CaptureOutgoing(child.TraceID, RequestSnapshot{Body: body})
	s.ObserveToolResults(child.TraceID, []byte(body))
	finishPrivate(s, child, "child-response")
	_, childScope := s.PrivacyContext(child.TraceID)
	unreferenced := s.Privacy.Text(p, childScope, "not-attached@example.com")
	parent := privacyRequest(s, "parent-later", `{"input":"parent"}`, nil, nil)
	_, parentScope := s.PrivacyContext(parent.TraceID)
	if childScope == parentScope {
		t.Fatal("test did not create distinct historical namespaces")
	}
	s.ObserveResponse(parent.TraceID, correlation.Response{ID: "late-parent-response"})
	s.ObserveTools(parent.TraceID, []observation.ToolCall{{ID: "tool-1", CallID: "tool-1", Name: "lookup", Source: "api"}})
	finishPrivate(s, parent, "late-parent-response")
	log, _ := s.Log(parent.TraceID)
	if len(log.Tools) != 1 || log.Tools[0].ResultTrace != child.TraceID {
		t.Fatalf("tool result provenance not attached: %+v", log.Tools)
	}
	attached := log.Tools[0].Output
	if attached != s.records[child.TraceID].toolResults[0].Output {
		t.Fatal("attaching a tool result rewrote its projected output")
	}
	if restoredText(t, s, parent.TraceID, attached) != output || restoredText(t, s, child.TraceID, attached) != output {
		t.Fatal("copied output cannot be restored through both retained captures")
	}
	if restoredText(t, s, parent.TraceID, unreferenced) != unreferenced || restoredText(t, s, parent.TraceID, unknownToken) != unknownToken {
		t.Fatal("tool attachment imported unreferenced or unrelated mappings")
	}
	if aliases := s.Privacy.Snapshot().Values[parentScope]; len(aliases) != 1 {
		t.Fatalf("parent retained more than the one referenced source alias: %+v", aliases)
	}
	capture, _ := s.RequestSnapshot(child.TraceID)
	if capture.Original.Body != body || capture.Outgoing == nil || capture.Outgoing.Body != body {
		t.Fatal("tool attachment changed original or outgoing capture bytes")
	}
	if _, _, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == child.TraceID }); err != nil {
		t.Fatal(err)
	}
	if _, present := s.Privacy.Snapshot().Values[childScope]; present {
		t.Fatal("cleanup retained the entire discarded result namespace")
	}
	if restoredText(t, s, parent.TraceID, attached) != output {
		t.Fatal("child cleanup removed the surviving tool owner's aliases")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log, _ = s.Log(parent.TraceID)
	if len(log.Tools) != 1 || log.Tools[0].Output != attached || log.Tools[0].ResultTrace != child.TraceID || restoredText(t, s, parent.TraceID, attached) != output {
		t.Fatal("restored tool owner lost output, provenance or referenced aliases")
	}
	if restoredText(t, s, parent.TraceID, unreferenced) != unreferenced || restoredText(t, s, parent.TraceID, unknownToken) != unknownToken {
		t.Fatal("restart widened the tool owner's restore authority")
	}
}

func TestLateToolAliasesHonorBothCaptureRetentionPolicies(t *testing.T) {
	for _, tc := range []struct {
		name                string
		source, destination bool
	}{
		{"source-discards", false, true},
		{"owner-discards", true, false},
		{"both-discard", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			base := recordingPolicy(t, s)
			base.Outbound = false
			source := base
			source.RetainRaw, source.AllowReveal = tc.source, tc.source
			if err := s.Privacy.Set(source); err != nil {
				t.Fatal(err)
			}
			const body = `{"previous_response_id":"late-parent-response","input":[{"type":"function_call_output","call_id":"tool-1","output":"result@example.com"}]}`
			child := privacyRequest(s, "result-first", body, nil, nil)
			s.ObserveToolResults(child.TraceID, []byte(body))
			finishPrivate(s, child, "child-response")
			_, childScope := s.PrivacyContext(child.TraceID)
			token := placeholderIn(t, s.records[child.TraceID].toolResults[0].Output)
			// Another capture can have retained a value in the same historical
			// namespace. It must not override this source capture's discard policy.
			if saved := s.Privacy.Text(base, childScope, "result@example.com"); saved != token {
				t.Fatal("test token does not belong to source namespace")
			}
			destination := base
			destination.RetainRaw, destination.AllowReveal = tc.destination, tc.destination
			if err := s.Privacy.Set(destination); err != nil {
				t.Fatal(err)
			}
			parent := privacyRequest(s, "parent-later", `{"input":"parent"}`, nil, nil)
			_, parentScope := s.PrivacyContext(parent.TraceID)
			// Current global opt-in may change, but retention belongs to each
			// capture and stays frozen when attaching a later tool result.
			if err := s.Privacy.Set(base); err != nil {
				t.Fatal(err)
			}
			s.ObserveResponse(parent.TraceID, correlation.Response{ID: "late-parent-response"})
			s.ObserveTools(parent.TraceID, []observation.ToolCall{{ID: "tool-1", CallID: "tool-1", Name: "lookup", Source: "api"}})
			log, _ := s.Log(parent.TraceID)
			if len(log.Tools) != 1 || log.Tools[0].Output != token || log.Tools[0].ResultTrace != child.TraceID {
				t.Fatal("discarded tool output lost placeholder or provenance")
			}
			if _, retained := s.Privacy.Snapshot().Values[parentScope][token]; retained {
				t.Fatal("tool owner acquired raw data against a capture retention policy")
			}
			if restored, err := s.Privacy.Reveal(parentScope, token); err != nil || restored != token {
				t.Fatal("discarded source value became available through destination scope")
			}
		})
	}
}
