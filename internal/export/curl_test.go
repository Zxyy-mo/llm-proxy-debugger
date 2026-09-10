package export

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func snapshotOf(t *testing.T, method, target string, body []byte, headers http.Header) store.RequestSnapshot {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	for name, values := range headers {
		request.Header[name] = values
	}
	forwarding, credentials := Classify(request)
	snapshot := store.RequestSnapshot{
		Method: method, URL: request.URL.Path, Body: string(body), ContentLength: int64(len(body)),
		Credentials: credentials, Forwarding: forwarding,
	}
	if headers.Get("Content-Encoding") != "" {
		snapshot.Body = base64.StdEncoding.EncodeToString(body)
		snapshot.BodyEncoding = "base64"
	}
	return snapshot
}

// shellWords is a tiny POSIX word splitter for the command shapes we generate:
// single quotes, double quotes with ${VAR} references and backslash-newline
// continuations. It fails loudly on anything else so the exporter cannot drift
// into constructs the test does not model.
func shellWords(t *testing.T, command string, env map[string]string) []string {
	t.Helper()
	var words []string
	var current strings.Builder
	inWord := false
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case c == '\\' && i+1 < len(runes) && runes[i+1] == '\n':
			i++
		case c == '\\' && i+1 < len(runes):
			// Backslash outside quotes escapes exactly one character ('\'' idiom).
			inWord = true
			current.WriteRune(runes[i+1])
			i++
		case c == ' ' || c == '\n':
			if inWord {
				words = append(words, current.String())
				current.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			i++
			for ; i < len(runes) && runes[i] != '\''; i++ {
				current.WriteRune(runes[i])
			}
			if i >= len(runes) {
				t.Fatal("unterminated single quote")
			}
		case c == '"':
			inWord = true
			i++
			for ; i < len(runes) && runes[i] != '"'; i++ {
				if runes[i] == '$' {
					if i+1 >= len(runes) || runes[i+1] != '{' {
						t.Fatalf("unbraced variable in %q", command)
					}
					end := i + 2
					for ; end < len(runes) && runes[end] != '}'; end++ {
					}
					name := string(runes[i+2 : end])
					value, ok := env[name]
					if !ok {
						t.Fatalf("command references unexpected variable %s", name)
					}
					current.WriteString(value)
					i = end
					continue
				}
				if strings.ContainsRune("`\\!", runes[i]) {
					t.Fatalf("unsafe character inside double quotes: %q", command)
				}
				current.WriteRune(runes[i])
			}
			if i >= len(runes) {
				t.Fatal("unterminated double quote")
			}
		default:
			if strings.ContainsRune("$`\\!\"'*?[]{}()<>|&;", c) {
				t.Fatalf("unquoted shell metacharacter %q in %q", c, command)
			}
			inWord = true
			current.WriteRune(c)
		}
	}
	if inWord {
		words = append(words, current.String())
	}
	return words
}

func requestFromWords(t *testing.T, words []string) (method, target string, headers http.Header, body []byte, file string) {
	t.Helper()
	headers = make(http.Header)
	if words[0] != "curl" {
		t.Fatalf("command must start with curl: %v", words)
	}
	for i := 1; i < len(words); i++ {
		switch words[i] {
		case "-X":
			method = words[i+1]
			i++
		case "-H":
			name, value, _ := strings.Cut(words[i+1], ":")
			headers[http.CanonicalHeaderKey(name)] = append(headers[http.CanonicalHeaderKey(name)], strings.TrimPrefix(value, " "))
			i++
		case "--data-binary":
			if strings.HasPrefix(words[i+1], "@") {
				file = words[i+1][1:]
			} else {
				body = []byte(words[i+1])
			}
			i++
		case "--user":
			headers.Set("X-Test-User", words[i+1])
			i++
		default:
			if target != "" {
				t.Fatalf("unexpected word %q", words[i])
			}
			target = words[i]
		}
	}
	return method, target, headers, body, file
}

