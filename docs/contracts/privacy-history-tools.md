# Privacy, local history and tool tracing contract

Implemented in M4–M6.

## Recording and outbound privacy

`GET/PUT /api/privacy` reads/replaces:

```json
{
  "record": false,
  "outbound": false,
  "retain_raw": true,
  "allow_reveal": false,
  "patterns": [{"name": "email", "expression": "(?i)[a-z0-9._%+\\-]+@[a-z0-9.\\-]+\\.[a-z]{2,}"}]
}
```

Defaults include email and mainland-China mobile-number patterns. Up to 32 unique patterns are supported; names are 1–32 safe identifier characters and expressions are 1–2048 characters. Invalid regular expressions or matches of empty text return 422. `retain_raw:false` requires `record:true`; reveal requires retained originals.

- `record` projects observable text and stored responses. It never changes response bytes returned to the original client.
- `outbound` substitutes strings in valid JSON request bodies before sending. Invalid/non-JSON bodies remain unchanged.
- `retain_raw` retains request originals and substitution mappings for reproduction/reveal. When disabled, only projected request data and permitted metadata remain. Recorded response files are still redacted even when originals are retained.
- `allow_reveal` is a current global opt-in for the explicit restore endpoint; it is not automatic outbound restoration.

Policy is captured when a request begins. Textual projection precedes preview truncation. JSON processing preserves number literals. Patterns inspect string values rather than JSON property names. This is configurable pattern matching, not automatic detection of every secret.

New captures use a keyed hash of their **admission-time conversation plus upstream/client credential identity**, pattern name and matched value. The same value stays consistent in already-associated calls within that conversation; unrelated conversations sharing credentials receive different placeholders. Scope allocation and full-body projection happen atomically after initial correlation and before preview truncation. Existing placeholders are protected from further regex rewriting. No raw authentication key is stored to produce these hashes.

Each capture keeps its privacy namespace for its lifetime. Later graph merges, detaches or response identifiers do not rewrite already-forwarded bytes or grant another namespace's reveal authority. Previously unrelated captures that are linked later can therefore retain different historical placeholders. Same-identity replay may retain its source's modern namespace when no explicit different conversation takes precedence. Older version 1 captures keep their legacy namespace for restoration; fresh captures never inherit that credential-wide namespace. Snapshot version 2 persists the frozen namespace and still reads version 1 data.

`POST /api/privacy/restore` accepts `{trace_id, body}`. It requires the trace's original-retention policy and the current reveal opt-in, and restores only available mappings in that trace's scope. Missing trace: 404; unretained originals: 409; reveal disabled: 403. The API returns text for operator inspection; it does not send another model request.

With recording privacy, live SSE/WS content and unfinished tool fragments are withheld, and the final redacted output projection is stored. The breakpoint UI supports unchanged release/cancel rather than displaying raw sensitive text. Exact replay is refused when a required original was discarded, unless a complete replacement body is provided.

Policy updates apply to new captures; they do not retroactively erase previously retained originals. Cleanup deletes relevant retained mappings as described below.

## Persistence and recovery

`-data` defaults to `<logdir>/gateway.db`; `-` selects memory-only metadata. SQLite uses WAL, FULL synchronous writes and a coalesced JSON metadata snapshot row. Body files remain separate, using capture-root-checked absolute paths.

Persisted metadata includes requests/sessions, correlation indices, rules, captured forwarding profiles without secrets, response representations, privacy policy/mappings, provider settings, replay records/keys and tool results/spans.

Ordinary changes trigger an approximately 200 ms coalesced flush; normal shutdown flushes. Abrupt termination can lose recent ordinary metadata not yet flushed. New replay registrations are a synchronous boundary: the record, fingerprint and idempotency key must be committed before execution starts. A failed registration returns 503 and sends no upstream request. Restoring a running/pending request produces `status:error`, HTTP 503 and an explicit restart interruption message, with unknown metrics. Pending interception reason becomes `gateway_restarted`. Saved running replays likewise become errors. Neither restore nor browsing automatically executes anything.

Persisted replay idempotency keys continue to suppress duplicate actions. The registration barrier prevents an accepted persistent replay from losing its key in the ordinary background-flush window. It does not guarantee exactly-once completion: a crash after registration but before dispatch can leave an interrupted action that was never sent, and a crash after sending can leave its final result unknown. Memory-only mode remains non-durable. Client/network libraries may also retry their own requests.

