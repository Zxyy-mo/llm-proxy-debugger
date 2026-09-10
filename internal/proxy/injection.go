package proxy

import (
	"encoding/json"
	"strings"
)

// injectPrompt edits only the protocol's instruction field. RawMessage keeps
// unrelated numbers and payload fields byte-accurate through re-serialization.
func injectPrompt(path string, body []byte, text string) []byte {
	var payload map[string]json.RawMessage
	if text == "" || json.Unmarshal(body, &payload) != nil || payload == nil {
		return body
	}
	appendText := func(raw json.RawMessage) (json.RawMessage, bool) {
		var value string
		if len(raw) == 0 || string(raw) == "null" {
			encoded, _ := json.Marshal(text)
			return encoded, true
		}
		if json.Unmarshal(raw, &value) == nil {
			encoded, _ := json.Marshal(value + "\n\n" + text)
			return encoded, true
		}
		var blocks []json.RawMessage
		if json.Unmarshal(raw, &blocks) != nil {
			return nil, false
		}
		block, _ := json.Marshal(map[string]string{"type": "text", "text": text})
		encoded, _ := json.Marshal(append(blocks, block))
		return encoded, true
	}
	path = strings.TrimSuffix(path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		var messages []map[string]json.RawMessage
		if json.Unmarshal(payload["messages"], &messages) != nil {
			return body
		}
		found := false
		for _, message := range messages {
			var role string
			_ = json.Unmarshal(message["role"], &role)
			if role == "system" || role == "developer" {
				value, ok := appendText(message["content"])
				if !ok {
					return body
				}
				message["content"] = value
				found = true
				break
			}
		}
		if !found {
			value, _ := json.Marshal(text)
			messages = append([]map[string]json.RawMessage{{"role": json.RawMessage(`"system"`), "content": value}}, messages...)
		}
		payload["messages"], _ = json.Marshal(messages)
	case strings.HasSuffix(path, "/responses"):
		var value string
		if raw := payload["instructions"]; len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &value) != nil {
			return body
		}
		if value != "" {
			value += "\n\n"
		}
		payload["instructions"], _ = json.Marshal(value + text)
	case strings.HasSuffix(path, "/messages"):
		value, ok := appendText(payload["system"])
		if !ok {
			return body
		}
		payload["system"] = value
	default:
		return body
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}
