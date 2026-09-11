package store

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/privacy"
)

const conversationPrivacyPrefix = "conversation:v2:"

type requestPreview struct {
	body  []byte
	limit int
}

// setInitialPreview requires the store lock and an allocated privacy scope.
// Full-body projection precedes both summary extraction and body truncation.
func (s *Store) setInitialPreview(rec *record, preview requestPreview) {
	body := preview.body
	if rec.privacy.Record {
		body = s.Privacy.JSON(rec.privacy, rec.privacyScope, body)
		rec.log.Summary = correlation.ExtractRequest(nil, rec.log.Path, body, "").Summary
		if !utf8.Valid(preview.body) {
			body = []byte("[非文本请求正文未展示：记录脱敏策略]")
		}
	}
	limit := max(0, preview.limit)
	if len(body) <= limit {
		rec.log.RequestBody = string(body)
		return
	}
	end := limit
	for end > 0 && !utf8.RuneStart(body[end]) {
		end--
	}
	rec.log.RequestBody = string(body[:end]) + fmt.Sprintf("... [truncated, total %d bytes]", len(body))
}

// admissionPrivacyScope runs after initial correlation under the store lock.
// Later graph movement never changes a capture's namespace or emitted tokens.
func (s *Store) admissionPrivacyScope(rec *record) string {
	if replay := rec.log.Replay; replay != nil {
		if source := s.records[replay.Of]; source != nil && source.input.Scope == rec.input.Scope &&
			strings.HasPrefix(source.privacyScope, conversationPrivacyPrefix) &&
			(!rec.fixedSession || rec.log.SessionID == source.log.SessionID) {
			return source.privacyScope
		}
	}
	return conversationPrivacyPrefix + correlation.Hash(rec.input.Scope, rec.log.SessionID)
}

func (s *Store) PrivacyContext(trace string) (privacy.Policy, string) {
	s.RLock()
	defer s.RUnlock()
	if rec := s.records[trace]; rec != nil {
		return rec.privacy, rec.privacyScope
	}
	return privacy.Policy{}, ""
}

func (s *Store) displayLog(rec *record) RequestLog {
	log := copyLog(rec.log)
	if !rec.privacy.Record {
		return log
	}
	project := func(value string) string { return s.Privacy.Text(rec.privacy, rec.privacyScope, value) }
	log.RequestBody = string(s.Privacy.JSON(rec.privacy, rec.privacyScope, []byte(log.RequestBody)))
	log.ResponseBody = string(s.Privacy.JSON(rec.privacy, rec.privacyScope, []byte(log.ResponseBody)))
	log.Summary, log.ThinkingContent, log.Error = project(log.Summary), project(log.ThinkingContent), project(log.Error)
	for i := range log.Tools {
		if log.Tools[i].Truncated {
			log.Tools[i].Input, log.Tools[i].Output = "[工具报文已截断：记录脱敏策略]", ""
			continue
		}
		log.Tools[i].Input = string(s.Privacy.JSON(rec.privacy, rec.privacyScope, []byte(log.Tools[i].Input)))
		log.Tools[i].Output = string(s.Privacy.JSON(rec.privacy, rec.privacyScope, []byte(log.Tools[i].Output)))
		log.Tools[i].Error = project(log.Tools[i].Error)
	}
	for key, value := range log.Headers {
		log.Headers[key] = project(value)
	}
	log.Query = project(log.Query)
	log.Privacy = &PrivacyMetadata{Recorded: true, Outbound: rec.privacy.Outbound, RawRetained: rec.privacy.RetainRaw}
	return log
}

type PrivacyMetadata struct {
	Recorded    bool `json:"recorded"`
	Outbound    bool `json:"outbound"`
	RawRetained bool `json:"raw_retained"`
}

func (s *Store) projectSnapshot(snapshot RequestSnapshot, policy privacy.Policy, scope string) RequestSnapshot {
	if !policy.Record {
		return snapshot
	}
	original := snapshot.Body
	if snapshot.BodyEncoding != "" {
		snapshot.Body = "[非文本请求正文未展示：记录脱敏策略]"
		snapshot.BodyEncoding = ""
	} else {
		snapshot.Body = string(s.Privacy.JSON(policy, scope, []byte(snapshot.Body)))
	}
	snapshot.Redacted = snapshot.Redacted || original != snapshot.Body
	snapshot.RawRetained = policy.RetainRaw
	return snapshot
}

func (s *Store) DisplayCapture(trace string, capture RequestCapture) RequestCapture {
	policy, scope := s.PrivacyContext(trace)
	capture.Original = s.projectSnapshot(capture.Original, policy, scope)
	if capture.Outgoing != nil {
		snapshot := s.projectSnapshot(*capture.Outgoing, policy, scope)
		capture.Outgoing = &snapshot
	}
	return capture
}

func (s *Store) PrivacyHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/api/privacy/restore" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, 405, "method not allowed")
			return
		}
		var input struct {
			TraceID string `json:"trace_id"`
			Body    string `json:"body"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
		d.DisallowUnknownFields()
		if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
			writeError(w, 400, "invalid restore request")
			return
		}
		log, ok := s.Log(input.TraceID)
		if !ok {
			writeError(w, 404, "request not found")
			return
		}
		policy, scope := s.PrivacyContext(log.TraceID)
		if !policy.RetainRaw {
			writeError(w, 409, "此请求的策略没有保留原文")
			return
		}
		body, err := s.Privacy.Reveal(scope, input.Body)
		if err != nil {
			writeError(w, 403, err.Error())
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"body": body})
		return
	}
	if r.URL.Path != "/api/privacy" {
		writeError(w, 404, "not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.Privacy.Policy())
	case http.MethodPut:
		var policy privacy.Policy
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		d.DisallowUnknownFields()
		if d.Decode(&policy) != nil || d.Decode(new(any)) != io.EOF {
			writeError(w, 400, "invalid privacy policy")
			return
		}
		if err := s.Privacy.Set(policy); err != nil {
			writeError(w, 422, err.Error())
			return
		}
		s.changed()
		json.NewEncoder(w).Encode(policy)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, 405, "method not allowed")
	}
}

// Outbound applies the policy captured at Begin, so changing settings cannot
// change a request that is already waiting at a breakpoint.
func (s *Store) Outbound(trace string, body []byte) []byte {
	policy, scope := s.PrivacyContext(trace)
	if !policy.Outbound {
		return body
	}
	if !json.Valid(body) {
		return body
	}
	return s.Privacy.JSON(policy, scope, body)
}

func (s *Store) HasPrivatePlaceholders(body string) bool { return strings.Contains(body, "[PRIVATE_") }
