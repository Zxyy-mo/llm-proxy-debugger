package store

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/google/uuid"
)

// AttemptDetail 同时提供尝试结果和该次独立出站快照；旧历史缺少正文时明确返回 unavailable。
type AttemptDetail struct {
	TraceID string          `json:"trace_id"`
	Attempt RouteAttempt    `json:"attempt"`
	Request RequestSnapshot `json:"request"`
}

func copyAttempt(attempt RouteAttempt) RouteAttempt {
	if attempt.TotalDuration != nil {
		value := *attempt.TotalDuration
		attempt.TotalDuration = &value
	}
	return attempt
}

func copyAttempts(attempts []RouteAttempt) []RouteAttempt {
	result := make([]RouteAttempt, len(attempts))
	for i, attempt := range attempts {
		result[i] = copyAttempt(attempt)
	}
	return result
}

// BeginAttempt 必须紧邻实际传输调用，不能用于本地校验或路由预选。
// 快照落盘后才设置默认开始时间，避免把存储耗时算作上游发送耗时。
func (s *Store) BeginAttempt(trace string, attempt RouteAttempt, snapshot RequestSnapshot) (RouteAttempt, bool) {
	s.Lock()
	defer s.Unlock()
	rec := s.records[trace]
	if rec == nil || (rec.log.Status != "running" && rec.log.Status != "pending") {
		return RouteAttempt{}, false
	}
	if rec.log.Route == nil {
		rec.log.Route = &RouteInfo{Attempts: []RouteAttempt{}}
	}
	if attempt.ID == "" {
		attempt.ID = uuid.NewString()
	}
	for _, existing := range rec.log.Route.Attempts {
		if existing.ID == attempt.ID {
			return copyAttempt(existing), false
		}
	}
	attempt.TraceID, attempt.Sequence = trace, len(rec.log.Route.Attempts)+1
	attempt.Source, attempt.Status = "gateway", "running"
	attempt.HeadersAt, attempt.EndedAt, attempt.TotalDuration = "", "", nil
	attempt.StatusCode, attempt.Duration, attempt.Error = 0, 0, ""
	if attempt.RequestURL == "" {
		attempt.RequestURL = snapshot.URL
	}
	if attempt.Transport == "" {
		attempt.Transport = "http"
	}
	if rec.privacy.Record && !rec.privacy.RetainRaw {
		snapshot = s.projectSnapshot(snapshot, rec.privacy, rec.privacyScope)
		attempt = s.projectAttempt(rec, attempt)
	}
	// 文件名只使用内部哈希；同一 Request 的多次尝试不能覆盖旧 outgoing 文件。
	snapshot = s.stashSnapshot(trace, "attempt-"+correlation.Hash(attempt.ID), snapshot)
	if rec.attemptCaptures == nil {
		rec.attemptCaptures = make(map[string]RequestSnapshot)
	}
	rec.attemptCaptures[attempt.ID] = copySnapshot(snapshot)
	if rec.capture == nil {
		rec.capture = &RequestCapture{TraceID: trace, Original: RequestSnapshot{Unavailable: "此请求未保存原始入站快照"}}
	}
	outgoing := copySnapshot(snapshot)
	rec.capture.Outgoing = &outgoing
	if attempt.StartedAt == "" {
		attempt.StartedAt = time.Now().Format(time.RFC3339Nano)
	}
	rec.log.Route.ProviderID = attempt.ProviderID
	rec.log.Route.Attempts = append(rec.log.Route.Attempts, copyAttempt(attempt))
	s.touch(rec)
	return copyAttempt(attempt), true
}

// UpdateAttempt 只接受已登记尝试的结果更新，身份与目的地保持不变。
// EOF 后才发现的协议错误可单向把 done 补为 error，但不能重写真实结束时间或重新进入运行状态。
func (s *Store) UpdateAttempt(trace string, attempt RouteAttempt) bool {
	s.Lock()
	defer s.Unlock()
	rec := s.records[trace]
	if rec == nil || rec.log.Route == nil {
		return false
	}
	switch attempt.Status {
	case "running", "done", "error", "canceled", "abandoned", "interrupted":
	default:
		return false
	}
	for i, existing := range rec.log.Route.Attempts {
		if existing.ID != attempt.ID || existing.Source != "gateway" {
			continue
		}
		if existing.Status == "done" && attempt.Status == "error" {
			existing.Status, existing.Error = "error", attempt.Error
			if rec.privacy.Record && !rec.privacy.RetainRaw {
				existing = s.projectAttempt(rec, existing)
			}
			rec.log.Route.Attempts[i] = copyAttempt(existing)
			s.touch(rec)
			return true
		}
		if existing.Status != "running" {
			return false
		}
		existing.Status, existing.StatusCode = attempt.Status, attempt.StatusCode
		existing.Duration, existing.Error = attempt.Duration, attempt.Error
		if existing.HeadersAt == "" {
			existing.HeadersAt = attempt.HeadersAt
		}
		existing.EndedAt, existing.TotalDuration = attempt.EndedAt, attempt.TotalDuration
		if rec.privacy.Record && !rec.privacy.RetainRaw {
			existing = s.projectAttempt(rec, existing)
		}
		rec.log.Route.Attempts[i] = copyAttempt(existing)
		s.touch(rec)
		return true
	}
	return false
}

