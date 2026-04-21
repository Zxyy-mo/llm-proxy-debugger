package protocol

import "github.com/tidwall/gjson"

// OpenAIHandler 适配 OpenAI 协议（支持 o1/o3-mini 的 reasoning_content）
type OpenAIHandler struct{}

func (h *OpenAIHandler) Name() string { return "openai" }

func (h *OpenAIHandler) Parse(data []byte) (*Metrics, error) {
	if len(data) == 0 {
		return nil, nil
	}
	jsonStr := string(data)

	delta := gjson.Get(jsonStr, "choices.0.delta")
	if !delta.Exists() {
		usage := gjson.Get(jsonStr, "usage")
		if usage.Exists() {
			return &Metrics{
				InputTokens:    int(usage.Get("prompt_tokens").Int()),
				OutputTokens:   int(usage.Get("completion_tokens").Int()),
				ThinkingTokens: int(usage.Get("completion_tokens_details.reasoning_tokens").Int()),
			}, nil
		}
		return nil, nil
	}

	metrics := &Metrics{}
	if content := delta.Get("content"); content.Exists() {
		metrics.OutputTokens = len([]rune(content.String()))
	}
	if reasoning := delta.Get("reasoning_content"); reasoning.Exists() {
		content := reasoning.String()
		metrics.ThinkingContent = content
		metrics.ThinkingTokens = len([]rune(content))
	}

	return metrics, nil
}
