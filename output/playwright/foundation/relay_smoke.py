"""Small real-provider smoke test. All credentials come from process environment.

GATEWAY_TEST_URL points to the locally running gateway (without /v1).
GATEWAY_TEST_MODEL and GATEWAY_TEST_KEY are required; no secret is saved.
"""
import gzip
import json
import os
import time
import urllib.error
import urllib.request

gateway = os.environ['GATEWAY_TEST_URL'].rstrip('/')
model = os.environ['GATEWAY_TEST_MODEL']
key = os.environ['GATEWAY_TEST_KEY']
client = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def read(path):
    with client.open(gateway + path, timeout=10) as response:
        return response.read()


def run(stream):
    payload = {'model': model, 'messages': [{'role': 'user', 'content': 'Reply with only: gateway-ok'}], 'max_tokens': 128, 'stream': stream}
    if stream:
        payload['stream_options'] = {'include_usage': True}
    request = urllib.request.Request(gateway + '/v1/chat/completions', data=json.dumps(payload).encode(), headers={'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json'})
    with client.open(request, timeout=60) as response:
        trace = response.headers['X-Gateway-Trace-ID']
        wire = response.read()
        if response.headers.get('Content-Encoding') == 'gzip':
            wire = gzip.decompress(wire)
    log = None
    for _ in range(60):
        log = json.loads(read('/api/history/' + trace))
        if log['status'] not in ('pending', 'running'):
            break
        time.sleep(0.05)
    detail = json.loads(read('/api/responses/' + trace))
    downloaded = read('/api/responses/' + trace + '/download')
    capture = json.loads(read('/api/requests/' + trace))
    assert downloaded == wire, 'response download changed the relayed bytes'
    assert key not in json.dumps([log, detail, capture]), 'credential leaked into capture API'
    assert log['status'] == 'done', 'gateway did not record a successful completion'
    return {'case': 'sse' if stream else 'json', 'status': log['status_code'], 'trace_id': trace, 'response_bytes': len(wire), 'download_exact': True, 'credential_isolated': True, 'token_sources': log['token_sources'], 'ttfb_ms': log.get('ttfb_ms'), 'ttfc_ms': log.get('ttfc_ms')}


if __name__ == '__main__':
    try:
        for streaming in (False, True):
            print(json.dumps(run(streaming)), flush=True)
    except urllib.error.HTTPError as error:
        print(json.dumps({'error': 'HTTP request failed', 'status': error.code}), flush=True)
        raise SystemExit(1)
    except Exception as error:
        print(json.dumps({'error': type(error).__name__, 'message': str(error).replace(key, '[redacted]')}), flush=True)
        raise SystemExit(1)
