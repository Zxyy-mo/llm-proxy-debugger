package store

import (
	"maps"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/observation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/privacy"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
)

func referenceKey(scope, kind, id string) string {
	return correlation.Hash(scope, kind, id)
}

func addIndex(index map[string]map[string]bool, key, traceID string) {
	if index[key] == nil {
		index[key] = make(map[string]bool)
	}
	index[key][traceID] = true
}

func (s *Store) touch(rec *record) {
	s.revision++
	rec.log.Revision = s.revision
	s.changed()
}

func copyLog(log RequestLog) RequestLog {
	if log.WebSocket != nil {
		info := *log.WebSocket
		log.WebSocket = &info
	}
	log.Headers = maps.Clone(log.Headers)
	log.Tools = append([]observation.ToolCall(nil), log.Tools...)
	for i := range log.Tools {
		if log.Tools[i].Duration != nil {
			value := *log.Tools[i].Duration
			log.Tools[i].Duration = &value
		}
	}
	if log.TTFB != nil {
		value := *log.TTFB
		log.TTFB = &value
	}
	if log.TTFC != nil {
		value := *log.TTFC
		log.TTFC = &value
	}
	if log.Interception != nil {
		metadata := *log.Interception
		log.Interception = &metadata
	}
	if log.Replay != nil {
		replay := *log.Replay
		log.Replay = &replay
	}
	if log.Route != nil {
		route := *log.Route
		route.Attempts = append([]RouteAttempt(nil), route.Attempts...)
		log.Route = &route
	}
	return log
}

// UpdateInput reindexes history after rule or editor changes. Identity and
// explicit references are validated as immutable at the editing boundary.
func (s *Store) UpdateInput(traceID string, input correlation.Request) RequestLog {
	s.Lock()
	defer s.Unlock()
	rec := s.records[traceID]
	if rec == nil {
		return RequestLog{}
	}
	changed := rec.input.ContextHash != input.ContextHash || !slices.Equal(rec.input.Messages, input.Messages)
	rec.input = input
	rec.privacyHistory = nil
	if session := s.sessions[rec.log.SessionID]; session.Label == rec.log.Summary {
		session.Label = input.Summary
	}
	rec.log.Model, rec.log.Protocol, rec.log.Summary = input.Model, input.Protocol, input.Summary
	if changed {
		c := &rec.log.Correlation
		if c.LinkSource == "history" {
			delete(s.children[c.ParentTraceID], traceID)
			c.ParentTraceID, c.LinkSource, c.Confidence = "", "", ""
			if !rec.fixedSession {
				c.SessionSource = "new"
				s.moveTree(rec, rec.preferredSession)
			}
		}
		if c.Warning == "ambiguous_history" || c.Warning == "history_limit" {
			c.Warning = ""
		}
		if input.HistoryLimited {
			c.Warning = "history_limit"
		}
		if c.ParentReference == "" {
			s.matchHistory(rec)
		}
	}
	s.touch(rec)
	return s.displayLog(rec)
}

// UpdateOutboundInput indexes the privacy-substituted transcript without
// treating substitution itself as new conversation evidence. Keep only the
// bounded pre-substitution fingerprints so a client resubmitting its original
// prompt plus the actual assistant reply can match the same conversation.
func (s *Store) UpdateOutboundInput(traceID string, input correlation.Request) RequestLog {
	s.Lock()
	defer s.Unlock()
	rec := s.records[traceID]
	if rec == nil {
		return RequestLog{}
	}
	before := rec.input
	stateful := before.PreviousResponseID != ""
	if before.Protocol == "responses" {
		for _, id := range before.Identities {
			stateful = stateful || id.Kind == "conversation"
		}
	}
	if rec.privacy.Outbound && !stateful && len(before.Messages) > 0 {
		rec.privacyHistory = &correlation.Request{
			ContextHash: before.ContextHash, Messages: slices.Clone(before.Messages), HasUser: before.HasUser,
		}
	}
	// Privacy can substitute JSON identity values, but that is not a change
	// to the explicit evidence captured at admission. Keep it available for
	// alias ownership and later correlation while indexing outgoing messages.
	input.Identities = slices.Clone(before.Identities)
	input.ParentTraceID, input.PreviousResponseID = before.ParentTraceID, before.PreviousResponseID
	rec.input = input
	if session := s.sessions[rec.log.SessionID]; session.Label == rec.log.Summary {
		session.Label = input.Summary
	}
	rec.log.Model, rec.log.Protocol, rec.log.Summary = input.Model, input.Protocol, input.Summary
	s.touch(rec)
	return s.displayLog(rec)
}

