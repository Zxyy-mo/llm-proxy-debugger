# LLM Debug Gateway

面向个人开发者的 LLM 调试网关：转发 HTTP / SSE / WebSocket 请求，自动归档会话，并在可交互画布中查看模型调用、分支和上下文关联。

功能状态见 [规划与实现清单](FEATURE_CHECKLIST.md)，下一阶段范围与依赖见 [实施路线](IMPLEMENTATION_ROADMAP.md)；接手开发请先读 [交接记录](HANDOFF.md)。

## 功能

- **自动关联会话**：识别请求头、请求 JSON、metadata 和 URL 中的会话/对话/线程 ID。
- **调用路径**：通过 `previous_response_id`、父 trace ID 或完整历史匹配关联请求，支持分支、缺失父调用占位和迟到补链。
- **交互画布**：当前会话/全部会话、拖动节点、平移、缩放、自动布局、定位选中调用、图谱 JSON 导出。
- **实时详情**：状态、模型、耗时、token、工具调用计数、关联依据、请求与响应，以及接口明确返回的思考内容。
- **协议支持**：OpenAI Chat Completions、OpenAI Responses、Anthropic Messages 的 JSON 与 SSE 响应。
- **调试工具**：动态 Prompt 注入规则、WebSocket 实时广播、结构化日志与鉴权头脱敏。
- **请求断点**：匹配规则后倒计时等待，编辑正文与允许的请求头，校验、保存、放行或取消；到期按策略处理。
- **出站对比**：保留完整客户端原文和实际出站请求，查看字段差异，并区分人工等待与上游耗时。
- **安全 cURL**：按“客户端原始 → 网关”或“实际出站 → 上游”导出可直接运行的命令，凭证以环境变量占位，大正文或二进制正文以文件形式精确导出。
- **单次重放**：从任一快照复制成草稿，按需编辑、校验、补充本次凭证后重新发送一次模型请求；新的调用标注重放来源，历史记录不变。

## 本地运行

需要 Go 1.24+、Node.js 22.12+ 和 npm。

启动代理，将 `-target` 换成上游 API 地址：

```bash
go run ./cmd/proxy \
  -listen 127.0.0.1:12337 \
  -target http://127.0.0.1:28000 \
  -logdir log \
  -maxbody 10240
```

另一个终端启动界面：

```bash
cd web
npm ci
npm run dev
```

打开 Vite 输出的本地地址，默认是 `http://localhost:5173`。前端默认连接 `http://localhost:12337`，可通过环境变量切换：

```bash
BACKEND_URL=http://127.0.0.1:12666 npm run dev
```

将 LLM 客户端的 base URL 设为 `http://127.0.0.1:12337/v1`，保留客户端原来的鉴权设置。若客户端请求路径已经包含 `/v1`，`-target` 通常只填写上游 origin，避免重复拼接 `/v1/v1`。

## 会话与关联依据

### 显式会话标识

按以下顺序选择主会话标识；同一次请求中出现的其他标识会建立别名，方便后续只携带一种标识的请求找到同一会话：

1. `X-Session-ID`、`X-Conversation-ID`、`X-Thread-ID` 请求头。
2. JSON 的 `session_id`、`conversation` 字符串或 `conversation.id`、`conversation_id`、`thread_id`。
3. `metadata.session_id`、`metadata.conversation_id`、`metadata.thread_id`。
4. `/threads/{thread_id}/...` 或 `/conversations/{conversation_id}/...` 路径。

响应返回的 `conversation.id` 也会建立会话别名。关联索引按上游地址和鉴权身份隔离。相同的显式 ID 在不同凭证下不会混在一起；出现冲突时详情会显示原因。

没有标识或关联证据的请求会建立独立的 `auto:...` 会话。旧版统一的 `default` 归档不再用于这些请求；手工传入 `X-Session-ID: default` 仍可归档到这个名字。

### 父调用

父调用依据的优先级：

| 依据 | 画布展示 | 行为 |
| --- | --- | --- |
| `X-Parent-Trace-ID` 或 `metadata.parent_trace_id` | 实线 | 引用网关捕获的 trace |
| `previous_response_id` | 实线 | 引用生成该响应的请求 |
| 完整历史内容匹配 | 虚线 | 唯一匹配已完成请求及其输出时推断关联 |

