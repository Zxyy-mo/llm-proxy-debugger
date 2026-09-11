# Complete response and metrics contract

Implemented on `refactor/modular-structure` (M2, extended by M4/M7/M8).

## Captures and API

The recorder saves complete response bodies independently of `-maxbody`. Request/response log text and live previews remain bounded. Body files stay under the configured capture root; API callers cannot select arbitrary file paths.

`GET /api/responses/{trace}` returns a `ResponseSnapshot`. `GET /api/responses/{trace}/download` streams the stored bytes with an attachment filename. Optional `variant=client` (default) selects what the gateway returned; `variant=upstream` selects the separate pre-conversion upstream capture when available. An unknown variant is 400; a missing trace/variant is 404.

| Field | Meaning |
| --- | --- |
| `type` | `json`, `sse`, `binary` or `websocket` |
| `content_type` / `content_encoding` | Observed representation facts |
| `decoded` | gzip was decoded for storage; forwarding bytes are unchanged |
| `status_code` | HTTP status, or 101 for a WebSocket observation |
| `bytes` | Stored representation size; JSONL envelope bytes for WebSocket |
| `stored` | `file` or `missing` |
| `receiving` / `complete` | Capture lifecycle and successful completion |
| `reason` | Missing, failed or incomplete capture explanation |
| `body` / `body_encoding` | Detail content; `base64` for non-text/encoded bytes |
| `truncated` | Only the returned preview was limited |
| `offset` / `end` | Inclusive start and exclusive end byte offsets of the returned part |
| `next_offset` | Exact continuation offset, or `null` at the observed end |
| `last_offset` | Bounded final-part start for the selected limit |
| `redacted` / `representation` | Projection, conversion or frame-envelope representation |
| `observation_warning` | Forwarding may be complete while parsed observation is incomplete |

Without `offset` or `limit` the detail API retains its complete-body behavior. `offset` selects an exact nonnegative byte offset; `limit=1…16777216` bounds the part. Supplying only `offset` defaults to 256 KiB. Invalid, repeated or overflowing pagination parameters return 400; an offset beyond the observed file end returns 416, while an offset exactly at EOF returns an empty part. Downloads are unavailable while receiving (409) or missing (404).

Text parts end at a complete UTF-8 code point. A limit smaller than one code point may exceed the requested limit by at most three bytes to make progress. Follow `next_offset`, not the requested limit, to reconstruct text without gaps. Invalid/binary/encoded data, including an arbitrary offset inside a code point, is returned as lossless Base64 instead of trimming bytes or inserting replacement characters. `last_offset` uses a bounded local read to align text near EOF.

The UI reads and renders one 256 KiB part at a time, with previous/next, return-to-start and final-part controls and a visible byte range. Refresh keeps the selected part. Source and replay panels navigate independently. Partial JSON is displayed as original text; complete JSON formatting preserves number literals and has a bounded expansion budget. Exact full downloads are unchanged.

Native JSON and SSE downloads preserve the saved complete bytes. Gzip JSON is saved decoded and labelled. Other encodings/binary data use Base64 in the API and raw bytes in downloads. A storage error reports a missing capture without breaking client forwarding.

With recording privacy enabled, JSON is projected before storage. SSE/Responses WebSocket store the final redacted text/thinking projection instead of the original event stream. This is labelled `redacted-output`. Conversion stores `converted-from-openai` and a separate `upstream-original` capture; privacy intentionally omits the raw upstream variant.

## Metrics

`token_sources` contains independent `input`, `output` and `thinking` values:

- `usage`: the provider supplied the counter, including an explicit zero.
- `estimated`: character-based observation, not tokenizer output.
- `unknown`: no authoritative or usable observation, failure, cancellation, or parsing limit.

Authoritative usage replaces estimates. Later content cannot add estimated counts to an already authoritative counter. Logs, graph nodes, live metrics and UI use the same source semantics. Chat Completions content observation uses the first choice.

`ttfb_ms` measures arrival of the selected upstream response headers. `ttfc_ms` measures the first observed nonempty text, returned thinking or tool-call content. Both start at the actual upstream attempt, after interception; with explicit failover they include elapsed prior attempts. JSON conversion must record header arrival before buffering/conversion.

Unavailable, failed/canceled, unrecognized or empty content omits these timing fields. They do not estimate internal provider queueing or pure model computation. `wait_duration_ms`, `upstream_duration_ms` and total `duration_ms` remain separate.

## Limits and incomplete streams

A recognized native SSE stream without its protocol completion marker is an error. The parser has a 2 MiB observation buffer/event limit. On overflow it stops parsing further fragments; forwarding and raw capture continue, while Token sources/timing become unknown and `observation_warning` explains incomplete text/tool observation. Such a transcript cannot become a completed history-match anchor.

For a fully transferred raw response, `complete` may remain true even though observation is limited. A redacted projection based on partial parsing is marked incomplete. An adapter that cannot parse/convert an oversized or unsupported event fails conversion explicitly.

Completed bodies are file-backed, but JSON buffering and accumulated streaming text still use memory. This contract is not a constant-memory processing guarantee.

## Verification

`internal/proxy/responses_test.go` covers full JSON/SSE/binary/gzip bytes, downloads, missing files, UTF-8 previews, wait exclusion and parser overflow. `internal/protocol/metrics_test.go` covers source/zero/final-usage behavior. Conversion timing/variants are tested in `providers_test.go`. Browser and real relay evidence: [foundation verification](../../output/playwright/foundation/verification.md).
