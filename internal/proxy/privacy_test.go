package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordingRedactionDoesNotChangeForwarding(t *testing.T) {
	for _, outbound := range []bool{false, true} {
		t.Run(map[bool]string{false: "record-only", true: "outbound"}[outbound], func(t *testing.T) {
			const private = "a@example.com"
			var received string
			proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				received = string(body)
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"id":"privacy","choices":[{"message":{"role":"assistant","content":"a@example.com"}}]}`)
			}), 1024)
			policy := storage.Privacy.Policy()
			policy.Record = true
			policy.Outbound = outbound
			policy.RetainRaw = false
			if err := storage.Privacy.Set(policy); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"a@example.com"}],"seed":9007199254740993}`)))
			if strings.Contains(received, private) == outbound || !strings.Contains(received, "9007199254740993") {
				t.Fatalf("outbound policy mismatch: %s", received)
			}
			if !strings.Contains(w.Body.String(), private) {
				t.Fatal("recording redaction changed upstream response forwarding")
			}
			log := onlyLog(t, storage)
			raw, _ := json.Marshal(log)
			if strings.Contains(string(raw), private) {
				t.Fatalf("log leaked private text: %s", raw)
			}
			api := httptest.NewRecorder()
			proxy.RequestsHandler(api, httptest.NewRequest("GET", "/api/requests/"+log.TraceID, nil))
			if strings.Contains(api.Body.String(), private) {
				t.Fatal("capture API leaked raw text")
			}
			if err := filepath.WalkDir(proxy.cfg.LogDir, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !entry.IsDir() {
					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}
					if strings.Contains(string(data), private) {
						t.Errorf("recording retained raw text in %s", entry.Name())
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrivateSSEDoesNotStoreSplitSensitiveFragments(t *testing.T) {
	stream := "data: {\"id\":\"privacy-sse\",\"choices\":[{\"delta\":{\"content\":\"a@exa\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"mple.com\"}}]}\n\ndata: [DONE]\n\n"
	proxy, storage := testProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}), 4096)
	policy := storage.Privacy.Policy()
	policy.Record = true
	policy.RetainRaw = false
	_ = storage.Privacy.Set(policy)
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hello"}]}`)))
	if w.Body.String() != stream {
		t.Fatal("privacy changed forwarded stream")
	}
	log := onlyLog(t, storage)
	detail := responseDetail(t, storage, "/api/responses/"+log.TraceID)
	if detail.Representation != "redacted-output" || strings.Contains(detail.Body, "a@exa") || strings.Contains(detail.Body, "mple.com") || !strings.Contains(detail.Body, "PRIVATE_email") {
		t.Fatalf("unsafe stream representation: %+v", detail)
	}
}
