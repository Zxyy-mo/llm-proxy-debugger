package protocol

type TokenSource string

const (
	Usage     TokenSource = "usage"
	Estimated TokenSource = "estimated"
	Unknown   TokenSource = "unknown"
)

type TokenSources struct {
	Input    TokenSource `json:"input"`
	Output   TokenSource `json:"output"`
	Thinking TokenSource `json:"thinking"`
}

func UnknownSources() TokenSources {
	return TokenSources{Input: Unknown, Output: Unknown, Thinking: Unknown}
}

// Metrics 协议指标
type Metrics struct {
	InputTokens        int
	OutputTokens       int
	ThinkingTokens     int
	ThinkingContent    string
	OutputContent      string
	ToolUseCount       int
	IsThinkingLoop     bool
	EventType          string
	IsFinalInputTokens bool

	// IsFinalOutputTokens 为 true 时，OutputTokens 为权威累计值，Accumulator 应覆盖而非追加。
	IsFinalOutputTokens bool
	// IsFinalThinkingTokens 同上。
	IsFinalThinkingTokens  bool
	IsFinalContent         bool
	IsFinalThinkingContent bool
	IsFinalToolUseCount    bool
}

// Handler 定义了不同 LLM 协议的解析逻辑
type Handler interface {
	Parse(data []byte) (*Metrics, error)
	Name() string
}
