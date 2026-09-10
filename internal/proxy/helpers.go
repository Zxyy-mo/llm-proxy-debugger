package proxy

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"unicode/utf8"
)

// getClientIP 获取客户端真实 IP
func getClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		return strings.TrimSpace(ips[0])
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// truncateBody 截断过长的 body
func truncateBody(body []byte, maxSize int) string {
	if len(body) <= maxSize {
		return string(body)
	}
	end := maxSize
	for end > 0 && end < len(body) && !utf8.RuneStart(body[end]) {
		end--
	}
	return string(body[:end]) + fmt.Sprintf("... [truncated, total %d bytes]", len(body))
}

// extractHeaders 提取重要的请求头，对敏感字段脱敏
func extractHeaders(h http.Header) map[string]string {
	headers := make(map[string]string)
	importantHeaders := []string{
		"Content-Type",
		"Content-Encoding",
		"Authorization",
		"X-API-Key",
		"Accept",
		"Origin",
		"Referer",
		"X-Session-ID",
		"X-Conversation-ID",
		"X-Thread-ID",
		"X-Parent-Trace-ID",
		"Anthropic-Version",
		"Anthropic-Beta",
		"OpenAI-Beta",
	}
	for _, key := range importantHeaders {
		if v := h.Get(key); v != "" {
			if key == "Authorization" || key == "X-API-Key" {
				headers[key] = "****"
			} else {
				headers[key] = v
			}
		}
	}
	return headers
}

// singleJoiningSlash 合并路径，避免双斜杠或缺失斜杠
func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}
