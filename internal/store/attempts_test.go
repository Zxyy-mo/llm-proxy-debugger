package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
)

func attemptRequest(body, host string) RequestSnapshot {
	return RequestSnapshot{Method: "POST", URL: "https://" + host + "/mount/v1/chat/completions", Headers: map[string]string{"Content-Type": "application/json"}, Body: body, ContentLength: int64(len(body)), Forwarding: &Forwarding{Scheme: "https", Host: host, BaseURL: "https://" + host + "/mount", Path: "/mount/v1/chat/completions", Headers: http.Header{"Content-Type": {"application/json"}, "X-Custom": {"first", "second"}}}}
}

func TestAttemptsKeepIndependentRequestsAndAuthoritativeResults(t *testing.T) {
	s := New()
	if err := s.SetCaptureRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	log := beginRun(s, "trace", "session", "run")
	s.CaptureOriginal(log.TraceID, attemptRequest(`{"original":true}`, "original.test"))
	firstSnapshot := attemptRequest(`{"attempt":1}`, "first.test")
	first, ok := s.BeginAttempt(log.TraceID, RouteAttempt{ProviderID: "first", URL: "https://first.test/mount"}, firstSnapshot)
	if !ok || first.ID == "" || first.Sequence != 1 || first.TraceID != log.TraceID || first.Source != "gateway" || first.Status != "running" || first.TotalDuration != nil {
		t.Fatalf("incorrect attempt admission: %+v", first)
	}
	if _, err := time.Parse(time.RFC3339Nano, first.StartedAt); err != nil {
		t.Fatal("actual admission time missing")
	}
	firstSnapshot.Headers["Content-Type"] = "mutated"
	firstSnapshot.Forwarding.Headers["X-Custom"][0] = "mutated"
	first.Status, first.StatusCode, first.Duration = "abandoned", 503, 4
	first.EndedAt = time.Now().Format(time.RFC3339Nano)
	total := 9.0
	first.TotalDuration = &total
	if !s.UpdateAttempt(log.TraceID, first) {
		t.Fatal("first attempt did not finish")
	}
	total = 1000
	second, ok := s.BeginAttempt(log.TraceID, RouteAttempt{ProviderID: "second", URL: "https://second.test/mount"}, attemptRequest(`{"attempt":2}`, "second.test"))
	if !ok || second.Sequence != 2 || second.ID == first.ID {
		t.Fatalf("second attempt is not independent: %+v", second)
	}
	// 修改结果时提交过期或恶意的不可变字段，不能改变实际发送目的地。
	second.Status, second.StatusCode, second.Duration = "done", 200, 5
	second.ProviderID, second.URL, second.RequestURL, second.Sequence = "changed", "changed", "changed", 99
	second.TotalDuration = new(float64)
	*second.TotalDuration = 15
	second.EndedAt = time.Now().Format(time.RFC3339Nano)
	if !s.UpdateAttempt(log.TraceID, second) {
		t.Fatal("second attempt did not finish")
	}
	first.Status = "running"
	if s.UpdateAttempt(log.TraceID, first) {
		t.Fatal("finished attempt became running")
	}
	s.SetRoute(log.TraceID, RouteInfo{ProviderID: "stale", Attempts: []RouteAttempt{}})
	log.StatusCode = 200
	completed := s.Complete(log.TraceID, log, correlation.Response{Complete: true})
	if len(completed.Route.Attempts) != 2 || completed.Route.ProviderID != "second" {
		t.Fatalf("stale route/completion erased attempts: %+v", completed.Route)
	}
	firstDetail, _ := s.AttemptSnapshot(log.TraceID, first.ID)
	secondDetail, _ := s.AttemptSnapshot(log.TraceID, second.ID)
	if firstDetail.Request.Body != `{"attempt":1}` || secondDetail.Request.Body != `{"attempt":2}` || firstDetail.Request.FilePath == secondDetail.Request.FilePath || *firstDetail.Attempt.TotalDuration != 9 {
		t.Fatalf("attempt snapshots/results overwritten: %+v %+v", firstDetail, secondDetail)
	}
	if firstDetail.Request.Headers["Content-Type"] != "application/json" || firstDetail.Request.Forwarding.Headers["X-Custom"][0] != "first" {
		t.Fatal("outgoing snapshot kept mutable caller-owned maps")
	}
	if secondDetail.Attempt.ProviderID != "second" || secondDetail.Attempt.Sequence != 2 || !strings.Contains(secondDetail.Attempt.RequestURL, "second.test/mount/v1/chat/completions") {
		t.Fatalf("mutable result changed identity: %+v", secondDetail.Attempt)
	}
	capture, _ := s.RequestSnapshot(log.TraceID)
	if capture.Outgoing == nil || capture.Outgoing.Body != secondDetail.Request.Body || capture.Outgoing.FilePath != secondDetail.Request.FilePath {
		t.Fatalf("legacy outgoing does not point to final attempt: %+v", capture)
	}
	firstDetail.Request.Forwarding.Headers["X-Custom"][0] = "returned-mutation"
	*firstDetail.Attempt.TotalDuration = -1
	firstDetail, _ = s.AttemptSnapshot(log.TraceID, first.ID)
	if firstDetail.Request.Forwarding.Headers["X-Custom"][0] != "first" || *firstDetail.Attempt.TotalDuration != 9 {
		t.Fatal("attempt API returned mutable internal state")
	}
}

