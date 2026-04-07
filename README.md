# LLM Debug Gateway (goproxy)

一个面向开发者的 LLM 调试型反向代理：转发 HTTP/SSE/WebSocket 流量，实时解析 SSE 增量并统计指标，通过 WebSocket 广播到前端，同时支持按会话归档与运行时 Prompt 注入。

## 功能

- 反向代理：普通 HTTP + SSE（`text/event-stream`）+ WebSocket 透传
- 会话归档：通过 `X-Session-ID` 将请求归档到内存 Session（`/api/sessions` 可查询）
- 实时广播：SSE 增量与聚合指标通过 `/api/ws` 广播（便于做 WebUI）
- 动态规则：按路径/Body 关键字匹配，注入请求 JSON 的 `system` 字段（`/api/rules` 管理）
- 日志与脱敏：结构化日志（zap + lumberjack），`Authorization`/`X-API-Key` 自动脱敏

## 快速开始

要求：Go 版本以 `go.mod` 为准（当前为 `go 1.24.0`）。

运行：

```bash
go run . \
  -listen 0.0.0.0:12337 \
  -target http://127.0.0.1:8080 \
  -logdir log \
  -maxbody 10240
```

把你的客户端请求指向本服务的监听地址即可（可选传 `X-Session-ID` 做会话隔离）。

## API

### WebSocket: `/api/ws`

连接地址：`ws://<listen>/api/ws`

广播事件（示例字段，便于前端消费）：

- `request_start`
```json
{
  "event": "request_start",
  "trace_id": "uuid",
  "session_id": "default",
  "method": "POST",
  "path": "/v1/messages",
  "time": "2026-01-02T15:04:05Z"
}
```

- `sse_delta`（每个 SSE event 的 `data` 聚合后派发一次，同时附带累计指标）
```json
{
  "event": "sse_delta",
  "trace_id": "uuid",
  "data": "{\"type\":\"content_block_delta\",...}",
  "sse_event": "",
  "sse_id": "",
  "sse_retry": 0,
  "sse_fields": {},
  "metrics": {
    "trace_id": "uuid",
    "input_tokens": 123,
    "output_tokens": 456,
    "thinking_tokens": 0,
    "tool_use_count": 1,
    "is_thinking_loop": false
  }
}
```

- `request_end`
```json
{
  "event": "request_end",
  "trace_id": "uuid",
  "log": { "...": "RequestLog" }
}
```

### Sessions: `/api/sessions`

获取当前内存中的所有会话与请求日志：

```bash
curl http://<listen>/api/sessions
```

### Rules: `/api/rules`

获取规则列表：

```bash
curl http://<listen>/api/rules
```

新增规则（示例：对路径包含 `/messages` 的请求注入 `system`）：

```bash
curl -X POST http://<listen>/api/rules \
  -H 'Content-Type: application/json' \
  -d '{
    "path_match": "/messages",
    "body_match": "",
    "inject_system": "[System] Be concise.",
    "intercept": false
  }'
```

说明：

- `path_match`：只做字符串包含匹配
- `body_match`：可选，只有请求 body 文本包含该子串才触发
- `inject_system`：对请求 JSON 的 `system` 字段注入内容（当前假设 body 可反序列化为 JSON object）
- `intercept`：预留字段（当前未实现拦截/确认流）

## SSE 解析与指标

- SSE 解析器为增量、逐行解析：兼容 `\r\n`、多行 `data:`、chunk 边界、注释行 `:`
- 协议指标目前内置 `AnthropicHandler`（基于 SSE `data` 中的 JSON `type` 字段做统计）
- OpenAI 协议解析目前是占位逻辑，后续可按 `ProtocolHandler` 接口扩展

## 注意事项

- 代理转发的 `http.Transport` 默认 `InsecureSkipVerify=true`（便于内网/自签证书调试），不建议直接用于生产环境
- 会话、规则都在内存中，进程重启即丢失；长时间运行需要自行加淘汰/持久化

## Roadmap

详见 `plan.md`：计划补齐 WebUI（React/Vite）对 `/api/ws` 与规则管理的可视化支持。