// projectAttempt 使用所属请求的冻结隐私策略，新增目的地和错误摘要不绕过记录脱敏。
func (s *Store) projectAttempt(rec *record, attempt RouteAttempt) RouteAttempt {
	attempt = copyAttempt(attempt)
	if rec.privacy.Record {
		project := func(value string) string { return s.Privacy.Text(rec.privacy, rec.privacyScope, value) }
		attempt.URL, attempt.RequestURL, attempt.Error = project(attempt.URL), project(attempt.RequestURL), project(attempt.Error)
	}
	return attempt
}

// AttemptsSnapshot 返回请求当前的尝试列表，不存在的请求与尚未发送的空列表分开表达。
func (s *Store) AttemptsSnapshot(trace string) ([]RouteAttempt, bool) {
	s.RLock()
	defer s.RUnlock()
	rec := s.records[trace]
	if rec == nil {
		return nil, false
	}
	attempts := []RouteAttempt{}
	if rec.log.Route != nil {
		for _, attempt := range rec.log.Route.Attempts {
			attempts = append(attempts, s.projectAttempt(rec, attempt))
		}
	}
	return attempts, true
}

// AttemptSnapshot 按请求和尝试双重身份取出独立正文，防止回退到最后一次 outgoing 冒充旧尝试。
func (s *Store) AttemptSnapshot(trace, id string) (AttemptDetail, bool) {
	s.RLock()
	defer s.RUnlock()
	rec := s.records[trace]
	if rec == nil || rec.log.Route == nil {
		return AttemptDetail{}, false
	}
	for _, attempt := range rec.log.Route.Attempts {
		if attempt.ID != id {
			continue
		}
		request, found := rec.attemptCaptures[id]
		if !found {
			request = RequestSnapshot{Unavailable: "此历史摘要未保存该次尝试的独立出站内容"}
		} else {
			request = s.projectSnapshot(s.loadSnapshot(request), rec.privacy, rec.privacyScope)
		}
		return AttemptDetail{TraceID: trace, Attempt: s.projectAttempt(rec, attempt), Request: request}, true
	}
	return AttemptDetail{}, false
}

// AttemptsHandler 提供逐次出站查看；读取不会补发请求，也不会把缺失捕获替换为其他尝试。
func (s *Store) AttemptsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/attempts/"), "/")
	if len(parts) == 0 || len(parts) > 3 || parts[0] == "" || (len(parts) >= 2 && parts[1] == "") || (len(parts) == 3 && parts[2] != "body") {
		writeError(w, http.StatusNotFound, "attempt not found")
		return
	}
	if len(parts) == 1 {
		attempts, ok := s.AttemptsSnapshot(parts[0])
		if !ok {
			writeError(w, http.StatusNotFound, "request not found")
			return
		}
		json.NewEncoder(w).Encode(struct {
			TraceID  string         `json:"trace_id"`
			Attempts []RouteAttempt `json:"attempts"`
		}{parts[0], attempts})
		return
	}
	detail, ok := s.AttemptSnapshot(parts[0], parts[1])
	if !ok {
		writeError(w, http.StatusNotFound, "attempt not found")
		return
	}
	if len(parts) == 3 {
		if detail.Request.Unavailable != "" {
			writeError(w, http.StatusUnprocessableEntity, detail.Request.Unavailable)
			return
		}
		body, filename := []byte(detail.Request.Body), "attempt-request.json"
		if detail.Request.BodyEncoding == "base64" {
			var err error
			body, err = base64.StdEncoding.DecodeString(detail.Request.Body)
			if err != nil {
				writeError(w, http.StatusUnprocessableEntity, "captured attempt body cannot be decoded")
				return
			}
			filename = "attempt-request.bin"
		} else if detail.Request.BodyEncoding != "" {
			writeError(w, http.StatusUnprocessableEntity, "unsupported attempt body encoding")
			return
		}
		// 下载来自已投影的快照内容，不接受文件路径，也不把外部标识写入响应头。
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
		return
	}
	json.NewEncoder(w).Encode(detail)
}
