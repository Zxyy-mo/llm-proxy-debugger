package protocol

import "github.com/tidwall/gjson"

// OpenAIHandler 适配 OpenAI 协议
// 思考字段按优先级匹配：
//   - reasoning_content: OpenAI 官方 / DeepSeek 等
//   - reasoning: Qwen3 / 部分 vLLM 部署
type OpenAIHandler struct{}

func (h *OpenAIHandler) Name() string { return "openai" }

var reasoningFieldCandidates = []string{"reasoning_content", "reasoning"}

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
				InputTokens:           int(usage.Get("prompt_tokens").Int()),
				OutputTokens:          int(usage.Get("completion_tokens").Int()),
				ThinkingTokens:        int(usage.Get("completion_tokens_details.reasoning_tokens").Int()),
				IsFinalOutputTokens:   true,
				IsFinalThinkingTokens: true,
			}, nil
		}
		return nil, nil
	}

	metrics := &Metrics{}
	if content := delta.Get("content"); content.Type == gjson.String {
		s := content.String()
		metrics.OutputContent = s
		metrics.OutputTokens = len([]rune(s))
	}
	// 遇到 null / 缺失都会跳过，继续尝试下一个候选字段
	for _, key := range reasoningFieldCandidates {
		r := delta.Get(key)
		if r.Type != gjson.String {
			continue
		}
		s := r.String()
		metrics.ThinkingContent = s
		metrics.ThinkingTokens = len([]rune(s))
		break
	}

	return metrics, nil
}
