package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/tidwall/gjson"
)

func TestEscapedPathsSurviveRoutingExportAndReplay(t *testing.T) {
	for _, tc := range []struct{ name, base, path, method, want string }{
		{"identifier", "/v1", "/v1/responses/resp%2Fone?x=%2f&x=%2B", "GET", "/v1/responses/resp%2Fone?x=%2f&x=%2B"},
		{"mount-and-replay", "/mount%2Ftenant/v1", "/v1/chat/completions?x=%2f", "POST", "/mount%2Ftenant/v1/chat/completions?x=%2f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.RequestURI != tc.want {
					t.Errorf("escaped request target changed: got %q want %q", r.RequestURI, tc.want)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"escaped","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			}), 1024)
			proxy.target, _ = url.Parse(proxy.target.String() + tc.base)
			trace, reply := send(t, proxy, tc.method, tc.path, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, nil)
			if reply.Code != 200 {
				t.Fatalf("forwarding failed: %d", reply.Code)
			}
			capture, _ := storage.RequestSnapshot(trace)
			if !strings.HasSuffix(capture.Outgoing.URL, tc.want) {
				t.Fatal("capture lost encoded path")
			}
			code, body := api(t, proxy.RequestsHandler, "GET", "/api/requests/"+trace+"/curl", nil)
			if code != 200 || !strings.HasSuffix(gjson.GetBytes(body, "destination.url").String(), tc.want) {
				t.Fatalf("export lost encoded path: %s", body)
			}
			if tc.method == "POST" {
				record := createReplay(t, proxy, map[string]any{"trace_id": trace, "idempotency_key": "escaped-path"}, 202)
				record = finishedReplay(t, proxy, record.ID)
				if record.State != "done" || hits.Load() != 2 {
					t.Fatalf("encoded mount replay failed: %+v", record)
				}
			}
		})
	}
}

func TestProviderFallbackAliasCredentialsAndFrozenReplay(t *testing.T) {
	t.Setenv("GATEWAY_TEST_PRIMARY", "primary-secret")
	t.Setenv("GATEWAY_TEST_BACKUP", "backup-secret")
	var firstHits, secondHits atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		if r.Header.Get("Authorization") != "Bearer primary-secret" {
			t.Error("primary auth")
		}
		w.WriteHeader(503)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHits.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/base/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer backup-secret" || gjson.GetBytes(raw, "model").String() != "actual" || !strings.Contains(string(raw), "9007199254740993") {
			t.Errorf("bad backup request: %s %s", r.URL.Path, raw)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"ok","choices":[{"message":{"content":"done"}}]}`)
	}))
	defer second.Close()
	proxy, storage := testProxy(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("default target received routed request") }), 1024)
	config := provider.Config{Providers: []provider.Provider{{ID: "primary", BaseURL: first.URL, Protocol: "passthrough", KeyEnv: "GATEWAY_TEST_PRIMARY"}, {ID: "backup", BaseURL: second.URL + "/base/v1", Protocol: "passthrough", KeyEnv: "GATEWAY_TEST_BACKUP"}}, Routes: []provider.Route{{ID: "model-route", Model: "alias", TargetModel: "actual", ProviderID: "primary", Failover: []string{"backup"}}}}
	if err := proxy.providers.Set(config); err != nil {
		t.Fatal(err)
	}
	trace, response := send(t, proxy, "POST", "/v1/chat/completions", `{"model":"alias","messages":[{"role":"user","content":"go"}],"seed":9007199254740993}`, map[string]string{"Authorization": "Bearer client-secret"})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	log := logOf(t, storage, trace)
	if log.Route == nil || log.Route.ProviderID != "backup" || len(log.Route.Attempts) != 2 || log.Route.Attempts[0].StatusCode != 503 {
		t.Fatalf("attempts missing: %+v", log.Route)
	}
	capture, _ := storage.RequestSnapshot(trace)
	raw, _ := json.Marshal(capture)
	if strings.Contains(string(raw), "primary-secret") || strings.Contains(string(raw), "backup-secret") || strings.Contains(string(raw), "client-secret") {
		t.Fatal("credential persisted")
	}
	record := createReplay(t, proxy, map[string]any{"trace_id": trace, "idempotency_key": "frozen-provider"}, 202)
	record = finishedReplay(t, proxy, record.ID)
	if record.State != "done" || firstHits.Load() != 1 || secondHits.Load() != 2 {
		t.Fatalf("replay rerouted or demanded transient provider credentials: %+v", record)
	}
	config.Providers[1].BaseURL = first.URL
	_ = proxy.providers.Set(config)
	if status, _ := api(t, proxy.ReplaysHandler, "POST", "/api/replays", map[string]any{"trace_id": trace, "idempotency_key": "changed-provider"}); status != 409 {
		t.Fatalf("changed destination accepted: %d", status)
	}
}

func TestConversionTTFBUsesUpstreamHeadersBeforeJSONBuffering(t *testing.T) {
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(120 * time.Millisecond)
		io.WriteString(w, `{"id":"up","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}), 1024)
	_ = proxy.providers.Set(provider.Config{Providers: []provider.Provider{{ID: "chat", BaseURL: proxy.target.String(), Protocol: "openai"}}, Routes: []provider.Route{{ID: "r", Model: "m", ProviderID: "chat"}}})
	trace, _ := send(t, proxy, "POST", "/v1/responses", `{"model":"m","input":"hello","store":false}`, nil)
	log := logOf(t, storage, trace)
	if log.TTFB == nil || log.TTFC == nil || *log.TTFC-*log.TTFB < 100 {
		t.Fatalf("header timing includes body buffering: %+v", log)
	}
}

