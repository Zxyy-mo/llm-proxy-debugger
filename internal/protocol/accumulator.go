package protocol

import "time"

// Accumulator 实时累加 SSE 事件指标
type Accumulator struct {
	TraceID         string    `json:"trace_id"`
	Handler         Handler   `json:"-"`
	InputTokens     int       `json:"input_tokens"`
	OutputTokens    int       `json:"output_tokens"`
	ThinkingTokens  int       `json:"thinking_tokens"`
	ThinkingContent string    `json:"thinking_content"`
	OutputContent   string    `json:"output_content"`
	ToolUseCount    int       `json:"tool_use_count"`
	IsThinkingLoop  bool      `json:"is_thinking_loop"`
	StartTime       time.Time `json:"-"`
}

// NewAccumulator 创建一个新的累加器
func NewAccumulator(traceID string, handler Handler) *Accumulator {
	return &Accumulator{
		TraceID:   traceID,
		Handler:   handler,
		StartTime: time.Now(),
	}
}

// Accumulate 解析一条 SSE data 并累加到状态
func (acc *Accumulator) Accumulate(data []byte) {
	metrics, err := acc.Handler.Parse(data)
	if err != nil || metrics == nil {
		return
	}

	if metrics.InputTokens > 0 {
		acc.InputTokens = metrics.InputTokens
	}
	if metrics.IsFinalOutputTokens {
		acc.OutputTokens = metrics.OutputTokens
	} else {
		acc.OutputTokens += metrics.OutputTokens
	}
	if metrics.IsFinalThinkingTokens {
		acc.ThinkingTokens = metrics.ThinkingTokens
	} else {
		acc.ThinkingTokens += metrics.ThinkingTokens
	}
	acc.ThinkingContent += metrics.ThinkingContent
	acc.OutputContent += metrics.OutputContent
	acc.ToolUseCount += metrics.ToolUseCount
	if metrics.IsThinkingLoop {
		acc.IsThinkingLoop = true
	}
}
