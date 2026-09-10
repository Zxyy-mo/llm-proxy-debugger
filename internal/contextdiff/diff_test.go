package contextdiff

import (
	"strings"
	"testing"
)

func TestInsertionDoesNotMisalignHistory(t *testing.T) {
	a, _ := Parse([]byte(`{"messages":[{"role":"system","content":"before"},{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}],"tools":[],"seed":9007199254740993}`))
	b, _ := Parse([]byte(`{"messages":[{"role":"system","content":"after"},{"role":"user","content":"a"},{"role":"tool","content":"inserted","tool_call_id":"x"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}],"tools":[{"value":9007199254740993}]}`))
	diff := Compare(a, b)
	if diff.Added != 1 || diff.Removed != 0 || diff.Unchanged != 3 || !diff.System.Changed || !diff.Tools.Changed {
		t.Fatalf("bad alignment: %+v", diff)
	}
	if !strings.Contains(diff.Tools.After, "9007199254740993") {
		t.Fatal("large number lost precision")
	}
}

func TestResponsesHistoryAndUnknownRemoteContext(t *testing.T) {
	a, _ := Parse([]byte(`{"input":"hello","previous_response_id":"external"}`))
	b, _ := Parse([]byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`))
	diff := Compare(a, b)
	if !diff.RemoteContext || diff.Unchanged != 1 || diff.Added != 0 {
		t.Fatalf("normalization: %+v", diff)
	}
}
