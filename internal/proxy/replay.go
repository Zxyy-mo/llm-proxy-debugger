package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/export"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/replay"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/google/uuid"
)

const (
	defaultReplayTimeout = 600
	maxReplayTimeout     = 3600
)

type replayKey struct{}

// replayOptions travel in the request context so that only server-side code
// can mark a request as a replay or bypass rules; clients cannot spoof them
// through headers.
type replayOptions struct {
	TraceID   string
	Info      store.ReplayInfo
	SkipRules bool
	Selection *provider.Selection
	Timeout   time.Duration
	started   chan struct{}
	once      sync.Once
}

func (o *replayOptions) markStarted() {
	o.once.Do(func() { close(o.started) })
}

func replayFrom(ctx context.Context) *replayOptions {
	opts, _ := ctx.Value(replayKey{}).(*replayOptions)
	return opts
}

// interrupted reports why the caller's context ended. A replay deadline is a
// failure of the replay itself, not a client cancellation, and must not mark
// the record as canceled.
func (s *Server) interrupted(r *http.Request, traceID string) (int, error) {
	err := r.Context().Err()
	if err == nil {
		return 0, nil
	}
	if opts := replayFrom(r.Context()); opts != nil && errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, fmt.Errorf("replay timed out after %s", opts.Timeout)
	}
	s.store.MarkCanceled(traceID)
	return 499, err
}

// replayWriter discards the upstream response: the recorder and the store
// already capture what the debugger shows, and no client is waiting.
type replayWriter struct {
	header http.Header
	status int
}

func (w *replayWriter) Header() http.Header         { return w.header }
func (w *replayWriter) WriteHeader(code int)        { w.status = code }
func (w *replayWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *replayWriter) Flush()                      {}

type credentialValue struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type replayRequest struct {
	TraceID        string            `json:"trace_id"`
	Source         string            `json:"source"`
	Body           *string           `json:"body,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Credentials    []credentialValue `json:"credentials,omitempty"`
	IdempotencyKey string            `json:"idempotency_key,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

type replayPlan struct {
	traceID     string
	method      string
	url         *url.URL
	host        string
	header      http.Header
	body        []byte
	info        store.ReplayInfo
	skipRules   bool
	timeout     time.Duration
	missing     []string
	fingerprint string
	selection   *provider.Selection
}

type replayError struct {
	status int
	err    error
}

func (e *replayError) Error() string { return e.err.Error() }

func rejectReplay(status int, format string, args ...any) error {
	return &replayError{status: status, err: fmt.Errorf(format, args...)}
}

func generatingEndpoint(path string) bool {
	path = strings.TrimSuffix(path, "/")
	return strings.HasSuffix(path, "/messages") || strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/responses")
}

func validCredentialValue(value string) error {
	if value == "" || len(value) > 8192 {
		return errors.New("credential values must be 1–8192 characters")
	}
	if strings.Contains(value, "****") {
		return errors.New("credential value looks like a redacted display value; supply the real secret")
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return errors.New("credential values cannot contain control characters")
		}
	}
	return nil
}

