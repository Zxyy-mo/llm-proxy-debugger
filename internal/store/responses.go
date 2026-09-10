package store

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"net/http"
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
	response, ok := s.ResponseSnapshot(parts[0])
	if variant := r.URL.Query().Get("variant"); variant == "upstream" {
		response, ok = s.UpstreamResponseSnapshot(parts[0])
	} else if variant != "" && variant != "client" {
		writeError(w, http.StatusBadRequest, "variant must be client or upstream")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "request not found")
		return
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
	limit := int64(0)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		var err error
		limit, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || limit < 1 || limit > 16<<20 {
			writeError(w, http.StatusBadRequest, "limit must be 1–16777216 bytes")
			return
		}
	}
	if response.Stored == "file" {
		var reader io.Reader = file
		if limit > 0 {
			reader = io.LimitReader(file, limit+1)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			response.Stored, response.Reason = "missing", "响应读取失败"
		} else {
			if limit > 0 && int64(len(body)) > limit {
				body, response.Truncated = body[:limit], true
			}
			// Do not turn a split multibyte character at a preview boundary into U+FFFD.
			if response.Truncated {
				for i := 0; i < 3 && len(body) > 0 && !utf8.Valid(body); i++ {
					body = body[:len(body)-1]
				}
			}
			if utf8.Valid(body) && (response.ContentEncoding == "" || response.Decoded || response.ContentEncoding == "identity") {
				response.Body = string(body)
			} else {
				response.Body, response.BodyEncoding = base64.StdEncoding.EncodeToString(body), "base64"
			}
		}
	}
	json.NewEncoder(w).Encode(response)
}
