package store

import (
	"encoding/json"
	"maps"
	"net/http"
	"strings"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
)

// Credential names a secret that was removed from a snapshot. Exports render it
// as an environment placeholder and replays must receive it again explicitly;
// its value is never retained after the original request finishes.
type Credential struct {
	Kind       string `json:"kind"`             // header, query or url
	Name       string `json:"name"`             // header/parameter name, or "userinfo"
	Scheme     string `json:"scheme,omitempty"` // Bearer, Basic, ... for header credentials
	Replayable bool   `json:"replayable"`       // false for signed or challenge-based schemes
}

// Forwarding keeps what a faithful reproduction needs beyond the display
// snapshot: the destination the request was addressed to and every forwardable
// header with all of its values, minus credentials and hop-by-hop headers. It
// is process-local and never serialized to API clients.
type Forwarding struct {
	Scheme  string
	Host    string
	Path    string
	Query   string // raw query with secret parameters removed
	Headers http.Header
}

// RequestSnapshot is captured before display truncation. Headers are already
// redacted by the proxy and never used to reconstruct upstream authentication.
type RequestSnapshot struct {
	Method        string            `json:"method"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	Body          string            `json:"body"`
	ContentLength int64             `json:"content_length"`
	BodyEncoding  string            `json:"body_encoding,omitempty"`
	Credentials   []Credential      `json:"credentials,omitempty"`
	Forwarding    *Forwarding       `json:"-"`
}

type RequestCapture struct {
	TraceID  string           `json:"trace_id"`
	Original RequestSnapshot  `json:"original"`
	Outgoing *RequestSnapshot `json:"outgoing,omitempty"`
}

// ReplayInfo records that a request was created by an operator replay. It is
// provenance only and never substitutes for conversation parent evidence.
type ReplayInfo struct {
	ID       string `json:"id"`
	Of       string `json:"of"`
	Source   string `json:"source"` // original or outgoing
	Modified bool   `json:"modified"`
}

func copySnapshot(snapshot RequestSnapshot) RequestSnapshot {
	snapshot.Headers = maps.Clone(snapshot.Headers)
	snapshot.Credentials = append([]Credential(nil), snapshot.Credentials...)
	if snapshot.Forwarding != nil {
		forwarding := *snapshot.Forwarding
		forwarding.Headers = snapshot.Forwarding.Headers.Clone()
		snapshot.Forwarding = &forwarding
	}
	return snapshot
}

func (s *Store) CaptureOriginal(traceID string, snapshot RequestSnapshot) {
	s.Lock()
	defer s.Unlock()
	if rec := s.records[traceID]; rec != nil {
		rec.capture = &RequestCapture{TraceID: traceID, Original: copySnapshot(snapshot)}
	}
}

func (s *Store) CaptureOutgoing(traceID string, snapshot RequestSnapshot) {
	s.Lock()
	defer s.Unlock()
	if rec := s.records[traceID]; rec != nil && rec.capture != nil {
		outgoing := copySnapshot(snapshot)
		rec.capture.Outgoing = &outgoing
	}
}

func (s *Store) RequestSnapshot(traceID string) (RequestCapture, bool) {
	s.RLock()
	defer s.RUnlock()
	rec := s.records[traceID]
	if rec == nil || rec.capture == nil {
		return RequestCapture{}, false
	}
	capture := *rec.capture
	capture.Original = copySnapshot(capture.Original)
	if capture.Outgoing != nil {
		outgoing := copySnapshot(*capture.Outgoing)
		capture.Outgoing = &outgoing
	}
	return capture, true
}

// Log returns the authoritative log of one captured request.
func (s *Store) Log(traceID string) (RequestLog, bool) {
	s.RLock()
	defer s.RUnlock()
	rec := s.records[traceID]
	if rec == nil {
		return RequestLog{}, false
	}
	return copyLog(rec.log), true
}

func (s *Store) SetInterception(traceID string, metadata intercept.Metadata) RequestLog {
	s.Lock()
	defer s.Unlock()
	rec := s.records[traceID]
	rec.log.Interception = &metadata
	rec.log.WaitDuration = metadata.WaitDuration
	switch metadata.State {
	case "pending":
		rec.log.Status = "pending"
	case "canceled":
		rec.log.Status = "canceled"
	default:
		rec.log.Status = "running"
	}
	s.touch(rec)
	return copyLog(rec.log)
}

func (s *Store) MarkCanceled(traceID string) {
	s.Lock()
	defer s.Unlock()
	if rec := s.records[traceID]; rec != nil {
		rec.log.Status = "canceled"
		s.touch(rec)
	}
}

func (s *Store) RequestsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	capture, ok := s.RequestSnapshot(strings.TrimPrefix(r.URL.Path, "/api/requests/"))
	if !ok {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	json.NewEncoder(w).Encode(capture)
}
