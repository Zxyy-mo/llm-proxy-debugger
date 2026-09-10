// Package contextdiff compares only context actually present in captured requests.
package contextdiff

import (
	"bytes"
	"encoding/json"
	"fmt"
)

type Document struct {
	Messages []json.RawMessage
	System   json.RawMessage
	Tools    json.RawMessage
	Remote   bool
}

type Change struct {
	Kind        string `json:"kind"`
	BeforeIndex *int   `json:"before_index,omitempty"`
	AfterIndex  *int   `json:"after_index,omitempty"`
	Content     string `json:"content"`
}

type Field struct {
	Changed bool   `json:"changed"`
	Before  string `json:"before"`
	After   string `json:"after"`
}
type Result struct {
	Messages      []Change `json:"messages"`
	System        Field    `json:"system"`
	Tools         Field    `json:"tools"`
	Added         int      `json:"added"`
	Removed       int      `json:"removed"`
	Unchanged     int      `json:"unchanged"`
	Limited       bool     `json:"limited"`
	RemoteContext bool     `json:"remote_context"`
}

func canonical(value any) json.RawMessage {
	if value == nil {
		return json.RawMessage("null")
	}
	raw, _ := json.Marshal(value)
	return raw
}

func Parse(body []byte) (Document, error) {
	var root map[string]any
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if d.Decode(&root) != nil || root == nil {
		return Document{}, fmt.Errorf("上下文不是可比较的 JSON 对象")
	}
	doc := Document{Messages: []json.RawMessage{}, System: canonical(root["system"]), Tools: canonical(root["tools"]), Remote: root["previous_response_id"] != nil || root["conversation"] != nil}
	if instructions, ok := root["instructions"]; ok {
		doc.System = canonical(instructions)
	}
	var messages []any
	if value, ok := root["messages"].([]any); ok {
		messages = value
	} else {
		switch input := root["input"].(type) {
		case string:
			messages = []any{map[string]any{"role": "user", "content": input}}
		case []any:
			messages = input
		}
	}
	var system []any
	for _, message := range messages {
		if item, ok := message.(map[string]any); ok {
			role, _ := item["role"].(string)
			if role == "system" || role == "developer" {
				system = append(system, item)
				continue
			}
			if role != "" {
				if item["type"] == "message" {
					delete(item, "type")
				}
				switch content := item["content"].(type) {
				case string:
					item["content"] = []any{map[string]any{"type": "text", "text": content}}
				case []any:
					for _, block := range content {
						if part, ok := block.(map[string]any); ok && (part["type"] == "input_text" || part["type"] == "output_text") {
							part["type"] = "text"
						}
					}
				}
			}
		}
		doc.Messages = append(doc.Messages, canonical(message))
	}
	if len(system) > 0 {
		doc.System = canonical(system)
	}
	return doc, nil
}

func Compare(before, after Document) Result {
	result := Result{Messages: []Change{}, System: Field{!bytes.Equal(before.System, after.System), string(before.System), string(after.System)}, Tools: Field{!bytes.Equal(before.Tools, after.Tools), string(before.Tools), string(after.Tools)}, RemoteContext: before.Remote || after.Remote}
	a, b := before.Messages, after.Messages
	add := func(kind string, i, j int, raw json.RawMessage) {
		c := Change{Kind: kind, Content: string(raw)}
		if i >= 0 {
			index := i
			c.BeforeIndex = &index
		}
		if j >= 0 {
			index := j
			c.AfterIndex = &index
		}
		result.Messages = append(result.Messages, c)
		switch kind {
		case "added":
			result.Added++
		case "removed":
			result.Removed++
		default:
			result.Unchanged++
		}
	}
	// Bound the alignment matrix. Large histories still compare exact prefixes
	// and suffixes, with the unaligned interval explicitly marked as limited.
	if int64(len(a)+1)*int64(len(b)+1) > 1000000 {
		result.Limited = true
		prefix := 0
		for prefix < len(a) && prefix < len(b) && bytes.Equal(a[prefix], b[prefix]) {
			add("unchanged", prefix, prefix, a[prefix])
			prefix++
		}
		i, j := len(a)-1, len(b)-1
		for i >= prefix && j >= prefix && bytes.Equal(a[i], b[j]) {
			i--
			j--
		}
		for k := prefix; k <= i; k++ {
			add("removed", k, -1, a[k])
		}
		for k := prefix; k <= j; k++ {
			add("added", -1, k, b[k])
		}
		for i, j = i+1, j+1; i < len(a) && j < len(b); i, j = i+1, j+1 {
			add("unchanged", i, j, a[i])
		}
		return result
	}
	width := len(b) + 1
	dp := make([]int, (len(a)+1)*width)
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if bytes.Equal(a[i], b[j]) {
				dp[i*width+j] = 1 + dp[(i+1)*width+j+1]
			} else {
				dp[i*width+j] = max(dp[(i+1)*width+j], dp[i*width+j+1])
			}
		}
	}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		if i < len(a) && j < len(b) && bytes.Equal(a[i], b[j]) {
			add("unchanged", i, j, a[i])
			i++
			j++
		} else if j < len(b) && (i == len(a) || dp[i*width+j+1] >= dp[(i+1)*width+j]) {
			add("added", -1, j, b[j])
			j++
		} else {
			add("removed", i, -1, a[i])
			i++
		}
	}
	return result
}
