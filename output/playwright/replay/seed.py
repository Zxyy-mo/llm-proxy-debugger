"""Seed the QA gateway with a rule and captured requests. Prints trace IDs."""
import json
import sys
import urllib.request

API = 'http://127.0.0.1:12338'


def call(method, path, payload=None, headers=None):
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(API + path, data=data, method=method, headers={'Content-Type': 'application/json', **(headers or {})})
    with urllib.request.urlopen(request, timeout=30) as response:
        return response.status, response.headers, response.read().decode()


def main():
    status, _, body = call('POST', '/api/rules', {'path_match': '/responses', 'body_match': '', 'inject_system': 'QA_INJECTED', 'intercept': False, 'disabled': False, 'wait_seconds': 30, 'timeout_action': 'forward'})
    assert status == 201, body
    payload = {'model': 'qa-model', 'input': "it's $HOME `id` \\ \"quoted\" 你好 ! tail", 'metadata': {'qa_case': 'replay-source', 'big': 9007199254740993}}
    status, headers, body = call('POST', '/v1/responses?trace=qa&key=qa-query-secret', payload, {'Authorization': 'Bearer qa-auth-original', 'Accept': 'application/json'})
    assert status == 200, body
    source = headers['X-Gateway-Trace-ID']
    status, headers, body = call('POST', '/v1/responses', {'model': 'qa-model', 'input': 'stream me', 'stream': True}, {'Authorization': 'Bearer qa-auth-original', 'Accept': 'text/event-stream'})
    assert status == 200 and 'streamed answer' in body, body
    streamed = headers['X-Gateway-Trace-ID']
    print(json.dumps({'source': source, 'streamed': streamed}))


if __name__ == '__main__':
    sys.exit(main())
