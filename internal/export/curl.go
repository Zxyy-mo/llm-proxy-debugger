package export

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

// InlineBodyLimit keeps a single shell argument well below the per-argument
// limits of Linux (128 KiB) and macOS (256 KiB).
const InlineBodyLimit = 64 * 1024

type Destination struct {
	Kind string `json:"kind"` // gateway or upstream
	URL  string `json:"url"`  // credential-free
}

// EnvVar is a placeholder the operator must provide before running the command.
type EnvVar struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Target      string `json:"target"` // header, parameter or "userinfo"
	Scheme      string `json:"scheme,omitempty"`
	Replayable  bool   `json:"replayable"`
	Description string `json:"description"`
}

type Export struct {
	TraceID     string      `json:"trace_id"`
	Source      string      `json:"source"`
	Shell       string      `json:"shell"`
	Destination Destination `json:"destination"`
	Command     string      `json:"command"`
	BodyMode    string      `json:"body_mode"` // none, inline or file
	BodyFile    string      `json:"body_file,omitempty"`
	BodyBytes   int         `json:"body_bytes"`
	Environment []EnvVar    `json:"environment"`
	Notes       []string    `json:"notes"`
}

var (
	ErrNoForwarding = errors.New("this capture predates forwarding metadata and cannot be exported")
	envName         = regexp.MustCompile(`[^A-Z0-9]+`)
	// Characters that stay literal inside POSIX double quotes. `!` is excluded
	// because interactive shells apply history expansion to it.
	doubleQuoteSafe = regexp.MustCompile("^[^\"$`\\\\!\n\r]*$")
)

// BodyBytes returns the exact captured bytes, decoding base64 snapshots.
func BodyBytes(snapshot store.RequestSnapshot) ([]byte, error) {
	if snapshot.BodyEncoding == "base64" {
		return base64.StdEncoding.DecodeString(snapshot.Body)
	}
	return []byte(snapshot.Body), nil
}

// BodyFileName is stable so a downloaded body matches the exported command.
func BodyFileName(traceID, source string, snapshot store.RequestSnapshot) string {
	id := traceID
	if len(id) > 8 {
		id = id[:8]
	}
	extension := ".bin"
	if snapshot.BodyEncoding == "" && snapshot.Forwarding != nil {
		if mediaType := strings.ToLower(snapshot.Forwarding.Headers.Get("Content-Type")); strings.Contains(mediaType, "json") {
			extension = ".json"
		}
	}
	return "replay-" + id + "-" + source + extension
}

