package correlation

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

// RunEvidence 仅记录明确任务标识及来源，不参与会话身份或因果父子关系的推断。
// Fingerprint 只用于编辑时识别歧义字段变动，不保留无效标识的原始内容。
type RunEvidence struct {
	Value       string
	Sources     []string
	Warning     string
	Fingerprint string
}

// Equal 比较任务标识的有效值和原始证据，避免两个不同的无效值被误认为未修改。
func (r RunEvidence) Equal(other RunEvidence) bool {
	return r.Value == other.Value && r.Warning == other.Warning && r.Fingerprint == other.Fingerprint && slices.Equal(r.Sources, other.Sources)
}

// extractRun 检查所有头值及 metadata.run_id，冲突或非法证据不会被择优忽略。
// 普通请求继续按原样转发，观测归属留空并给出 warning。
func extractRun(headers http.Header, root gjson.Result) RunEvidence {
	run := RunEvidence{Sources: []string{}}
	values := map[string]bool{}
	evidence := []string{}
	add := func(value, source string, valid bool) {
		raw := value
		valid = valid && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
		value = strings.TrimSpace(value)
		if !slices.Contains(run.Sources, source) {
			run.Sources = append(run.Sources, source)
		}
		if !valid || value == "" || len(value) > 1024 {
			evidence = append(evidence, source, "invalid", raw)
			run.Warning = "invalid_run_identifier"
			return
		}
		evidence = append(evidence, source, "valid", value)
		values[value] = true
	}
	for _, value := range headers.Values("X-Run-ID") {
		add(value, "header:X-Run-ID", true)
	}
	metadataCount, runCount := 0, 0
	root.ForEach(func(key, metadata gjson.Result) bool {
		if key.String() != "metadata" {
			return true
		}
		metadataCount++
		if metadata.IsObject() {
			metadata.ForEach(func(key, value gjson.Result) bool {
				if key.String() != "run_id" {
					return true
				}
				runCount++
				if value.Type == gjson.String {
					add(value.String(), "body:metadata.run_id", true)
				} else {
					add(value.Raw, "body:metadata.run_id", false)
				}
				return true
			})
		}
		return true
	})
	if len(values) > 1 || (runCount > 0 && (metadataCount > 1 || runCount > 1)) {
		run.Warning = "conflicting_run_identifier"
	}
	if len(evidence) > 0 {
		raw, _ := json.Marshal(evidence)
		run.Fingerprint = Hash(string(raw))
	}
	if run.Warning == "" {
		for value := range values {
			run.Value = value
		}
	}
	return run
}

// conflictingSessionEvidence 同时检查有效身份之间的矛盾及原始字段的歧义。
// GJSON/HTTP 的首个匹配值继续服务旧 Session 行为，但不能替 Run 证明重复字段的唯一含义。
func conflictingSessionEvidence(headers http.Header, root gjson.Result, identities []Identity) bool {
	seen := map[string]string{}
	for _, identity := range identities {
		if old := seen[identity.Kind]; old != "" && old != identity.Value {
			return true
		}
		seen[identity.Kind] = identity.Value
	}
	for _, header := range []string{"X-Session-ID", "X-Conversation-ID", "X-Thread-ID"} {
		if len(headers.Values(header)) > 1 {
			return true
		}
	}
	return ambiguousSessionJSON(root)
}

// ambiguousSessionJSON 只遍历实际参与会话归属的对象，不把消息/工具内容中同名字段当作证据。
// 重复 metadata 可能使不同解析器选择另一组会话标识；即使某一份没有该字段，也不能认证首值。
func ambiguousSessionJSON(root gjson.Result) bool {
	rootSeen := map[string]bool{}
	ambiguous, metadataHasIdentity := false, false
	metadataCount := 0
	root.ForEach(func(key, value gjson.Result) bool {
		switch name := key.String(); name {
		case "session_id", "conversation_id", "thread_id", "conversation":
			if rootSeen[name] {
				ambiguous = true
			}
			rootSeen[name] = true
			if name == "conversation" && value.IsObject() {
				ids := 0
				value.ForEach(func(key, _ gjson.Result) bool {
					if key.String() == "id" {
						ids++
					}
					return true
				})
				ambiguous = ambiguous || ids > 1
			}
		case "metadata":
			metadataCount++
			if !value.IsObject() {
				return true
			}
			seen := map[string]bool{}
			value.ForEach(func(key, _ gjson.Result) bool {
				switch name := key.String(); name {
				case "session_id", "conversation_id", "thread_id":
					metadataHasIdentity = true
					if seen[name] {
						ambiguous = true
					}
					seen[name] = true
				}
				return true
			})
		}
		return true
	})
	return ambiguous || (metadataCount > 1 && metadataHasIdentity)
}
