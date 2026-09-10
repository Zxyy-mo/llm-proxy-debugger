// Package correlation extracts only protocol-visible conversation evidence.
// Message fingerprints are independent of the display/log body size limit.
package correlation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const MaxHistoryBytes = 4 << 20

type Identity struct {
	Kind   string
	Value  string
	Source string
}

// Request holds indexing material. Scope and message hashes are never exposed
// through the log or graph API, and no raw credentials are retained here.
type Request struct {
	Scope              string
	Protocol           string
	Model              string
	Summary            string
	Identities         []Identity
	PreviousResponseID string
	ParentTraceID      string
	ContextHash        string
	Messages           []string
	HasUser            bool
	HistoryLimited     bool
}

func Hash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func Protocol(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for _, part := range parts {
		if part == "responses" {
			return "responses"
		}
	}
	if strings.Contains(path, "chat/completions") || strings.Contains(path, "openai") {
		return "openai"
	}
	return "anthropic"
}

func ExtractRequest(h http.Header, path string, body []byte, upstream string) Request {
	r := Request{
		Scope:    Hash(upstream, h.Get("Authorization"), h.Get("X-API-Key")),
		Protocol: Protocol(path),
	}
	seen := make(map[string]bool)
	add := func(kind, value, source string) {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 1024 || seen[kind+"\x00"+value] {
			return
		}
		seen[kind+"\x00"+value] = true
		r.Identities = append(r.Identities, Identity{Kind: kind, Value: value, Source: source})
	}
	add("session", h.Get("X-Session-ID"), "header:X-Session-ID")
	add("conversation", h.Get("X-Conversation-ID"), "header:X-Conversation-ID")
	add("thread", h.Get("X-Thread-ID"), "header:X-Thread-ID")
	r.ParentTraceID = boundedID(h.Get("X-Parent-Trace-ID"))

	var root gjson.Result
	if gjson.ValidBytes(body) {
		root = gjson.ParseBytes(body)
	}
	r.Model = stringValue(root.Get("model"))
	r.PreviousResponseID = boundedID(stringValue(root.Get("previous_response_id")))
	if r.ParentTraceID == "" {
		r.ParentTraceID = boundedID(stringValue(root.Get("metadata.parent_trace_id")))
	}
	for _, field := range []struct{ path, kind string }{
		{"session_id", "session"}, {"conversation", "conversation"},
		{"conversation.id", "conversation"}, {"conversation_id", "conversation"},
		{"thread_id", "thread"}, {"metadata.session_id", "session"},
		{"metadata.conversation_id", "conversation"}, {"metadata.thread_id", "thread"},
	} {
		add(field.kind, stringValue(root.Get(field.path)), "body:"+field.path)
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "threads" {
			add("thread", parts[i+1], "path:thread_id")
		} else if parts[i] == "conversations" {
			add("conversation", parts[i+1], "path:conversation_id")
		}
	}

	input := root.Get("messages")
	if !input.IsArray() {
		input = root.Get("input")
	}
	if input.Type == gjson.String {
		r.Summary = summary(input.String())
	} else if input.IsArray() {
		items := input.Array()
		for i := len(items) - 1; i >= 0; i-- {
			if items[i].Get("role").String() == "user" {
				r.Summary = summary(contentText(items[i].Get("content")))
				break
			}
		}
	}
	if len(body) > MaxHistoryBytes {
		r.HistoryLimited = true
		return r
	}
	context, _ := json.Marshal([]any{
		r.Protocol, normalizeContent(root.Get("system")),
		normalizeContent(root.Get("instructions")), root.Get("tools").Value(),
	})
	r.ContextHash = Hash(r.Scope, string(context))
	if input.Type == gjson.String && input.String() != "" {
		message, _ := json.Marshal(map[string]any{"role": "user", "content": input.String()})
		if fp := fingerprintItem(gjson.ParseBytes(message)); fp != "" {
			r.Messages = append(r.Messages, fp)
			r.HasUser = true
		}
	} else if input.IsArray() {
		for _, item := range input.Array() {
			if fp := fingerprintItem(item); fp != "" {
				r.Messages = append(r.Messages, fp)
				if item.Get("role").String() == "user" {
					r.HasUser = true
				}
			}
		}
	}
	return r
}

