package proxy

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/config"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/hub"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/replay"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"go.uber.org/zap"
)

const persistentReplayBody = `{"model":"mock","input":"durable replay","large":9007199254740993}`

func newReplayPersistenceProxy(t *testing.T, root, target string) (*Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(root, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New(zap.NewNop())
	proxy, err := NewServer(&config.Config{TargetAddr: target, MaxBodyLogSize: 64, LogDir: root, ListenAddr: "127.0.0.1:12337"}, zap.NewNop(), h, s)
	if err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		proxy.Close()
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return proxy, s
}

func readReplayState(path string) ([]replay.Saved, []byte, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()
	var payload []byte
	if err := db.QueryRow("SELECT payload FROM gateway_state WHERE id=1").Scan(&payload); err != nil {
		return nil, nil, err
	}
	var state struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		return nil, nil, err
	}
	var saved []replay.Saved
	err = json.Unmarshal(state.Settings["replays"], &saved)
	return saved, payload, err
}

func TestReplayRegistrationIsDurableBeforeUpstreamAndKeepsSecretsTransient(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "gateway.db")
	var hits atomic.Int32
	checked := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := hits.Add(1)
		if count == 2 {
			// Inspect the real database at the first replay upstream boundary,
			// before its execution can finish or shutdown can flush anything.
			saved, raw, err := readReplayState(path)
			if err == nil && (len(saved) != 1 || saved[0].Record.IdempotencyKey != "durable-on-dispatch" || saved[0].Fingerprint == "" || saved[0].Record.State != "running") {
				err = fmt.Errorf("upstream received an unregistered replay: %+v", saved)
			}
			for _, secret := range []string{"original-auth-secret", "original-query-secret", "transient-auth-secret", "transient-query-secret"} {
				if strings.Contains(string(raw), secret) {
					err = fmt.Errorf("transient credential was written to metadata")
				}
			}
			if r.Header.Get("Authorization") != "Bearer transient-auth-secret" || r.URL.Query().Get("key") != "transient-query-secret" {
				err = fmt.Errorf("replay did not use its transient credentials")
			}
			checked <- err
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp-%d","output_text":"done"}`, count)
	}))
	t.Cleanup(upstream.Close)
	proxy, storage := newReplayPersistenceProxy(t, root, upstream.URL)
	source, response := send(t, proxy, http.MethodPost, "/v1/responses?key=original-query-secret", persistentReplayBody, map[string]string{"Authorization": "Bearer original-auth-secret", "Content-Type": "application/json"})
	if response.Code != http.StatusOK {
		t.Fatalf("source request failed: %d %s", response.Code, response.Body.String())
	}
	if err := storage.Flush(); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"trace_id": source, "source": "outgoing", "idempotency_key": "durable-on-dispatch",
		"credentials": []map[string]string{
			{"kind": "header", "name": "Authorization", "value": "Bearer transient-auth-secret"},
			{"kind": "query", "name": "key", "value": "transient-query-secret"},
		},
	}
	record := createReplay(t, proxy, request, http.StatusAccepted)
	finishedReplay(t, proxy, record.ID)
	select {
	case err := <-checked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replay did not reach the local upstream")
	}
	duplicate := createReplay(t, proxy, request, http.StatusOK)
	if duplicate.ID != record.ID || hits.Load() != 2 {
		t.Fatalf("duplicate action sent another request: %+v hits=%d", duplicate, hits.Load())
	}
}

