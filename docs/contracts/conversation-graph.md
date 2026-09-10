# Conversation graph contract

Correlation is computed server-side, before request/response log truncation. `internal/correlation` normalizes protocol-visible messages and hashes them; `internal/store` owns identifiers, parent links and graph projection under its mutex. Frontend code must render server-provided edges, not infer causality from timestamps.

## API and lifecycle

`GET /api/graph?session_id=<encoded ID>` returns `{revision, session_id, nodes, edges}` with non-null arrays. Unknown session: 404 JSON error. Other methods: 405 JSON error and `Allow: GET`. Unfiltered requests return the whole captured graph.

`RequestLog.correlation` contains `session_source`, optional `response_id`, `previous_response_id`, `conversation_id`, `thread_id`, `parent_trace_id`, `parent_reference`, `link_source`, `confidence`, and `warning`. `RequestLog.status` is `pending`, `running`, `done`, `error`, or `canceled`; timestamps use RFC3339Nano; `revision` is monotonically increasing for authoritative log changes. See `request-interception.md` for pending decisions, intermediate `request_updated` events and separated wait/upstream timing.

`Store.UpdateInput(traceID, input)` recomputes inferred history after rule/editor body changes and indexes the outgoing transcript on completion. Explicit identities and parent references are immutable during editing. A changed prefix must lose its old inferred edge before rematching; canceled requests never populate the successful history index.

`request_start.log` provides initial metadata while retaining the existing top-level event fields. `request_end.log` is authoritative. `sessions_updated` invalidates cached session and graph snapshots when late identifiers repair existing records. Never send mutable accumulator pointers to the WebSocket hub.

## Validation and evidence

Explicit session identifiers have priority over history. Scope implicit identifiers/history by upstream and credential hash; never serialize credentials used for indexing. Exact links use `parent_trace_id` or `previous_response_id`. History edges require a unique, completed, successful prior input-plus-output prefix including a user message; shared prompts alone never qualify. Ambiguous, conflicting or cyclic links remain unlinked with a warning. Session membership alone is not a parent relation.

Graph nodes represent captured requests or references to parents outside the visible capture. Missing references stay visible and may resolve later. Group movement must preserve each trace exactly once, including descendants. Sessions remain in memory and vanish on restart.

Only generating POSTs to `/responses`, `/chat/completions`, and `/messages` own response IDs. GET polling and POST cancellation can expose the same ID without becoming another owner. Generic SSE without recognized LLM events is not classified as a failed LLM stream. Completed records release their per-message fingerprints after adding the final history index entry.

Browser selection is persisted locally; reconnect refreshes authoritative snapshots, removes old server state, and uses log revisions to avoid overwriting newer events. The graph fetch is cancellable on session changes. User viewport changes survive metric-only updates. In full-graph mode the canvas reframes on node topology and container-size changes; manual zoom/pan and selected-node focus are separate modes. See the frontend layout guidelines for clipping regression checks.

## Required verification

- Good: a captured parent with two explicit response children produces two edges; replayed history produces an inferred edge.
- Base: one request without identifiers stays isolated; explicit session-only siblings have no invented edge.
- Bad: invalid JSON, missing parents, duplicate response IDs, ambiguous histories, cross-credential histories and cycles cannot create false links.
- Test JSON/SSE metadata before display truncation, tool-output history, arbitrary SSE chunk boundaries, late parent reconciliation, HTTP byte preservation and concurrent snapshots with `go test -race ./...`.
- Build Vue types and assets and exercise graph selection, fitting/zoom, reload and reconnect against a local mock upstream.
