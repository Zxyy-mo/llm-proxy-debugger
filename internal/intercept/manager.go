// Package intercept owns pending HTTP requests independently of browser lifetime.
package intercept

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotFound = errors.New("interception not found")
	ErrConflict = errors.New("interception changed or is no longer pending")
)

type ValidationError struct{ Err error }

func (e *ValidationError) Error() string { return e.Err.Error() }

type Metadata struct {
	RuleID        string  `json:"rule_id"`
	State         string  `json:"state"`
	Reason        string  `json:"reason,omitempty"`
	Revision      uint64  `json:"revision"`
	StartedAt     string  `json:"started_at"`
	Deadline      string  `json:"deadline"`
	ResolvedAt    string  `json:"resolved_at,omitempty"`
	TimeoutAction string  `json:"timeout_action"`
	Modified      bool    `json:"modified"`
	WaitDuration  float64 `json:"wait_duration_ms"`
}

type Summary struct {
	Metadata
	TraceID string `json:"trace_id"`
	Method  string `json:"method"`
	Path    string `json:"path"`
	Model   string `json:"model,omitempty"`
}

type Detail struct {
	Summary
	Body              string            `json:"body"`
	Headers           map[string]string `json:"headers"`
	OriginalBody      string            `json:"original_body"`
	OriginalHeaders   map[string]string `json:"original_headers"`
	EditBlockedReason string            `json:"edit_blocked_reason,omitempty"`
	BodyEncoding      string            `json:"body_encoding,omitempty"`
}

type Edit struct {
	Revision uint64            `json:"revision"`
	Body     *string           `json:"body,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
}

type entry struct {
	detail   Detail
	ctx      context.Context
	started  time.Time
	deadline time.Time
	done     chan struct{}
}

type Manager struct {
	mu      sync.Mutex
	entries map[string]*entry
	changed func()
}

func New(changed func()) *Manager {
	return &Manager{entries: make(map[string]*entry), changed: changed}
}

func clone(detail Detail) Detail {
	detail.Headers = maps.Clone(detail.Headers)
	detail.OriginalHeaders = maps.Clone(detail.OriginalHeaders)
	return detail
}

func (m *Manager) notify() {
	if m.changed != nil {
		m.changed()
	}
}

// Add is called once per gateway-generated trace ID. It retains only editable
// headers; authentication stays exclusively on the proxy's HTTP request.
func (m *Manager) Add(ctx context.Context, detail Detail, wait time.Duration) Detail {
	now := time.Now()
	detail.State, detail.Revision = "pending", 1
	detail.StartedAt = now.Format(time.RFC3339Nano)
	detail.Deadline = now.Add(wait).Format(time.RFC3339Nano)
	detail.Modified = detail.Body != detail.OriginalBody || !maps.Equal(detail.Headers, detail.OriginalHeaders)
	m.mu.Lock()
	m.entries[detail.TraceID] = &entry{
		detail: clone(detail), ctx: ctx, started: now, deadline: now.Add(wait), done: make(chan struct{}),
	}
	m.mu.Unlock()
	m.notify()
	return clone(detail)
}

func (m *Manager) finishLocked(e *entry, state, reason string) {
	now := time.Now()
	e.detail.State, e.detail.Reason = state, reason
	e.detail.ResolvedAt = now.Format(time.RFC3339Nano)
	e.detail.WaitDuration = float64(now.Sub(e.started).Microseconds()) / 1000
	e.detail.Revision++
	e.ctx = nil // Do not retain finished HTTP contexts for the process lifetime.
	close(e.done)
}

func (m *Manager) expireLocked(e *entry) bool {
	if e.detail.State != "pending" {
		return false
	}
	if e.ctx.Err() != nil {
		m.finishLocked(e, "canceled", "client_disconnected")
		return true
	}
	if !time.Now().Before(e.deadline) {
		state := "released"
		if e.detail.TimeoutAction == "cancel" {
			state = "canceled"
		}
		m.finishLocked(e, state, "timeout")
		return true
	}
	return false
}

func (m *Manager) Get(traceID string) (Detail, error) {
	m.mu.Lock()
	e := m.entries[traceID]
	if e == nil {
		m.mu.Unlock()
		return Detail{}, ErrNotFound
	}
	changed := m.expireLocked(e)
	detail := clone(e.detail)
	m.mu.Unlock()
	if changed {
		m.notify()
	}
	return detail, nil
}

func (m *Manager) Pending() []Summary {
	m.mu.Lock()
	items := make([]Summary, 0)
	changed := false
	for _, e := range m.entries {
		changed = m.expireLocked(e) || changed
		if e.detail.State == "pending" {
			items = append(items, e.detail.Summary)
		}
	}
	m.mu.Unlock()
	if changed {
		m.notify()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt < items[j].StartedAt })
	return items
}

// Wait runs in the original HTTP handler, with no goroutine per browser or edit.
func (m *Manager) Wait(ctx context.Context, traceID string) Detail {
	m.mu.Lock()
	e := m.entries[traceID]
	done, deadline := e.done, e.deadline
	m.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
	m.mu.Lock()
	changed := m.expireLocked(e)
	detail := clone(e.detail)
	m.mu.Unlock()
	if changed {
		m.notify()
	}
	return detail
}

// Apply validates outside the shared lock, then checks context, time and version
// again before committing. An expensive edit cannot prevent other deadlines.
func (m *Manager) Apply(traceID, action string, edit Edit) (Detail, error) {
	current, err := m.Get(traceID)
	if err != nil {
		return Detail{}, err
	}
	if current.State != "pending" || edit.Revision != current.Revision {
		return Detail{}, ErrConflict
	}
	candidate := clone(current)
	if action != "cancel" {
		if edit.Body != nil {
			candidate.Body = *edit.Body
		}
		if edit.Headers != nil {
			candidate.Headers, err = NormalizeHeaders(edit.Headers)
			if err != nil {
				return Detail{}, &ValidationError{err}
			}
		}
		if action == "validate" || candidate.Body != current.Body || !maps.Equal(candidate.Headers, current.Headers) {
			if current.EditBlockedReason != "" {
				return Detail{}, &ValidationError{errors.New(current.EditBlockedReason)}
			}
			if err := Validate(candidate.Path, candidate.OriginalBody, candidate.Body, candidate.Headers); err != nil {
				return Detail{}, &ValidationError{err}
			}
		}
	}
	m.mu.Lock()
	e := m.entries[traceID]
	expired := m.expireLocked(e)
	if e.detail.State != "pending" || e.detail.Revision != edit.Revision {
		m.mu.Unlock()
		if expired {
			m.notify()
		}
		return Detail{}, ErrConflict
	}
	switch action {
	case "save", "release":
		e.detail.Body, e.detail.Headers = candidate.Body, maps.Clone(candidate.Headers)
		e.detail.Modified = candidate.Body != candidate.OriginalBody || !maps.Equal(candidate.Headers, candidate.OriginalHeaders)
		if action == "release" {
			m.finishLocked(e, "released", "manual")
		} else {
			e.detail.Revision++
		}
	case "cancel":
		m.finishLocked(e, "canceled", "manual")
	case "validate":
		// No saved state changes.
	default:
		m.mu.Unlock()
		return Detail{}, fmt.Errorf("unknown interception action: %s", action)
	}
	result := clone(e.detail)
	m.mu.Unlock()
	if action != "validate" {
		m.notify()
	}
	return result, nil
}
