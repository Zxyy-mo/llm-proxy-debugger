// Package adapter implements an explicit, bounded text/function-tool bridge to
// Chat Completions. Native passthrough never enters this package's conversion.
package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

type Object = map[string]any

type Plan struct {
	Source string
	Stream bool
	ID     string
	Model  string
}

func decode(body []byte) (Object, error) {
	var value Object
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	if !json.Valid(body) || d.Decode(&value) != nil || value == nil {
		return nil, fmt.Errorf("conversion requires a JSON object")
	}
	return value, nil
}

func object(v any) Object { o, _ := v.(map[string]any); return o }
func array(v any) []any   { a, _ := v.([]any); return a }
func str(v any) string    { s, _ := v.(string); return s }
func boolean(v any) bool  { b, _ := v.(bool); return b }

func fields(o Object, allowed string) error {
	for key, value := range o {
		if value != nil && !strings.Contains(" "+allowed+" ", " "+key+" ") {
			return fmt.Errorf("field %q is not supported by text/function conversion; use a native passthrough provider", key)
		}
	}
	return nil
}

// Request returns the actual upstream path and body, plus a response converter.
// An alias-only request changes just the model field; all JSON numbers retain
// their original decimal precision through json.Number.
func Request(path string, body []byte, mode, alias string) (string, []byte, *Plan, error) {
	convert := mode == "openai" && !strings.HasSuffix(strings.TrimSuffix(path, "/"), "/chat/completions")
	if !convert && alias == "" {
		return path, body, nil, nil
	}
	root, err := decode(body)
	if err != nil {
		return "", nil, nil, err
	}
	if alias != "" {
		root["model"] = alias
	}
	if !convert {
		raw, err := json.Marshal(root)
		return path, raw, nil, err
	}
	source := ""
	switch {
	case strings.HasSuffix(strings.TrimSuffix(path, "/"), "/messages"):
		source = "anthropic"
	case strings.HasSuffix(strings.TrimSuffix(path, "/"), "/responses"):
		source = "responses"
	default:
		return "", nil, nil, fmt.Errorf("only /messages and /responses can be converted to Chat Completions")
	}
	if str(root["model"]) == "" {
		return "", nil, nil, fmt.Errorf("model is required")
	}
	out := Object{"model": root["model"]}
	for _, key := range []string{"temperature", "top_p", "stream", "metadata"} {
		if v, ok := root[key]; ok {
			out[key] = v
		}
	}
	var messages []any
	if source == "anthropic" {
		messages, err = anthropicRequest(root, out)
	} else {
		messages, err = responsesRequest(root, out)
	}
	if err != nil {
		return "", nil, nil, err
	}
	if len(messages) == 0 {
		return "", nil, nil, fmt.Errorf("conversion requires at least one message")
	}
	out["messages"] = messages
	if boolean(root["stream"]) {
		out["stream_options"] = Object{"include_usage": true}
	}
	raw, err := json.Marshal(out)
	prefix := "msg_gateway_"
	if source == "responses" {
		prefix = "resp_gateway_"
	}
	return "/v1/chat/completions", raw, &Plan{Source: source, Stream: boolean(root["stream"]), Model: str(root["model"]), ID: prefix + strings.ReplaceAll(uuid.NewString(), "-", "")}, err
}

