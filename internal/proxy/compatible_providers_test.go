package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/tidwall/gjson"
)

// compatibleChatResponse 模拟可控的兼容接口，不代表任一真实服务或版本已完成验收。
func compatibleChatResponse(stream, result bool) string {
	if !stream {
		if result {
			return `{"id":"reply-result","object":"chat.completion","model":"actual-model","choices":[{"index":0,"message":{"role":"assistant","content":"上海晴天"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":4,"total_tokens":16}}`
		}
		return `{"id":"reply-tool","object":"chat.completion","model":"actual-model","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"weather-call","type":"function","function":{"name":"weather","arguments":"{\"city\":\"上海\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}}`
	}
	choice := func(id string, delta map[string]any, finish string) map[string]any {
		item := map[string]any{"index": 0, "delta": delta}
		if finish != "" {
			item["finish_reason"] = finish
		}
		return map[string]any{"id": id, "choices": []any{item}}
	}
	var chunks []map[string]any
	if result {
		chunks = []map[string]any{
			choice("reply-result", map[string]any{"role": "assistant", "content": "上海"}, ""),
			choice("reply-result", map[string]any{"content": "晴天"}, "stop"),
			{"id": "reply-result", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 12, "completion_tokens": 4, "total_tokens": 16}},
		}
	} else {
		firstCall := map[string]any{"index": 0, "id": "weather-call", "type": "function", "function": map[string]string{"name": "weather", "arguments": `{"city":`}}
		lastCall := map[string]any{"index": 0, "function": map[string]string{"arguments": `"上海"}`}}
		chunks = []map[string]any{
			choice("reply-tool", map[string]any{"role": "assistant", "tool_calls": []any{firstCall}}, ""),
			choice("reply-tool", map[string]any{"tool_calls": []any{lastCall}}, "tool_calls"),
			{"id": "reply-tool", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 3, "total_tokens": 11}},
		}
	}
	var response strings.Builder
	for _, chunk := range chunks {
		raw, _ := json.Marshal(chunk)
		fmt.Fprintf(&response, "data: %s\n\n", raw)
	}
	response.WriteString("data: [DONE]\n\n")
	return response.String()
}

func TestCompatibleProfilesChatJSONSSEToolsUsageAliasAndErrors(t *testing.T) {
	for _, profile := range []provider.Profile{provider.ProfileCPA, provider.ProfileNewAPI, provider.ProfileSub2API, provider.ProfileVLLM} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", profile, stream), func(t *testing.T) {
				t.Setenv("COMPATIBLE_PROFILE_KEY", "controlled-upstream-key")
				proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/relay/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer controlled-upstream-key" || gjson.GetBytes(body, "model").String() != "actual-model" {
						t.Errorf("兼容接口未保留模型别名/鉴权/挂载路径: %s", r.URL.Path)
					}
					if gjson.GetBytes(body, "metadata.test_error").Bool() {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(429)
						io.WriteString(w, `{"error":{"message":"controlled rate limit","type":"rate_limit_error","code":"rate_limit"}}`)
						return
					}
					if gjson.GetBytes(body, "stream").Bool() != stream || gjson.GetBytes(body, "tools.0.function.name").String() != "weather" {
						t.Error("请求的流标志或函数定义被修改")
					}
					withResult := gjson.GetBytes(body, "messages.2.role").String() == "tool"
					if withResult && (gjson.GetBytes(body, "messages.2.tool_call_id").String() != "weather-call" || gjson.GetBytes(body, "messages.2.content").String() != "上海晴天") {
						t.Error("工具返回被修改")
					}
					contentType := "application/json"
					if stream {
						contentType = "text/event-stream"
					}
					w.Header().Set("Content-Type", contentType)
					io.WriteString(w, compatibleChatResponse(stream, withResult))
				}), 8192)
				p := provider.Provider{ID: "compatible", Profile: profile, BaseURL: proxy.target.String() + "/relay/v1", Protocol: "passthrough", KeyEnv: "COMPATIBLE_PROFILE_KEY", Capabilities: provider.Capabilities{ChatCompletions: provider.CapabilitySupported}}
				if err := proxy.providers.Set(provider.Config{Providers: []provider.Provider{p}, Routes: []provider.Route{{ID: "alias", Model: "visible-model", TargetModel: "actual-model", ProviderID: p.ID}}}); err != nil {
					t.Fatal(err)
				}
				tools := `"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]`
				initial := fmt.Sprintf(`{"model":"visible-model","stream":%t,"messages":[{"role":"user","content":"上海天气"}],%s}`, stream, tools)
				headers := map[string]string{"X-Session-ID": "controlled-conversation", "Authorization": "Bearer client-key"}
				trace, first := send(t, proxy, "POST", "/v1/chat/completions", initial, headers)
				log := logOf(t, storage, trace)
				if first.Code != 200 || first.Body.String() != compatibleChatResponse(stream, false) || log.Status != "done" || len(log.Tools) != 1 || log.Tools[0].CallID != "weather-call" || log.Tools[0].Input != `{"city":"上海"}` || log.Tools[0].Duration != nil || log.InputTokens != 8 || log.OutputTokens != 3 {
					t.Fatalf("JSON/SSE 函数或 usage 观察错误: %+v %s", log, first.Body)
				}
				headers["X-Parent-Trace-ID"] = trace
				followup := fmt.Sprintf(`{"model":"visible-model","stream":%t,"messages":[{"role":"user","content":"上海天气"},{"role":"assistant","content":null,"tool_calls":[{"id":"weather-call","type":"function","function":{"name":"weather","arguments":"{\"city\":\"上海\"}"}}]},{"role":"tool","tool_call_id":"weather-call","content":"上海晴天"}],%s}`, stream, tools)
				resultTrace, second := send(t, proxy, "POST", "/v1/chat/completions", followup, headers)
				resultLog, toolLog := logOf(t, storage, resultTrace), logOf(t, storage, trace)
				if second.Code != 200 || second.Body.String() != compatibleChatResponse(stream, true) || resultLog.InputTokens != 12 || resultLog.OutputTokens != 4 || toolLog.Tools[0].Output != "上海晴天" || toolLog.Tools[0].ResultTrace != resultTrace || toolLog.Tools[0].Duration != nil {
					t.Fatalf("工具返回/usage 未保持可追溯: %+v %+v", resultLog, toolLog.Tools)
				}
				errorTrace, rejected := send(t, proxy, "POST", "/v1/chat/completions", `{"model":"visible-model","messages":[{"role":"user","content":"request"}],"metadata":{"test_error":true}}`, headers)
				if rejected.Code != 429 || !strings.Contains(rejected.Body.String(), "controlled rate limit") || logOf(t, storage, errorTrace).Status != "error" {
					t.Fatal("上游错误被改写或当作成功")
				}
			})
		}
	}
}
