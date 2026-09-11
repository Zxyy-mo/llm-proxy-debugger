package store

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func (s *Store) HistoryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, "/api/history/") && r.URL.Path != "/api/history/cleanup" {
		trace := strings.TrimPrefix(r.URL.Path, "/api/history/")
		if r.Method == http.MethodGet {
			log, ok := s.Log(trace)
			if !ok {
				writeError(w, 404, "request not found")
				return
			}
			json.NewEncoder(w).Encode(log)
			return
		}
		if r.Method != http.MethodDelete {
			w.Header().Set("Allow", "GET, DELETE")
			writeError(w, 405, "method not allowed")
			return
		}
		removed, skipped, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == trace })
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		if skipped > 0 {
			writeError(w, 409, "进行中的请求不能删除，请先结束或取消")
			return
		}
		if removed == 0 {
			writeError(w, 404, "request not found")
			return
		}
		json.NewEncoder(w).Encode(map[string]int{"deleted": removed})
		return
	}
	if r.URL.Path == "/api/history/cleanup" {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeError(w, 405, "method not allowed")
			return
		}
		var input struct {
			Before    string `json:"before"`
			SessionID string `json:"session_id"`
			All       bool   `json:"all"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
		d.DisallowUnknownFields()
		if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
			writeError(w, 400, "invalid cleanup request")
			return
		}
		if !input.All && input.Before == "" && input.SessionID == "" {
			writeError(w, 400, "cleanup requires before, session_id or explicit all=true")
			return
		}
		var before time.Time
		if input.Before != "" {
			var err error
			before, err = time.Parse(time.RFC3339, input.Before)
			if err != nil {
				writeError(w, 400, "before must be RFC3339")
				return
			}
		}
		removed, skipped, err := s.cleanup(func(log RequestLog) bool {
			stamp, _ := time.Parse(time.RFC3339Nano, log.Time)
			return (input.SessionID == "" || log.SessionID == input.SessionID) && (before.IsZero() || stamp.Before(before))
		})
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		json.NewEncoder(w).Encode(map[string]int{"deleted": removed, "active_skipped": skipped})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, 405, "method not allowed")
		return
	}
	query := r.URL.Query()
	limit := 50
	offset := 0
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			writeError(w, 400, "limit must be 1–200")
			return
		}
		limit = value
	}
	if raw := query.Get("cursor"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			writeError(w, 400, "invalid cursor")
			return
		}
		offset = value
	}
	search := strings.ToLower(query.Get("q"))
	items := []RequestLog{}
	total := 0
	s.RLock()
	ordered := s.orderedRecords()
	for i := len(ordered) - 1; i >= 0; i-- {
		rec := ordered[i]
		log := s.displayLog(rec)
		if query.Get("model") != "" && log.Model != query.Get("model") {
			continue
		}
		if query.Get("status") != "" && log.Status != query.Get("status") {
			continue
		}
		if query.Get("session_id") != "" && log.SessionID != query.Get("session_id") {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(log.TraceID+" "+log.SessionID+" "+log.Model+" "+log.Path+" "+log.Summary+" "+log.Error+" "+log.Correlation.ResponseID+" "+log.Correlation.PreviousResponseID), search) {
			continue
		}
		if total >= offset && len(items) < limit {
			items = append(items, log)
		}
		total++
	}
	s.RUnlock()
	next := ""
	if offset+len(items) < total {
		next = strconv.Itoa(offset + len(items))
	}
	json.NewEncoder(w).Encode(map[string]any{"items": items, "total": total, "next_cursor": next, "storage": s.PersistenceStatus()})
}

func (s *Store) cleanup(match func(RequestLog) bool) (int, int, error) {
	s.Lock()
	deleted := map[string]bool{}
	removedRecords := map[string]*record{}
	files := []string{}
	skipped := 0
	for id, rec := range s.records {
		if !match(rec.log) {
			continue
		}
		if rec.log.Status == "running" || rec.log.Status == "pending" {
			skipped++
			continue
		}
		deleted[id] = true
		removedRecords[id] = rec
		if rec.capture != nil {
			files = append(files, rec.capture.Original.FilePath)
			if rec.capture.Outgoing != nil {
				files = append(files, rec.capture.Outgoing.FilePath)
			}
		}
		if rec.response != nil {
			files = append(files, rec.response.Path)
		}
		if rec.upstreamResponse != nil {
			files = append(files, rec.upstreamResponse.Path)
		}
		delete(s.records, id)
	}
	for _, index := range []map[string]map[string]bool{s.owners, s.waiters, s.children, s.history} {
		for key, ids := range index {
			for id := range deleted {
				delete(ids, id)
			}
			if len(ids) == 0 {
				delete(index, key)
			}
		}
	}
	for key, id := range s.aliases {
		removed := removedRecords[id]
		if removed == nil {
			continue
		}
		// Only a survivor with its own matching identity evidence can inherit
		// an alias. An inferred descendant may later detach after an edit.
		var successor *record
		for _, candidate := range s.records {
			if candidate.input.Scope == removed.input.Scope && candidate.log.SessionID == removed.log.SessionID &&
				candidate.hasIdentityAlias(key) &&
				(successor == nil || candidate.sequence < successor.sequence) {
				successor = candidate
			}
		}
		if successor == nil {
			delete(s.aliases, key)
		} else {
			s.aliases[key] = successor.log.TraceID
		}
	}
	visibleSessions := map[string]bool{}
	for _, rec := range s.records {
		visibleSessions[rec.log.SessionID] = true
		if deleted[rec.log.Correlation.ParentTraceID] {
			parent := rec.log.Correlation.ParentTraceID
			rec.log.Correlation.ParentTraceID = ""
			if rec.log.Correlation.ParentReference == "" {
				rec.log.Correlation.ParentReference = parent
			}
			rec.log.Correlation.Warning = "deleted_parent"
			s.touch(rec)
		}
	}
	for id := range s.sessions {
		if !visibleSessions[id] {
			delete(s.sessions, id)
		}
	}
	// A surviving capture still owns its immutable namespace, even if later
	// correlation moved it elsewhere or it was loaded from legacy history.
	keep := map[string]bool{}
	for _, rec := range s.records {
		keep[rec.privacyScope] = true
	}
	s.Privacy.ForgetScopes(keep)
	s.revision++
	s.changed()
	s.Unlock()
	if err := s.Flush(); err != nil {
		return len(deleted), skipped, err
	}
	var failure error
	for _, path := range files {
		if s.safeCapturePath(path) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				failure = err
			}
		}
	}
	if p := s.persistence; p != nil {
		p.mu.Lock()
		_, err := p.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		p.mu.Unlock()
		if failure == nil {
			failure = err
		}
	}
	return len(deleted), skipped, failure
}

func (rec *record) hasIdentityAlias(key string) bool {
	for _, identity := range rec.input.Identities {
		if referenceKey(rec.input.Scope, identity.Kind, identity.Value) == key {
			return true
		}
	}
	// Conversation IDs observed in a response are explicit identity evidence
	// too; membership or an inferred parent link alone is not evidence.
	if id := rec.log.Correlation.ConversationID; id != "" {
		return referenceKey(rec.input.Scope, "conversation", id) == key
	}
	return false
}