// Begin records a request before forwarding it. Running requests therefore
// survive a browser reload and can already act as explicit trace parents.
func (s *Store) Begin(log RequestLog, input correlation.Request, frozen ...privacy.Policy) RequestLog {
	return s.begin(log, input, nil, frozen)
}

// BeginWithBody allocates correlation and privacy identity before projecting
// the complete body and applying the display limit, all under one store lock.
// Snapshots can never observe a raw or credential-scoped intermediate preview.
func (s *Store) BeginWithBody(log RequestLog, input correlation.Request, body []byte, limit int, frozen ...privacy.Policy) RequestLog {
	return s.begin(log, input, &requestPreview{body: body, limit: limit}, frozen)
}

func (s *Store) begin(log RequestLog, input correlation.Request, preview *requestPreview, frozen []privacy.Policy) RequestLog {
	s.Lock()
	defer s.Unlock()
	if existing := s.records[log.TraceID]; existing != nil {
		return s.displayLog(existing)
	}
	if log.Time == "" {
		log.Time = time.Now().Format(time.RFC3339Nano)
	}
	log.Model, log.Protocol, log.Summary = input.Model, input.Protocol, input.Summary
	log.Status = "running"
	log.TokenSources = protocol.UnknownSources()
	log.ResponseStored = "missing"
	log.Correlation = Correlation{SessionSource: "new", PreviousResponseID: input.PreviousResponseID}
	log.SessionID = "auto:" + log.TraceID
	for _, id := range input.Identities {
		switch id.Kind {
		case "conversation":
			if log.Correlation.ConversationID == "" {
				log.Correlation.ConversationID = id.Value
			}
		case "thread":
			if log.Correlation.ThreadID == "" {
				log.Correlation.ThreadID = id.Value
			}
		}
	}
	if input.ParentTraceID != "" {
		log.Correlation.ParentReference = input.ParentTraceID
		log.Correlation.LinkSource = "parent_trace_id"
	} else if input.PreviousResponseID != "" {
		log.Correlation.ParentReference = input.PreviousResponseID
		log.Correlation.LinkSource = "previous_response_id"
	}
	label := input.Summary
	summaryLabel := true
	if len(input.Identities) > 0 {
		primary := input.Identities[0]
		log.SessionID, label = s.identitySession(input.Scope, primary), primary.Value
		log.Correlation.SessionSource = primary.Source
		summaryLabel = false
	} else if log.Correlation.ParentReference != "" {
		log.SessionID = "pending:" + referenceKey(input.Scope, log.Correlation.LinkSource, log.Correlation.ParentReference)[:24]
		log.Correlation.SessionSource = log.Correlation.LinkSource
		label = log.Correlation.ParentReference
		summaryLabel = false
	} else if log.Replay != nil {
		// A replay without its own conversation evidence stays next to the request
		// it reproduces. Membership is provenance, not a parent relation, so no
		// edge is invented here; history matching may still attach a real parent.
		if source := s.records[log.Replay.Of]; source != nil {
			log.SessionID = source.log.SessionID
			log.Correlation.SessionSource = "replay"
		}
	}
	if input.HistoryLimited {
		log.Correlation.Warning = "history_limit"
	}
	s.sequence++
	rec := &record{
		log: copyLog(log), input: input, sequence: s.sequence, privacy: s.Privacy.Policy(),
		fixedSession: len(input.Identities) > 0, preferredSession: log.SessionID,
	}
	if len(frozen) > 0 {
		rec.privacy = frozen[0]
	}
	s.records[log.TraceID] = rec
	createdSession := s.sessions[log.SessionID] == nil
	// Initial correlation may move this record to an existing session. Keep
	// provisional labels neutral until its final privacy namespace is allocated.
	s.ensureSession(rec, "请求")
	s.touch(rec)
	for _, id := range input.Identities {
		key := referenceKey(input.Scope, id.Kind, id.Value)
		if anchor := s.aliases[key]; anchor == "" {
			s.aliases[key] = log.TraceID
		} else if s.records[anchor].log.SessionID != rec.log.SessionID {
			rec.log.Correlation.Warning = "conflicting_identifier"
		}
	}
	if log.Correlation.ParentReference != "" {
		kind := "response"
		if log.Correlation.LinkSource == "parent_trace_id" {
			kind = "trace"
		}
		key := referenceKey(input.Scope, kind, log.Correlation.ParentReference)
		addIndex(s.waiters, key, log.TraceID)
		s.resolveWaiters(key)
	} else {
		s.matchHistory(rec)
	}
	key := referenceKey(input.Scope, "trace", log.TraceID)
	addIndex(s.owners, key, log.TraceID)
	s.resolveWaiters(key)
	rec.privacyScope = s.admissionPrivacyScope(rec)
	if preview != nil {
		s.setInitialPreview(rec, *preview)
	}
	if rec.privacy.Record {
		rec.log.Summary = s.Privacy.Text(rec.privacy, rec.privacyScope, rec.log.Summary)
		rec.input.Summary = rec.log.Summary
		if !rec.privacy.RetainRaw {
			rec.log = s.displayLog(rec)
		}
	}
	if summaryLabel {
		label = rec.log.Summary
	}
	if rec.privacy.Record {
		label = s.Privacy.Text(rec.privacy, rec.privacyScope, label)
	}
	if createdSession {
		visible := false
		for _, candidate := range s.records {
			if candidate.log.SessionID == log.SessionID {
				visible = true
				break
			}
		}
		if visible {
			if label == "" {
				label = rec.log.Model
			}
			if label == "" {
				label = log.SessionID
			}
			s.sessions[log.SessionID].Label = label
		} else {
			delete(s.sessions, log.SessionID)
		}
	}
	return s.displayLog(rec)
}

