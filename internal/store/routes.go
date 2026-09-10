package store

type RouteAttempt struct {
	ProviderID string  `json:"provider_id,omitempty"`
	URL        string  `json:"url"`
	StatusCode int     `json:"status_code,omitempty"`
	Duration   float64 `json:"duration_ms"`
	Error      string  `json:"error,omitempty"`
}
type RouteInfo struct {
	ID            string         `json:"id,omitempty"`
	ProviderID    string         `json:"provider_id,omitempty"`
	Attempts      []RouteAttempt `json:"attempts"`
	Conversion    string         `json:"conversion,omitempty"`
	OriginalModel string         `json:"original_model,omitempty"`
	TargetModel   string         `json:"target_model,omitempty"`
}

func (s *Store) SetRoute(trace string, info RouteInfo) {
	s.Lock()
	defer s.Unlock()
	if rec := s.records[trace]; rec != nil {
		info.Attempts = append([]RouteAttempt(nil), info.Attempts...)
		rec.log.Route = &info
		s.touch(rec)
	}
}
