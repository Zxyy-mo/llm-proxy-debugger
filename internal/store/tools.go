package store

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/observation"
)

func (s *Store) ObserveTools(trace string, calls []observation.ToolCall) {
	s.Lock()
	defer s.Unlock()
	rec := s.records[trace]
	if rec == nil {
		return
	}
	for _, call := range calls {
		found := -1
		for i, old := range rec.log.Tools {
			if old.CallID == call.CallID {
				found = i
				break
			}
		}
		if found >= 0 {
			old := rec.log.Tools[found]
			call.Output, call.ResultTrace, call.SpanID, call.StartedAt, call.EndedAt, call.Duration = old.Output, old.ResultTrace, old.SpanID, old.StartedAt, old.EndedAt, old.Duration
			if old.Source == "trace" {
				call.Source, call.Status, call.Error = old.Source, old.Status, old.Error
			} else if old.ResultTrace != "" {
				call.Status = old.Status
			}
			rec.log.Tools[found] = call
		} else {
			rec.log.Tools = append(rec.log.Tools, call)
		}
	}
	if rec.privacy.Record && !rec.privacy.RetainRaw {
		rec.log = s.displayLog(rec)
	}
	s.touch(rec)
	// Results can arrive immediately after a streamed tool call, before the
	// producing HTTP handler has finished its final bookkeeping.
	for _, other := range s.records {
		if other.input.Scope == rec.input.Scope && other.log.SessionID == rec.log.SessionID {
			s.attachResults(other)
		}
	}
}

func (s *Store) ObserveToolResults(trace string, body []byte) {
	policy, scope := s.PrivacyContext(trace)
	if policy.Record {
		body = s.Privacy.JSON(policy, scope, body)
	}
	results := observation.Results(body)
	if len(results) == 0 {
		return
	}
	s.Lock()
	defer s.Unlock()
	rec := s.records[trace]
	if rec == nil {
		return
	}
	if rec.privacy.Record && !rec.privacy.RetainRaw {
		for i := range results {
			results[i].Output = string(s.Privacy.JSON(rec.privacy, rec.privacyScope, []byte(results[i].Output)))
		}
	}
	rec.toolResults = results
	s.attachResults(rec)
	s.changed()
}

func (s *Store) attachResults(rec *record) {
	for _, result := range rec.toolResults {
		if result.CallID == "" {
			continue
		}
		var owner *record
		index := -1
		ambiguous := false
		for _, candidate := range s.records {
			if candidate.input.Scope != rec.input.Scope || candidate.log.SessionID != rec.log.SessionID {
				continue
			}
			for i, call := range candidate.log.Tools {
				if call.CallID == result.CallID {
					if owner != nil {
						ambiguous = true
					}
					owner, index = candidate, i
					if candidate.log.TraceID == rec.log.Correlation.ParentTraceID {
						ambiguous = false
						break
					}
				}
			}
			if owner != nil && owner.log.TraceID == rec.log.Correlation.ParentTraceID {
				break
			}
		}
		if owner == nil || ambiguous {
			continue
		}
		call := &owner.log.Tools[index]
		if call.Source == "trace" || call.ResultTrace != "" {
			continue
		}
		call.Output = result.Output
		if rec.privacy.RetainRaw && owner.privacy.RetainRaw {
			// Late correlation can join captures with different frozen privacy
			// namespaces. Retain only aliases used by this copied output so its
			// owner can still restore it after the result trace is cleaned up.
			s.Privacy.RetainAliases(rec.privacyScope, owner.privacyScope, call.Output)
		}
		call.ResultTrace = rec.log.TraceID
		call.Status = "result_observed"
		if result.Error {
			call.Status = "error"
		}
		if owner.privacy.Record && !owner.privacy.RetainRaw {
			owner.log = s.displayLog(owner)
		}
		s.touch(owner)
	}
}

func (s *Store) ToolSpansHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, 405, "method not allowed")
		return
	}
	var input struct {
		TraceID   string `json:"trace_id"`
		SpanID    string `json:"span_id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Kind      string `json:"kind"`
		Status    string `json:"status"`
		StartedAt string `json:"started_at"`
		EndedAt   string `json:"ended_at"`
		Input     string `json:"input"`
		Output    string `json:"output"`
		Error     string `json:"error"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	d.DisallowUnknownFields()
	if d.Decode(&input) != nil || d.Decode(new(any)) != io.EOF {
		writeError(w, 400, "invalid tool span JSON")
		return
	}
	if input.SpanID == "" || len(input.SpanID) > 200 || input.Name == "" || len(input.Input) > 65536 || len(input.Output) > 65536 || (input.Kind != "mcp" && input.Kind != "agent" && input.Kind != "tool") || (input.Status != "running" && input.Status != "done" && input.Status != "error") {
		writeError(w, 422, "span_id, name, kind (mcp/agent/tool), status (running/done/error) and bounded input/output are required")
		return
	}
	start, err := time.Parse(time.RFC3339Nano, input.StartedAt)
	if err != nil {
		writeError(w, 422, "started_at must be RFC3339")
		return
	}
	var duration *float64
	if input.Status != "running" {
		end, err := time.Parse(time.RFC3339Nano, input.EndedAt)
		if err != nil || end.Before(start) {
			writeError(w, 422, "ended_at must not precede started_at")
			return
		}
		value := float64(end.Sub(start).Microseconds()) / 1000
		duration = &value
	}
	s.Lock()
	rec := s.records[input.TraceID]
	if rec == nil {
		s.Unlock()
		writeError(w, 404, "parent request trace not found")
		return
	}
	index := -1
	for i, call := range rec.log.Tools {
		if call.SpanID == input.SpanID || (call.SpanID == "" && input.CallID != "" && call.CallID == input.CallID) {
			index = i
			break
		}
	}
	if index < 0 {
		if len(rec.log.Tools) >= 512 {
			s.Unlock()
			writeError(w, 422, "too many tools for one request")
			return
		}
		rec.log.Tools = append(rec.log.Tools, observation.ToolCall{ID: "span:" + input.SpanID, CallID: input.CallID})
		index = len(rec.log.Tools) - 1
	}
	call := &rec.log.Tools[index]
	if call.Source == "trace" && call.Status != "running" && input.Status == "running" {
		s.Unlock()
		writeError(w, 409, "a finished span cannot become running again")
		return
	}
	call.SpanID, call.Source, call.Name, call.Kind, call.Status = input.SpanID, "trace", input.Name, input.Kind, input.Status
	call.StartedAt, call.EndedAt, call.Duration = input.StartedAt, input.EndedAt, duration
	call.Input, call.Output, call.Error = input.Input, input.Output, input.Error
	if rec.privacy.Record && !rec.privacy.RetainRaw {
		rec.log = s.displayLog(rec)
	}
	s.touch(rec)
	log := s.displayLog(rec)
	s.Unlock()
	json.NewEncoder(w).Encode(log)
}

func toolNodeID(trace, id string) string {
	return "tool:" + trace + ":" + strings.ReplaceAll(id, " ", "_")
}
