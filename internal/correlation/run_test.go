package correlation

import (
	"net/http"
	"strings"
	"testing"
)

func TestRunEvidenceRequiresOneUnambiguousExplicitValue(t *testing.T) {
	for _, tc := range []struct {
		name, body, value, warning string
		headers                    []string
	}{
		{name: "missing", body: `{"metadata":{"trace_id":"not-a-task"}}`},
		{name: "header", headers: []string{" run-1 "}, value: "run-1"},
		{name: "metadata", body: `{"metadata":{"run_id":"run-1"}}`, value: "run-1"},
		{name: "same-evidence", headers: []string{"run-1", "run-1"}, body: `{"metadata":{"run_id":"run-1"}}`, value: "run-1"},
		{name: "header-conflict", headers: []string{"run-1", "run-2"}, warning: "conflicting_run_identifier"},
		{name: "body-conflict", headers: []string{"run-1"}, body: `{"metadata":{"run_id":"run-2"}}`, warning: "conflicting_run_identifier"},
		{name: "duplicate-key", body: `{"metadata":{"run_id":"run-1","run_id":"run-1"}}`, warning: "conflicting_run_identifier"},
		{name: "duplicate-metadata", body: `{"metadata":{"run_id":"run-1"},"metadata":{}}`, warning: "conflicting_run_identifier"},
		{name: "number", body: `{"metadata":{"run_id":42}}`, warning: "invalid_run_identifier"},
		{name: "null", body: `{"metadata":{"run_id":null}}`, warning: "invalid_run_identifier"},
		{name: "empty", headers: []string{""}, warning: "invalid_run_identifier"},
		{name: "invalid-does-not-fallback", headers: []string{"run-1"}, body: `{"metadata":{"run_id":42}}`, warning: "invalid_run_identifier"},
		{name: "control", body: `{"metadata":{"run_id":"bad\u0000id"}}`, warning: "invalid_run_identifier"},
		{name: "trailing-control", body: `{"metadata":{"run_id":"bad\n"}}`, warning: "invalid_run_identifier"},
		{name: "too-long", headers: []string{strings.Repeat("r", 1025)}, warning: "invalid_run_identifier"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			for _, value := range tc.headers {
				h.Add("X-Run-ID", value)
			}
			run := ExtractRequest(h, "/v1/chat/completions", []byte(tc.body), "https://upstream.test").Run
			if run.Value != tc.value || run.Warning != tc.warning {
				t.Fatalf("unexpected evidence: %+v", run)
			}
			if tc.value != "" && (len(run.Sources) == 0 || run.Fingerprint == "") {
				t.Fatalf("explicit evidence lost its provenance: %+v", run)
			}
		})
	}
}

func TestRunEvidenceIsIndependentFromSessionAndHistoryBudget(t *testing.T) {
	header := make(http.Header)
	header.Set("X-Run-ID", "run-1")
	input := ExtractRequest(header, "/v1/responses", []byte(`{"input":"`+strings.Repeat("x", MaxHistoryBytes)+`"}`), "https://upstream.test")
	if !input.HistoryLimited || input.Run.Value != "run-1" || len(input.Identities) != 0 {
		t.Fatalf("task was confused with session/history evidence: %+v", input)
	}
	header.Add("X-Session-ID", "one")
	header.Add("X-Session-ID", "two")
	if got := ExtractRequest(header, "/v1/responses", nil, "").Run.Warning; got != "conflicting_session_identifier" {
		t.Fatalf("ambiguous session scoped a run: %q", got)
	}
	header.Del("X-Session-ID")
	header.Set("X-Session-ID", "one")
	if got := ExtractRequest(header, "/v1/responses", []byte(`{"session_id":"two"}`), "").Run.Warning; got != "conflicting_session_identifier" {
		t.Fatalf("conflicting body session was ignored: %q", got)
	}
	before := ExtractRequest(nil, "/v1/responses", []byte(`{"metadata":{"run_id":1}}`), "").Run
	after := ExtractRequest(nil, "/v1/responses", []byte(`{"metadata":{"run_id":2}}`), "").Run
	if before.Equal(after) {
		t.Fatal("different invalid task fields compare equal")
	}
}

