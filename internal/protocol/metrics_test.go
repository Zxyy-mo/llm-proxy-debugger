package protocol

import "testing"

func TestUsageProvenanceAndZeroValues(t *testing.T) {
	cases := []struct {
		name                  string
		handler               Handler
		estimate, usage, late string
	}{
		{"chat", &OpenAIHandler{}, `{"choices":[{"delta":{"content":"你好","reasoning_content":"thinking"}}]}`, `{"usage":{"prompt_tokens":0,"completion_tokens":0,"completion_tokens_details":{"reasoning_tokens":0}}}`, `{"choices":[{"delta":{"content":"late"}}]}`},
		{"responses", &ResponsesHandler{}, `{"type":"response.output_text.delta","delta":"hello"}`, `{"type":"response.completed","response":{"usage":{"input_tokens":0,"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0}}}}`, `{"type":"response.output_text.delta","delta":"late"}`},
		{"anthropic", &AnthropicHandler{}, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}`, `{"type":"message","content":[],"usage":{"input_tokens":0,"output_tokens":0}}`, `{"type":"content_block_delta","delta":{"type":"text_delta","text":"late"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			acc := NewAccumulator("trace", tc.handler)
			if acc.TokenSources != UnknownSources() {
				t.Fatal("empty counts must be unknown")
			}
			acc.Accumulate([]byte(tc.estimate))
			if acc.TokenSources.Output != Estimated || acc.OutputTokens == 0 {
				t.Fatal("content was not labelled estimated")
			}
			acc.Accumulate([]byte(tc.usage))
			if acc.TokenSources.Input != Usage || acc.TokenSources.Output != Usage || acc.InputTokens != 0 || acc.OutputTokens != 0 {
				t.Fatalf("zero usage did not replace estimate: %+v", acc)
			}
			acc.Accumulate([]byte(tc.late))
			if acc.OutputTokens != 0 || acc.TokenSources.Output != Usage {
				t.Fatal("late content contaminated authoritative usage")
			}
		})
	}
}

func TestPartialUsageDoesNotInventCounters(t *testing.T) {
	acc := NewAccumulator("trace", &OpenAIHandler{})
	acc.Accumulate([]byte(`{"usage":{"completion_tokens":12}}`))
	if acc.TokenSources.Input != Unknown || acc.TokenSources.Thinking != Unknown || acc.TokenSources.Output != Usage {
		t.Fatalf("partial usage: %+v", acc.TokenSources)
	}
	acc = NewAccumulator("trace", &ResponsesHandler{})
	acc.Accumulate([]byte(`{"type":"response.output_text.delta","delta":"你好"}`))
	acc.Accumulate([]byte(`{"object":"response","output":[{"type":"message","content":[{"type":"output_text","text":"你好"}]}]}`))
	if acc.OutputTokens != 2 || acc.TokenSources.Output != Estimated {
		t.Fatalf("final text double-counted: %+v", acc)
	}
}
