package protocol

import "time"

// Accumulator 实时累加 SSE 事件指标
type Accumulator struct {
	TraceID         string       `json:"trace_id"`
	Handler         Handler      `json:"-"`
	InputTokens     int          `json:"input_tokens"`
	OutputTokens    int          `json:"output_tokens"`
	ThinkingTokens  int          `json:"thinking_tokens"`
	TokenSources    TokenSources `json:"token_sources"`
	ThinkingContent string       `json:"thinking_content"`
	OutputContent   string       `json:"output_content"`
	ToolUseCount    int          `json:"tool_use_count"`
	IsThinkingLoop  bool         `json:"is_thinking_loop"`
	StartTime       time.Time    `json:"-"`
}

// NewAccumulator 创建一个新的累加器
func NewAccumulator(traceID string, handler Handler) *Accumulator {
	return &Accumulator{
		TraceID:      traceID,
		Handler:      handler,
		StartTime:    time.Now(),
		TokenSources: UnknownSources(),
	}
}

// Accumulate 解析一条 SSE data 并累加到状态
func (acc *Accumulator) Accumulate(data []byte) {
	metrics, err := acc.Handler.Parse(data)
	if err != nil || metrics == nil {
		return
	}

	if metrics.IsFinalInputTokens {
		acc.InputTokens = metrics.InputTokens
		acc.TokenSources.Input = Usage
	}
	if metrics.IsFinalOutputTokens {
		acc.OutputTokens = metrics.OutputTokens
		acc.TokenSources.Output = Usage
	} else if acc.TokenSources.Output != Usage && metrics.OutputTokens > 0 {
		if metrics.IsFinalContent {
			acc.OutputTokens = 0
		}
		acc.OutputTokens += metrics.OutputTokens
		acc.TokenSources.Output = Estimated
	}
	if metrics.IsFinalThinkingTokens {
		acc.ThinkingTokens = metrics.ThinkingTokens
		acc.TokenSources.Thinking = Usage
	} else if acc.TokenSources.Thinking != Usage && metrics.ThinkingTokens > 0 {
		if metrics.IsFinalThinkingContent {
			acc.ThinkingTokens = 0
		}
		acc.ThinkingTokens += metrics.ThinkingTokens
		acc.TokenSources.Thinking = Estimated
	}
	if metrics.IsFinalThinkingContent {
		acc.ThinkingContent = metrics.ThinkingContent
	} else {
		acc.ThinkingContent += metrics.ThinkingContent
	}
	if metrics.IsFinalContent {
		acc.OutputContent = metrics.OutputContent
	} else {
		acc.OutputContent += metrics.OutputContent
	}
	if metrics.IsFinalToolUseCount {
		acc.ToolUseCount = metrics.ToolUseCount
	} else {
		acc.ToolUseCount += metrics.ToolUseCount
	}
	if metrics.IsThinkingLoop {
		acc.IsThinkingLoop = true
	}
}
