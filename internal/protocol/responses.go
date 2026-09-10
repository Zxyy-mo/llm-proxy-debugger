package protocol

import "github.com/tidwall/gjson"

// ResponsesHandler parses the Responses API's native response.* events.
type ResponsesHandler struct{}

func (h *ResponsesHandler) Name() string { return "responses" }

func (h *ResponsesHandler) Parse(data []byte) (*Metrics, error) {
	if !gjson.ValidBytes(data) {
		return nil, nil
	}
	root := gjson.ParseBytes(data)
	kind := root.Get("type").String()
	metrics := &Metrics{EventType: kind}
	switch kind {
	case "response.output_text.delta":
		metrics.OutputContent = root.Get("delta").String()
		metrics.OutputTokens = len([]rune(metrics.OutputContent))
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		metrics.ThinkingContent = root.Get("delta").String()
		metrics.ThinkingTokens = len([]rune(metrics.ThinkingContent))
	case "response.output_item.added":
		if isToolCall(root.Get("item.type").String()) {
			metrics.ToolUseCount = 1
		}
	case "response.completed", "response.incomplete", "response.failed":
		responseMetrics(metrics, root.Get("response"))
	default:
		if root.Get("object").String() == "response" || root.Get("output").IsArray() {
			responseMetrics(metrics, root)
		}
	}
	return metrics, nil
}

func responseMetrics(metrics *Metrics, response gjson.Result) {
	output := response.Get("output")
	for _, item := range output.Array() {
		switch item.Get("type").String() {
		case "message":
			for _, content := range item.Get("content").Array() {
				if content.Get("type").String() == "output_text" {
					metrics.OutputContent += content.Get("text").String()
				}
			}
		case "reasoning":
			for _, content := range item.Get("summary").Array() {
				metrics.ThinkingContent += content.Get("text").String()
			}
		default:
			if isToolCall(item.Get("type").String()) {
				metrics.ToolUseCount++
			}
		}
	}
	if len(output.Array()) > 0 {
		metrics.IsFinalContent = true
		metrics.IsFinalToolUseCount = true
		metrics.IsFinalThinkingContent = metrics.ThinkingContent != ""
	}
	usage := response.Get("usage")
	metrics.InputTokens = int(usage.Get("input_tokens").Int())
	if value := usage.Get("output_tokens"); value.Exists() {
		metrics.OutputTokens = int(value.Int())
		metrics.IsFinalOutputTokens = true
	}
	if value := usage.Get("output_tokens_details.reasoning_tokens"); value.Exists() {
		metrics.ThinkingTokens = int(value.Int())
		metrics.IsFinalThinkingTokens = true
	}
}

func isToolCall(kind string) bool {
	switch kind {
	case "function_call", "custom_tool_call", "web_search_call", "file_search_call", "computer_call", "mcp_call", "code_interpreter_call":
		return true
	}
	return false
}
