package intercept

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func pendingFixture(id string) Detail {
	body := `{"model":"mock","input":"original"}`
	return Detail{
		Summary: Summary{TraceID: id, Method: "POST", Path: "/v1/responses", Metadata: Metadata{TimeoutAction: "forward"}},
		Body:    body, OriginalBody: body, Headers: map[string]string{}, OriginalHeaders: map[string]string{},
	}
}

func TestSavedDraftSurvivesUntilTimeout(t *testing.T) {
	m := New(nil)
	pending := m.Add(context.Background(), pendingFixture("saved"), 30*time.Millisecond)
	body := `{"model":"mock","input":"saved"}`
	saved, err := m.Apply("saved", "save", Edit{Revision: 1, Body: &body})
	if err != nil || saved.Deadline != pending.Deadline {
		t.Fatalf("saving should retain deadline: %v", err)
	}
	finished := m.Wait(context.Background(), "saved")
	if finished.State != "released" || finished.Reason != "timeout" || finished.Body != body || !finished.Modified {
		t.Fatalf("timeout used incorrect candidate: %+v", finished)
	}
	if _, err := m.Apply("saved", "cancel", Edit{Revision: finished.Revision}); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal decision was reversed")
	}
}

func TestExpiredRequestRejectsEditEvenBeforeWaitRuns(t *testing.T) {
	m := New(nil)
	m.Add(context.Background(), pendingFixture("expired"), time.Millisecond)
	time.Sleep(3 * time.Millisecond)
	body := `{"model":"mock","input":"too late"}`
	if _, err := m.Apply("expired", "release", Edit{Revision: 1, Body: &body}); !errors.Is(err, ErrConflict) {
		t.Fatalf("late edit was accepted: %v", err)
	}
	detail, _ := m.Get("expired")
	if detail.Reason != "timeout" || detail.Body != detail.OriginalBody {
		t.Fatal("late edit replaced the timeout candidate")
	}
}

func TestConcurrentDecisionsAndDisconnects(t *testing.T) {
	m := New(nil)
	var handlers sync.WaitGroup
	for i := range 40 {
		handlers.Add(1)
		go func(i int) {
			defer handlers.Done()
			id := fmt.Sprint(i)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m.Add(ctx, pendingFixture(id), 100*time.Millisecond)
			var decisions sync.WaitGroup
			var successes atomic.Int32
			for _, action := range []string{"release", "cancel", "release"} {
				decisions.Add(1)
				go func(action string) {
					defer decisions.Done()
					if _, err := m.Apply(id, action, Edit{Revision: 1}); err == nil {
						successes.Add(1)
					} else if !errors.Is(err, ErrConflict) {
						t.Errorf("unexpected decision failure: %v", err)
					}
				}(action)
			}
			if i%2 == 0 {
				cancel()
			}
			decisions.Wait()
			finished := m.Wait(ctx, id)
			if successes.Load() > 1 || finished.State == "pending" || finished.Revision != 2 {
				t.Errorf("multiple terminal decisions: %+v (successes %d)", finished, successes.Load())
			}
		}(i)
	}
	handlers.Wait()
	if len(m.Pending()) != 0 {
		t.Fatal("completed requests remained in the pending list")
	}
}
