# Request cURL export and single-request replay verification

Date: 2026-09-10

## Delivered scope

Completed milestone M1 from IMPLEMENTATION_ROADMAP.md: safe cURL export for the client-original and effective-outgoing snapshots, exact body-file download, and operator-initiated single model request replay with transient credentials, validation, idempotency, cancellation, timeouts and provenance. Prior conversation correlation, graph, layout and interception work remains present and uncommitted.

## Automated verification

- `go test -race ./...` and `go vet ./...` passed after the final changes (see `go_tests` in [results.json](results.json)).
- `cd web && npm run build` passed (Vue TypeScript checking and production build).
- New Go tests cover: shell quoting of quotes, `$`, backticks, backslashes, newlines, `!` and Unicode; credential placeholders for header, query and URL credentials; signed schemes marked non-replayable; hop-by-hop, `Connection`-listed, forwarded and `Content-Length` headers dropped; multi-value headers preserved; curl default headers suppressed; inline versus body-file export by size and encoding; a real `curl` run through `sh` that reproduces both an inline Unicode body and a 64 KiB file body byte for byte with environment-supplied credentials; export API defaults, sources and status codes; outgoing replay without a second injection or breakpoint; original replay re-applying rules exactly once; provenance in logs, sessions and graph edges; idempotent retries, reused keys and new keys; transient credentials never present in sessions, replays, captures, graph or exports; rejected credential shapes; validated and rejected edits with immutable source records; cancellation (499, upstream request stopped), replay deadline (504, reason `timeout`) and upstream failures; destination host and prefix checks; non-generating requests and never-forwarded requests refused explicitly; gzip bodies replayed unchanged and never edited; SSE replays recorded through the normal pipeline; replay manager idempotency, cancellation and deadline reasons; store copies that do not alias forwarding metadata, and replay session membership that yields to explicit identifiers.

## Browser verification

Used an isolated local proxy at 12338, Vite at 5174 and a synthetic upstream at 28001 (`mock_upstream.py`). No external model service was called. Structured evidence is in [results.json](results.json); the CLI snippets (`curl_panel.js`, `workbench.js`, `layout_checks.js`) and API scripts (`seed.py`, `run_exported_curl.py`, `api_cases.py`) are in this directory.

- The compare view lists credential names (`Authorization (Bearer)`, `?key=`) without values and masks the secret query parameter as `key=****`.
- The cURL panel defaults to the outgoing snapshot addressed to the upstream (`http://127.0.0.1:28001/…`), includes the rule-injected content, renders `${REPLAY_AUTHORIZATION}` and `${REPLAY_QUERY_KEY}` placeholders, and never contains the Vite origin or a secret. Switching to the original snapshot targets the gateway (`http://127.0.0.1:12338/…`) without the injected content.
- The copy button places exactly the API command on the clipboard; the body download equals the captured bytes and uses the file name the command references.
- Both exported commands were executed with the real `curl` binary and environment credentials. The upstream received byte-identical bodies, the environment credentials, the preserved `User-Agent`, exactly one injected system, and the correct `Content-Length`. The original-source command was recorded by the gateway as a new call whose own capture matches the original bytes.
- The replay workbench keeps the run button disabled and lists the missing credentials until they are entered; validation works before credentials exist and reports what is still missing; an invalid JSON draft is rejected without creating a replay or an upstream request.
- An edited outgoing replay with transient credentials reached the upstream exactly once, with the `Bearer` scheme applied, the edited text, the literal `你好` escape and the integer `9007199254740993` preserved, and one injected system. The record shows `done · HTTP 200`; the new log carries `replay.of`; the source record is unchanged and both share a session. No gateway record, API payload or browser storage contains the supplied secrets.
- Provenance: "查看新调用" opens the replayed request; the inspector shows the replay section and jumps back to the source; the graph draws one dotted "重放来源" edge, no parent edge, and a "重放·已修改" badge on the node.
- A slow replay was canceled from the UI: record `canceled (manual)`, log status `canceled` with 499, and the upstream saw the connection close. A two-second timeout produced `失败（超时） · HTTP 504`. The history list showed all three replays.
- Reloading the page created no replay and no upstream request.
- API cases: an idempotent retry returned the same record with one upstream call; the same key with different content returned 409; a never-forwarded request refuses outgoing export (404) and outgoing replay (422) while its original replays; an SSE replay was recorded as `SSE` with `streamed answer` and 2 output tokens; the gateway log directory contained no credential values.
- All browser suites reported zero page errors.

## Layout verification

Checked the cURL copy button, replay run/validate buttons, the credential field and the body editor at seven viewports. No document-level horizontal overflow was observed.

| Viewport | Copy / Run / Validate / Credential reachable | Visible editor height |
| --- | --- | --- |
| 1600 × 1000 | Pass | 213 px |
| 1366 × 768 | Pass | 213 px |
| 1024 × 768 | Pass | 213 px |
| 768 × 600 | Pass | 213 px |
| 390 × 844 | Pass | 213 px |
| 320 × 640 | Pass | 213 px |
| 844 × 390 | Pass | 213 px |

Screenshots: [curl-panel.png](curl-panel.png), [workbench-done.png](workbench-done.png), [graph-provenance.png](graph-provenance.png), [mobile-replay.png](mobile-replay.png), [landscape-replay.png](landscape-replay.png).

## Limits observed

- Replays send one model API request; local tools, MCP servers and Agent loops are not orchestrated. Provider-managed tools inside the request may still run.
- Signed authentication schemes (AWS SigV4, Digest, HMAC, OAuth signatures) are exported as placeholders with a warning and cannot be replayed with a static secret.
- Replay records, like sessions and captures, live in process memory and are gone after a restart; no replay runs automatically after a restart or a page reload.
- Exported commands target POSIX shells (`sh`, `bash`, `zsh`).

## Cleanup

The three QA listeners were stopped after verification and the temporary gateway log directory was discarded.
