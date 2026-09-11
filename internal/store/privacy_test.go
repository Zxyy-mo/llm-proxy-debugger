package store

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/privacy"
)

var emailPlaceholder = regexp.MustCompile(`\[PRIVATE_email_[a-f0-9]{20}\]`)

func recordingPolicy(t *testing.T, s *Store) privacy.Policy {
	t.Helper()
	p := s.Privacy.Policy()
	p.Record, p.Outbound, p.AllowReveal = true, true, true
	if err := s.Privacy.Set(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func privacyRequest(s *Store, trace, body string, headers http.Header, replay *ReplayInfo) RequestLog {
	const path = "/v1/responses"
	input := correlation.ExtractRequest(headers, path, []byte(body), "http://privacy.test")
	return s.BeginWithBody(RequestLog{TraceID: trace, Method: "POST", Path: path, Replay: replay}, input, []byte(body), 4096)
}

func placeholderIn(t *testing.T, text string) string {
	t.Helper()
	token := emailPlaceholder.FindString(text)
	if token == "" {
		t.Fatalf("missing email placeholder: %s", text)
	}
	return token
}

func restoredText(t *testing.T, s *Store, trace, body string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"trace_id": trace, "body": body})
	w := httptest.NewRecorder()
	s.PrivacyHandler(w, httptest.NewRequest("POST", "/api/privacy/restore", strings.NewReader(string(raw))))
	if w.Code != http.StatusOK {
		t.Fatalf("restore failed: %d %s", w.Code, w.Body)
	}
	var result struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Body
}

func finishPrivate(s *Store, log RequestLog, responseID string) {
	log.StatusCode = 200
	s.Complete(log.TraceID, log, correlation.Response{ID: responseID, Complete: true})
}

func TestConversationPrivacyAdmissionAndReplayBoundaries(t *testing.T) {
	s := New()
	recordingPolicy(t, s)
	const body = `{"input":"same@example.com"}`
	headers := http.Header{"X-Session-Id": {"one"}, "Authorization": {"Bearer identity-one"}}
	first := privacyRequest(s, "first", body, headers, nil)
	second := privacyRequest(s, "second", body, headers, nil)
	firstToken := placeholderIn(t, first.RequestBody)
	if placeholderIn(t, second.RequestBody) != firstToken {
		t.Fatal("same conversation did not reuse its placeholder")
	}
	for _, tc := range []struct {
		name    string
		headers http.Header
		replay  *ReplayInfo
	}{
		{"different-session", http.Header{"X-Session-Id": {"two"}, "Authorization": {"Bearer identity-one"}}, nil},
		{"different-identity", http.Header{"X-Session-Id": {"one"}, "Authorization": {"Bearer identity-two"}}, nil},
		{"replay-different-identity", http.Header{"Authorization": {"Bearer identity-two"}}, &ReplayInfo{Of: first.TraceID}},
		{"replay-explicit-session", http.Header{"X-Session-Id": {"two"}, "Authorization": {"Bearer identity-one"}}, &ReplayInfo{Of: first.TraceID}},
	} {
		other := privacyRequest(s, tc.name, body, tc.headers, tc.replay)
		if placeholderIn(t, other.RequestBody) == firstToken || restoredText(t, s, other.TraceID, firstToken) != firstToken {
			t.Fatalf("privacy crossed %s", tc.name)
		}
	}
	if restoredText(t, s, first.TraceID, firstToken) != "same@example.com" {
		t.Fatal("own conversation mapping was not retained")
	}
	replayed := privacyRequest(s, "replay", body, headers, &ReplayInfo{Of: first.TraceID})
	if placeholderIn(t, replayed.RequestBody) != firstToken {
		t.Fatal("same-identity replay changed placeholder")
	}
	anonymous := privacyRequest(s, "anonymous-one", body, nil, nil)
	otherAnonymous := privacyRequest(s, "anonymous-two", body, nil, nil)
	if placeholderIn(t, anonymous.RequestBody) == placeholderIn(t, otherAnonymous.RequestBody) {
		t.Fatal("unrelated anonymous requests shared a namespace")
	}
	s.ObserveResponse(anonymous.TraceID, correlation.Response{ID: "response-parent", ConversationID: "conversation-one"})
	for trace, relatedBody := range map[string]string{
		"response-child": `{"previous_response_id":"response-parent","input":"same@example.com"}`,
		"alias-child":    `{"conversation":"conversation-one","input":"same@example.com"}`,
	} {
		related := privacyRequest(s, trace, relatedBody, nil, nil)
		if related.SessionID != anonymous.SessionID || placeholderIn(t, related.RequestBody) != placeholderIn(t, anonymous.RequestBody) {
			t.Fatalf("admission did not resolve related scope: %+v", related)
		}
	}
	lateAlias := privacyRequest(s, "late-alias", body, nil, nil)
	lateToken := placeholderIn(t, lateAlias.RequestBody)
	s.ObserveResponse(lateAlias.TraceID, correlation.Response{ConversationID: "conversation-one"})
	merged, _ := s.Log(lateAlias.TraceID)
	if merged.SessionID != anonymous.SessionID || placeholderIn(t, merged.RequestBody) != lateToken || restoredText(t, s, lateAlias.TraceID, lateToken) != "same@example.com" {
		t.Fatal("response conversation alias changed historical namespace")
	}
}

