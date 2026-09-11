# Foundation refinement verification — 2026-09-11

Accepted on the local branch after fast-forwarding to `00ec270`. Checks use synthetic traffic only; no external Provider credentials or calls were needed.

## Delivered behavior

- New captures isolate privacy substitutions by admission-time conversation and upstream/client identity. Full-body projection precedes preview truncation; v1 captures remain restorable and v2 snapshots persist frozen namespaces.
- Cleanup preserves surviving restoration data and genuine identity aliases. Attached tool outputs retain only their referenced permitted token aliases, including after result cleanup/restart. Outbound privacy keeps immutable identity evidence and a bounded hash-only client-history anchor.
- New persistent replay registrations commit before dispatch. Failed SQL writes return 503 without a trace/upstream call and roll back the rejected key. Manager ordering prevents older notifications from overwriting newer keys. Shutdown and ambiguous restart states do not resume execution.
- Complete response inspection supports bounded byte paging, previous/next/reset/final navigation, UTF-8-safe text, exact Base64 fallback and independent source/replay panels. Full downloads remain exact.

## Verification

| Check | Result |
| --- | --- |
| `go test -race ./...` | Passed after final application fixes |
| `go vet ./...` | Passed |
| `cd web && npm run build` | Passed |
| Independent implementation review | Four findings fixed and rechecked; no remaining blocker |
| Synthetic API assertions | 32 passed |
| Browser assertions | 32 passed |
| Desktop/mobile/landscape controls | 1440x900, 390x844, 320x640 and 844x390 passed |

JSON (3,300,341 bytes), SSE (3,320,981 bytes) and binary (307,213 bytes) downloads matched the forwarded bytes exactly. Browser checks reached JSON/SSE end markers beyond 2 MiB, verified back/reset/refresh, independent comparison navigation and bounded rendering. Missing-file and zero-byte UI recovery used temporary browser route fixtures, removed before completion. No uncaught browser exception was recorded. Hashes, trace IDs and individual assertions are in [results.json](results.json).

SQLite tests use a separate reader at the first upstream replay hit, trigger-forced write failures and a subprocess exiting immediately after the durable commit without final flush. The restored action keeps its key and does not execute again. Privacy regressions cover late merges/detaches, raw client history, original/outgoing replay, live WebSocket bytes, v1 migration, response-only mappings, tool-result aliases and identity handoff.

Independent review additionally rejected malformed query parsing that otherwise selected unlimited reads, repaired copied tool-result restore ownership, prevented explicit aliases following edited inferred children, and preserved pre-substitution identity evidence. Permanent tests cover these failures.

## Reproduction and evidence

Start `mock_upstream.py --port 28003`, then an isolated built gateway on `127.0.0.1:12341`, target `http://127.0.0.1:28003/v1`, with a temporary capture/database directory, `-maxbody 256` and `-web web/dist`. Run `seed.py --output <temporary-results.json>`. Open `/ui/` in an isolated Playwright CLI session and run `browser_qa.js` with `run-code --filename`.

- [Desktop response paging](desktop-response.png)
- [Source/result comparison](comparison.png)
- [Mobile controls](mobile-comparison.png)
- [Landscape controls](landscape-comparison.png)

The owned synthetic services are reused for the following operator-workflow child, then stopped at parent-task completion. Databases, raw runtime captures and local browser state remain outside versioned evidence.

## Remaining boundaries

Historical tokens are not rewritten when late evidence changes grouping. Legacy captures keep their prior permissions. Mappings live while their namespace is referenced; discarded originals cannot be recovered. Ordinary metadata is still coalesced, and SQLite still stores a full metadata snapshot with in-memory filters. Durable registration does not imply exactly-once completion. Existing protocol/Provider/tracing subsets are unchanged.
