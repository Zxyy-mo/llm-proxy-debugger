package main

import (
	"encoding/json"
	"net/http"
	"os"
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

// ModelRoute 模型路由结构，用于 API 解析
type ModelRoute struct {
	ModelName string `json:"model_name"`
	TargetURL string `json:"target_url"`
}

// GlobalStore 全局存储
type GlobalStore struct {
	sync.RWMutex
	Sessions map[string]*Session
	Rules    []Rule
	Routes   map[string]string // 新增: Model -> URL
}

var store = &GlobalStore{
	Sessions: make(map[string]*Session),
	Rules: []Rule{
		{ID: "default-compact", PathMatch: "/messages", BodyMatch: "", InjectSystem: "[System Interjection] Be concise and direct.", Intercept: false},
	},
	Routes: map[string]string{
		"glm-5.1":  "http://127.0.0.1:8081", // 示例: glm
		"qwen-3.5": "http://127.0.0.1:8082", // 示例: qwen
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

// apiRoutesHandler 管理模型路由规则
func apiRoutesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		store.RLock()
		json.NewEncoder(w).Encode(store.Routes)
		store.RUnlock()
	} else if r.Method == http.MethodPost {
		var route ModelRoute
		if err := json.NewDecoder(r.Body).Decode(&route); err == nil && route.ModelName != "" {
			store.Lock()
			store.Routes[route.ModelName] = route.TargetURL
			store.Unlock()
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(route)
		} else {
			http.Error(w, "invalid payload or empty model name", http.StatusBadRequest)
		}
	} else if r.Method == http.MethodDelete {
		modelName := r.URL.Query().Get("model")
		if modelName != "" {
			store.Lock()
			delete(store.Routes, modelName)
			store.Unlock()
			w.WriteHeader(http.StatusOK)
		}
	}
}

// LoadRoutesFromFile 从配置文件加载初始的模型路由字典
func LoadRoutesFromFile(filename string) error {
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	var fileRoutes map[string]string
	if err := json.Unmarshal(data, &fileRoutes); err != nil {
		return err
	}

	store.Lock()
	for k, v := range fileRoutes {
		store.Routes[k] = v // 覆盖或合并到内存
	}
	store.Unlock()
	return nil
}
