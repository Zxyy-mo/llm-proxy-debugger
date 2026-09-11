package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/provider"
)

const (
	providerModelsTimeout = 30 * time.Second
	providerModelsMaxBody = 2 << 20
	providerModelsMaxRows = 4096
)

type providerModel struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by,omitempty"`
	Created *int64 `json:"created,omitempty"`
}

type providerModelsResult struct {
	ProviderID string            `json:"provider_id"`
	Provider   provider.Provider `json:"provider"`
	StatusCode int               `json:"status_code"`
	Models     []providerModel   `json:"models"`
	Source     string            `json:"source"`
	CheckedAt  time.Time         `json:"checked_at"`
	Error      string            `json:"error,omitempty"`
	ErrorCode  string            `json:"error_code,omitempty"`
}

// ProviderPresetsHandler 返回配置起点与网关处理范围，不访问或验证任何上游实例。
func (s *Server) ProviderPresetsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		replayJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 GET"})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"presets":           provider.Presets(),
		"capability_source": "operator_declared",
		"gateway": map[string]any{
			"native_forwarding":        "原生 HTTP/SSE 透传；可用接口由上游实例决定",
			"structured_observation":   []string{"chat_completions", "responses", "messages"},
			"conversion":               "独立 Anthropic Messages / Responses 的文本和函数工具调用、结果转为 Chat Completions",
			"history":                  "仅按已知 Response ID 查询，需显式开启",
			"websocket":                "原生 Responses WebSocket，需显式开启，不做协议转换",
			"model_discovery_evidence": "本次 GET /v1/models 返回的列表，不证明模型可调用或其他接口可用",
		},
	})
}