func TestPrivateAdmissionProjectsBeforeTruncationAndLeavesNoRawLabels(t *testing.T) {
	s := New()
	p := recordingPolicy(t, s)
	p.RetainRaw, p.AllowReveal = false, false
	if err := s.Privacy.Set(p); err != nil {
		t.Fatal(err)
	}
	const path = "/v1/chat/completions"
	secret := strings.Repeat("private-fragment", 12) + "@example.com"
	parentBody := `{"messages":[{"role":"user","content":"first"}]}`
	input := correlation.ExtractRequest(nil, path, []byte(parentBody), "http://privacy.test")
	parent := s.BeginWithBody(RequestLog{TraceID: "parent", Method: "POST", Path: path}, input, []byte(parentBody), 64)
	response := correlation.NewResponseCapture("openai")
	response.JSON([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`))
	parent.StatusCode = 200
	s.Complete(parent.TraceID, parent, response.Response())
	body := `{"messages":[{"role":"user","content":"first"},{"role":"assistant","content":"answer"},{"role":"user","content":"` + secret + `"}]}`
	input = correlation.ExtractRequest(nil, path, []byte(body), "http://privacy.test")
	child := s.BeginWithBody(RequestLog{TraceID: "child", Method: "POST", Path: path}, input, []byte(body), 150)
	s.CaptureOriginal(child.TraceID, RequestSnapshot{Body: body})
	if child.SessionID != parent.SessionID || child.Correlation.ParentTraceID != parent.TraceID {
		t.Fatalf("history did not resolve before projection: %+v", child)
	}
	if _, exists := s.sessions["auto:child"]; exists {
		t.Fatal("provisional session survived admission")
	}
	payload, err := s.snapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "private-fragment") || strings.Contains(child.Summary, "private-fragment") || strings.Contains(child.RequestBody, "private-fragment") {
		t.Fatalf("truncation left raw fragments in a capture or label: %s", payload)
	}
	if !strings.Contains(child.Summary, "PRIVATE_email") || !strings.Contains(child.RequestBody, "truncated") {
		t.Fatalf("full projection or bounded preview missing: %+v", child)
	}
	if len(s.Privacy.Snapshot().Values) != 0 {
		t.Fatal("no-retain admission saved raw mappings")
	}
}

func TestPrivacyNamespaceSurvivesLateMergeDetachAndReplay(t *testing.T) {
	s := New()
	recordingPolicy(t, s)
	const childBody = `{"previous_response_id":"late-response","input":"same@example.com"}`
	child := privacyRequest(s, "child", childBody, nil, nil)
	childToken := placeholderIn(t, child.RequestBody)
	_, childScope := s.PrivacyContext(child.TraceID)
	s.CaptureOriginal(child.TraceID, RequestSnapshot{Body: childBody})
	outgoing := string(s.Outbound(child.TraceID, []byte(childBody)))
	s.CaptureOutgoing(child.TraceID, RequestSnapshot{Body: outgoing})
	finishPrivate(s, child, "child-response")
	grandchild := privacyRequest(s, "grandchild", `{"previous_response_id":"child-response","input":"same@example.com"}`, nil, nil)
	parent := privacyRequest(s, "late-parent", `{"input":"same@example.com"}`, nil, nil)
	s.ObserveResponse(parent.TraceID, correlation.Response{ID: "late-response"})
	for _, trace := range []string{child.TraceID, grandchild.TraceID} {
		log, _ := s.Log(trace)
		_, scope := s.PrivacyContext(trace)
		if log.SessionID != parent.SessionID || scope != childScope || placeholderIn(t, log.RequestBody) != childToken {
			t.Fatalf("late grouping rewrote historical privacy for %s", trace)
		}
	}
	if restoredText(t, s, child.TraceID, childToken) != "same@example.com" {
		t.Fatal("late merge broke historical restoration")
	}
	payload, err := s.snapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	restored := New()
	if err := restored.restore(payload); err != nil {
		t.Fatal(err)
	}
	_, restoredScope := restored.PrivacyContext(child.TraceID)
	if restoredScope != childScope || restoredText(t, restored, child.TraceID, childToken) != "same@example.com" {
		t.Fatal("restart recomputed the scope of a moved capture")
	}
	fresh := privacyRequest(s, "fresh", `{"previous_response_id":"child-response","input":"same@example.com"}`, nil, nil)
	if placeholderIn(t, fresh.RequestBody) != placeholderIn(t, parent.RequestBody) {
		t.Fatal("new admission did not use resolved conversation")
	}
	replayed := privacyRequest(s, "replay", childBody, nil, &ReplayInfo{Of: child.TraceID})
	if placeholderIn(t, replayed.RequestBody) != childToken {
		t.Fatal("source replay lost its immutable modern namespace")
	}
	duplicate := privacyRequest(s, "duplicate", `{"input":"other"}`, nil, nil)
	s.ObserveResponse(duplicate.TraceID, correlation.Response{ID: "late-response"})
	log, _ := s.Log(child.TraceID)
	_, scope := s.PrivacyContext(child.TraceID)
	if log.Correlation.Warning != "ambiguous_parent" || log.Correlation.ParentTraceID != "" || scope != childScope {
		t.Fatalf("detach changed privacy or left a false parent: %+v", log.Correlation)
	}
	capture, _ := s.RequestSnapshot(child.TraceID)
	if capture.Outgoing == nil || capture.Outgoing.Body != outgoing {
		t.Fatal("grouping changes rewrote captured outgoing bytes")
	}
}

func TestPrivacyHistoryUsesEditedInputAndNeverAnchorsCancellation(t *testing.T) {
	s := New()
	p := recordingPolicy(t, s)
	p.RetainRaw, p.AllowReveal = false, false
	if err := s.Privacy.Set(p); err != nil {
		t.Fatal(err)
	}
	const path = "/v1/chat/completions"
	parse := func(body string) correlation.Request {
		return correlation.ExtractRequest(nil, path, []byte(body), "http://privacy.test")
	}
	const original = `{"messages":[{"role":"user","content":"stale@example.com"}]}`
	const edited = `{"messages":[{"role":"user","content":"current@example.com"}]}`
	log := s.BeginWithBody(RequestLog{TraceID: "edited", Method: "POST", Path: path}, parse(original), []byte(original), 4096)
	s.UpdateInput(log.TraceID, parse(edited))
	outgoing := s.Outbound(log.TraceID, []byte(edited))
	s.UpdateOutboundInput(log.TraceID, parse(string(outgoing)))
	fingerprints, _ := json.Marshal(s.records[log.TraceID].privacyHistory)
	if strings.Contains(string(fingerprints), "current") || strings.Contains(string(fingerprints), "PRIVATE_") {
		t.Fatal("pre-substitution history kept text instead of fingerprints")
	}
	response := correlation.NewResponseCapture("openai")
	response.JSON([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`))
	log.StatusCode = 200
	s.Complete(log.TraceID, log, response.Response())
	if s.records[log.TraceID].privacyHistory != nil {
		t.Fatal("completed privacy history retained per-message fingerprints")
	}
	for _, tc := range []struct{ trace, first, parent string }{
		{"stale-child", "stale@example.com", ""},
		{"edited-child", "current@example.com", log.TraceID},
	} {
		body := `{"messages":[{"role":"user","content":"` + tc.first + `"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`
		child := s.BeginWithBody(RequestLog{TraceID: tc.trace, Method: "POST", Path: path}, parse(body), []byte(body), 4096)
		if child.Correlation.ParentTraceID != tc.parent {
			t.Fatalf("edited privacy history linked %s to %q", tc.trace, child.Correlation.ParentTraceID)
		}
	}
	const canceledBody = `{"messages":[{"role":"user","content":"canceled@example.com"}]}`
	canceled := s.BeginWithBody(RequestLog{TraceID: "canceled", Method: "POST", Path: path}, parse(canceledBody), []byte(canceledBody), 4096)
	s.UpdateOutboundInput(canceled.TraceID, parse(string(s.Outbound(canceled.TraceID, []byte(canceledBody)))))
	s.MarkCanceled(canceled.TraceID)
	canceled.StatusCode = 200
	s.Complete(canceled.TraceID, canceled, response.Response())
	const canceledHistory = `{"messages":[{"role":"user","content":"canceled@example.com"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`
	child := s.BeginWithBody(RequestLog{TraceID: "after-cancel", Method: "POST", Path: path}, parse(canceledHistory), []byte(canceledHistory), 4096)
	if child.Correlation.ParentTraceID != "" {
		t.Fatal("canceled request became a privacy history anchor")
	}
}

