package store

// RouteAttempt 描述网关对配置上游的一次实际传输尝试；Duration 保留原有到响应头的耗时语义。
// TotalDuration 仅在真实终止时填写，缺失表示无法确定完整执行耗时。
type RouteAttempt struct {
	ID            string   `json:"id,omitempty"`
	TraceID       string   `json:"trace_id,omitempty"`
	Sequence      int      `json:"sequence,omitempty"`
	Transport     string   `json:"transport,omitempty"`
	Source        string   `json:"source,omitempty"`
	Status        string   `json:"status,omitempty"`
	StartedAt     string   `json:"started_at,omitempty"`
	HeadersAt     string   `json:"headers_at,omitempty"`
	EndedAt       string   `json:"ended_at,omitempty"`
	TotalDuration *float64 `json:"total_duration_ms,omitempty"`
	RequestURL    string   `json:"request_url,omitempty"`
	ProviderID    string   `json:"provider_id,omitempty"`
	URL           string   `json:"url"`
	StatusCode    int      `json:"status_code,omitempty"`
	Duration      float64  `json:"duration_ms"`
	Error         string   `json:"error,omitempty"`
}
type RouteInfo struct {
	ID            string         `json:"id,omitempty"`
	ProviderID    string         `json:"provider_id,omitempty"`
	Attempts      []RouteAttempt `json:"attempts"`
	Conversion    string         `json:"conversion,omitempty"`
	OriginalModel string         `json:"original_model,omitempty"`
	TargetModel   string         `json:"target_model,omitempty"`
}

// SetRoute 保存路由选择；已经产生的逐次尝试由 BeginAttempt/UpdateAttempt 维护，不能被旧摘要覆盖。
func (s *Store) SetRoute(trace string, info RouteInfo) {
	s.Lock()
	defer s.Unlock()
	if rec := s.records[trace]; rec != nil {
		if len(rec.attemptCaptures) > 0 && rec.log.Route != nil {
			info.Attempts = copyAttempts(rec.log.Route.Attempts)
			info.ProviderID = rec.log.Route.ProviderID
		} else {
			info.Attempts = copyAttempts(info.Attempts)
		}
		rec.log.Route = &info
		s.touch(rec)
	}
}