// ProviderModelsHandler 只查询已保存的目的地；临时密钥、响应体和模型列表均不进入配置或请求记录。
func (s *Server) ProviderModelsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		replayJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "仅支持 POST"})
		return
	}
	var input struct {
		ProviderID string `json:"provider_id"`
		APIKey     string `json:"api_key,omitempty"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.ProviderID == "" {
		replayJSON(w, http.StatusBadRequest, map[string]string{"error": "模型查询需要 provider_id 和可选的 api_key"})
		return
	}
	p, ok := s.providers.Get(input.ProviderID)
	if !ok {
		replayJSON(w, http.StatusNotFound, map[string]string{"error": "未找到已保存的 Provider，请先保存配置"})
		return
	}
	if err := p.CheckEndpoint("/v1/models"); err != nil {
		replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	target, err := url.Parse(p.BaseURL)
	if err != nil {
		replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Provider 地址无效"})
		return
	}
	target.Path, target.RawPath = provider.JoinURLPath(target, &url.URL{Path: "/v1/models"})
	ctx, cancel := context.WithTimeout(r.Context(), providerModelsTimeout)
	defer cancel()
	if s.lifecycle != nil {
		stop := context.AfterFunc(s.lifecycle, cancel)
		defer stop()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "无法创建模型列表查询"})
		return
	}
	req.Header.Set("Accept", "application/json")
	if input.APIKey != "" {
		if err := validCredentialValue(input.APIKey); err != nil {
			replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "本次查询密钥不能为空或包含控制字符"})
			return
		}
		key := strings.TrimSpace(input.APIKey)
		input.APIKey = ""
		if len(key) > 7 && strings.EqualFold(key[:7], "Bearer ") {
			key = strings.TrimSpace(key[7:])
		}
		if key == "" {
			replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "本次查询密钥不能为空"})
			return
		}
		err = p.AuthorizeKey(req.Header, key)
	} else {
		// 管理接口的 Authorization 绝不能借用为上游凭证；未配置时允许访问免鉴权实例。
		err = p.Authorize(req.Header)
	}
	if err != nil {
		replayJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	client := &http.Client{Transport: s.transport, Timeout: providerModelsTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		status, message := http.StatusBadGateway, "Provider 模型查询连接失败"
		var networkError net.Error
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			status, message = http.StatusGatewayTimeout, "Provider 模型查询超时（最长 30 秒）"
		} else if ctx.Err() != nil {
			status, message = http.StatusRequestTimeout, "Provider 模型查询已取消"
		}
		// 网络错误可能包含目标、请求头等不受控内容，界面只返回固定诊断。
		replayJSON(w, status, map[string]string{"error": message})
		return
	}
	defer response.Body.Close()
	result := providerModelsResult{ProviderID: p.ID, Provider: p, StatusCode: response.StatusCode, Models: []providerModel{}, Source: "provider_models", CheckedAt: time.Now().UTC()}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// 上游错误页可能回显密钥；只报告状态及可操作的说明，不向客户端复制原始响应体。
		result.ErrorCode = "upstream_rejected"
		result.Error = fmt.Sprintf("Provider 返回 HTTP %d，请检查实例地址、鉴权和模型列表权限", response.StatusCode)
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			result.ErrorCode = "upstream_redirect"
			result.Error = fmt.Sprintf("Provider 返回 HTTP %d；模型查询不会跟随重定向，请直接配置 API 地址", response.StatusCode)
		}
	} else {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, providerModelsMaxBody+1))
		switch {
		case readErr != nil:
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				replayJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "Provider 模型查询超时（最长 30 秒）"})
				return
			}
			if ctx.Err() != nil {
				replayJSON(w, http.StatusRequestTimeout, map[string]string{"error": "Provider 模型查询已取消"})
				return
			}
			result.ErrorCode, result.Error = "unreadable_response", "模型列表读取失败，请检查上游连接"
		case len(body) > providerModelsMaxBody:
			result.ErrorCode, result.Error = "response_too_large", "模型列表超过 2 MiB，未返回截断的列表"
		default:
			models, parseErr := parseProviderModels(body)
			if parseErr != nil {
				result.ErrorCode, result.Error = "invalid_response", parseErr.Error()
			} else {
				result.Models = models
			}
		}
	}
	// 查询期间被删除或改动的目的地不能再代表当前配置；调用方应重新保存并查询。
	if current, found := s.providers.Get(p.ID); !found || current != p {
		replayJSON(w, http.StatusConflict, map[string]string{"error": "查询期间 Provider 配置已改变，结果已失效，请重新查询"})
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

// parseProviderModels 只提取有界的 OpenAI 兼容模型元数据；缺失、部分或非列表响应不能伪装成空列表。
func parseProviderModels(body []byte) ([]providerModel, error) {
	var envelope struct {
		Object string          `json:"object"`
		Data   json.RawMessage `json:"data"`
		Error  json.RawMessage `json:"error"`
	}
	invalid := func() ([]providerModel, error) {
		return nil, fmt.Errorf("Provider 未返回有效的 OpenAI 兼容模型列表（需要 data 数组和非空模型 id）")
	}
	if json.Unmarshal(body, &envelope) != nil || (envelope.Object != "" && envelope.Object != "list") || (len(envelope.Error) != 0 && string(envelope.Error) != "null") {
		return invalid()
	}
	var models []providerModel
	if len(envelope.Data) == 0 || json.Unmarshal(envelope.Data, &models) != nil || models == nil {
		return invalid()
	}
	if len(models) > providerModelsMaxRows {
		return nil, fmt.Errorf("模型列表超过 4096 条，未返回截断的列表")
	}
	result := make([]providerModel, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model.ID == "" || strings.TrimSpace(model.ID) != model.ID || len(model.ID) > 512 || strings.IndexFunc(model.ID, unicode.IsControl) >= 0 || len(model.OwnedBy) > 512 || strings.IndexFunc(model.OwnedBy, unicode.IsControl) >= 0 || (model.Created != nil && *model.Created < 0) {
			return invalid()
		}
		if !seen[model.ID] {
			result = append(result, model)
			seen[model.ID] = true
		}
	}
	slices.SortFunc(result, func(a, b providerModel) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}
