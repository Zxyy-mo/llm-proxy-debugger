package protocol

// Metrics 协议指标
type Metrics struct {
	InputTokens     int
	OutputTokens    int
	ThinkingTokens  int
	ThinkingContent string
	OutputContent   string
	ToolUseCount    int
	IsThinkingLoop  bool
	EventType       string

	// IsFinalOutputTokens 为 true 时，OutputTokens 为权威累计值，Accumulator 应覆盖而非追加。
	IsFinalOutputTokens bool
	// IsFinalThinkingTokens 同上。
	IsFinalThinkingTokens bool
}

// Handler 定义了不同 LLM 协议的解析逻辑
type Handler interface {
	Parse(data []byte) (*Metrics, error)
	Name() string
}
