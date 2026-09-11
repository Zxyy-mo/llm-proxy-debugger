package intercept

import (
	"strings"
	"testing"
)

func TestEditingCannotChangeRunEvidence(t *testing.T) {
	const path = "/v1/chat/completions"
	const original = `{"model":"mock","metadata":{"run_id":"run-1"},"messages":[{"role":"user","content":"before"}]}`
	const messages = `"messages":[{"role":"user","content":"after"}]`
	for _, candidate := range []string{
		`{"model":"mock","metadata":{"run_id":"run-2"},` + messages + `}`,
		`{"model":"mock",` + messages + `}`,
		`{"model":"mock","metadata":{"run_id":1},` + messages + `}`,
	} {
		if err := Validate(path, original, candidate, nil); err == nil || !strings.Contains(err.Error(), "run") {
			t.Fatalf("changed task identity was accepted: %v", err)
		}
	}
	if err := Validate(path, original, `{"model":"other","metadata":{"run_id":"run-1"},`+messages+`}`, nil); err != nil {
		t.Fatalf("ordinary edits with unchanged task failed: %v", err)
	}
	invalidBefore := `{"model":"mock","metadata":{"run_id":1},` + messages + `}`
	invalidAfter := `{"model":"mock","metadata":{"run_id":2},` + messages + `}`
	if err := Validate(path, invalidBefore, invalidAfter, nil); err == nil {
		t.Fatal("two different invalid task identifiers bypassed immutable evidence check")
	}
}
