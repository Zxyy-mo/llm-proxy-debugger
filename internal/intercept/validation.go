package intercept

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/tidwall/gjson"
)

var editableHeaderNames = []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta", "Openai-Beta"}

func EditableHeaders(headers http.Header) map[string]string {
	result := make(map[string]string)
	for _, name := range editableHeaderNames {
		if values := headers.Values(name); len(values) > 0 {
			result[name] = strings.Join(values, ", ")
		}
	}
	return result
}

func NormalizeHeaders(headers map[string]string) (map[string]string, error) {
	result := make(map[string]string)
	for name, value := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if !slices.Contains(editableHeaderNames, canonical) {
			return nil, fmt.Errorf("header %q is not editable", name)
		}
		if _, exists := result[canonical]; exists {
			return nil, fmt.Errorf("duplicate header %q", canonical)
		}
		if len(value) > 8192 {
			return nil, fmt.Errorf("header %q is too long", canonical)
		}
		for _, c := range value {
			if (c < 32 && c != '\t') || c == 127 {
				return nil, fmt.Errorf("header %q contains control characters", canonical)
			}
		}
		result[canonical] = value
	}
	return result, nil
}

// Validate deliberately checks structural invariants, not a frozen copy of every
// provider schema. Unknown fields and multimodal content types remain intact.
func Validate(path, original, body string, headers map[string]string) error {
	if _, err := NormalizeHeaders(headers); err != nil {
		return err
	}
	if value := headers["Content-Type"]; value != "" {
		mediaType, _, err := mime.ParseMediaType(value)
		if err != nil || (mediaType != "application/json" && !strings.HasSuffix(mediaType, "+json")) {
			return fmt.Errorf("edited JSON requests require a JSON Content-Type")
		}
	}
	if err := uniqueJSON(body); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	root := gjson.Parse(body)
	if !root.IsObject() {
		return fmt.Errorf("request body must be a JSON object")
	}
	before := correlation.ExtractRequest(nil, path, []byte(original), "")
	after := correlation.ExtractRequest(nil, path, []byte(body), "")
	if !slices.Equal(before.Identities, after.Identities) || before.ParentTraceID != after.ParentTraceID || before.PreviousResponseID != after.PreviousResponseID {
		return fmt.Errorf("session, conversation, thread and parent identifiers cannot be changed on a captured request")
	}
	path = strings.TrimSuffix(path, "/")
	if !strings.HasSuffix(path, "/messages") && !strings.HasSuffix(path, "/chat/completions") && !strings.HasSuffix(path, "/responses") {
		return nil
	}
	if root.Get("model").Type != gjson.String || strings.TrimSpace(root.Get("model").String()) == "" {
		return fmt.Errorf("model must be a non-empty string")
	}
	if stream := root.Get("stream"); stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
		return fmt.Errorf("stream must be a boolean")
	}
	if tools := root.Get("tools"); tools.Exists() && !tools.IsArray() {
		return fmt.Errorf("tools must be an array")
	}
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return validateChat(root.Get("messages"))
	case strings.HasSuffix(path, "/responses"):
		return validateResponses(root)
	default:
		if max := root.Get("max_tokens"); max.Type != gjson.Number || max.Float() <= 0 || float64(max.Int()) != max.Float() {
			return fmt.Errorf("max_tokens must be a positive integer")
		}
		return validateAnthropic(root.Get("messages"))
	}
}

// Reject duplicate keys so validation and provider JSON parsers cannot disagree
// about which occurrence supplies identifiers or edited content.
func uniqueJSON(body string) error {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			keys := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || keys[name] {
					return fmt.Errorf("duplicate or invalid object key %q", key)
				}
				keys[name] = true
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

type toolPairs struct {
	pending map[string]bool
	seen    map[string]bool
	results map[string]bool
}

func newToolPairs() *toolPairs {
	return &toolPairs{pending: make(map[string]bool), seen: make(map[string]bool), results: make(map[string]bool)}
}

func (p *toolPairs) call(id string) error {
	if id == "" || p.seen[id] {
		return fmt.Errorf("tool call IDs must be non-empty and unique: %q", id)
	}
	p.seen[id], p.pending[id] = true, true
	return nil
}

func (p *toolPairs) result(id string, remote bool) error {
	if id == "" || p.results[id] || (!p.pending[id] && !remote) {
		return fmt.Errorf("tool result %q must match one preceding tool call exactly once", id)
	}
	p.results[id] = true
	delete(p.pending, id)
	return nil
}

func (p *toolPairs) complete() error {
	if len(p.pending) > 0 {
		return fmt.Errorf("each tool call needs a result before continuing the conversation")
	}
	return nil
}

func messagesArray(messages gjson.Result) error {
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return fmt.Errorf("messages must be a non-empty array")
	}
	return nil
}

func contentShape(content gjson.Result, allowNull bool) error {
	if content.Type == gjson.String || (allowNull && content.Type == gjson.Null) {
		return nil
	}
	if content.IsArray() {
		for _, block := range content.Array() {
			if !block.IsObject() || block.Get("type").Type != gjson.String || block.Get("type").String() == "" {
				return fmt.Errorf("content blocks need a non-empty type")
			}
		}
		return nil
	}
	return fmt.Errorf("message content must be text or an array of content blocks")
}