func (s *Store) identitySession(scope string, identity correlation.Identity) string {
	key := referenceKey(scope, identity.Kind, identity.Value)
	if anchor := s.records[s.aliases[key]]; anchor != nil {
		return anchor.log.SessionID
	}
	id := identity.Value
	if identity.Kind != "session" {
		id = identity.Kind + ":" + id
	}
	// Preserve legacy X-Session-ID names while keeping different credentials
	// and identifier namespaces from silently sharing an internal session.
	if s.sessions[id] != nil {
		id += ":" + key[:12]
	}
	return id
}

func (s *Store) ensureSession(rec *record, label string) {
	created, err := time.Parse(time.RFC3339Nano, rec.log.Time)
	if err != nil {
		created = time.Now()
	}
	if session := s.sessions[rec.log.SessionID]; session != nil {
		if created.Before(session.CreatedAt) {
			session.CreatedAt = created
		}
		return
	}
	if label == "" {
		label = rec.log.Model
	}
	if label == "" {
		label = rec.log.SessionID
	}
	s.sessions[rec.log.SessionID] = &Session{ID: rec.log.SessionID, Label: label, CreatedAt: created}
}

func (s *Store) matchHistory(rec *record) {
	keys := rec.input.Prefixes()
	for i := len(keys) - 1; i >= 0; i-- {
		var candidates []*record
		for id := range s.history[keys[i]] {
			parent := s.records[id]
			if parent == nil || id == rec.log.TraceID || (rec.fixedSession && parent.log.SessionID != rec.log.SessionID) {
				continue
			}
			candidates = append(candidates, parent)
		}
		if len(candidates) > 1 {
			rec.log.Correlation.Warning = "ambiguous_history"
			s.touch(rec)
			return
		}
		if len(candidates) == 1 {
			s.attach(rec, candidates[0], "history", "inferred")
			return
		}
	}
}

