package privacy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCustomPatternsCannotRewriteExistingPlaceholders(t *testing.T) {
	e := New()
	policy := e.Policy()
	policy.Record = true
	policy.AllowReveal = true
	policy.Patterns = append(policy.Patterns, Pattern{Name: "word", Expression: `[a-z]+`})
	if err := e.Set(policy); err != nil {
		t.Fatal(err)
	}
	redacted := e.Text(policy, "scope", "private@example.com hello")
	if again := e.Text(policy, "scope", redacted); again != redacted {
		t.Fatalf("placeholder was rewritten: %s -> %s", redacted, again)
	}
	restored, err := e.Reveal("scope", redacted)
	if err != nil || restored != "private@example.com hello" {
		t.Fatalf("restoration broken: %s %v", restored, err)
	}
}

func TestStableJSONRedactionAndControlledRestore(t *testing.T) {
	e := New()
	p := e.Policy()
	p.Record = true
	p.Outbound = true
	p.AllowReveal = true
	if err := e.Set(p); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"messages":[{"role":"user","content":"a@example.com 13812345678"},{"role":"tool","content":"a@example.com"}],"large":9007199254740993}`)
	projected := e.JSON(p, "credential-scope", body)
	if strings.Contains(string(projected), "a@example.com") || strings.Contains(string(projected), "13812345678") || !strings.Contains(string(projected), "9007199254740993") || !json.Valid(projected) {
		t.Fatalf("bad projection: %s", projected)
	}
	var v struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(projected, &v)
	if !strings.HasPrefix(v.Messages[0].Content, v.Messages[1].Content) {
		t.Fatal("same value received different placeholders")
	}
	restored, err := e.Reveal("credential-scope", string(projected))
	if err != nil || !strings.Contains(restored, "a@example.com") {
		t.Fatal("explicit restore failed")
	}
	other, _ := e.Reveal("other-scope", string(projected))
	if strings.Contains(other, "a@example.com") {
		t.Fatal("cross-identity reveal")
	}
	p.AllowReveal = false
	_ = e.Set(p)
	if _, err = e.Reveal("credential-scope", string(projected)); err == nil {
		t.Fatal("restore ignored policy")
	}
}

func TestNoRetainedOriginalsAndValidation(t *testing.T) {
	e := New()
	p := e.Policy()
	p.Record = true
	p.RetainRaw = false
	_ = e.Set(p)
	e.JSON(p, "scope", []byte(`{"input":"a@example.com"}`))
	state, _ := json.Marshal(e.Snapshot())
	if strings.Contains(string(state), "a@example.com") {
		t.Fatal("raw value retained despite policy")
	}
	p.Patterns = []Pattern{{Name: "empty", Expression: ".*"}}
	if Validate(p) == nil {
		t.Fatal("empty matches accepted")
	}
}