// planReplay resolves the source snapshot, applies validated edits and
// transient credentials, and fixes the destination from the capture and the
// configured upstream. Nothing here retains the supplied credentials.
// requireCredentials is false for validation, which reports what is missing
// instead of refusing, so drafts can be checked before secrets are entered.
func (s *Server) planReplay(req replayRequest, requireCredentials bool) (*replayPlan, error) {
	capture, ok := s.store.RequestSnapshot(req.TraceID)
	if !ok {
		return nil, rejectReplay(http.StatusNotFound, "request not found")
	}
	source := req.Source
	if source == "" {
		source = "original"
		if capture.Outgoing != nil {
			source = "outgoing"
		}
	}
	snapshot := capture.Original
	switch source {
	case "original":
	case "outgoing":
		if capture.Outgoing == nil {
			return nil, rejectReplay(http.StatusUnprocessableEntity, "this request was never forwarded, so it has no outgoing snapshot; replay the original instead")
		}
		snapshot = *capture.Outgoing
	default:
		return nil, rejectReplay(http.StatusBadRequest, "source must be original or outgoing")
	}
	if snapshot.Unavailable != "" {
		return nil, rejectReplay(422, "%s", snapshot.Unavailable)
	}
	if snapshot.Redacted && !snapshot.RawRetained && req.Body == nil {
		return nil, rejectReplay(422, "原文未保留，无法精确重放；请提供完整的新正文")
	}
	forwarding := snapshot.Forwarding
	if forwarding == nil {
		return nil, rejectReplay(http.StatusUnprocessableEntity, "this capture lacks forwarding metadata and cannot be replayed")
	}
	if snapshot.Method != http.MethodPost || !generatingEndpoint(forwarding.Path) {
		return nil, rejectReplay(http.StatusUnprocessableEntity, "only POST requests to /messages, /chat/completions or /responses can be replayed; other operations are not replayed implicitly")
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > maxReplayTimeout {
		return nil, rejectReplay(http.StatusBadRequest, "timeout_seconds must be 1–%d", maxReplayTimeout)
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if req.TimeoutSeconds == 0 {
		timeout = defaultReplayTimeout * time.Second
	}

	path, rawPath := forwarding.Path, forwarding.RawPath
	var selection *provider.Selection
	if source == "outgoing" {
		endpoint := provider.Provider{BaseURL: s.target.String(), Protocol: "passthrough"}
		if forwarding.ProviderID != "" {
			var ok bool
			endpoint, ok = s.providers.Get(forwarding.ProviderID)
			if !ok {
				return nil, rejectReplay(409, "captured Provider %s no longer exists", forwarding.ProviderID)
			}
		}
		target, _ := url.Parse(endpoint.BaseURL)
		if forwarding.Scheme != target.Scheme || forwarding.Host != target.Host || (forwarding.BaseURL != "" && strings.TrimSuffix(forwarding.BaseURL, "/") != strings.TrimSuffix(endpoint.BaseURL, "/")) {
			return nil, rejectReplay(409, "the captured upstream %s://%s differs from its current provider configuration; refusing to replay to a different destination", forwarding.Scheme, forwarding.Host)
		}
		if prefix := strings.TrimSuffix(target.Path, "/"); prefix != "" {
			if path != prefix && !strings.HasPrefix(path, prefix+"/") {
				return nil, rejectReplay(http.StatusConflict, "the captured upstream path %q does not start with the configured prefix %q", path, prefix)
			}
			escaped := (&url.URL{Path: path, RawPath: rawPath}).EscapedPath()
			escapedPrefix := strings.TrimSuffix(target.EscapedPath(), "/")
			if escaped != escapedPrefix && !strings.HasPrefix(escaped, escapedPrefix+"/") {
				return nil, rejectReplay(http.StatusConflict, "the captured escaped path does not match the configured upstream prefix")
			}
			rawPath = strings.TrimPrefix(escaped, escapedPrefix)
			path = strings.TrimPrefix(path, prefix)
			if path == "" {
				path, rawPath = "/", "/"
			}
		}
		// This body is already in the destination's wire protocol. Keep its
		// exact path/model and never apply the provider's conversion again.
		endpoint.Protocol = "passthrough"
		selection = &provider.Selection{Endpoints: []provider.Provider{endpoint}}
	}

	body, err := export.BodyBytes(snapshot)
	if err != nil {
		return nil, rejectReplay(http.StatusUnprocessableEntity, "captured body cannot be decoded: %v", err)
	}
	header := forwarding.Headers.Clone()
	baseHeaders := intercept.EditableHeaders(header)
	candidateHeaders := maps.Clone(baseHeaders)
	if req.Headers != nil {
		candidateHeaders, err = intercept.NormalizeHeaders(req.Headers)
		if err != nil {
			return nil, rejectReplay(http.StatusUnprocessableEntity, "%v", err)
		}
	}
	candidateBody := snapshot.Body
	if req.Body != nil {
		candidateBody = *req.Body
	}
	modified := candidateBody != snapshot.Body || !maps.Equal(candidateHeaders, baseHeaders)
	if modified {
		if snapshot.BodyEncoding != "" {
			return nil, rejectReplay(http.StatusUnprocessableEntity, "compressed or binary bodies can only be replayed unchanged")
		}
		if err := intercept.Validate(forwarding.Path, snapshot.Body, candidateBody, candidateHeaders); err != nil {
			return nil, rejectReplay(http.StatusUnprocessableEntity, "%v", err)
		}
		body = []byte(candidateBody)
		for name := range baseHeaders {
			header.Del(name)
		}
		for name, value := range candidateHeaders {
			header.Set(name, value)
		}
	}
	if selection == nil {
		chosen := s.selectProvider(correlation.ExtractRequest(header, path, body, s.target.String()).Model)
		selection = &chosen
	}

	required := make(map[string]store.Credential)
	for _, credential := range snapshot.Credentials {
		if selection.Endpoints[0].KeyEnv != "" && credential.Kind == "header" && (strings.EqualFold(credential.Name, "Authorization") || strings.EqualFold(credential.Name, "X-API-Key") || strings.EqualFold(credential.Name, "api-key")) {
			continue
		}
		required[credential.Kind+"\x00"+strings.ToLower(credential.Name)] = credential
	}
	supplied := make(map[string]bool)
	var query []string
	if forwarding.Query != "" {
		query = append(query, forwarding.Query)
	}
	names := make([]string, 0, len(req.Credentials))
	for _, credential := range req.Credentials {
		key := credential.Kind + "\x00" + strings.ToLower(credential.Name)
		if supplied[key] || credential.Name == "" {
			return nil, rejectReplay(http.StatusUnprocessableEntity, "credential %s %q is empty or repeated", credential.Kind, credential.Name)
		}
		if err := validCredentialValue(credential.Value); err != nil {
			return nil, rejectReplay(http.StatusUnprocessableEntity, "%s %q: %v", credential.Kind, credential.Name, err)
		}
		switch credential.Kind {
		case "header":
			if !export.IsCredentialHeader(credential.Name) {
				return nil, rejectReplay(http.StatusUnprocessableEntity, "%q is not a credential header; editable headers belong in headers", credential.Name)
			}
			header.Set(credential.Name, credential.Value)
		case "query":
			if !export.IsSecretParameter(credential.Name) {
				return nil, rejectReplay(http.StatusUnprocessableEntity, "%q is not a secret query parameter", credential.Name)
			}
			query = append(query, url.QueryEscape(credential.Name)+"="+url.QueryEscape(credential.Value))
		case "url":
			if credential.Name != "userinfo" {
				return nil, rejectReplay(http.StatusUnprocessableEntity, "url credentials support only userinfo")
			}
			// HTTP carries URL userinfo as Basic authentication; the transport
			// never sends it as part of the request target.
			header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(credential.Value)))
		default:
			return nil, rejectReplay(http.StatusUnprocessableEntity, "credential kind must be header, query or url")
		}
		supplied[key] = true
		names = append(names, credential.Kind+":"+strings.ToLower(credential.Name))
	}
	var missing []string
	for key, credential := range required {
		if !credential.Replayable {
			return nil, rejectReplay(http.StatusUnprocessableEntity, "%s uses the %s scheme, which must be re-signed for each request; replay is not supported for signed requests", credential.Name, credential.Scheme)
		}
		if !supplied[key] {
			missing = append(missing, credential.Kind+" "+credential.Name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 && requireCredentials {
		return nil, rejectReplay(http.StatusUnprocessableEntity, "the original request carried credentials that are not stored; supply them for this replay: %s", strings.Join(missing, ", "))
	}
	sort.Strings(names)

	editedHeaders, _ := json.Marshal(candidateHeaders)
	bodyMarker := "unchanged"
	if req.Body != nil {
		bodyMarker = candidateBody
	}
	plan := &replayPlan{
		selection: selection,
		traceID:   uuid.New().String(), method: snapshot.Method,
		url:  &url.URL{Path: path, RawPath: rawPath, RawQuery: strings.Join(query, "&")},
		host: forwarding.Host, header: header, body: body,
		info:      store.ReplayInfo{Of: req.TraceID, Source: source, Modified: modified},
		skipRules: source == "outgoing", timeout: timeout, missing: missing,
		fingerprint: correlation.Hash(req.TraceID, source, bodyMarker, string(editedHeaders), strings.Join(names, ","), strconv.Itoa(int(timeout/time.Second)), selection.Endpoints[0].BaseURL, selection.Endpoints[0].ID, selection.RouteID),
	}
	return plan, nil
}

// execute runs the plan through the normal proxy pipeline so recording,
// correlation and live events behave exactly like a client request.
func (s *Server) execute(ctx context.Context, plan *replayPlan, opts *replayOptions) replay.Outcome {
	ctx = context.WithValue(ctx, replayKey{}, opts)
	request, err := http.NewRequestWithContext(ctx, plan.method, plan.url.String(), bytes.NewReader(plan.body))
	if err != nil {
		opts.markStarted()
		return replay.Outcome{State: "error", Error: err.Error()}
	}
	request.Host = plan.host
	request.Header = plan.header.Clone()
	request.ContentLength = int64(len(plan.body))
	s.handleHTTP(&replayWriter{header: make(http.Header)}, request, time.Now(), "replay")
	opts.markStarted()
	log, ok := s.store.Log(plan.traceID)
	if !ok {
		return replay.Outcome{State: "error", Error: "replay produced no record"}
	}
	outcome := replay.Outcome{State: log.Status, Error: log.Error, StatusCode: log.StatusCode}
	if outcome.State != "done" && outcome.State != "canceled" {
		outcome.State = "error"
	}
	return outcome
}

func replayJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(value)
}

func replayFailure(w http.ResponseWriter, err error) {
	var rejected *replayError
	if errors.As(err, &rejected) {
		replayJSON(w, rejected.status, map[string]string{"error": rejected.err.Error()})
		return
	}
	replayJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func decodeReplay(w http.ResponseWriter, r *http.Request) (replayRequest, bool) {
	var req replayRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&req)
	if err == nil && decoder.Decode(new(any)) != io.EOF {
		err = errors.New("expected one JSON object")
	}
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		replayJSON(w, status, map[string]string{"error": err.Error()})
		return req, false
	}
	if req.TraceID == "" {
		replayJSON(w, http.StatusBadRequest, map[string]string{"error": "trace_id is required"})
		return req, false
	}
	return req, true
}

func (s *Server) createReplay(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeReplay(w, r)
	if !ok {
		return
	}
	if req.IdempotencyKey == "" || len(req.IdempotencyKey) > 200 {
		replayJSON(w, http.StatusBadRequest, map[string]string{"error": "idempotency_key is required (1–200 characters) so retries do not execute twice"})
		return
	}
	plan, err := s.planReplay(req, true)
	if err != nil {
		replayFailure(w, err)
		return
	}
	opts := &replayOptions{TraceID: plan.traceID, Info: plan.info, SkipRules: plan.skipRules, Selection: plan.selection, Timeout: plan.timeout, started: make(chan struct{})}
	opts.Info.ID = uuid.New().String()
	record := replay.Record{ID: opts.Info.ID, TraceID: plan.traceID, Of: plan.info.Of, Source: plan.info.Source, Modified: plan.info.Modified}
	record, created, err := s.replays.Start(req.IdempotencyKey, plan.fingerprint, record, plan.timeout, func(ctx context.Context) replay.Outcome {
		return s.execute(ctx, plan, opts)
	})
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, replay.ErrPersistence) || errors.Is(err, replay.ErrClosed) {
			status = http.StatusServiceUnavailable
		}
		replayJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
		// Let the new trace exist before answering so the UI can select it.
		select {
		case <-opts.started:
		case <-s.lifecycle.Done():
		case <-time.After(3 * time.Second):
		}
		record, _ = s.replays.Get(record.ID)
	}
	replayJSON(w, status, record)
}