func TestCurlExportQuotesUntrustedContentAndPlaceholdersCredentials(t *testing.T) {
	body := []byte("{\"model\":\"m\",\"input\":\"it's $HOME `id` \\\\n \\\"quoted\\\" 你好 ! 9007199254740993\",\n\"stream\":true}")
	headers := http.Header{
		"Content-Type":    {"application/json"},
		"Authorization":   {"Bearer sk-live-secret"},
		"X-Api-Key":       {"anthropic-secret"},
		"Anthropic-Beta":  {"tools-2024-04-04", "pdfs-2024-09-25"},
		"X-Custom":        {"value with 'quotes' and $dollar"},
		"Connection":      {"keep-alive, X-Hop"},
		"X-Hop":           {"dropped"},
		"X-Forwarded-For": {"10.0.0.1"},
		"Content-Length":  {"999"},
	}
	snapshot := snapshotOf(t, http.MethodPost, "http://gateway.local:12337/v1/responses?trace=1&key=query-secret&api_key=another", body, headers)
	if snapshot.Forwarding.Query != "trace=1" {
		t.Fatalf("secret parameters remained in the forwarding query: %q", snapshot.Forwarding.Query)
	}
	if RedactQuery("trace=1&key=query-secret&api_key=another") != "trace=1&key=****&api_key=****" {
		t.Fatal("display query was not redacted in place")
	}
	result, err := Build("0123456789abcdef", "original", snapshot, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sk-live-secret", "anthropic-secret", "query-secret", "another"} {
		if strings.Contains(result.Command, secret) {
			t.Fatalf("command leaked %q: %s", secret, result.Command)
		}
	}
	if result.Destination.Kind != "gateway" || result.Destination.URL != "http://gateway.local:12337/v1/responses?trace=1" || result.BodyMode != "inline" {
		t.Fatalf("unexpected destination/body mode: %+v", result.Destination)
	}
	env := map[string]string{}
	for _, item := range result.Environment {
		env[item.Name] = "value-of-" + item.Name
	}
	if len(env) != 4 || env["REPLAY_AUTHORIZATION"] == "" || env["REPLAY_X_API_KEY"] == "" || env["REPLAY_QUERY_KEY"] == "" || env["REPLAY_QUERY_API_KEY"] == "" {
		t.Fatalf("unexpected environment: %+v", result.Environment)
	}
	method, target, parsedHeaders, parsedBody, file := requestFromWords(t, shellWords(t, result.Command, env))
	if method != "POST" || file != "" || !bytes.Equal(parsedBody, body) {
		t.Fatalf("body or method not preserved: %q %q", method, parsedBody)
	}
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Host != "gateway.local:12337" || parsed.Path != "/v1/responses" || query.Get("trace") != "1" || query.Get("key") != "value-of-REPLAY_QUERY_KEY" || query.Get("api_key") != "value-of-REPLAY_QUERY_API_KEY" {
		t.Fatalf("destination lost query placeholders: %s", target)
	}
	if parsedHeaders.Get("Authorization") != "Bearer value-of-REPLAY_AUTHORIZATION" || parsedHeaders.Get("X-Api-Key") != "value-of-REPLAY_X_API_KEY" {
		t.Fatalf("credential headers not rendered as placeholders: %v", parsedHeaders)
	}
	if got := parsedHeaders.Values("Anthropic-Beta"); len(got) != 2 || got[0] != "tools-2024-04-04" || got[1] != "pdfs-2024-09-25" {
		t.Fatalf("multi-value header lost: %v", got)
	}
	if parsedHeaders.Get("X-Custom") != "value with 'quotes' and $dollar" {
		t.Fatalf("untrusted header value changed: %q", parsedHeaders.Get("X-Custom"))
	}
	for _, dropped := range []string{"X-Hop", "Connection", "X-Forwarded-For", "Content-Length"} {
		if parsedHeaders.Get(dropped) != "" {
			t.Fatalf("hop-by-hop or generated header %s was exported", dropped)
		}
	}
	// curl defaults that the capture lacked are explicitly removed.
	if _, ok := parsedHeaders["Accept"]; !ok || parsedHeaders.Get("Accept") != "" || parsedHeaders.Get("User-Agent") != "" || parsedHeaders.Get("Expect") != "" {
		t.Fatalf("curl default headers were not suppressed: %v", parsedHeaders)
	}
	if _, ok := parsedHeaders["Content-Type"]; !ok || parsedHeaders.Get("Content-Type") != "application/json" {
		t.Fatal("content type missing")
	}
}

