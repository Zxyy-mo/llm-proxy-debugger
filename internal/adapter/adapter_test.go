package adapter

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/sse"
	"github.com/tidwall/gjson"
)

func TestRequestConversionPreservesToolTurnsAndPrecision(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/messages", `{"model":"alias","max_tokens":20,"system":[{"type":"text","text":"rules"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"lookup","input":{"id":9007199254740993}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"found"},{"type":"text","text":"continue"}]}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"any","disable_parallel_tool_use":true}}`},
		{"/v1/responses", `{"model":"alias","max_output_tokens":20,"instructions":"rules","store":false,"input":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"id\":9007199254740993}"},{"type":"function_call_output","call_id":"call-1","output":"found"},{"role":"user","content":[{"type":"input_text","text":"continue"}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required","parallel_tool_calls":false}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			path, raw, plan, err := Request(tc.path, []byte(tc.body), "openai", "actual-model")
			if err != nil {
				t.Fatal(err)
			}
			root := gjson.ParseBytes(raw)
			if path != "/v1/chat/completions" || plan == nil || root.Get("model").String() != "actual-model" || root.Get("messages.0.content").String() != "rules" || root.Get("messages.2.role").String() != "tool" || root.Get("messages.2.tool_call_id").String() != "call-1" || root.Get("messages.3.content").String() != "continue" || !strings.Contains(string(raw), "9007199254740993") || root.Get("tool_choice").String() != "required" || root.Get("parallel_tool_calls").Bool() {
				t.Fatalf("bad translation: %s", raw)
			}
		})
	}
}

func TestUnsupportedSemanticsAreRejected(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"m","input":"x","previous_response_id":"remote"}`},
		{"/v1/responses", `{"model":"m","input":"x","store":true}`},
		{"/v1/responses", `{"model":"m","input":"x","tools":[{"type":"web_search"}]}`},
		{"/v1/messages", `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`},
		{"/v1/messages", `{"model":"m","messages":[],"thinking":{"type":"enabled","budget_tokens":1024}}`},
	} {
		if _, _, _, err := Request(tc.path, []byte(tc.body), "openai", ""); err == nil {
			t.Errorf("silently accepted %s", tc.body)
		}
	}
	const raw = ` { "model":"m", "opaque":{"future":9007199254740993} } `
	_, body, plan, err := Request("/v1/responses", []byte(raw), "passthrough", "")
	if err != nil || string(body) != raw || plan != nil {
		t.Fatal("native bytes changed")
	}
}

func chatResponse(body string, stream bool) *http.Response {
	content := "application/json"
	if stream {
		content = "text/event-stream"
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {content}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestJSONAndSSEConversionsKeepContentToolsAndUsage(t *testing.T) {
	const jsonBody = `{"id":"upstream-id","model":"actual","choices":[{"message":{"role":"assistant","content":"你好","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"id\":9007199254740993}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":0,"completion_tokens":12,"completion_tokens_details":{"reasoning_tokens":0}}}`
	stream := "data: {\"id\":\"upstream-id\",\"model\":\"actual\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"id\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"9007199254740993}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":12,\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\n" + "data: [DONE]\n\n"
	for _, source := range []string{"anthropic", "responses"} {
		for _, streaming := range []bool{false, true} {
			name := source + "-json"
			body := jsonBody
			if streaming {
				name = source + "-sse"
				body = stream
			}
			t.Run(name, func(t *testing.T) {
				p := &Plan{Source: source, Stream: streaming, ID: "gateway-id", Model: "model"}
				resp, err := p.Response(chatResponse(body, streaming))
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				raw, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				var handler protocol.Handler = &protocol.AnthropicHandler{}
				if source == "responses" {
					handler = &protocol.ResponsesHandler{}
				}
				metrics := protocol.NewAccumulator("t", handler)
				capture := correlation.NewResponseCapture(source)
				if streaming {
					parser := sse.NewSSEParser()
					for _, event := range parser.Feed(raw) {
						metrics.Accumulate([]byte(event.Data))
						capture.Event([]byte(event.Data))
					}
				} else {
					metrics.Accumulate(raw)
					capture.JSON(raw)
				}
				if metrics.OutputContent != "你好" || metrics.InputTokens != 0 || metrics.OutputTokens != 12 || metrics.TokenSources.Input != "usage" || metrics.TokenSources.Output != "usage" || metrics.ToolUseCount != 1 || !capture.Response().Complete || capture.Response().ID != "gateway-id" || !strings.Contains(string(raw), "9007199254740993") {
					t.Fatalf("conversion lost data: %+v, %+v\n%s", metrics, capture.Response(), raw)
				}
			})
		}
	}
}

func TestConversionErrorsAreExplicitInJSONAndSSE(t *testing.T) {
	p := &Plan{Source: "anthropic"}
	r := chatResponse(`{"error":{"message":"quota exceeded"}}`, false)
	r.StatusCode = 429
	resp, err := p.Response(r)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 429 || gjson.GetBytes(raw, "type").String() != "error" || !strings.Contains(string(raw), "quota exceeded") {
		t.Fatalf("error changed: %s", raw)
	}
	p.Stream = true
	resp, err = p.Response(chatResponse("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", true))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "event: error") || strings.Contains(string(raw), "event: message_stop") {
		t.Fatalf("incomplete response advertised as complete: %s", raw)
	}
}
