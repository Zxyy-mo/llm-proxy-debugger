"""Synthetic HTTP/SSE/Responses WebSocket upstream for foundation QA.

No real credentials or network services are required. Start with Python 3:
  python output/playwright/foundation/mock_upstream.py --port 28002
"""
import argparse
import base64
import hashlib
import json
import struct
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

lock = threading.Lock()
records = []
saved = {}


def encoded(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def make_response(index, model, text, tool=False):
    output = [{"type": "message", "id": f"msg_{index}", "role": "assistant", "content": [{"type": "output_text", "text": text}]}]
    if tool:
        output.append({"type": "function_call", "id": f"fc_{index}", "call_id": f"call_{index}", "name": "lookup", "arguments": '{"query":"synthetic"}', "status": "completed"})
    return {"id": f"resp_{index}", "object": "response", "status": "completed", "model": model, "output": output, "usage": {"input_tokens": 7, "output_tokens": 5, "output_tokens_details": {"reasoning_tokens": 0}}}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        pass

    def reply(self, value, status=200):
        raw = encoded(value)
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.headers.get("Upgrade", "").lower() == "websocket":
            self.websocket()
            return
        if "/responses/" in self.path:
            response_id = self.path.rsplit("/", 1)[1]
            self.reply(saved.get(response_id, {"id": response_id, "object": "response", "status": "completed", "output": [{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Provider 中保存的合成历史"}]}]}))
            return
        with lock:
            self.reply({"records": list(records), "count": len(records)})

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        try:
            body = json.loads(raw)
        except ValueError:
            self.reply({"error": {"message": "invalid JSON"}}, 400)
            return
        model = body.get("model", "mock")
        with lock:
            index = len(records) + 1
            records.append({"index": index, "path": self.path, "model": model, "body": raw.decode("utf-8"), "authenticated": bool(self.headers.get("Authorization") or self.headers.get("X-API-Key"))})
        if self.path.startswith("/unavailable/"):
            self.reply({"error": {"message": "synthetic temporary failure"}}, 503)
            return
        prompt = body.get("input", "")
        if body.get("messages"):
            prompt = body["messages"][-1].get("content", "")
        if not isinstance(prompt, str):
            prompt = json.dumps(prompt, ensure_ascii=False)
        if "slow" in prompt:
            time.sleep(2)
        if "fail" in prompt:
            self.reply({"error": {"message": "synthetic failure"}}, 429)
            return
        text = "本地验证：" + prompt[:120]
        if "large" in prompt:
            text += "完整响应保真。" * 12000
        tool = "tool" in prompt
        response = make_response(index, model, text, tool)
        saved[response["id"]] = response
        if self.path.rstrip("/").endswith("/chat/completions"):
            message = {"role": "assistant", "content": text}
            if tool:
                message["tool_calls"] = [{"id": f"call_{index}", "type": "function", "function": {"name": "lookup", "arguments": '{"query":"synthetic"}'}}]
            result = {"id": f"chat_{index}", "model": model, "choices": [{"index": 0, "message": message, "finish_reason": "tool_calls" if tool else "stop"}], "usage": {"prompt_tokens": 7, "completion_tokens": 5, "completion_tokens_details": {"reasoning_tokens": 0}}}
            events = [{"id": result["id"], "model": model, "choices": [{"index": 0, "delta": message, "finish_reason": "tool_calls" if tool else "stop"}]}, {"choices": [], "usage": result["usage"]}, "[DONE]"]
            if tool:
                for call in message["tool_calls"]:
                    call["index"] = 0
        elif self.path.rstrip("/").endswith("/messages"):
            result = {"id": f"msg_{index}", "type": "message", "model": model, "role": "assistant", "content": [{"type": "text", "text": text}], "stop_reason": "end_turn", "usage": {"input_tokens": 7, "output_tokens": 5}}
            events = [{"type": "message_start", "message": {"id": result["id"], "type": "message", "role": "assistant", "model": model, "content": [], "usage": {"input_tokens": 7}}}, {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": text}}, {"type": "message_delta", "usage": {"output_tokens": 5}, "delta": {"stop_reason": "end_turn"}}, {"type": "message_stop"}]
        else:
            result = response
            events = [{"type": "response.created", "response": {"id": response["id"], "object": "response", "model": model}}, {"type": "response.output_text.delta", "delta": text}, {"type": "response.completed", "response": response}]
        if not body.get("stream"):
            self.reply(result)
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            for event in events:
                data = event.encode() if isinstance(event, str) else encoded(event)
                self.wfile.write(b"data: " + data + b"\n\n")
                self.wfile.flush()
                time.sleep(0.03)
        except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
            pass
        self.close_connection = True

    def ws_read(self):
        header = self.rfile.read(2)
        if len(header) != 2:
            return None
        opcode, length = header[0] & 15, header[1] & 127
        if length == 126:
            length = struct.unpack("!H", self.rfile.read(2))[0]
        elif length == 127:
            length = struct.unpack("!Q", self.rfile.read(8))[0]
        mask = self.rfile.read(4) if header[1] & 128 else b""
        data = self.rfile.read(length)
        if mask:
            data = bytes(value ^ mask[i % 4] for i, value in enumerate(data))
        if opcode == 8:
            return None
        return data

    def ws_send(self, value):
        data = encoded(value)
        header = b"\x81" + (bytes([len(data)]) if len(data) < 126 else b"\x7e" + struct.pack("!H", len(data)))
        self.wfile.write(header + data)
        self.wfile.flush()

    def websocket(self):
        accept = base64.b64encode(hashlib.sha1((self.headers["Sec-WebSocket-Key"] + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        self.send_response(101)
        self.send_header("Upgrade", "websocket")
        self.send_header("Connection", "Upgrade")
        self.send_header("Sec-WebSocket-Accept", accept)
        self.end_headers()
        try:
            while True:
                data = self.ws_read()
                if data is None:
                    break
                body = json.loads(data)
                if body.get("type") != "response.create":
                    continue
                with lock:
                    index = len(records) + 1
                    records.append({"index": index, "path": self.path, "model": body.get("model"), "body": data.decode(), "transport": "websocket"})
                response = make_response(index, body.get("model", "mock"), "WebSocket 本地验证")
                lane = {"stream_id": body["stream_id"]} if "stream_id" in body else {}
                self.ws_send({"type": "response.created", "response": {"id": response["id"]}, **lane})
                self.ws_send({"type": "response.output_text.delta", "delta": "WebSocket 本地验证", **lane})
                self.ws_send({"type": "response.completed", "response": response, **lane})
        except (OSError, ValueError):
            pass
        self.close_connection = True


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=28002)
    args = parser.parse_args()
    print(f"Synthetic foundation upstream on 127.0.0.1:{args.port}", flush=True)
    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()
