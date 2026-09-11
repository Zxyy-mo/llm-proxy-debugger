package provider

import (
	"fmt"
	"strings"
)

// Profile 标识接入配置模板，不表示已验证某个产品、版本或实例。
type Profile string

const (
	ProfileCustom  Profile = "custom"
	ProfileCPA     Profile = "cpa"
	ProfileNewAPI  Profile = "newapi"
	ProfileSub2API Profile = "sub2api"
	ProfileVLLM    Profile = "vllm"
)

// CapabilityState 是操作者对实例接口的声明；空值和 unknown 均保留原有透传行为。
type CapabilityState string

const (
	CapabilityUnknown     CapabilityState = "unknown"
	CapabilitySupported   CapabilityState = "supported"
	CapabilityUnsupported CapabilityState = "unsupported"
)

// Capabilities 与传输、转换模式分开保存；工具执行和中转站内部重试不属于这些能力。
type Capabilities struct {
	ChatCompletions CapabilityState `json:"chat_completions,omitempty"`
	Responses       CapabilityState `json:"responses,omitempty"`
	Messages        CapabilityState `json:"messages,omitempty"`
	Models          CapabilityState `json:"models,omitempty"`
}

func (p Provider) validateCapabilities() error {
	switch p.Profile {
	case "", ProfileCustom, ProfileCPA, ProfileNewAPI, ProfileSub2API, ProfileVLLM:
	default:
		return fmt.Errorf("Provider %s: profile 必须为 custom、cpa、newapi、sub2api 或 vllm", p.ID)
	}
	for _, capability := range []struct {
		name  string
		state CapabilityState
	}{
		{"chat_completions", p.Capabilities.ChatCompletions},
		{"responses", p.Capabilities.Responses},
		{"messages", p.Capabilities.Messages},
		{"models", p.Capabilities.Models},
	} {
		switch capability.state {
		case "", CapabilityUnknown, CapabilitySupported, CapabilityUnsupported:
		default:
			return fmt.Errorf("Provider %s: capabilities.%s 必须为 unknown、supported 或 unsupported", p.ID, capability.name)
		}
	}
	return nil
}

// UnsupportedEndpointError 表示配置明确禁止该接口，调用者应在发送前返回可诊断的错误。
type UnsupportedEndpointError struct {
	ProviderID string
	Capability string
}

func (e *UnsupportedEndpointError) Error() string {
	return fmt.Sprintf("Provider %s 明确声明不支持 %s；请检查实例能力声明或选择其他 Provider", e.ProviderID, e.Capability)
}

// CheckEndpoint 检查实际出站接口，调用方应在协议转换之后、拼接 Base URL 挂载路径之前调用。
// 未声明或未知路径继续兼容透传；预设名称和模型列表成功都不能自动证明其他接口可用。
func (p Provider) CheckEndpoint(path string) error {
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimPrefix(path, "/v1/")
	path = strings.TrimPrefix(path, "/")
	var name string
	var state CapabilityState
	switch {
	case path == "chat/completions" || strings.HasPrefix(path, "chat/completions/"):
		name, state = "chat_completions", p.Capabilities.ChatCompletions
	case path == "responses" || strings.HasPrefix(path, "responses/"):
		name, state = "responses", p.Capabilities.Responses
	case path == "messages" || strings.HasPrefix(path, "messages/"):
		name, state = "messages", p.Capabilities.Messages
	case path == "models" || strings.HasPrefix(path, "models/"):
		name, state = "models", p.Capabilities.Models
	}
	if state == CapabilityUnsupported {
		return &UnsupportedEndpointError{ProviderID: p.ID, Capability: name}
	}
	return nil
}

// Preset 是不带秘密的接入起点；模板保留全部接口为未知，实际地址由操作者填写。
type Preset struct {
	ID                 Profile  `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	BaseURLPlaceholder string   `json:"base_url_placeholder"`
	Notes              []string `json:"notes"`
	Defaults           Provider `json:"defaults"`
}

// Presets 返回独立模板，既不探测远端，也不依据平台名称启用历史、WebSocket 或转换。
func Presets() []Preset {
	items := []Preset{
		{ID: ProfileCustom, Name: "自定义兼容接口", Description: "保留现有透传、鉴权和模型路由，自行声明实例支持的接口。", BaseURLPlaceholder: "https://api.example.com/v1", Notes: []string{"Base URL 可以是 origin、/v1 或带挂载前缀的路径。"}},
		{ID: ProfileCPA, Name: "CPA", Description: "用于接入你实际使用的 CPA 实例；当前未绑定某个具体同名项目或版本。", BaseURLPlaceholder: "http://127.0.0.1:端口/v1", Notes: []string{"请以实际 CPA 项目的地址、鉴权方式和接口文档为准。", "先验证 Chat Completions 和模型列表；不能从名称推断 Responses、Messages 等能力。"}},
		{ID: ProfileNewAPI, Name: "New API", Description: "使用实例公开的 OpenAI 兼容地址与令牌，模型可见范围由该实例决定。", BaseURLPlaceholder: "https://newapi.example.com/v1", Notes: []string{"填写对客户端公开的 API 地址；有反向代理挂载时保留其前缀。", "通道协议、模型权限和接口可用性仍取决于实例配置。"}},
		{ID: ProfileSub2API, Name: "Sub2API", Description: "通过实例公开的兼容接口转发请求，按该实例的模型名称配置路由。", BaseURLPlaceholder: "https://sub2api.example.com/v1", Notes: []string{"实例可能只开放部分协议；不依赖产品名自动开启任何高级接口。", "中转站内部的重试与供应商切换需要其自身证据才能关联。"}},
		{ID: ProfileVLLM, Name: "vLLM", Description: "接入 OpenAI 兼容服务地址，可用模型及工具调用行为取决于服务端启动配置。", BaseURLPlaceholder: "http://127.0.0.1:8000/v1", Notes: []string{"模型 ID 使用服务端暴露的名称，可通过模型列表查询并设置别名。", "工具调用仍依赖服务端模型、聊天模板与解析器配置；请用真实请求验证。"}},
	}
	for i := range items {
		items[i].Defaults = Provider{
			Name: items[i].Name, Profile: items[i].ID, Protocol: "passthrough",
			AuthHeader: "Authorization", AuthScheme: "Bearer",
			Capabilities: Capabilities{ChatCompletions: CapabilityUnknown, Responses: CapabilityUnknown, Messages: CapabilityUnknown, Models: CapabilityUnknown},
		}
		if items[i].ID != ProfileCustom {
			items[i].Defaults.KeyEnv = strings.ToUpper(string(items[i].ID)) + "_API_KEY"
		}
	}
	return items
}