func TestReplayRegistrationSaveFailureReturns503WithoutExecutingAndCanRetry(t *testing.T) {
	root := t.TempDir()
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp-%d","output_text":"done"}`, count)
	}))
	t.Cleanup(upstream.Close)
	proxy, storage := newReplayPersistenceProxy(t, root, upstream.URL)
	source, _ := send(t, proxy, http.MethodPost, "/v1/responses", persistentReplayBody, map[string]string{"Content-Type": "application/json"})
	prior := createReplay(t, proxy, map[string]any{"trace_id": source, "idempotency_key": "previous-key"}, http.StatusAccepted)
	finishedReplay(t, proxy, prior.ID)
	if err := storage.Flush(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TRIGGER reject_replay_registration BEFORE UPDATE ON gateway_state BEGIN SELECT RAISE(ABORT, 'forced replay save failure'); END"); err != nil {
		t.Fatal(err)
	}
	var recovered sync.Once
	recoverStorage := func() {
		recovered.Do(func() {
			if _, err := db.Exec("DROP TRIGGER reject_replay_registration"); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(recoverStorage)
	request := map[string]any{"trace_id": source, "idempotency_key": "failed-key"}
	status, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", request)
	if status != http.StatusServiceUnavailable || !strings.Contains(string(raw), "no upstream request was sent") {
		t.Fatalf("registration failure was not explicit: %d %s", status, raw)
	}
	var saved []replay.Saved
	if hits.Load() != 2 || len(proxy.replays.List()) != 1 || !storage.Setting("replays", &saved) || len(saved) != 1 || saved[0].Record.ID != prior.ID {
		t.Fatalf("rejected replay executed or changed registered keys: hits=%d replays=%+v saved=%+v", hits.Load(), proxy.replays.List(), saved)
	}
	logs := 0
	for _, session := range storage.SessionsSnapshot() {
		logs += len(session.Logs)
	}
	if logs != 2 || storage.PersistenceStatus()["error"] == nil {
		t.Fatalf("rejected replay left a trace or hid its persistence error: logs=%d status=%+v", logs, storage.PersistenceStatus())
	}
	recoverStorage()
	if err := storage.Flush(); err != nil {
		t.Fatal(err)
	}
	saved, _, err = readReplayState(filepath.Join(root, "gateway.db"))
	if err != nil || len(saved) != 1 || saved[0].Record.IdempotencyKey != "previous-key" {
		t.Fatalf("later flush persisted the failed registration: %+v %v", saved, err)
	}
	retried := createReplay(t, proxy, request, http.StatusAccepted)
	finishedReplay(t, proxy, retried.ID)
	if hits.Load() != 3 {
		t.Fatalf("recovered storage did not execute exactly one retry: %d", hits.Load())
	}
	proxy.Close()
	status, raw = api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{"trace_id": source, "idempotency_key": "after-shutdown"})
	if status != http.StatusServiceUnavailable || !strings.Contains(string(raw), "shutting down") || hits.Load() != 3 {
		t.Fatalf("shutdown accepted another execution: %d %s hits=%d", status, raw, hits.Load())
	}
	if duplicate := createReplay(t, proxy, request, http.StatusOK); duplicate.ID != retried.ID || hits.Load() != 3 {
		t.Fatal("shutdown forgot a previously registered key")
	}
}

func TestReplayRegistrationSurvivesAbruptExit(t *testing.T) {
	if os.Getenv("GOPROXY_REPLAY_CRASH_HELPER") == "1" {
		root, target := os.Getenv("GOPROXY_REPLAY_CRASH_ROOT"), os.Getenv("GOPROXY_REPLAY_CRASH_TARGET")
		proxy, storage := newReplayPersistenceProxy(t, root, target)
		source, response := send(t, proxy, http.MethodPost, "/v1/responses", persistentReplayBody, map[string]string{"Authorization": "Bearer crash-source-secret", "Content-Type": "application/json"})
		if response.Code != http.StatusOK {
			t.Fatalf("crash helper source failed: %d", response.Code)
		}
		proxy.replays.Shutdown()
		proxy.replays = replay.NewWithPersistence(nil, func(saved []replay.Saved, durable bool) error {
			if !durable {
				storage.SetSetting("replays", saved)
				return nil
			}
			if err := storage.SetSettingDurable("replays", saved); err != nil {
				return err
			}
			// Terminate precisely after the registration commit, before dispatch
			// or any completion/Close flush. Leave the live SQLite WAL intact.
			os.Exit(0)
			return nil
		})
		status, raw := api(t, proxy.ReplaysHandler, http.MethodPost, "/api/replays", map[string]any{
			"trace_id": source, "source": "outgoing", "idempotency_key": "crash-key",
			"credentials": []map[string]string{{"kind": "header", "name": "Authorization", "value": "Bearer crash-replay-secret"}},
		})
		t.Fatalf("crash helper returned instead of exiting: %d %s", status, raw)
	}

	root := t.TempDir()
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count := hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp-%d","output_text":"done"}`, count)
	}))
	t.Cleanup(upstream.Close)
	cmd := exec.Command(os.Args[0], "-test.run=^TestReplayRegistrationSurvivesAbruptExit$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "GOPROXY_REPLAY_CRASH_HELPER=1", "GOPROXY_REPLAY_CRASH_ROOT="+root, "GOPROXY_REPLAY_CRASH_TARGET="+upstream.URL)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("abrupt-exit helper failed: %v\n%s", err, output)
	}
	if hits.Load() != 1 {
		t.Fatalf("helper should exit before replay dispatch: hits=%d", hits.Load())
	}
	saved, raw, err := readReplayState(filepath.Join(root, "gateway.db"))
	if err != nil || len(saved) != 1 || saved[0].Record.IdempotencyKey != "crash-key" || saved[0].Record.State != "running" {
		t.Fatalf("abrupt exit lost the committed registration: %+v %v", saved, err)
	}
	if strings.Contains(string(raw), "crash-source-secret") || strings.Contains(string(raw), "crash-replay-secret") {
		t.Fatal("crash recovery metadata retained a transient credential")
	}
	proxy, storage := newReplayPersistenceProxy(t, root, upstream.URL)
	if _, exists := storage.Log(saved[0].Record.TraceID); exists {
		t.Fatal("restart invented a trace for the never-dispatched replay")
	}
	record := createReplay(t, proxy, map[string]any{
		"trace_id": saved[0].Record.Of, "source": "outgoing", "idempotency_key": "crash-key",
		"credentials": []map[string]string{{"kind": "header", "name": "Authorization", "value": "Bearer crash-replay-secret"}},
	}, http.StatusOK)
	if record.ID != saved[0].Record.ID || record.State != "error" || record.Reason != "gateway_restarted" || record.StatusCode != http.StatusServiceUnavailable || hits.Load() != 1 {
		t.Fatalf("restart resumed or forgot an interrupted replay: %+v hits=%d", record, hits.Load())
	}
}
