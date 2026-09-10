package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/adapter"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/export"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func (s *Server) ProvidersHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(map[string]any{"config": s.providers.Snapshot(), "default_target": s.target.String(), "capabilities": map[string]any{"native": []string{"openai", "anthropic", "responses"}, "conversion": "Anthropic/Responses text and function tools to Chat Completions; independent turns only", "websocket": "Responses response.create with stream_id lanes", "history": "GET saved Responses by ID; provider must support it"}})
	case http.MethodPut:
		var config provider.Config
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		d.DisallowUnknownFields()
		if d.Decode(&config) != nil || d.Decode(new(any)) != io.EOF {
			replayJSON(w, 400, map[string]string{"error": "invalid provider configuration"})
			return
		}
		if err := s.providers.Set(config); err != nil {
			replayJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		s.store.SetSetting("providers", config)
		if err := s.store.Flush(); err != nil {
			replayJSON(w, 500, map[string]string{"error": "配置已应用，但持久化失败"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"config": s.providers.Snapshot()})
	default:
		w.Header().Set("Allow", "GET, PUT")
		replayJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

type routeTransport struct {
	base       http.RoundTripper
	selection  provider.Selection
	path       string
	rawPath    string
	body       []byte
	info       store.RouteInfo
	conversion *adapter.Plan
	upstream   func(*http.Response) *http.Response
	received   func(time.Time)
	started    func()
	capture    func(*http.Request, provider.Provider)
	update     func(store.RouteInfo)
}

func (t routeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	info := t.info
	for index, endpoint := range t.selection.Endpoints {
		req := request.Clone(request.Context())
		target, _ := url.Parse(endpoint.BaseURL)
		path, rawPath := provider.JoinURLPath(target, &url.URL{Path: t.path, RawPath: t.rawPath})
		req.URL.Scheme, req.URL.Host, req.URL.Path, req.URL.RawPath = target.Scheme, target.Host, path, rawPath
		req.Host = target.Host
		if index > 0 {
			req.Body = io.NopCloser(bytes.NewReader(t.body))
			req.ContentLength = int64(len(t.body))
		}
		if t.conversion != nil {
			if req.Header.Get("Authorization") == "" && req.Header.Get("X-API-Key") != "" {
				req.Header.Set("Authorization", "Bearer "+req.Header.Get("X-API-Key"))
			}
			req.Header.Del("X-API-Key")
			req.Header.Del("Anthropic-Version")
			req.Header.Del("Anthropic-Beta")
		}
		if endpoint.KeyEnv != "" {
			forwarding, _ := export.Classify(req)
			req.URL.RawQuery = forwarding.Query
		}
		if err := endpoint.Authorize(req.Header); err != nil {
			return nil, err
		}
		t.capture(req, endpoint)
		if index == 0 {
			t.started()
		}
		start := time.Now()
		response, err := t.base.RoundTrip(req)
		attempt := store.RouteAttempt{ProviderID: endpoint.ID, URL: target.String(), Duration: float64(time.Since(start).Microseconds()) / 1000}
		if response != nil {
			attempt.StatusCode = response.StatusCode
		}
		if err != nil {
			attempt.Error = err.Error()
		}
		info.ProviderID = endpoint.ID
		info.Attempts = append(info.Attempts, attempt)
		t.update(info)
		retry := err != nil || (response != nil && (response.StatusCode == 502 || response.StatusCode == 503 || response.StatusCode == 504))
		if !retry || index == len(t.selection.Endpoints)-1 || request.Context().Err() != nil {
			if response != nil && t.received != nil {
				t.received(time.Now())
			}
			if err == nil && response != nil && t.conversion != nil {
				if t.upstream != nil {
					response = t.upstream(response)
				}
				return t.conversion.Response(response)
			}
			return response, err
		}
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
	}
	return nil, fmt.Errorf("no upstream configured")
}

func (s *Server) selectProvider(model string) provider.Selection {
	selection := s.providers.Select(model)
	if len(selection.Endpoints) == 0 {
		selection.Endpoints = []provider.Provider{{BaseURL: s.target.String(), Protocol: "passthrough", WebSocket: true}}
	}
	return selection
}

func (s *Server) ProviderHistoryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		replayJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var input struct {
		ProviderID string `json:"provider_id"`
		ResponseID string `json:"response_id"`
		APIKey     string `json:"api_key,omitempty"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`).MatchString(input.ResponseID) {
		replayJSON(w, 400, map[string]string{"error": "invalid provider/response ID"})
		return
	}
	p, ok := s.providers.Get(input.ProviderID)
	if !ok || !p.History {
		replayJSON(w, 422, map[string]string{"error": "此 Provider 未启用已保存 Responses 查询能力"})
		return
	}
	target, _ := url.Parse(p.BaseURL)
	target.Path, target.RawPath = provider.JoinURLPath(target, &url.URL{Path: "/v1/responses/" + input.ResponseID})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		replayJSON(w, 400, map[string]string{"error": "invalid history request"})
		return
	}
	if input.APIKey != "" {
		if err := validCredentialValue(input.APIKey); err != nil {
			replayJSON(w, 422, map[string]string{"error": err.Error()})
			return
		}
		key := strings.TrimSpace(input.APIKey)
		if len(key) > 7 && strings.EqualFold(key[:7], "Bearer ") {
			key = key[7:]
		}
		err = p.AuthorizeKey(req.Header, key)
	} else {
		err = p.Authorize(req.Header)
	}
	if err != nil {
		replayJSON(w, 422, map[string]string{"error": err.Error()})
		return
	}
	client := &http.Client{Transport: s.transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		replayJSON(w, 502, map[string]string{"error": "Provider 历史查询失败"})
		return
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20+1))
	if err != nil || len(body) > 16<<20 {
		replayJSON(w, 502, map[string]string{"error": "历史响应不可读取或超过 16 MiB"})
		return
	}
	// History reads do not invent another model execution or a producer trace.
	policy := s.store.Privacy.Policy()
	if policy.Record {
		scope := correlation.ExtractRequest(req.Header, "/v1/responses", nil, p.BaseURL).Scope
		body = s.store.Privacy.JSON(policy, scope, body)
	}
	json.NewEncoder(w).Encode(map[string]any{"provider_id": p.ID, "response_id": input.ResponseID, "status_code": response.StatusCode, "body": string(body), "source": "provider_history", "redacted": policy.Record})
}