func TestCleanupPreservesSurvivorResponseMappingAcrossRestart(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, "gateway.db")
	s, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	p := recordingPolicy(t, s)
	headers := http.Header{"X-Session-Id": {"shared"}}
	first := privacyRequest(s, "first", `{"input":"request@example.com"}`, headers, nil)
	survivor := privacyRequest(s, "survivor", `{"input":"request@example.com"}`, headers, nil)
	unrelated := privacyRequest(s, "unrelated", `{"input":"other@example.com"}`, nil, nil)
	_, scope := s.PrivacyContext(survivor.TraceID)
	// This value occurs only in a response projection, never in raw request/log
	// metadata. Reading a survivor cannot recreate a wrongly deleted mapping.
	token := s.Privacy.Text(p, scope, "response-only@example.com")
	responsePath := filepath.Join(root, "survivor.response")
	if err := os.WriteFile(responsePath, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	s.CaptureResponse(survivor.TraceID, ResponseSnapshot{Path: responsePath, Bytes: int64(len(token)), Stored: "file", Complete: true})
	finishPrivate(s, first, "")
	finishPrivate(s, survivor, "")
	finishPrivate(s, unrelated, "")
	if _, _, err = s.cleanup(func(log RequestLog) bool { return log.TraceID == first.TraceID }); err != nil {
		t.Fatal(err)
	}
	if restoredText(t, s, survivor.TraceID, token) != "response-only@example.com" {
		t.Fatal("deleting peer erased surviving response mapping")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if restoredText(t, s, survivor.TraceID, token) != "response-only@example.com" {
		t.Fatal("survivor mapping did not survive cleanup and restart")
	}
	if _, _, err = s.cleanup(func(log RequestLog) bool { return log.TraceID == survivor.TraceID }); err != nil {
		t.Fatal(err)
	}
	if _, exists := s.Privacy.Snapshot().Values[scope]; exists {
		t.Fatal("last reference cleanup retained its mapping dictionary")
	}
	if _, err := os.Stat(responsePath); !os.IsNotExist(err) {
		t.Fatal("last reference cleanup left response body file")
	}
	other, _ := s.Log(unrelated.TraceID)
	if restoredText(t, s, unrelated.TraceID, placeholderIn(t, other.RequestBody)) != "other@example.com" {
		t.Fatal("unrelated namespace was pruned")
	}
}

func TestSessionCleanupKeepsActivePrivacyNamespace(t *testing.T) {
	s := New()
	p := recordingPolicy(t, s)
	headers := http.Header{"X-Session-Id": {"active-session"}}
	done := privacyRequest(s, "done", `{"input":"hello"}`, headers, nil)
	active := privacyRequest(s, "active", `{"input":"hello"}`, headers, nil)
	finishPrivate(s, done, "")
	_, scope := s.PrivacyContext(active.TraceID)
	token := s.Privacy.Text(p, scope, "active-response@example.com")
	removed, skipped, err := s.cleanup(func(log RequestLog) bool { return log.SessionID == active.SessionID })
	if err != nil || removed != 1 || skipped != 1 || restoredText(t, s, active.TraceID, token) != "active-response@example.com" {
		t.Fatalf("session cleanup lost active mapping: removed=%d skipped=%d err=%v", removed, skipped, err)
	}
}

func TestCleanupRetainsConversationAliasForSurvivors(t *testing.T) {
	s := New()
	recordingPolicy(t, s)
	headers := http.Header{"X-Session-Id": {"continuing-session"}}
	const body = `{"input":"same@example.com"}`
	anchor := privacyRequest(s, "anchor", body, headers, nil)
	survivor := privacyRequest(s, "survivor", body, headers, nil)
	finishPrivate(s, anchor, "")
	finishPrivate(s, survivor, "")
	if _, _, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == anchor.TraceID }); err != nil {
		t.Fatal(err)
	}
	continuation := privacyRequest(s, "continuation", body, headers, nil)
	if continuation.SessionID != survivor.SessionID || placeholderIn(t, continuation.RequestBody) != placeholderIn(t, survivor.RequestBody) {
		t.Fatal("deleting the first trace split a surviving conversation namespace")
	}
}

