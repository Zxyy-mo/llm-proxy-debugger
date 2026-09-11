package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
)

func configureModelsProvider(t *testing.T, proxy *Server, p provider.Provider) {
	t.Helper()
	if err := proxy.providers.Set(provider.Config{Providers: []provider.Provider{p}}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderModelsUsesSavedURLAndIsolatedCredentials(t *testing.T) {
	for _, tc := range []struct{ name, base, path, header, scheme, transient string }{
		{"origin", "", "/v1/models", "Authorization", "Bearer", ""},
		{"v1", "/v1", "/v1/models", "Authorization", "Bearer", "Bearer transient-secret"},
		{"mount", "/gateway", "/gateway/v1/models", "X-API-Key", "", "transient-secret"},
		{"escaped-mount", "/tenant%2Fone/v1/", "/tenant%2Fone/v1/models", "api-key", "Token", "transient-secret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROVIDER_MODELS_TEST_KEY", "environment-secret")
			var hits atomic.Int32
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				wantKey := "environment-secret"
				if tc.transient != "" {
					wantKey = "transient-secret"
				}
				if tc.scheme != "" {
					wantKey = tc.scheme + " " + wantKey
				}
				if r.Method != "GET" || r.RequestURI != tc.path || r.Header.Get(tc.header) != wantKey || r.Header.Get("Accept") != "application/json" {
					t.Errorf("模型发现未复用路径/鉴权: %s %s", r.Method, r.RequestURI)
				}
				for _, header := range []string{"Authorization", "X-API-Key", "api-key"} {
					if !strings.EqualFold(header, tc.header) && r.Header.Get(header) != "" {
						t.Error("混发了凭证")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"object":"list","data":[{"id":"z-model","owned_by":"local","created":0,"root":"ignored"},{"id":"a-model"},{"id":"z-model"}]}`)
			}), 1024)
			p := provider.Provider{ID: "relay", Name: "实例", BaseURL: proxy.target.String() + tc.base, Protocol: "passthrough", Profile: provider.ProfileVLLM, KeyEnv: "PROVIDER_MODELS_TEST_KEY", AuthHeader: tc.header, AuthScheme: tc.scheme}
			configureModelsProvider(t, proxy, p)
			body, _ := json.Marshal(map[string]string{"provider_id": "relay", "api_key": tc.transient})
			r := httptest.NewRequest("POST", "/api/provider-models", strings.NewReader(string(body)))
			r.Header.Set("Authorization", "Bearer operator-secret")
			rr := httptest.NewRecorder()
			proxy.ProviderModelsHandler(rr, r)
			var result providerModelsResult
			if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil || rr.Code != 200 || result.Error != "" || result.StatusCode != 200 || len(result.Models) != 2 || result.Models[0].ID != "a-model" || result.Source != "provider_models" || result.Provider != p || result.CheckedAt.IsZero() {
				t.Fatalf("模型列表结果无效: %d %s", rr.Code, rr.Body)
			}
			if hits.Load() != 1 || len(storage.SessionsSnapshot()) != 0 || proxy.providers.Snapshot().Providers[0] != p {
				t.Fatal("模型查询创建了执行记录或改动配置")
			}
			for _, secret := range []string{"environment-secret", "transient-secret", "operator-secret"} {
				if strings.Contains(rr.Body.String(), secret) {
					t.Fatal("响应含有查询密钥")
				}
			}
		})
	}
}

func TestProviderModelsInputCapabilityAndNoImplicitCredential(t *testing.T) {
	var hits atomic.Int32
	proxy, _ := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("管理请求的凭证被借用")
		}
		io.WriteString(w, `{"data":[]}`)
	}), 1024)
	p := provider.Provider{ID: "relay", BaseURL: proxy.target.String(), Protocol: "passthrough"}
	configureModelsProvider(t, proxy, p)
	for _, tc := range []struct {
		body any
		code int
	}{
		{map[string]string{}, 400},
		{map[string]string{"provider_id": "missing"}, 404},
		{map[string]string{"provider_id": "relay", "base_url": "https://other.invalid"}, 400},
		{map[string]string{"provider_id": "relay", "api_key": "key\nheader"}, 422},
		{map[string]string{"provider_id": "relay", "api_key": "   "}, 422},
		{map[string]string{"provider_id": "relay", "api_key": strings.Repeat("k", 17000)}, 400},
	} {
		if status, _ := api(t, proxy.ProviderModelsHandler, "POST", "/api/provider-models", tc.body); status != tc.code {
			t.Fatalf("input=%v: got %d want %d", tc.body, status, tc.code)
		}
	}
	if code, _ := api(t, proxy.ProviderModelsHandler, "GET", "/api/provider-models", nil); code != 405 {
		t.Fatal("GET 触发了查询")
	}
	p.Capabilities.Models = provider.CapabilityUnsupported
	configureModelsProvider(t, proxy, p)
	if code, _ := api(t, proxy.ProviderModelsHandler, "POST", "/api/provider-models", map[string]string{"provider_id": "relay"}); code != 422 || hits.Load() != 0 {
		t.Fatal("明确不支持仍访问了上游")
	}
	p.Capabilities.Models = provider.CapabilityUnknown
	p.KeyEnv = "UNSET_PROVIDER_MODELS_KEY"
	t.Setenv(p.KeyEnv, "")
	configureModelsProvider(t, proxy, p)
	if code, _ := api(t, proxy.ProviderModelsHandler, "POST", "/api/provider-models", map[string]string{"provider_id": "relay"}); code != 422 || hits.Load() != 0 {
		t.Fatal("环境变量缺失仍访问了上游")
	}
	p.KeyEnv = ""
	configureModelsProvider(t, proxy, p)
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/provider-models", strings.NewReader(`{"provider_id":"relay"}`))
	r.Header.Set("Authorization", "Bearer operator")
	proxy.ProviderModelsHandler(rr, r)
	if rr.Code != 200 || hits.Load() != 1 || !strings.Contains(rr.Body.String(), `"models":[]`) {
		t.Fatalf("未允许免鉴权空列表: %s", rr.Body)
	}
}

func TestProviderModelsDoesNotFollowRedirectsOrExposeErrorBodies(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer other.Close()
	for _, status := range []int{301, 307, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			proxy, _ := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", other.URL+"?key=secret-query")
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"message":"echoed-secret-key"}}`)
			}), 1024)
			configureModelsProvider(t, proxy, provider.Provider{ID: "relay", BaseURL: proxy.target.String(), Protocol: "passthrough"})
			code, body := api(t, proxy.ProviderModelsHandler, "POST", "/api/provider-models", map[string]string{"provider_id": "relay", "api_key": "echoed-secret-key"})
			var result providerModelsResult
			_ = json.Unmarshal(body, &result)
			if code != 200 || result.StatusCode != status || result.Error == "" || len(result.Models) != 0 || strings.Contains(string(body), "secret") || redirected.Load() != 0 {
				t.Fatalf("错误或重定向处理不正确: %d %s", code, body)
			}
		})
	}
}

