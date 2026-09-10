# Complete responses and trustworthy metrics (M2)

Status: **implemented and verified, 2026-09-10**, as part of the current-branch foundation delivery. This replaces the earlier planning-only status.

## Delivered outcome

An operator can inspect/download complete JSON and SSE captures, compare a replay with its source response, distinguish provider Token usage from character estimates and unknown values, and see upstream first-response/first-content timing without interception wait time.

The selected storage strategy is file-backed complete bodies plus bounded log/UI previews. Metadata and file references persist through M5. Privacy and conversion explicitly identify changed representations; original forwarding remains faithful within native mode.

## Acceptance

- [x] JSON larger than `-maxbody` and complete SSE can be downloaded byte for byte.
- [x] Gzip JSON is saved decoded and labelled; preview truncation is UTF-8 safe.
- [x] Input/output/thinking counters carry `usage / estimated / unknown` across logs, graph and live events; explicit zero usage remains authoritative.
- [x] TTFB/TTFC start at actual upstream dispatch, excluding wait; unavailable/failed/canceled observations remain unknown.
- [x] Source-versus-replay response comparison is usable from the UI.
- [x] Go race/vet, production build and browser checks pass, including small/short viewports.
- [x] Versioned contracts, README, feature checklist, roadmap and verification report describe delivered behavior.

## Final decisions and extensions

- `GET /api/responses/{trace}` reads full or bounded detail; `/download` returns saved bytes.
- `variant=client/upstream` separates converted output from the original upstream response.
- `token_sources` is per counter. Character counts are explicitly estimates.
- `ttfb_ms` records header arrival even when JSON conversion later buffers the body.
- Exceeding the 2 MiB SSE observation limit discloses incomplete parsing and unknown metrics; native forwarding/raw capture continue.
- Recording privacy stores a redacted JSON/output projection and suppresses raw event storage.
- JSON buffering and streaming text accumulation still consume memory. File-backed completed storage does not imply constant-memory processing.

The authoritative implementation contract is [response-metrics.md](../../contracts/response-metrics.md). Tests are in `internal/proxy/responses_test.go`, `internal/proxy/providers_test.go` and `internal/protocol/metrics_test.go`. Browser, restart and real relay evidence is consolidated in [foundation verification](../../../output/playwright/foundation/verification.md).

The remaining M3–M8 capabilities are delivered under their own [contracts](../../README.md), rather than pending prerequisites for M2.
