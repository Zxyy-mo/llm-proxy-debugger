package store

import (
	"sync"
	"time"
)

// RequestLog 请求日志结构
type RequestLog struct {
	Time            string            `json:"time"`
	TraceID         string            `json:"trace_id,omitempty"`
	SessionID       string            `json:"session_id,omitempty"`
	Type            string            `json:"type"`
	ClientIP        string            `json:"client_ip"`
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	Query           string            `json:"query,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	RequestBody     string            `json:"request_body,omitempty"`
	StatusCode      int               `json:"status_code"`
	ResponseBody    string            `json:"response_body,omitempty"`
	Duration        float64           `json:"duration_ms"`
	UserAgent       string            `json:"user_agent"`
	Error           string            `json:"error,omitempty"`
	InputTokens     int               `json:"input_tokens,omitempty"`
	OutputTokens    int               `json:"output_tokens,omitempty"`
	ThinkingTokens  int               `json:"thinking_tokens,omitempty"`
	ThinkingContent string            `json:"thinking_content,omitempty"`
	ToolUseCount    int               `json:"tool_use_count,omitempty"`
	IsThinkingLoop  bool              `json:"is_thinking_loop,omitempty"`
}

// Session 会话记录
type Session struct {
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	Logs      []RequestLog `json:"logs"`
}

// Rule 动态干预规则
type Rule struct {
	ID           string `json:"id"`
	PathMatch    string `json:"path_match"`
	BodyMatch    string `json:"body_match"`
	InjectSystem string `json:"inject_system"`
	Intercept    bool   `json:"intercept"`
}

// Store 全局数据存储
type Store struct {
	sync.RWMutex
	Sessions map[string]*Session
	Rules    []Rule
}

// New 创建并返回一个有默认规则的 Store
func New() *Store {
	return &Store{
		Sessions: make(map[string]*Session),
		Rules: []Rule{
			{
				ID:           "default-compact",
				PathMatch:    "/messages",
				BodyMatch:    "",
				InjectSystem: "[System Interjection] Be concise and direct.",
				Intercept:    false,
			},
		},
	}
}
