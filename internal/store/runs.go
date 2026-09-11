package store

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
)

// RunAssociation 描述请求的任务归属证据；missing/conflict 不拥有任务 ID。
type RunAssociation struct {
	State      string   `json:"state"`
	ExternalID string   `json:"external_id,omitempty"`
	Sources    []string `json:"sources"`
	Warning    string   `json:"warning,omitempty"`
}

// RunSummary 汇总当前可观察请求；活动数量不是 Agent 任务是否结束的信号。
// 会话筛选只改变请求和计数，SessionIDs/SessionState 仍反映整轮已捕获的归属范围。
type RunSummary struct {
	ID           string   `json:"id"`
	ExternalID   string   `json:"external_id,omitempty"`
	Sources      []string `json:"sources"`
	SessionIDs   []string `json:"session_ids"`
	SessionState string   `json:"session_state"`
	TraceIDs     []string `json:"trace_ids"`
	RequestCount int      `json:"request_count"`
	AttemptCount int      `json:"attempt_count"`
	ActiveCount  int      `json:"active_count"`
	StartedAt    string   `json:"started_at"`
	LastAt       string   `json:"last_at"`
	Partial      bool     `json:"partial"`
}

// Runs 保存任务分组及没有可靠任务标识的请求，两类请求互不重复。
type Runs struct {
	Revision             uint64       `json:"revision"`
	Runs                 []RunSummary `json:"runs"`
	UnassociatedTraceIDs []string     `json:"unassociated_trace_ids"`
}

func copyRun(run *RunAssociation) *RunAssociation {
	if run == nil {
		return nil
	}
	copy := *run
	copy.Sources = append([]string{}, run.Sources...)
	return &copy
}

// assignRun 在入站关联完成后调用；任务身份依赖明确身份而非可被晚到证据移动的 SessionID。
// 重放来源只能由服务器持有的 ReplayInfo 注入，捕获报文中的旧 Run 不会继承到新执行。
func (s *Store) assignRun(rec *record) {
	rec.log.RunID = ""
	run := &RunAssociation{State: "missing", Sources: append([]string{}, rec.input.Run.Sources...)}
	rec.log.Run = run
	if replay := rec.log.Replay; replay != nil {
		id := replay.ID
		if id == "" {
			id = rec.log.TraceID
		}
		rec.log.RunID = "run:replay:" + id
		run.State, run.Sources = "replay", []string{"gateway:replay"}
		return
	}
	if rec.input.Run.Warning != "" {
		run.State, run.Warning = "conflict", rec.input.Run.Warning
		return
	}
	if rec.input.Run.Value == "" {
		return
	}
	if rec.log.Correlation.Warning == "conflicting_identifier" {
		run.State, run.Warning = "conflict", "conflicting_session_identifier"
		return
	}
	primary := correlation.Identity{}
	if len(rec.input.Identities) > 0 {
		primary = rec.input.Identities[0]
	}
	rec.log.RunID = "run:" + correlation.Hash(rec.input.Scope, primary.Kind, primary.Value, rec.input.Run.Value)
	run.State, run.ExternalID = "explicit", rec.input.Run.Value
}

// RunsSnapshot 从存活记录派生分组，不另存会随会话移动或历史清理失真的索引。
func (s *Store) RunsSnapshot(sessionID string) (Runs, bool) {
	s.RLock()
	defer s.RUnlock()
	result := Runs{Revision: s.revision, Runs: []RunSummary{}, UnassociatedTraceIDs: []string{}}
	ordered := s.orderedRecords()
	groups := map[string][]*record{}
	knownSessions := map[string]bool{}
	for _, rec := range ordered {
		if rec.fixedSession || rec.log.Correlation.ConversationID != "" || rec.log.Correlation.ThreadID != "" {
			knownSessions[rec.log.SessionID] = true
		}
		if rec.log.RunID != "" {
			groups[rec.log.RunID] = append(groups[rec.log.RunID], rec)
		}
	}
	indices := map[string]int{}
	selected := false
	for _, rec := range ordered {
		if sessionID != "" && rec.log.SessionID != sessionID {
			continue
		}
		selected = true
		log := s.displayLog(rec)
		if log.RunID == "" {
			result.UnassociatedTraceIDs = append(result.UnassociatedTraceIDs, log.TraceID)
			continue
		}
		index, exists := indices[log.RunID]
		if !exists {
			index = len(result.Runs)
			indices[log.RunID] = index
			summary := RunSummary{ID: log.RunID, Sources: []string{}, SessionIDs: []string{}, TraceIDs: []string{}, SessionState: "unknown", StartedAt: log.Time, LastAt: log.Time}
			if log.Run != nil {
				summary.ExternalID = log.Run.ExternalID
			}
			known, unknown, conflict := map[string]bool{}, false, false
			for _, member := range groups[log.RunID] {
				conflict = conflict || member.log.Correlation.Warning == "conflicting_identifier"
				if !slices.Contains(summary.SessionIDs, member.log.SessionID) {
					summary.SessionIDs = append(summary.SessionIDs, member.log.SessionID)
				}
				if knownSessions[member.log.SessionID] {
					known[member.log.SessionID] = true
				} else {
					unknown = true
				}
			}
			slices.Sort(summary.SessionIDs)
			if conflict || len(known) > 1 {
				summary.SessionState = "conflict"
			} else if len(known) == 1 && !unknown {
				summary.SessionState = "known"
			}
			result.Runs = append(result.Runs, summary)
		}
		summary := &result.Runs[index]
		summary.TraceIDs = append(summary.TraceIDs, log.TraceID)
		summary.RequestCount++
		if log.Route != nil {
			summary.AttemptCount += len(log.Route.Attempts)
		}
		if log.Status == "running" || log.Status == "pending" {
			summary.ActiveCount++
		}
		if log.Run != nil {
			for _, source := range log.Run.Sources {
				if !slices.Contains(summary.Sources, source) {
					summary.Sources = append(summary.Sources, source)
				}
			}
		}
		stamp, err := time.Parse(time.RFC3339Nano, log.Time)
		if err == nil {
			first, _ := time.Parse(time.RFC3339Nano, summary.StartedAt)
			last, _ := time.Parse(time.RFC3339Nano, summary.LastAt)
			if stamp.Before(first) {
				summary.StartedAt = log.Time
			}
			if stamp.After(last) {
				summary.LastAt = log.Time
			}
		}
		summary.Partial = summary.RequestCount < len(groups[log.RunID])
	}
	return result, sessionID == "" || selected
}

// RunsHandler 提供只读任务分组，空任务与未知会话分别返回空数组和 404。
func (s *Store) RunsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query["session_id"]) > 1 {
		writeError(w, http.StatusBadRequest, "invalid run query parameters")
		return
	}
	runs, ok := s.RunsSnapshot(query.Get("session_id"))
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	json.NewEncoder(w).Encode(runs)
}
