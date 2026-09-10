package protocol

import "github.com/tidwall/gjson"

// OpenAIHandler handles Chat Completions JSON and SSE, including reasoning
// fields explicitly emitted by compatible providers such as DeepSeek/Qwen.
type OpenAIHandler struct{}

func (h *OpenAIHandler) Name() string { return "openai" }

var reasoningFieldCandidates = []string{"reasoning_content", "reasoning"}

func (h *OpenAIHandler) Parse(data []byte) (*Metrics, error) {
	if !gjson.ValidBytes(data) {
		return nil, nil
	}
	root := gjson.ParseBytes(data)
	delta := root.Get("choices.0.delta")
	if !delta.Exists() {
		delta = root.Get("choices.0.message")
	}
	metrics := &Metrics{}
	if content := delta.Get("content"); content.Type == gjson.String {
		metrics.OutputContent = content.String()
		metrics.OutputTokens = len([]rune(content.String()))
	}
	for _, key := range reasoningFieldCandidates {
		if content := delta.Get(key); content.Type == gjson.String {
			metrics.ThinkingContent = content.String()
			metrics.ThinkingTokens = len([]rune(content.String()))
			break
		}
	}
	for _, call := range delta.Get("tool_calls").Array() {
		if call.Get("id").String() != "" {
			metrics.ToolUseCount++
		}
	}
	if delta.Get("function_call.name").String() != "" {
		metrics.ToolUseCount++
	}
	if usage := root.Get("usage"); usage.IsObject() {
		metrics.InputTokens = int(usage.Get("prompt_tokens").Int())
		metrics.IsFinalInputTokens = usage.Get("prompt_tokens").Exists()
		if value := usage.Get("completion_tokens"); value.Exists() {
			metrics.OutputTokens = int(value.Int())
			metrics.IsFinalOutputTokens = true
		}
		if value := usage.Get("completion_tokens_details.reasoning_tokens"); value.Exists() {
			metrics.ThinkingTokens = int(value.Int())
			metrics.IsFinalThinkingTokens = true
		}
	}
	return metrics, nil
}
