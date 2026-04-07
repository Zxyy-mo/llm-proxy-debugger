package main

import (
	"bytes"
	"strconv"
)

// SSEEvent represents one dispatched Server-Sent Events message.
// It is intentionally "够用" for debugging: we preserve parsed fields and the concatenated Data.
type SSEEvent struct {
	Event   string            // event:
	Data    string            // concatenated data: lines (joined by '\n')
	ID      string            // id:
	RetryMS int               // retry: (milliseconds)
	Fields  map[string]string // other fields for debugging/forward-compat
}

// SSEParser incrementally parses an SSE byte stream.
// It is line-based and robust to chunking, CRLF, multi-line data and comments.
type SSEParser struct {
	buf []byte

	curEvent   string
	curID      string
	curRetryMS int
	curFields  map[string]string
	dataBuf    bytes.Buffer

	// Hard guardrails to avoid unbounded memory growth on malformed streams.
	maxBufferBytes int
	maxDataBytes   int
}

func NewSSEParser() *SSEParser {
	return &SSEParser{
		// 2MB is plenty for normal SSE chunks; if exceeded we reset the parser state.
		maxBufferBytes: 2 * 1024 * 1024,
		// Current-event data (concatenated data: lines). If exceeded we drop the current event.
		maxDataBytes: 2 * 1024 * 1024,
	}
}

func (p *SSEParser) resetEvent() {
	p.curEvent = ""
	p.curID = ""
	p.curRetryMS = 0
	p.curFields = nil
	p.dataBuf.Reset()
}

func (p *SSEParser) hasEventData() bool {
	return p.curEvent != "" || p.curID != "" || p.curRetryMS != 0 || p.dataBuf.Len() > 0 || len(p.curFields) > 0
}

func (p *SSEParser) appendDataLine(value []byte) {
	if p.dataBuf.Len() > 0 {
		_ = p.dataBuf.WriteByte('\n')
	}
	_, _ = p.dataBuf.Write(value)
}

func (p *SSEParser) rememberField(field string, value []byte) {
	// Keep only one value per field for debugging; repeated fields overwrite (except data:).
	if p.curFields == nil {
		p.curFields = make(map[string]string)
	}
	p.curFields[field] = string(value)
}

// Feed consumes a byte chunk and returns all fully-dispatched SSE events.
func (p *SSEParser) Feed(chunk []byte) []SSEEvent {
	if len(chunk) == 0 {
		return nil
	}

	p.buf = append(p.buf, chunk...)
	if p.maxBufferBytes > 0 && len(p.buf) > p.maxBufferBytes {
		// Malformed stream or upstream bug: reset to avoid OOM.
		p.buf = nil
		p.resetEvent()
		return nil
	}

	var out []SSEEvent
	for {
		nl := bytes.IndexByte(p.buf, '\n')
		if nl == -1 {
			break
		}

		line := p.buf[:nl]
		p.buf = p.buf[nl+1:]

		// Handle CRLF by trimming trailing '\r'
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		// Blank line dispatches the event.
		if len(line) == 0 {
			if p.hasEventData() {
				out = append(out, SSEEvent{
					Event:   p.curEvent,
					Data:    p.dataBuf.String(),
					ID:      p.curID,
					RetryMS: p.curRetryMS,
					Fields:  p.curFields,
				})
			}
			p.resetEvent()
			continue
		}

		// Comments start with ":" and are ignored.
		if line[0] == ':' {
			continue
		}

		field, value := splitSSEField(line)
		if field == "" {
			continue
		}

		switch field {
		case "event":
			p.curEvent = string(value)
		case "data":
			if p.maxDataBytes > 0 && p.dataBuf.Len()+len(value)+1 > p.maxDataBytes {
				// Too large; drop current event and continue parsing from next dispatch boundary.
				p.resetEvent()
				continue
			}
			p.appendDataLine(value)
		case "id":
			p.curID = string(value)
		case "retry":
			if n, err := strconv.Atoi(string(value)); err == nil && n >= 0 {
				p.curRetryMS = n
			}
		default:
			p.rememberField(field, value)
		}
	}

	return out
}

func splitSSEField(line []byte) (field string, value []byte) {
	colon := bytes.IndexByte(line, ':')
	if colon == -1 {
		// Per spec: field name, empty value.
		return string(line), nil
	}

	field = string(line[:colon])
	value = line[colon+1:]
	// Optional single leading space.
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}
