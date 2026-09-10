# Conversation graph verification

Verified on 2026-09-09 in the local goproxy workspace.

## Automated checks

- `go test -race ./...`: passed, including correlation, protocol-visible history, proxy forwarding, arbitrary SSE chunks, missing/late parents, ambiguous IDs, cycles, response retrieval/cancellation, generic SSE and concurrent snapshots.
- `go vet ./...`: passed.
- `npm run build`: passed with TypeScript checking; graph code is split into a separate bundle. The existing Browserslist data-age notice remains informational.
- `git diff --check` and `gofmt -l internal cmd`: clean.

## Browser checks

- Keyboard node selection, mouse selection and corresponding log inspection.
- Canvas zoom, pan, fit, relayout controls and focusing the selected node.
- Current-session and global graphs, inferred edges, error nodes and missing-parent references.
- JSON graph export checked for 2 nodes and 1 inferred edge; compact payload excludes headers and full request bodies.
- Browser reload restores the selected session and trace.
- Proxy restart triggers reconnection and replaces the old mock snapshot with the new live server state.
- 390 × 844 viewport: no horizontal page overflow; readable focused node; inspector drawer fits at x=0, width=390, height=844; Escape closes it.
- One expected WebSocket handshake error occurred while the test proxy was deliberately stopped; no application exception was observed.

## Live endpoint checks

Used the endpoint explicitly supplied by the user. The API key was supplied to the test process through its environment and is absent from source and artifacts.

- `glm-5.3-flash` Chat Completions SSE: 200, response ID and authoritative input/output usage captured.
- `glm-5.3-flash` Chat Completions JSON: 200, replayed history linked to the streamed root.
- `Xiaopu-Ai` Chat Completions SSE: 200, a model switch still linked the replayed history to the same root.
- Responses API root and `previous_response_id` follow-up: both 200, exact parent ID association verified.
- Two non-streaming requests returned empty upstream 502 responses. Both remained correctly correlated error nodes; subsequent validation succeeded.

Live summaries are in `live-results.json`. Screenshots include `desktop-selected.png`, `all-sessions.png`, `mobile-focused.png`, `mobile-detail.png`, `live-cross-model.png` and `live-responses-chain.png`.

Session/graph data remains in memory. A gateway process restart clears it; browser reloads and WebSocket reconnects restore the current process snapshot.
