package protocol

import "github.com/tidwall/gjson"

type AnthropicHandler struct {
	thinkingStreak int
}

func (h *AnthropicHandler) Name() string { return "anthropic" }

func (h *AnthropicHandler) Parse(data []byte) (*Metrics, error) {
	if !gjson.ValidBytes(data) {
		return nil, nil
	}
	root := gjson.ParseBytes(data)
	kind := root.Get("type").String()
	metrics := &Metrics{EventType: kind}
	switch kind {
	case "message":
		for _, block := range root.Get("content").Array() {
			switch block.Get("type").String() {
			case "text":
				metrics.OutputContent += block.Get("text").String()
			case "thinking":
				metrics.ThinkingContent += block.Get("thinking").String()
			case "tool_use":
				metrics.ToolUseCount++
			}
		}
		metrics.InputTokens = int(root.Get("usage.input_tokens").Int())
		metrics.OutputTokens = int(root.Get("usage.output_tokens").Int())
		metrics.IsFinalOutputTokens = root.Get("usage.output_tokens").Exists()
	case "message_start":
		metrics.InputTokens = int(root.Get("message.usage.input_tokens").Int())
	case "thinking_delta":
		metrics.ThinkingContent = root.Get("thinking").String()
	case "content_block_delta":
		switch root.Get("delta.type").String() {
		case "text_delta", "text":
			metrics.OutputContent = root.Get("delta.text").String()
			metrics.OutputTokens = len([]rune(metrics.OutputContent))
			h.thinkingStreak = 0
		case "thinking_delta":
			metrics.ThinkingContent = root.Get("delta.thinking").String()
		}
	case "content_block_start":
		if root.Get("content_block.type").String() == "tool_use" {
			metrics.ToolUseCount = 1
			h.thinkingStreak = 0
		}
	case "message_delta":
		if value := root.Get("usage.output_tokens"); value.Exists() {
			metrics.OutputTokens = int(value.Int())
			metrics.IsFinalOutputTokens = true
		}
	}
	if metrics.ThinkingContent != "" {
		metrics.ThinkingTokens = len([]rune(metrics.ThinkingContent))
		if !metrics.IsFinalOutputTokens {
			metrics.OutputTokens += metrics.ThinkingTokens
		}
		h.thinkingStreak++
		metrics.IsThinkingLoop = h.thinkingStreak > 15
	}
	return metrics, nil
}
