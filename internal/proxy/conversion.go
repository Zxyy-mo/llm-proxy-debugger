package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/adapter"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func writeProtocolError(w http.ResponseWriter, source string, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	plan := adapter.Plan{Source: source}
	_ = json.NewEncoder(w).Encode(plan.Error(message))
}

type upstreamTap struct {
	io.ReadCloser
	mu               sync.Mutex
	recorder         *responseRecorder
	complete, closed bool
	save             func(store.ResponseSnapshot)
}

func (t *upstreamTap) Read(b []byte) (int, error) {
	n, err := t.ReadCloser.Read(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.recorder.dumpRaw(b[:n])
		if err == io.EOF {
			t.complete = true
		}
	}
	return n, err
}
func (t *upstreamTap) Close() error {
	err := t.ReadCloser.Close()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closed {
		t.closed = true
		t.save(*t.recorder.finishResponse(t.complete))
	}
	return err
}

func (s *Server) captureConversionUpstream(trace string, resp *http.Response, private bool) *http.Response {
	if private {
		s.store.CaptureUpstreamResponse(trace, store.ResponseSnapshot{Stored: "missing", Reason: "记录脱敏已启用，不保留转换前原始响应；可查看脱敏后的客户端响应", Representation: "upstream-original"})
		return resp
	}
	recorder := newResponseRecorder(&replayWriter{header: make(http.Header)}, trace+"-upstream", &protocol.OpenAIHandler{}, s.hub, s.logger, 0, filepath.Join(s.cfg.LogDir, "sse"))
	recorder.isSSE = strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	recorder.beginResponse(resp)
	recorder.response.TraceID = trace
	recorder.response.Representation = "upstream-original"
	s.store.CaptureUpstreamResponse(trace, *recorder.response)
	resp.Body = &upstreamTap{ReadCloser: resp.Body, recorder: recorder, save: func(snapshot store.ResponseSnapshot) { s.store.CaptureUpstreamResponse(trace, snapshot) }}
	return resp
}
