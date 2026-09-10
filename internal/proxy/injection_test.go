package proxy

import (
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/tidwall/gjson"
)

func TestProtocolAwarePromptInjection(t *testing.T) {
	cases := []struct{ path, body, field, want string }{
		{"/v1/messages", `{"system":[{"type":"text","text":"old"}],"messages":[],"seed":9007199254740993}`, "system.1.text", "new"},
		{"/v1/chat/completions", `{"messages":[{"role":"user","content":"hello"}],"seed":9007199254740993}`, "messages.0.content", "new"},
		{"/v1/chat/completions", `{"messages":[{"role":"developer","content":"old"}],"seed":9007199254740993}`, "messages.0.content", "old\n\nnew"},
		{"/v1/responses", `{"instructions":"old","input":"hello","seed":9007199254740993}`, "instructions", "old\n\nnew"},
	}
	for _, tc := range cases {
		result := injectPrompt(tc.path, []byte(tc.body), "new")
		if gjson.GetBytes(result, tc.field).String() != tc.want || !strings.Contains(string(result), "9007199254740993") {
			t.Fatalf("%s injection corrupted request: %s", tc.path, result)
		}
	}
}

func TestRulePriorityKeepsTiesStable(t *testing.T) {
	s := store.New()
	s.Rules = []store.Rule{{ID: "first", Priority: 0}, {ID: "high", Priority: 10}, {ID: "second", Priority: 0}}
	rules := s.RulesSnapshot()
	if rules[0].ID != "high" || rules[1].ID != "first" || rules[2].ID != "second" {
		t.Fatal("priority/tie order incorrect")
	}
}
