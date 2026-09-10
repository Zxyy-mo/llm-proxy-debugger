"""Run the exported command with the real curl binary and compare what the
synthetic upstream received with the captured request."""
import json
import os
import subprocess
import sys
import urllib.request

API = 'http://127.0.0.1:12338'
UPSTREAM = 'http://127.0.0.1:28001/'


def get(url):
    with urllib.request.urlopen(url, timeout=10) as response:
        return json.load(response)


def main(trace):
    before = get(UPSTREAM)['records']
    capture = get(f'{API}/api/requests/{trace}')
    results = {}
    for source in ('original', 'outgoing'):
        export = get(f'{API}/api/requests/{trace}/curl?source={source}')
        env = {**os.environ, 'REPLAY_AUTHORIZATION': 'qa-auth-from-env', 'REPLAY_QUERY_KEY': 'qa-key-from-env'}
        completed = subprocess.run(['sh', '-c', export['command']], env=env, capture_output=True, text=True, timeout=30)
        if completed.returncode != 0:
            raise SystemExit(f'{source}: curl failed: {completed.stderr}')
        records = get(UPSTREAM)['records']
        assert len(records) == len(before) + 1, 'expected exactly one new upstream request'
        hit = records[-1]
        before = records
        # Both commands end at the upstream with the rule-applied body: the
        # original one re-enters the gateway (rules apply once), the outgoing
        # one already contains them and bypasses the gateway.
        expected = capture['outgoing']['body']
        assert hit['body'] == expected, f'{source}: body differs'
        assert hit['auth'] == 'env', f'{source}: credential from the environment did not arrive: {hit["auth"]}'
        assert 'key=qa-key-from-env' in hit['query'] and 'trace=qa' in hit['query'], f'{source}: query differs: {hit["query"]}'
        assert hit['content_length'] == len(expected.encode()), f'{source}: content length differs'
        assert hit['user_agent'] == 'Python-urllib/3.12', f'{source}: user agent not preserved: {hit["user_agent"]}'
        # The original command goes through the gateway again: rules apply once
        # and a new call is recorded. The outgoing command bypasses the gateway.
        assert hit['system_count'] == 1, f'{source}: injected system count {hit["system_count"]}'
        results[source] = {'destination': export['destination'], 'status': 'PASS', 'upstream_body_matches': True, 'env_credentials_used': True}
        if source == 'original':
            logs = [log for session in get(f'{API}/api/sessions').values() for log in session['logs']]
            recorded = [log for log in logs if log.get('user_agent') == 'Python-urllib/3.12' and log['trace_id'] != trace and not log.get('replay') and 'tail' in (log.get('summary') or '')]
            assert recorded, 'gateway did not record the curl request as a new call'
            assert recorded[-1]['query'] == 'trace=qa&key=****', recorded[-1]['query']
            replayed = get(f'{API}/api/requests/{recorded[-1]["trace_id"]}')
            assert replayed['original']['body'] == capture['original']['body'], 'gateway capture of the curl request differs from the original bytes'
            assert replayed['outgoing']['body'] == expected, 'gateway forwarded something other than the rule-applied body'
            results[source]['gateway_recorded_trace'] = recorded[-1]['trace_id']
            results[source]['gateway_original_bytes_match'] = True
    print(json.dumps({'status': 'PASS', 'trace': trace, 'sources': results}, ensure_ascii=False, indent=1))


if __name__ == '__main__':
    main(sys.argv[1])
