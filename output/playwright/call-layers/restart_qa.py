"""用自有子进程验收 Run/Attempt 恢复、文件保真和重放幂等；不接触现有服务。"""

import argparse
import hashlib
import http.client
import json
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--binary', required=True)
args = parser.parse_args()
binary = str(Path(args.binary).resolve(strict=True))
checks = []


def check(name, passed):
    if not passed:
        raise AssertionError(name)
    checks.append({'name': name, 'passed': True})


def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def wait(test, name):
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        try:
            value = test()
            if value:
                return value
        except (OSError, urllib.error.URLError, sqlite3.Error):
            pass
        time.sleep(0.05)
    raise TimeoutError(name)


gateway_port, upstream_port = free_port(), free_port()
origin, upstream = f'http://127.0.0.1:{gateway_port}', f'http://127.0.0.1:{upstream_port}'


def api(path, body=None, method=None, headers=None):
    data = None if body is None else json.dumps(body, ensure_ascii=False).encode()
    request = urllib.request.Request(origin + path, data=data, method=method, headers={'Content-Type': 'application/json', **(headers or {})})
    with urllib.request.urlopen(request, timeout=5) as response:
        raw = response.read()
        return response.status, response.headers, raw


def get(path):
    return json.loads(api(path)[2])


def upstream_count():
    with urllib.request.urlopen(upstream + '/state', timeout=3) as response:
        return json.load(response)['count']


def upstream_ready():
    with urllib.request.urlopen(upstream + '/state', timeout=1) as response:
        return response.status == 200


with tempfile.TemporaryDirectory(prefix='goproxy-restart-qa-') as temp:
    directory = Path(temp)
    mock = subprocess.Popen([sys.executable, str(Path(__file__).with_name('mock_upstream.py')), '--port', str(upstream_port)], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    process = None
    hold = None
    try:
        wait(upstream_ready, '受控上游启动')
        command = [binary, '-listen', f'127.0.0.1:{gateway_port}', '-target', upstream + '/backup/v1', '-logdir', str(directory / 'data'), '-web', '-']

        def start():
            child = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            try:
                wait(lambda: api('/api/history')[0] == 200, '验收网关启动')
            except BaseException:
                # 启动失败也回收本脚本刚创建的进程，不给后续验收留下监听端口。
                child.terminate()
                child.wait(timeout=5)
                raise
            return child

        process = start()
        providers = [{'id': name, 'base_url': upstream + '/' + name + '/v1', 'protocol': 'passthrough', 'history': False, 'websocket': False} for name in ['primary', 'backup']]
        api('/api/providers', {'providers': providers, 'routes': [{'id': 'r', 'model': 'm', 'provider_id': 'primary', 'failover': ['backup']}]}, 'PUT')
        body = {'model': 'm', 'messages': [{'role': 'user', 'content': 'restart completed capture'}], 'seed': 9007199254740993}
        _, headers, _ = api('/v1/chat/completions', body, headers={'X-Session-ID': 'restart-session', 'X-Run-ID': 'task-before'})
        source = headers['X-Gateway-Trace-ID']
        replay_input = {'trace_id': source, 'source': 'outgoing', 'idempotency_key': 'controlled-restart-key'}
        replay = json.loads(api('/api/replays', replay_input)[2])
        wait(lambda: get('/api/replays/' + replay['id'])['state'] == 'done', '重放完成')

        hold = http.client.HTTPConnection('127.0.0.1', gateway_port, timeout=5)
        hold_body = json.dumps({'model': 'm', 'messages': [{'role': 'user', 'content': 'restart active capture'}], 'stream': True, 'qa_hold': True})
        hold.request('POST', '/v1/chat/completions', hold_body, {'Content-Type': 'application/json', 'X-Session-ID': 'restart-session', 'X-Run-ID': 'task-active'})
        response = hold.getresponse()
        active = response.getheader('X-Gateway-Trace-ID')
        response.read(1)
        database = directory / 'data' / 'gateway.db'

        def saved_active():
            with sqlite3.connect(f'file:{database}?mode=ro', uri=True) as connection:
                row = connection.execute('SELECT payload FROM gateway_state WHERE id=1').fetchone()
            if row is None:
                return False
            state = json.loads(row[0])
            captured = next((item['log'] for item in state['records'] if item['log']['trace_id'] == active), None)
            return state if captured and captured['route']['attempts'][-1]['status'] == 'running' else False

        state = wait(saved_active, '活动尝试实际落盘')
        check('实际 SQLite 快照为 v3 且活动尝试已落盘', state['version'] == 3)
        before = get('/api/history?limit=200')['items']
        baseline = {log['trace_id']: {'run_id': log.get('run_id'), 'attempts': [a['id'] for a in log['route']['attempts']]} for log in before}
        hashes = {}
        for trace, entry in baseline.items():
            for attempt in entry['attempts']:
                hashes[attempt] = hashlib.sha256(api(f'/api/attempts/{trace}/{attempt}/body')[2]).hexdigest()
        count = upstream_count()
        # 只终止本脚本创建并持有句柄的网关；调用方没有自动重试机制。
        process.kill()
        process.wait(timeout=5)
        hold.close()
        process = start()
        after = {log['trace_id']: log for log in get('/api/history?limit=200')['items']}
        check('恢复没有新增或遗漏模型记录', set(after) == set(baseline))
        for trace, entry in baseline.items():
            check('请求和任务身份恢复 ' + trace[:8], after[trace].get('run_id') == entry['run_id'])
            check('逐次尝试身份恢复 ' + trace[:8], [a['id'] for a in after[trace]['route']['attempts']] == entry['attempts'])
            for attempt in entry['attempts']:
                check('独立出站文件哈希一致 ' + attempt[:8], hashlib.sha256(api(f'/api/attempts/{trace}/{attempt}/body')[2]).hexdigest() == hashes[attempt])
        interrupted = after[active]['route']['attempts'][-1]
        check('活动请求恢复为中断失败', after[active]['status'] == 'error' and after[active]['status_code'] == 503)
        check('活动尝试恢复为 interrupted，结束时刻与耗时保持未知', interrupted['status'] == 'interrupted' and not interrupted.get('ended_at') and 'total_duration_ms' not in interrupted)
        check('已完成尝试不被重启改写为中断', after[source]['route']['attempts'][-1]['status'] == 'done')
        code, _, raw = api('/api/replays', replay_input)
        recovered = json.loads(raw)
        check('重放幂等键跨进程恢复', code == 200 and recovered['id'] == replay['id'])
        check('重启、下载及同键重试不补发请求', upstream_count() == count)
        result = {'checks': checks, 'requests': len(after), 'attempts': len(hashes), 'upstream_requests': count, 'source': 'controlled_process_restart'}
        Path('output/playwright/call-layers/restart-result.json').write_text(json.dumps(result, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
        print(json.dumps({'checks': len(checks), 'requests': len(after), 'attempts': len(hashes), 'passed': True}, ensure_ascii=False))
    finally:
        if hold is not None:
            hold.close()
        for child in [process, mock]:
            if child is not None and child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait(timeout=5)