func TestProviderModelsRejectsPartialOversizedAndInvalidLists(t *testing.T) {
	for _, tc := range []struct{ name, body, errorCode string }{
		{"missing-data", `{}`, "invalid_response"},
		{"null-data", `{"data":null}`, "invalid_response"},
		{"empty-id", `{"data":[{"id":""}]}`, "invalid_response"},
		{"null-item", `{"data":[null]}`, "invalid_response"},
		{"wrong-id-type", `{"data":[{"id":12}]}`, "invalid_response"},
		{"bad-created", `{"data":[{"id":"m","created":-1}]}`, "invalid_response"},
		{"bad-object", `{"object":"error","data":[]}`, "invalid_response"},
		{"successful-error", `{"error":{"message":"secret-error"},"data":[]}`, "invalid_response"},
		{"partial", `{"data":[{"id":"model"}`, "invalid_response"},
		{"trailing-json", `{"data":[]} {}`, "invalid_response"},
		{"too-large", `{"data":[],"extra":"` + strings.Repeat("x", providerModelsMaxBody) + `"}`, "response_too_large"},
		{"too-many", `{"data":[` + strings.Repeat(`{"id":"m"},`, providerModelsMaxRows) + `{"id":"last"}]}`, "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, _ := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }), 1024)
			configureModelsProvider(t, proxy, provider.Provider{ID: "relay", BaseURL: proxy.target.String(), Protocol: "passthrough"})
			code, body := api(t, proxy.ProviderModelsHandler, "POST", "/api/provider-models", map[string]string{"provider_id": "relay"})
			var result providerModelsResult
			_ = json.Unmarshal(body, &result)
			if code != 200 || result.ErrorCode != tc.errorCode || len(result.Models) != 0 || strings.Contains(string(body), "secret-error") {
				t.Fatalf("部分/无效响应被当作模型列表: %d %s", code, body)
			}
		})
	}
}

