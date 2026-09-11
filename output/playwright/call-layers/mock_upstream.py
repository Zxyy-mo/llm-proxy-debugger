"""仅用于四层模型验收的本地受控上游；不接入任何真实供应商。"""

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

parser = argparse.ArgumentParser()
parser.add_argument('--port', type=int, required=True)
args = parser.parse_args()
lock = threading.Lock()
count = 0


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, status, body):
        raw = json.dumps(body, ensure_ascii=False).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        if self.path.endswith('/models'):
            self.reply(200, {'object': 'list', 'data': [{'id': 'actual-echo', 'object': 'model'}, {'id': 'actual-tools', 'object': 'model'}]})
        else:
            with lock:
                value = count
            self.reply(200, {'count': value, 'source': 'controlled_mock'})

    def do_POST(self):
        global count
        raw = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        body = json.loads(raw)
        with lock:
            count += 1
            request_number = count
        # 主上游固定拒绝，只有显式配置的备用地址才能成功，用于检验真实故障切换。
        if '/primary/' in self.path:
            self.reply(503, {'error': {'message': '受控主上游暂不可用'}})
            return
        if body.get('qa_failure'):
            self.reply(429, {'error': {'message': '受控限流错误'}})
            return
        if body.get('qa_hold'):
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            try:
                self.wfile.write(b'data: {"choices":[{"delta":{"content":"pending"}}]}\n\n')
                self.wfile.flush()
                # 仅验收客户端保持读取；网关被测试进程停止后套接字关闭，循环随即退出。
                while True:
                    time.sleep(0.05)
                    self.wfile.write(b': heartbeat\n\n')
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                return
        message = body.get('messages', [{}])[-1].get('content', 'ok')
        response = {'id': f'qa-response-{request_number}', 'model': body.get('model'), 'choices': [{'message': {'role': 'assistant', 'content': f'已处理：{message}'}, 'finish_reason': 'stop'}], 'usage': {'prompt_tokens': 12, 'completion_tokens': 8}}
        if body.get('stream'):
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            chunk = {'id': response['id'], 'choices': [{'delta': {'content': f'已处理：{message}'}, 'finish_reason': None}]}
            self.wfile.write(('data: ' + json.dumps(chunk, ensure_ascii=False) + '\n\n').encode())
            self.wfile.flush()
            time.sleep(0.05)
            terminal = {'id': response['id'], 'choices': [{'delta': {}, 'finish_reason': 'stop'}], 'usage': response['usage']}
            self.wfile.write(('data: ' + json.dumps(terminal) + '\n\ndata: [DONE]\n\n').encode())
        else:
            self.reply(200, response)


ThreadingHTTPServer(('127.0.0.1', args.port), Handler).serve_forever()
