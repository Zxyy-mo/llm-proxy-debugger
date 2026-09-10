// Package replay owns operator-initiated request executions. Each explicit
// action creates one record; retries that reuse an idempotency key return the
// existing record instead of sending another upstream request.
package replay

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("replay not found")
	// ErrKeyReused means the idempotency key was already used for different content.
	ErrKeyReused = errors.New("idempotency key was already used for a different replay")
	ErrFinished  = errors.New("replay already finished")
)

type Record struct {
	ID             string `json:"id"`
	TraceID        string `json:"trace_id"`
	Of             string `json:"replay_of"`
	Source         string `json:"source"`
	Modified       bool   `json:"modified"`
	State          string `json:"state"`            // running, done, error or canceled
	Reason         string `json:"reason,omitempty"` // manual or timeout for canceled/error replays
	Error          string `json:"error,omitempty"`
	StatusCode     int    `json:"status_code,omitempty"`
	CreatedAt      string `json:"created_at"`
	FinishedAt     string `json:"finished_at,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Outcome is what a completed execution reports back from the request log.
type Outcome struct {
	State      string
	Error      string
	StatusCode int
}

type entry struct {
	record      Record
	fingerprint string
	cancel      context.CancelFunc
	canceled    bool
	done        chan struct{}
}

type Manager struct {
	mu      sync.Mutex
	entries map[string]*entry
	keys    map[string]string
	changed func()
}

func New(changed func()) *Manager {
	return &Manager{entries: make(map[string]*entry), keys: make(map[string]string), changed: changed}
}

func (m *Manager) notify() {
	if m.changed != nil {
		m.changed()
	}
}

// Start registers a replay and runs it on a server-owned context that outlives
// the creating HTTP request. It returns the existing record, with created=false,
// when the key was already used for identical content.
func (m *Manager) Start(key, fingerprint string, record Record, timeout time.Duration, run func(context.Context) Outcome) (Record, bool, error) {
	m.mu.Lock()
	if id, exists := m.keys[key]; exists {
		existing := m.entries[id]
		m.mu.Unlock()
		if existing.fingerprint != fingerprint {
			return Record{}, false, ErrKeyReused
		}
		return existing.record, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	record.State = "running"
	record.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	record.TimeoutSeconds = int(timeout / time.Second)
	record.IdempotencyKey = key
	e := &entry{record: record, fingerprint: fingerprint, cancel: cancel, done: make(chan struct{})}
	m.entries[record.ID] = e
	m.keys[key] = record.ID
	m.mu.Unlock()
	m.notify()
	go func() {
		defer cancel()
		outcome := run(ctx)
		m.mu.Lock()
		e.record.State, e.record.Error, e.record.StatusCode = outcome.State, outcome.Error, outcome.StatusCode
		if e.record.State == "" {
			e.record.State = "error"
		}
		switch {
		case e.canceled:
			e.record.Reason = "manual"
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			e.record.Reason = "timeout"
		}
		e.record.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		close(e.done)
		m.mu.Unlock()
		m.notify()
	}()
	return e.record, true, nil
}

func (m *Manager) Get(id string) (Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return Record{}, ErrNotFound
	}
	return e.record, nil
}

func (m *Manager) List() []Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	records := make([]Record, 0, len(m.entries))
	for _, e := range m.entries {
		records = append(records, e.record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].CreatedAt < records[j].CreatedAt })
	return records
}

// Cancel stops a running replay. The record reaches its terminal state once the
// execution observes the cancellation and reports its outcome.
func (m *Manager) Cancel(id string) (Record, error) {
	m.mu.Lock()
	e := m.entries[id]
	if e == nil {
		m.mu.Unlock()
		return Record{}, ErrNotFound
	}
	if e.record.State != "running" {
		record := e.record
		m.mu.Unlock()
		return record, ErrFinished
	}
	e.canceled = true
	cancel, done := e.cancel, e.done
	m.mu.Unlock()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return m.Get(id)
}

// Wait blocks until the replay finishes or the context ends; tests use it.
func (m *Manager) Wait(ctx context.Context, id string) (Record, error) {
	m.mu.Lock()
	e := m.entries[id]
	m.mu.Unlock()
	if e == nil {
		return Record{}, ErrNotFound
	}
	select {
	case <-e.done:
	case <-ctx.Done():
	}
	return m.Get(id)
}
