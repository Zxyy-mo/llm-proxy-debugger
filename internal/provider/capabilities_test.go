package provider

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestOldProviderConfigurationRemainsCompatible(t *testing.T) {
	const old = `{"providers":[{"id":"old","name":"原有","base_url":"https://example.invalid/v1","protocol":"passthrough","key_env":"OLD_KEY","history":true,"websocket":true}],"routes":[{"id":"route","model":"visible","provider_id":"old","target_model":"actual","priority":2,"disabled":false}]}`
	var config Config
	if err := json.Unmarshal([]byte(old), &config); err != nil {
		t.Fatal(err)
	}
	m := New()
	if err := m.Set(config); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Snapshot(), config) {
		t.Fatal("旧配置被重新解释")
	}
	p, _ := m.Get("old")
	for _, path := range []string{"/v1/chat/completions", "/responses", "/v1/messages", "/models"} {
		if err := p.CheckEndpoint(path); err != nil {
			t.Fatalf("旧配置不能透传 %s: %v", path, err)
		}
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), `"profile"`) || strings.Contains(string(raw), `"capabilities"`) {
		t.Fatalf("空增量字段未保持可选: %s", raw)
	}
}

func TestCapabilitiesValidateAndBlockOnlyExplicitUnsupported(t *testing.T) {
	p := Provider{ID: "relay", BaseURL: "https://example.invalid/v1", Protocol: "openai", Profile: ProfileVLLM,
		Capabilities: Capabilities{ChatCompletions: CapabilitySupported, Responses: CapabilityUnsupported, Messages: CapabilityUnsupported, Models: CapabilityUnknown}}
	if err := Validate(Config{Providers: []Provider{p}}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/responses", "/responses/", "/v1/responses/resp_1", "/messages", "/v1/messages/count_tokens"} {
		err := p.CheckEndpoint(path)
		var unsupported *UnsupportedEndpointError
		if !errors.As(err, &unsupported) || unsupported.ProviderID != "relay" {
			t.Fatalf("未拦截显式不支持的接口 %s: %v", path, err)
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/chat/completions/", "/models", "/v1/models/model", "/v1/embeddings", "/v1/responses-other", "/v1/messages-other"} {
		if err := p.CheckEndpoint(path); err != nil {
			t.Fatalf("错误拦截兼容路径 %s: %v", path, err)
		}
	}
	for _, change := range []func(*Provider){
		func(p *Provider) { p.Profile = "guess" },
		func(p *Provider) { p.Capabilities.ChatCompletions = "yes" },
		func(p *Provider) { p.Capabilities.Responses = "automatic" },
		func(p *Provider) { p.Capabilities.Messages = "SUPPORTED" },
		func(p *Provider) { p.Capabilities.Models = "false" },
	} {
		invalid := p
		change(&invalid)
		if err := Validate(Config{Providers: []Provider{invalid}}); err == nil {
			t.Fatal("非法配置未被拒绝")
		}
	}
	for _, state := range []CapabilityState{"", CapabilityUnknown, CapabilitySupported, CapabilityUnsupported} {
		p.Capabilities.Models = state
		if err := Validate(Config{Providers: []Provider{p}}); err != nil {
			t.Fatal(err)
		}
		if blocked := p.CheckEndpoint("/v1/models") != nil; blocked != (state == CapabilityUnsupported) {
			t.Fatalf("错误的三态语义: %q", state)
		}
	}
}

func TestPresetsDoNotPromiseInstanceCapabilities(t *testing.T) {
	items := Presets()
	if len(items) != 5 {
		t.Fatal("预设缺失")
	}
	seen := map[Profile]bool{}
	for _, item := range items {
		seen[item.ID] = true
		p := item.Defaults
		if p.Profile != item.ID || p.BaseURL != "" || p.History || p.WebSocket || p.Protocol != "passthrough" {
			t.Fatalf("预设自动声称高级能力: %+v", item)
		}
		if p.Capabilities != (Capabilities{CapabilityUnknown, CapabilityUnknown, CapabilityUnknown, CapabilityUnknown}) {
			t.Fatal("预设不能替操作者声明接口支持")
		}
		p.ID, p.BaseURL = "configured", "https://instance.invalid/v1"
		if err := Validate(Config{Providers: []Provider{p}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range []Profile{ProfileCustom, ProfileCPA, ProfileNewAPI, ProfileSub2API, ProfileVLLM} {
		if !seen[expected] {
			t.Fatalf("缺少 %s", expected)
		}
	}
	items[0].Notes[0] = "changed"
	if Presets()[0].Notes[0] == "changed" {
		t.Fatal("预设共享了可变状态")
	}
}