func TestNoFallbackAfterPartialStream(t *testing.T) {
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { backupHits.Add(1) }))
	defer backup.Close()
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"id\":\"partial\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
	}), 1024)
	_ = proxy.providers.Set(provider.Config{Providers: []provider.Provider{{ID: "first", BaseURL: proxy.target.String(), Protocol: "passthrough"}, {ID: "backup", BaseURL: backup.URL, Protocol: "passthrough"}}, Routes: []provider.Route{{ID: "r", Model: "m", ProviderID: "first", Failover: []string{"backup"}}}})
	trace, _ := send(t, proxy, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"x"}],"stream":true}`, nil)
	if backupHits.Load() != 0 || logOf(t, storage, trace).Status != "error" {
		t.Fatal("partial response retried or not reported")
	}
}

func TestProxyConversionHasSeparateUpstreamCapture(t *testing.T) {
	const upstream = `{"id":"upstream-id","model":"m","choices":[{"message":{"role":"assistant","content":"answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" || gjson.GetBytes(body, "messages.0.content").String() != "question" {
			t.Errorf("wrong converted request: %s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, upstream)
	}), 4096)
	_ = proxy.providers.Set(provider.Config{Providers: []provider.Provider{{ID: "chat", BaseURL: proxy.target.String() + "/v1", Protocol: "openai"}}, Routes: []provider.Route{{ID: "r", Model: "m", ProviderID: "chat"}}})
	trace, reply := send(t, proxy, "POST", "/v1/responses", `{"model":"m","input":"question","store":false}`, nil)
	log := logOf(t, storage, trace)
	if reply.Code != 200 || log.Status != "done" || log.Route.Conversion != "responses → openai" || gjson.Get(reply.Body.String(), "object").String() != "response" {
		t.Fatalf("conversion failed: %+v %s", log, reply.Body.String())
	}
	client := responseDetail(t, storage, "/api/responses/"+trace)
	raw := responseDetail(t, storage, "/api/responses/"+trace+"?variant=upstream")
	if client.Representation != "converted-from-openai" || client.Body != reply.Body.String() || raw.Body != upstream || raw.Representation != "upstream-original" {
		t.Fatalf("capture variants confused: %+v %+v", client, raw)
	}
	_, reply = send(t, proxy, "POST", "/v1/responses", `{"model":"m","input":"question","previous_response_id":"remote"}`, nil)
	if reply.Code != 422 {
		t.Fatal("remote history silently dropped")
	}
}
