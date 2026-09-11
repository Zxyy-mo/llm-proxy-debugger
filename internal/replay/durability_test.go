package replay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type startResult struct {
	record  Record
	created bool
	err     error
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for replay test gate")
	}
}

func TestRegistrationWaitsForDurabilityAndDeduplicatesConcurrentStarts(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	var executions atomic.Int32
	m := NewWithPersistence(nil, func(saved []Saved, durable bool) error {
		if durable {
			if len(saved) != 1 || saved[0].Record.IdempotencyKey != "key" {
				return errors.New("registration snapshot is incomplete")
			}
			close(entered)
			<-release
		}
		return nil
	})
	t.Cleanup(m.Shutdown)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	run := func(context.Context) Outcome {
		executions.Add(1)
		return Outcome{State: "done", StatusCode: 200}
	}
	const duplicates = 8
	results := make(chan startResult, duplicates+1)
	go func() {
		record, created, err := m.Start("key", "fp", Record{ID: "first"}, time.Second, run)
		results <- startResult{record, created, err}
	}()
	awaitSignal(t, entered)
	attempted := make(chan struct{}, duplicates)
	for i := range duplicates {
		go func() {
			attempted <- struct{}{}
			record, created, err := m.Start("key", "fp", Record{ID: fmt.Sprintf("duplicate-%d", i)}, time.Second, run)
			results <- startResult{record, created, err}
		}()
	}
	for range duplicates {
		awaitSignal(t, attempted)
	}
	select {
	case result := <-results:
		t.Fatalf("registration was acknowledged before its save: %+v", result)
	default:
	}
	if executions.Load() != 0 {
		t.Fatal("replay executed before its registration was durable")
	}
	released.Do(func() { close(release) })
	createdCount := 0
	for range duplicates + 1 {
		select {
		case result := <-results:
			if result.err != nil || result.record.ID != "first" {
				t.Fatalf("concurrent retry did not return the registered record: %+v", result)
			}
			if result.created {
				createdCount++
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent registration did not return")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished, err := m.Wait(ctx, "first")
	if err != nil || finished.State != "done" || createdCount != 1 || executions.Load() != 1 {
		t.Fatalf("duplicate registrations executed incorrectly: %+v, created=%d, executions=%d, err=%v", finished, createdCount, executions.Load(), err)
	}
}

func TestRegistrationFailureRollsBackAndSameKeyCanRetry(t *testing.T) {
	var fail, executions, notifications atomic.Int32
	fail.Store(1)
	m := NewWithPersistence(func() { notifications.Add(1) }, func(_ []Saved, durable bool) error {
		if durable && fail.Load() != 0 {
			return errors.New("disk write failed")
		}
		return nil
	})
	t.Cleanup(m.Shutdown)
	run := func(context.Context) Outcome {
		executions.Add(1)
		return Outcome{State: "done", StatusCode: 200}
	}
	_, created, err := m.Start("retryable", "fp", Record{ID: "failed"}, time.Second, run)
	if !errors.Is(err, ErrPersistence) || created || executions.Load() != 0 || notifications.Load() != 0 || len(m.List()) != 0 || len(m.Snapshot()) != 0 {
		t.Fatalf("failed registration left state or executed: created=%v executions=%d notifications=%d records=%+v err=%v", created, executions.Load(), notifications.Load(), m.List(), err)
	}
	fail.Store(0)
	record, created, err := m.Start("retryable", "fp", Record{ID: "accepted"}, time.Second, run)
	if err != nil || !created || record.ID != "accepted" {
		t.Fatalf("key did not become retryable after save recovery: %+v %v %v", record, created, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if record, err = m.Wait(ctx, record.ID); err != nil || record.State != "done" || executions.Load() != 1 {
		t.Fatalf("retried execution failed: %+v executions=%d err=%v", record, executions.Load(), err)
	}
	if _, _, err := m.Start("retryable", "different", Record{ID: "conflict"}, time.Second, run); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("accepted key must still reject different content: %v", err)
	}
}

func TestDelayedNotificationCannotOverwriteNewerReplaySnapshots(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	finishFirst := make(chan struct{})
	var released, finished sync.Once
	var notifications atomic.Int32
	var savedMu sync.Mutex
	var saved []Saved
	m := NewWithPersistence(func() {
		if notifications.Add(1) == 1 {
			close(entered)
			<-release
		}
	}, func(snapshot []Saved, _ bool) error {
		savedMu.Lock()
		saved = snapshot
		savedMu.Unlock()
		return nil
	})
	t.Cleanup(m.Shutdown)
	t.Cleanup(func() {
		released.Do(func() { close(release) })
		finished.Do(func() { close(finishFirst) })
	})
	firstResult := make(chan startResult, 1)
	go func() {
		record, created, err := m.Start("first-key", "fp", Record{ID: "first"}, time.Second, func(ctx context.Context) Outcome {
			select {
			case <-finishFirst:
			case <-ctx.Done():
			}
			return Outcome{State: "done", StatusCode: 200}
		})
		firstResult <- startResult{record, created, err}
	}()
	awaitSignal(t, entered)
	_, created, err := m.Start("second-key", "fp", Record{ID: "second"}, time.Second, func(context.Context) Outcome {
		return Outcome{State: "done", StatusCode: 200}
	})
	if err != nil || !created {
		t.Fatalf("second registration blocked on an old notification: %v %v", created, err)
	}
	finished.Do(func() { close(finishFirst) })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, id := range []string{"first", "second"} {
		if record, err := m.Wait(ctx, id); err != nil || record.State != "done" {
			t.Fatalf("replay did not finish: %+v %v", record, err)
		}
	}
	released.Do(func() { close(release) })
	select {
	case result := <-firstResult:
		if result.err != nil || !result.created {
			t.Fatalf("first creation failed: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("first notification did not finish")
	}
	savedMu.Lock()
	defer savedMu.Unlock()
	if len(saved) != 2 {
		t.Fatalf("an older notification erased an accepted key: %+v", saved)
	}
	for _, item := range saved {
		if item.Record.State != "done" || item.Record.FinishedAt == "" {
			t.Fatalf("an older notification erased the terminal state: %+v", saved)
		}
	}
}

func TestShutdownDuringRegistrationPreventsDispatchAndRejectsNewKeys(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var released sync.Once
	var executions atomic.Int32
	m := NewWithPersistence(nil, func(_ []Saved, durable bool) error {
		if durable {
			close(entered)
			<-release
		}
		return nil
	})
	t.Cleanup(m.Shutdown)
	t.Cleanup(func() { released.Do(func() { close(release) }) })
	run := func(context.Context) Outcome {
		executions.Add(1)
		return Outcome{State: "done"}
	}
	result := make(chan startResult, 1)
	go func() {
		record, created, err := m.Start("key", "fp", Record{ID: "registered"}, time.Second, run)
		result <- startResult{record, created, err}
	}()
	awaitSignal(t, entered)
	shutdown := make(chan struct{})
	go func() { m.Shutdown(); close(shutdown) }()
	awaitSignal(t, m.lifecycle.Done())
	released.Do(func() { close(release) })
	select {
	case result := <-result:
		if !errors.Is(result.err, ErrClosed) || result.created || result.record.State != "canceled" || result.record.Reason != "shutdown" {
			t.Fatalf("shutdown during registration was not explicit: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("registration did not stop after shutdown")
	}
	awaitSignal(t, shutdown)
	if _, _, err := m.Start("new-key", "fp", Record{ID: "late"}, time.Second, run); !errors.Is(err, ErrClosed) {
		t.Fatalf("shutdown accepted a new key: %v", err)
	}
	record, created, err := m.Start("key", "fp", Record{ID: "duplicate"}, time.Second, run)
	if err != nil || created || record.ID != "registered" || executions.Load() != 0 {
		t.Fatalf("shutdown lost its committed key or dispatched: %+v %v %v executions=%d", record, created, err, executions.Load())
	}
}

func TestRestoreKeepsLegacyKeysWithoutResumingExecutions(t *testing.T) {
	m := New(nil)
	t.Cleanup(m.Shutdown)
	m.Restore([]Saved{
		{Record: Record{ID: "interrupted", TraceID: "not-yet-captured", State: "running", IdempotencyKey: "running-key"}, Fingerprint: "fp"},
		{Record: Record{ID: "finished", State: "done", StatusCode: 200, IdempotencyKey: "finished-key"}, Fingerprint: "fp"},
	})
	for key, id := range map[string]string{"running-key": "interrupted", "finished-key": "finished"} {
		record, created, err := m.Start(key, "fp", Record{ID: "must-not-execute"}, time.Second, func(context.Context) Outcome {
			t.Error("restore must not resume an execution")
			return Outcome{State: "error"}
		})
		if err != nil || created || record.ID != id {
			t.Fatalf("saved key was lost: %+v %v %v", record, created, err)
		}
		if key == "running-key" && (record.State != "error" || record.StatusCode != 503 || record.Reason != "gateway_restarted") {
			t.Fatalf("saved running record was not interrupted: %+v", record)
		}
		if key == "finished-key" && (record.State != "done" || record.StatusCode != 200) {
			t.Fatalf("restore changed a terminal record: %+v", record)
		}
	}
}
