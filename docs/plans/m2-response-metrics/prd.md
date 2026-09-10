# Complete responses and trustworthy metrics (M2)

Status: planned, not implemented. Prepared on 2026-09-10 as a handoff after M1 (cURL export and single-request replay) was delivered. Read `HANDOFF.md` at the repository root first.

## Goal

Let an operator see and download the complete response of any captured or replayed request, know whether token counts are authoritative or estimated, and see when the upstream started answering — without mixing human/rule waiting into those timings.

## Evidence and existing patterns

- `internal/proxy/handler.go` `ModifyResponse` reads the whole JSON upstream body (gzip decoded) but `recorder.captureJSON` in `internal/proxy/recorder.go` keeps only `-maxbody` bytes for `RequestLog.response_body`; `correlation.ResponseCapture` already sees the full body for identifiers.
- SSE raw bytes are appended verbatim to `{logdir}/sse/{trace_id}.log` by `responseRecorder.dumpRaw`; the log only keeps the accumulated text, truncated. There is no API to read or download that file.
- Token counts come from `internal/protocol/{openai,anthropic,responses}.go`. `len([]rune(...))` values are character estimates; `Metrics.IsFinalOutputTokens` (and thinking) mark authoritative `usage` values that override the estimate in `protocol.Accumulator`. Nothing records which one the final number came from.
- `handler.go` records `upstreamStarted` when `captureTransport` sends the request and stores `upstream_duration_ms`; `wait_duration_ms` covers interception waiting. Response headers arrive in `ModifyResponse`; the first body bytes reach `responseRecorder.Write`; the first non-empty `Accumulator.OutputContent`/`ThinkingContent` is the first useful content. None of these instants is recorded today.
- `RequestLog.replay` (M1) links a replay to its source, so "compare this replay's response with the source response" is a natural UI extension of the replay workbench (`web/src/components/ReplayWorkbench.vue`) and the audit view (`RequestAudit.vue`).
- `web/src/lib/correlation.ts` `formatBody` re-indents JSON without parsing numbers; reuse it for full response display.

## Requirements

### Complete responses

- Keep the complete upstream response for JSON and SSE requests, independent of `-maxbody`. SSE can reuse the existing dump file; JSON needs either an in-memory full copy (mind memory growth) or a file next to the SSE dumps (`{logdir}/responses/{trace_id}.json`). Decide once and document it; persistence beyond process lifetime remains M5.
- `GET /api/responses/{trace_id}` returns metadata (`type` json/sse, `content_type`, `status_code`, byte count, encoding/compression facts, storage source) and the complete body or a clear "not stored" reason (for example the process restarted or the dump failed). `GET /api/responses/{trace_id}/download` streams the exact bytes with a stable file name, mirroring `GET /api/requests/{trace_id}/body`.
- Compressed JSON responses: store the decoded bytes the client would read, and say so; never present replacement characters as content.
- The UI shows the full response (formatted when JSON, raw event stream when SSE) in the details/inspector area with a download link, and lets a replay be compared with its source response.

### Trustworthy token counts

- Add a source label per counter (at least input and output; thinking if available): `usage` (provider usage), `estimated` (character count), `unknown` (no data, error, canceled). Expose it on `RequestLog`, graph nodes and live `sse_delta` metrics so the UI never displays an estimate as an exact number.
- Estimates must be replaced, not summed, when authoritative usage arrives (existing behaviour) and the label must flip at that moment.

### Timings

- Record `ttfb_ms` (first response byte/headers after `upstreamStarted`) and `ttfc_ms` (first useful content delta) measured from the actual upstream send, excluding wait time. Omit them (UI shows "unknown") for errors before headers, canceled requests, empty outputs and non-LLM responses.
- Provider queue or pure model time is shown only when the provider returns such data; do not infer it.

## Proposed API and data direction

- `RequestLog` additions: `token_sources: {input, output, thinking}`, `ttfb_ms`, `ttfc_ms`, `response_bytes`, `response_stored: memory|file|missing`.
- `GET /api/responses/{trace_id}` and `/download` as above; `sessions_updated`/`request_end` already carry the log, so no new WebSocket event should be necessary.
- Keep `RequestLog.response_body` truncated as today for list/live views; full content only through the new endpoint.

## Acceptance criteria

- [ ] A JSON response larger than `-maxbody` is downloadable byte for byte; an SSE stream is downloadable as the raw event stream that was forwarded.
- [ ] Gzip JSON responses are stored decoded and labelled; replacement characters never appear.
- [ ] Token labels are `usage` for all three protocols when usage is present, `estimated` for streams without usage, and `unknown` for errors/cancellations; the UI shows the label next to the numbers and in graph nodes.
- [ ] `ttfb_ms` and `ttfc_ms` exclude interception waiting (verify with an intercepted request) and are absent for failed/canceled requests.
- [ ] A replay's response can be compared with its source response from the replay workbench.
- [ ] Go race/vet, Vue build and browser checks against the local mock upstream pass; small/short viewport checks for the new panels pass.
- [ ] README, checklist, roadmap, a backend spec (`.trellis/spec/backend/response-metrics.md`) and `output/playwright/response-metrics/verification.md` describe the delivered contract and limits.

## Implementation sequence

1. Decide the JSON storage strategy and add the response store + API with tests (byte fidelity, gzip, missing file).
2. Add token source labels in `protocol` metrics, the accumulator and logs; update graph projection and live metrics.
3. Add TTFB/TTFC instants in the recorder/handler; test with delayed mock upstream and an intercepted request.
4. UI: full response panel, download, labels, timings, replay-vs-source comparison.
5. Verify end to end, write docs and evidence, update checklist/roadmap.

## Out of scope

Adjacent-context diff and rule protocol adaptation (M3), redaction (M4), persistence across restarts (M5), tool nodes/tracing (M6), multi-provider routing (M7), WebSocket frames (M8).
