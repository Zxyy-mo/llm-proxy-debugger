package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/observation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/privacy"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/protocol"
	_ "modernc.org/sqlite"
)

type persistence struct {
	db       *sql.DB
	path     string
	notify   chan struct{}
	stop     chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	once     sync.Once
	lastSave time.Time
	err      error
	closed   bool
}

type diskRecord struct {
	Log                RequestLog               `json:"log"`
	Input              correlation.Request      `json:"input"`
	Sequence           uint64                   `json:"sequence"`
	Fixed              bool                     `json:"fixed"`
	Preferred          string                   `json:"preferred"`
	Capture            *RequestCapture          `json:"capture,omitempty"`
	OriginalForwarding *Forwarding              `json:"original_forwarding,omitempty"`
	OutgoingForwarding *Forwarding              `json:"outgoing_forwarding,omitempty"`
	OriginalPath       string                   `json:"original_path,omitempty"`
	OutgoingPath       string                   `json:"outgoing_path,omitempty"`
	Response           *ResponseSnapshot        `json:"response,omitempty"`
	ResponsePath       string                   `json:"response_path,omitempty"`
	UpstreamResponse   *ResponseSnapshot        `json:"upstream_response,omitempty"`
	UpstreamPath       string                   `json:"upstream_path,omitempty"`
	Privacy            privacy.Policy           `json:"privacy"`
	PrivacyScope       string                   `json:"privacy_scope"`
	ToolResults        []observation.ToolResult `json:"tool_results,omitempty"`
}
type diskState struct {
	Version  int                        `json:"version"`
	Revision uint64                     `json:"revision"`
	Sequence uint64                     `json:"sequence"`
	Records  []diskRecord               `json:"records"`
	Rules    []Rule                     `json:"rules"`
	Sessions map[string]*Session        `json:"sessions"`
	Aliases  map[string]string          `json:"aliases"`
	Owners   map[string]map[string]bool `json:"owners"`
	Waiters  map[string]map[string]bool `json:"waiters"`
	Children map[string]map[string]bool `json:"children"`
	History  map[string]map[string]bool `json:"history"`
	Privacy  privacy.State              `json:"privacy"`
	Settings map[string]json.RawMessage `json:"settings"`
}

// Open restores metadata and indices. Full request/response bodies live in
// separate files so neither SQLite snapshots nor list APIs duplicate them.
func Open(path string) (*Store, error) {
	s := New()
	if path == "" || path == "-" {
		return s, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s.captureRoot = filepath.Dir(abs)
	if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", abs)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000", "PRAGMA secure_delete=ON", "CREATE TABLE IF NOT EXISTS gateway_state (id INTEGER PRIMARY KEY CHECK(id=1), payload BLOB NOT NULL)"} {
		if _, err = db.Exec(statement); err != nil {
			db.Close()
			return nil, err
		}
	}
	var payload []byte
	err = db.QueryRow("SELECT payload FROM gateway_state WHERE id=1").Scan(&payload)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		db.Close()
		return nil, err
	}
	if len(payload) > 0 {
		if err = s.restore(payload); err != nil {
			db.Close()
			return nil, fmt.Errorf("restore gateway state: %w", err)
		}
	}
	p := &persistence{db: db, path: abs, notify: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	s.persistence = p
	go func() {
		defer close(p.done)
		for {
			select {
			case <-p.stop:
				return
			case <-p.notify:
				timer := time.NewTimer(200 * time.Millisecond)
				select {
				case <-p.stop:
					timer.Stop()
					return
				case <-timer.C:
				}
				_ = s.Flush()
			}
		}
	}()
	s.changed()
	return s, nil
}

// changed is nonblocking and may be called while holding the store lock.
func (s *Store) changed() {
	if p := s.persistence; p != nil {
		select {
		case p.notify <- struct{}{}:
		default:
		}
	}
}

func (s *Store) snapshotJSON() ([]byte, error) {
	s.RLock()
	defer s.RUnlock()
	return s.snapshotJSONLocked()
}

// snapshotJSONLocked requires the caller to hold the store lock for reading
// or writing until all referenced records and indices have been encoded.
func (s *Store) snapshotJSONLocked() ([]byte, error) {
	state := diskState{Version: 2, Revision: s.revision, Sequence: s.sequence, Rules: s.Rules, Sessions: s.sessions, Aliases: s.aliases, Owners: s.owners, Waiters: s.waiters, Children: s.children, History: s.history, Privacy: s.Privacy.Snapshot(), Settings: s.settings}
	for _, rec := range s.orderedRecords() {
		item := diskRecord{Log: rec.log, Input: rec.input, Sequence: rec.sequence, Fixed: rec.fixedSession, Preferred: rec.preferredSession, Capture: rec.capture, Response: rec.response, Privacy: rec.privacy, PrivacyScope: rec.privacyScope}
		item.ToolResults = rec.toolResults
		if rec.capture != nil {
			item.OriginalForwarding = rec.capture.Original.Forwarding
			item.OriginalPath = rec.capture.Original.FilePath
			if rec.capture.Outgoing != nil {
				item.OutgoingForwarding = rec.capture.Outgoing.Forwarding
				item.OutgoingPath = rec.capture.Outgoing.FilePath
			}
		}
		if rec.response != nil {
			item.ResponsePath = rec.response.Path
		}
		if rec.upstreamResponse != nil {
			item.UpstreamResponse, item.UpstreamPath = rec.upstreamResponse, rec.upstreamResponse.Path
		}
		state.Records = append(state.Records, item)
	}
	return json.Marshal(state)
}

