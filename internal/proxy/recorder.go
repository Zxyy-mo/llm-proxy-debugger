package proxy

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/sse"
	"go.uber.org/zap"
)

// responseRecorder 在代理响应的同时，记录状态码、body，并实时处理 SSE 事件。
type responseRecorder struct {
	http.ResponseWriter
	statusCode  int
	body        *bytes.Buffer
	isSSE       bool
	wroteHeader bool
	accumulator *protocol.Accumulator
	sseParser   *sse.SSEParser
	hub         *hub.Hub
	logger      *zap.Logger
	maxBodySize int

	traceID    string
	sseDumpDir string
	dumpFile   *os.File
	dumpFailed bool
}

func newResponseRecorder(
	w http.ResponseWriter,
	traceID string,
	handler protocol.Handler,
	h *hub.Hub,
	log *zap.Logger,
	maxBodySize int,
	sseDumpDir string,
) *responseRecorder {
	return &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           &bytes.Buffer{},
		accumulator:    protocol.NewAccumulator(traceID, handler),
		sseParser:      sse.NewSSEParser(),
		hub:            h,
		logger:         log,
		maxBodySize:    maxBodySize,
		traceID:        traceID,
		sseDumpDir:     sseDumpDir,
	}
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.statusCode = code
		r.wroteHeader = true
		r.ResponseWriter.WriteHeader(code)
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.isSSE {
		r.dumpRaw(b)

		events := r.sseParser.Feed(b)
		for _, evt := range events {
			if evt.Data == "" {
				continue
			}
			r.accumulator.Accumulate([]byte(evt.Data))
			r.hub.Broadcast <- map[string]interface{}{
				"event":      "sse_delta",
				"trace_id":   r.accumulator.TraceID,
				"data":       evt.Data,
				"sse_event":  evt.Event,
				"sse_id":     evt.ID,
				"sse_retry":  evt.RetryMS,
				"sse_fields": evt.Fields,
				"metrics":    r.accumulator,
			}
			if r.accumulator.IsThinkingLoop {
				r.logger.Warn("⚠️ [Thinking Loop Detected]",
					zap.String("trace_id", r.accumulator.TraceID),
					zap.Int("thinking_tokens", r.accumulator.ThinkingTokens))
			}
		}
		return r.ResponseWriter.Write(b)
	}

	// 非 SSE：限量缓存 body 用于日志
	if r.body.Len() < r.maxBodySize {
		remaining := r.maxBodySize - r.body.Len()
		if len(b) <= remaining {
			r.body.Write(b)
		} else {
			r.body.Write(b[:remaining])
		}
	}
	return r.ResponseWriter.Write(b)
}

func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// dumpRaw 把上游原始 SSE 字节追加写入 {logDir}/sse/{trace_id}.log。
// 首次写入时惰性创建文件；任何 I/O 错误仅记一次 warn 后放弃，不影响代理主路径。
func (r *responseRecorder) dumpRaw(b []byte) {
	if r.sseDumpDir == "" || r.dumpFailed {
		return
	}
	if r.dumpFile == nil {
		if err := os.MkdirAll(r.sseDumpDir, 0755); err != nil {
			r.logger.Warn("❗ SSE dump mkdir 失败",
				zap.String("trace_id", r.traceID), zap.Error(err))
			r.dumpFailed = true
			return
		}
		path := filepath.Join(r.sseDumpDir, r.traceID+".log")
		f, err := os.Create(path)
		if err != nil {
			r.logger.Warn("❗ SSE dump 文件创建失败",
				zap.String("trace_id", r.traceID), zap.Error(err))
			r.dumpFailed = true
			return
		}
		r.dumpFile = f
	}
	if _, err := r.dumpFile.Write(b); err != nil {
		r.logger.Warn("❗ SSE dump 写入失败",
			zap.String("trace_id", r.traceID), zap.Error(err))
		r.dumpFailed = true
	}
}

func (r *responseRecorder) close() {
	if r.dumpFile != nil {
		_ = r.dumpFile.Close()
		r.dumpFile = nil
	}
}