func textContent(value any, kinds string) (string, error) {
	if value == nil {
		return "", nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := value.([]any)
	if !ok {
		return "", fmt.Errorf("content must be text or an array of text blocks")
	}
	var result strings.Builder
	for _, v := range parts {
		p := object(v)
		if p == nil || !strings.Contains(" "+kinds+" ", " "+str(p["type"])+" ") || str(p["type"]) == "" {
			return "", fmt.Errorf("content block %q is unsupported; images, audio, files and signed reasoning require native passthrough", str(p["type"]))
		}
		if err := fields(p, "type text annotations"); err != nil {
			return "", err
		}
		if len(array(p["annotations"])) > 0 {
			return "", fmt.Errorf("annotated text requires native passthrough")
		}
		text, ok := p["text"].(string)
		if !ok {
			return "", fmt.Errorf("text block needs a text string")
		}
		result.WriteString(text)
	}
	return result.String(), nil
}

func anthropicRequest(root, out Object) ([]any, error) {
	if err := fields(root, "model messages system max_tokens temperature top_p stop_sequences stream tools tool_choice metadata"); err != nil {
		return nil, err
	}
	for from, to := range map[string]string{"max_tokens": "max_tokens", "stop_sequences": "stop"} {
		if v, ok := root[from]; ok {
			out[to] = v
		}
	}
	var messages []any
	if root["system"] != nil {
		text, err := textContent(root["system"], "text")
		if err != nil {
			return nil, err
		}
		messages = append(messages, Object{"role": "system", "content": text})
	}
	items, ok := root["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("messages must be an array")
	}
	for _, item := range items {
		m := object(item)
		if err := fields(m, "role content"); err != nil {
			return nil, err
		}
		role := str(m["role"])
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("invalid Anthropic message role")
		}
		if text, ok := m["content"].(string); ok {
			messages = append(messages, Object{"role": role, "content": text})
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			return nil, fmt.Errorf("message content must be text or blocks")
		}
		current := Object{"role": role, "content": ""}
		flush := func() {
			if str(current["content"]) != "" || len(array(current["tool_calls"])) > 0 {
				messages = append(messages, current)
				current = Object{"role": role, "content": ""}
			}
		}
		for _, part := range parts {
			p := object(part)
			switch str(p["type"]) {
			case "text":
				text, err := textContent([]any{p}, "text")
				if err != nil {
					return nil, err
				}
				current["content"] = str(current["content"]) + text
			case "tool_use":
				if role != "assistant" || str(p["id"]) == "" || str(p["name"]) == "" || object(p["input"]) == nil {
					return nil, fmt.Errorf("tool_use needs an assistant role, id, name and object input")
				}
				if err := fields(p, "type id name input"); err != nil {
					return nil, err
				}
				args, _ := json.Marshal(p["input"])
				current["tool_calls"] = append(array(current["tool_calls"]), Object{"id": p["id"], "type": "function", "function": Object{"name": p["name"], "arguments": string(args)}})
			case "tool_result":
				if role != "user" || str(p["tool_use_id"]) == "" {
					return nil, fmt.Errorf("tool_result needs a user role and tool_use_id")
				}
				if err := fields(p, "type tool_use_id content is_error"); err != nil {
					return nil, err
				}
				if boolean(p["is_error"]) {
					return nil, fmt.Errorf("Anthropic is_error tool results require native passthrough")
				}
				text, err := textContent(p["content"], "text")
				if err != nil {
					return nil, err
				}
				flush()
				messages = append(messages, Object{"role": "tool", "tool_call_id": p["tool_use_id"], "content": text})
			default:
				return nil, fmt.Errorf("Anthropic content block %q requires native passthrough", str(p["type"]))
			}
		}
		flush()
	}
	if root["tools"] != nil {
		if _, ok := root["tools"].([]any); !ok {
			return nil, fmt.Errorf("tools must be an array")
		}
		var tools []any
		for _, v := range array(root["tools"]) {
			tool := object(v)
			if err := fields(tool, "name description input_schema type"); err != nil {
				return nil, err
			}
			if str(tool["type"]) != "" && str(tool["type"]) != "custom" {
				return nil, fmt.Errorf("only function tools can be converted")
			}
			if str(tool["name"]) == "" || object(tool["input_schema"]) == nil {
				return nil, fmt.Errorf("tool needs name and input_schema")
			}
			function := Object{"name": tool["name"], "parameters": tool["input_schema"]}
			if tool["description"] != nil {
				function["description"] = tool["description"]
			}
			tools = append(tools, Object{"type": "function", "function": function})
		}
		out["tools"] = tools
	}
	if root["tool_choice"] != nil {
		choice := object(root["tool_choice"])
		if err := fields(choice, "type name disable_parallel_tool_use"); err != nil {
			return nil, err
		}
		switch str(choice["type"]) {
		case "auto", "none":
			out["tool_choice"] = choice["type"]
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			if str(choice["name"]) == "" {
				return nil, fmt.Errorf("tool_choice name is required")
			}
			out["tool_choice"] = Object{"type": "function", "function": Object{"name": choice["name"]}}
		default:
			return nil, fmt.Errorf("unsupported tool_choice")
		}
		if v, ok := choice["disable_parallel_tool_use"]; ok {
			out["parallel_tool_calls"] = !boolean(v)
		}
	}
	return messages, nil
}

