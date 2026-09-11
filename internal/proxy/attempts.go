package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
	"github.com/google/uuid"
)

// upstreamAttempt 只追踪网关向配置上游的一次发送；中转内部重试不属于本记录。
// 正文读取和关闭可能并发发生，结果通过互斥锁发布为不可变快照。
type upstreamAttempt struct {
	mu      sync.Mutex
	server  *Server
	ctx     context.Context
	entry   store.RouteAttempt
	started time.Time
	ended   bool
}

// beginUpstreamAttempt 在所有本地校验通过后保存独立出站快照，并紧接实际发送。
// Store 完成捕获后才设置 started_at，避免把写请求文件的时间当成上游耗时。
func (s *Server) beginUpstreamAttempt(trace string, req *http.Request, endpoint provider.Provider, body []byte, transport string) *upstreamAttempt {
	snapshot := snapshotRequest(req, body)
	snapshot.Forwarding.ProviderID, snapshot.Forwarding.BaseURL = endpoint.ID, endpoint.BaseURL
	metadata := store.RouteAttempt{
		ID: uuid.NewString(), TraceID: trace, ProviderID: endpoint.ID,
		URL: endpoint.BaseURL, RequestURL: snapshot.URL, Transport: transport,
		Source: "gateway", Status: "running",
	}
	entry, ok := s.store.BeginAttempt(trace, metadata, snapshot)
	if !ok {
		return nil
	}
	started, err := time.Parse(time.RFC3339Nano, entry.StartedAt)
	if err != nil {
		started = time.Now()
	}
	attempt := &upstreamAttempt{server: s, ctx: req.Context(), entry: entry, started: started}
	attempt.publish()
	return attempt
}

func (a *upstreamAttempt) publish() {
	if log, ok := a.server.store.Log(a.entry.TraceID); ok {
		a.server.publishLog(log)
	}
}

// headers 分别保存响应头到达时间和旧版 duration_ms；这不是正文接收总耗时。
func (a *upstreamAttempt) headers(status int, at time.Time) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended || a.entry.HeadersAt != "" {
		return
	}
	a.entry.StatusCode = status
	a.entry.HeadersAt = at.Format(time.RFC3339Nano)
	a.entry.Duration = elapsedMilliseconds(a.started, at)
	a.server.store.UpdateAttempt(a.entry.TraceID, a.entry)
	a.publish()
}

// websocketSent 表示模型帧已写入现有连接，不制造一次新的 HTTP 握手或头部耗时。
func (a *upstreamAttempt) websocketSent() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended {
		return
	}
	a.entry.StatusCode = http.StatusSwitchingProtocols
	a.server.store.UpdateAttempt(a.entry.TraceID, a.entry)
	a.publish()
}

func elapsedMilliseconds(start, end time.Time) float64 {
	return max(0, float64(end.Sub(start).Microseconds())/1000)
}

// finish 幂等结束本次真实发送，EOF、错误或主动关闭中的第一个终止事件决定耗时。
func (a *upstreamAttempt) finish(status string, err error) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended {
		return
	}
	a.ended = true
	if errors.Is(a.ctx.Err(), context.Canceled) {
		status = "canceled"
		if err == nil {
			err = a.ctx.Err()
		}
	}
	if status == "done" && a.entry.StatusCode >= 400 {
		status = "error"
		if err == nil {
			err = fmt.Errorf("上游返回 HTTP %d", a.entry.StatusCode)
		}
	}
	now := time.Now()
	total := elapsedMilliseconds(a.started, now)
	a.entry.Status, a.entry.EndedAt, a.entry.TotalDuration = status, now.Format(time.RFC3339Nano), &total
	if a.entry.HeadersAt == "" {
		a.entry.Duration = total
	}
	if err != nil {
		a.entry.Error = err.Error()
	}
	a.server.store.UpdateAttempt(a.entry.TraceID, a.entry)
	a.publish()
}

// observationFailure 补充协议层才可识别的错误，不把本地解析耗时加到已结束的传输上。
func (a *upstreamAttempt) observationFailure(err error) {
	if a == nil || err == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.ended || a.entry.Status != "done" {
		return
	}
	a.entry.Status, a.entry.Error = "error", err.Error()
	a.server.store.UpdateAttempt(a.entry.TraceID, a.entry)
	a.publish()
}

// attemptBody 跟随原始上游正文的消费结束计时；协议转换仍使用转换前的读流。
type attemptBody struct {
	io.ReadCloser
	attempt *upstreamAttempt
}

func (b *attemptBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.attempt.finish("done", nil)
	} else if err != nil {
		b.attempt.finish("error", err)
	}
	return n, err
}

func (b *attemptBody) Close() error {
	err := b.ReadCloser.Close()
	if err != nil {
		b.attempt.finish("error", err)
	} else {
		b.attempt.finish("abandoned", fmt.Errorf("响应正文未读取到末尾，接收已结束"))
	}
	return err
}
