// Package observation extracts protocol-visible tool calls without pretending
// that a model request observed the tool's execution time.
package observation

import (
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type ToolCall struct {
	ID          string   `json:"id"`
	CallID      string   `json:"call_id"`
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Input       string   `json:"input"`
	Output      string   `json:"output,omitempty"`
	Status      string   `json:"status"`
	Source      string   `json:"source"`
	ResultTrace string   `json:"result_trace,omitempty"`
	SpanID      string   `json:"span_id,omitempty"`
	StartedAt   string   `json:"started_at,omitempty"`
	EndedAt     string   `json:"ended_at,omitempty"`
	Duration    *float64 `json:"duration_ms,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type Collector struct {
	protocol string
	calls    map[string]*ToolCall
	order    []string
}

func New(protocol string) *Collector {
	return &Collector{protocol: protocol, calls: map[string]*ToolCall{}}
}

func (c *Collector) get(key string) *ToolCall {
	if call := c.calls[key]; call != nil {
		return call
	}
	if len(c.calls) >= 512 {
		return &ToolCall{}
	}
	call := &ToolCall{ID: key, CallID: key, Status: "requested", Source: "llm", Kind: "function"}
	c.calls[key] = call
	c.order = append(c.order, key)
	return call
}
func bounded(value string) (string, bool) {
	if len(value) <= 65536 {
		return value, false
	}
	end := 65536
	for end > 0 && value[end]&0xc0 == 0x80 {
		end--
	}
	return value[:end] + "…", true
}

func (c *Collector) Feed(data []byte) {
	if !gjson.ValidBytes(data) {
		return
	}
	root := gjson.ParseBytes(data)
	switch c.protocol {
	case "openai":
		part := root.Get("choices.0.delta")
		delta := part.Exists()
		if !delta {
			part = root.Get("choices.0.message")
		}
		for i, item := range part.Get("tool_calls").Array() {
			index := i
			if item.Get("index").Exists() {
				index = int(item.Get("index").Int())
			}
			call := c.get(strconv.Itoa(index))
			if id := item.Get("id").String(); id != "" {
				call.ID, call.CallID = id, id
			}
			if name := item.Get("function.name").String(); name != "" {
				call.Name = name
			}
			args := item.Get("function.arguments").String()
			if delta {
				args = call.Input + args
			}
			call.Input, call.Truncated = bounded(args)
		}
	case "anthropic":
		switch root.Get("type").String() {
		case "message":
			for i, item := range root.Get("content").Array() {
				if item.Get("type").String() == "tool_use" {
					call := c.get(strconv.Itoa(i))
					call.ID, call.CallID, call.Name = item.Get("id").String(), item.Get("id").String(), item.Get("name").String()
					call.Input, call.Truncated = bounded(item.Get("input").Raw)
				}
			}
		case "content_block_start":
			item := root.Get("content_block")
			if item.Get("type").String() == "tool_use" {
				call := c.get(root.Get("index").Raw)
				call.ID, call.CallID, call.Name = item.Get("id").String(), item.Get("id").String(), item.Get("name").String()
				if item.Get("input").Raw != "{}" {
					call.Input = item.Get("input").Raw
				}
			}
		case "content_block_delta":
			if root.Get("delta.type").String() == "input_json_delta" {
				call := c.get(root.Get("index").Raw)
				call.Input, call.Truncated = bounded(call.Input + root.Get("delta.partial_json").String())
			}
		}
	case "responses":
		kind := root.Get("type").String()
		switch kind {
		case "response.output_item.added", "response.output_item.done":
			c.item(root.Get("item"))
		case "response.function_call_arguments.delta":
			call := c.get(root.Get("item_id").String())
			call.Input, call.Truncated = bounded(call.Input + root.Get("delta").String())
		case "response.function_call_arguments.done":
			call := c.get(root.Get("item_id").String())
			call.Input, call.Truncated = bounded(root.Get("arguments").String())
		case "response.completed", "response.incomplete", "response.failed":
			for _, item := range root.Get("response.output").Array() {
				c.item(item)
			}
		default:
			for _, item := range root.Get("output").Array() {
				c.item(item)
			}
		}
	}
}

func (c *Collector) item(item gjson.Result) {
	kind := item.Get("type").String()
	if !strings.HasSuffix(kind, "_call") {
		return
	}
	key := item.Get("id").String()
	if key == "" {
		key = item.Get("call_id").String()
	}
	if key == "" {
		return
	}
	call := c.get(key)
	call.ID = key
	call.CallID = item.Get("call_id").String()
	if call.CallID == "" {
		call.CallID = key
	}
	call.Name = item.Get("name").String()
	if call.Name == "" {
		call.Name = kind
	}
	call.Kind = kind
	if args := item.Get("arguments"); args.Exists() {
		call.Input, call.Truncated = bounded(args.String())
	} else if input := item.Get("input"); input.Exists() {
		call.Input, call.Truncated = bounded(input.Raw)
	}
	if kind != "function_call" && item.Get("status").String() == "completed" {
		call.Status = "result_observed"
		call.Source = "provider"
	}
}

func (c *Collector) Calls() []ToolCall {
	calls := make([]ToolCall, 0, len(c.calls))
	for _, key := range c.order {
		calls = append(calls, *c.calls[key])
	}
	return calls
}

type ToolResult struct {
	CallID, Output string
	Error          bool
}

func Results(body []byte) []ToolResult {
	results := []ToolResult{}
	root := gjson.ParseBytes(body)
	content := func(value gjson.Result) string {
		if value.Type == gjson.String {
			return value.String()
		}
		return value.Raw
	}
	for _, message := range root.Get("messages").Array() {
		if message.Get("role").String() == "tool" {
			results = append(results, ToolResult{CallID: message.Get("tool_call_id").String(), Output: content(message.Get("content"))})
		}
		for _, block := range message.Get("content").Array() {
			if block.Get("type").String() == "tool_result" {
				results = append(results, ToolResult{CallID: block.Get("tool_use_id").String(), Output: content(block.Get("content")), Error: block.Get("is_error").Bool()})
			}
		}
	}
	for _, item := range root.Get("input").Array() {
		if strings.HasSuffix(item.Get("type").String(), "call_output") {
			results = append(results, ToolResult{CallID: item.Get("call_id").String(), Output: content(item.Get("output"))})
		}
	}
	for i := range results {
		results[i].Output, _ = bounded(results[i].Output)
	}
	return results
}
