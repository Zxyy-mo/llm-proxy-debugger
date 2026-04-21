package protocol

import "github.com/tidwall/gjson"

// AnthropicHandler 适配 Anthropic/Claude 协议
type AnthropicHandler struct {
	thinkingStreak int
}

func (h *AnthropicHandler) Name() string { return "anthropic" }

func (h *AnthropicHandler) Parse(data []byte) (*Metrics, error) {
	if len(data) == 0 {
		return nil, nil
	}
	jsonStr := string(data)
	eventType := gjson.Get(jsonStr, "type").String()

	metrics := &Metrics{EventType: eventType}

	switch eventType {
	case "message_start":
		metrics.InputTokens = int(gjson.Get(jsonStr, "message.usage.input_tokens").Int())
	case "thinking_delta":
		content := gjson.Get(jsonStr, "thinking").String()
		metrics.ThinkingContent = content
		delta := len([]rune(content))
		metrics.ThinkingTokens = delta
		metrics.OutputTokens = delta
		h.thinkingStreak++
		if h.thinkingStreak > 15 {
			metrics.IsThinkingLoop = true
		}
	case "content_block_delta":
		if gjson.Get(jsonStr, "delta.type").String() == "text" {
			text := gjson.Get(jsonStr, "delta.text").String()
			metrics.OutputContent = text
			metrics.OutputTokens = len([]rune(text))
			h.thinkingStreak = 0
		}
	case "content_block_start":
		if gjson.Get(jsonStr, "content_block.type").String() == "tool_use" {
			metrics.ToolUseCount = 1
			h.thinkingStreak = 0
		}
	case "message_delta":
		if usage := gjson.Get(jsonStr, "usage"); usage.Exists() {
			metrics.OutputTokens = int(usage.Get("output_tokens").Int())
			metrics.IsFinalOutputTokens = true
		}
	}
	return metrics, nil
}
