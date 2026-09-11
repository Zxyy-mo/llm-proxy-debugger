package store

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ResponseSnapshot contains no provider credentials or user-controlled file path.
// Body is loaded only for the detail endpoint, never for session/live snapshots.
type ResponseSnapshot struct {
	TraceID            string `json:"trace_id"`
	Type               string `json:"type"`
	ContentType        string `json:"content_type"`
	ContentEncoding    string `json:"content_encoding,omitempty"`
	Decoded            bool   `json:"decoded"`
	StatusCode         int    `json:"status_code"`
	Bytes              int64  `json:"bytes"`
	Stored             string `json:"stored"`
	Complete           bool   `json:"complete"`
	Receiving          bool   `json:"receiving"`
	Reason             string `json:"reason,omitempty"`
	ObservationWarning string `json:"observation_warning,omitempty"`
	Body               string `json:"body"`
	BodyEncoding       string `json:"body_encoding,omitempty"`
	Truncated          bool   `json:"truncated,omitempty"`
	Offset             int64  `json:"offset"`
	End                int64  `json:"end"`
	NextOffset         *int64 `json:"next_offset"`
	LastOffset         int64  `json:"last_offset"`
	Redacted           bool   `json:"redacted,omitempty"`
	Representation     string `json:"representation,omitempty"`
	Path               string `json:"-"`
	Variant            string `json:"variant,omitempty"`
}

func (s *Store) CaptureUpstreamResponse(trace string, response ResponseSnapshot) {
	s.Lock()
	defer s.Unlock()
	if rec := s.records[trace]; rec != nil {
		response.TraceID, response.Variant = trace, "upstream"
		rec.upstreamResponse = &response
		s.changed()
	}
}

func (s *Store) UpstreamResponseSnapshot(trace string) (ResponseSnapshot, bool) {
	s.RLock()
	defer s.RUnlock()
	if rec := s.records[trace]; rec != nil && rec.upstreamResponse != nil {
		return *rec.upstreamResponse, true
	}
	return ResponseSnapshot{}, false
}

func (s *Store) CaptureResponse(trace string, response ResponseSnapshot) {
	s.Lock()
	defer s.Unlock()
	if rec := s.records[trace]; rec != nil {
		response.TraceID = trace
		rec.response = &response
		rec.log.ResponseBytes, rec.log.ResponseStored = response.Bytes, response.Stored
		s.changed()
	}
}

func (s *Store) ResponseSnapshot(trace string) (ResponseSnapshot, bool) {
	s.RLock()
	defer s.RUnlock()
	rec := s.records[trace]
	if rec == nil {
		return ResponseSnapshot{}, false
	}
	if rec.response == nil {
		reason := "尚未收到上游响应"
		if rec.log.Status != "running" && rec.log.Status != "pending" {
			reason = "此请求没有保存的上游响应"
		}
		return ResponseSnapshot{TraceID: trace, Stored: "missing", Reason: reason}, true
	}
	return *rec.response, true
}

