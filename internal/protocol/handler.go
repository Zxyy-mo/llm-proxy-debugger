package protocol

// Metrics 协议指标
type Metrics struct {
	InputTokens     int
	OutputTokens    int
	ThinkingTokens  int
	ThinkingContent string
	ToolUseCount    int
	IsThinkingLoop  bool
	EventType       string
}

// Handler 定义了不同 LLM 协议的解析逻辑
type Handler interface {
	Parse(data []byte) (*Metrics, error)
	Name() string
}
