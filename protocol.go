package main

import (
	"time"

	"github.com/tidwall/gjson"
)

// ProtocolMetrics 协议指标
type ProtocolMetrics struct {
	InputTokens    int
	OutputTokens   int
	ThinkingTokens int
	ToolUseCount   int
	IsThinkingLoop bool
	EventType      string
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
		delta := len([]rune(gjson.Get(jsonStr, "thinking").String()))
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

// SSEEventAccumulator 实时累加器 (SRP 原则：仅负责状态聚合)
type SSEEventAccumulator struct {
	TraceID        string          `json:"trace_id"`
	Handler        ProtocolHandler `json:"-"`
	InputTokens    int             `json:"input_tokens"`
	OutputTokens   int             `json:"output_tokens"`
	ThinkingTokens int             `json:"thinking_tokens"`
	ToolUseCount   int             `json:"tool_use_count"`
	IsThinkingLoop bool            `json:"is_thinking_loop"`
	StartTime      time.Time       `json:"-"`
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
	acc.ToolUseCount += metrics.ToolUseCount
	if metrics.IsThinkingLoop {
		acc.IsThinkingLoop = true
	}
}
