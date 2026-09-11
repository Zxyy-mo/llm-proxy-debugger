"""Create a realistic, entirely local Agent investigation for browser QA."""
import argparse
import json
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timedelta, timezone
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--gateway", default="http://127.0.0.1:12341")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    address = urllib.parse.urlsplit(args.gateway)
    if address.scheme != "http" or address.hostname not in ("127.0.0.1", "localhost", "::1"):
        raise SystemExit("Only an isolated localhost gateway is supported")

    def request(path, method="GET", value=None, headers=None):
        data = None if value is None else json.dumps(value, ensure_ascii=False).encode()
        supplied = dict(headers or {})
        if data is not None:
            supplied["Content-Type"] = "application/json"
        try:
            with urllib.request.urlopen(urllib.request.Request(args.gateway + path, data, supplied, method=method), timeout=40) as response:
                return response.status, response.headers, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.headers, error.read()

    def api(path, method="GET", value=None):
        status, _, data = request(path, method, value)
        if not 200 <= status < 300:
            raise AssertionError(path + ": " + str(status) + " " + data.decode())
        return None if not data else json.loads(data)

    policy = api("/api/privacy")
    api("/api/privacy", "PUT", {**policy, "record": False, "outbound": False, "retain_raw": True, "allow_reveal": False})
    for rule in api("/api/rules"):
        api("/api/rules/" + rule["id"], "DELETE")

    def capture(tag, prompt, session="qa-operator-main", *, messages=None, parent=None, stream=False):
        body = {
            "model": "qa-debugger",
            "messages": messages or [{"role": "user", "content": prompt}],
            "stream": stream,
            "metadata": {"qa_case": tag},
            "seed": 9007199254740993,
        }
        headers = {"X-Session-ID": session, "Authorization": "Bearer synthetic-loop-seed"}
        if parent:
            headers["X-Parent-Trace-ID"] = parent
        status, reply_headers, reply_body = request("/v1/chat/completions", "POST", body, headers)
        expected = 429 if "qa-failure" in prompt else 200
        if status != expected:
            raise AssertionError(tag + ": HTTP " + str(status))
        return {
            "trace": reply_headers["X-Gateway-Trace-ID"],
            "status": status,
            "request": body,
            "response": None if stream else json.loads(reply_body),
        }

    fixtures = {"created_at": datetime.now(timezone.utc).isoformat(), "traces": {}, "checks": []}
    parent_prompt = "op-loop-parent qa-tool retrieve the deployment guide"
    parent = capture("operator-parent", parent_prompt)
    tool_call = parent["response"]["choices"][0]["message"]["tool_calls"][0]
    tool_id = tool_call["id"]
    child_messages = [
        {"role": "system", "content": "Use the retrieved guide and give a concrete answer."},
        {"role": "user", "content": parent_prompt},
        parent["response"]["choices"][0]["message"],
        {"role": "tool", "tool_call_id": tool_id, "content": "Deployment guide: check configuration, run validation, then release."},
        {"role": "user", "content": "op-loop-failure qa-failure explain the deployment result"},
    ]
    failed = capture("operator-failure", child_messages[-1]["content"], messages=child_messages, parent=parent["trace"])
    started = datetime.now(timezone.utc) - timedelta(seconds=1)
    api("/api/tool-spans", "POST", {
        "trace_id": parent["trace"], "span_id": "operator-lookup", "call_id": tool_id,
        "name": "lookup_document", "kind": "mcp", "status": "done",
        "started_at": started.isoformat(), "ended_at": (started + timedelta(milliseconds=125)).isoformat(),
        "input": '{"query":"deployment guide"}', "output": "Found the deployment guide.",
    })
    fixtures["traces"]["parent"] = parent["trace"]
    fixtures["traces"]["failed"] = failed["trace"]
    fixtures["failed_request"] = failed["request"]

    # The initial failure remains on page two of the op-loop history search.
    for index in range(64):
        session = "qa-operator-main" if index < 34 else ("qa-operator-secondary-a" if index < 49 else "qa-operator-secondary-b")
        prompt = "op-loop-step-" + str(index).zfill(2) + " inspect synthetic Agent context"
        if index in (12, 37):
            prompt += " qa-slow"
        result = capture("operator-step-" + str(index), prompt, session)
        if index == 12:
            fixtures["traces"]["slow"] = result["trace"]
        if index == 50:
            fixtures["traces"]["other_source"] = result["trace"]

    # These sources are fast; editing them to include op-delay triggers a
    # cancelable upstream wait for the running-reset/navigation tests.
    for key, text, stream in [
        ("delay_source", "op-loop-delay-source", False),
        ("uncertain_source", "op-loop-uncertain-source", False),
        ("zero", "op-loop-zero qa-zero", False),
        ("estimated", "op-loop-estimated qa-estimated", True),
    ]:
        result = capture("operator-" + key, text, stream=stream)
        fixtures["traces"][key] = result["trace"]

    failed_log = api("/api/history/" + failed["trace"])
    parent_log = api("/api/history/" + parent["trace"])
    assert failed_log["status"] == "error" and failed_log["correlation"]["parent_trace_id"] == parent["trace"]
    assert parent_log["tools"][0]["duration_ms"] == 125
    assert api("/api/context-diff/" + failed["trace"])["base_trace_id"] == parent["trace"]
    history = api("/api/history?q=op-loop&limit=50")
    assert history["total"] >= 70 and history["next_cursor"] == "50"
    fixtures["checks"] = [
        {"name": "failed child has real parent evidence", "passed": True},
        {"name": "tool duration comes from explicit execution span", "passed": True},
        {"name": "context comparison chooses captured parent", "passed": True},
        {"name": "70+ calls span three sessions and multiple history pages", "passed": True},
    ]
    Path(args.output).write_text(json.dumps(fixtures, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({"checks": fixtures["checks"], "traces": fixtures["traces"], "history_total": history["total"]}, ensure_ascii=False))


if __name__ == "__main__":
    main()
