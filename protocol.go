package main

import (
	"time"

	"github.com/tidwall/gjson"
)

// ProtocolMetrics 协议指标
type ProtocolMetrics struct {
	InputTokens     int
	OutputTokens    int
	ThinkingTokens  int
	ThinkingContent string // 新增：提取的明文推理内容
	ToolUseCount    int
	IsThinkingLoop  bool
	EventType       string
}

// ProtocolHandler 定义了不同 LLM 协议的解析逻辑 (OCP 原则)
type ProtocolHandler interface {
	Parse(data []byte) (*ProtocolMetrics, error)
	Name() string
}

// AnthropicHandler 适配 Anthropic/Claude 协议
type AnthropicHandler struct {
	thinkingStreak int
}

func (h *AnthropicHandler) Name() string { return "anthropic" }

func (h *AnthropicHandler) Parse(data []byte) (*ProtocolMetrics, error) {
	if len(data) == 0 {
		return nil, nil
	}
	jsonStr := string(data)
	eventType := gjson.Get(jsonStr, "type").String()

	metrics := &ProtocolMetrics{EventType: eventType}

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
			delta := len([]rune(gjson.Get(jsonStr, "delta.text").String()))
			metrics.OutputTokens = delta
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
		}
	}
	return metrics, nil
}

// OpenAIHandler 适配 OpenAI 协议 (支持 o1/o3-mini 的 reasoning_content)
type OpenAIHandler struct{}

func (h *OpenAIHandler) Name() string { return "openai" }

func (h *OpenAIHandler) Parse(data []byte) (*ProtocolMetrics, error) {
	if len(data) == 0 {
		return nil, nil
	}
	jsonStr := string(data)

	// OpenAI 常用结构：choices[0].delta.content 或 choices[0].delta.reasoning_content
	delta := gjson.Get(jsonStr, "choices.0.delta")
	if !delta.Exists() {
		// 可能是结束包中的 usage
		usage := gjson.Get(jsonStr, "usage")
		if usage.Exists() {
			return &ProtocolMetrics{
				InputTokens:    int(usage.Get("prompt_tokens").Int()),
				OutputTokens:   int(usage.Get("completion_tokens").Int()),
				ThinkingTokens: int(usage.Get("completion_tokens_details.reasoning_tokens").Int()),
			}, nil
		}
		return nil, nil
	}

	metrics := &ProtocolMetrics{}
	if content := delta.Get("content"); content.Exists() {
		metrics.OutputTokens = len([]rune(content.String()))
	}
	if reasoning := delta.Get("reasoning_content"); reasoning.Exists() {
		content := reasoning.String()
		metrics.ThinkingContent = content
		metrics.ThinkingTokens = len([]rune(content))
	}

	return metrics, nil
}

// SSEEventAccumulator 实时累加器 (SRP 原则：仅负责状态聚合)
type SSEEventAccumulator struct {
	TraceID         string          `json:"trace_id"`
	Handler         ProtocolHandler `json:"-"`
	InputTokens     int             `json:"input_tokens"`
	OutputTokens    int             `json:"output_tokens"`
	ThinkingTokens  int             `json:"thinking_tokens"`
	ThinkingContent string          `json:"thinking_content"` // 新增：聚合的推理文本
	ToolUseCount    int             `json:"tool_use_count"`
	IsThinkingLoop  bool            `json:"is_thinking_loop"`
	StartTime       time.Time       `json:"-"`
}

func NewSSEAccumulator(traceID string, handler ProtocolHandler) *SSEEventAccumulator {
	return &SSEEventAccumulator{
		TraceID:   traceID,
		Handler:   handler,
		StartTime: time.Now(),
	}
}

func (acc *SSEEventAccumulator) Accumulate(data []byte) {
	metrics, err := acc.Handler.Parse(data)
	if err != nil || metrics == nil {
		return
	}

	if metrics.InputTokens > 0 {
		acc.InputTokens = metrics.InputTokens
	}
	acc.OutputTokens += metrics.OutputTokens
	acc.ThinkingTokens += metrics.ThinkingTokens
	acc.ThinkingContent += metrics.ThinkingContent
	acc.ToolUseCount += metrics.ToolUseCount
	if metrics.IsThinkingLoop {
		acc.IsThinkingLoop = true
	}
}
