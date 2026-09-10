package sse

import (
	"bytes"
	"fmt"
	"strconv"
)

// SSEEvent 表示一个已分发的 Server-Sent Events 消息。
type SSEEvent struct {
	Event   string            // event:
	Data    string            // concatenated data: lines (joined by '\n')
	ID      string            // id:
	RetryMS int               // retry: (milliseconds)
	Fields  map[string]string // other fields for debugging/forward-compat
}

// SSEParser 增量解析 SSE 字节流。
type SSEParser struct {
	buf []byte

	curEvent   string
	curID      string
	curRetryMS int
	curFields  map[string]string
	dataBuf    bytes.Buffer

	maxBufferBytes int
	maxDataBytes   int
	err            error
}

func (p *SSEParser) Err() error { return p.err }

func NewSSEParser() *SSEParser {
	return &SSEParser{
		maxBufferBytes: 2 * 1024 * 1024,
		maxDataBytes:   2 * 1024 * 1024,
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
	if p.curFields == nil {
		p.curFields = make(map[string]string)
	}
	p.curFields[field] = string(value)
}

// Feed 消费一个字节块，并返回所有已完整组装分发的 SSE 事件。
func (p *SSEParser) Feed(chunk []byte) []SSEEvent {
	if len(chunk) == 0 || p.err != nil {
		return nil
	}

	p.buf = append(p.buf, chunk...)
	if p.maxBufferBytes > 0 && len(p.buf) > p.maxBufferBytes {
		p.err = fmt.Errorf("SSE frame exceeds the observation limit")
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

		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

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
				p.err = fmt.Errorf("SSE event exceeds the observation limit")
				p.buf = nil
				p.resetEvent()
				return out
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
		return string(line), nil
	}

	field = string(line[:colon])
	value = line[colon+1:]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}
