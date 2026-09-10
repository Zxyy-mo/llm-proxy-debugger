package correlation

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

type Response struct {
	ID             string
	ConversationID string
	Model          string
	Messages       []string
	Complete       bool
	Error          string
	Recognized     bool
}

type streamBlock struct {
	Kind      string
	ID        string
	Name      string
	Text      strings.Builder
	Arguments strings.Builder
	Input     any
}

// ResponseCapture retains bounded output solely for history fingerprinting.
// Explicit IDs continue to be captured even after the history limit is reached.
type ResponseCapture struct {
	protocol string
	metadata Response
	bytes    int
	text     strings.Builder
	blocks   map[int]*streamBlock
	items    map[int]string
	output   []string
}

func NewResponseCapture(protocol string) *ResponseCapture {
	return &ResponseCapture{
		protocol: protocol, blocks: make(map[int]*streamBlock), items: make(map[int]string),
	}
}

func (c *ResponseCapture) Metadata() Response {
	return c.metadata
}

func (c *ResponseCapture) captureMetadata(root gjson.Result) bool {
	beforeID, beforeConversation := c.metadata.ID, c.metadata.ConversationID
	if id := boundedID(stringValue(root.Get("id"))); id != "" {
		c.metadata.ID = id
	}
	if model := stringValue(root.Get("model")); model != "" {
		c.metadata.Model = model
	}
	conversation := root.Get("conversation")
	if conversation.IsObject() {
		conversation = conversation.Get("id")
	}
	if id := boundedID(stringValue(conversation)); id != "" {
		c.metadata.ConversationID = id
	}
	return beforeID != c.metadata.ID || beforeConversation != c.metadata.ConversationID
}

func (c *ResponseCapture) captureOutput(root gjson.Result) {
	c.output = nil
	var items []gjson.Result
	switch c.protocol {
	case "openai":
		if message := root.Get("choices.0.message"); message.IsObject() {
			items = append(items, message)
		}
	case "responses":
		items = root.Get("output").Array()
	default:
		if root.Get("role").String() == "assistant" {
			items = append(items, root)
		}
	}
	for _, item := range items {
		if fp := fingerprintItem(item); fp != "" {
			c.output = append(c.output, fp)
		}
	}
}

func (c *ResponseCapture) JSON(data []byte) {
	if !gjson.ValidBytes(data) {
		return
	}
	root := gjson.ParseBytes(data)
	if root.Get("choices").IsArray() || root.Get("output").IsArray() || root.Get("type").String() == "message" {
		c.metadata.Recognized = true
		c.captureMetadata(root)
	}
	c.metadata.Complete = true
	c.captureError(root)
	if len(data) <= MaxHistoryBytes {
		c.captureOutput(root)
	}
}

func (c *ResponseCapture) captureError(root gjson.Result) {
	if message := root.Get("error.message").String(); message != "" {
		c.metadata.Error = message
	} else if root.Get("status").String() == "incomplete" {
		c.metadata.Error = "Incomplete response: " + root.Get("incomplete_details.reason").String()
	} else if root.Get("status").String() == "failed" {
		c.metadata.Error = "Upstream response failed"
	}
}

