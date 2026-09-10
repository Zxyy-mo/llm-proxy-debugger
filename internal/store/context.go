package store

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/contextdiff"
)

func (s *Store) ContextHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, 405, "method not allowed")
		return
	}
	trace := strings.TrimPrefix(r.URL.Path, "/api/context-diff/")
	log, ok := s.Log(trace)
	if !ok {
		writeError(w, 404, "request not found")
		return
	}
	base, by := r.URL.Query().Get("base"), "manual"
	if base == "" {
		base, by = log.Correlation.ParentTraceID, "parent"
	}
	if base == "" {
		writeError(w, 409, "没有已捕获的父调用，请选择一个比较对象")
		return
	}
	if base == trace {
		writeError(w, 400, "请选择不同的调用作为比较对象")
		return
	}
	current, ok := s.RequestSnapshot(trace)
	if !ok {
		writeError(w, 404, "request snapshot not found")
		return
	}
	previous, ok := s.RequestSnapshot(base)
	if !ok {
		writeError(w, 404, "比较对象的快照不存在")
		return
	}
	current = s.DisplayCapture(trace, current)
	previous = s.DisplayCapture(base, previous)
	choose := func(c RequestCapture) (RequestSnapshot, string) {
		if c.Outgoing != nil {
			return *c.Outgoing, "outgoing"
		}
		return c.Original, "original"
	}
	a, as := choose(previous)
	b, bs := choose(current)
	if a.Unavailable != "" || b.Unavailable != "" {
		writeError(w, 404, "请求正文不可读取")
		return
	}
	if a.BodyEncoding != "" || b.BodyEncoding != "" {
		writeError(w, 422, "压缩或二进制请求暂不支持上下文比较")
		return
	}
	if len(a.Body) > 16<<20 || len(b.Body) > 16<<20 {
		writeError(w, 413, "上下文超过 16 MiB，请下载快照后比较")
		return
	}
	before, err := contextdiff.Parse([]byte(a.Body))
	if err != nil {
		writeError(w, 422, err.Error())
		return
	}
	after, err := contextdiff.Parse([]byte(b.Body))
	if err != nil {
		writeError(w, 422, err.Error())
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"trace_id": trace, "base_trace_id": base, "selected_by": by, "source": bs, "base_source": as, "diff": contextdiff.Compare(before, after)})
}
