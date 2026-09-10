"""Synthetic restart and cleanup verification, using only 127.0.0.1 services.

Run hold in a separate terminal to keep a request pending without client retries,
run prepare, stop the owned gateway, restart it with the same data directory,
then run verify and cleanup. State contains synthetic metadata and body hashes.
"""
import argparse
import hashlib
import http.client
import json
from pathlib import Path
import time
import urllib.error
import urllib.request

GATEWAY = "http://127.0.0.1:12340"
UPSTREAM = "http://127.0.0.1:28002"
ROOT = Path(__file__).resolve().parents[3]
DATA = ROOT / "output" / "foundation-qa"
STATE = DATA / "restart-baseline.json"
client = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def api(path, method="GET", body=None, status=200, origin=GATEWAY, timeout=15):
    request = urllib.request.Request(
        origin + path, method=method,
        data=None if body is None else json.dumps(body).encode("utf-8"),
        headers={} if body is None else {"Content-Type": "application/json"})
    try:
        response = client.open(request, timeout=timeout)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        raw = response.read()
        assert response.status == status, (path, response.status, raw[:300])
        return json.loads(raw) if raw else None


def response_hash(trace, variant="client"):
    with client.open(GATEWAY + "/api/responses/" + trace + "/download?variant=" + variant, timeout=15) as response:
        return hashlib.sha256(response.read()).hexdigest()


def hold():
    # A non-retrying client isolates gateway recovery from browser network retries.
    print(json.dumps({"phase": "hold", "waiting": True}), flush=True)
    try:
        api("/v1/responses", "POST", {"model": "qa-native", "input": "foundation pending restart recovery"}, timeout=300)
    except (http.client.RemoteDisconnected, ConnectionError, urllib.error.URLError):
        return {"phase": "hold", "connection_closed": True, "retried": False}
    raise AssertionError("held request unexpectedly completed")


def prepare():
    pending = api("/api/interceptions")["requests"]
    assert len(pending) == 1, "exactly one pending synthetic request is required"
    trace = pending[0]["trace_id"]
    log = api("/api/history/" + trace)
    assert "foundation pending restart recovery" in log["summary"]
    api("/api/history/" + trace, "DELETE", status=409)
    skipped = api("/api/history/cleanup", "POST", {"session_id": log["session_id"]})
    assert skipped == {"deleted": 0, "active_skipped": 1}
    providers = api("/api/providers")["config"]
    api("/api/providers", "PUT", providers)  # Explicit persistence barrier.
    logs = api("/api/history?limit=200")["items"]
    completed = [item for item in logs if item["status"] not in ("pending", "running")]
    captures, responses = {}, {}
    for item in completed:
        current = item["trace_id"]
        captures[current] = api("/api/requests/" + current)
        snapshot = api("/api/responses/" + current)
        if snapshot["stored"] == "file":
            responses[current + ":client"] = response_hash(current)
        if item.get("route", {}).get("conversion"):
            responses[current + ":upstream"] = response_hash(current, "upstream")
    state = {
        "pending": trace, "logs": completed, "total": len(logs),
        "providers": providers, "privacy": api("/api/privacy"),
        "rules": api("/api/rules"), "replays": api("/api/replays"),
        "captures": captures, "responses": responses,
        "upstream_count": api("/", origin=UPSTREAM)["count"],
        "tools": [node for node in api("/api/graph")["nodes"] if node["kind"] == "tool"],
    }
    STATE.write_text(json.dumps(state, ensure_ascii=False, indent=2), encoding="utf-8")
    return {"phase": "prepare", "pending": trace, "completed": len(completed),
            "capture_hashes": len(responses), "active_delete_protected": True}


