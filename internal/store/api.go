package store

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

// SessionsHandler 返回所有会话
func (s *Store) SessionsHandler(w http.ResponseWriter, r *http.Request) {
	s.RLock()
	defer s.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.Sessions)
}

// RulesHandler 管理规则（GET 列出，POST 新增）
func (s *Store) RulesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodGet:
		s.RLock()
		json.NewEncoder(w).Encode(s.Rules)
		s.RUnlock()
	case http.MethodPost:
		var rule Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err == nil {
			s.Lock()
			rule.ID = uuid.New().String()
			s.Rules = append(s.Rules, rule)
			s.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(rule)
		}
	}
}