每个代理响应都会附加 `X-Gateway-Trace-ID`，可用于显式关联后续调用。

历史匹配支持 `messages` 和 Responses `input`，并处理文本内容块及工具调用/返回。它要求完整的历史前缀包含先前用户消息与助手输出。只有共同系统提示、重复的首轮问题、不同鉴权身份或存在多个候选时，都会保持独立。使用服务端上下文的 Responses 请求不会把局部 `input` 当作完整历史。

相同会话标识表示共同归属；画布连线需要独立的父调用证据。一个父调用可以有多个分支。缺失或无法唯一确定的父调用显示为引用节点，后续捕获到生成请求时会自动补齐。查询、轮询或取消一个已保存 Response，不会成为这个响应的另一个生成者。

节点代表实际经过网关的请求。工具调用显示在所属请求的指标与报文中。

## API

### 会话：`GET /api/sessions`

返回以 session ID 为 key 的对象，每个会话包含 `id`、`label`、`created_at`、`logs`。运行中的请求也包含在快照内。

`RequestLog` 在原有报文与指标字段外提供：

```json
{
  "trace_id": "gateway-trace-b",
  "session_id": "auto:gateway-trace-a",
  "time": "2026-09-09T02:00:00.123456+08:00",
  "model": "your-model",
  "protocol": "responses",
  "summary": "继续上一轮问题",
  "status": "done",
  "revision": 12,
  "correlation": {
    "session_source": "previous_response_id",
    "response_id": "resp-b",
    "previous_response_id": "resp-a",
    "parent_reference": "resp-a",
    "parent_trace_id": "gateway-trace-a",
    "link_source": "previous_response_id",
    "confidence": "exact"
  }
}
```

`status` 是 `pending` / `running` / `done` / `error` / `canceled`；`revision` 随日志的权威状态变化递增。`correlation` 还可包含 `conversation_id`、`thread_id` 和 `warning`。HTTP 4xx/5xx、代理错误和已识别 LLM 流的异常结束显示为错误；人工、超时或客户端取消显示为已取消。`wait_duration_ms` 与 `upstream_duration_ms` 分别记录等待和上游耗时；总耗时仍包含请求处理开销。

### 图谱：`GET /api/graph`

```bash
# 所有已捕获的调用
curl http://127.0.0.1:12337/api/graph

# 指定会话，正确编码特殊字符
curl -G http://127.0.0.1:12337/api/graph \
  --data-urlencode 'session_id=auto:gateway-trace-a'
```

响应结构：

```json
{
  "revision": 12,
  "session_id": "auto:gateway-trace-a",
  "nodes": [],
  "edges": [
    {
      "id": "gateway-trace-a:gateway-trace-b",
      "source": "gateway-trace-a",
      "target": "gateway-trace-b",
      "kind": "previous_response_id",
      "confidence": "exact"
    }
  ]
}
```

上例省略了节点内容。节点的 `id` 对应 trace 或引用 ID，`kind` 是 `request` / `reference`，并包含 `label`、模型、时间、状态、指标和 `correlation`；完整报文从 sessions 读取。空图返回空数组，未知会话返回 JSON 404，非 GET 方法返回 JSON 405。

### 实时事件：`/api/ws`

| 事件 | 内容 |
| --- | --- |
| `request_start` | 原有 `trace_id` / `session_id` / `method` / `path` / `time`，以及初始 `log` |
| `request_updated` | `trace_id` 和进入等待、放行等中间状态的权威 `log` |
| `sse_delta` | SSE `data`、事件字段与不可变的累计 `metrics` 快照 |
| `request_end` | `trace_id` 和最终 `log` |
| `sessions_updated` | 会话/关联已变化，客户端重新获取会话和图谱快照 |
| `interceptions_updated` | 待处理请求或已保存草稿变化，客户端重新获取拦截快照 |
| `replays_updated` | 重放记录状态变化，客户端重新获取重放记录 |

界面会自动重连并定期同步快照，保留用户选中的会话/节点。浏览器刷新后也会恢复选择。实时广播不会阻塞代理转发或倒计时。

### 动态规则：`/api/rules`

GET 列出规则，POST 创建规则；`PUT /api/rules/{id}` 替换配置，`DELETE /api/rules/{id}` 删除：

