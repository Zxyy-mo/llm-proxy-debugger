package main

import (
	"bytes"
	"strconv"
)

// SSEEvent 表示一个已分发的 Server-Sent Events 消息。
// 它被设计为调试“够用”：我们保留了解析出来的字段以及合并后的 Data 数据。
type SSEEvent struct {
	Event   string            // event:
	Data    string            // concatenated data: lines (joined by '\n')
	ID      string            // id:
	RetryMS int               // retry: (milliseconds)
	Fields  map[string]string // other fields for debugging/forward-compat
}

// SSEParser 增量解析 SSE 字节流。
// 它是基于行的，并且能很好地针对分块、CRLF 换行、多行 data 以及注释进行鲁棒性处理。
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
		// 2MB 对于正常的 SSE 分块来说已经足够；如果超过此限制，我们将重置解析器状态。
		maxBufferBytes: 2 * 1024 * 1024,
		// 当前事件的数据体积上限（按行拼接的 data 数据）。超过此限制将丢弃当前事件。
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
	// 为了调试目的，每个字段仅保留一个值；重复声明的字段将被覆盖（除了 data: 外）。
	if p.curFields == nil {
		p.curFields = make(map[string]string)
	}
	p.curFields[field] = string(value)
}

// Feed 消费一个字节块，并返回所有已完整组装分发的 SSE 事件。
func (p *SSEParser) Feed(chunk []byte) []SSEEvent {
	if len(chunk) == 0 {
		return nil
	}

	p.buf = append(p.buf, chunk...)
	if p.maxBufferBytes > 0 && len(p.buf) > p.maxBufferBytes {
		// 数据流格式错误或发生上游故障：强制重置状态以避免 OOM。
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

		// 处理 CRLF 换行，截断末尾的 '\r'
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		// 读到空行，触发事件分发。
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

		// 注释以 ":" 开头，在这里予以忽略。
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
				// 数据过大；丢弃当前事件，并从下一个分发边界继续向后解析。
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
		// 根据协议规范：字段名存在，但值为空。
		return string(line), nil
	}

	field = string(line[:colon])
	value = line[colon+1:]
	// 剔除可选的单一前导空格。
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return field, value
}