func TestCleanupAliasDoesNotFollowAnEditedInferredSurvivor(t *testing.T) {
	s := New()
	recordingPolicy(t, s)
	const path = "/v1/chat/completions"
	const target = "http://privacy.test"
	headers := http.Header{"X-Session-Id": {"named-session"}}
	parentBody := []byte(`{"messages":[{"role":"user","content":"prompt"}]}`)
	parent := s.BeginWithBody(RequestLog{TraceID: "parent", Method: "POST", Path: path}, correlation.ExtractRequest(headers, path, parentBody, target), parentBody, 4096)
	response := correlation.NewResponseCapture("openai")
	response.JSON([]byte(`{"choices":[{"message":{"role":"assistant","content":"answer"}}]}`))
	parent.StatusCode = 200
	s.Complete(parent.TraceID, parent, response.Response())
	childBody := []byte(`{"messages":[{"role":"user","content":"prompt"},{"role":"assistant","content":"answer"},{"role":"user","content":"child"}]}`)
	child := s.BeginWithBody(RequestLog{TraceID: "child", Method: "POST", Path: path}, correlation.ExtractRequest(nil, path, childBody, target), childBody, 4096)
	if child.Correlation.ParentTraceID != parent.TraceID {
		t.Fatal("test child did not match the parent's completed history")
	}
	if _, _, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == parent.TraceID }); err != nil {
		t.Fatal(err)
	}
	edited := []byte(`{"messages":[{"role":"user","content":"unrelated edit"}]}`)
	detached := s.UpdateInput(child.TraceID, correlation.ExtractRequest(nil, path, edited, target))
	if detached.SessionID == parent.SessionID || s.records[child.TraceID].fixedSession {
		t.Fatal("cleanup pinned an inferred child to an explicit identity")
	}
	continuation := s.BeginWithBody(RequestLog{TraceID: "continuation", Method: "POST", Path: path}, correlation.ExtractRequest(headers, path, parentBody, target), parentBody, 4096)
	if continuation.SessionID == detached.SessionID {
		t.Fatal("deleted owner's identity alias followed an unrelated edited child")
	}
}

