package intercept

import "testing"

func TestProtocolAndToolPairValidation(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		valid            bool
	}{
		{"chat tools", "/v1/chat/completions", `{"model":"mock","messages":[{"role":"user","content":"weather"},{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"sunny"}]}`, true},
		{"chat missing result", "/v1/chat/completions", `{"model":"mock","messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"weather","arguments":"{}"}}]}]}`, false},
		{"responses tools", "/v1/responses", `{"model":"mock","input":[{"type":"function_call","call_id":"a","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"sunny"}]}`, true},
		{"responses stateful result", "/v1/responses", `{"model":"mock","previous_response_id":"remote","input":[{"type":"function_call_output","call_id":"a","output":"sunny"}]}`, true},
		{"responses orphan", "/v1/responses", `{"model":"mock","input":[{"type":"function_call_output","call_id":"a","output":"sunny"}]}`, false},
		{"responses empty remote context", "/v1/responses", `{"model":"mock","previous_response_id":"","input":[{"type":"function_call_output","call_id":"a","output":"sunny"}]}`, false},
		{"responses duplicate result", "/v1/responses", `{"model":"mock","previous_response_id":"remote","input":[{"type":"function_call_output","call_id":"a","output":"one"},{"type":"function_call_output","call_id":"a","output":"two"}]}`, false},
		{"anthropic tools", "/v1/messages", `{"model":"mock","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"weather","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"sunny"}]}]}`, true},
		{"anthropic wrong result", "/v1/messages", `{"model":"mock","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"weather","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"sunny"}]}]}`, false},
		{"anthropic missing result", "/v1/messages", `{"model":"mock","max_tokens":100,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"weather","input":{}}]},{"role":"user","content":"continue"}]}`, false},
		{"multimodal", "/v1/chat/completions", `{"model":"mock","custom_provider_option":true,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.invalid/image"}},{"type":"text","text":"describe"}]}]}`, true},
		{"invalid stream", "/v1/responses", `{"model":"mock","input":"hi","stream":"yes"}`, false},
		{"duplicate JSON field", "/v1/responses", `{"model":"one","input":"hi","model":"two"}`, false},
		{"trailing JSON", "/v1/responses", `{"model":"mock","input":"hi"} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.path, tc.body, tc.body, map[string]string{"Content-Type": "application/json"})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, got %v", tc.valid, err)
			}
		})
	}
}
