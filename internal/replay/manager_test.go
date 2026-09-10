package replay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestIdempotentStartReturnsOneRecordPerAction(t *testing.T) {
	var executions int
	var mu sync.Mutex
	notified := 0
	m := New(func() { mu.Lock(); notified++; mu.Unlock() })
	run := func(ctx context.Context) Outcome {
		mu.Lock()
		executions++
		mu.Unlock()
		return Outcome{State: "done", StatusCode: 200}
	}
	first, created, err := m.Start("key", "fp-a", Record{ID: "r1", TraceID: "t1"}, time.Second, run)
	if err != nil || !created || first.State != "running" || first.TimeoutSeconds != 1 || first.IdempotencyKey != "key" {
		t.Fatalf("first start wrong: %+v %v %v", first, created, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	finished, err := m.Wait(ctx, "r1")
	if err != nil || finished.State != "done" || finished.FinishedAt == "" || finished.StatusCode != 200 {
		t.Fatalf("record did not finish: %+v %v", finished, err)
	}
	again, created, err := m.Start("key", "fp-a", Record{ID: "r2"}, time.Second, run)
	if err != nil || created || again.ID != "r1" {
		t.Fatalf("retry must return the existing record: %+v %v %v", again, created, err)
	}
	if _, _, err := m.Start("key", "fp-b", Record{ID: "r3"}, time.Second, run); !errors.Is(err, ErrKeyReused) {
		t.Fatalf("key reuse with other content must fail: %v", err)
	}
	if _, created, _ := m.Start("other", "fp-a", Record{ID: "r4"}, time.Second, run); !created {
		t.Fatal("a new key must execute again")
	}
	m.Wait(ctx, "r4")
	mu.Lock()
	defer mu.Unlock()
	if executions != 2 || len(m.List()) != 2 || notified < 4 {
		t.Fatalf("executions=%d records=%d notified=%d", executions, len(m.List()), notified)
	}
	if _, err := m.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown record must be ErrNotFound")
	}
}

func TestCancelAndDeadlineAreReportedDistinctly(t *testing.T) {
	m := New(nil)
	block := func(ctx context.Context) Outcome {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Outcome{State: "error", Error: ctx.Err().Error(), StatusCode: 504}
		}
		return Outcome{State: "canceled", StatusCode: 499}
	}
	m.Start("cancel", "fp", Record{ID: "c"}, time.Minute, block)
	record, err := m.Cancel("c")
	if err != nil || record.State != "canceled" || record.Reason != "manual" || record.StatusCode != 499 {
		t.Fatalf("cancel wrong: %+v %v", record, err)
	}
	if _, err := m.Cancel("c"); !errors.Is(err, ErrFinished) {
		t.Fatal("finished replays cannot be canceled again")
	}
	if _, err := m.Cancel("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown replays cannot be canceled")
	}
	m.Start("deadline", "fp", Record{ID: "d"}, 20*time.Millisecond, block)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	record, _ = m.Wait(ctx, "d")
	if record.State != "error" || record.Reason != "timeout" || record.StatusCode != 504 {
		t.Fatalf("deadline wrong: %+v", record)
	}
	m.Start("blank", "fp", Record{ID: "b"}, time.Second, func(context.Context) Outcome { return Outcome{} })
	if record, _ = m.Wait(ctx, "b"); record.State != "error" {
		t.Fatalf("an empty outcome must not look successful: %+v", record)
	}
}
