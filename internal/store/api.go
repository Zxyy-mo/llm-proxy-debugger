package store

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
)

// SessionsHandler 返回所有会话
func (s *Store) SessionsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	json.NewEncoder(w).Encode(s.SessionsSnapshot())
}

func (s *Store) GraphHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	graph, ok := s.GraphSnapshot(r.URL.Query().Get("session_id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	json.NewEncoder(w).Encode(graph)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (s *Store) RulesSnapshot() []Rule {
	s.RLock()
	defer s.RUnlock()
	return append([]Rule{}, s.Rules...)
}

func decodeRule(w http.ResponseWriter, r *http.Request) (Rule, bool) {
	rule := Rule{WaitSeconds: 30, TimeoutAction: "forward"}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rule); err != nil {
		writeError(w, http.StatusBadRequest, "invalid rule JSON: "+err.Error())
		return rule, false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "expected one rule JSON object")
		return rule, false
	}
	rule.PathMatch = strings.TrimSpace(rule.PathMatch)
	if rule.PathMatch == "" || rule.WaitSeconds < 1 || rule.WaitSeconds > 3600 || !slices.Contains([]string{"forward", "cancel"}, rule.TimeoutAction) {
		writeError(w, http.StatusBadRequest, "path_match is required; wait_seconds must be 1–3600; timeout_action must be forward or cancel")
		return rule, false
	}
	return rule, true
}

// RulesHandler manages snapshots and future-request rule configuration. Pending
// requests retain the policy that matched them at capture time.
func (s *Store) RulesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if strings.HasPrefix(r.URL.Path, "/api/rules/") {
		s.ruleHandler(w, r, strings.TrimPrefix(r.URL.Path, "/api/rules/"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(s.RulesSnapshot())
	case http.MethodPost:
		if rule, ok := decodeRule(w, r); ok {
			s.Lock()
			rule.ID = uuid.New().String()
			s.Rules = append(s.Rules, rule)
			s.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(rule)
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Store) ruleHandler(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var rule Rule
	if r.Method == http.MethodPut {
		var ok bool
		rule, ok = decodeRule(w, r)
		if !ok {
			return
		}
		rule.ID = id
	}
	s.Lock()
	index := slices.IndexFunc(s.Rules, func(item Rule) bool { return item.ID == id })
	if index < 0 {
		s.Unlock()
		writeError(w, http.StatusNotFound, "rule not found")
		return
	}
	if r.Method == http.MethodDelete {
		s.Rules = slices.Delete(s.Rules, index, index+1)
	} else {
		s.Rules[index] = rule
	}
	s.Unlock()
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
	} else {
		json.NewEncoder(w).Encode(rule)
	}
}
