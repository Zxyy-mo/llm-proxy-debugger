"""Local, synthetic upstream for request interception browser verification."""
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

records = []
lock = threading.Lock()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_GET(self):
        with lock:
            body = json.dumps(records, ensure_ascii=False).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get('Content-Length', '0')))
        with lock:
            records.append({
                'path': self.path, 'body': raw.decode(),
                'content_length': int(self.headers.get('Content-Length', '0')),
                'auth_preserved': self.headers.get('Authorization') == 'Bearer qa-auth-original',
                'accept': self.headers.get('Accept'),
            })
            response_id = f'qa-response-{len(records)}'
        response = {'id': response_id, 'object': 'response', 'model': 'qa-model',
                    'output': [{'type': 'message', 'role': 'assistant',
                                'content': [{'type': 'output_text', 'text': 'Local QA response'}]}],
                    'usage': {'input_tokens': 8, 'output_tokens': 4}}
        body = json.dumps(response).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


print('Synthetic upstream listening on 127.0.0.1:28001', flush=True)
ThreadingHTTPServer(('127.0.0.1', 28001), Handler).serve_forever()