def verify():
    state = json.loads(STATE.read_text(encoding="utf-8"))
    checks = []

    def check(name, value):
        assert value, name
        checks.append(name)

    check("provider configuration restored", api("/api/providers")["config"] == state["providers"])
    check("privacy policy restored", api("/api/privacy") == state["privacy"])
    check("rules restored", api("/api/rules") == state["rules"])
    pending = api("/api/history/" + state["pending"])
    check("interrupted request has explicit restart reason",
          pending["status"] == "error" and pending["status_code"] == 503
          and pending["interception"]["reason"] == "gateway_restarted"
          and pending["token_sources"] == {"input": "unknown", "output": "unknown", "thinking": "unknown"})
    check("pending queue is empty", not api("/api/interceptions")["requests"])
    check("no duplicate or missing model records", api("/api/history?limit=1")["total"] == state["total"])
    for expected in state["logs"]:
        actual = api("/api/history/" + expected["trace_id"])
        for key in ("status", "correlation", "route", "tools", "token_sources", "ttfb_ms", "ttfc_ms", "websocket", "privacy"):
            assert actual.get(key) == expected.get(key), (expected["trace_id"], key)
    checks.append("completed metrics, lineage, routes, tools and WebSocket metadata restored")
    for trace, capture in state["captures"].items():
        assert api("/api/requests/" + trace) == capture, trace
    checks.append("complete original and outgoing request snapshots restored")
    for key, digest in state["responses"].items():
        trace, variant = key.split(":")
        assert response_hash(trace, variant) == digest, key
    checks.append("all client and upstream response variants retain exact bytes")
    tools = [node for node in api("/api/graph")["nodes"] if node["kind"] == "tool"]
    check("independent tool graph nodes restored", tools == state["tools"])
    check("replay records restored", api("/api/replays") == state["replays"])
    record = state["replays"][0]
    repeated = api("/api/replays", "POST", {
        "trace_id": record["replay_of"], "source": record["source"],
        "idempotency_key": record["idempotency_key"]})
    check("replay idempotency survives restart", repeated["id"] == record["id"])
    time.sleep(0.3)
    check("restart and replay retry did not resend traffic", api("/", origin=UPSTREAM)["count"] == state["upstream_count"])
    return {"phase": "verify", "passed": len(checks), "checks": checks,
            "completed": len(state["logs"]), "capture_hashes": len(state["responses"])}


def model(input_text, parent=None):
    body = {"model": "qa-native", "input": input_text}
    if parent:
        body["previous_response_id"] = parent
    response = api("/v1/responses", "POST", body)
    for _ in range(60):
        logs = api("/api/history?q=" + response["id"])["items"]
        if len(logs) == 1 and logs[0]["status"] == "done":
            return logs[0], response
        time.sleep(0.05)
    raise AssertionError("model record did not finish")


def cleanup():
    policy = api("/api/privacy")
    api("/api/privacy", "PUT", {**policy, "record": False, "outbound": False, "retain_raw": True, "allow_reveal": False})
    parent, response = model("foundation cleanup parent")
    child, _ = model("foundation cleanup child", response["id"])
    assert child["correlation"]["parent_trace_id"] == parent["trace_id"]
    parent_files = list(DATA.rglob(parent["trace_id"] + "*"))
    child_files = list(DATA.rglob(child["trace_id"] + "*"))
    assert len(parent_files) >= 3 and len(child_files) >= 3
    api("/api/history/" + parent["trace_id"], "DELETE")
    assert not any(path.exists() for path in parent_files)
    api("/api/history/" + parent["trace_id"], status=404)
    survivor = api("/api/history/" + child["trace_id"])
    assert survivor["correlation"]["warning"] == "deleted_parent"
    assert not survivor["correlation"].get("parent_trace_id")
    result = api("/api/history/cleanup", "POST", {"session_id": child["session_id"]})
    assert result["deleted"] == 1 and result["active_skipped"] == 0
    assert not any(path.exists() for path in child_files)
    api("/api/history/" + child["trace_id"], status=404)
    for rule in api("/api/rules"):
        if rule["body_match"] == "foundation pending":
            api("/api/rules/" + rule["id"], "DELETE", status=204)
    api("/api/providers", "PUT", api("/api/providers")["config"])
    return {"phase": "cleanup", "deleted": 2, "removed_files": len(parent_files) + len(child_files),
            "deleted_parent_reference_preserved": True, "synthetic_rule_removed": True}


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("phase", choices=("hold", "prepare", "verify", "cleanup"))
    args = parser.parse_args()
    print(json.dumps(globals()[args.phase](), ensure_ascii=False), flush=True)