func (s *Store) resolveWaiters(key string) {
	for id := range s.waiters[key] {
		rec := s.records[id]
		if rec == nil {
			continue
		}
		if len(s.owners[key]) != 1 {
			warning := "unresolved_parent"
			if len(s.owners[key]) > 1 {
				warning = "ambiguous_parent"
			}
			s.detach(rec, warning)
			continue
		}
		for parentID := range s.owners[key] {
			s.attach(rec, s.records[parentID], rec.log.Correlation.LinkSource, "exact")
		}
	}
}

func (s *Store) detach(rec *record, warning string) {
	c := &rec.log.Correlation
	if c.ParentTraceID == "" && c.Warning == warning {
		return
	}
	delete(s.children[c.ParentTraceID], rec.log.TraceID)
	c.ParentTraceID, c.Warning, c.Confidence = "", warning, "exact"
	if !rec.fixedSession {
		s.moveTree(rec, rec.preferredSession)
	}
	s.touch(rec)
}

func (s *Store) attach(rec, parent *record, source, confidence string) {
	if parent == nil {
		return
	}
	for ancestor := parent; ancestor != nil; ancestor = s.records[ancestor.log.Correlation.ParentTraceID] {
		if ancestor.log.TraceID == rec.log.TraceID {
			s.detach(rec, "cycle")
			return
		}
	}
	c := &rec.log.Correlation
	delete(s.children[c.ParentTraceID], rec.log.TraceID)
	c.ParentTraceID, c.LinkSource, c.Confidence = parent.log.TraceID, source, confidence
	if c.Warning == "unresolved_parent" || c.Warning == "ambiguous_parent" || c.Warning == "cycle" {
		c.Warning = ""
	}
	addIndex(s.children, parent.log.TraceID, rec.log.TraceID)
	if !rec.fixedSession {
		c.SessionSource = source
		s.moveTree(rec, parent.log.SessionID)
	} else if rec.log.SessionID != parent.log.SessionID {
		c.Warning = "session_boundary"
	}
	s.touch(rec)
}

func (s *Store) moveTree(root *record, sessionID string) {
	queue := []*record{root}
	for len(queue) > 0 {
		rec := queue[0]
		queue = queue[1:]
		if rec.log.SessionID != sessionID {
			rec.log.SessionID = sessionID
			s.ensureSession(rec, rec.log.Summary)
			s.touch(rec)
		}
		for childID := range s.children[rec.log.TraceID] {
			if child := s.records[childID]; child != nil && !child.fixedSession {
				queue = append(queue, child)
			}
		}
	}
}

// ObserveResponse registers IDs as soon as a stream exposes them. The caller
// broadcasts a snapshot invalidation if this returns true.
func (s *Store) ObserveResponse(traceID string, response correlation.Response) bool {
	s.Lock()
	defer s.Unlock()
	rec := s.records[traceID]
	if rec == nil {
		return false
	}
	before := s.revision
	s.observeResponse(rec, response)
	return before != s.revision
}

