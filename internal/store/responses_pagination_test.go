package store_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Zxyy-mo/llm-proxy-debugger/internal/correlation"
	"github.com/Zxyy-mo/llm-proxy-debugger/internal/store"
)

func responsePagingFixture(t *testing.T, body []byte, snapshot store.ResponseSnapshot) (*store.Store, string) {
	t.Helper()
	s := store.New()
	root := t.TempDir()
	if err := s.SetCaptureRoot(root); err != nil {
		t.Fatal(err)
	}
	s.Begin(store.RequestLog{TraceID: "paged"}, correlation.Request{})
	path := filepath.Join(root, "response.body")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot.Path, snapshot.Stored, snapshot.Bytes = path, "file", int64(len(body))
	s.CaptureResponse("paged", snapshot)
	return s, path
}

func responsePage(t *testing.T, s *store.Store, query string) store.ResponseSnapshot {
	t.Helper()
	w := httptest.NewRecorder()
	s.ResponsesHandler(w, httptest.NewRequest(http.MethodGet, "/api/responses/paged"+query, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("detail %s: %d %s", query, w.Code, w.Body)
	}
	var snapshot store.ResponseSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"offset", "end", "next_offset", "last_offset"} {
		if _, ok := fields[field]; !ok {
			t.Fatalf("missing range field %q", field)
		}
	}
	return snapshot
}

func responsePageBytes(t *testing.T, snapshot store.ResponseSnapshot) []byte {
	t.Helper()
	if snapshot.BodyEncoding == "" {
		return []byte(snapshot.Body)
	}
	if snapshot.BodyEncoding != "base64" {
		t.Fatalf("unknown body encoding %q", snapshot.BodyEncoding)
	}
	body, err := base64.StdEncoding.DecodeString(snapshot.Body)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func verifyResponsePages(t *testing.T, s *store.Store, body []byte, limit int, text bool) {
	t.Helper()
	var reconstructed []byte
	var offset int64
	for {
		part := responsePage(t, s, fmt.Sprintf("?offset=%d&limit=%d", offset, limit))
		decoded := responsePageBytes(t, part)
		if part.Stored != "file" || part.Offset != offset || part.End != offset+int64(len(decoded)) || part.Bytes != int64(len(body)) {
			t.Fatalf("incorrect byte range: %+v", part)
		}
		if len(decoded) == 0 && len(body) != 0 {
			t.Fatal("pagination did not advance")
		}
		if len(decoded) > limit+utf8.UTFMax-1 {
			t.Fatalf("unbounded part: %d bytes for limit %d", len(decoded), limit)
		}
		if text && (part.BodyEncoding != "" || !utf8.Valid(decoded)) {
			t.Fatalf("UTF-8 boundary damaged: offset=%d limit=%d encoding=%s", offset, limit, part.BodyEncoding)
		}
		if !bytes.Equal(decoded, body[part.Offset:part.End]) {
			t.Fatalf("part changed bytes at offset %d", offset)
		}
		reconstructed = append(reconstructed, decoded...)
		if part.NextOffset == nil {
			if part.Truncated || part.End != int64(len(body)) {
				t.Fatal("final part does not end at EOF")
			}
			break
		}
		if !part.Truncated || *part.NextOffset != part.End || *part.NextOffset <= offset {
			t.Fatal("invalid continuation offset")
		}
		offset = *part.NextOffset
	}
	if !bytes.Equal(reconstructed, body) {
		t.Fatal("pages did not reconstruct exact saved bytes")
	}
	first := responsePage(t, s, fmt.Sprintf("?offset=0&limit=%d", limit))
	last := responsePage(t, s, fmt.Sprintf("?offset=%d&limit=%d", first.LastOffset, limit))
	if last.End != int64(len(body)) || last.NextOffset != nil || last.Truncated {
		t.Fatalf("last_offset does not reach the end: offset=%d end=%d total=%d", last.Offset, last.End, last.Bytes)
	}
	if text && (last.BodyEncoding != "" || !utf8.ValidString(last.Body)) {
		t.Fatal("final-part shortcut split UTF-8")
	}
}

func TestResponsePaginationReconstructsUTF8(t *testing.T) {
	for _, tc := range []struct {
		name, typ, contentType, body string
	}{
		{"json", "json", "application/json", `{"text":"A¢中🌍文𐐷Z","integer":9007199254740993,"escaped":"\u4f60\\n"}`},
		{"sse", "sse", "text/event-stream", "data: {\"text\":\"🌍你好\"}\r\n\r\ndata: [DONE]\r\n\r\n"},
		{"websocket", "websocket", "application/x-ndjson", "{\"data\":\"你好🌍\"}\n{\"data\":\"end\"}\n"},
		{"text", "binary", "text/plain; charset=utf-8", "¢你好🌍A\nreal replacement character: �; end🌍"},
		{"tiny", "json", "application/json", "🌍"},
		{"empty", "json", "application/json", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := responsePagingFixture(t, []byte(tc.body), store.ResponseSnapshot{Type: tc.typ, ContentType: tc.contentType, Complete: true})
			for _, limit := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 137} {
				t.Run(fmt.Sprint(limit), func(t *testing.T) { verifyResponsePages(t, s, []byte(tc.body), limit, true) })
			}
		})
	}
}

