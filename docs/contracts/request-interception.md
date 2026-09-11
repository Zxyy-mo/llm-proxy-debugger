# Request interception contract

## Flow and ownership

The proxy captures the complete client request separately from the display-limited log. Enabled rules match the original body and the default-upstream-prefixed path, descending by `priority` and preserving saved order for ties. Protocol-aware instruction injections compose before interception; the first matching intercept rule supplies the wait policy. A server-owned manager waits on a timer, the original HTTP context, and one decision channel. Browser availability never owns forwarding. Notifications are best-effort; HTTP snapshots are authoritative.

`Rule` retains its existing fields and adds `disabled` (default false), `wait_seconds` (default 30, range 1–3600), and `timeout_action` (`forward`, default, or `cancel`). Changing/deleting a rule affects future requests only. GET/POST `/api/rules` and PUT/DELETE `/api/rules/{id}` support rule lifecycle. Empty paths, invalid JSON, invalid waits and policies return JSON 400; unknown IDs return 404; unsupported methods return 405 with Allow.

## Pending API

- `GET /api/interceptions`: `{server_time, requests: []}`; pending summaries only, ordered by arrival.
- `GET /api/interceptions/{trace_id}`: `{server_time, request}` including full `body`, editable `headers`, `original_body`, and `original_headers`. Terminal decisions remain inspectable for the process lifetime.
- `PATCH /api/interceptions/{trace_id}` saves a validated draft without extending its deadline.
- `POST /api/interceptions/{trace_id}/validate` validates without saving.
- `POST /api/interceptions/{trace_id}/release` atomically applies optional edits and releases exactly once.
- `POST /api/interceptions/{trace_id}/cancel` cancels without forwarding.

Mutations accept `{revision, body?, headers?}`. `body` replaces the complete body. `headers` replaces the editable header subset only; all other original headers, including credentials, remain untouched. Omitted edits preserve bytes. The revision must be positive and match the active snapshot. Admin JSON bodies are limited to 16 MiB. Unknown control fields, malformed/trailing JSON: 400; too large: 413; invalid edits: 422; unknown trace: 404; stale revision, expired deadline, or terminal state: 409. Failed edits do not modify the request or reset the deadline.

Metadata fields are `rule_id`, `state` (`pending`, `released`, `canceled`), `reason` (`manual`, `timeout`, `client_disconnected`), `revision`, `started_at`, `deadline`, `resolved_at`, `timeout_action`, `modified`, and `wait_duration_ms`. Times use RFC3339Nano. Summaries add `trace_id`, `method`, `path`, and `model`. Browser countdowns account for `server_time`; the server rechecks time, revision and client cancellation at every commit, including after validation.

Timeout forwarding sends the latest saved candidate. Unsaved browser text is never used. Timeout cancellation returns HTTP 504 to the original caller; manual cancellation returns 409. A disconnected caller is logged with status 499 and is never intentionally forwarded. The original context also remains attached to the upstream request to close the final release/disconnect race. A terminal decision cannot be reversed.

## Editing and validation

Editable headers: `Content-Type`, `Accept`, `Anthropic-Version`, `Anthropic-Beta`, and `OpenAI-Beta` (case-insensitive). Reject unknown/duplicate header names, line breaks/control characters, and non-JSON Content-Type for edited JSON payloads. Host, routing, credentials, cookies, session headers and framing headers are not editable. The proxy recalculates body length and removes stale transfer/content length information after edits.

Edited bodies must be a JSON object. Supported LLM endpoints validate model, messages/input, stream, content shapes and local tool call/result pairing for Chat Completions, Responses and Anthropic Messages. Unknown provider fields are preserved. Stateful Responses may reference server-side tool calls through an unchanged previous_response_id/conversation; local validation cannot prove those remote pairings. This is structural validation, not a replacement for provider schema validation. Unedited original traffic can still be forwarded as-is.

Compressed or non-UTF-8 requests expose `edit_blocked_reason` and `body_encoding: "base64"` in the detail; body/original_body contain complete base64 data. They can be released unchanged or canceled, and must bypass JSON injection/editing. Never send replacement characters or plain JSON while retaining a compressed Content-Encoding.

Session/conversation/thread identifiers and explicit parent references are immutable while editing one captured request. This keeps recorded identity and parent references stable. Message/context edits recompute inferred history edges and the final transcript index from the effective outgoing body; never index original text paired with a modified response.

`metadata.run_id` 的有效值、非法值及重复/冲突证据同样不可编辑；`X-Run-ID` 不属于允许编辑的头。任务归属固定于入站，具体规则见 [四层调用契约](call-layers.md)。

## Capture and event contract

`GET /api/requests/{trace_id}` returns `{trace_id, original, outgoing?}`. Each snapshot has `method`, `url`, `headers`, `body`, and `content_length`; complete bodies are independent of `-maxbody`. `outgoing` is recorded at the proxy transport boundary and is absent for requests never forwarded. Captured headers use the existing redacted allowlist plus the editable headers. Raw credentials stay on the original HTTP request and never enter these API payloads.

Compressed/non-UTF-8 snapshot bodies are base64 encoded with `body_encoding: "base64"`; the optional Content-Encoding header remains visible. Text request snapshots omit body_encoding. The browser must label encoded bodies rather than treating them as editable JSON.

`RequestLog.status` adds `pending` and `canceled`. `interception` carries the metadata above. `wait_duration_ms` and `upstream_duration_ms` separate human/rule waiting from the elapsed upstream operation (not TTFT). Total duration still includes capture/processing overhead. `request_updated` contains `{event, trace_id, log}` for intermediate lifecycle changes. `interceptions_updated` invalidates pending snapshots. Existing request_start/end and sessions_updated events remain compatible and log revisions remain authoritative. Graphs and inspectors must render pending/canceled states explicitly.

Full request files, rule configuration and interception metadata persist by default. Live decision channels, client sockets and editor drafts do not resume across process restart: restored active records become HTTP 503 errors with `interception.reason:gateway_restarted`, and no traffic is automatically sent. Historical metadata is available through history/requests APIs; the old live interception manager entry is not recreated.

Recording privacy projects pending content and disables raw body editing; unchanged release/cancel remain available. Provider selection and privacy policy are fixed at admission. See [context/rules](context-rules.md), [privacy/history](privacy-history-tools.md) and [provider routing](providers-transports.md). WebSocket frame interception remains outside this contract.

## Required verification

- Good: create a timed rule, capture a body larger than maxbody, edit it and an allowed header, inspect the diff, save, then release. The upstream receives one exact edited request with original authentication and correct length; both complete snapshots remain available.
- Base: no rule forwards unchanged; an unedited pending request forwards at timeout using its original candidate. Rule injection is visible before release.
- Bad: invalid JSON/schema/tool pairing/headers leaves the pending version intact. Stale edits, duplicate release/cancel, and edits after deadline cannot forward twice. Manual cancellation, timeout cancellation and client disconnect never hit the upstream.
- Concurrent requests resolve independently under `go test -race`; cancellation during validation is rechecked before committing.
- Browser verification covers countdown, validation errors, save/release/cancel, original/outgoing comparison, reconnect, edits preserved across refresh events, and small/short viewport scrolling.
