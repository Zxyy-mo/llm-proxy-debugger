# Operator investigation contract

The UI uses existing capture, history, context and replay APIs to support finding a failed call, inspecting its context, editing one request, evaluating the replay and returning to the investigation.

## Selection and navigation

`App.vue` owns the inspected trace separately from `auditSourceTraceId`. `InvestigationAction` is `{trace, action: context|replay|compare|source}`. Context/replay actions explicitly select an audit source. Inspecting a replay result does not silently replace its source editor. One workbench stays mounted; a deliberate source or original/outgoing change resets its draft credential fields. Browser storage contains selection/view identifiers only.

`activeRunId` 增加任务列表筛选，使用服务端 `run_id`，保留当前请求和完整会话图。保存的 `auditTrace:null` 保持“没有打开工作台”的含义，刷新不会偷偷选取当前 Trace 当草稿来源。任务栏只出现在画布/详情，重放工作台保留编辑空间。任务和尝试异步查询、错误恢复及视口验收见 [四层契约](call-layers.md)。

The finder filters the current session by keyword and status, with deterministic newest/oldest/duration ordering. Unknown or unfinished durations sort after known durations. Filtering never removes causal graph data or silently selects another trace. An out-of-filter selection is labelled. Explicit desktop-row/mobile selection sends a graph focus intent; live metrics do not. Subsequent manual camera movement and Show All keep their own modes.

Single-trace navigation uses `GET /api/history/{trace}` when needed and respects local revisions. Missing or failed reads are explicit rather than falling back to an unrelated latest call. History keeps its applied query/status/cursor and exposes a return action. Hidden views abort irrelevant loads without discarding the active investigation.

## Replay state

`ReplayOperation` separates draft data from `idle|submitting|unknown|accepted` invocation state. Its public state has `record`, `sourceTrace`, `source`, `busy` and `error`; it never exposes credentials or request bodies. A private immutable `ReplayRequest` and key are retained only while acceptance is unresolved. Draft reset cannot clear an invocation. Accepted running records can be found from history after reload and canceled without posting again.

| Boundary | Result |
| --- | --- |
| Definite initial validation/key conflict | Keep the draft editable; no automatic new model request |
| Initial registration save/shutdown rejection | Display 503; a definite rejection permits correction |
| Lost creation response | Keep the frozen attempt/key and block a new implicit action |
| Implicit same-key retry also fails, including 404/503 | `ReplayCreationUncertainError`; the earlier outcome remains unknown |
| Recover | GET existing records by `idempotency_key`; no POST |
| Explicit retry when recovery finds no record | POST the same frozen request/key, not the current edited draft |
| Cancel races with completion | Read the authoritative terminal record before claiming cancellation |
| Old poll/list arrives after completion | Never regress a terminal record to running |

An opened comparison is reconciled by replay ID and source/result identity against current/history data. It follows completion without changing the chosen pair or opening another comparison automatically. Navigation and reload never resume execution. The [durable backend contract](request-replay.md) remains authoritative.

## Evaluation and bounded text

`ResponseComparison` accepts source/result traces, optional logs/replay record and `active`; it emits `select(trace)`. Its independent reads use `fetchRequestLog(trace, signal)` and bounded response detail. Missing source metadata leaves the result visible. Status, errors, source/modification flags, total/wait/upstream duration, TTFB/TTFC and each token source are shown together.

Token deltas require terminal records with comparable provenance, model and Provider. Unknown is not zero; character estimates are never silently subtracted from usage counters. SSE/WebSocket text comes from existing bounded observations. JSON text extraction only reads known string fields from a complete client capture no larger than 64 KiB. Output and thinking excerpts are each at most 8,192 UTF-16 units without splitting surrogate pairs. Numeric/tool payloads are not reserialized as semantic output. Equal previews do not establish whole-response equality. Exact raw responses retain independent [pagination and downloads](response-metrics.md).

`formatBody` preserves raw numeric/string text. Input above 512K UTF-16 units, depth above 64, invalid input or generated formatting above 512K falls back to the exact original text. It never truncates a request to fit the editor.

`requestChanges` uses a typed lossless JSON tree. Numeric lexemes are compared literally; whitespace, object-key order and equivalent string escapes are ignored. Invalid/duplicate-key JSON uses labelled raw text comparison. Bounds: 512K input units per body, 64 levels, 20,000 parsed nodes, 12,000 comparisons, 200 changes, 4,096 units per displayed value, 512 per path and 64K total output. `complete`, `bodyMode`, `fallbackReason`, `limits` and clipping flags distinguish partial work from equality.

## Required checks

- Good: among 70 calls, filter a failed child, inspect its real parent/tool result, edit/replay, evaluate a successful result and return to the same draft/history page; one action sends one upstream request.
- Base: same-source navigation preserves drafts; known zero stays zero; response parts and comparison pair remain independent; explicit selection focuses a readable node.
- Bad: dropped creation responses, retry 503/404, reset/navigation/reload while running, missing source, initial load failure, oversized/deep/invalid JSON and differing token sources never create duplicate execution or false certainty.

Node suites cover invocation ordering/recovery, finder semantics, lossless diff, bounded formatting and comparable metrics. Browser scripts under `output/playwright/agent-debug-loop/` cover the complete operator journey, real request counts, seven viewports, manual camera stability and recovery states. Backend race/vet and the production frontend build remain required.