func (s *Store) ResponsesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/responses/"), "/")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] != "download") {
		writeError(w, http.StatusNotFound, "response not found")
		return
	}
	query, queryErr := url.ParseQuery(r.URL.RawQuery)
	response, ok := s.ResponseSnapshot(parts[0])
	if variant := query.Get("variant"); variant == "upstream" {
		response, ok = s.UpstreamResponseSnapshot(parts[0])
	} else if variant != "" && variant != "client" {
		writeError(w, http.StatusBadRequest, "variant must be client or upstream")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	// An offset opts into bounded browsing. With neither parameter, preserve
	// the existing full-body detail API. Downloads do not use these parameters.
	offset, limit := int64(0), int64(0)
	if len(parts) == 1 {
		// URL.Query silently drops malformed values. Treat that as a client
		// error instead of accidentally falling back to an unlimited read.
		if queryErr != nil {
			writeError(w, http.StatusBadRequest, "invalid response query parameters")
			return
		}
		if values, present := query["offset"]; present {
			var ok bool
			offset, ok = responseByteParameter(values)
			if !ok {
				writeError(w, http.StatusBadRequest, "offset must be a nonnegative integer byte offset")
				return
			}
			limit = 256 << 10
		}
		if values, present := query["limit"]; present {
			var ok bool
			limit, ok = responseByteParameter(values)
			if !ok || limit < 1 || limit > 16<<20 {
				writeError(w, http.StatusBadRequest, "limit must be 1–16777216 bytes")
				return
			}
		}
	}
	var file *os.File
	if response.Stored == "file" {
		var err error
		if !s.safeCapturePath(response.Path) {
			response.Stored, response.Reason = "missing", "响应文件不在当前捕获目录内"
		} else {
			file, err = os.Open(response.Path)
		}
		if err != nil {
			response.Stored, response.Reason = "missing", "响应文件不可读取，可能已被清理"
		} else if file != nil {
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() {
				response.Stored, response.Reason = "missing", "响应文件不可读取"
			} else {
				response.Bytes = info.Size()
			}
		}
	}
	if len(parts) == 2 {
		if response.Stored != "file" {
			writeError(w, http.StatusNotFound, response.Reason)
			return
		}
		if response.Receiving {
			writeError(w, http.StatusConflict, "响应尚在接收，请结束后下载")
			return
		}
		ext := ".bin"
		if response.Type == "json" {
			ext = ".json"
		} else if response.Type == "sse" {
			ext = ".sse"
		} else if response.Type == "websocket" {
			ext = ".jsonl"
		} else if strings.HasPrefix(response.ContentType, "text/") {
			ext = ".txt"
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "response-" + response.TraceID + ext}))
		w.Header().Set("Content-Length", strconv.FormatInt(response.Bytes, 10))
		_, _ = io.Copy(w, file)
		return
	}
	response.Body, response.BodyEncoding = "", ""
	response.Offset, response.End, response.LastOffset = 0, 0, 0
	response.Truncated, response.NextOffset = false, nil
	if response.Stored == "file" {
		if offset > response.Bytes {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "offset exceeds stored response size")
			return
		}
		response.Offset, response.End = offset, offset
		// Capture the currently available size, including while receiving. A
		// growing file must not turn a bounded read into an unbounded one.
		readBytes := response.Bytes - offset
		if limit > 0 && readBytes > limit+utf8.UTFMax-1 {
			readBytes = limit + utf8.UTFMax - 1
		}
		body, err := io.ReadAll(io.NewSectionReader(file, offset, readBytes))
		if err != nil {
			response.Stored, response.Reason = "missing", "响应读取失败"
		} else {
			if int64(len(body)) < readBytes {
				// Files normally only grow until completion, but an externally
				// shortened file must not produce a non-advancing cursor.
				response.Bytes = offset + int64(len(body))
			}
			text := responseText(response)
			if limit > 0 {
				body = body[:responsePartLength(body, limit, text)]
			}
			response.End = offset + int64(len(body))
			response.Truncated = response.End < response.Bytes
			if response.Truncated {
				next := response.End
				response.NextOffset = &next
			}
			response.LastOffset = responseLastOffset(file, response.Bytes, limit, text)
			if text && utf8.Valid(body) {
				response.Body = string(body)
			} else {
				response.Body, response.BodyEncoding = base64.StdEncoding.EncodeToString(body), "base64"
			}
		}
	}
	json.NewEncoder(w).Encode(response)
}

func responseByteParameter(values []string) (int64, bool) {
	if len(values) != 1 || values[0] == "" {
		return 0, false
	}
	for _, c := range values[0] {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseInt(values[0], 10, 64)
	return value, err == nil
}

func responseText(response ResponseSnapshot) bool {
	if response.ContentEncoding != "" && response.ContentEncoding != "identity" && !response.Decoded {
		return false
	}
	return response.Type != "binary" || strings.HasPrefix(strings.ToLower(response.ContentType), "text/")
}

// responsePartLength preserves exact bytes at arbitrary offsets. Only a valid
// final codepoint split by the limit may move to the next part; invalid bytes
// are never trimmed. A limit too small for the first codepoint may expand by
// at most UTFMax-1 bytes so even limit=1 makes progress through UTF-8 text.
func responsePartLength(body []byte, limit int64, text bool) int {
	if int64(len(body)) <= limit {
		return len(body)
	}
	n := int(limit)
	if !text || utf8.Valid(body[:n]) {
		return n
	}
	start := n - 1
	for start > 0 && n-start < utf8.UTFMax && !utf8.RuneStart(body[start]) {
		start--
	}
	_, size := utf8.DecodeRune(body[start:])
	if size <= 1 || start+size <= n || !utf8.Valid(body[:start]) {
		return n
	}
	if start == 0 {
		return size
	}
	return start
}

// The final-part shortcut is separate from exact requested offsets. For text,
// it starts after a codepoint straddling the nominal boundary, keeping the
// final part within the limit (or one complete codepoint for tiny limits).
func responseLastOffset(file *os.File, total, limit int64, text bool) int64 {
	if limit == 0 || total <= limit {
		return 0
	}
	offset := total - limit
	if !text {
		return offset
	}
	start := max(int64(0), offset-(utf8.UTFMax-1))
	var boundary [2*utf8.UTFMax - 1]byte
	n, _ := file.ReadAt(boundary[:min(int64(len(boundary)), total-start)], start)
	for i := 0; i < n && start+int64(i) < offset; i++ {
		_, size := utf8.DecodeRune(boundary[i:n])
		end := start + int64(i+size)
		if size > 1 && end > offset {
			if end == total {
				return start + int64(i)
			}
			return end
		}
	}
	return offset
}