```bash
curl -X POST http://127.0.0.1:12337/api/rules \
  -H 'Content-Type: application/json' \
  -d '{"path_match":"/messages","body_match":"","inject_system":"Be concise.","intercept":false}'
```

`path_match` / `body_match` 使用子串匹配，匹配依据为原始正文及加上上游前缀的路径。启用的规则按创建顺序执行，`inject_system` 依次追加到 JSON 的 `system` 字段（支持字符串或内容块）。默认带有一条 `/messages` 的简洁回答注入规则，界面可以编辑、停用或删除。

启用拦截的规则示例：

```bash
curl -X POST http://127.0.0.1:12337/api/rules \
  -H 'Content-Type: application/json' \
  -d '{"path_match":"/responses","intercept":true,"wait_seconds":30,"timeout_action":"forward","disabled":false}'
```

`wait_seconds` 默认 30，范围 1–3600；`timeout_action` 为 `forward`（默认）或 `cancel`。首条匹配的拦截规则决定策略；修改、停用或删除规则只影响后续请求。

### 等待与编辑：`/api/interceptions`

在 Dynamic Rules 中新建并启用拦截规则，再从“待处理”进入请求。正文编辑源是完整原文，包含已应用的规则注入；“变更对比”展示与客户端原文的字段差异。保存不延长倒计时；到期转发使用最近一次保存成功的版本。刷新页面可恢复已保存内容，未保存的本地草稿不会发送。

| API | 行为 |
| --- | --- |
| `GET /api/interceptions` | `{server_time, requests: []}`，列出等待中的摘要 |
| `GET /api/interceptions/{trace_id}` | `{server_time, request}`，完整正文、允许编辑的头、原文及状态 |
| `PATCH /api/interceptions/{trace_id}` | 校验并保存候选请求 |
| `POST /api/interceptions/{trace_id}/validate` | 只校验，不保存 |
| `POST /api/interceptions/{trace_id}/release` | 应用可选修改并放行 |
| `POST /api/interceptions/{trace_id}/cancel` | 取消，不转发 |

写操作必须携带最新 `revision`，例如 `{"revision":1,"body":"{\"model\":\"your-model\",\"input\":\"edited\"}"}`。`body` 为完整 JSON 文本，`headers` 为允许编辑的请求头集合；省略即保留。取消只传 `revision`。保存和放行返回更新后的快照；过期、重复或版本冲突返回 409，非法修改返回 422，非法控制 JSON 返回 400，超过 16 MiB 返回 413，未知请求返回 404。

允许编辑 Content-Type、Accept、Anthropic-Version、Anthropic-Beta 和 OpenAI-Beta。鉴权、会话标识、显式父引用及转发地址保持不变。修改后检查 JSON、协议结构和本地 tool call/result 配对；Responses 服务端历史中的工具调用仍需由上游验证。压缩或二进制请求支持原样放行和取消。消息内容改变后，历史关联与后续索引使用实际出站内容。

手动取消向原客户端返回 409；超时取消返回 504。客户端断开时立即结束等待，日志状态码为 499；放行后也继续使用原请求的取消信号。每个等待请求只能产生一次放行或取消决定。

### 完整请求：`GET /api/requests/{trace_id}`

返回 `{trace_id, original, outgoing?}`，每份快照包含 `method`、`url`、脱敏后的选定 `headers`、完整 `body`、`content_length` 和 `credentials`。在“原始 / 出站”中查看；未转发的请求没有 `outgoing`。正文不受 `-maxbody` 限制；压缩或非 UTF-8 字节使用 Base64，标记 `body_encoding: "base64"`。`credentials` 只列出请求携带的凭证名称和方案（例如 `{"kind":"header","name":"Authorization","scheme":"Bearer","replayable":true}`），URL 中的密钥类查询参数在展示时也遮盖为 `key=****`。原始鉴权只保留在转发中的 HTTP 请求上，不会从这些接口返回。

### 安全 cURL：`GET /api/requests/{trace_id}/curl`

`?source=original` 生成发往网关的命令（再次经过规则并被记录为新的调用），`?source=outgoing` 生成发往上游的命令（包含上游路径前缀和已应用的规则修改，不经过网关）。默认使用出站快照；未出站的请求只有原始快照，请求出站快照返回 404。

