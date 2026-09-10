# Request interception verification

Date: 2026-09-09

## Delivered scope

Completed the three P0 items from FEATURE_CHECKLIST.md: server-owned request waiting/decisions, browser editing/validation/diff/release/cancel, and complete original/outgoing request captures. Rules can also be edited, disabled and deleted. Prior conversation correlation, graph and layout work remains present and uncommitted.

## Automated verification

- `go test -race ./...` passed; affected proxy/interception packages were rerun with race detection after the final encoded-body and validation changes.
- `go vet ./...` passed.
- `cd web && npm run build` passed (Vue TypeScript checking and production build).
- Existing JSON/SSE forwarding and correlation regressions remain green.
- New tests cover saved candidates, unchanged deadlines, competing decisions, disconnects, stale revisions, invalid JSON/headers/tool pairs, rule defaults/CRUD, immutable snapshots, full captures beyond maxbody, gzip bytes, and outgoing transcript history indexing.

## Browser verification

Used an isolated local proxy at 12338, Vite at 5174 and a synthetic upstream at 28001. No external model service was called. Structured evidence is in [results.json](results.json); reusable CLI snippets and the synthetic upstream are in this directory.

- Created a timed rule through the UI; edited, enabled/disabled and deleted it through the UI.
- Captured a 2036-byte request with a 128-byte display limit; the editor and audit retained the tail marker.
- Rejected invalid JSON with HTTP 422 without changing the saved revision. This intentional 422 is the expected browser network error, not an application exception.
- Edited the body and Accept header, validated, saved, switched views, refreshed, and reloaded. The saved body recovered; local unsaved text survived snapshot/view changes.
- Released exactly once; verified the upstream body, Content-Length and original authentication. Original/outgoing capture APIs did not expose the raw credential.
- Four-second timeout forward returned 200; timeout cancellation returned 504 and produced no upstream call.
- Manual cancellation returned 409 with no outgoing capture.
- Another editor's saved revision preserved the local draft, disabled stale submission, and allowed explicitly loading the latest version.
- Closing a real HTTP client's browser context canceled the waiting request with logged status 499, without forwarding.
- Disabling a rule affected subsequent traffic while a previously intercepted request retained its original policy.
- Timed browser suite reported zero application page errors.

## Layout verification

Checked editor and action reachability with three simultaneous pending requests. The same seven viewports also passed rule-form Save hit-target checks. No document-level horizontal overflow was observed.

| Viewport | Pending requests | Usable editor height | Release and rule Save |
| --- | --- | --- | --- |
| 1600 × 1000 | 3 | 607 px | Pass |
| 1366 × 768 | 3 | 422 px | Pass |
| 1024 × 768 | 3 | 422 px | Pass |
| 768 × 600 | 3 | 242 px | Pass |
| 390 × 844 | 3 | 402 px | Pass |
| 320 × 640 | 3 | 222 px | Pass |
| 844 × 390 | 3 | 119 px | Pass |

A visual check caught a short-landscape problem that button-only checks missed: the queue squeezed the editor. The final layout uses a side list in short landscape, a compact selector in portrait, omits duplicate request navigation, and keeps a scrollable minimum editor height. The final measurements above include this fix.

Screenshots: [desktop editor](desktop-editor.png), [mobile editor](mobile-editor.png), [landscape queue](landscape-queue.png), [original/outgoing audit](desktop-audit.png), [revision conflict](revision-conflict.png), [mobile rules](mobile-rules.png), [graph states](final-graph.png).

## Cross-layer and finish review

- Traced original capture → rule candidate → pending manager → validated decision → transport capture → completed log/graph → browser snapshots.
- API error codes, revision semantics, new pending/canceled statuses, editable headers, timing fields, encoded bodies and client types are documented in the executable Trellis contract.
- History inference is recomputed from outgoing content after edits; canceled requests never seed the successful history index.
- Snapshot maps/metadata are copied; credentials remain on the original HTTP request. Event publication cannot block deadlines.
- Shared display helpers, import paths and all consumers of lifecycle statuses were reviewed. No frontend dependencies were added for interception or diff.

## Handoff and limits

- Updated FEATURE_CHECKLIST.md and both READMEs. Code remains uncommitted.
- Restored the local UI at http://127.0.0.1:5173 and proxy/API at http://127.0.0.1:12337; the default upstream remains http://127.0.0.1:28000. UI and API smoke checks returned 200.
- Synthetic pending requests were drained and the synthetic interception rule was removed. The three QA listeners were stopped; only the default local services remain running.
- Storage is process-local. Restarting the gateway clears rules, conversations, pending state and full captures.
- Editing supports UTF-8 JSON and the documented header allowlist. Compressed/binary content can be inspected as base64 and released unchanged or canceled.
- Validation checks protocol structure and local tool pairings; remote Responses context is still validated by the provider. TTFT, replay/cURL, persistence, redaction and the later roadmap items remain separate work.
