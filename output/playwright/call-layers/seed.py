"""为独立验收网关注入两轮任务、同轮分支和未知/冲突归属。"""

import argparse
import json
import urllib.request
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--gateway', required=True)
parser.add_argument('--upstream', required=True)
args = parser.parse_args()


def request(path, body=None, headers=None, method=None):
    data = None if body is None else json.dumps(body, ensure_ascii=False).encode()
    req = urllib.request.Request(args.gateway + path, data=data, headers={'Content-Type': 'application/json', **(headers or {})}, method=method)
    with urllib.request.urlopen(req, timeout=15) as response:
        raw = response.read()
        return response.headers, raw


providers = [{'id': name, 'name': label, 'profile': 'custom', 'base_url': args.upstream + '/' + name + '/v1', 'protocol': 'passthrough', 'history': False, 'websocket': False, 'capabilities': {'chat_completions': 'supported', 'models': 'supported'}} for name, label in [('primary', '受控主上游'), ('backup', '受控备用上游')]]
request('/api/providers', {'providers': providers, 'routes': [{'id': 'qa-route', 'model': 'qa-*', 'target_model': 'actual-echo', 'provider_id': 'primary', 'priority': 0, 'disabled': False, 'failover': ['backup']}]}, method='PUT')
traces = {}


def capture(name, run=None, parent=None, body_run=None, stream=False):
    headers = {'X-Session-ID': 'qa-four-layers', 'Authorization': 'Bearer synthetic-layers-credential'}
    if run is not None:
        headers['X-Run-ID'] = run
    if parent:
        headers['X-Parent-Trace-ID'] = parent
    body = {'model': 'qa-echo', 'stream': stream, 'messages': [{'role': 'user', 'content': name}], 'seed': 9007199254740993}
    if body_run is not None:
        body['metadata'] = {'run_id': body_run}
    response, _ = request('/v1/chat/completions', body, headers)
    traces[name] = {'trace_id': response['X-Gateway-Trace-ID'], 'run_id': response.get('X-Gateway-Run-ID', '')}
    return traces[name]['trace_id']


root = capture('定位根因', 'task-diagnose', body_run='task-diagnose')
capture('分支：核对测试', 'task-diagnose', root)
capture('分支：核对配置', 'task-diagnose', root)
capture('完善体验', 'task-polish')
capture('流式检查', 'task-polish', stream=True)
capture('缺少任务标识')
capture('任务标识冲突', 'header-run', body_run='different-body-run')

evidence = {'gateway': args.gateway, 'upstream': args.upstream, 'session_id': 'qa-four-layers', 'traces': traces, 'source': 'controlled_mock'}
Path('output/playwright/call-layers/seed-result.json').write_text(json.dumps(evidence, ensure_ascii=False, indent=2) + '\n')
print(json.dumps({'requests': len(traces), 'runs': 2, 'unassociated': 2, 'gateway': args.gateway}, ensure_ascii=False))