func validateChat(messages gjson.Result) error {
	if err := messagesArray(messages); err != nil {
		return err
	}
	pairs := newToolPairs()
	for _, message := range messages.Array() {
		role := message.Get("role").String()
		if !slices.Contains([]string{"system", "developer", "user", "assistant", "tool", "function"}, role) {
			return fmt.Errorf("invalid chat message role %q", role)
		}
		if err := contentShape(message.Get("content"), role == "assistant"); err != nil {
			return err
		}
		if role == "tool" || role == "function" {
			id := message.Get("tool_call_id").String()
			if role == "function" {
				id = "function:" + message.Get("name").String()
			}
			if err := pairs.result(id, false); err != nil {
				return err
			}
			continue
		}
		if err := pairs.complete(); err != nil {
			return err
		}
		calls := message.Get("tool_calls")
		if calls.Exists() && calls.Type != gjson.Null {
			if role != "assistant" || !calls.IsArray() {
				return fmt.Errorf("tool_calls must be an array on an assistant message")
			}
			for _, call := range calls.Array() {
				switch call.Get("type").String() {
				case "function":
					if call.Get("function.name").String() == "" || call.Get("function.arguments").Type != gjson.String {
						return fmt.Errorf("function tool calls need a name and string arguments")
					}
				case "custom":
					if call.Get("custom.name").String() == "" || call.Get("custom.input").Type != gjson.String {
						return fmt.Errorf("custom tool calls need a name and string input")
					}
				default:
					return fmt.Errorf("unsupported chat tool call type")
				}
				if err := pairs.call(call.Get("id").String()); err != nil {
					return err
				}
			}
		}
		if call := message.Get("function_call"); call.Exists() && call.Type != gjson.Null {
			if role != "assistant" || call.Get("name").String() == "" || call.Get("arguments").Type != gjson.String {
				return fmt.Errorf("function_call needs an assistant role, name and string arguments")
			}
			id := "function:" + call.Get("name").String()
			// Legacy function calls have names rather than unique IDs. A name
			// may be called again after the preceding call has been answered.
			delete(pairs.seen, id)
			delete(pairs.results, id)
			if err := pairs.call(id); err != nil {
				return err
			}
		}
	}
	return pairs.complete()
}

func validateResponses(root gjson.Result) error {
	input := root.Get("input")
	nonEmptyString := func(value gjson.Result) bool {
		return value.Type == gjson.String && strings.TrimSpace(value.String()) != ""
	}
	remote := nonEmptyString(root.Get("previous_response_id")) || nonEmptyString(root.Get("conversation")) || nonEmptyString(root.Get("conversation.id"))
	if !input.Exists() && (remote || root.Get("prompt").IsObject()) {
		return nil
	}
	if input.Type == gjson.String {
		return nil
	}
	if !input.IsArray() {
		return fmt.Errorf("Responses input must be a string or array")
	}
	pairs := newToolPairs()
	for _, item := range input.Array() {
		if !item.IsObject() {
			return fmt.Errorf("Responses input items must be objects")
		}
		switch item.Get("type").String() {
		case "function_call", "custom_tool_call":
			if item.Get("name").String() == "" {
				return fmt.Errorf("tool calls need a name")
			}
			if err := pairs.call(item.Get("call_id").String()); err != nil {
				return err
			}
		case "function_call_output", "custom_tool_call_output":
			if !item.Get("output").Exists() {
				return fmt.Errorf("tool results need output")
			}
			if err := pairs.result(item.Get("call_id").String(), remote); err != nil {
				return err
			}
		case "", "message":
			if !slices.Contains([]string{"user", "system", "developer", "assistant"}, item.Get("role").String()) {
				return fmt.Errorf("invalid Responses message role")
			}
			if err := contentShape(item.Get("content"), false); err != nil {
				return err
			}
		}
	}
	return pairs.complete()
}

func validateAnthropic(messages gjson.Result) error {
	if err := messagesArray(messages); err != nil {
		return err
	}
	pairs := newToolPairs()
	for _, message := range messages.Array() {
		role := message.Get("role").String()
		if role != "user" && role != "assistant" {
			return fmt.Errorf("Anthropic messages require user or assistant roles")
		}
		content := message.Get("content")
		if err := contentShape(content, false); err != nil {
			return err
		}
		if role == "assistant" {
			if err := pairs.complete(); err != nil {
				return err
			}
		}
		for _, block := range content.Array() {
			switch block.Get("type").String() {
			case "tool_use":
				if role != "assistant" || block.Get("name").String() == "" || !block.Get("input").IsObject() {
					return fmt.Errorf("tool_use needs assistant role, name and object input")
				}
				if err := pairs.call(block.Get("id").String()); err != nil {
					return err
				}
			case "tool_result":
				if role != "user" {
					return fmt.Errorf("tool_result must appear in a user message")
				}
				if err := pairs.result(block.Get("tool_use_id").String(), false); err != nil {
					return err
				}
			default:
				if role == "user" {
					if err := pairs.complete(); err != nil {
						return err
					}
				}
			}
		}
		if role == "user" {
			if err := pairs.complete(); err != nil {
				return err
			}
		}
	}
	return pairs.complete()
}
