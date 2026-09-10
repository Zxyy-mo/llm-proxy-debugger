package provider

import (
	"net/http"
	"strings"
	"testing"
)

func TestPriorityAliasAndCredentialIsolation(t *testing.T) {
	m := New()
	cfg := Config{Providers: []Provider{{ID: "primary", BaseURL: "https://example.invalid/v1", Protocol: "openai", KeyEnv: "TEST_PROVIDER_KEY"}}, Routes: []Route{{ID: "all", Model: "*", ProviderID: "primary"}, {ID: "exact", Model: "visible", TargetModel: "actual", ProviderID: "primary", Priority: 10}}}
	if err := m.Set(cfg); err != nil {
		t.Fatal(err)
	}
	selected := m.Select("visible")
	if selected.RouteID != "exact" || selected.Model != "actual" {
		t.Fatal("route priority ignored")
	}
	t.Setenv("TEST_PROVIDER_KEY", "provider-secret")
	h := http.Header{"Authorization": []string{"Bearer original"}, "X-Api-Key": []string{"other-original"}}
	if err := selected.Endpoints[0].Authorize(h); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer provider-secret" || h.Get("X-API-Key") != "" {
		t.Fatal("provider credentials mixed with client credentials")
	}
	if strings.Contains(m.Snapshot().Providers[0].KeyEnv, "provider-secret") {
		t.Fatal("raw key stored in config")
	}
}
func TestBaseURLJoining(t *testing.T) {
	for _, tc := range []struct{ base, path, want string }{{"/v1", "/v1/chat/completions", "/v1/chat/completions"}, {"/mount", "/v1/responses", "/mount/v1/responses"}, {"/v1", "/responses", "/v1/responses"}} {
		if value := JoinPath(tc.base, tc.path); value != tc.want {
			t.Fatalf("joined %s", value)
		}
	}
}