// Event returns true when identifiers change, so pending children can be linked
// while a parent is still streaming, not just when it finishes.
func (c *ResponseCapture) Event(data []byte) bool {
	if strings.TrimSpace(string(data)) == "[DONE]" {
		c.metadata.Complete = true
		return false
	}
	if !gjson.ValidBytes(data) {
		return false
	}
	root := gjson.ParseBytes(data)
	kind := root.Get("type").String()
	changed := false
	switch c.protocol {
	case "openai":
		if root.Get("choices").IsArray() {
			c.metadata.Recognized = true
			changed = c.captureMetadata(root)
		}
		if finish := root.Get("choices.0.finish_reason"); finish.Type == gjson.String && finish.String() != "" {
			c.metadata.Complete = true
		}
	case "responses":
		if strings.HasPrefix(kind, "response.") {
			c.metadata.Recognized = true
		}
		if response := root.Get("response"); response.IsObject() {
			changed = c.captureMetadata(response)
			c.captureError(response)
		}
		if kind == "response.completed" || kind == "response.incomplete" || kind == "response.failed" {
			c.metadata.Complete = true
			if kind != "response.completed" && c.metadata.Error == "" {
				c.metadata.Error = kind
			}
		}
	default:
		if kind == "message_start" || kind == "message_delta" || kind == "content_block_delta" || kind == "content_block_start" {
			c.metadata.Recognized = true
		}
		if kind == "message_start" {
			changed = c.captureMetadata(root.Get("message"))
		}
		if kind == "message_stop" {
			c.metadata.Complete = true
		}
	}
	if kind == "error" || root.Get("error").Exists() {
		c.captureError(root)
		if c.metadata.Error == "" {
			c.metadata.Error = "Upstream stream error"
		}
	}
	c.bytes += len(data)
	if c.bytes > MaxHistoryBytes {
		c.output = nil
		c.items = nil
		c.blocks = nil
		c.text.Reset()
		return changed
	}

	switch c.protocol {
	case "openai":
		delta := root.Get("choices.0.delta")
		c.text.WriteString(stringValue(delta.Get("content")))
		for _, call := range delta.Get("tool_calls").Array() {
			block := c.block(int(call.Get("index").Int()))
			block.Kind = "tool_use"
			if id := call.Get("id").String(); id != "" {
				block.ID = id
			}
			block.Name += stringValue(call.Get("function.name"))
			block.Arguments.WriteString(stringValue(call.Get("function.arguments")))
		}
	case "responses":
		switch kind {
		case "response.output_text.delta":
			c.text.WriteString(root.Get("delta").String())
		case "response.output_item.done":
			c.items[int(root.Get("output_index").Int())] = root.Get("item").Raw
		case "response.completed", "response.incomplete", "response.failed":
			c.captureOutput(root.Get("response"))
		}
	default:
		index := int(root.Get("index").Int())
		switch kind {
		case "content_block_start":
			part := root.Get("content_block")
			block := c.block(index)
			block.Kind, block.ID, block.Name = part.Get("type").String(), part.Get("id").String(), part.Get("name").String()
			block.Input = jsonValue(part.Get("input"))
			block.Text.WriteString(stringValue(part.Get("text")))
		case "content_block_delta":
			block := c.block(index)
			switch root.Get("delta.type").String() {
			case "text_delta", "text":
				block.Kind = "text"
				block.Text.WriteString(root.Get("delta.text").String())
			case "input_json_delta":
				block.Kind = "tool_use"
				block.Arguments.WriteString(root.Get("delta.partial_json").String())
			}
		}
	}
	return changed
}

func (c *ResponseCapture) block(index int) *streamBlock {
	block := c.blocks[index]
	if block == nil {
		block = &streamBlock{}
		c.blocks[index] = block
	}
	return block
}

func (c *ResponseCapture) Response() Response {
	result := c.metadata
	if c.bytes > MaxHistoryBytes {
		return result
	}
	if len(c.output) > 0 {
		result.Messages = append([]string(nil), c.output...)
		return result
	}
	if c.protocol == "responses" && len(c.items) > 0 {
		for _, index := range sortedKeys(c.items) {
			if fp := fingerprintItem(gjson.Parse(c.items[index])); fp != "" {
				result.Messages = append(result.Messages, fp)
			}
		}
		return result
	}
	message := map[string]any{"role": "assistant", "content": c.text.String()}
	var content, calls []any
	for _, index := range sortedKeys(c.blocks) {
		block := c.blocks[index]
		if c.protocol == "openai" {
			calls = append(calls, map[string]any{
				"id": block.ID, "type": "function",
				"function": map[string]any{"name": block.Name, "arguments": block.Arguments.String()},
			})
		} else if block.Kind == "text" {
			content = append(content, map[string]any{"type": "text", "text": block.Text.String()})
		} else if block.Kind == "tool_use" {
			input := block.Input
			if block.Arguments.Len() > 0 {
				decoder := json.NewDecoder(strings.NewReader(block.Arguments.String()))
				decoder.UseNumber()
				if err := decoder.Decode(&input); err != nil {
					return result // incomplete tool JSON is not a reliable history anchor
				}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": block.ID, "name": block.Name, "input": input})
		}
	}
	if c.protocol == "anthropic" {
		message["content"] = content
	} else if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	if c.text.Len() == 0 && len(content) == 0 && len(calls) == 0 {
		return result
	}
	encoded, _ := json.Marshal(message)
	result.Messages = []string{fingerprintItem(gjson.ParseBytes(encoded))}
	return result
}

func sortedKeys[T any](values map[int]T) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}