func TestAttemptsDoNotCreateMissingOrUnsentRequests(t *testing.T) {
	s := New()
	if _, ok := s.BeginAttempt("absent", RouteAttempt{}, RequestSnapshot{}); ok || len(s.records) != 0 {
		t.Fatal("begin attempt fabricated a request")
	}
	log := beginRun(s, "local-failure", "", "")
	log.StatusCode, log.Error = 422, "local conversion rejected"
	s.Complete(log.TraceID, log, correlation.Response{})
	if _, ok := s.BeginAttempt(log.TraceID, RouteAttempt{}, RequestSnapshot{}); ok {
		t.Fatal("terminal local failure acquired an attempt")
	}
	w := httptest.NewRecorder()
	s.AttemptsHandler(w, httptest.NewRequest("GET", "/api/attempts/local-failure", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"attempts":[]`) {
		t.Fatalf("unsent is not an empty list: %d %s", w.Code, w.Body)
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/api/attempts/absent", 404},
		{"GET", "/api/attempts/local-failure/unknown", 404},
		{"GET", "/api/attempts/local-failure/unknown/other", 404},
		{"POST", "/api/attempts/local-failure", 405},
	} {
		w := httptest.NewRecorder()
		s.AttemptsHandler(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
}

func TestAttemptProtocolFailureOnlyCorrectsOutcome(t *testing.T) {
	s := New()
	log := beginRun(s, "protocol-error", "", "")
	attempt, _ := s.BeginAttempt(log.TraceID, RouteAttempt{}, attemptRequest(`{}`, "upstream.test"))
	attempt.Status, attempt.StatusCode, attempt.Duration = "done", 200, 3
	attempt.HeadersAt = "2026-09-11T00:00:00.003Z"
	attempt.EndedAt = "2026-09-11T00:00:00.010Z"
	total := 10.0
	attempt.TotalDuration = &total
	s.UpdateAttempt(log.TraceID, attempt)
	correction := attempt
	correction.Status, correction.Error = "error", "SSE stream ended without completion"
	correction.StatusCode, correction.Duration, correction.HeadersAt, correction.EndedAt = 502, 0, "changed", "changed"
	correction.TotalDuration = nil
	if !s.UpdateAttempt(log.TraceID, correction) {
		t.Fatal("late protocol failure was dropped")
	}
	observed, _ := s.AttemptsSnapshot(log.TraceID)
	if observed[0].Status != "error" || observed[0].Error != correction.Error || observed[0].StatusCode != 200 || observed[0].HeadersAt != attempt.HeadersAt || observed[0].EndedAt != attempt.EndedAt || observed[0].Duration != 3 || *observed[0].TotalDuration != 10 {
		t.Fatalf("protocol correction rewrote transport observations: %+v", observed[0])
	}
	if s.UpdateAttempt(log.TraceID, attempt) {
		t.Fatal("error was changed back to success")
	}
}

func TestAttemptBodyDownloadPreservesCompleteBytes(t *testing.T) {
	s := New()
	for _, tc := range []struct {
		trace   string
		body    []byte
		encoded bool
	}{
		{"text", []byte(strings.Repeat("完整正文\n", 10000) + "END"), false},
		{"binary", []byte{0, 0xff, '\n', 1, 2, 3}, true},
	} {
		beginRun(s, tc.trace, "", "")
		snapshot := attemptRequest(string(tc.body), "upstream.test")
		if tc.encoded {
			snapshot.Body = base64.StdEncoding.EncodeToString(tc.body)
			snapshot.BodyEncoding = "base64"
		}
		attempt, _ := s.BeginAttempt(tc.trace, RouteAttempt{}, snapshot)
		w := httptest.NewRecorder()
		s.AttemptsHandler(w, httptest.NewRequest("GET", "/api/attempts/"+tc.trace+"/"+attempt.ID+"/body", nil))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), tc.body) || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("body download changed bytes: %s status=%d length=%d", tc.trace, w.Code, w.Body.Len())
		}
	}
}

func TestAttemptPersistenceRestoresCapturesAndInterruptsOnlyActive(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gateway.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	log := beginRun(s, "persistent", "session", "run")
	s.CaptureOriginal(log.TraceID, attemptRequest(`{"original":true}`, "original.test"))
	first, _ := s.BeginAttempt(log.TraceID, RouteAttempt{ProviderID: "first"}, attemptRequest(`{"attempt":1}`, "first.test"))
	first.Status, first.StatusCode, first.Duration = "abandoned", 503, 7
	first.EndedAt = time.Now().Format(time.RFC3339Nano)
	total := 8.0
	first.TotalDuration = &total
	s.UpdateAttempt(log.TraceID, first)
	second, _ := s.BeginAttempt(log.TraceID, RouteAttempt{ProviderID: "second"}, attemptRequest(`{"attempt":2}`, "second.test"))
	second.StatusCode, second.Duration, second.HeadersAt = 200, 4, time.Now().Format(time.RFC3339Nano)
	s.UpdateAttempt(log.TraceID, second)
	firstCapture, _ := s.AttemptSnapshot(log.TraceID, first.ID)
	secondCapture, _ := s.AttemptSnapshot(log.TraceID, second.ID)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restored, _ := s.Log(log.TraceID)
	if restored.RunID != log.RunID || restored.Status != "error" {
		t.Fatalf("run identity or interruption lost: %+v", restored)
	}
	attempts, _ := s.AttemptsSnapshot(log.TraceID)
	if len(attempts) != 2 || attempts[0].Status != "abandoned" || *attempts[0].TotalDuration != 8 || attempts[1].Status != "interrupted" || attempts[1].EndedAt != "" || attempts[1].TotalDuration != nil || attempts[1].StatusCode != 200 {
		t.Fatalf("restored attempt invented an outcome: %+v", attempts)
	}
	for _, saved := range []AttemptDetail{firstCapture, secondCapture} {
		detail, ok := s.AttemptSnapshot(log.TraceID, saved.Attempt.ID)
		if !ok || detail.Request.Body != saved.Request.Body || detail.Request.FilePath != saved.Request.FilePath || detail.Request.Forwarding.Host != saved.Request.Forwarding.Host || detail.Request.Forwarding.Headers["X-Custom"][1] != "second" {
			t.Fatalf("private attempt forwarding/body disappeared: %+v", detail)
		}
	}
	if _, _, err = s.cleanup(func(item RequestLog) bool { return item.TraceID == log.TraceID }); err != nil {
		t.Fatal(err)
	}
	for _, capture := range []AttemptDetail{firstCapture, secondCapture} {
		if _, err = os.Stat(capture.Request.FilePath); !os.IsNotExist(err) {
			t.Fatalf("owned attempt file remains: %s", capture.Request.FilePath)
		}
	}
	runs, _ := s.RunsSnapshot("")
	if len(runs.Runs) != 0 {
		t.Fatalf("cleanup left an empty phantom run: %+v", runs)
	}
}

func TestLegacyVersionsDoNotFabricateRunOrAttemptCapture(t *testing.T) {
	for _, version := range []int{1, 2} {
		s := New()
		input := correlation.ExtractRequest(nil, "/v1/responses", []byte(`{"metadata":{"run_id":"not-previously-observed"}}`), "https://upstream.test")
		state := diskState{Version: version, Privacy: s.Privacy.Snapshot(), Records: []diskRecord{{
			Log:   RequestLog{TraceID: "legacy", SessionID: "session", Status: "done", Route: &RouteInfo{Attempts: []RouteAttempt{{ProviderID: "old", URL: "https://old.test", StatusCode: 200, Duration: 12}}}},
			Input: input, PrivacyScope: "conversation:v2:kept", Capture: &RequestCapture{TraceID: "legacy", Outgoing: &RequestSnapshot{Body: "only-last-body"}},
		}}}
		payload, _ := json.Marshal(state)
		if err := s.restore(payload); err != nil {
			t.Fatal(err)
		}
		log, _ := s.Log("legacy")
		attempts, _ := s.AttemptsSnapshot("legacy")
		if log.RunID != "" || log.Run.State != "missing" || len(attempts) != 1 || attempts[0].Source != "legacy_summary" || attempts[0].Status != "unknown" || attempts[0].Duration != 12 || attempts[0].StartedAt != "" || attempts[0].TotalDuration != nil {
			t.Fatalf("v%d gained invented evidence: %+v %+v", version, log.Run, attempts)
		}
		detail, _ := s.AttemptSnapshot("legacy", attempts[0].ID)
		if detail.Request.Unavailable == "" || detail.Request.Body != "" {
			t.Fatalf("v%d used last outgoing as an independent capture: %+v", version, detail)
		}
		w := httptest.NewRecorder()
		s.AttemptsHandler(w, httptest.NewRequest("GET", "/api/attempts/legacy/"+attempts[0].ID+"/body", nil))
		if w.Code != 422 {
			t.Fatalf("legacy missing body download: %d", w.Code)
		}
		updated, err := s.snapshotJSON()
		if err != nil {
			t.Fatal(err)
		}
		reopened := New()
		if err = reopened.restore(updated); err != nil {
			t.Fatal(err)
		}
		restored, _ := reopened.AttemptsSnapshot("legacy")
		if restored[0].ID != attempts[0].ID || restored[0].Source != "legacy_summary" {
			t.Fatal("legacy summary identity changed on v3 reopen")
		}
	}
}

func TestAttemptCleanupPreservesSurvivingFileReference(t *testing.T) {
	s := New()
	if err := s.SetCaptureRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	owner := beginRun(s, "owner", "session", "run")
	s.CaptureOriginal(owner.TraceID, attemptRequest(`{}`, "upstream.test"))
	attempt, _ := s.BeginAttempt(owner.TraceID, RouteAttempt{}, attemptRequest(`{"shared":true}`, "upstream.test"))
	attempt.Status = "done"
	s.UpdateAttempt(owner.TraceID, attempt)
	owner.StatusCode = 200
	s.Complete(owner.TraceID, owner, correlation.Response{Complete: true})
	detail, _ := s.AttemptSnapshot(owner.TraceID, attempt.ID)
	survivor := beginRun(s, "survivor", "session", "run")
	s.CaptureOriginal(survivor.TraceID, attemptRequest(`{}`, "upstream.test"))
	// 兼容快照可以共享只读文件，清理所有者仍须检查其他存活引用。
	shared := detail.Request
	s.records[survivor.TraceID].capture.Outgoing = &shared
	if _, _, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == owner.TraceID }); err != nil {
		t.Fatal(err)
	}
	capture, _ := s.RequestSnapshot(survivor.TraceID)
	if capture.Outgoing.Body != `{"shared":true}` || capture.Outgoing.Unavailable != "" {
		t.Fatal("cleanup removed a survivor's referenced attempt file")
	}
	survivor.StatusCode = 200
	s.Complete(survivor.TraceID, survivor, correlation.Response{Complete: true})
	if _, _, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == survivor.TraceID }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(detail.Request.FilePath); !os.IsNotExist(err) {
		t.Fatal("unreferenced shared file was not cleaned")
	}
}

func TestAttemptAndRunPrivacyDoNotPersistDiscardedIdentifiers(t *testing.T) {
	s := New()
	if err := s.SetCaptureRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	p := recordingPolicy(t, s)
	p.RetainRaw, p.AllowReveal = false, false
	if err := s.Privacy.Set(p); err != nil {
		t.Fatal(err)
	}
	const secret = "private-task@example.com"
	header := make(http.Header)
	header.Set("X-Run-ID", secret)
	body := []byte(`{"metadata":{"run_id":"` + secret + `"},"input":"hello"}`)
	input := correlation.ExtractRequest(header, "/v1/responses", body, "https://upstream.test")
	log := s.BeginWithBody(RequestLog{TraceID: "private", Path: "/v1/responses", Method: "POST"}, input, body, 4096)
	snapshot := attemptRequest(string(body), "upstream.test")
	snapshot.Headers["X-Run-ID"] = secret
	snapshot.Forwarding.Headers.Set("X-Run-ID", secret)
	s.CaptureOriginal(log.TraceID, snapshot)
	attempt, _ := s.BeginAttempt(log.TraceID, RouteAttempt{}, snapshot)
	attempt.Status, attempt.Error = "error", "failed for "+secret
	s.UpdateAttempt(log.TraceID, attempt)
	detail, _ := s.AttemptSnapshot(log.TraceID, attempt.ID)
	runs, _ := s.RunsSnapshot("")
	payload, err := s.snapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	visible, _ := json.Marshal(struct {
		Detail AttemptDetail
		Runs   Runs
	}{detail, runs})
	if bytes.Contains(payload, []byte(secret)) || bytes.Contains(visible, []byte(secret)) || !strings.Contains(detail.Request.Body, "[PRIVATE_email_") {
		t.Fatal("discarded private task identifier leaked through new metadata")
	}
	for _, path := range []string{detail.Request.FilePath, s.records[log.TraceID].capture.Original.FilePath} {
		stored, err := os.ReadFile(path)
		if err != nil || bytes.Contains(stored, []byte(secret)) {
			t.Fatalf("raw task identifier persisted in request file: %v", err)
		}
	}
	w := httptest.NewRecorder()
	s.AttemptsHandler(w, httptest.NewRequest("GET", "/api/attempts/private/"+attempt.ID+"/body", nil))
	if w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte(secret)) || !strings.Contains(w.Body.String(), "[PRIVATE_email_") {
		t.Fatal("body download bypassed request privacy")
	}
}