func (r Request) Prefixes() []string {
	keys := make([]string, 0, len(r.Messages))
	key := r.ContextHash
	for _, message := range r.Messages {
		key = Hash(key, message)
		keys = append(keys, key)
	}
	return keys
}

// CompletedKey only indexes a visible, complete transcript. A stateful Responses
// request can refer to hidden server context, so its local input is not sufficient.
func (r Request) CompletedKey(response Response) string {
	if !r.HasUser || len(r.Messages) == 0 || len(response.Messages) == 0 || !response.Complete || response.Error != "" || r.PreviousResponseID != "" {
		return ""
	}
	if r.Protocol == "responses" {
		for _, id := range r.Identities {
			if id.Kind == "conversation" {
				return ""
			}
		}
	}
	key := r.ContextHash
	for _, message := range r.Messages {
		key = Hash(key, message)
	}
	for _, message := range response.Messages {
		key = Hash(key, message)
	}
	return key
}

func boundedID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1024 {
		return ""
	}
	return value
}

func stringValue(value gjson.Result) string {
	if value.Type == gjson.String {
		return value.String()
	}
	return ""
}

func summary(value string) string {
	runes := []rune(strings.Join(strings.Fields(value), " "))
	if len(runes) > 120 {
		return string(runes[:120]) + "…"
	}
	return string(runes)
}

func contentText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	var text strings.Builder
	for _, part := range content.Array() {
		if value := part.Get("text"); value.Type == gjson.String {
			text.WriteString(value.String())
		}
	}
	return text.String()
}

func normalizeContent(content gjson.Result) []any {
	parts := make([]any, 0)
	appendText := func(text string) {
		if text == "" {
			return
		}
		if len(parts) > 0 {
			if previous, ok := parts[len(parts)-1].(string); ok {
				parts[len(parts)-1] = previous + text
				return
			}
		}
		parts = append(parts, text)
	}
	if content.Type == gjson.String {
		appendText(content.String())
	} else if content.IsArray() {
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "text", "input_text", "output_text":
				appendText(part.Get("text").String())
			case "thinking", "redacted_thinking", "reasoning":
				// Reasoning/signatures are often not replayed by clients.
			case "tool_use":
				parts = append(parts, map[string]any{
					"type": "tool_use", "id": part.Get("id").String(),
					"name": part.Get("name").String(), "input": part.Get("input").Value(),
				})
			default:
				parts = append(parts, part.Value())
			}
		}
	}
	return parts
}

func fingerprintItem(item gjson.Result) string {
	if !item.IsObject() {
		return ""
	}
	kind := item.Get("type").String()
	if kind == "reasoning" {
		return ""
	}
	var normalized any
	role := item.Get("role").String()
	if role != "" {
		message := map[string]any{"role": role, "content": normalizeContent(item.Get("content"))}
		for _, key := range []string{"name", "tool_call_id"} {
			if value := item.Get(key); value.Type == gjson.String && value.String() != "" {
				message[key] = value.String()
			}
		}
		if calls := item.Get("tool_calls"); calls.IsArray() && len(calls.Array()) > 0 {
			var normalizedCalls []any
			for _, call := range calls.Array() {
				normalizedCalls = append(normalizedCalls, map[string]any{
					"id": call.Get("id").String(), "name": call.Get("function.name").String(),
					"arguments": call.Get("function.arguments").String(),
				})
			}
			message["tool_calls"] = normalizedCalls
		}
		if function := item.Get("function_call"); function.IsObject() {
			message["function_call"] = function.Value()
		}
		normalized = message
	} else if kind == "function_call" {
		normalized = map[string]any{
			"type": kind, "call_id": item.Get("call_id").String(),
			"name": item.Get("name").String(), "arguments": item.Get("arguments").String(),
		}
	} else {
		// Unknown/multimodal items must match exactly; never guess their semantics.
		normalized = item.Value()
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return ""
	}
	return Hash(string(canonical))
}
