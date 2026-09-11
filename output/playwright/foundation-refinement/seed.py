"""Seed and verify an isolated gateway; never use against a real provider."""
import argparse
import hashlib
import json
import re
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--gateway", default="http://127.0.0.1:12341")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    address = urllib.parse.urlsplit(args.gateway)
    if address.scheme != "http" or address.hostname not in ("127.0.0.1", "localhost", "::1"):
        raise SystemExit("An isolated localhost gateway is required")
    results = {"checks": [], "traces": {}, "downloads": {}}

    def check(name, condition):
        if not condition:
            raise AssertionError(name)
        results["checks"].append({"name": name, "passed": True})

    def request(path, method="GET", value=None, headers=None):
        data = None if value is None else json.dumps(value, ensure_ascii=False).encode("utf-8")
        supplied = {} if headers is None else dict(headers)
        if data is not None:
            supplied["Content-Type"] = "application/json"
        req = urllib.request.Request(args.gateway + path, data, supplied, method=method)
        try:
            with urllib.request.urlopen(req, timeout=60) as response:
                return response.status, response.headers, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.headers, error.read()

    def api(path, method="GET", value=None):
        status, _, body = request(path, method, value)
        if not 200 <= status < 300:
            raise AssertionError(path + ": HTTP " + str(status) + " " + body.decode())
        return None if not body else json.loads(body)

    def capture(case, *, stream=False, session=None, authenticated=False):
        headers = {}
        if session:
            headers["X-Session-ID"] = session
        if authenticated:
            headers["Authorization"] = "Bearer synthetic-refinement-identity"
        body = {"model": "qa-debugger", "messages": [{"role": "user", "content": case}], "stream": stream}
        status, response_headers, raw = request("/v1/chat/completions", "POST", body, headers)
        expected_status = 429 if "qa-failure" in case else 200
        check(case + " forwarded", status == expected_status)
        trace = response_headers["X-Gateway-Trace-ID"]
        return trace, raw

    deadline = time.monotonic() + 30
    while True:
        try:
            with urllib.request.urlopen(args.gateway + "/api/rules", timeout=1) as ready:
                if ready.status == 200:
                    break
        except (urllib.error.URLError, TimeoutError):
            if time.monotonic() >= deadline:
                raise SystemExit("The isolated gateway did not become ready")
            time.sleep(0.1)

    for rule in api("/api/rules"):
        api("/api/rules/" + rule["id"], "DELETE")
    policy = api("/api/privacy")
    plain = {**policy, "record": False, "outbound": False, "retain_raw": True, "allow_reveal": False}
    api("/api/privacy", "PUT", plain)

    for label, case, stream in [
        ("json", "qa-large-json", False),
        ("sse", "qa-large-sse", True),
        ("binary", "qa-binary", False),
        ("empty", "qa-empty", False),
    ]:
        trace, wire = capture(case, stream=stream, session="qa-response-browsing")
        results["traces"][label] = trace
        status, _, downloaded = request("/api/responses/" + trace + "/download")
        check(label + " exact full download", status == 200 and wire == downloaded)
        if label in ("json", "sse"):
            check(label + " larger than 2 MiB", len(wire) > 2 << 20)
        results["downloads"][label] = {"bytes": len(wire), "sha256": hashlib.sha256(wire).hexdigest()}

    replay_request = {
        "trace_id": results["traces"]["json"], "source": "outgoing",
        "idempotency_key": "qa-refinement-" + str(uuid.uuid4()),
    }
    replay = api("/api/replays", "POST", replay_request)
    deadline = time.monotonic() + 30
    while replay["state"] == "running" and time.monotonic() < deadline:
        time.sleep(0.05)
        replay = api("/api/replays/" + replay["id"])
    check("large response replay completed", replay["state"] == "done")
    duplicate = api("/api/replays", "POST", replay_request)
    check("duplicate replay uses same trace", duplicate["trace_id"] == replay["trace_id"])
    results["traces"]["replay"] = replay["trace_id"]
    results["replay"] = replay

    private = {**plain, "record": True, "allow_reveal": True}
    api("/api/privacy", "PUT", private)
    scopes = {}
    for name, session in [("a1", "qa-private-a"), ("a2", "qa-private-a"), ("b", "qa-private-b")]:
        trace, wire = capture("qa-response-only", session=session, authenticated=True)
        detail = api("/api/responses/" + trace)
        tokens = re.findall(r"\[PRIVATE_email_[a-f0-9]{20}\]", detail["body"])
        check(name + " recording projection exists", bool(tokens) and "response-only@example.test" not in detail["body"])
        check(name + " recording does not change forwarding", b"response-only@example.test" in wire)
        scopes[name] = (trace, tokens[0])
    check("same session stable placeholder", scopes["a1"][1] == scopes["a2"][1])
    check("independent sessions isolated", scopes["a1"][1] != scopes["b"][1])
    cross = api("/api/privacy/restore", "POST", {"trace_id": scopes["b"][0], "body": scopes["a2"][1]})
    check("unrelated session cannot reveal token", cross["body"] == scopes["a2"][1])
    api("/api/history/" + scopes["a1"][0], "DELETE")
    own = api("/api/privacy/restore", "POST", {"trace_id": scopes["a2"][0], "body": scopes["a2"][1]})
    check("same-session survivor retains response-only mapping", own["body"] == "response-only@example.test")
    successor, _ = capture("qa-response-only", session="qa-private-a", authenticated=True)
    successor_log = api("/api/history/" + successor)
    successor_body = api("/api/responses/" + successor)["body"]
    check("new capture keeps session after first trace cleanup", successor_log["session_id"] == "qa-private-a")
    check("new capture keeps placeholder after first trace cleanup", scopes["a2"][1] in successor_body)
    api("/api/history/cleanup", "POST", {"session_id": "qa-private-a"})
    other = api("/api/privacy/restore", "POST", {"trace_id": scopes["b"][0], "body": scopes["b"][1]})
    check("cleaned session does not affect another restore", other["body"] == "response-only@example.test")
    results["privacy_survivor"] = {"trace": scopes["b"][0], "token": scopes["b"][1]}
    api("/api/privacy", "PUT", plain)

    for label, case in [("failure", "qa-failure"), ("slow", "qa-slow"), ("tool", "qa-tool")]:
        trace, _ = capture(case, session="qa-diagnosis")
        results["traces"][label] = trace
    Path(args.output).write_text(json.dumps(results, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(results, ensure_ascii=False))


if __name__ == "__main__":
    main()
