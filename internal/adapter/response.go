package adapter

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/sse"
)

const maxConversionBytes = 64 << 20

type part struct {
	Kind, ID, CallID, Name, Value string
	Index                         int
	Started                       bool
}

type responseState struct {
	plan            *Plan
	model           string
	usage           Object
	parts           []*part
	tools           map[int]*part
	text, refusal   *part
	finish          string
	started, done   bool
	bytes, sequence int
	writer          io.Writer
}

func (p *Plan) newState(w io.Writer) *responseState {
	return &responseState{plan: p, model: p.Model, usage: Object{}, tools: map[int]*part{}, writer: w}
}

func (s *responseState) event(kind string, value Object) error {
	if s.writer == nil {
		return nil
	}
	value["type"] = kind
	if s.plan.Source == "responses" {
		value["sequence_number"] = s.sequence
		s.sequence++
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.writer, "event: %s\ndata: %s\n\n", kind, raw)
	return err
}

func (s *responseState) updateUsage(root Object) {
	usage := object(root["usage"])
	for from, to := range map[string]string{"prompt_tokens": "input_tokens", "completion_tokens": "output_tokens", "total_tokens": "total_tokens"} {
		if v, ok := usage[from]; ok && v != nil {
			s.usage[to] = v
		}
	}
	if details := object(usage["prompt_tokens_details"]); details != nil {
		s.usage["input_tokens_details"] = details
	}
	if details := object(usage["completion_tokens_details"]); details != nil {
		s.usage["output_tokens_details"] = details
	}
}

func (s *responseState) anthropicUsage() Object {
	usage := Object{}
	for _, key := range []string{"input_tokens", "output_tokens"} {
		if v, ok := s.usage[key]; ok {
			usage[key] = v
		}
	}
	if v, ok := object(s.usage["input_tokens_details"])["cached_tokens"]; ok {
		usage["cache_read_input_tokens"] = v
	}
	return usage
}

