"""Local, synthetic upstream for request replay browser verification.

Records every POST without storing credential values: it only reports which
known QA credential arrived. Behaviour is keyed off the request body so that
faithful replays (which resend headers) can still select slow or streaming
responses through editable content.
"""
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

records = []
lock = threading.Lock()
inflight = {'count': 0}


def credential_label(value):
    if not value:
        return 'none'
    if value == 'Bearer qa-auth-original':
        return 'original'
    if value == 'Bearer qa-replay-secret':
        return 'replay'
    if value == 'Bearer qa-auth-from-env':
        return 'env'
    return 'other'


class Handler(BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def log_message(self, *_args):
        pass

    def do_GET(self):
        with lock:
            payload = {'records': records, 'inflight': inflight['count']}
        body = json.dumps(payload, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        text = raw.decode('utf-8', 'replace')
        query = self.path.split('?', 1)[1] if '?' in self.path else ''
        with lock:
            records.append({
                'path': self.path.split('?', 1)[0], 'query': query, 'body': text,
                'content_length': int(self.headers.get('Content-Length', '0')),
                'auth': credential_label(self.headers.get('Authorization')),
                'accept': self.headers.get('Accept'),
                'user_agent': self.headers.get('User-Agent'),
                'system_count': text.count('QA_INJECTED'),
            })
            index = len(records)
        if 'slow' in text:
            with lock:
                inflight['count'] += 1
            try:
                for _ in range(200):
                    time.sleep(0.05)
                    if self.connection_closed():
                        break
            finally:
                with lock:
                    inflight['count'] -= 1
            return
        if '"stream":true' in text.replace(' ', ''):
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Cache-Control', 'no-cache')
            self.send_header('Connection', 'close')
            self.end_headers()
            events = [
                'event: response.created\ndata: {"type":"response.created","response":{"id":"qa-stream-%d"}}\n\n' % index,
                'data: {"type":"response.output_text.delta","delta":"streamed "}\n\n',
                'data: {"type":"response.output_text.delta","delta":"answer"}\n\n',
                'data: {"type":"response.completed","response":{"id":"qa-stream-%d","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"streamed answer"}]}],"usage":{"input_tokens":6,"output_tokens":2}}}\n\n' % index,
            ]
            for event in events:
                self.wfile.write(event.encode())
                self.wfile.flush()
                time.sleep(0.05)
            self.close_connection = True
            return
        response = {'id': f'qa-response-{index}', 'object': 'response', 'model': 'qa-model',
                    'output': [{'type': 'message', 'role': 'assistant',
                                'content': [{'type': 'output_text', 'text': f'Local QA response {index}'}]}],
                    'usage': {'input_tokens': 8, 'output_tokens': 4}}
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def connection_closed(self):
        try:
            self.connection.settimeout(0.01)
            data = self.connection.recv(1, 0x2)  # MSG_PEEK
            return data == b''
        except (BlockingIOError, TimeoutError, OSError):
            return False


print('Synthetic upstream listening on 127.0.0.1:28001', flush=True)
ThreadingHTTPServer(('127.0.0.1', 28001), Handler).serve_forever()
