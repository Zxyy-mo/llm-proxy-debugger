package main

import "testing"

func TestSSEParser_LF(t *testing.T) {
	p := NewSSEParser()
	evts := p.Feed([]byte("data: {\"type\":\"message_start\"}\n\n"))
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].Data != "{\"type\":\"message_start\"}" {
		t.Fatalf("unexpected data: %q", evts[0].Data)
	}
}

func TestSSEParser_CRLF_AndFields(t *testing.T) {
	p := NewSSEParser()
	evts := p.Feed([]byte("event: ping\r\ndata: 123\r\nfoo: bar\r\n\r\n"))
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].Event != "ping" {
		t.Fatalf("unexpected event: %q", evts[0].Event)
	}
	if evts[0].Data != "123" {
		t.Fatalf("unexpected data: %q", evts[0].Data)
	}
	if evts[0].Fields["foo"] != "bar" {
		t.Fatalf("unexpected foo field: %q", evts[0].Fields["foo"])
	}
}

func TestSSEParser_MultilineData_Chunked(t *testing.T) {
	p := NewSSEParser()
	if got := p.Feed([]byte("data: line1\n")); len(got) != 0 {
		t.Fatalf("expected 0 events, got %d", len(got))
	}

	evts := p.Feed([]byte("data: line2\n\n"))
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].Data != "line1\nline2" {
		t.Fatalf("unexpected data: %q", evts[0].Data)
	}
}

func TestSSEParser_ChunkBoundaryInsideLine(t *testing.T) {
	p := NewSSEParser()
	if got := p.Feed([]byte("data: {\"a\":1")); len(got) != 0 {
		t.Fatalf("expected 0 events, got %d", len(got))
	}

	evts := p.Feed([]byte("}\n\n"))
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].Data != "{\"a\":1}" {
		t.Fatalf("unexpected data: %q", evts[0].Data)
	}
}

func TestSSEParser_CommentsIgnored(t *testing.T) {
	p := NewSSEParser()
	evts := p.Feed([]byte(": ping\n\n"))
	if len(evts) != 0 {
		t.Fatalf("expected 0 events, got %d", len(evts))
	}
}