func (s *responseState) start() error {
	if s.started {
		return nil
	}
	s.started = true
	if s.plan.Source == "anthropic" {
		return s.event("message_start", Object{"message": Object{"id": s.plan.ID, "type": "message", "role": "assistant", "model": s.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": s.anthropicUsage()}})
	}
	return s.event("response.created", Object{"response": Object{"id": s.plan.ID, "object": "response", "status": "in_progress", "model": s.model, "output": []any{}}})
}

func (s *responseState) addPart(kind string) *part {
	p := &part{Kind: kind, Index: len(s.parts), ID: "item_" + s.plan.ID + "_" + strconv.Itoa(len(s.parts))}
	s.parts = append(s.parts, p)
	return p
}

func (s *responseState) startPart(p *part) error {
	if p.Started {
		return nil
	}
	p.Started = true
	if s.plan.Source == "anthropic" {
		block := Object{"type": "text", "text": ""}
		if p.Kind == "tool" {
			block = Object{"type": "tool_use", "id": p.CallID, "name": p.Name, "input": Object{}}
		}
		return s.event("content_block_start", Object{"index": p.Index, "content_block": block})
	}
	item := s.responseItem(p, false)
	if p.Kind != "tool" {
		item["content"] = []any{}
	}
	if err := s.event("response.output_item.added", Object{"output_index": p.Index, "item": item}); err != nil {
		return err
	}
	if p.Kind != "tool" {
		content := Object{"type": "output_text", "text": "", "annotations": []any{}}
		if p.Kind == "refusal" {
			content = Object{"type": "refusal", "refusal": ""}
		}
		return s.event("response.content_part.added", Object{"item_id": p.ID, "output_index": p.Index, "content_index": 0, "part": content})
	}
	return nil
}

func (s *responseState) delta(p *part, value string) error {
	if value == "" {
		return nil
	}
	s.bytes += len(value)
	if s.bytes > maxConversionBytes {
		return fmt.Errorf("converted response exceeds 64 MiB")
	}
	if err := s.startPart(p); err != nil {
		return err
	}
	p.Value += value
	if s.plan.Source == "anthropic" {
		delta := Object{"type": "text_delta", "text": value}
		if p.Kind == "tool" {
			delta = Object{"type": "input_json_delta", "partial_json": value}
		}
		return s.event("content_block_delta", Object{"index": p.Index, "delta": delta})
	}
	kind := "response.output_text.delta"
	if p.Kind == "tool" {
		kind = "response.function_call_arguments.delta"
	} else if p.Kind == "refusal" {
		kind = "response.refusal.delta"
	}
	v := Object{"item_id": p.ID, "output_index": p.Index, "delta": value}
	if p.Kind != "tool" {
		v["content_index"] = 0
	}
	return s.event(kind, v)
}

func (s *responseState) message(message Object, stream bool) error {
	if message == nil {
		return nil
	}
	// Signed thinking, audio and legacy function calls cannot be faithfully
	// represented by this bridge. The raw upstream variant remains inspectable.
	if str(message["reasoning_content"]) != "" || str(message["reasoning"]) != "" || message["audio"] != nil || message["function_call"] != nil {
		return fmt.Errorf("reasoning, audio or legacy function_call output requires native passthrough")
	}
	if message["content"] != nil {
		value, ok := message["content"].(string)
		if !ok {
			return fmt.Errorf("non-text Chat Completions output cannot be converted")
		}
		if value != "" {
			if s.text == nil {
				s.text = s.addPart("text")
			}
			if err := s.delta(s.text, value); err != nil {
				return err
			}
		}
	}
	if value := str(message["refusal"]); value != "" {
		if s.refusal == nil {
			s.refusal = s.addPart("refusal")
		}
		if err := s.delta(s.refusal, value); err != nil {
			return err
		}
	}
	for ordinal, v := range array(message["tool_calls"]) {
		call := object(v)
		index := ordinal
		if stream {
			number, ok := call["index"].(json.Number)
			if !ok {
				return fmt.Errorf("streamed tool call is missing index")
			}
			n, err := strconv.Atoi(string(number))
			if err != nil || n < 0 || n > 511 {
				return fmt.Errorf("invalid tool index")
			}
			index = n
		}
		if kind := str(call["type"]); kind != "" && kind != "function" {
			return fmt.Errorf("only function tool output can be converted")
		}
		p := s.tools[index]
		if p == nil {
			p = s.addPart("tool")
			s.tools[index] = p
		}
		if id := str(call["id"]); id != "" {
			if p.Started && id != p.CallID {
				return fmt.Errorf("tool call ID changed after arguments started")
			}
			p.CallID = id
		}
		function := object(call["function"])
		if name := str(function["name"]); name != "" {
			if p.Started {
				return fmt.Errorf("tool name arrived after arguments started")
			}
			p.Name += name
		}
		if args := str(function["arguments"]); args != "" {
			if p.Name == "" || p.CallID == "" {
				return fmt.Errorf("tool arguments arrived without a name and ID")
			}
			if err := s.delta(p, args); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *responseState) accept(root Object, stream bool) error {
	if e := root["error"]; e != nil {
		return fmt.Errorf("upstream error: %s", str(object(e)["message"]))
	}
	if model := str(root["model"]); model != "" {
		s.model = model
	}
	s.updateUsage(root)
	choices := array(root["choices"])
	if len(choices) > 1 {
		return fmt.Errorf("multiple choices cannot be converted")
	}
	if err := s.start(); err != nil {
		return err
	}
	if len(choices) == 0 {
		if !stream {
			return fmt.Errorf("upstream response has no choices")
		}
		return nil
	}
	choice := object(choices[0])
	if v := str(choice["finish_reason"]); v != "" {
		switch v {
		case "stop", "tool_calls", "length", "content_filter":
			s.finish = v
		default:
			return fmt.Errorf("unsupported finish_reason %q", v)
		}
	}
	key := "message"
	if stream {
		key = "delta"
	}
	return s.message(object(choice[key]), stream)
}

func (s *responseState) responseItem(p *part, complete bool) Object {
	status := "in_progress"
	if complete {
		status = "completed"
	}
	if p.Kind == "tool" {
		args := ""
		if complete {
			args = p.Value
		}
		return Object{"type": "function_call", "id": p.ID, "call_id": p.CallID, "name": p.Name, "arguments": args, "status": status}
	}
	content := Object{"type": "output_text", "text": p.Value, "annotations": []any{}}
	if p.Kind == "refusal" {
		content = Object{"type": "refusal", "refusal": p.Value}
	}
	return Object{"type": "message", "id": p.ID, "role": "assistant", "status": status, "content": []any{content}}
}

func (s *responseState) validate() error {
	if s.finish == "" {
		return fmt.Errorf("upstream response ended without finish_reason")
	}
	for _, p := range s.parts {
		if p.Kind == "tool" {
			if p.CallID == "" || p.Name == "" {
				return fmt.Errorf("incomplete tool call")
			}
			if _, err := decode([]byte(p.Value)); err != nil {
				return fmt.Errorf("tool arguments are not a complete JSON object")
			}
		}
	}
	return nil
}

func (s *responseState) result() Object {
	if s.plan.Source == "anthropic" {
		content := []any{}
		for _, p := range s.parts {
			if p.Kind == "tool" {
				input, _ := decode([]byte(p.Value))
				content = append(content, Object{"type": "tool_use", "id": p.CallID, "name": p.Name, "input": input})
			} else {
				content = append(content, Object{"type": "text", "text": p.Value})
			}
		}
		return Object{"id": s.plan.ID, "type": "message", "role": "assistant", "model": s.model, "content": content, "stop_reason": s.stopReason(), "stop_sequence": nil, "usage": s.anthropicUsage()}
	}
	output := []any{}
	for _, p := range s.parts {
		output = append(output, s.responseItem(p, true))
	}
	response := Object{"id": s.plan.ID, "object": "response", "status": "completed", "model": s.model, "output": output, "usage": s.usage, "error": nil, "incomplete_details": nil}
	if s.finish == "length" || s.finish == "content_filter" {
		response["status"] = "incomplete"
		reason := "max_output_tokens"
		if s.finish == "content_filter" {
			reason = "content_filter"
		}
		response["incomplete_details"] = Object{"reason": reason}
	}
	return response
}

func (s *responseState) stopReason() string {
	if s.refusal != nil || s.finish == "content_filter" {
		return "refusal"
	}
	switch s.finish {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func (s *responseState) complete() error {
	if s.done {
		return fmt.Errorf("duplicate stream terminator")
	}
	if err := s.validate(); err != nil {
		return err
	}
	for _, p := range s.parts {
		if err := s.startPart(p); err != nil {
			return err
		}
		if s.plan.Source == "anthropic" {
			if err := s.event("content_block_stop", Object{"index": p.Index}); err != nil {
				return err
			}
			continue
		}
		if p.Kind == "tool" {
			if err := s.event("response.function_call_arguments.done", Object{"item_id": p.ID, "output_index": p.Index, "arguments": p.Value, "name": p.Name}); err != nil {
				return err
			}
		} else {
			kind, key := "response.output_text.done", "text"
			if p.Kind == "refusal" {
				kind, key = "response.refusal.done", "refusal"
			}
			if err := s.event(kind, Object{"item_id": p.ID, "output_index": p.Index, "content_index": 0, key: p.Value}); err != nil {
				return err
			}
			item := s.responseItem(p, true)
			if err := s.event("response.content_part.done", Object{"item_id": p.ID, "output_index": p.Index, "content_index": 0, "part": array(item["content"])[0]}); err != nil {
				return err
			}
		}
		if err := s.event("response.output_item.done", Object{"output_index": p.Index, "item": s.responseItem(p, true)}); err != nil {
			return err
		}
	}
	s.done = true
	if s.plan.Source == "anthropic" {
		if err := s.event("message_delta", Object{"delta": Object{"stop_reason": s.stopReason(), "stop_sequence": nil}, "usage": s.anthropicUsage()}); err != nil {
			return err
		}
		return s.event("message_stop", Object{})
	}
	result := s.result()
	kind := "response.completed"
	if result["status"] == "incomplete" {
		kind = "response.incomplete"
	}
	return s.event(kind, Object{"response": result})
}

func (p *Plan) Error(message string) Object {
	if p.Source == "anthropic" {
		return Object{"type": "error", "error": Object{"type": "api_error", "message": message}}
	}
	return Object{"error": Object{"type": "gateway_conversion_error", "message": message, "code": "gateway_conversion_error"}}
}

type streamBody struct {
	*io.PipeReader
	source io.ReadCloser
}

func (b *streamBody) Close() error { _ = b.PipeReader.Close(); return b.source.Close() }

// Response preserves HTTP error status, converts only the supported payload,
// and never retries a stream. Closing the client body closes its upstream too.
func (p *Plan) Response(resp *http.Response) (*http.Response, error) {
	copy := *resp
	copy.Header = resp.Header.Clone()
	var input io.Reader = resp.Body
	if encoding := resp.Header.Get("Content-Encoding"); strings.EqualFold(encoding, "gzip") {
		reader, err := gzip.NewReader(resp.Body)
		if err != nil {
			resp.Body.Close()
			return nil, err
		}
		input = reader
	} else if encoding != "" && !strings.EqualFold(encoding, "identity") {
		resp.Body.Close()
		return nil, fmt.Errorf("unsupported upstream Content-Encoding for conversion")
	}
	copy.Header.Del("Content-Encoding")
	copy.Header.Del("Content-Length")
	copy.Header.Del("ETag")
	copy.ContentLength = -1
	if resp.StatusCode >= 400 || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(input, maxConversionBytes+1))
		if err != nil {
			return nil, err
		}
		if len(body) > maxConversionBytes {
			return nil, fmt.Errorf("upstream JSON exceeds 64 MiB conversion limit")
		}
		root, err := decode(body)
		if err != nil {
			return nil, err
		}
		var value Object
		if resp.StatusCode >= 400 {
			message := str(object(root["error"])["message"])
			if message == "" {
				message = "upstream returned HTTP " + strconv.Itoa(resp.StatusCode)
			}
			value = p.Error(message)
		} else {
			state := p.newState(nil)
			if err := state.accept(root, false); err != nil {
				return nil, err
			}
			if err := state.validate(); err != nil {
				return nil, err
			}
			value = state.result()
			if p.Stream {
				return nil, fmt.Errorf("provider returned JSON for a streaming conversion request")
			}
		}
		raw, _ := json.Marshal(value)
		copy.Body = io.NopCloser(bytes.NewReader(raw))
		copy.ContentLength = int64(len(raw))
		copy.Header.Set("Content-Type", "application/json")
		return &copy, nil
	}
	if !p.Stream {
		resp.Body.Close()
		return nil, fmt.Errorf("provider returned SSE for a non-streaming conversion request")
	}
	reader, writer := io.Pipe()
	copy.Body = &streamBody{PipeReader: reader, source: resp.Body}
	copy.Header.Set("Content-Type", "text/event-stream")
	copy.Header.Set("Cache-Control", "no-cache")
	go func() {
		defer resp.Body.Close()
		state := p.newState(writer)
		parser := sse.NewSSEParser()
		buffer := make([]byte, 32<<10)
		var failure error
		for failure == nil {
			n, readErr := input.Read(buffer)
			for _, event := range parser.Feed(buffer[:n]) {
				if event.Data == "" {
					continue
				}
				if state.done {
					failure = fmt.Errorf("data arrived after stream terminator")
					break
				}
				if event.Data == "[DONE]" {
					failure = state.complete()
				} else {
					root, err := decode([]byte(event.Data))
					if err == nil {
						err = state.accept(root, true)
					}
					failure = err
				}
				if failure != nil {
					break
				}
			}
			if failure == nil {
				failure = parser.Err()
			}
			if readErr != nil {
				if readErr != io.EOF {
					failure = readErr
				} else if !state.done {
					failure = fmt.Errorf("Chat Completions stream ended before [DONE]")
				}
				break
			}
		}
		if failure != nil {
			_ = state.event("error", Object{"error": Object{"type": "gateway_conversion_error", "message": failure.Error()}})
		}
		_ = writer.Close()
	}()
	return &copy, nil
}
