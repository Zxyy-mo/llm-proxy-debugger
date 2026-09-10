package store

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
)

func TestPersistentHistoryRestoresFilesAndInterruptsPending(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gateway.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := correlation.ExtractRequest(http.Header{"X-Session-Id": []string{"history"}}, "/v1/responses", []byte(`{"model":"mock","input":"hello"}`), "https://provider.invalid")
	log := s.Begin(RequestLog{TraceID: "one", Method: "POST", Path: "/v1/responses"}, input)
	s.CaptureOriginal("one", RequestSnapshot{Method: "POST", Body: `{"input":"hello","large":9007199254740993}`, Forwarding: &Forwarding{Host: "provider.invalid", Headers: http.Header{"Accept": []string{"application/json"}}}})
	log.StatusCode = 200
	s.Complete("one", log, correlation.Response{ID: "response-one", Complete: true})
	s.Begin(RequestLog{TraceID: "pending", Method: "POST", Path: "/v1/responses"}, input)
	s.SetSetting("test", map[string]string{"value": "persisted"})
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, _ := s.Log("pending")
	if loaded.Status != "error" || loaded.StatusCode != 503 || !strings.Contains(loaded.Error, "不会自动补发") {
		t.Fatalf("pending was resurrected: %+v", loaded)
	}
	capture, ok := s.RequestSnapshot("one")
	if !ok || capture.Original.Unavailable != "" || !strings.Contains(capture.Original.Body, "9007199254740993") || capture.Original.Forwarding.Host != "provider.invalid" {
		t.Fatalf("snapshot did not survive restart: %+v", capture)
	}
	var setting map[string]string
	if !s.Setting("test", &setting) || setting["value"] != "persisted" {
		t.Fatal("settings lost")
	}
	w := httptest.NewRecorder()
	s.HistoryHandler(w, httptest.NewRequest("GET", "/api/history?q=mock&limit=1", nil))
	var result struct {
		Total int          `json:"total"`
		Next  string       `json:"next_cursor"`
		Items []RequestLog `json:"items"`
	}
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Total != 2 || len(result.Items) != 1 || result.Next == "" {
		t.Fatalf("pagination: %s", w.Body)
	}
	file := capture.Original.FilePath
	w = httptest.NewRecorder()
	s.HistoryHandler(w, httptest.NewRequest("DELETE", "/api/history/one", nil))
	if w.Code != 200 {
		t.Fatalf("cleanup: %d %s", w.Code, w.Body)
	}
	if _, err = os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("body file remained after cleanup")
	}
	if _, ok = s.Log("one"); ok {
		t.Fatal("deleted record remained")
	}
}

func TestCleanupProtectsActiveRequestsAndOutsideFiles(t *testing.T) {
	s := New()
	if err := s.SetCaptureRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	s.Begin(RequestLog{TraceID: "active"}, correlation.Request{})
	w := httptest.NewRecorder()
	s.HistoryHandler(w, httptest.NewRequest("DELETE", "/api/history/active", nil))
	if w.Code != 409 {
		t.Fatal("active request was deleted")
	}
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	log := s.Begin(RequestLog{TraceID: "finished"}, correlation.Request{})
	log.StatusCode = 200
	s.Complete("finished", log, correlation.Response{})
	s.CaptureResponse("finished", ResponseSnapshot{Path: outside, Stored: "file"})
	_, _, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == "finished" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(outside); err != nil {
		t.Fatal("cleanup escaped its capture root")
	}
}