func TestResponsePaginationPreservesBinaryAndInvalidUTF8(t *testing.T) {
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	for _, tc := range []struct {
		name, typ, encoding string
		body                []byte
	}{
		{"binary", "binary", "", allBytes},
		{"binary-valid-utf8", "binary", "", []byte("A¢中🌍Z")},
		{"encoded-valid-utf8", "json", "gzip", []byte("hello world")},
		{"invalid-interior", "json", "", []byte{'a', 0xff, 'b', 0xe4, 0xb8, 0xad, 0xff, 'z'}},
		{"incomplete-final-codepoint", "json", "", []byte{'a', 0xe4, 0xb8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := responsePagingFixture(t, tc.body, store.ResponseSnapshot{Type: tc.typ, ContentEncoding: tc.encoding, Complete: true})
			for _, limit := range []int{1, 2, 3, 4, 5, 8, 137} {
				verifyResponsePages(t, s, tc.body, limit, false)
			}
			if part := responsePage(t, s, "?limit=3"); (tc.typ == "binary" || tc.encoding != "") && part.BodyEncoding != "base64" {
				t.Fatal("binary/opaque encoding was presented as text")
			}
		})
	}
}

func TestResponsePaginationHonorsEveryRequestedByteOffset(t *testing.T) {
	body := []byte("A¢中🌍Z")
	s, _ := responsePagingFixture(t, body, store.ResponseSnapshot{Type: "json", Complete: true})
	for offset := range len(body) + 1 {
		for limit := 1; limit <= 5; limit++ {
			part := responsePage(t, s, fmt.Sprintf("?offset=%d&limit=%d", offset, limit))
			decoded := responsePageBytes(t, part)
			if part.Offset != int64(offset) || !bytes.Equal(decoded, body[part.Offset:part.End]) {
				t.Fatalf("arbitrary offset was changed: offset=%d limit=%d", offset, limit)
			}
			if !utf8.Valid(decoded) && part.BodyEncoding != "base64" {
				t.Fatal("mid-codepoint start was not encoded losslessly")
			}
		}
	}
}

func TestResponsePaginationLargeBoundedAndUnlimitedCompatibility(t *testing.T) {
	body := []byte(`{"text":"` + strings.Repeat("a界🌍", 400000) + `TAIL-END","integer":9007199254740993}`)
	s, _ := responsePagingFixture(t, body, store.ResponseSnapshot{Type: "json", Complete: true})
	full := responsePage(t, s, "")
	if !bytes.Equal(responsePageBytes(t, full), body) || full.Truncated || full.Offset != 0 || full.End != int64(len(body)) {
		t.Fatal("unlimited detail compatibility changed")
	}
	first := responsePage(t, s, "?offset=0")
	if first.End > (256<<10)+3 || first.NextOffset == nil || first.LastOffset <= 2<<20 || strings.Contains(first.Body, "TAIL-END") {
		t.Fatal("offset browsing did not default to a bounded part")
	}
	last := responsePage(t, s, fmt.Sprintf("?offset=%d", first.LastOffset))
	if last.BodyEncoding != "" || !strings.Contains(last.Body, "TAIL-END") || last.End != int64(len(body)) || len(last.Body) > (256<<10)+3 {
		t.Fatal("bounded final part did not expose the end marker")
	}
	preview := responsePage(t, s, "?limit=137")
	if preview.Offset != 0 || !preview.Truncated || preview.BodyEncoding != "" || !bytes.Equal([]byte(preview.Body), body[:preview.End]) {
		t.Fatal("limit-only preview compatibility changed")
	}
	w := httptest.NewRecorder()
	s.ResponsesHandler(w, httptest.NewRequest(http.MethodGet, "/api/responses/paged/download?offset=-1&limit=bad", nil))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), body) {
		t.Fatal("download was affected by paging parameters")
	}
	metadata, _ := s.ResponseSnapshot("paged")
	if metadata.Body != "" || metadata.End != 0 || metadata.NextOffset != nil {
		t.Fatal("detail read mutated stored capture metadata")
	}
}

