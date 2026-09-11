package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
)

type Provider struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	BaseURL      string       `json:"base_url"`
	Protocol     string       `json:"protocol"` // passthrough 原生透传；openai 显式转为 Chat Completions。
	Profile      Profile      `json:"profile,omitempty"`
	Capabilities Capabilities `json:"capabilities,omitzero"`
	KeyEnv       string       `json:"key_env,omitempty"`
	AuthHeader   string       `json:"auth_header,omitempty"`
	AuthScheme   string       `json:"auth_scheme,omitempty"`
	History      bool         `json:"history"`
	WebSocket    bool         `json:"websocket"`
}
type Route struct {
	ID          string   `json:"id"`
	Model       string   `json:"model"`
	ProviderID  string   `json:"provider_id"`
	TargetModel string   `json:"target_model,omitempty"`
	Priority    int      `json:"priority"`
	Disabled    bool     `json:"disabled"`
	Failover    []string `json:"failover,omitempty"`
}
type Config struct {
	Providers []Provider `json:"providers"`
	Routes    []Route    `json:"routes"`
}
type Selection struct {
	RouteID   string
	Model     string
	Endpoints []Provider
}
type Manager struct {
	mu     sync.RWMutex
	config Config
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`)

func New() *Manager { return &Manager{config: Config{Providers: []Provider{}, Routes: []Route{}}} }
func (m *Manager) Snapshot() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	raw, _ := json.Marshal(m.config)
	var result Config
	_ = json.Unmarshal(raw, &result)
	return result
}

// Validate 检查可持久化的配置；预设名称只用于接入提示，不能替代实例能力声明。
func Validate(config Config) error {
	if len(config.Providers) > 64 || len(config.Routes) > 256 {
		return fmt.Errorf("最多允许 64 个 Provider 和 256 条路由")
	}
	providers := map[string]Provider{}
	for _, p := range config.Providers {
		if !identifier.MatchString(p.ID) || providers[p.ID].ID != "" {
			return fmt.Errorf("Provider id 必须唯一且只包含字母、数字、下划线和短横线")
		}
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("%s: base_url 必须是无凭证、查询或片段的 http(s) 地址", p.ID)
		}
		if p.Protocol != "passthrough" && p.Protocol != "openai" {
			return fmt.Errorf("protocol must be passthrough or openai")
		}
		if err := p.validateCapabilities(); err != nil {
			return err
		}
		if p.KeyEnv != "" && !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(p.KeyEnv) {
			return fmt.Errorf("invalid key environment variable")
		}
		if p.AuthHeader != "" && !slices.Contains([]string{"authorization", "x-api-key", "api-key"}, strings.ToLower(p.AuthHeader)) {
			return fmt.Errorf("auth_header must be Authorization, X-API-Key or api-key")
		}
		if strings.ContainsAny(p.AuthScheme, "\r\n\x00") {
			return fmt.Errorf("invalid auth scheme")
		}
		providers[p.ID] = p
	}
	seen := map[string]bool{}
	for _, r := range config.Routes {
		if !identifier.MatchString(r.ID) || seen[r.ID] || r.Model == "" || len(r.Model) > 200 {
			return fmt.Errorf("路由需要唯一 id 和非空 model 模式")
		}
		seen[r.ID] = true
		p, ok := providers[r.ProviderID]
		if !ok {
			return fmt.Errorf("route %s references an unknown provider", r.ID)
		}
		if strings.Count(r.Model, "*") > 1 || (strings.Contains(r.Model, "*") && !strings.HasSuffix(r.Model, "*")) {
			return fmt.Errorf("model 仅支持精确名称或末尾 * 通配")
		}
		if len(r.Failover) > 3 {
			return fmt.Errorf("每条路由最多 3 个备用 Provider")
		}
		fallbacks := map[string]bool{p.ID: true}
		for _, id := range r.Failover {
			fallback, ok := providers[id]
			if !ok || fallbacks[id] || fallback.Protocol != p.Protocol {
				return fmt.Errorf("备用 Provider 必须唯一、存在且使用同一种协议模式")
			}
			fallbacks[id] = true
		}
	}
	return nil
}

// Set 校验并复制配置，使正在执行的请求继续使用原来选择的不可变快照。
func (m *Manager) Set(config Config) error {
	if err := Validate(config); err != nil {
		return err
	}
	raw, _ := json.Marshal(config)
	var copy Config
	_ = json.Unmarshal(raw, &copy)
	if copy.Providers == nil {
		copy.Providers = []Provider{}
	}
	if copy.Routes == nil {
		copy.Routes = []Route{}
	}
	m.mu.Lock()
	m.config = copy
	m.mu.Unlock()
	return nil
}
func (m *Manager) Get(id string) (Provider, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.config.Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// Select 以优先级和原有列表顺序匹配模型；别名与备用上游在请求进入时一起固定。
func (m *Manager) Select(model string) Selection {
	if model == "" {
		return Selection{}
	}
	config := m.Snapshot()
	routes := append([]Route(nil), config.Routes...)
	slices.SortStableFunc(routes, func(a, b Route) int {
		if a.Priority > b.Priority {
			return -1
		}
		if a.Priority < b.Priority {
			return 1
		}
		return 0
	})
	for _, route := range routes {
		if route.Disabled {
			continue
		}
		match := model == route.Model
		if strings.HasSuffix(route.Model, "*") {
			match = strings.HasPrefix(model, strings.TrimSuffix(route.Model, "*"))
		}
		if !match {
			continue
		}
		selection := Selection{RouteID: route.ID, Model: route.TargetModel}
		ids := append([]string{route.ProviderID}, route.Failover...)
		for _, id := range ids {
			for _, p := range config.Providers {
				if p.ID == id {
					selection.Endpoints = append(selection.Endpoints, p)
				}
			}
		}
		return selection
	}
	return Selection{}
}

// Authorize 仅在配置环境变量时替换客户端鉴权；空配置保留原生透传语义。
func (p Provider) Authorize(header http.Header) error {
	if p.KeyEnv == "" {
		return nil
	}
	key := os.Getenv(p.KeyEnv)
	if key == "" {
		return fmt.Errorf("Provider %s 的凭证环境变量 %s 未设置", p.ID, p.KeyEnv)
	}
	return p.AuthorizeKey(header, key)
}

// AuthorizeKey 使用单一凭证来源替换所有受支持的鉴权头，避免混发客户端和上游密钥。
func (p Provider) AuthorizeKey(header http.Header, key string) error {
	if strings.ContainsAny(key, "\r\n\x00") {
		return fmt.Errorf("invalid provider credential")
	}
	name, scheme := p.AuthHeader, p.AuthScheme
	if name == "" {
		name = "Authorization"
	}
	if scheme == "" && strings.EqualFold(name, "Authorization") {
		scheme = "Bearer"
	}
	header.Del("Authorization")
	header.Del("X-API-Key")
	header.Del("Api-Key")
	if scheme != "" {
		key = scheme + " " + key
	}
	header.Set(name, key)
	return nil
}

// JoinPath 兼容 origin、/v1 和自定义挂载路径；仅去重字面量 /v1 前缀。
func JoinPath(base, path string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		if path == "" {
			return "/"
		}
		return path
	}
	if strings.HasSuffix(base, "/v1") && (path == "/v1" || strings.HasPrefix(path, "/v1/")) {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + "/" + strings.TrimPrefix(path, "/")
}

// JoinURLPath 先拼接转义路径，保证编码斜线仍属于原 ID 或挂载段，而不是目录分隔符。
func JoinURLPath(base, request *url.URL) (path, rawPath string) {
	rawPath = JoinPath(base.EscapedPath(), request.EscapedPath())
	path, _ = url.PathUnescape(rawPath)
	return path, rawPath
}