func (s *Store) restore(payload []byte) error {
	var state diskState
	if err := json.Unmarshal(payload, &state); err != nil {
		return err
	}
	if state.Version != 1 && state.Version != 2 {
		return fmt.Errorf("unsupported state version %d", state.Version)
	}
	s.revision, s.sequence = state.Revision, state.Sequence
	s.Rules = state.Rules
	if state.Sessions != nil {
		s.sessions = state.Sessions
	}
	if state.Aliases != nil {
		s.aliases = state.Aliases
	}
	if state.Owners != nil {
		s.owners = state.Owners
	}
	if state.Waiters != nil {
		s.waiters = state.Waiters
	}
	if state.Children != nil {
		s.children = state.Children
	}
	if state.History != nil {
		s.history = state.History
	}
	if state.Settings != nil {
		s.settings = state.Settings
	}
	if err := s.Privacy.RestoreState(state.Privacy); err != nil {
		return err
	}
	for _, item := range state.Records {
		scope := item.PrivacyScope
		if state.Version == 1 {
			// Legacy placeholders were generated from the credential scope. Keep
			// their dictionary readable without giving it to new captures.
			scope = item.Input.Scope
		} else if scope == "" {
			return fmt.Errorf("record %q has no privacy namespace", item.Log.TraceID)
		}
		rec := &record{log: item.Log, input: item.Input, sequence: item.Sequence, fixedSession: item.Fixed, preferredSession: item.Preferred, capture: item.Capture, response: item.Response, privacy: item.Privacy, privacyScope: scope}
		rec.toolResults = item.ToolResults
		if rec.capture != nil {
			rec.capture.Original.Forwarding = item.OriginalForwarding
			rec.capture.Original.FilePath = item.OriginalPath
			if rec.capture.Outgoing != nil {
				rec.capture.Outgoing.Forwarding = item.OutgoingForwarding
				rec.capture.Outgoing.FilePath = item.OutgoingPath
			}
		}
		if rec.response != nil {
			rec.response.Path = item.ResponsePath
			rec.response.Receiving = false
		}
		if item.UpstreamResponse != nil {
			rec.upstreamResponse = item.UpstreamResponse
			rec.upstreamResponse.Path = item.UpstreamPath
			rec.upstreamResponse.Receiving = false
		}
		if rec.log.Status == "running" || rec.log.Status == "pending" {
			rec.log.Status, rec.log.StatusCode, rec.log.Error = "error", http.StatusServiceUnavailable, "网关重启，请求已中断；不会自动补发"
			rec.log.TTFB, rec.log.TTFC = nil, nil
			rec.log.TokenSources = protocol.UnknownSources()
			if rec.log.Interception != nil {
				rec.log.Interception.State = "canceled"
				rec.log.Interception.Reason = "gateway_restarted"
			}
			if rec.response != nil {
				rec.response.Complete = false
				rec.response.Reason = "网关重启，仅保留已捕获部分"
			}
			s.touch(rec)
		}
		s.records[rec.log.TraceID] = rec
	}
	return nil
}

func (s *Store) Flush() error {
	p := s.persistence
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.saveLocked(nil, nil)
	}
	payload, err := s.snapshotJSON()
	return p.saveLocked(payload, err)
}

func (s *Store) Close() error {
	p := s.persistence
	if p == nil {
		return nil
	}
	var result error
	p.once.Do(func() {
		close(p.stop)
		<-p.done
		result = s.Flush()
		p.mu.Lock()
		defer p.mu.Unlock()
		_, _ = p.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		if err := p.db.Close(); result == nil {
			result = err
		}
		p.closed = true
	})
	return result
}

func (s *Store) SetSetting(key string, value any) {
	raw, err := json.Marshal(value)
	if err != nil {
		return
	}
	s.Lock()
	s.settings[key] = raw
	s.Unlock()
	s.changed()
}
func (s *Store) Setting(key string, value any) bool {
	s.RLock()
	raw := append([]byte(nil), s.settings[key]...)
	s.RUnlock()
	return len(raw) > 0 && json.Unmarshal(raw, value) == nil
}

func (s *Store) PersistenceStatus() map[string]any {
	status := map[string]any{"enabled": false, "flush_interval_ms": 200}
	if p := s.persistence; p != nil {
		p.mu.Lock()
		defer p.mu.Unlock()
		status["enabled"] = true
		status["last_saved_at"] = p.lastSave
		if p.err != nil {
			status["error"] = p.err.Error()
		}
	}
	return status
}
