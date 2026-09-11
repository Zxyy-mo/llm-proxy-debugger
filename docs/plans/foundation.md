# Current-branch foundation decisions

Status: implemented and verified on `refactor/modular-structure`, 2026-09-10. This branch is the product baseline. Existing modular proxy, correlation, interception and replay behavior remains the compatibility starting point.

## Delivered stages

1. **M2 — response fidelity and metrics:** complete file captures, byte-preserving downloads, gzip facts, per-counter provenance, upstream TTFB/TTFC and replay response comparison.
2. **M3 — context and rules:** evidence-based adjacent comparison, bounded message alignment, protocol-aware prompt injection and explicit priority.
3. **M4 — privacy:** separate recording/outbound policies, stable scoped substitutions and explicit retention/reveal choices.
4. **M5 — history:** SQLite metadata, file-backed bodies, configuration/index/replay restoration, queries and cleanup. Interrupted executions are not resumed.
5. **M6 — tools:** API-visible tool intentions/results plus application-reported execution Spans and independent graph nodes.
6. **M7 — providers:** model routing, aliases, environment credentials, explicit fallback and bounded Anthropic/Responses → Chat text/function conversion.
7. **M8 — integrations:** Responses WebSocket call observation and saved Response retrieval by known ID.

Each stage includes backend behavior, usable UI, contracts and tests. See [contract index](../README.md) and [verification](../../output/playwright/foundation/verification.md).

## Representation and fidelity

Complete responses are stored under the configured log directory; `-maxbody` governs previews, not full files. Gzip JSON is stored decoded and labelled, binary data is represented as Base64 for API inspection, and downloads preserve saved bytes. Conversion exposes separate upstream/client variants.

WebSocket JSONL envelopes preserve individual original text payloads with whitespace; they do not claim packet-level wire capture. Privacy stores a final output projection instead of raw SSE/WS frames and clearly distinguishes it from originals.

Path joining handles origins, mounts and standard `/v1` bases while preserving escaped IDs/mount segments. Outgoing replay reuses the frozen wire destination/body and avoids aliasing/conversion/rules twice.

## Metrics and evidence

Token source is independent for input/output/thinking: `usage`, `estimated` or `unknown`. Explicit zero usage is authoritative. TTFB starts at upstream dispatch and ends at headers, TTFC at the first meaningful observed content. These omit human wait and do not invent provider-internal time.

A 2 MiB SSE parser limit stops observation and reports incomplete text/tools/metrics while native bytes continue. Partial parsing cannot create a completed transcript anchor.

Parent links need exact IDs or unique complete transcript evidence; shared sessions and WebSocket lane names do not prove causality. Canonical JSON fingerprints retain large integers. Tool duration comes only from a reported Span, never gaps between model requests.

## Privacy and persistence

Patterns project JSON string values using keyed placeholders scoped to the admission-time conversation and upstream/client-authentication identity. Captures freeze that namespace, so later regrouping does not rewrite historical tokens; new captures do not inherit legacy credential-wide scopes. Cleanup keeps mappings referenced by surviving records, including the exact permitted token aliases copied with a tool result. Policy is frozen at admission. Discarding originals disables reconstruction; explicit reveal requires retained mappings and a current opt-in.

SQLite stores a whole-metadata JSON snapshot row, with separate body files and in-memory query indices. Ordinary updates use an approximately 200 ms flush cycle; new replay registration is synchronously committed before dispatch, with rollback and no execution on save failure. Saved pending/running traffic restores as interrupted. Registered replay keys remain effective, without promising exactly-once completion; a crash before dispatch may leave a registered action that never executed.

Backup includes metadata and body files. Capture paths are absolute and root checked. Cleanup protects active records, preserves missing-parent references and deletes owned files/reveal mappings.

## Provider and integration boundaries

- Fallback occurs only at transport failure or 502/503/504 before client output, and only to explicit compatible entries.
- Conversion handles independent text/function turns; hidden server history, saved/background Responses, multimodal/built-in tools and unrepresentable reasoning require native passthrough.
- WebSocket is Responses observation with a provider pinned to the first create request, FIFO association per lane, 16 outstanding calls and 32 named lanes.
- Provider history reads a saved Response by ID and creates no model execution or automatic history import.
- External tool tracing is a validated reporting interface. The gateway is not a complete Agent/MCP workflow executor.

## Verification performed

Full Go race tests and vet, Vue typecheck/build, mock protocol tests, 25 browser functional assertions, five viewport checks, thirteen restart assertions, history/file cleanup, and small real Chat Completions JSON/SSE relay checks using `glm-5.3-flash`.

Credentials were ephemeral and excluded from fixtures/reports/configuration. Mock WebSocket/history/conversion verification is not evidence that the real relay supports those capabilities.

## Capacity limits

Completed captures are file-backed, but full JSON responses and accumulated streaming text still use memory while processing. Metadata snapshots and queries are not optimized for high-volume archival. Incremental SQL storage, new SDK adapters, broader protocols or an Agent executor should be specified as separate future work.