func (s *Store) observeResponse(rec *record, response correlation.Response) {
	if response.Model != "" && rec.log.Model != response.Model {
		rec.log.Model = response.Model
		s.touch(rec)
	}
	c := &rec.log.Correlation
	if response.ID != "" && c.ResponseID != response.ID {
		path := strings.TrimSuffix(rec.log.Path, "/")
		producesResponse := (rec.log.Method == http.MethodPost || rec.log.Method == "WS") &&
			(strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/messages"))
		if producesResponse && c.ResponseID != "" {
			old := referenceKey(rec.input.Scope, "response", c.ResponseID)
			delete(s.owners[old], rec.log.TraceID)
			s.resolveWaiters(old)
		}
		c.ResponseID = response.ID
		s.touch(rec)
		// Retrieving or cancelling a stored response exposes the same ID but is
		// not another generating request and must not make its owner ambiguous.
		if producesResponse {
			key := referenceKey(rec.input.Scope, "response", response.ID)
			addIndex(s.owners, key, rec.log.TraceID)
			s.resolveWaiters(key)
		}
	}
	if response.ConversationID != "" && c.ConversationID != response.ConversationID {
		if c.ConversationID != "" {
			c.Warning = "conflicting_identifier"
			s.touch(rec)
			return
		}
		c.ConversationID = response.ConversationID
		key := referenceKey(rec.input.Scope, "conversation", response.ConversationID)
		if anchor := s.records[s.aliases[key]]; anchor != nil {
			if !rec.fixedSession {
				c.SessionSource = "response:conversation.id"
				s.moveTree(rec, anchor.log.SessionID)
			}
		} else {
			s.aliases[key] = rec.log.TraceID
		}
		s.touch(rec)
	}
}

// Complete replaces the running log while preserving the current authoritative
// session/link metadata, which may have changed since Begin returned.
func (s *Store) Complete(traceID string, log RequestLog, response correlation.Response) RequestLog {
	s.Lock()
	defer s.Unlock()
	rec := s.records[traceID]
	if rec == nil {
		return log
	}
	log.TraceID, log.SessionID, log.Correlation = rec.log.TraceID, rec.log.SessionID, rec.log.Correlation
	log.Model, log.Protocol, log.Summary = rec.log.Model, rec.log.Protocol, rec.log.Summary
	log.Interception, log.WaitDuration = rec.log.Interception, rec.log.WaitDuration
	log.Replay = rec.log.Replay
	log.Tools = rec.log.Tools
	log.Route = rec.log.Route
	log.Status = "done"
	if rec.log.Status == "canceled" {
		log.Status = "canceled"
	} else if log.Error != "" || log.StatusCode >= 400 || log.StatusCode == 0 {
		log.Status = "error"
	}
	rec.log = copyLog(log)
	if rec.privacy.Record && !rec.privacy.RetainRaw {
		rec.log = s.displayLog(rec)
	}
	s.touch(rec)
	s.observeResponse(rec, response)
	if log.Status == "done" {
		if key := rec.input.CompletedKey(response); key != "" {
			addIndex(s.history, key, traceID)
		}
		if rec.privacyHistory != nil {
			if key := rec.privacyHistory.CompletedKey(response); key != "" {
				addIndex(s.history, key, traceID)
			}
		}
	}
	// Keep only final transcript index entries (wire and optional pre-privacy),
	// not every message prefix, to avoid quadratic growth in long conversations.
	rec.input.Messages = nil
	rec.privacyHistory = nil
	return s.displayLog(rec)
}

func (s *Store) orderedRecords() []*record {
	ordered := make([]*record, 0, len(s.records))
	for _, rec := range s.records {
		ordered = append(ordered, rec)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].sequence < ordered[j].sequence })
	return ordered
}

func (s *Store) SessionsSnapshot() map[string]*Session {
	s.RLock()
	defer s.RUnlock()
	result := make(map[string]*Session)
	for _, rec := range s.orderedRecords() {
		id := rec.log.SessionID
		if result[id] == nil {
			session := *s.sessions[id]
			if rec.privacy.Record {
				session.Label = s.Privacy.Text(rec.privacy, rec.privacyScope, session.Label)
			}
			session.Logs = make([]RequestLog, 0)
			result[id] = &session
		}
		result[id].Logs = append(result[id].Logs, s.displayLog(rec))
	}
	return result
}
