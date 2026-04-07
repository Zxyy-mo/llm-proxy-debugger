package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session 会话记录
type Session struct {
	ID        string       `json:"id"`
	CreatedAt time.Time    `json:"created_at"`
	Logs      []RequestLog `json:"logs"`
}

// Rule 动态干预规则
type Rule struct {
	ID           string `json:"id"`
	PathMatch    string `json:"path_match"`
	BodyMatch    string `json:"body_match"`
	InjectSystem string `json:"inject_system"`
	Intercept    bool   `json:"intercept"`
}

// GlobalStore 全局存储
type GlobalStore struct {
	sync.RWMutex
	Sessions map[string]*Session
	Rules    []Rule
}

var store = &GlobalStore{
	Sessions: make(map[string]*Session),
	Rules: []Rule{
		{ID: "default-compact", PathMatch: "/messages", BodyMatch: "", InjectSystem: "[System Interjection] Be concise and direct.", Intercept: false},
	},
}

// apiSessionsHandler 返回所有会话
func apiSessionsHandler(w http.ResponseWriter, r *http.Request) {
	store.RLock()
	defer store.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(store.Sessions)
}

// apiRulesHandler 管理规则
func apiRulesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		store.RLock()
		json.NewEncoder(w).Encode(store.Rules)
		store.RUnlock()
	} else if r.Method == http.MethodPost {
		var rule Rule
		if err := json.NewDecoder(r.Body).Decode(&rule); err == nil {
			store.Lock()
			rule.ID = uuid.New().String()
			store.Rules = append(store.Rules, rule)
			store.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(rule)
		}
	}
}
