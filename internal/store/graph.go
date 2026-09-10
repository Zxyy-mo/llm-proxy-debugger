package store

// GraphNode is a compact projection, without request bodies, headers or thinking
// content. Inspectors obtain those fields from the session's authoritative log.
type GraphNode struct {
	ID           string      `json:"id"`
	Kind         string      `json:"kind"`
	TraceID      string      `json:"trace_id,omitempty"`
	SessionID    string      `json:"session_id,omitempty"`
	Label        string      `json:"label"`
	Time         string      `json:"time,omitempty"`
	Model        string      `json:"model,omitempty"`
	Protocol     string      `json:"protocol,omitempty"`
	Method       string      `json:"method,omitempty"`
	Path         string      `json:"path,omitempty"`
	Status       string      `json:"status"`
	StatusCode   int         `json:"status_code,omitempty"`
	Duration     float64     `json:"duration_ms"`
	InputTokens  int         `json:"input_tokens"`
	OutputTokens int         `json:"output_tokens"`
	ToolUseCount int         `json:"tool_use_count"`
	Correlation  Correlation `json:"correlation"`
	Replay       *ReplayInfo `json:"replay,omitempty"`
}

// GraphEdge kinds are parent evidence (parent_trace_id, previous_response_id,
// history) or "replay", which records operator provenance and must be drawn
// differently from conversation causality.
type GraphEdge struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Target     string `json:"target"`
	Kind       string `json:"kind"`
	Confidence string `json:"confidence"`
}

type Graph struct {
	Revision  uint64      `json:"revision"`
	SessionID string      `json:"session_id,omitempty"`
	Nodes     []GraphNode `json:"nodes"`
	Edges     []GraphEdge `json:"edges"`
}

func graphNode(log RequestLog) GraphNode {
	label := log.Summary
	if label == "" {
		label = log.Method + " " + log.Path
	}
	node := GraphNode{
		ID: log.TraceID, Kind: "request", TraceID: log.TraceID, SessionID: log.SessionID,
		Label: label, Time: log.Time, Model: log.Model, Protocol: log.Protocol,
		Method: log.Method, Path: log.Path, Status: log.Status, StatusCode: log.StatusCode,
		Duration: log.Duration, InputTokens: log.InputTokens, OutputTokens: log.OutputTokens,
		ToolUseCount: log.ToolUseCount, Correlation: log.Correlation,
	}
	if log.Replay != nil {
		replay := *log.Replay
		node.Replay = &replay
	}
	return node
}

// GraphSnapshot returns false for an unknown (or now-empty, merged) session.
func (s *Store) GraphSnapshot(sessionID string) (Graph, bool) {
	s.RLock()
	defer s.RUnlock()
	graph := Graph{Revision: s.revision, SessionID: sessionID, Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	visible := make(map[string]bool)
	var selected []*record
	for _, rec := range s.orderedRecords() {
		if sessionID != "" && rec.log.SessionID != sessionID {
			continue
		}
		selected = append(selected, rec)
		visible[rec.log.TraceID] = true
		graph.Nodes = append(graph.Nodes, graphNode(rec.log))
	}
	if sessionID != "" && len(selected) == 0 {
		return graph, false
	}
	// A record outside the selection is shown once as a reference node.
	reference := func(id string, fallback GraphNode) {
		if visible[id] {
			return
		}
		node := fallback
		if other := s.records[id]; other != nil {
			node = graphNode(other.log)
		}
		node.Kind = "reference"
		graph.Nodes = append(graph.Nodes, node)
		visible[id] = true
	}
	for _, rec := range selected {
		c := rec.log.Correlation
		if c.ParentTraceID == "" && c.ParentReference == "" {
			continue
		}
		parentID := c.ParentTraceID
		if parentID == "" {
			parentID = "reference:" + referenceKey(rec.input.Scope, c.LinkSource, c.ParentReference)[:24]
		}
		fallback := GraphNode{ID: parentID, Kind: "reference", Label: c.ParentReference, Status: "external"}
		fallback.Correlation.Warning = c.Warning
		reference(parentID, fallback)
		graph.Edges = append(graph.Edges, GraphEdge{
			ID:     parentID + ":" + rec.log.TraceID,
			Source: parentID, Target: rec.log.TraceID, Kind: c.LinkSource, Confidence: c.Confidence,
		})
	}
	for _, rec := range selected {
		if rec.log.Replay == nil {
			continue
		}
		reference(rec.log.Replay.Of, GraphNode{ID: rec.log.Replay.Of, Kind: "reference", Label: rec.log.Replay.Of, Status: "external"})
		graph.Edges = append(graph.Edges, GraphEdge{
			ID:     rec.log.Replay.Of + ":replay:" + rec.log.TraceID,
			Source: rec.log.Replay.Of, Target: rec.log.TraceID, Kind: "replay", Confidence: "exact",
		})
	}
	return graph, true
}
