package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
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
	Log                RequestLog                    `json:"log"`
	Input              correlation.Request           `json:"input"`
	Sequence           uint64                        `json:"sequence"`
	Fixed              bool                          `json:"fixed"`
	Preferred          string                        `json:"preferred"`
	Capture            *RequestCapture               `json:"capture,omitempty"`
	OriginalForwarding *Forwarding                   `json:"original_forwarding,omitempty"`
	OutgoingForwarding *Forwarding                   `json:"outgoing_forwarding,omitempty"`
	OriginalPath       string                        `json:"original_path,omitempty"`
	OutgoingPath       string                        `json:"outgoing_path,omitempty"`
	Response           *ResponseSnapshot             `json:"response,omitempty"`
	ResponsePath       string                        `json:"response_path,omitempty"`
	UpstreamResponse   *ResponseSnapshot             `json:"upstream_response,omitempty"`
	UpstreamPath       string                        `json:"upstream_path,omitempty"`
	Privacy            privacy.Policy                `json:"privacy"`
	PrivacyScope       string                        `json:"privacy_scope"`
	ToolResults        []observation.ToolResult      `json:"tool_results,omitempty"`
	AttemptCaptures    map[string]diskAttemptCapture `json:"attempt_captures,omitempty"`
}

// diskAttemptCapture 显式保存 JSON API 隐藏的转发资料和路径，恢复后仍能读取每次独立出站正文。
type diskAttemptCapture struct {
	Snapshot   RequestSnapshot `json:"snapshot"`
	Forwarding *Forwarding     `json:"forwarding,omitempty"`
	Path       string          `json:"path,omitempty"`
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

// Open 恢复元数据与索引；请求和响应正文独立存放，SQLite 快照不重复加载完整文件。
// 恢复不会执行请求或尝试，活动记录会保守标记为中断。
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
	if err = s.SetCaptureRoot(filepath.Dir(abs)); err != nil {
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

// snapshotJSONLocked 写出 v3 元数据及逐次尝试的私有文件资料。
// 调用方必须持续持有 Store 读锁或写锁，直到所有引用的数据编码完毕。
func (s *Store) snapshotJSONLocked() ([]byte, error) {
	state := diskState{Version: 3, Revision: s.revision, Sequence: s.sequence, Rules: s.Rules, Sessions: s.sessions, Aliases: s.aliases, Owners: s.owners, Waiters: s.waiters, Children: s.children, History: s.history, Privacy: s.Privacy.Snapshot(), Settings: s.settings}
	for _, rec := range s.orderedRecords() {
		item := diskRecord{Log: rec.log, Input: rec.input, Sequence: rec.sequence, Fixed: rec.fixedSession, Preferred: rec.preferredSession, Capture: rec.capture, Response: rec.response, Privacy: rec.privacy, PrivacyScope: rec.privacyScope}
		item.ToolResults = rec.toolResults
		if len(rec.attemptCaptures) > 0 {
			item.AttemptCaptures = make(map[string]diskAttemptCapture, len(rec.attemptCaptures))
			for id, snapshot := range rec.attemptCaptures {
				item.AttemptCaptures[id] = diskAttemptCapture{Snapshot: snapshot, Forwarding: snapshot.Forwarding, Path: snapshot.FilePath}
			}
		}
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

// restore 读取 v1/v2/v3，保留旧隐私命名空间；没有证据的任务和尝试资料保持未知。
// 重启只恢复可检查的历史，绝不补发请求，也不把停机时长当成上游耗时。
func (s *Store) restore(payload []byte) error {
	var state diskState
	if err := json.Unmarshal(payload, &state); err != nil {
		return err
	}
	if state.Version != 1 && state.Version != 2 && state.Version != 3 {
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
		if state.Version < 3 {
			// 旧版没有承诺任务边界，不能重新解析正文或会话替历史补造 Run。
			rec.log.RunID = ""
			rec.log.Run = &RunAssociation{State: "missing", Sources: []string{}, Warning: "legacy_run_unavailable"}
			rec.input.Run = correlation.RunEvidence{}
		} else if rec.log.Run == nil {
			rec.log.Run = &RunAssociation{State: "missing", Sources: []string{}}
		}
		if len(item.AttemptCaptures) > 0 {
			rec.attemptCaptures = make(map[string]RequestSnapshot, len(item.AttemptCaptures))
			for id, saved := range item.AttemptCaptures {
				snapshot := saved.Snapshot
				snapshot.Forwarding, snapshot.FilePath = saved.Forwarding, saved.Path
				rec.attemptCaptures[id] = snapshot
			}
		}
		if rec.log.Route != nil {
			for index := range rec.log.Route.Attempts {
				attempt := &rec.log.Route.Attempts[index]
				if state.Version < 3 || attempt.Source == "" {
					// 摘要只有头部耗时；派生身份只用于稳定查看，不代表重新获得了发送证据。
					attempt.ID = "legacy:" + correlation.Hash(rec.log.TraceID, strconv.Itoa(index))[:24]
					attempt.TraceID, attempt.Sequence = rec.log.TraceID, index+1
					attempt.Source, attempt.Status = "legacy_summary", "unknown"
					attempt.StartedAt, attempt.HeadersAt, attempt.EndedAt, attempt.TotalDuration = "", "", "", nil
				} else if attempt.Status == "running" {
					attempt.Status, attempt.Error = "interrupted", "网关重启，尝试结果未知；不会自动补发"
					// 重启时间不是上游结束时间，不能把停机时长累加到真实尝试耗时。
					attempt.EndedAt, attempt.TotalDuration = "", nil
				}
			}
		}
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