// GatewayHost turns a listen address into something a client can dial.
func GatewayHost(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// EnvironmentName derives a deterministic placeholder for one credential.
func EnvironmentName(credential store.Credential) string {
	name := strings.ToUpper(credential.Name)
	switch credential.Kind {
	case "query":
		name = "QUERY_" + name
	case "url":
		name = "URL_" + name
	}
	return "REPLAY_" + strings.Trim(envName.ReplaceAllString(name, "_"), "_")
}

type segment struct {
	literal string
	env     string
}

func literal(text string) segment  { return segment{literal: text} }
func variable(name string) segment { return segment{env: name} }

func singleQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// word renders a shell word from literal and variable segments. Plain words are
// single-quoted so every byte stays data; words that reference a variable use
// double quotes when their literal parts cannot be interpreted, otherwise
// adjacent single-quoted and double-quoted pieces.
func word(segments ...segment) string {
	hasVariable, safe := false, true
	for _, s := range segments {
		if s.env != "" {
			hasVariable = true
		} else if !doubleQuoteSafe.MatchString(s.literal) {
			safe = false
		}
	}
	if !hasVariable {
		var text strings.Builder
		for _, s := range segments {
			text.WriteString(s.literal)
		}
		return singleQuote(text.String())
	}
	var out strings.Builder
	if safe {
		out.WriteString(`"`)
		for _, s := range segments {
			if s.env != "" {
				out.WriteString("${" + s.env + "}")
			} else {
				out.WriteString(s.literal)
			}
		}
		out.WriteString(`"`)
		return out.String()
	}
	for _, s := range segments {
		if s.env != "" {
			out.WriteString(`"${` + s.env + `}"`)
		} else if s.literal != "" {
			out.WriteString(singleQuote(s.literal))
		}
	}
	return out.String()
}

type Options struct {
	// GatewayHost is used for original snapshots whose Host header was empty.
	GatewayHost string
}

// Build renders a POSIX shell curl command for one snapshot. Credentials become
// environment placeholders; bodies that cannot be a shell argument are exported
// as a file the command references by name.
func Build(traceID, source string, snapshot store.RequestSnapshot, opts Options) (Export, error) {
	forwarding := snapshot.Forwarding
	if forwarding == nil {
		return Export{}, ErrNoForwarding
	}
	body, err := BodyBytes(snapshot)
	if err != nil {
		return Export{}, fmt.Errorf("decode captured body: %w", err)
	}
	result := Export{TraceID: traceID, Source: source, Shell: "posix-sh", BodyBytes: len(body), Environment: []EnvVar{}, Notes: []string{}}
	result.Destination.Kind = "upstream"
	host := forwarding.Host
	if source == "original" {
		result.Destination.Kind = "gateway"
		if host == "" {
			host = opts.GatewayHost
		}
	}
	if host == "" {
		return Export{}, errors.New("the capture has no destination host")
	}
	base := url.URL{Scheme: forwarding.Scheme, Host: host, Path: forwarding.Path, RawQuery: forwarding.Query}
	result.Destination.URL = base.String()

	used := make(map[string]bool)
	environment := func(credential store.Credential, description string) string {
		name := EnvironmentName(credential)
		for i := 2; used[name]; i++ {
			name = fmt.Sprintf("%s_%d", EnvironmentName(credential), i)
		}
		used[name] = true
		result.Environment = append(result.Environment, EnvVar{
			Name: name, Kind: credential.Kind, Target: credential.Name, Scheme: credential.Scheme,
			Replayable: credential.Replayable, Description: description,
		})
		return name
	}

	urlSegments := []segment{literal(base.String())}
	var lines []string
	var userinfo string
	for _, credential := range snapshot.Credentials {
		switch credential.Kind {
		case "query":
			name := environment(credential, fmt.Sprintf("查询参数 %s 的值", credential.Name))
			separator := "?"
			if strings.Contains(urlSegments[0].literal, "?") || len(urlSegments) > 1 {
				separator = "&"
			}
			urlSegments = append(urlSegments, literal(separator+url.QueryEscape(credential.Name)+"="), variable(name))
		case "url":
			userinfo = environment(credential, "URL 中的 user:password")
		}
	}
	lines = append(lines, "curl -X "+singleQuote(snapshot.Method)+" "+word(urlSegments...))
	if userinfo != "" {
		lines = append(lines, "  --user "+word(variable(userinfo)))
	}

	names := make([]string, 0, len(forwarding.Headers))
	for name := range forwarding.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, value := range forwarding.Headers[name] {
			lines = append(lines, "  -H "+word(literal(name+": "+value)))
		}
	}
	for _, credential := range snapshot.Credentials {
		if credential.Kind != "header" {
			continue
		}
		description := fmt.Sprintf("%s 请求头的完整值", credential.Name)
		prefix := ""
		if credential.Scheme != "" {
			description = fmt.Sprintf("%s 请求头中 %s 之后的凭证", credential.Name, credential.Scheme)
			prefix = credential.Scheme + " "
		}
		name := environment(credential, description)
		lines = append(lines, "  -H "+word(literal(credential.Name+": "+prefix), variable(name)))
		if !credential.Replayable {
			result.Notes = append(result.Notes, fmt.Sprintf("%s 使用 %s 签名方案，静态凭证无法直接重放，需要按上游要求重新签名。", credential.Name, credential.Scheme))
		}
	}

	// curl adds its own defaults; suppress them when the capture lacked them so
	// the upstream sees the same header set.
	if forwarding.Headers.Get("Accept") == "" {
		lines = append(lines, "  -H 'Accept:'")
	}
	if forwarding.Headers.Get("User-Agent") == "" {
		lines = append(lines, "  -H 'User-Agent:'")
	}
	switch {
	case len(body) == 0:
		result.BodyMode = "none"
	case snapshot.BodyEncoding == "" && len(body) <= InlineBodyLimit && utf8.Valid(body) && !strings.ContainsRune(string(body), 0):
		result.BodyMode = "inline"
	default:
		result.BodyMode = "file"
		result.BodyFile = BodyFileName(traceID, source, snapshot)
	}
	if result.BodyMode != "none" {
		if forwarding.Headers.Get("Content-Type") == "" {
			lines = append(lines, "  -H 'Content-Type:'")
		}
		lines = append(lines, "  -H 'Expect:'")
		if result.BodyMode == "inline" {
			lines = append(lines, "  --data-binary "+singleQuote(string(body)))
		} else {
			lines = append(lines, "  --data-binary "+singleQuote("@"+result.BodyFile))
		}
	}
	result.Command = strings.Join(lines, " \\\n")

	if result.Destination.Kind == "gateway" {
		result.Notes = append(result.Notes, "目标是网关地址：请求会再次经过动态规则并被记录为新的调用。")
	} else {
		result.Notes = append(result.Notes, "目标是实际上游地址：请求不经过网关，不会被记录；已包含上游路径前缀和已应用的规则修改。")
	}
	if len(result.Environment) > 0 {
		result.Notes = append(result.Notes, "凭证以环境变量占位，请在运行前设置；不要粘贴界面中遮盖后的值。")
	}
	if result.BodyMode == "file" {
		reason := "正文超过 64 KiB"
		if snapshot.BodyEncoding != "" {
			reason = "正文是压缩或二进制数据"
		}
		result.Notes = append(result.Notes, reason+"，无法作为命令行参数保真传递：请下载正文文件 "+result.BodyFile+"，并在该文件所在目录运行命令。")
	}
	result.Notes = append(result.Notes, "Content-Length、Host、逐跳请求头和网关添加的 X-Forwarded-* 由 curl 重新生成或已省略；其余请求头保持原值。命令面向 POSIX shell（sh/bash/zsh）。")
	return result, nil
}
