# Development documentation

The versioned contracts below describe the current `refactor/modular-structure` implementation. Local assistant settings, runtime data and developer journals stay outside the shared deliverables.

## Implemented contracts

- [Conversation correlation and graph](contracts/conversation-graph.md)
- [HTTP interception and full request captures](contracts/request-interception.md)
- [Safe cURL export and single-model-request replay](contracts/request-replay.md)
- [Complete response captures and trustworthy metrics](contracts/response-metrics.md)
- [Context comparison and protocol-aware rules](contracts/context-rules.md)
- [Privacy, persistent history and tool tracing](contracts/privacy-history-tools.md)
- [Providers, conversion, Responses WebSocket and saved history](contracts/providers-transports.md)
- [Frontend layout and regression checks](contracts/layout-guidelines.md)

## Delivery and evidence

- [Foundation decisions, M2–M8](plans/foundation.md)
- [M2 delivered acceptance record](plans/m2-response-metrics/prd.md)
- [Foundation verification, scripts and screenshots](../output/playwright/foundation/verification.md)

See the repository [roadmap](../IMPLEMENTATION_ROADMAP.md), [feature checklist](../FEATURE_CHECKLIST.md) and [handoff](../HANDOFF.md) for current status. Earlier reports under `output/playwright/` remain historical verification evidence; they do not override current contracts.