```bash
curl -s "http://127.0.0.1:12337/api/requests/<trace_id>/curl?source=outgoing"
```

响应包含 `command`、`destination`、`environment`、`body_mode`、`body_file` 和 `notes`。命令面向 POSIX shell，所有 URL、请求头和正文都已引号保护；凭证以 `${REPLAY_AUTHORIZATION}`、`${REPLAY_QUERY_KEY}` 等环境变量占位，不会复制遮盖后的值。签名类鉴权（AWS SigV4、Digest、HMAC 等）会标注无法静态重放。超过 64 KiB、压缩或二进制的正文使用文件模式：`GET /api/requests/{trace_id}/body?source=…` 下载精确字节，命令通过 `--data-binary @文件名` 引用。

### 单次重放：`/api/replays`

| API | 行为 |
| --- | --- |
| `POST /api/replays` | 发起一次重放，返回 202 和重放记录 |
| `POST /api/replays/validate` | 只做结构校验，并列出仍缺少的凭证 |
| `GET /api/replays` / `GET /api/replays/{id}` | 重放记录列表 / 单条记录 |
| `POST /api/replays/{id}/cancel` | 取消进行中的重放 |

```bash
curl -X POST http://127.0.0.1:12337/api/replays \
  -H 'Content-Type: application/json' \
  -d '{"trace_id":"<trace_id>","source":"outgoing","idempotency_key":"<uuid>","credentials":[{"kind":"header","name":"Authorization","value":"Bearer <key>"}]}'
```

`source` 为 `original` 或 `outgoing`（默认出站）；`body` 与 `headers` 与拦截编辑相同，省略即原样发送；`timeout_seconds` 默认 600。`idempotency_key` 必填：同一 key 同一内容只执行一次并返回已有记录（200），同一 key 不同内容返回 409。只支持 `/messages`、`/chat/completions`、`/responses` 的 POST 请求；出站快照必须与当前 `-target` 一致，重放时去掉一次前缀且不再重复注入或进入断点；原始快照重放会像新的客户端请求一样再次经过规则。凭证只用于这一次重放，不会进入日志、快照、导出命令或浏览器存储；缺少凭证返回 422 并列出名称。

重放记录包含 `state`（`running` / `done` / `error` / `canceled`）、`reason`（`manual` / `timeout`）、`status_code` 和 `trace_id`。新的调用在日志和图谱节点上带有 `replay: {id, of, source, modified}`，画布用虚点线“重放来源”连接被重放的请求，这不代表对话上的父子关系。手动取消记录状态码 499，超时记录 504。服务重启、页面刷新或重连都不会自动重放。

## 范围与存储

- 会话、关联索引、规则、等待状态、完整请求快照和重放记录保存在进程内存，重启后清空。界面刷新与 WebSocket 重连可以恢复当前进程的快照。
- ID 提取先于 `-maxbody` 的展示截断。历史匹配有 4 MiB 的内容上限；超限时继续使用显式 ID。已完成请求只保留历史索引需要的摘要。
- SSE 原始字节写入 `log/sse/{trace_id}.log`；`-maxbody` 不限制这些原始转储。
- Chat Completions 的内容统计与历史匹配使用首个 choice。接口没有提供 token 用量时，增量字符数只是估算；最终 usage 到达后覆盖估算值。
- 画布呈现网关实际观察到的调用与接口返回的数据。网关未捕获的历史会显示为引用，不会向提供商自动拉取历史。
- WebSocket 目前做字节透传，帧内的 LLM 请求尚不进入调用图。代理保持现有的宽松 TLS 校验配置，适用于本地调试。

## 验证

```bash
go test -race ./...
go vet ./...
cd web
npm run build
```

后端测试覆盖三种协议的 JSON/SSE、工具历史、内容截断、分支、迟到父调用、ID 冲突、循环引用、响应轮询、跨身份隔离和并发快照，以及倒计时、并发决策、非法修改、原鉴权保留、取消、规则管理、完整请求和修改后的历史索引；cURL 导出会用真实的 `curl` 逐字节回放，重放测试覆盖幂等、凭证隔离、编辑校验、取消、超时、目的地校验、压缩正文和 SSE。浏览器验证见 [请求拦截验证记录](output/playwright/interception/verification.md) 和 [cURL 导出与重放验证记录](output/playwright/replay/verification.md)。
