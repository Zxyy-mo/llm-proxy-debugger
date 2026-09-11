// Package replay owns operator-initiated request executions. Each explicit
// action creates one record; retries that reuse an idempotency key return the
// existing record instead of sending another upstream request.
package replay

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("replay not found")
	// ErrKeyReused means the idempotency key was already used for different content.
	ErrKeyReused   = errors.New("idempotency key was already used for a different replay")
	ErrFinished    = errors.New("replay already finished")
	ErrClosed      = errors.New("replay service is shutting down; no new execution was started")
	ErrPersistence = errors.New("replay registration could not be saved; no upstream request was sent")
)

type Record struct {
	ID             string `json:"id"`
	TraceID        string `json:"trace_id"`
	Of             string `json:"replay_of"`
	Source         string `json:"source"`
	Modified       bool   `json:"modified"`
	State          string `json:"state"`            // running, done, error or canceled
	Reason         string `json:"reason,omitempty"` // manual, timeout, shutdown or gateway_restarted
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
	mu        sync.Mutex
	entries   map[string]*entry
	keys      map[string]string
	changed   func()
	persist   Persist
	lifecycle context.Context
	stop      context.CancelFunc
}

// Persist receives an immutable snapshot in manager mutation order. A durable
// save must commit before returning success. It runs under the manager lock
// and must not call back into the manager. Non-durable changes may be coalesced.
type Persist func(saved []Saved, durable bool) error

func New(changed func()) *Manager {
	return NewWithPersistence(changed, nil)
}

func NewWithPersistence(changed func(), persist Persist) *Manager {
	lifecycle, stop := context.WithCancel(context.Background())
	return &Manager{entries: make(map[string]*entry), keys: make(map[string]string), changed: changed, persist: persist, lifecycle: lifecycle, stop: stop}
}

func (m *Manager) notify() {
	if m.changed != nil {
		m.changed()
	}
}

// Start registers a replay and runs it on a server-owned context that outlives
// the creating HTTP request. It returns the existing record, with created=false,
// when the key was already used for identical content. New registrations must
// be durably saved before execution; a failed save leaves the key retryable.
func (m *Manager) Start(key, fingerprint string, record Record, timeout time.Duration, run func(context.Context) Outcome) (Record, bool, error) {
	m.mu.Lock()
	if id, exists := m.keys[key]; exists {
		existing := m.entries[id]
		current, previousFingerprint := existing.record, existing.fingerprint
		m.mu.Unlock()
		if previousFingerprint != fingerprint {
			return Record{}, false, ErrKeyReused
		}
		return current, false, nil
	}
	if m.lifecycle.Err() != nil {
		m.mu.Unlock()
		return Record{}, false, ErrClosed
	}
	record.State = "running"
	record.CreatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	record.TimeoutSeconds = int(timeout / time.Second)
	record.IdempotencyKey = key
	e := &entry{record: record, fingerprint: fingerprint, done: make(chan struct{})}
	m.entries[record.ID] = e
	m.keys[key] = record.ID
	if err := m.saveLocked(true); err != nil {
		delete(m.entries, record.ID)
		delete(m.keys, key)
		m.mu.Unlock()
		return Record{}, false, fmt.Errorf("%w: %v", ErrPersistence, err)
	}
	// The execution timeout excludes registration I/O. Shutdown cancels the
	// parent immediately, including while a registration is waiting on storage.
	ctx, cancel := context.WithTimeout(m.lifecycle, timeout)
	e.cancel = cancel
	if m.lifecycle.Err() != nil {
		m.finishLocked(e, ctx, Outcome{State: "canceled", Error: ErrClosed.Error(), StatusCode: 499})
		record = e.record
		cancel()
		m.mu.Unlock()
		m.notify()
		return record, false, ErrClosed
	}
	go m.execute(ctx, cancel, e, run)
	m.mu.Unlock()
	m.notify()
	return record, true, nil
}

func (m *Manager) execute(ctx context.Context, cancel context.CancelFunc, e *entry, run func(context.Context) Outcome) {
	defer cancel()
	var outcome Outcome
	if err := ctx.Err(); err != nil {
		outcome = Outcome{State: "canceled", Error: err.Error(), StatusCode: 499}
		if errors.Is(err, context.DeadlineExceeded) {
			outcome.State, outcome.StatusCode = "error", 504
		}
	} else {
		outcome = run(ctx)
	}
	m.mu.Lock()
	m.finishLocked(e, ctx, outcome)
	m.mu.Unlock()
	m.notify()
}

func (m *Manager) finishLocked(e *entry, ctx context.Context, outcome Outcome) {
	e.record.State, e.record.Error, e.record.StatusCode = outcome.State, outcome.Error, outcome.StatusCode
	if e.record.State == "" {
		e.record.State = "error"
	}
	switch {
	case e.canceled:
		e.record.Reason = "manual"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		e.record.Reason = "timeout"
	case m.lifecycle.Err() != nil && e.record.State == "canceled":
		e.record.Reason = "shutdown"
		e.record.Error = "网关关闭，重放已中断；不会自动补发"
	}
	e.record.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	// A completion save cannot roll back the upstream side effect. The durable
	// registration still suppresses retries if this coalesced update is lost.
	_ = m.saveLocked(false)
	close(e.done)
}

func (m *Manager) saveLocked(durable bool) error {
	if m.persist == nil {
		return nil
	}
	return m.persist(m.snapshotLocked(), durable)
}

type Saved struct {
	Record      Record `json:"record"`
	Fingerprint string `json:"fingerprint"`
}

func (m *Manager) Snapshot() []Saved {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

func (m *Manager) snapshotLocked() []Saved {
	saved := make([]Saved, 0, len(m.entries))
	for _, e := range m.entries {
		saved = append(saved, Saved{e.record, e.fingerprint})
	}
	return saved
}

func (m *Manager) Restore(saved []Saved) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range saved {
		record := item.Record
		if record.State == "running" {
			record.State = "error"
			record.Reason = "gateway_restarted"
			record.Error = "网关重启，重放已中断；不会自动补发"
			record.StatusCode = 503
			record.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		}
		done := make(chan struct{})
		close(done)
		m.entries[record.ID] = &entry{record: record, fingerprint: item.Fingerprint, done: done}
		m.keys[record.IdempotencyKey] = record.ID
	}
	_ = m.saveLocked(false)
}

func (m *Manager) Shutdown() {
	// Cancel without waiting on mu: a registration can be in synchronous
	// storage I/O, and must not dispatch once that save eventually returns.
	m.stop()
	m.mu.Lock()
	var cancels []context.CancelFunc
	var done []chan struct{}
	for _, e := range m.entries {
		if e.record.State == "running" && e.cancel != nil {
			cancels = append(cancels, e.cancel)
			done = append(done, e.done)
		}
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for _, finished := range done {
		select {
		case <-finished:
		case <-timeout.C:
			return
		}
	}
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
