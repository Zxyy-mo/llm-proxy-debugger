// Package privacy separates recording projections from outbound substitutions.
package privacy

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

type Pattern struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}
type Policy struct {
	Record      bool      `json:"record"`
	Outbound    bool      `json:"outbound"`
	RetainRaw   bool      `json:"retain_raw"`
	AllowReveal bool      `json:"allow_reveal"`
	Patterns    []Pattern `json:"patterns"`
}
type State struct {
	Key    string                       `json:"key"`
	Policy Policy                       `json:"policy"`
	Values map[string]map[string]string `json:"values"`
}
type Engine struct {
	mu       sync.Mutex
	state    State
	compiled map[string]*regexp.Regexp
}

var placeholder = regexp.MustCompile(`\[PRIVATE_[a-zA-Z][a-zA-Z0-9_-]{0,31}_[a-f0-9]{20}\]`)

func New() *Engine {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &Engine{state: State{Key: hex.EncodeToString(key), Policy: Policy{RetainRaw: true, Patterns: []Pattern{{"email", `(?i)[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}`}, {"phone", `\b(?:\+?86[- ]?)?1[3-9][0-9]{9}\b`}}}, Values: map[string]map[string]string{}}}
}
func Validate(policy Policy) error {
	if !policy.RetainRaw && !policy.Record {
		return fmt.Errorf("关闭原文保留需要同时启用记录脱敏")
	}
	if policy.AllowReveal && !policy.RetainRaw {
		return fmt.Errorf("受控还原需要启用原文保留")
	}
	if len(policy.Patterns) > 32 {
		return fmt.Errorf("最多允许 32 条脱敏模式")
	}
	seen := map[string]bool{}
	for _, p := range policy.Patterns {
		if !regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,31}$`).MatchString(p.Name) || seen[p.Name] {
			return fmt.Errorf("模式名称必须唯一，由字母、数字、下划线或短横线组成")
		}
		seen[p.Name] = true
		if len(p.Expression) == 0 || len(p.Expression) > 2048 {
			return fmt.Errorf("脱敏表达式长度必须为 1–2048")
		}
		re, err := regexp.Compile(p.Expression)
		if err != nil {
			return fmt.Errorf("%s: %w", p.Name, err)
		}
		if re.MatchString("") {
			return fmt.Errorf("脱敏表达式不能匹配空字符串")
		}
	}
	return nil
}
func (e *Engine) Policy() Policy {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.state.Policy
	p.Patterns = append([]Pattern{}, p.Patterns...)
	return p
}
func (e *Engine) Set(policy Policy) error {
	if err := Validate(policy); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	policy.Patterns = append([]Pattern{}, policy.Patterns...)
	e.state.Policy = policy
	return nil
}
func (e *Engine) Snapshot() State {
	e.mu.Lock()
	defer e.mu.Unlock()
	raw, _ := json.Marshal(e.state)
	var state State
	_ = json.Unmarshal(raw, &state)
	return state
}
func (e *Engine) RestoreState(state State) error {
	if err := Validate(state.Policy); err != nil {
		return err
	}
	if len(state.Key) != 64 {
		return fmt.Errorf("invalid privacy key")
	}
	if state.Values == nil {
		state.Values = map[string]map[string]string{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = state
	return nil
}

// Token identity uses the caller's opaque capture namespace, never credentials
// themselves. The store freezes conversation namespaces before first projection.
func (e *Engine) Text(policy Policy, scope, text string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, pattern := range policy.Patterns {
		re := e.compiled[pattern.Expression]
		if re == nil {
			var err error
			re, err = regexp.Compile(pattern.Expression)
			if err != nil {
				continue
			}
			if e.compiled == nil || len(e.compiled) >= 256 {
				e.compiled = map[string]*regexp.Regexp{}
			}
			e.compiled[pattern.Expression] = re
		}
		replace := func(raw string) string {
			return re.ReplaceAllStringFunc(raw, func(value string) string {
				if strings.HasPrefix(value, "[PRIVATE_") {
					return value
				}
				mac := hmac.New(sha256.New, []byte(e.state.Key))
				mac.Write([]byte(scope + "\x00" + pattern.Name + "\x00" + value))
				token := "[PRIVATE_" + pattern.Name + "_" + hex.EncodeToString(mac.Sum(nil))[:20] + "]"
				if policy.RetainRaw {
					if e.state.Values[scope] == nil {
						e.state.Values[scope] = map[string]string{}
					}
					e.state.Values[scope][token] = value
				}
				return token
			})
		}
		var projected strings.Builder
		start := 0
		for _, span := range placeholder.FindAllStringIndex(text, -1) {
			projected.WriteString(replace(text[start:span[0]]))
			projected.WriteString(text[span[0]:span[1]])
			start = span[1]
		}
		projected.WriteString(replace(text[start:]))
		text = projected.String()
	}
	return text
}

// JSON visits string values without converting JSON numbers to float64.
func (e *Engine) JSON(policy Policy, scope string, body []byte) []byte {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if !json.Valid(body) || decoder.Decode(&value) != nil {
		return []byte(e.Text(policy, scope, string(body)))
	}
	var visit func(any) any
	visit = func(value any) any {
		switch v := value.(type) {
		case string:
			return e.Text(policy, scope, v)
		case []any:
			for i := range v {
				v[i] = visit(v[i])
			}
		case map[string]any:
			for key := range v {
				v[key] = visit(v[key])
			}
		}
		return value
	}
	result, _ := json.Marshal(visit(value))
	return result
}

func (e *Engine) Reveal(scope, body string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.state.Policy.AllowReveal || !e.state.Policy.RetainRaw {
		return "", fmt.Errorf("当前策略未允许原文还原")
	}
	replace := func(text string) string {
		for token, value := range e.state.Values[scope] {
			text = strings.ReplaceAll(text, token, value)
		}
		return text
	}
	if !json.Valid([]byte(body)) {
		return replace(body), nil
	}
	var value any
	d := json.NewDecoder(strings.NewReader(body))
	d.UseNumber()
	if d.Decode(&value) != nil {
		return replace(body), nil
	}
	var visit func(any) any
	visit = func(value any) any {
		switch v := value.(type) {
		case string:
			return replace(v)
		case []any:
			for i := range v {
				v[i] = visit(v[i])
			}
		case map[string]any:
			for k := range v {
				v[k] = visit(v[k])
			}
		}
		return value
	}
	raw, _ := json.Marshal(visit(value))
	return string(raw), nil
}

// RetainAliases preserves only known tokens literally referenced by copied
// text. The caller must verify common identity and both captures' retention
// policies. It does not reveal or rewrite text, or import an entire namespace.
func (e *Engine) RetainAliases(from, to, text string) {
	if from == to {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, token := range placeholder.FindAllString(text, -1) {
		value, known := e.state.Values[from][token]
		if !known {
			continue
		}
		if e.state.Values[to] == nil {
			e.state.Values[to] = map[string]string{}
		}
		if _, retained := e.state.Values[to][token]; !retained {
			e.state.Values[to][token] = value
		}
	}
}

func (e *Engine) ForgetScopes(keep map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for scope := range e.state.Values {
		if !keep[scope] {
			delete(e.state.Values, scope)
		}
	}
}