func TestProviderModelsDeadlineAndConfigurationChanges(t *testing.T) {
	for _, flushHeaders := range []bool{false, true} {
		t.Run(map[bool]string{false: "deadline-headers", true: "deadline-body"}[flushHeaders], func(t *testing.T) {
			proxy, _ := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if flushHeaders {
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}), 1024)
			configureModelsProvider(t, proxy, provider.Provider{ID: "relay", BaseURL: proxy.target.String(), Protocol: "passthrough"})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
			defer cancel()
			r := httptest.NewRequest("POST", "/api/provider-models", strings.NewReader(`{"provider_id":"relay"}`)).WithContext(ctx)
			rr := httptest.NewRecorder()
			proxy.ProviderModelsHandler(rr, r)
			if rr.Code != 504 {
				t.Fatalf("未遵守查询截止时间: %d %s", rr.Code, rr.Body)
			}
		})
	}
	for _, remove := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "removed"}[remove], func(t *testing.T) {
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			proxy, _ := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				io.WriteString(w, `{"data":[{"id":"old-model"}]}`)
			}), 1024)
			p := provider.Provider{ID: "relay", BaseURL: proxy.target.String(), Protocol: "passthrough"}
			configureModelsProvider(t, proxy, p)
			rr := httptest.NewRecorder()
			go func() {
				defer close(finished)
				proxy.ProviderModelsHandler(rr, httptest.NewRequest("POST", "/api/provider-models", strings.NewReader(`{"provider_id":"relay"}`)))
			}()
			<-entered
			if remove {
				_ = proxy.providers.Set(provider.Config{})
			} else {
				p.KeyEnv = "CHANGED_KEY"
				configureModelsProvider(t, proxy, p)
			}
			close(release)
			<-finished
			if rr.Code != 409 || strings.Contains(rr.Body.String(), "old-model") {
				t.Fatalf("返回了旧配置查询结果: %d %s", rr.Code, rr.Body)
			}
		})
	}
}

func TestProviderModelsInterruptedBody(t *testing.T) {
	proxy, _ := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		io.WriteString(w, `{"data":[{"id":"unfinished"}]}`)
	}), 1024)
	configureModelsProvider(t, proxy, provider.Provider{ID: "relay", BaseURL: proxy.target.String(), Protocol: "passthrough"})
	code, body := api(t, proxy.ProviderModelsHandler, "POST", "/api/provider-models", map[string]string{"provider_id": "relay"})
	var result providerModelsResult
	_ = json.Unmarshal(body, &result)
	if code != 200 || result.ErrorCode != "unreadable_response" || len(result.Models) != 0 || strings.Contains(string(body), "unfinished") {
		t.Fatalf("未完整传输的响应被用作列表: %d %s", code, body)
	}
}

func TestProviderPresetsAndConfigurationValidation(t *testing.T) {
	proxy, _ := testProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("模板不应访问上游") }), 1024)
	code, body := api(t, proxy.ProviderPresetsHandler, "GET", "/api/provider-presets", nil)
	var result struct {
		Presets          []provider.Preset `json:"presets"`
		CapabilitySource string            `json:"capability_source"`
	}
	if json.Unmarshal(body, &result) != nil || code != 200 || len(result.Presets) != 5 || result.CapabilitySource != "operator_declared" {
		t.Fatalf("预设契约无效: %s", body)
	}
	for _, bad := range []string{
		`"profile":"invented"`,
		`"capabilities":{"models":"maybe"}`,
		`"capabilities":{"unknown_interface":"supported"}`,
		`"capabilities":{"models":true}`,
	} {
		rr := httptest.NewRecorder()
		proxy.ProvidersHandler(rr, httptest.NewRequest("PUT", "/api/providers", strings.NewReader(`{"providers":[{"id":"relay","base_url":"https://example.invalid","protocol":"passthrough",`+bad+`}],"routes":[]}`)))
		if rr.Code != 400 && rr.Code != 422 {
			t.Fatalf("新增字段未严格校验: %s %d", bad, rr.Code)
		}
	}
}