func TestCurlExportUsesBodyFilesForLargeAndBinaryPayloads(t *testing.T) {
	large := []byte(`{"model":"m","input":"` + strings.Repeat("x", InlineBodyLimit) + `"}`)
	snapshot := snapshotOf(t, http.MethodPost, "http://127.0.0.1:12337/v1/responses", large, http.Header{"Content-Type": {"application/json"}})
	result, err := Build("abcdefgh-1234", "original", snapshot, Options{})
	if err != nil || result.BodyMode != "file" || result.BodyFile != "replay-abcdefgh-original.json" {
		t.Fatalf("large body should export as a file: %+v %v", result, err)
	}
	_, _, _, body, file := requestFromWords(t, shellWords(t, result.Command, nil))
	if body != nil || file != result.BodyFile {
		t.Fatalf("command does not reference the body file: %q %q", body, file)
	}

	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	writer.Write([]byte(`{"model":"m"}`))
	writer.Close()
	compressed := snapshotOf(t, http.MethodPost, "http://127.0.0.1:12337/v1/responses", encoded.Bytes(), http.Header{"Content-Type": {"application/json"}, "Content-Encoding": {"gzip"}})
	result, err = Build("abcdefgh-1234", "original", compressed, Options{})
	if err != nil || result.BodyMode != "file" || result.BodyFile != "replay-abcdefgh-original.bin" || result.BodyBytes != encoded.Len() {
		t.Fatalf("compressed body should export as a binary file: %+v %v", result, err)
	}
	decoded, err := BodyBytes(compressed)
	if err != nil || !bytes.Equal(decoded, encoded.Bytes()) {
		t.Fatal("body download would not return the original bytes")
	}
	if !strings.Contains(result.Command, "'Content-Encoding: gzip'") {
		t.Fatal("content encoding must accompany compressed bodies")
	}

	empty := snapshotOf(t, http.MethodGet, "http://127.0.0.1:12337/v1/models", nil, nil)
	result, err = Build("trace", "original", empty, Options{GatewayHost: "127.0.0.1:12337"})
	if err != nil || result.BodyMode != "none" || strings.Contains(result.Command, "--data-binary") || strings.Contains(result.Command, "Expect") {
		t.Fatalf("empty body should not add data flags: %+v %v", result, err)
	}
}

