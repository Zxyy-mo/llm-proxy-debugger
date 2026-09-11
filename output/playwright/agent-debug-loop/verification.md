# Agent debugger workflow verification — 2026-09-11

The complete operator loop is accepted on top of the [foundation refinement](../foundation-refinement/verification.md). All traffic in this round used isolated local synthetic data. This verifies the actual gateway/UI workflow, not additional real Provider or SDK capability.

## Result

An operator can find a failing call, inspect its captured parent/tool context, edit and replay one request, compare the successful result and return to the same investigation. Draft reset/navigation does not hide an execution. Lost responses retain one submission/key and cannot silently create another model call.

| Check | Result |
| --- | --- |
| `go test -race ./...` | Passed |
| `go vet ./...` | Passed |
| `npm --prefix web test` | 46 tests passed |
| `npm --prefix web run build` | Type checking and production build passed |
| Independent code review | Findings fixed and rechecked |
| Browser assertions | 85 passed across the five scripts below |
| Responsive sizes | Seven passed, including real correction flows on phone and short landscape |

The primary fixture has 70 calls across three sessions, a real captured parent reference, a tool result and an explicit 125 ms execution span. The failed source returns 429; editing its final instruction produces a successful replay. Synthetic data, not timing proximity, establishes its relationships.

## Browser evidence

- `journey.js`: 23 assertions for keyword/status/duration lookup, retained graph evidence, captured-parent context, same-source draft/header/credential continuity, exact large-integer edits, one execution, readable result evaluation and history-page return.
- `replay_recovery.js`: 18 assertions for reset/navigation/reload while running, cancellation reaching the upstream, and recovery after both responses are lost or the hidden retry returns 503. Each ambiguity case made two management POST attempts but exactly one upstream execution, using one key. 404 retry ambiguity is covered by the real-wrapper helper regression.
- `layout_qa.js`: 27 assertions at 1600x1000, 1366x768, 1024x768, 768x600, 390x844, 320x640 and 844x390. Enabled controls received real hit-target checks, editors retained useful height, and documents did not overflow. Phone/landscape runs completed corrected replays. Explicit selection made the selected graph node readable; manual zoom survived a metric-only update and Show All still worked.
- `edge_states.js`: 13 assertions for a comparison opened while running and updated automatically on completion, downloadable final response, missing/recovered source, bounded real editor handling of deep valid JSON, initial fetch failure/retry, no captures and a working Settings action. Missing/initial-failure states used temporary browser routes; the routes were removed afterward.
- `keyboard_filter.js`: four assertions for keyboard activation, explicit no-match state, preserved selection and restoring choices after clearing filters.

Counts, keys and trace references are in [results.json](results.json). The source/result text and metrics are actual responses from the synthetic upstream. Preview equality is never claimed as full-body equality.

## Review fixes and limits

Review caught a lost first response followed by an apparently definitive retry error, a comparison holding an old running record, and unbounded shared JSON indentation before the bounded diff ran. The final code preserves uncertain submission identity, reconciles comparison records by identity and falls back to exact raw text before deep formatting can expand.

The lossless diff preserves integer, decimal and exponent spelling and bounds parsing/comparison/rendering. Known zero counters remain known; unknown and estimated counters retain their provenance. Complete responses keep independent 256 KiB paging and downloads. JSON text extraction for evaluation is limited to complete captures up to 64 KiB; observed output/thinking excerpts are bounded. Full raw content remains available below.

## Screenshots and reproduction

- [Readable focused investigation](focused-investigation.png)
- [Source/result evaluation](result-evaluation.png)
- [Phone evaluation](mobile-evaluation.png)
- [Short-landscape evaluation](landscape-evaluation.png)
- [Lost-response recovery](uncertain-both-lost.png)
- [Retry-503 recovery](uncertain-retry-503.png)
- [Bounded deep editor](deep-editor.png)

Build the UI/gateway; start `../foundation-refinement/mock_upstream.py --port 28003`. Start an isolated gateway on `127.0.0.1:12341`, target `http://127.0.0.1:28003/v1`, with a temporary database/capture directory and `-maxbody 256`. Run `seed.py --output <temporary-fixtures.json>`. Open `/ui/` in a named Playwright CLI session, then run the five scripts using `run-code --filename` in the order listed above. Use an isolated gateway because the fixture changes synthetic rules/privacy and creates records.

Browser route fixtures, databases, raw captures and process logs are not published. Owned QA services are stopped at delivery. Startup and operator instructions are in [docs/USAGE.md](../../../docs/USAGE.md).