func responsesRequest(root, out Object) ([]any, error) {
	if err := fields(root, "model input instructions max_output_tokens temperature top_p stream tools tool_choice parallel_tool_calls metadata store background previous_response_id conversation text"); err != nil {
		return nil, err
	}
	if str(root["previous_response_id"]) != "" || root["conversation"] != nil || boolean(root["store"]) || boolean(root["background"]) {
		return nil, fmt.Errorf("server-side history, store:true and background execution require native Responses passthrough")
	}
	for from, to := range map[string]string{"max_output_tokens": "max_completion_tokens", "parallel_tool_calls": "parallel_tool_calls"} {
		if v, ok := root[from]; ok {
			out[to] = v
		}
	}
	if root["text"] != nil {
		text := object(root["text"])
		if err := fields(text, "format"); err != nil {
			return nil, err
		}
		format := object(text["format"])
		switch str(format["type"]) {
		case "text":
			if err := fields(format, "type"); err != nil {
				return nil, err
			}
		case "json_object":
			if err := fields(format, "type"); err != nil {
				return nil, err
			}
			out["response_format"] = format
		case "json_schema":
			if err := fields(format, "type name description schema strict"); err != nil {
				return nil, err
			}
			schema := Object{}
			for k, v := range format {
				if k != "type" {
					schema[k] = v
				}
			}
			out["response_format"] = Object{"type": "json_schema", "json_schema": schema}
		default:
			return nil, fmt.Errorf("unsupported Responses text format")
		}
	}
	var messages []any
	if root["instructions"] != nil {
		instructions, ok := root["instructions"].(string)
		if !ok {
			return nil, fmt.Errorf("instructions must be text")
		}
		messages = append(messages, Object{"role": "system", "content": instructions})
	}
	if input, ok := root["input"].(string); ok {
		messages = append(messages, Object{"role": "user", "content": input})
	} else {
		input, ok := root["input"].([]any)
		if !ok {
			return nil, fmt.Errorf("Responses input must be text or an array")
		}
		for _, v := range input {
			item := object(v)
			kind := str(item["type"])
			switch kind {
			case "", "message":
				if err := fields(item, "type role content id status"); err != nil {
					return nil, err
				}
				role := str(item["role"])
				if role != "user" && role != "assistant" && role != "system" && role != "developer" {
					return nil, fmt.Errorf("unsupported Responses role")
				}
				text, err := textContent(item["content"], "input_text output_text")
				if err != nil {
					return nil, err
				}
				messages = append(messages, Object{"role": role, "content": text})
			case "function_call":
				if err := fields(item, "type id status call_id name arguments"); err != nil {
					return nil, err
				}
				if str(item["call_id"]) == "" || str(item["name"]) == "" || !json.Valid([]byte(str(item["arguments"]))) {
					return nil, fmt.Errorf("function_call needs call_id, name and JSON arguments")
				}
				call := Object{"id": item["call_id"], "type": "function", "function": Object{"name": item["name"], "arguments": item["arguments"]}}
				if len(messages) > 0 && str(object(messages[len(messages)-1])["role"]) == "assistant" {
					last := object(messages[len(messages)-1])
					last["tool_calls"] = append(array(last["tool_calls"]), call)
				} else {
					messages = append(messages, Object{"role": "assistant", "content": nil, "tool_calls": []any{call}})
				}
			case "function_call_output":
				if err := fields(item, "type id status call_id output"); err != nil {
					return nil, err
				}
				output, ok := item["output"].(string)
				if !ok || str(item["call_id"]) == "" {
					return nil, fmt.Errorf("function_call_output needs call_id and text output")
				}
				messages = append(messages, Object{"role": "tool", "tool_call_id": item["call_id"], "content": output})
			default:
				return nil, fmt.Errorf("Responses input item %q requires native passthrough", kind)
			}
		}
	}
	if root["tools"] != nil {
		if _, ok := root["tools"].([]any); !ok {
			return nil, fmt.Errorf("tools must be an array")
		}
		var tools []any
		for _, v := range array(root["tools"]) {
			tool := object(v)
			if err := fields(tool, "type name description parameters strict"); err != nil {
				return nil, err
			}
			if str(tool["type"]) != "function" || str(tool["name"]) == "" {
				return nil, fmt.Errorf("only named function tools can be converted")
			}
			function := Object{}
			for k, v := range tool {
				if k != "type" {
					function[k] = v
				}
			}
			tools = append(tools, Object{"type": "function", "function": function})
		}
		out["tools"] = tools
	}
	if v := root["tool_choice"]; v != nil {
		if choice, ok := v.(string); ok && (choice == "auto" || choice == "none" || choice == "required") {
			out["tool_choice"] = choice
		} else {
			choice := object(v)
			if err := fields(choice, "type name"); err != nil {
				return nil, err
			}
			if str(choice["type"]) != "function" || str(choice["name"]) == "" {
				return nil, fmt.Errorf("unsupported Responses tool_choice")
			}
			out["tool_choice"] = Object{"type": "function", "function": Object{"name": choice["name"]}}
		}
	}
	return messages, nil
}
