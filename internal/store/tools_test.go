package store

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
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