func (s *Server) validateReplay(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeReplay(w, r)
	if !ok {
		return
	}
	plan, err := s.planReplay(req, false)
	if err != nil {
		replayFailure(w, err)
		return
	}
	missing := plan.missing
	if missing == nil {
		missing = []string{}
	}
	replayJSON(w, http.StatusOK, map[string]any{
		"valid": true, "modified": plan.info.Modified, "source": plan.info.Source,
		"body_bytes": len(plan.body), "timeout_seconds": int(plan.timeout / time.Second),
		"missing_credentials": missing,
	})
}

// ReplaysHandler serves POST /api/replays, POST /api/replays/validate,
// GET /api/replays, GET /api/replays/{id} and POST /api/replays/{id}/cancel.
func (s *Server) ReplaysHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/api/replays"), "/")
	methodNotAllowed := func(allow string) {
		w.Header().Set("Allow", allow)
		replayJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
	switch {
	case rest == "":
		switch r.Method {
		case http.MethodGet:
			replayJSON(w, http.StatusOK, s.replays.List())
		case http.MethodPost:
			s.createReplay(w, r)
		default:
			methodNotAllowed("GET, POST")
		}
	case rest == "validate":
		if r.Method != http.MethodPost {
			methodNotAllowed("POST")
			return
		}
		s.validateReplay(w, r)
	default:
		id, action, _ := strings.Cut(rest, "/")
		switch action {
		case "":
			if r.Method != http.MethodGet {
				methodNotAllowed("GET")
				return
			}
			record, err := s.replays.Get(id)
			if err != nil {
				replayJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
				return
			}
			replayJSON(w, http.StatusOK, record)
		case "cancel":
			if r.Method != http.MethodPost {
				methodNotAllowed("POST")
				return
			}
			record, err := s.replays.Cancel(id)
			switch {
			case errors.Is(err, replay.ErrNotFound):
				replayJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			case errors.Is(err, replay.ErrFinished):
				replayJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "replay": record})
			default:
				replayJSON(w, http.StatusOK, record)
			}
		default:
			replayJSON(w, http.StatusNotFound, map[string]string{"error": "unknown replay action"})
		}
	}
}