func TestOutboundPrivacyRetainsExplicitIdentityAndReferenceEvidence(t *testing.T) {
	for _, identity := range []struct{ field, kind string }{
		{"session_id", "session"}, {"thread_id", "thread"}, {"conversation", "conversation"},
	} {
		t.Run(identity.kind, func(t *testing.T) {
			s := New()
			recordingPolicy(t, s)
			body := fmt.Sprintf(`{%q:"identity@example.com","input":"message@example.com"}`, identity.field)
			var first, survivor RequestLog
			for _, trace := range []string{"first", "survivor"} {
				log := privacyRequest(s, trace, body, nil, nil)
				outgoing := s.Outbound(trace, []byte(body))
				input := correlation.ExtractRequest(nil, "/v1/responses", outgoing, "http://privacy.test")
				if input.Identities[0].Value == "identity@example.com" {
					t.Fatal("test did not substitute the outgoing body identity")
				}
				s.UpdateOutboundInput(trace, input)
				if retained := s.records[trace].input; len(retained.Identities) != 1 || retained.Identities[0].Kind != identity.kind || retained.Identities[0].Value != "identity@example.com" || retained.Messages[0] != input.Messages[0] {
					t.Fatal("outbound projection lost explicit identity or outgoing fingerprints")
				}
				finishPrivate(s, log, "")
				if trace == "first" {
					first = log
				} else {
					survivor = log
				}
			}
			if _, _, err := s.cleanup(func(log RequestLog) bool { return log.TraceID == first.TraceID }); err != nil {
				t.Fatal(err)
			}
			fresh := privacyRequest(s, "fresh", body, nil, nil)
			if fresh.SessionID != survivor.SessionID || placeholderIn(t, fresh.RequestBody) != placeholderIn(t, survivor.RequestBody) {
				t.Fatal("cleanup dropped substituted explicit identity evidence")
			}
		})
	}
	for _, body := range []string{
		`{"previous_response_id":"response@example.com","input":"hello"}`,
		`{"metadata":{"parent_trace_id":"parent@example.com"},"input":"hello"}`,
	} {
		s := New()
		recordingPolicy(t, s)
		log := privacyRequest(s, "reference", body, nil, nil)
		before := s.records[log.TraceID].input
		outgoing := s.Outbound(log.TraceID, []byte(body))
		s.UpdateOutboundInput(log.TraceID, correlation.ExtractRequest(nil, "/v1/responses", outgoing, "http://privacy.test"))
		after := s.records[log.TraceID].input
		if before.ParentTraceID != after.ParentTraceID || before.PreviousResponseID != after.PreviousResponseID {
			t.Fatal("outbound privacy replaced captured explicit parent evidence")
		}
	}
}

