package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/intercept"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func responseDetail(t *testing.T, s *store.Store, path string) store.ResponseSnapshot {
	t.Helper()
	w := httptest.NewRecorder()
	s.ResponsesHandler(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 200 {
		t.Fatalf("response detail: %d %s", w.Code, w.Body)
	}
	var response store.ResponseSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestCompleteResponseBytesAndDownloads(t *testing.T) {
	large := []byte(`{"object":"response","id":"large","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + strings.Repeat("你好large", 50000) + `"}]}],"number":9007199254740993,"usage":{"input_tokens":11,"output_tokens":17}}`)
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = gz.Write(large)
	_ = gz.Close()
	stream := []byte("event: message\r\ndata: {\"id\":\"chat\",\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\r\n\r\ndata: [DONE]\r\n\r\n")
	cases := []struct {
		name, typ, encoding string
		wire, saved         []byte
	}{
		{"large-json", "application/json", "", large, large},
		{"gzip", "application/json", "gzip", compressed.Bytes(), large},
		{"sse", "text/event-stream", "", stream, stream},
		{"binary", "application/octet-stream", "", []byte{0, 0xff, 0x80, 13, 10}, []byte{0, 0xff, 0x80, 13, 10}},
		{"empty", "application/json", "", []byte{}, []byte{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.typ)
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				_, _ = w.Write(tc.wire)
			}), 31)
			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"mock","input":"x"}`)))
			if !bytes.Equal(w.Body.Bytes(), tc.wire) {
				t.Fatal("forwarded bytes changed")
			}
			log := onlyLog(t, storage)
			path := "/api/responses/" + log.TraceID
			detail := responseDetail(t, storage, path)
			body := []byte(detail.Body)
			if detail.BodyEncoding == "base64" {
				var err error
				body, err = base64.StdEncoding.DecodeString(detail.Body)
				if err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(body, tc.saved) || detail.Bytes != int64(len(tc.saved)) || detail.Stored != "file" || detail.Receiving {
				t.Fatalf("full response not preserved: bytes=%d stored=%s encoding=%s", detail.Bytes, detail.Stored, detail.BodyEncoding)
			}
			if detail.Decoded != (tc.encoding == "gzip") {
				t.Fatal("compression facts incorrect")
			}
			download := httptest.NewRecorder()
			storage.ResponsesHandler(download, httptest.NewRequest("GET", path+"/download", nil))
			if download.Code != 200 || !bytes.Equal(download.Body.Bytes(), tc.saved) || !strings.Contains(download.Header().Get("Content-Disposition"), log.TraceID) {
				t.Fatal("download did not preserve exact bytes/name")
			}
			if log.ResponseBytes != detail.Bytes || log.ResponseStored != "file" {
				t.Fatal("log metadata disagrees with capture")
			}
			if tc.name == "large-json" {
				preview := responseDetail(t, storage, path+"?limit=137")
				if !preview.Truncated || !utf8.ValidString(preview.Body) || strings.Contains(preview.Body, "\ufffd") {
					t.Fatal("preview damaged UTF-8")
				}
			}
		})
	}
}

func TestResponseCaptureFailureAndMissingFiles(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "cannot-write"}[failed], func(t *testing.T) {
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"ok":true}`)
			}), 10)
			if failed {
				path := filepath.Join(t.TempDir(), "file")
				if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
				proxy.cfg.LogDir = path
			}
			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"x"}`)))
			if w.Code != 200 || w.Body.String() != `{"ok":true}` {
				t.Fatal("capture failure broke proxy")
			}
			log := onlyLog(t, storage)
			if !failed {
				response, _ := storage.ResponseSnapshot(log.TraceID)
				if err := os.Remove(response.Path); err != nil {
					t.Fatal(err)
				}
			}
			response := responseDetail(t, storage, "/api/responses/"+log.TraceID)
			if response.Stored != "missing" || response.Reason == "" || response.Body != "" {
				t.Fatalf("missing response was not explicit: %+v", response)
			}
		})
	}
}

func TestResponseTimingsExcludeInterceptionWait(t *testing.T) {
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(15 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(15 * time.Millisecond)
		io.WriteString(w, "data: {\"id\":\"timing\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
	}), 64)
	storage.Rules = []store.Rule{{ID: "pause", PathMatch: "/chat/completions", Intercept: true, WaitSeconds: 30, TimeoutAction: "forward"}}
	pending, _, done, _ := startPending(t, proxy, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)))
	time.Sleep(100 * time.Millisecond)
	control(t, proxy, "POST", "/api/interceptions/"+pending.TraceID+"/release", intercept.Edit{Revision: 1}, 200)
	waitRequest(t, done)
	log := onlyLog(t, storage)
	if log.TTFB == nil || log.TTFC == nil || *log.TTFB < 10 || *log.TTFC < *log.TTFB || log.WaitDuration < 95 || *log.TTFC > log.UpstreamDuration || log.Duration < log.WaitDuration+*log.TTFC {
		t.Fatalf("inconsistent clocks: %+v", log)
	}
	if log.TokenSources.Input != protocol.Usage || log.TokenSources.Output != protocol.Usage {
		t.Fatal("source lost on completion")
	}
	graph, _ := storage.GraphSnapshot("")
	if graph.Nodes[0].TokenSources != log.TokenSources || graph.Nodes[0].TTFC == nil {
		t.Fatal("graph disagrees with log")
	}
}

func TestFailedAndEmptyResponsesHaveUnknownTiming(t *testing.T) {
	for _, body := range []string{`{"error":{"message":"failure"}}`, `{"id":"empty","choices":[{"message":{"role":"assistant","content":""}}]}`} {
		proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(body, "failure") {
				w.WriteHeader(500)
			}
			io.WriteString(w, body)
		}), 64)
		proxy.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[]}`)))
		log := onlyLog(t, storage)
		if log.TTFB != nil || log.TTFC != nil || log.TokenSources != protocol.UnknownSources() {
			t.Fatalf("invented metrics: %+v", log)
		}
	}
}

func TestOversizedSSEPreservesWireAndDisclosesIncompleteObservation(t *testing.T) {
	stream := "data: {\"id\":\"large-event\",\"choices\":[{\"delta\":{\"content\":\"prefix\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", 2<<20) + "\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\ndata: [DONE]\n\n"
	for _, private := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "private-projection"}[private], func(t *testing.T) {
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, stream)
			}), 1024)
			policy := storage.Privacy.Policy()
			policy.Record, policy.RetainRaw = private, !private
			if err := storage.Privacy.Set(policy); err != nil {
				t.Fatal(err)
			}
			trace, reply := send(t, proxy, "POST", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"large"}],"stream":true}`, nil)
			log := logOf(t, storage, trace)
			if reply.Code != 200 || reply.Body.String() != stream || log.Status != "done" {
				t.Fatal("observation limit affected response forwarding")
			}
			if log.ObservationWarning == "" || log.TokenSources != protocol.UnknownSources() || log.TTFC != nil {
				t.Fatal("partial parsing was advertised as complete metrics")
			}
			detail := responseDetail(t, storage, "/api/responses/"+trace)
			if detail.ObservationWarning == "" || detail.Complete == private {
				t.Fatal("capture completeness does not distinguish raw bytes from partial projection")
			}
			if !private && detail.Body != stream {
				t.Fatal("raw SSE capture changed")
			}
		})
	}
}
