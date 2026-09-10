# Foundation verification — 2026-09-10

Scope: M2–M8 on **refactor/modular-structure**, retaining the existing correlation, graph, interception and replay baseline. Results cover the implementation delivered with this report.

## Checks completed

| Check | Result |
| --- | --- |
| `go test -race ./...` | Passed after all backend edits |
| `go vet ./...` | Passed |
| `npm run build` | Vue typecheck and Vite production build passed |
| `git diff --check` | Passed |
| Browser functional assertions | 25 passed |
| Provider form/history/tool graph manual checks | Passed |
| Target viewports | 5 passed |
| Process restart recovery | 13 assertions passed |
| History and file cleanup | 2 records / 6 associated body files removed |
| Real `glm-5.3-flash` JSON/SSE relay | Both HTTP 200, complete downloads byte-identical |

Environment: Windows amd64, Go 1.24.0, Node 24.18.0, npm 11.16.0. Synthetic upstream: `127.0.0.1:28002`; gateway/UI: `127.0.0.1:12340/ui/`. Real relay used a separate gateway at `127.0.0.1:12339`, the supplied `/v1` base and environment-only credentials.

## Functional and protocol coverage

The browser suite created captured parent/child requests, verified explicit lineage and instruction/context changes, downloaded a large SSE stream exactly, checked bounded previews and authoritative usage, enriched a visible tool call with a reported 125 ms MCP Span, and found an independent graph node.

It exercised Anthropic JSON and Responses SSE conversion, distinct upstream/client response variants, a browser-originated Responses WebSocket call, history selection, remote-context notices, provider-managed replay credentials, replay/source comparison, record-only privacy and the tracing example.

Manual checks saved Provider settings, retrieved a stored synthetic Response without creating a model call, opened a tool node and inspected its reported input/output/duration. A later UI check verified that omitted provider auth headers display `Authorization`, tool node accessibility text says `工具调用`, and the final reload has no console errors/warnings.

Backend regressions cover protocol errors/cancellation, fallback before output, no retry after partial output, frozen outgoing replay, credential isolation, gzip/binary/UTF-8 fidelity, 2 MiB SSE observation limits and conversion failure behavior. Additional regressions check escaped URL paths through capture/export/replay, large JSON integers in history evidence and distinct `api-key` identities.

The HTTP breakpoint smoke used the actual UI “放行” action. It returned 200, recorded 41,011.296 ms of waiting and only 2.586 ms to first effective upstream content; waiting was excluded from the latter.

## Restart and cleanup

The gateway was stopped while a known intercepted request remained pending, after an explicit metadata flush. It was restarted against the same SQLite file and capture directory.

Thirteen checks confirmed Provider config, privacy policy, rules, explicit interruption status/reason, an empty pending queue, record counts, completed metrics/lineage/routes/tools/WebSocket metadata, complete original/outgoing request snapshots, response hashes, independent tool nodes, replay records, persisted idempotency and no new upstream request on restart/retry.

The baseline contained 13 terminal records plus one held request, with **12 saved client/upstream response files** checked by SHA-256. A non-retrying Python client was used because Chrome can independently retry a POST when its connection is reset. This distinguishes client retry behavior from gateway recovery.

Cleanup protected an active request (409 for single delete; skipped in bulk cleanup). Deleting a dedicated synthetic parent preserved the child's missing-parent reference; deleting the remaining session removed its record and files. Both sets of owned body files were absent afterward. The temporary interception rule was removed.

## Responsive UI

Every visible enabled control in Provider, Privacy and History panels was scrolled into view and tested for reachability. No checked control was unreachable, and no document/panel horizontal overflow was found.

| Requested | Observed CSS viewport |
| --- | --- |
| 390 × 844 | 390 × 845 |
| 844 × 390 | 845 × 390 |
| 320 × 568 | 320 × 568 |
| 768 × 1024 | 768 × 1025 |
| 1440 × 900 | 1440 × 901 |

The one-pixel differences are Chrome emulation rounding. Screenshots use only synthetic data:

- [Final Provider form](desktop-final.png)
- [Mobile Provider controls](mobile-providers.png)
- [Landscape Provider controls](landscape-providers.png)
- [Desktop Provider form](desktop-providers.png)
- [Tool graph](tools-graph.png)
- [Initial desktop](desktop-initial.png)

## Real relay evidence

The final binary was built as `gateway-foundation-verified.exe`. Its default upstream used the user-supplied base URL ending in `/v1` and model **`glm-5.3-flash`**.

| Mode | Trace | HTTP | Captured bytes | TTFB / TTFC |
| --- | --- | --- | --- | --- |
| JSON | `83c78430-18f3-4011-92ec-9a8d9adc684f` | 200 | 1,618 | 4,019.598 / 4,021.198 ms |
| SSE | `e965f0f5-2da6-47b0-b57c-6c3eda2f5081` | 200 | 36,100 | 633.053 / 634.281 ms |

Both downloads matched received bytes exactly. All three token-source counters were `usage`. Capture, response and log APIs excluded the supplied credential value.

These timings are smoke observations, not performance benchmarks. Real Responses/history/WebSocket/conversion capabilities were **not** inferred from the Chat Completions checks; those features have controlled mock/protocol verification.

## Reproduction

1. Build the UI and gateway, then start `mock_upstream.py --port 28002`.
2. Start the isolated gateway at `12340`, target `http://127.0.0.1:28002/v1`, logdir `output/foundation-qa`, and set a synthetic `GATEWAY_QA_KEY` in its process environment.
3. Load `/ui/` and run `browser_qa.js` via Chrome DevTools `evaluate_script`. The function is self-contained and restricts itself to the synthetic origin.
4. Apply the five device viewports and run `viewport_qa.js` for each.
5. For restart testing, create a rule matching `foundation pending`, run `restart_qa.py hold` in one terminal, then `prepare`. Stop only the owned gateway, restart it on the same directory, then run `verify` and `cleanup`. The held client has a 300-second timeout; finish the stop/restart step within that period.
6. For real relay testing, start a separate gateway pointing to the intended provider; supply `GATEWAY_TEST_URL` (local gateway origin), `GATEWAY_TEST_MODEL` and `GATEWAY_TEST_KEY` only in process environment, then run `relay_smoke.py`.

`results.json` records traces, viewport assertions, recovery checks and real smoke metrics. Runtime data/credentials are not published with the curated report.

## Product boundaries

Final artifact checks scanned 240 versioned/new deliverable files and 26 real-relay runtime files: no supplied credential value was found. UTF-8 decoding, replacement-character checks and local links passed. The two owned gateways and synthetic upstream were stopped after flushing; ports 12339, 12340 and 28002 had no remaining test listeners at the end of verification.

Current storage is a coalesced SQLite metadata snapshot plus body files and in-memory filters. Abrupt termination has a pre-flush metadata window; full JSON/in-flight text still consumes memory. Conversion is a defined text/function subset; WebSocket frames are observable but not editable/replayable by HTTP controls. History reads require a saved Response ID. External Spans require instrumentation; this gateway does not execute complete Agent/MCP workflows.