func TestResponsePaginationValidation(t *testing.T) {
	s, _ := responsePagingFixture(t, []byte("abc"), store.ResponseSnapshot{Type: "json", Complete: true})
	for _, tc := range []struct {
		query string
		code  int
	}{
		{"?offset=", 400}, {"?offset=-1", 400}, {"?offset=-0", 400}, {"?offset=1.5", 400}, {"?offset=%2B1", 400},
		{"?offset=%", 400}, {"?offset=%zz", 400}, {"?limit=%zz", 400}, {"?offset=1;limit=2", 400},
		{"?offset=9223372036854775808", 400}, {"?offset=1&offset=2", 400}, {"?offset=1e2", 400},
		{"?limit=", 400}, {"?limit=0", 400}, {"?limit=-1", 400}, {"?limit=16777217", 400},
		{"?limit=9223372036854775808", 400}, {"?limit=1&limit=2", 400}, {"?limit=0x10", 400},
		{"?offset=4", 416}, {"?offset=9223372036854775807", 416},
		{"?offset=3&limit=1", 200}, {"?limit=16777216", 200}, {"?offset=0&limit=1", 200},
		{"?variant=unknown", 400}, {"?variant=upstream", 404},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.ResponsesHandler(w, httptest.NewRequest(http.MethodGet, "/api/responses/paged"+tc.query, nil))
			if w.Code != tc.code || !json.Valid(w.Body.Bytes()) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
		})
	}
	eof := responsePage(t, s, "?offset=3&limit=1")
	if eof.Offset != 3 || eof.End != 3 || eof.Body != "" || eof.NextOffset != nil || eof.Truncated {
		t.Fatal("EOF offset was not an empty terminal part")
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/responses/paged"},
		{http.MethodGet, "/api/responses/unknown"},
	} {
		w := httptest.NewRecorder()
		s.ResponsesHandler(w, httptest.NewRequest(tc.method, tc.path, nil))
		if tc.method == http.MethodPost && (w.Code != 405 || w.Header().Get("Allow") != "GET") {
			t.Fatal("method validation changed")
		}
		if tc.method == http.MethodGet && w.Code != 404 {
			t.Fatal("unknown trace was accepted")
		}
	}
}

func TestResponsePaginationReceivingMissingAndVariants(t *testing.T) {
	s, path := responsePagingFixture(t, []byte("a🌍"), store.ResponseSnapshot{Type: "json", Receiving: true})
	first := responsePage(t, s, "?limit=4")
	if !first.Receiving || first.Complete || first.End != 1 || first.NextOffset == nil || *first.NextOffset != 1 {
		t.Fatal("receiving capture lost its lifecycle/range")
	}
	w := httptest.NewRecorder()
	s.ResponsesHandler(w, httptest.NewRequest(http.MethodGet, "/api/responses/paged/download", nil))
	if w.Code != http.StatusConflict {
		t.Fatal("receiving capture was downloadable")
	}
	if err := os.WriteFile(path, []byte("a🌍tail"), 0600); err != nil {
		t.Fatal(err)
	}
	refreshed := responsePage(t, s, "?offset=1&limit=4")
	if refreshed.Offset != 1 || refreshed.Body != "🌍" || refreshed.Bytes != 9 || refreshed.NextOffset == nil || *refreshed.NextOffset != 5 {
		t.Fatal("refresh did not retain the selected range and see newly available bytes")
	}
	upstreamPath := filepath.Join(filepath.Dir(path), "upstream.body")
	if err := os.WriteFile(upstreamPath, []byte("upstream-end"), 0600); err != nil {
		t.Fatal(err)
	}
	s.CaptureUpstreamResponse("paged", store.ResponseSnapshot{Path: upstreamPath, Stored: "file", Type: "json", Complete: true})
	upstream := responsePage(t, s, "?variant=upstream&offset=9&limit=3")
	client := responsePage(t, s, "?variant=client&offset=5&limit=4")
	if upstream.Body != "end" || upstream.Bytes != 12 || upstream.Receiving || upstream.Variant != "upstream" || client.Body != "tail" || !client.Receiving {
		t.Fatal("variant paging mixed response representations")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	missing := responsePage(t, s, "?offset=5&limit=4")
	if missing.Stored != "missing" || missing.Reason == "" || missing.Body != "" || missing.NextOffset != nil {
		t.Fatal("deleted capture was not explicit")
	}
	if available := responsePage(t, s, "?variant=upstream&limit=4"); available.Stored != "file" {
		t.Fatal("deleting client file affected upstream variant")
	}
	// Validation still precedes file access when a capture no longer exists.
	w = httptest.NewRecorder()
	s.ResponsesHandler(w, httptest.NewRequest(http.MethodGet, "/api/responses/paged?offset=overflow", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatal("invalid offset was hidden by missing-file state")
	}
}
