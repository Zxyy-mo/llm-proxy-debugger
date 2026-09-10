# Context comparison and rule contract

Implemented in M3, alongside the existing HTTP interception contract.

## Context comparison

`GET /api/context-diff/{trace}?base={other_trace}` compares captured request contexts. If `base` is omitted, use the authoritative captured parent; never substitute the most recent timestamp or another member of the same session. No captured parent returns 409; self-comparison returns 400; missing records/files return 404.

Each side independently chooses its outgoing snapshot when available, otherwise original. Recording privacy projection applies before comparison. The response identifies `trace_id`, `base_trace_id`, `selected_by` (`parent` or `manual`), `source` and `base_source`.

`diff` contains:

- `messages` with `kind` (`added/removed/unchanged`), the applicable before/after index, and full normalized message JSON.
- `system` and `tools` with `changed`, `before` and `after`.
- `added`, `removed`, `unchanged` totals.
- `limited` when full alignment exceeded the matrix bound.
- `remote_context` when either request references provider-side context.

Parsing preserves JSON numbers using `json.Number`. Text strings and the supported text content-block forms are normalized. Message alignment uses an exact LCS; array insertion does not turn the remaining history into modifications. Above 1,000,000 alignment cells, compare matching prefixes/suffixes and mark the unaligned interval limited. Bodies over 16 MiB return 413; compressed/non-text bodies return 422.

The diff shows observed additions/deletions; it does not claim that a deletion was a model compression, summarize unseen remote history, or modify either request. Provider history retrieval is an independent explicit read.

## Rules and injection

`priority` is an integer, default 0. Higher values execute first; equal priorities retain saved list order. Enabled rules match `path_match` and optional `body_match` substrings against the original request. The existing default-target path-prefix matching behavior is retained. Provider route selection is a separate model rule.

All matching prompt injections compose in priority order. The first matching interception rule fixes the wait policy. Settings changes affect new requests, not an already pending decision.

| Protocol endpoint | Instruction injection |
| --- | --- |
| Anthropic `/messages` | Append to top-level `system` text or content blocks |
| Chat `/chat/completions` | Append to the first `system` or `developer` message; insert a system message when absent |
| Responses `/responses` | Append to `instructions` text |

Unrelated JSON values retain integer precision. Unsupported instruction shapes, non-JSON and compressed/binary bodies are left intact, not rewritten into invalid protocol shapes.

`GET/POST /api/rules` and `PUT/DELETE /api/rules/{id}` manage saved rules. DELETE succeeds with 204. Rules persist with metadata; a fresh store retains the original default concise-response rule for `/messages`, which can be edited or disabled.

The request pipeline is: choose provider from the incoming model; apply matching injections; wait/edit if intercepted; apply captured outbound privacy; perform the selected alias/conversion; capture and send. Editing the model while pending does not reselect the original route. Outgoing-snapshot replay skips rules and repeated conversion/aliasing; original-snapshot replay follows the current normal pipeline.

See [request interception](request-interception.md) for edit validation, immutable identifiers, deadlines and version conflicts.

## Verification

`internal/contextdiff` tests alignment, normalization, limits and numeric precision. `internal/proxy/injection_test.go` covers protocol placement and ordering. Browser verification exercises a real captured parent/child, changed instructions and the remote-context notice.
