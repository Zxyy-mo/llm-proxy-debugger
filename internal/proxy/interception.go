package proxy

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/export"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func (s *Server) prepareRequest(path string, original []byte) ([]byte, *store.Rule) {
	if s.target.Path != "" && s.target.Path != "/" {
		path = singleJoiningSlash(s.target.Path, path)
	}
	body := original
	var breakpoint *store.Rule
	for _, rule := range s.store.RulesSnapshot() {
		if rule.Disabled || !strings.Contains(path, rule.PathMatch) || (rule.BodyMatch != "" && !strings.Contains(string(original), rule.BodyMatch)) {
			continue
		}
		if rule.Intercept && breakpoint == nil {
			if rule.WaitSeconds == 0 {
				rule.WaitSeconds = 30
			}
			if rule.TimeoutAction == "" {
				rule.TimeoutAction = "forward"
			}
			selected := rule
			breakpoint = &selected
		}
		if rule.InjectSystem == "" {
			continue
		}
		body = injectPrompt(path, body, rule.InjectSystem)
	}
	return body, breakpoint
}

func (s *Server) publishLog(log store.RequestLog) {
	s.hub.Publish(map[string]any{"event": "request_updated", "trace_id": log.TraceID, "log": log})
	s.hub.Publish(map[string]any{"event": "sessions_updated"})
}

type captureTransport struct {
	base    http.RoundTripper
	capture func(*http.Request)
}

func (t captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	t.capture(r)
	return t.base.RoundTrip(r)
}

// snapshotRequest records the complete request for display and reproduction.
// Credential values are dropped: display headers are redacted and the private
// forwarding profile only lists which credentials must be supplied again.
func snapshotRequest(r *http.Request, body []byte) store.RequestSnapshot {
	forwarding, credentials := export.Classify(r)
	display := *r.URL
	display.User = nil
	display.RawQuery = export.RedactQuery(r.URL.RawQuery)
	snapshot := store.RequestSnapshot{
		Method: r.Method, URL: display.String(), Headers: extractHeaders(r.Header),
		Body: string(body), ContentLength: r.ContentLength,
		Credentials: credentials, Forwarding: forwarding,
	}
	if encoding := r.Header.Get("Content-Encoding"); (encoding != "" && !strings.EqualFold(encoding, "identity")) || !utf8.Valid(body) {
		snapshot.Body = base64.StdEncoding.EncodeToString(body)
		snapshot.BodyEncoding = "base64"
	}
	return snapshot
}

func interceptionError(w http.ResponseWriter, status int, err error) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) writeInterception(w http.ResponseWriter, detail intercept.Detail) {
	policy, scope := s.store.PrivacyContext(detail.TraceID)
	if policy.Record {
		detail.Body = string(s.store.Privacy.JSON(policy, scope, []byte(detail.Body)))
		detail.OriginalBody = string(s.store.Privacy.JSON(policy, scope, []byte(detail.OriginalBody)))
		detail.EditBlockedReason = "记录脱敏已启用，可原样放行或取消；原文不会在断点编辑器中展示。"
	}
	json.NewEncoder(w).Encode(map[string]any{"server_time": time.Now().UTC().Format(time.RFC3339Nano), "request": detail})
}

func (s *Server) InterceptionsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/api/interceptions" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			interceptionError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"server_time": time.Now().UTC().Format(time.RFC3339Nano), "requests": s.interceptions.Pending()})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/interceptions/"), "/")
	if len(parts) > 2 || parts[0] == "" {
		interceptionError(w, http.StatusNotFound, intercept.ErrNotFound)
		return
	}
	action, allow := "", "GET, PATCH"
	if len(parts) == 2 {
		action, allow = parts[1], "POST"
		if action != "release" && action != "cancel" && action != "validate" {
			interceptionError(w, http.StatusNotFound, errors.New("unknown action"))
			return
		}
	} else if r.Method == http.MethodGet {
		detail, err := s.interceptions.Get(parts[0])
		if err != nil {
			interceptionError(w, http.StatusNotFound, err)
			return
		}
		s.writeInterception(w, detail)
		return
	} else if r.Method == http.MethodPatch {
		action = "save"
	}
	if (len(parts) == 1 && r.Method != http.MethodPatch) || (len(parts) == 2 && r.Method != http.MethodPost) {
		w.Header().Set("Allow", allow)
		interceptionError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	var edit intercept.Edit
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&edit)
	if err == nil {
		if decoder.Decode(new(any)) != io.EOF {
			err = errors.New("expected one JSON object")
		}
	}
	if err != nil || edit.Revision == 0 || (action == "cancel" && (edit.Body != nil || edit.Headers != nil)) {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		if err == nil {
			err = errors.New("a positive revision is required; cancel accepts no edits")
		}
		interceptionError(w, status, err)
		return
	}
	detail, err := s.interceptions.Apply(parts[0], action, edit)
	if err != nil {
		status := http.StatusBadRequest
		var invalid *intercept.ValidationError
		switch {
		case errors.Is(err, intercept.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, intercept.ErrConflict):
			status = http.StatusConflict
		case errors.As(err, &invalid):
			status = http.StatusUnprocessableEntity
		}
		interceptionError(w, status, err)
		return
	}
	s.writeInterception(w, detail)
}