func TestLegacyPrivacyRestoresWithoutBroadeningNewCaptures(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p := recordingPolicy(t, s)
	const body = `{"input":"legacy@example.com"}`
	headers := http.Header{"X-Session-Id": {"legacy-session"}}
	input := correlation.ExtractRequest(headers, "/v1/responses", []byte(body), "http://privacy.test")
	legacyBody := string(s.Privacy.JSON(p, input.Scope, []byte(body)))
	token := placeholderIn(t, legacyBody)
	state := diskState{
		Version: 1, Privacy: s.Privacy.Snapshot(),
		Sessions: map[string]*Session{"legacy-session": {ID: "legacy-session", Label: "legacy"}},
		Aliases:  map[string]string{referenceKey(input.Scope, "session", "legacy-session"): "old-one"},
	}
	for i, trace := range []string{"old-one", "old-two"} {
		state.Records = append(state.Records, diskRecord{Log: RequestLog{TraceID: trace, SessionID: "legacy-session", Status: "done", StatusCode: 200, RequestBody: legacyBody}, Input: input, Privacy: p, Fixed: true, Preferred: "legacy-session", Sequence: uint64(i + 1)})
	}
	state.Sequence = 2
	payload, _ := json.Marshal(state)
	s.Lock()
	err = s.restore(payload)
	s.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	fresh := privacyRequest(s, "fresh", body, headers, nil)
	replay := privacyRequest(s, "replay", body, headers, &ReplayInfo{Of: "old-one"})
	for _, log := range []RequestLog{fresh, replay} {
		if log.SessionID != "legacy-session" || placeholderIn(t, log.RequestBody) == token || restoredText(t, s, log.TraceID, token) != token {
			t.Fatal("new capture inherited the legacy credential-wide namespace")
		}
	}
	if _, _, err = s.cleanup(func(log RequestLog) bool { return log.TraceID == "old-one" }); err != nil {
		t.Fatal(err)
	}
	if restoredText(t, s, "old-two", token) != "legacy@example.com" {
		t.Fatal("legacy survivor lost its mapping")
	}
	updated, err := s.snapshotJSON()
	if err != nil {
		t.Fatal(err)
	}
	var upgraded diskState
	if json.Unmarshal(updated, &upgraded) != nil || upgraded.Version != 2 {
		t.Fatal("new snapshots did not migrate to version 2")
	}
	reopened := New()
	if err = reopened.restore(updated); err != nil {
		t.Fatal(err)
	}
	if restoredText(t, reopened, "old-two", token) != "legacy@example.com" {
		t.Fatal("version 2 rewrite changed the legacy dictionary")
	}
	_, originalScope := s.PrivacyContext(fresh.TraceID)
	_, restoredScope := reopened.PrivacyContext(fresh.TraceID)
	if originalScope != restoredScope {
		t.Fatal("modern namespace was recomputed on restore")
	}
	upgraded.Records[0].PrivacyScope = ""
	invalid, _ := json.Marshal(upgraded)
	if New().restore(invalid) == nil {
		t.Fatal("version 2 accepted a missing namespace")
	}
}