func TestRunRejectsAmbiguousSessionFieldsBeforeParserSelection(t *testing.T) {
	for _, tc := range []struct {
		name, body, firstKind, firstValue string
	}{
		{"root-session", `{"session_id":"A","session_id":"B"}`, "session", "A"},
		{"root-conversation", `{"conversation_id":"A","conversation_id":"B"}`, "conversation", "A"},
		{"root-thread", `{"thread_id":"A","thread_id":"B"}`, "thread", "A"},
		{"root-conversation-object", `{"conversation":{"id":"A"},"conversation":{"id":"B"}}`, "conversation", "A"},
		{"conversation-id", `{"conversation":{"id":"A","id":"B"}}`, "conversation", "A"},
		{"metadata-session", `{"metadata":{"run_id":"r","session_id":"A","session_id":"B"}}`, "session", "A"},
		{"metadata-conversation", `{"metadata":{"conversation_id":"A","conversation_id":"B"}}`, "conversation", "A"},
		{"metadata-thread", `{"metadata":{"thread_id":"A","thread_id":"B"}}`, "thread", "A"},
		{"same-value-duplicate", `{"metadata":{"session_id":"A","session_id":"A"}}`, "session", "A"},
		{"escaped-duplicate-key", `{"metadata":{"session_id":"A","\u0073ession_id":"B"}}`, "session", "A"},
		{"metadata-objects", `{"metadata":{"session_id":"A"},"metadata":{"session_id":"B"}}`, "session", "A"},
		{"metadata-last-empty", `{"metadata":{"thread_id":"A"},"metadata":{}}`, "thread", "A"},
		{"metadata-first-empty", `{"metadata":{},"metadata":{"conversation_id":"B"}}`, "conversation", "B"},
		{"metadata-last-invalid", `{"metadata":{"session_id":"A"},"metadata":null}`, "session", "A"},
		{"invalid-second-value", `{"session_id":"A","session_id":42}`, "session", "A"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			header.Set("X-Run-ID", "r")
			input := ExtractRequest(header, "/v1/responses", []byte(tc.body), "https://upstream.test")
			if input.Run.Warning != "conflicting_session_identifier" {
				t.Fatalf("ambiguous session was accepted as run scope: %+v", input.Run)
			}
			if tc.firstKind == "" {
				if len(input.Identities) != 0 {
					t.Fatalf("Run validation changed legacy session extraction: %+v", input.Identities)
				}
			} else if len(input.Identities) == 0 || input.Identities[0].Kind != tc.firstKind || input.Identities[0].Value != tc.firstValue {
				t.Fatalf("Run validation changed legacy session extraction: %+v", input.Identities)
			}
		})
	}
	for _, name := range []string{"X-Session-ID", "X-Conversation-ID", "X-Thread-ID"} {
		for _, second := range []string{"A", "B", ""} {
			header := make(http.Header)
			header.Set("X-Run-ID", "r")
			header.Add(name, "A")
			header.Add(name, second)
			input := ExtractRequest(header, "/v1/responses", nil, "https://upstream.test")
			if input.Run.Warning != "conflicting_session_identifier" || len(input.Identities) != 1 || input.Identities[0].Value != "A" {
				t.Fatalf("multi-value %s changed certainty or legacy selection: %+v", name, input)
			}
		}
	}
}

func TestRunSessionChecksOnlyRelevantIdentityPaths(t *testing.T) {
	for _, body := range []string{
		`{"session_id":"A","metadata":{"session_id":"A","run_id":"r"}}`,
		`{"session_id":"A","conversation_id":"B","thread_id":"C"}`,
		`{"metadata":{"label":"first"},"metadata":{"label":"second"}}`,
		`{"input":[{"session_id":"A","session_id":"B"}],"unrelated":{"conversation":{"id":"A","id":"B"}}}`,
	} {
		header := make(http.Header)
		header.Set("X-Run-ID", "r")
		input := ExtractRequest(header, "/v1/responses", []byte(body), "https://upstream.test")
		if input.Run.Warning != "" || input.Run.Value != "r" {
			t.Fatalf("unrelated fields or corroborating identity rejected the run: %+v", input.Run)
		}
	}
}