The store restores in-memory indices from the saved snapshot; history queries currently filter those indices/records. It is not a normalized high-volume SQL schema. Copy both metadata and body files for backup. Paths must still resolve under the capture root after migration.

## History API and cleanup

`GET /api/history` supports `q` (Trace/Response ID, summary, model, path and error), exact `model` / `status` / `session_id` filters, `limit=1…200` (default 50), and an offset-style `cursor`. Response: `{items,total,next_cursor,storage}`, newest first. This cursor is not a stable snapshot under concurrent inserts. `storage` includes persistence state, last save and any save error.

`GET /api/history/{trace}` returns the authoritative log. `DELETE /api/history/{trace}` removes one terminal record and its owned request/response files; active records return 409.

`POST /api/history/cleanup` accepts `{before?,session_id?,all?}`. At least a date/session filter or explicit `all:true` is required. `before` is RFC3339. Response: `{deleted,active_skipped}`. Running/pending requests are skipped.

Deleting a parent retains the surviving child's reference and sets `correlation.warning:deleted_parent`. Indices/empty sessions are pruned. Cleanup forgets a privacy namespace only when no surviving record references it. This protects same-session survivors, active records, moved captures and legacy data; deleting one conversation cannot erase another conversation's restoration data. Mappings have namespace-level lifetime, so deleting one trace does not erase individual mappings still owned by a surviving namespace. Cleanup flushes metadata before deleting owned files and checkpoints WAL. It does not delete arbitrary external paths or promise forensic erasure of filesystem backups/log rotation.

Replay provenance/idempotency records remain separate; deleting a model record does not cause a replay to execute again. Existing rotated structured logs are separate from indexed history cleanup.

## Structured tool observations

Model response observation records tool ID/call ID, name/kind, arguments, source, status, result trace and any supplied output/error. Partial arguments and truncated payloads are labelled. Tool outputs in later model requests attach only with matching scope/session and a unique owner, preferring explicit parent evidence. An API-observed call/result has unknown execution duration.

If late association copies a tool output from a different frozen privacy namespace, its owner retains only the known placeholder aliases literally referenced by that output, and only when both captures retain originals. The output bytes and result-trace provenance stay unchanged. Unrelated/unknown mappings are not imported; either no-retain policy prevents raw aliases. Deleting the result trace then cannot destroy the surviving owner's permitted restoration data.

Identity-alias cleanup selects a successor only when that record independently carries the same explicit identity evidence. Deleting the last actual identity owner removes the alias rather than transferring it to a movable inferred child. Outbound substitutions preserve original identity/reference evidence while updating outgoing transcript hashes. A second bounded hash-only pre-substitution anchor lets successfully completed calls match a client's original resubmitted history; edited, failed, canceled or remote-state-only input does not invent an anchor.

`POST /api/tool-spans` accepts an application-reported execution:

```json
{
  "trace_id": "captured-model-trace",
  "span_id": "execution-1",
  "call_id": "call-1",
  "name": "lookup",
  "kind": "mcp",
  "status": "done",
  "started_at": "2026-09-10T10:00:00Z",
  "ended_at": "2026-09-10T10:00:00.125Z",
  "input": "{\"query\":\"example\"}",
  "output": "found"
}
```

`kind` is `mcp/agent/tool`; `status` is `running/done/error`. Parent trace must exist. Span ID and name are required; Span ID ≤200 characters; input/output each ≤64 KiB; request ≤256 KiB. Timestamps are RFC3339Nano. Terminal spans require end ≥ start and calculate duration only from those supplied timestamps.

The same Span ID updates an execution; a finished span cannot become running (409). A new Span ID with an already-associated model call ID creates a separate execution rather than overwriting the previous one. A model call without a span may be enriched by its first matching report. At most 512 tool records per parent are accepted by this endpoint.

Tool records appear as independent graph nodes and in details; `source:trace` distinguishes reported execution from `source:api` observation. All captured records follow privacy policy and persist. This endpoint is an integration contract for instrumented applications, not an MCP executor or automatic SDK instrumentation.

Verification: `internal/privacy`, `internal/store/*_test.go`, privacy/proxy tests and [browser/restart evidence](../../output/playwright/foundation/verification.md).
