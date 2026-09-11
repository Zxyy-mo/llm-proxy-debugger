"""Local synthetic traffic for response browsing and debugger workflow checks."""
import argparse
import json
import select
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


LOCK = threading.Lock()
REQUESTS = []
RUN_ID = format(time.time_ns(), "x")
LARGE_TEXT = (
    "QA_BEGIN_完整响应\n"
    + "分段验证🙂 保留原始中文和数字 9007199254740993。\n" * 50000
    + "QA_END_完整响应"
)
BINARY_BODY = bytes(range(256)) * 1200 + b"QA_BINARY_END"


def encode(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args):
        pass

    def reply(self, status, body, content_type="application/json"):
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        with LOCK:
            state = {"count": len(REQUESTS), "inflight": sum(item["state"] == "running" for item in REQUESTS), "requests": [dict(item) for item in REQUESTS]}
        self.reply(200, encode(state))

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        try:
            request = json.loads(body)
        except ValueError:
            self.reply(400, encode({"error": {"message": "Synthetic invalid JSON"}}))
            return
        prompt = request.get("input", "")
        if request.get("messages"):
            prompt = request["messages"][-1].get("content", "")
        if not isinstance(prompt, str):
            prompt = json.dumps(prompt, ensure_ascii=False)
        model = request.get("model", "qa-debugger")
        with LOCK:
            number = len(REQUESTS) + 1
            record = {
                "number": number,
                "case": prompt[:120],
                "model": model,
                "session": self.headers.get("X-Session-ID", ""),
                "tag": request.get("metadata", {}).get("qa_case", ""),
                "state": "running",
            }
            REQUESTS.append(record)

        def finish(state):
            with LOCK:
                record["state"] = state

        if "op-delay" in prompt.split():
            deadline = time.monotonic() + 12
            while time.monotonic() < deadline:
                readable, _, _ = select.select([self.connection], [], [], 0.05)
                if readable and self.connection.recv(1, socket.MSG_PEEK) == b"":
                    finish("canceled")
                    self.close_connection = True
                    return
        if "qa-binary" in prompt:
            self.reply(200, BINARY_BODY, "application/octet-stream")
            finish("done")
            return
        if "qa-failure" in prompt:
            self.reply(429, encode({"error": {
                "type": "rate_limit_error",
                "message": "Synthetic provider rate limit; retry after checking the request rate.",
            }}))
            finish("error")
            return
        if "qa-slow" in prompt:
            time.sleep(0.25)
        if "qa-large" in prompt:
            text = LARGE_TEXT
        elif "qa-response-only" in prompt:
            text = "仅存在于响应中的邮箱：response-only@example.test"
        elif "qa-empty" in prompt or "qa-zero" in prompt:
            text = ""
        else:
            text = "合成上游响应：" + prompt
        usage = {
            "prompt_tokens": 0 if "qa-zero" in prompt else 18,
            "completion_tokens": 0 if not text else 42,
            "completion_tokens_details": {"reasoning_tokens": 0},
        }
        response_id = "qa-response-" + RUN_ID + "-" + str(number)
        message = {"role": "assistant", "content": text}
        if "qa-tool" in prompt:
            message["tool_calls"] = [{
                "id": "qa-call-" + RUN_ID + "-" + str(number),
                "type": "function",
                "function": {"name": "lookup_document", "arguments": '{"query":"synthetic"}'},
            }]
        response = {
            "id": response_id,
            "object": "chat.completion",
            "model": model,
            "choices": [{
                "index": 0, "message": message,
                "finish_reason": "tool_calls" if "tool_calls" in message else "stop",
            }],
            "usage": usage,
            "large_integer": 9007199254740993,
        }
        if "qa-estimated" in prompt:
            response.pop("usage")
        if not request.get("stream"):
            self.reply(200, encode(response))
            finish("done")
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()
        try:
            # Individual events stay well below the observation limit; only the
            # complete response is large. This exercises real incremental data.
            for offset in range(0, len(text), 8192):
                event = {
                    "id": response_id, "model": model,
                    "choices": [{"index": 0, "delta": {"content": text[offset:offset + 8192]}}],
                }
                self.wfile.write(b"data: " + encode(event) + b"\n\n")
                self.wfile.flush()
            if "qa-estimated" not in prompt:
                self.wfile.write(b"data: " + encode({"choices": [], "usage": usage}) + b"\n\n")
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            finish("done")
        except (BrokenPipeError, ConnectionResetError):
            finish("canceled")
        self.close_connection = True


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=28003)
    args = parser.parse_args()
    print("Synthetic debugger upstream on 127.0.0.1:" + str(args.port), flush=True)
    ThreadingHTTPServer(("127.0.0.1", args.port), Handler).serve_forever()
