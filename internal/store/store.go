package store

import (
	"sync"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
)

// RequestLog 请求日志结构
type RequestLog struct {
	Time             string              `json:"time"`
	TraceID          string              `json:"trace_id,omitempty"`
	SessionID        string              `json:"session_id,omitempty"`
	Type             string              `json:"type"`
	ClientIP         string              `json:"client_ip"`
	Method           string              `json:"method"`
	Path             string              `json:"path"`
	Query            string              `json:"query,omitempty"`
	Headers          map[string]string   `json:"headers,omitempty"`
	RequestBody      string              `json:"request_body,omitempty"`
	StatusCode       int                 `json:"status_code"`
	ResponseBody     string              `json:"response_body,omitempty"`
	Duration         float64             `json:"duration_ms"`
	UserAgent        string              `json:"user_agent"`
	Error            string              `json:"error,omitempty"`
	InputTokens      int                 `json:"input_tokens,omitempty"`
	OutputTokens     int                 `json:"output_tokens,omitempty"`
	ThinkingTokens   int                 `json:"thinking_tokens,omitempty"`
	ThinkingContent  string              `json:"thinking_content,omitempty"`
	ToolUseCount     int                 `json:"tool_use_count,omitempty"`
	IsThinkingLoop   bool                `json:"is_thinking_loop,omitempty"`
	Model            string              `json:"model,omitempty"`
	Protocol         string              `json:"protocol,omitempty"`
	Summary          string              `json:"summary,omitempty"`
	Status           string              `json:"status"`
	Revision         uint64              `json:"revision"`
	Correlation      Correlation         `json:"correlation"`
	Interception     *intercept.Metadata `json:"interception,omitempty"`
	Replay           *ReplayInfo         `json:"replay,omitempty"`
	WaitDuration     float64             `json:"wait_duration_ms"`
	UpstreamDuration float64             `json:"upstream_duration_ms"`
}

// Correlation records evidence separately from session membership. A shared
// session ID does not, by itself, establish a parent/child relationship.
type Correlation struct {
	SessionSource      string `json:"session_source"`
	ResponseID         string `json:"response_id,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	ConversationID     string `json:"conversation_id,omitempty"`
	ThreadID           string `json:"thread_id,omitempty"`
	ParentTraceID      string `json:"parent_trace_id,omitempty"`
	ParentReference    string `json:"parent_reference,omitempty"`
	LinkSource         string `json:"link_source,omitempty"`
	Confidence         string `json:"confidence,omitempty"`
	Warning            string `json:"warning,omitempty"`
}

// Session 会话记录
type Session struct {
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	Label     string       `json:"label"`
	Logs      []RequestLog `json:"logs"`
}

type record struct {
	log              RequestLog
	input            correlation.Request
	sequence         uint64
	fixedSession     bool
	preferredSession string
	capture          *RequestCapture
}

// Rule 动态干预规则
type Rule struct {
	ID            string `json:"id"`
	PathMatch     string `json:"path_match"`
	BodyMatch     string `json:"body_match"`
	InjectSystem  string `json:"inject_system"`
	Intercept     bool   `json:"intercept"`
	Disabled      bool   `json:"disabled"`
	WaitSeconds   int    `json:"wait_seconds"`
	TimeoutAction string `json:"timeout_action"`
}

// Store 全局数据存储
type Store struct {
	sync.RWMutex
	Rules    []Rule
	records  map[string]*record
	sessions map[string]*Session
	aliases  map[string]string // identity key -> anchor trace (follows session moves)
	owners   map[string]map[string]bool
	waiters  map[string]map[string]bool
	children map[string]map[string]bool
	history  map[string]map[string]bool
	revision uint64
	sequence uint64
}

// New 创建并返回一个有默认规则的 Store
func New() *Store {
	return &Store{
		records:  make(map[string]*record),
		sessions: make(map[string]*Session),
		aliases:  make(map[string]string),
		owners:   make(map[string]map[string]bool),
		waiters:  make(map[string]map[string]bool),
		children: make(map[string]map[string]bool),
		history:  make(map[string]map[string]bool),
		Rules: []Rule{
			{
				ID:            "default-compact",
				PathMatch:     "/messages",
				BodyMatch:     "",
				InjectSystem:  "[System Interjection] Be concise and direct.",
				Intercept:     false,
				WaitSeconds:   30,
				TimeoutAction: "forward",
			},
		},
	}
}
