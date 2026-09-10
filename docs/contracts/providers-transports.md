# Provider, conversion, WebSocket and history contract

Implemented in M7–M8. Native protocol support, gateway conversion and a provider's actual capabilities are distinct.

## Provider configuration and selection

`GET /api/providers` returns `{config,default_target,capabilities}`. `PUT /api/providers` validates, replaces and explicitly flushes `config`. Malformed JSON: 400; invalid configuration: 422; persistence failure after applying configuration: 500.

`config.providers` entries contain `id`, `name`, `base_url`, `protocol` (`passthrough` or `openai`), optional `key_env` / `auth_header` / `auth_scheme`, and `history` / `websocket` booleans. Up to 64 providers and 256 routes are allowed.

Base URLs must be HTTP(S) with no userinfo, query or fragment. Origins, mounts and standard `/v1` bases are supported. Joining preserves escaped path segments such as `%2F`; only a literal standard `/v1` prefix is deduplicated. TLS verification is enabled unless the operator explicitly uses `-insecure`.

`key_env` stores an environment-variable **name**, never the key value. When configured, the selected key replaces client authentication headers and credential query parameters are removed. Supported headers are Authorization (default Bearer scheme), X-API-Key and api-key. Empty `key_env` preserves client-supplied authentication. A configured but missing environment variable produces an explicit failure.

Routes contain `id`, `model`, `provider_id`, optional `target_model`, integer `priority`, `disabled` and up to three `failover` IDs. Match exact model or a terminal `*` prefix; higher priority wins, ties preserve list order. A miss uses `-target`. Selection is fixed at request admission.

Fallbacks must exist, be unique and use the same protocol mode. HTTP attempts another configured endpoint only after a transport error or 502/503/504 and before any response has been exposed to the client. No retry follows partial JSON/SSE output. There is no automatic load balancing or WebSocket failover.

Logs expose route ID, selected/actual provider, original/target model, conversion and attempts (provider, status/error, elapsed time). Outgoing captures represent the actual endpoint used. Outgoing replay is pinned to that captured provider/base URL, skips repeated alias/conversion/rules and rejects removed or changed destinations.

## Protocol capability matrix

| Capability | Native passthrough | `openai` conversion mode |
| --- | --- | --- |
| Chat Completions HTTP JSON/SSE | Forward and observe first-choice content | Pass through, optional model alias |
| Anthropic Messages | Provider's native API required | Independent text/function turns → Chat → Anthropic JSON/SSE |
| Responses | Provider's native API required | Independent text/function turns → Chat → Responses JSON/SSE |
| Provider-side previous_response_id/conversation | Passed to native provider | Rejected |
| Stored/background Responses | Provider-dependent | `store:true` / background rejected |
| Images/audio/files, built-in tools, signed reasoning | Provider-dependent native data | Unsupported semantics rejected |
| Responses WebSocket | Requires enabled/native provider capability | Not converted |
| Saved Response retrieval | Requires history capability | Conversion-generated IDs are not retrievable |

Conversion preserves supported user/assistant instructions, text, function definitions/choices, call arguments/results, stop reasons, usage and relevant token limits. Supported Responses text formats map to the corresponding Chat response format. Fields that cannot be represented are not silently dropped. Error-marked Anthropic tool results, nonempty annotated text, unrepresentable reasoning outputs and legacy Chat function outputs require native support.

Conversion errors before response output fail explicitly; errors during an SSE conversion terminate it as failed. Converter-generated response IDs are local observation IDs. `variant=client` and `variant=upstream` keep converted client output and original upstream output distinct. Recording privacy omits the raw pre-conversion variant.

## Responses WebSocket

Incoming WebSocket upgrades are bridged with Gorilla WebSocket, preserving authentication/subprotocols and verifying upstream TLS. Other WebSocket endpoints are forwarded without fabricated LLM records.

For `/responses`, the first `response.create` selects the provider from its model. The connection is pinned after dialing; a later request that would require another provider/protocol is rejected. The default target is allowed to establish a native connection; configured providers require `websocket:true` and `protocol:passthrough`.

Each valid create becomes a distinct trace with a captured request, actual outgoing payload, model/route, response metrics/tools and `websocket` metadata (`connection_id`, `stream_id`, `event_id`, frame/byte counts). Aliases and outbound policy may apply; HTTP breakpoint editors do not own these frames.

The bridge associates requests FIFO within `stream_id`, supports interleaving lanes and actual producer response IDs, and does not infer a parent merely from a shared connection/lane. An actual previous_response_id can correlate HTTP and WebSocket calls.

Bounds: at most 16 outstanding calls, 32 named lanes, a 128-byte lane name and a 64 MiB individual WebSocket message. Limits are explicit; truncated inputs are never forwarded as if complete.

Completion, failure, incomplete/cancel events and transport disconnects finalize affected calls. `response.cancel` is forwarded; provider cancellation evidence is recorded. Calls still active on disconnect are marked interrupted. The gateway does not replay unsent/unfinished frames after reconnect.

Captured text messages are stored as JSONL envelopes:

```json
{"type":"text","data":"<the exact original text payload, including its whitespace>"}
```

This preserves message payload boundaries and text, not TCP segmentation, masking or control-frame wire bytes. Privacy replaces the event stream with a final redacted output projection. Frame breakpoint editing and frame replay are unavailable; the single-request replay API is HTTP-only.

## Provider history

`POST /api/provider-history` accepts `{provider_id,response_id,api_key?}` and performs an explicit `GET /v1/responses/{id}` on the configured provider. The provider must have `history:true`. Response IDs are limited to 1–200 alphanumeric/underscore/hyphen characters.

Transient `api_key` overrides the configured environment key for this query using the configured header/scheme. Neither the key nor a synthetic model call is stored. Response: `{provider_id,response_id,status_code,body,source:"provider_history",redacted}`. Provider non-2xx status is reported in `status_code`; the returned record is not automatically imported or registered as a response producer.

Requests have a 30-second timeout, do not follow redirects and limit the history response to 16 MiB. Current recording privacy projects the returned body. Unsupported/missing capability: 422; invalid ID/input: 400; connection/read failure: 502.

This is retrieval by known saved Response ID, not account-wide history enumeration. Gateway conversion IDs are not provider storage IDs.

## Verification and references

`internal/provider`, `internal/adapter`, `internal/proxy/providers_test.go` and `websocket_test.go` cover routing, fallback, protocol semantics, credentials, URL joining, capture variants, lanes and cancellation.

[OpenAI WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode) and [Responses retrieval](https://developers.openai.com/api/reference/resources/responses/methods/retrieve.md) informed the supported integration. Tests use controlled providers; capability toggles do not prove that an arbitrary provider supports an API.

The real user-supplied `glm-5.3-flash` relay was verified for native Chat Completions JSON/SSE only. See [verification](../../output/playwright/foundation/verification.md).
