package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/observation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/sse"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"go.uber.org/zap"
)

// responseRecorder 在代理响应的同时，记录状态码、body，并实时处理 SSE 事件。
type responseRecorder struct {
	tools   *observation.Collector
	onTools func([]observation.ToolCall)
	http.ResponseWriter
	statusCode   int
	body         *bytes.Buffer
	isSSE        bool
	wroteHeader  bool
	accumulator  *protocol.Accumulator
	sseParser    *sse.SSEParser
	hub          *hub.Hub
	logger       *zap.Logger
	maxBodySize  int
	bodyCaptured bool
	capture      *correlation.ResponseCapture
	onResponse   func(correlation.Response)

	traceID    string
	sseDumpDir string
	dumpFile   *os.File
	dumpFailed bool
	response   *store.ResponseSnapshot
	upstreamAt time.Time
	headersAt  time.Time
	contentAt  time.Time
	private    bool
	project    func([]byte) []byte
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
		capture:        correlation.NewResponseCapture(handler.Name()),
		tools:          observation.New(handler.Name()),
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
		if r.sseParser.Err() != nil {
			r.accumulator.TokenSources = protocol.UnknownSources()
			return r.ResponseWriter.Write(b)
		}
		for _, evt := range events {
			if evt.Data == "" {
				continue
			}
			if r.capture.Event([]byte(evt.Data)) && r.onResponse != nil {
				r.onResponse(r.capture.Metadata())
			}
			r.accumulator.Accumulate([]byte(evt.Data))
			r.captureTools([]byte(evt.Data))
			r.markContent()
			metrics, data := *r.accumulator, evt.Data
			metrics.OutputContent = truncateBody([]byte(metrics.OutputContent), r.maxBodySize)
			metrics.ThinkingContent = truncateBody([]byte(metrics.ThinkingContent), r.maxBodySize)
			data = truncateBody([]byte(data), r.maxBodySize)
			if r.private {
				metrics.OutputContent = ""
				metrics.ThinkingContent = ""
				data = ""
				evt.Fields = nil
			}
			r.hub.Publish(map[string]interface{}{
				"event":      "sse_delta",
				"trace_id":   r.accumulator.TraceID,
				"data":       data,
				"sse_event":  evt.Event,
				"sse_id":     evt.ID,
				"sse_retry":  evt.RetryMS,
				"sse_fields": evt.Fields,
				"metrics":    metrics,
			})
			if r.accumulator.IsThinkingLoop {
				r.logger.Warn("⚠️ [Thinking Loop Detected]",
					zap.String("trace_id", r.accumulator.TraceID),
					zap.Int("thinking_tokens", r.accumulator.ThinkingTokens))
			}
		}
		return r.ResponseWriter.Write(b)
	}

	// 非 SSE：限量缓存 body 用于日志
	if !r.bodyCaptured && r.body.Len() < r.maxBodySize {
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

func (r *responseRecorder) captureJSON(body []byte) {
	saved := body
	if r.project != nil {
		saved = r.project(body)
		if r.response != nil {
			r.response.Redacted = !bytes.Equal(saved, body)
			r.response.Representation = "redacted-json"
		}
	}
	r.dumpRaw(saved)
	r.bodyCaptured = true
	r.body.Reset()
	limit := len(saved)
	if limit > r.maxBodySize {
		limit = r.maxBodySize
	}
	if limit > 0 {
		r.body.Write(saved[:limit])
	}
	r.capture.JSON(body)
	r.accumulator.Accumulate(body)
	r.captureTools(body)
	r.markContent()
	if r.onResponse != nil {
		r.onResponse(r.capture.Metadata())
	}
}

func (r *responseRecorder) captureTools(body []byte) {
	r.tools.Feed(body)
	if r.private && r.isSSE {
		return
	}
	calls := r.tools.Calls()
	if len(calls) > 0 && r.onTools != nil {
		r.onTools(calls)
	}
}

func (r *responseRecorder) beginResponse(resp *http.Response) {
	if r.headersAt.IsZero() {
		r.headersAt = time.Now()
	}
	contentType := resp.Header.Get("Content-Type")
	typ, dir, ext := "binary", filepath.Join(filepath.Dir(r.sseDumpDir), "responses"), ".bin"
	if r.isSSE {
		typ, dir, ext = "sse", r.sseDumpDir, ".log"
	} else if strings.Contains(strings.ToLower(contentType), "json") {
		typ, ext = "json", ".json"
	}
	r.response = &store.ResponseSnapshot{TraceID: r.traceID, Type: typ, ContentType: contentType, ContentEncoding: resp.Header.Get("Content-Encoding"), StatusCode: resp.StatusCode, Stored: "file", Receiving: true, Path: filepath.Join(dir, r.traceID+ext)}
	if path, err := filepath.Abs(r.response.Path); err == nil {
		r.response.Path = path
	}
	if r.private && r.isSSE {
		r.response.Stored = "missing"
		r.response.Reason = "记录脱敏已启用，响应结束后保存脱敏输出"
	}
	r.dumpRaw(nil)
}

func (r *responseRecorder) markContent() {
	if r.contentAt.IsZero() && (r.accumulator.OutputContent != "" || r.accumulator.ThinkingContent != "" || r.accumulator.ToolUseCount > 0) {
		r.contentAt = time.Now()
	}
}

func (r *responseRecorder) finishResponse(complete bool) *store.ResponseSnapshot {
	if r.private && r.isSSE && r.response != nil {
		calls := r.tools.Calls()
		for i := range calls {
			if !complete || calls[i].Truncated {
				calls[i].Input = "[未完整捕获工具参数：记录脱敏策略]"
				calls[i].Output = ""
			}
		}
		if len(calls) > 0 && r.onTools != nil {
			r.onTools(calls)
		}
		body, _ := json.Marshal(map[string]string{"output_content": r.accumulator.OutputContent, "thinking_content": r.accumulator.ThinkingContent})
		r.response.Stored = "file"
		r.response.Reason = ""
		r.response.Type = "json"
		r.response.ContentType = "application/json"
		r.response.ContentEncoding = ""
		r.response.Redacted = true
		r.response.Representation = "redacted-output"
		r.response.Path = strings.TrimSuffix(r.response.Path, ".log") + ".json"
		r.private = false
		r.dumpRaw(r.project(body))
	}
	r.close()
	if r.response != nil {
		r.response.Receiving = false
		r.response.Complete = complete
		if !complete && r.response.Reason == "" {
			r.response.Reason = "响应未完整结束，保留已捕获的内容"
		}
	}
	return r.response
}

// Saving never changes the bytes forwarded to the original client. Disk errors
// are exposed in metadata instead of silently advertising a complete capture.
func (r *responseRecorder) dumpRaw(b []byte) {
	if r.private && r.isSSE {
		return
	}
	if r.response == nil {
		return
	}
	r.response.Bytes += int64(len(b))
	if r.dumpFailed {
		return
	}
	if r.dumpFile == nil {
		if err := os.MkdirAll(filepath.Dir(r.response.Path), 0700); err != nil {
			r.failDump(err)
			return
		}
		f, err := os.OpenFile(r.response.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			r.failDump(err)
			return
		}
		r.dumpFile = f
	}
	if _, err := r.dumpFile.Write(b); err != nil {
		r.failDump(err)
	}
}

func (r *responseRecorder) failDump(err error) {
	r.dumpFailed = true
	r.response.Stored, r.response.Reason = "missing", "响应保存失败；转发不受影响"
	r.logger.Warn("response capture failed", zap.String("trace_id", r.traceID), zap.Error(err))
}

func (r *responseRecorder) close() {
	if r.dumpFile != nil {
		if err := r.dumpFile.Close(); err != nil {
			r.failDump(err)
		}
		r.dumpFile = nil
	}
}
