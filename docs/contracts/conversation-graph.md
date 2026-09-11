# Conversation graph contract

Correlation is computed server-side, before request/response log truncation. `internal/correlation` normalizes protocol-visible messages and hashes them; `internal/store` owns identifiers, parent links and graph projection under its mutex. Frontend code must render server-provided edges, not infer causality from timestamps.

`trace_id` 继续表示一次 Request。日志及请求图节点增加独立 `run_id`/`run`，会话/父子边仍使用本契约；Run 归属不创建新因果边，任务筛选也不裁掉会话图中的其他节点。提取、歧义、作用域与新查询 API 见 [四层调用契约](call-layers.md)。

## API and lifecycle

`GET /api/graph?session_id=<encoded ID>` returns `{revision, session_id, nodes, edges}` with non-null arrays. Unknown session: 404 JSON error. Other methods: 405 JSON error and `Allow: GET`. Unfiltered requests return the whole captured graph.

`RequestLog.correlation` contains `session_source`, optional `response_id`, `previous_response_id`, `conversation_id`, `thread_id`, `parent_trace_id`, `parent_reference`, `link_source`, `confidence`, and `warning`. `RequestLog.status` is `pending`, `running`, `done`, `error`, or `canceled`; timestamps use RFC3339Nano; `revision` is monotonically increasing for authoritative log changes. See `request-interception.md` for pending decisions, intermediate `request_updated` events and separated wait/upstream timing.

`Store.UpdateInput(traceID, input)` recomputes inferred history after rule/editor body changes and indexes the outgoing transcript on completion. Explicit identities and parent references are immutable during editing. A changed prefix must lose its old inferred edge before rematching; canceled requests never populate the successful history index.

`request_start.log` provides initial metadata while retaining the existing top-level event fields. `request_end.log` is authoritative. `sessions_updated` invalidates cached session and graph snapshots when late identifiers repair existing records. Never send mutable accumulator pointers to the WebSocket hub.

## Validation and evidence

Explicit session identifiers have priority over history. Scope implicit identifiers/history by upstream and credential hash; never serialize credentials used for indexing. Exact links use `parent_trace_id` or `previous_response_id`. History edges require a unique, completed, successful prior input-plus-output prefix including a user message; shared prompts alone never qualify. Ambiguous, conflicting or cyclic links remain unlinked with a warning. Session membership alone is not a parent relation.

Graph nodes have kind `request`, `reference` or `tool`. Missing references stay visible and may resolve later. Group movement preserves each trace exactly once, including descendants. Sessions, indices, captures and tool metadata persist in SQLite/file storage by default; restored active requests become interrupted errors and are never automatically resent. Deleting a parent preserves the child's reference with `warning:deleted_parent`.

Generating POSTs to `/responses`, `/chat/completions` and `/messages`, plus observed Responses WebSocket `response.create` calls, own response IDs. GET polling, provider-history retrieval and POST cancellation can expose the same ID without becoming another owner. A WebSocket connection or `stream_id` alone is not parent evidence. Generic SSE without recognized LLM events is not classified as a failed LLM stream. A parser-limited transcript is not a completed history anchor. Completed records release their per-message fingerprints after adding the final history index entry.

Tool nodes retain API-observation or externally reported Span provenance. Their execution duration is unknown unless supplied by a real Span. Graph token fields include per-counter sources and optional upstream timing; see [response metrics](response-metrics.md) and [tool/history semantics](privacy-history-tools.md). Canonical fingerprints preserve large JSON integers instead of rounding through float64.

Browser selection is persisted locally; reconnect refreshes authoritative snapshots, removes old server state, and uses log revisions to avoid overwriting newer events. The graph fetch is cancellable on session changes. User viewport changes survive metric-only updates. In full-graph mode the canvas reframes on node topology and container-size changes; manual zoom/pan and selected-node focus are separate modes. See the frontend layout guidelines for clipping regression checks.

## Required verification

- Good: a captured parent with two explicit response children produces two edges; replayed history produces an inferred edge.
- Base: one request without identifiers stays isolated; explicit session-only siblings have no invented edge.
- Bad: invalid JSON, missing parents, duplicate response IDs, ambiguous histories, cross-credential histories and cycles cannot create false links.
- Test JSON/SSE metadata before display truncation, tool-output history, arbitrary SSE chunk boundaries, late parent reconciliation, HTTP byte preservation and concurrent snapshots with `go test -race ./...`.
- Build Vue types and assets and exercise graph selection, fitting/zoom, reload and reconnect against a local mock upstream.
