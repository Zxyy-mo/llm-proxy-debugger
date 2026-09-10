package correlation_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
)

func TestStreamingHistoryMatchesReplayedToolTurns(t *testing.T) {
	cases := []struct {
		name, path, request, next string
		events                    []string
	}{
		{
			name: "chat tool arguments across chunks", path: "/v1/chat/completions",
			request: `{"messages":[{"role":"user","content":"check weather"}]}`,
			events: []string{
				`{"id":"chat-tool","choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"weather","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			},
			next: `{"messages":[{"role":"user","content":"check weather"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},{"role":"tool","tool_call_id":"call-1","content":"sunny"}]}`,
		},
		{
			name: "anthropic tool and thinking", path: "/v1/messages",
			request: `{"system":"help","messages":[{"role":"user","content":"check weather"}]}`,
			events: []string{
				`{"type":"message_start","message":{"id":"msg-tool","role":"assistant"}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
				`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"visible reasoning"}}`,
				`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"weather","input":{}}}`,
				`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Paris\"}"}}`,
				`{"type":"message_stop"}`,
			},
			next: `{"system":[{"type":"text","text":"help"}],"messages":[{"role":"user","content":"check weather"},{"role":"assistant","content":[{"type":"thinking","thinking":"visible reasoning","signature":"signature"},{"type":"tool_use","id":"call-1","name":"weather","input":{"city":"Paris"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"sunny"}]}]}`,
		},
		{
			name: "responses output items", path: "/v1/responses",
			request: `{"input":"check weather"}`,
			events: []string{
				`{"type":"response.created","response":{"id":"resp-tool","model":"test"}}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-1","call_id":"call-1","name":"weather","arguments":"{}","status":"completed"}}`,
				`{"type":"response.completed","response":{"id":"resp-tool","output":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"weather","arguments":"{}","status":"completed"}]}}`,
			},
			next: `{"input":[{"role":"user","content":[{"type":"input_text","text":"check weather"}]},{"type":"function_call","call_id":"call-1","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"call-1","output":"sunny"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := correlation.ExtractRequest(nil, tc.path, []byte(tc.request), "upstream")
			capture := correlation.NewResponseCapture(input.Protocol)
			for _, event := range tc.events {
				capture.Event([]byte(event))
			}
			output := capture.Response()
			key := input.CompletedKey(output)
			if output.ID == "" || key == "" {
				t.Fatalf("stream did not capture a usable response: %+v", output)
			}
			next := correlation.ExtractRequest(nil, tc.path, []byte(tc.next), "upstream")
			found := false
			for _, prefix := range next.Prefixes() {
				found = found || prefix == key
			}
			if !found {
				t.Fatal("streamed output fingerprint did not match the client's replay")
			}
		})
	}
}

func TestMetadataIgnoresDisplaySizeAndInvalidIDs(t *testing.T) {
	body := `{"input":"` + strings.Repeat("x", correlation.MaxHistoryBytes+1) + `","conversation":{"id":"conv-large"},"previous_response_id":"resp-old"}`
	request := correlation.ExtractRequest(nil, "/v1/responses", []byte(body), "upstream")
	if !request.HistoryLimited || request.PreviousResponseID != "resp-old" || len(request.Identities) != 1 || request.Identities[0].Value != "conv-large" {
		t.Fatalf("large-body metadata missing: %+v", request.Identities)
	}
	bad := correlation.ExtractRequest(http.Header{"X-Session-Id": {"manual"}}, "/v1/responses", []byte(`{"conversation":42,"previous_response_id":true}`), "upstream")
	if len(bad.Identities) != 1 || bad.PreviousResponseID != "" {
		t.Fatal("non-string IDs should not be interpreted as identifiers")
	}
	malformed := correlation.ExtractRequest(nil, "/v1/responses", []byte(`{"conversation":"partial"`), "upstream")
	if len(malformed.Identities) != 0 || len(malformed.Messages) != 0 {
		t.Fatal("malformed JSON generated correlation evidence")
	}
}

func TestIncompleteAndServerSideContextAreNotHistoryAnchors(t *testing.T) {
	input := correlation.ExtractRequest(nil, "/v1/chat/completions", []byte(`{"messages":[{"role":"user","content":"hello"}]}`), "upstream")
	capture := correlation.NewResponseCapture("openai")
	capture.Event([]byte(`{"id":"chat-1","choices":[{"delta":{"content":"partial"}}]}`))
	if input.CompletedKey(capture.Response()) != "" {
		t.Fatal("incomplete stream became a history anchor")
	}
	stateful := correlation.ExtractRequest(nil, "/v1/responses", []byte(`{"conversation":"conv-1","input":"hello"}`), "upstream")
	response := correlation.NewResponseCapture("responses")
	response.JSON([]byte(`{"id":"resp-1","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`))
	if stateful.CompletedKey(response.Response()) != "" {
		t.Fatal("unknown server context became a full-history anchor")
	}
	systemOnly := correlation.ExtractRequest(nil, "/v1/chat/completions", []byte(`{"messages":[{"role":"system","content":"help"}]}`), "upstream")
	capture.Event([]byte(`[DONE]`))
	if systemOnly.CompletedKey(capture.Response()) != "" {
		t.Fatal("shared system prompt became a history anchor")
	}
}

func TestLargeIntegerValuesRemainDistinctCorrelationEvidence(t *testing.T) {
	body := `{"tools":[{"name":"lookup","input_schema":{"type":"object","default":NUMBER}}],"messages":[{"role":"user","content":"lookup"},{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"lookup","input":{"id":NUMBER}}]}]}`
	a := correlation.ExtractRequest(nil, "/v1/messages", []byte(strings.ReplaceAll(body, "NUMBER", "9007199254740992")), "upstream")
	b := correlation.ExtractRequest(nil, "/v1/messages", []byte(strings.ReplaceAll(body, "NUMBER", "9007199254740993")), "upstream")
	if a.ContextHash == b.ContextHash {
		t.Fatal("distinct large integers in tool schemas became the same context")
	}
	if len(a.Messages) != 2 || len(b.Messages) != 2 || a.Messages[1] == b.Messages[1] {
		t.Fatal("distinct large integer tool inputs became the same message")
	}
}

func TestAPIKeyCredentialsHaveSeparateCorrelationScopes(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"same question"}]}`)
	first := correlation.ExtractRequest(http.Header{"Api-Key": {"first-client"}}, "/v1/chat/completions", body, "upstream")
	second := correlation.ExtractRequest(http.Header{"Api-Key": {"second-client"}}, "/v1/chat/completions", body, "upstream")
	if first.Scope == second.Scope || first.ContextHash == second.ContextHash {
		t.Fatal("different api-key identities share conversation evidence")
	}
}
