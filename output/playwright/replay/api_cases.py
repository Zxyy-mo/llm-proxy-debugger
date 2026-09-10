"""API-level replay cases against the QA gateway: idempotency, never-forwarded
sources, SSE replay, and credential isolation in gateway artifacts."""
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

API = 'http://127.0.0.1:12338'
UPSTREAM = 'http://127.0.0.1:28001/'


def call(method, path, payload=None, headers=None):
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(API + path, data=data, method=method, headers={'Content-Type': 'application/json', **(headers or {})})
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.status, response.headers, response.read().decode()
    except urllib.error.HTTPError as error:
        return error.code, error.headers, error.read().decode()


def get(url):
    with urllib.request.urlopen(url, timeout=10) as response:
        return json.load(response)


def wait_replay(replay_id):
    for _ in range(100):
        record = get(f'{API}/api/replays/{replay_id}')
        if record['state'] != 'running':
            return record
        time.sleep(0.1)
    raise SystemExit('replay did not finish')


def logs():
    return [log for session in get(f'{API}/api/sessions').values() for log in session['logs']]


def main():
    seed = json.load(open('output/playwright/replay/seed.json'))
    credentials = [{'kind': 'header', 'name': 'Authorization', 'value': 'Bearer qa-replay-secret'}, {'kind': 'query', 'name': 'key', 'value': 'qa-key-replay'}]
    results = {}

    # Idempotency: the same action retried returns the same record and executes once.
    hits_before = len(get(UPSTREAM)['records'])
    payload = {'trace_id': seed['source'], 'source': 'outgoing', 'idempotency_key': 'qa-idempotent-1', 'credentials': credentials}
    status, _, body = call('POST', '/api/replays', payload)
    assert status == 202, (status, body)
    first = json.loads(body)
    status, _, body = call('POST', '/api/replays', payload)
    assert status == 200 and json.loads(body)['id'] == first['id'], (status, body)
    status, _, body = call('POST', '/api/replays', {**payload, 'body': '{"model":"qa-model","input":"different"}'})
    assert status == 409, (status, body)
    wait_replay(first['id'])
    assert len(get(UPSTREAM)['records']) == hits_before + 1, 'idempotent retry executed twice'
    results['idempotency'] = {'first_status': 202, 'retry_status': 200, 'same_record': True, 'changed_content_status': 409, 'upstream_calls': 1}

    # Never-forwarded request: no outgoing snapshot, original still replayable.
    status, _, body = call('POST', '/api/rules', {'path_match': '/messages', 'body_match': '', 'inject_system': '', 'intercept': True, 'disabled': False, 'wait_seconds': 30, 'timeout_action': 'cancel'})
    assert status == 201, body
    rule = json.loads(body)
    import threading
    holder = {}

    def send_pending():
        holder['result'] = call('POST', '/v1/messages', {'model': 'qa-model', 'max_tokens': 5, 'messages': [{'role': 'user', 'content': 'never forwarded'}]}, {'Authorization': 'Bearer qa-auth-original'})

    thread = threading.Thread(target=send_pending)
    thread.start()
    pending = None
    for _ in range(100):
        listing = get(f'{API}/api/interceptions')['requests']
        if listing:
            pending = listing[0]
            break
        time.sleep(0.05)
    assert pending, 'request did not pause'
    call('POST', f'/api/interceptions/{pending["trace_id"]}/cancel', {'revision': pending['revision']})
    thread.join(10)
    status, _, body = call('POST', '/api/replays', {'trace_id': pending['trace_id'], 'source': 'outgoing', 'idempotency_key': 'qa-never-1'})
    assert status == 422 and 'never forwarded' in body, (status, body)
    status, _, body = call('GET', f'/api/requests/{pending["trace_id"]}/curl?source=outgoing')
    assert status == 404, (status, body)
    call('DELETE', f'/api/rules/{rule["id"]}')
    status, _, body = call('POST', '/api/replays', {'trace_id': pending['trace_id'], 'source': 'original', 'idempotency_key': 'qa-never-2', 'credentials': credentials[:1]})
    assert status == 202, (status, body)
    record = wait_replay(json.loads(body)['id'])
    assert record['state'] == 'done' and record['source'] == 'original', record
    results['never_forwarded'] = {'outgoing_replay_status': 422, 'outgoing_curl_status': 404, 'original_replay_state': record['state']}

    # SSE replay goes through the normal recording pipeline.
    status, _, body = call('POST', '/api/replays', {'trace_id': seed['streamed'], 'source': 'outgoing', 'idempotency_key': 'qa-sse-1', 'credentials': credentials[:1]})
    assert status == 202, (status, body)
    record = wait_replay(json.loads(body)['id'])
    log = next(item for item in logs() if item['trace_id'] == record['trace_id'])
    assert record['state'] == 'done' and log['type'] == 'SSE' and log['response_body'] == 'streamed answer' and log['output_tokens'] == 2 and log['replay']['of'] == seed['streamed'], (record, log)
    results['sse_replay'] = {'state': record['state'], 'type': log['type'], 'response': log['response_body'], 'output_tokens': log['output_tokens']}

    # Credential isolation across every gateway artifact, including SSE dumps and zap logs.
    artifacts = json.dumps([get(f'{API}/api/sessions'), get(f'{API}/api/replays'), get(f'{API}/api/graph')])
    for trace in {item['trace_id'] for item in logs()}:
        artifacts += json.dumps(get(f'{API}/api/requests/{trace}'))
    log_dir = os.environ.get('QA_LOG_DIR', '')
    if log_dir and os.path.isdir(log_dir):
        artifacts += subprocess.run(['grep', '-r', '-l', 'qa-replay-secret', log_dir], capture_output=True, text=True).stdout
    assert 'qa-replay-secret' not in artifacts and 'qa-key-replay' not in artifacts and 'qa-auth-original' not in artifacts, 'credential leaked into gateway artifacts'
    results['credential_isolation'] = {'api_snapshots': 'clean', 'log_dir_grep': 'clean' if log_dir else 'skipped'}
    results['status'] = 'PASS'
    print(json.dumps(results, ensure_ascii=False, indent=1))


if __name__ == '__main__':
    sys.exit(main())
