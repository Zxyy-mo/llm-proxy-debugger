package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/config"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"go.uber.org/zap"
)

func testProxy(t *testing.T, upstream http.Handler, limit int) (*Server, *store.Store) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	h := hub.New(zap.NewNop())
	stop, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		for {
			select {
			case event := <-h.Broadcast:
				// Exercise the asynchronous serialization boundary under -race.
				json.Marshal(event)
			case <-stop:
				return
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-finished })
	s := store.New()
	proxy, err := NewServer(&config.Config{TargetAddr: server.URL, MaxBodyLogSize: limit, LogDir: t.TempDir()}, zap.NewNop(), h, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(proxy.transport.CloseIdleConnections)
	return proxy, s
}

func onlyLog(t *testing.T, s *store.Store) store.RequestLog {
	t.Helper()
	for _, session := range s.SessionsSnapshot() {
		if len(session.Logs) == 1 {
			return session.Logs[0]
		}
	}
	t.Fatal("expected one captured request")
	return store.RequestLog{}
}

func TestJSONCaptureBeforeTruncationAndNoDoubleLogging(t *testing.T) {
	for _, limit := range []int{32, 4096, 0} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			response := `{"object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("answer", 30) + `"}]}],"id":"resp-after-long-content","usage":{"input_tokens":10,"output_tokens":8}}`
			request := `{"input":"` + strings.Repeat("question", 30) + `","conversation":{"id":"conv-at-end"},"model":"mock"}`
			proxy, s := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != request {
					t.Error("proxy changed the request body")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, response)
			}), limit)
			rr := httptest.NewRecorder()
			proxy.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(request)))
			if rr.Body.String() != response || rr.Code != http.StatusOK {
				t.Fatal("proxy changed the JSON response")
			}
			log := onlyLog(t, s)
			if log.Correlation.ResponseID != "resp-after-long-content" || log.Correlation.ConversationID != "conv-at-end" || log.InputTokens != 10 || log.OutputTokens != 8 {
				t.Fatalf("metadata was affected by log limit: %+v", log)
			}
			if rr.Header().Get("X-Gateway-Trace-ID") != log.TraceID {
				t.Fatal("gateway trace ID not exposed to caller")
			}
			if limit == 4096 && log.ResponseBody != response {
				t.Fatalf("JSON log duplicated or corrupted: %s", log.ResponseBody)
			}
			if limit == 0 && (!strings.HasPrefix(log.RequestBody, "... [truncated, total ") || log.ResponseBody != "") {
				t.Fatal("zero log size did not suppress body")
			}
		})
	}
}

func TestSSECaptureAcrossArbitraryChunkBoundaries(t *testing.T) {
	cases := []struct {
		path, stream, id, text, thinking string
		tokens                           int
	}{
		{
			path: "/v1/chat/completions", id: "chat-stream", text: "你好", thinking: "公开推理", tokens: 4,
			stream: "data: {\"id\":\"chat-stream\",\"choices\":[{\"delta\":{\"reasoning_content\":\"公开推理\"}}]}\n\n" +
				"data: {\"id\":\"chat-stream\",\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":4}}\n\n" + "data: [DONE]\n\n",
		},
		{
			path: "/v1/responses", id: "resp-stream", text: "hello", tokens: 2,
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stream\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":5,\"output_tokens\":2}}}\n\n",
		},
		{
			path: "/v1/messages", id: "msg-stream", text: "hello", thinking: "visible thought", tokens: 3,
			stream: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stream\",\"usage\":{\"input_tokens\":5}}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"visible thought\"}}\n\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
				"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n" + "data: {\"type\":\"message_stop\"}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			proxy, s := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for i := 0; i < len(tc.stream); i += 3 {
					end := min(i+3, len(tc.stream))
					io.WriteString(w, tc.stream[i:end])
					w.(http.Flusher).Flush()
				}
			}), 64)
			rr := httptest.NewRecorder()
			proxy.ServeHTTP(rr, httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"mock","stream":true}`)))
			if rr.Body.String() != tc.stream {
				t.Fatal("raw SSE bytes changed")
			}
			log := onlyLog(t, s)
			if log.Status != "done" || log.Correlation.ResponseID != tc.id || log.ResponseBody != tc.text || log.ThinkingContent != tc.thinking || log.OutputTokens != tc.tokens {
				t.Fatalf("stream not fully captured: %+v", log)
			}
		})
	}
}

func TestIncompleteStreamAndHTTPErrorAreVisible(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		status                  int
	}{
		{"incomplete", "text/event-stream", "data: {\"id\":\"partial\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n", 200},
		{"upstream error", "application/json", `{"error":{"message":"rate limited"}}`, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy, s := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}), 1024)
			rr := httptest.NewRecorder()
			proxy.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"test"}]}`)))
			log := onlyLog(t, s)
			if log.Status != "error" || log.Error == "" || rr.Code != tc.status || rr.Body.String() != tc.body {
				t.Fatalf("error did not survive capture: %+v", log)
			}
		})
	}
}

func TestGenericSSEIsNotMisreportedAsAnIncompleteLLMResponse(t *testing.T) {
	stream := "event: progress\ndata: {\"id\":\"job-1\",\"percent\":100}\n\n"
	proxy, s := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}), 1024)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, httptest.NewRequest("GET", "/progress", nil))
	log := onlyLog(t, s)
	if log.Status != "done" || log.Error != "" || log.Correlation.ResponseID != "" || rr.Body.String() != stream {
		t.Fatalf("generic SSE incorrectly treated as LLM output: %+v", log)
	}
}