func TestOutgoingDestinationAndSignedCredentials(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://api.example.com/base/v1/chat/completions?stream=false", strings.NewReader(`{"model":"m"}`))
	request.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=abc/20260909, SignedHeaders=host, Signature=deadbeef")
	request.Header.Set("Content-Type", "application/json")
	request.URL.User = url.UserPassword("user", "pass")
	forwarding, credentials := Classify(request)
	snapshot := store.RequestSnapshot{Method: http.MethodPost, Body: `{"model":"m"}`, Credentials: credentials, Forwarding: forwarding}
	result, err := Build("trace", "outgoing", snapshot, Options{GatewayHost: "127.0.0.1:12337"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Destination.Kind != "upstream" || result.Destination.URL != "https://api.example.com/base/v1/chat/completions?stream=false" {
		t.Fatalf("outgoing destination must keep the upstream prefix without userinfo: %+v", result.Destination)
	}
	if strings.Contains(result.Command, "deadbeef") || strings.Contains(result.Command, "user:pass") || strings.Contains(result.Command, "pass@") {
		t.Fatalf("signed credential or userinfo leaked: %s", result.Command)
	}
	var signed, userinfo bool
	for _, item := range result.Environment {
		if item.Target == "Authorization" && item.Scheme == "AWS4-HMAC-SHA256" && !item.Replayable {
			signed = true
		}
		if item.Kind == "url" && item.Name == "REPLAY_URL_USERINFO" {
			userinfo = true
		}
	}
	if !signed || !userinfo {
		t.Fatalf("environment did not describe signed auth and userinfo: %+v", result.Environment)
	}
	if !strings.Contains(strings.Join(result.Notes, "\n"), "AWS4-HMAC-SHA256") || !strings.Contains(result.Command, "--user") {
		t.Fatalf("notes/command missing signing warning or --user: %+v", result)
	}
	if _, err := Build("trace", "outgoing", store.RequestSnapshot{Method: "POST"}, Options{}); err != ErrNoForwarding {
		t.Fatal("captures without forwarding metadata must be rejected explicitly")
	}
	if GatewayHost("0.0.0.0:12337") != "127.0.0.1:12337" || GatewayHost(":9000") != "127.0.0.1:9000" || GatewayHost("[::]:1") != "127.0.0.1:1" {
		t.Fatal("unspecified listen addresses must map to loopback")
	}
}

func TestClassificationHeuristics(t *testing.T) {
	for name, credential := range map[string]bool{
		"Authorization": true, "x-api-key": true, "X-Goog-Api-Key": true, "Cookie": true, "X-Auth-Token": true, "Proxy-Authorization": true,
		"X-Session-ID": false, "X-Parent-Trace-ID": false, "Idempotency-Key": false, "Anthropic-Version": false, "Content-Type": false, "User-Agent": false,
	} {
		if IsCredentialHeader(name) != credential {
			t.Fatalf("IsCredentialHeader(%q) = %v", name, !credential)
		}
	}
	for name, secret := range map[string]bool{"key": true, "api_key": true, "access_token": true, "sig": true, "stream": false, "model": false, "session_id": false, "api-version": false} {
		if IsSecretParameter(name) != secret {
			t.Fatalf("IsSecretParameter(%q) = %v", name, !secret)
		}
	}
	bearer := HeaderCredential("authorization", "Bearer abc")
	raw := HeaderCredential("x-api-key", "sk-ant-raw")
	digest := HeaderCredential("Authorization", `Digest username="u", response="r"`)
	if bearer.Scheme != "Bearer" || !bearer.Replayable || raw.Scheme != "" || !raw.Replayable || digest.Replayable || raw.Name != "X-Api-Key" {
		t.Fatalf("scheme detection wrong: %+v %+v %+v", bearer, raw, digest)
	}
}

// TestCurlCommandReproducesRequest runs the generated command with the real curl
// binary against a local server and compares what arrives byte for byte.
func TestCurlCommandReproducesRequest(t *testing.T) {
	curl, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl is not installed")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh is not installed")
	}
	type received struct {
		method, path, query, auth, key, custom, contentType string
		accept, userAgent                                   []string
		body                                                []byte
	}
	hits := make(chan received, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		hits <- received{
			method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization"), key: r.Header.Get("X-Api-Key"),
			custom: r.Header.Get("X-Custom"), contentType: r.Header.Get("Content-Type"), accept: r.Header["Accept"], userAgent: r.Header["User-Agent"], body: body,
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	dir := t.TempDir()
	run := func(t *testing.T, result Export, env []string) received {
		t.Helper()
		cmd := exec.Command(sh, "-c", result.Command)
		cmd.Dir = dir
		cmd.Env = append(append(os.Environ(), "PATH="+filepath.Dir(curl)+":"+os.Getenv("PATH")), env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("curl failed: %v\n%s\n%s", err, out, result.Command)
		}
		return <-hits
	}

	body := []byte("{\"model\":\"m\",\"input\":\"it's $HOME `id` \\\\ \\\"q\\\" 你好 ! 9007199254740993\",\n\"stream\":true}")
	headers := http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer real-secret"}, "X-Api-Key": {"raw-secret"}, "X-Custom": {"a 'b' $c"}}
	inline := snapshotOf(t, http.MethodPost, "http://"+host+"/v1/responses?trace=1&key=qsecret", body, headers)
	result, err := Build("trace-inline", "original", inline, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, result, []string{"REPLAY_AUTHORIZATION=token-from-env", "REPLAY_X_API_KEY=key-from-env", "REPLAY_QUERY_KEY=query-from-env"})
	if got.method != "POST" || got.path != "/v1/responses" || !bytes.Equal(got.body, body) {
		t.Fatalf("inline body not reproduced: %+v", got)
	}
	if got.auth != "Bearer token-from-env" || got.key != "key-from-env" || got.custom != "a 'b' $c" || got.contentType != "application/json" {
		t.Fatalf("headers not reproduced: %+v", got)
	}
	if values, _ := url.ParseQuery(got.query); values.Get("trace") != "1" || values.Get("key") != "query-from-env" {
		t.Fatalf("query not reproduced: %q", got.query)
	}
	if len(got.accept) != 0 || len(got.userAgent) != 0 {
		t.Fatalf("curl defaults leaked into the request: accept=%v ua=%v", got.accept, got.userAgent)
	}

	large := []byte(`{"model":"m","input":"` + strings.Repeat("大", InlineBodyLimit) + `"}`)
	file := snapshotOf(t, http.MethodPost, "http://"+host+"/v1/responses", large, http.Header{"Content-Type": {"application/json"}})
	result, err = Build("trace-file", "original", file, Options{})
	if err != nil || result.BodyMode != "file" {
		t.Fatalf("expected file mode: %+v %v", result, err)
	}
	bytesOut, _ := BodyBytes(file)
	if err := os.WriteFile(filepath.Join(dir, result.BodyFile), bytesOut, 0o600); err != nil {
		t.Fatal(err)
	}
	got = run(t, result, nil)
	if !bytes.Equal(got.body, large) {
		t.Fatalf("file body not reproduced: %d bytes", len(got.body))
	}
}