// RequestsHandler adds cURL export and exact body download to the capture API.
func (s *Server) RequestsHandler(w http.ResponseWriter, r *http.Request) {
	trace, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/requests/"), "/")
	if action == "" {
		s.store.RequestsHandler(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Allow", http.MethodGet)
		replayJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	capture, ok := s.store.RequestSnapshot(trace)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		replayJSON(w, http.StatusNotFound, map[string]string{"error": "request not found"})
		return
	}
	capture = s.store.DisplayCapture(trace, capture)
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "original"
		if capture.Outgoing != nil {
			source = "outgoing"
		}
	}
	snapshot := capture.Original
	switch source {
	case "original":
	case "outgoing":
		if capture.Outgoing == nil {
			w.Header().Set("Content-Type", "application/json")
			replayJSON(w, http.StatusNotFound, map[string]string{"error": "this request was never forwarded; no outgoing snapshot exists"})
			return
		}
		snapshot = *capture.Outgoing
	default:
		w.Header().Set("Content-Type", "application/json")
		replayJSON(w, http.StatusBadRequest, map[string]string{"error": "source must be original or outgoing"})
		return
	}
	switch action {
	case "curl":
		w.Header().Set("Content-Type", "application/json")
		result, err := export.Build(trace, source, snapshot, export.Options{GatewayHost: export.GatewayHost(s.cfg.ListenAddr)})
		if err != nil {
			replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		if snapshot.Redacted {
			result.Notes = append(result.Notes, "正文包含脱敏占位符，与原始字节不同；发送前请按需还原或编辑。")
		}
		replayJSON(w, http.StatusOK, result)
	case "body":
		body, err := export.BodyBytes(snapshot)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="`+export.BodyFileName(trace, source, snapshot)+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	default:
		w.Header().Set("Content-Type", "application/json")
		replayJSON(w, http.StatusNotFound, map[string]string{"error": "unknown request action"})
	}
}
