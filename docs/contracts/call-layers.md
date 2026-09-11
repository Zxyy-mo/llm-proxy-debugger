# 会话、任务、模型请求与上游尝试

本契约描述网关已经观测到的调用层次。现有 `trace_id` 继续标识一次客户端逻辑请求；新增 Run 和 Attempt 不改变请求、工具 Span 或重放来源的含义。

| 层次 | 身份 | 含义 |
| --- | --- | --- |
| Session / Thread | `session_id` 与 `correlation` 中的明确标识 | 持续对话及其会话归属证据 |
| Run | `run_id`，另有 `run` 证据说明 | 调用方明确标记的一轮任务，可包含并发、分支请求 |
| Request | `trace_id` | 一次客户端模型 API 调用；单次重放会创建新请求 |
| Attempt | `route.attempts[].id` | 网关对配置上游真正开始的一次发送尝试 |

共享会话、相邻时间、历史前缀、工具 Call ID 和 WebSocket lane 均不自动建立 Run。请求因果继续遵循 [会话图契约](conversation-graph.md)；工具执行继续遵循 [工具 Span 契约](privacy-history-tools.md)。Run 归属本身不添加因果边。

## 明确标记一轮任务

HTTP 请求可携带 `X-Run-ID`，JSON 正文可携带 `metadata.run_id`。同一轮所有模型请求使用相同值，下一轮使用新值；推荐 UUID。正文和头部都提供时必须相同。普通透传请求的这些字节保持原样，能力由实际上游决定。

```sh
curl 'http://127.0.0.1:12337/v1/chat/completions' \
  -H 'Content-Type: application/json' \
  -H 'X-Session-ID: demo-conversation' \
  -H 'X-Run-ID: task-20260911-001' \
  -d '{"model":"your-model","messages":[{"role":"user","content":"分析这份代码"}]}'
```

两个字段都只接受不含控制字符的非空 UTF-8 字符串，去掉首尾空格后最多 1024 字节。所有重复 Header 值都会检查；相同值可以互证。不同值、重复 `metadata`/`run_id` JSON 键带来的歧义、非字符串、空值、过长或非法值不会被择优忽略。标识问题只使任务归属保持未关联，不改变原始请求转发行为。

新请求日志及图中的请求节点包含：

```json
{
  "trace_id": "request-uuid",
  "run_id": "run:<opaque-hash>",
  "run": {
    "state": "explicit",
    "external_id": "task-20260911-001",
    "sources": ["header:X-Run-ID", "body:metadata.run_id"]
  }
}
```

`run.state` 为 `explicit`、`replay`、`missing` 或 `conflict`。`missing`/`conflict` 没有 `run_id`；`sources` 始终为数组。`warning` 可为：

| warning | 含义 |
| --- | --- |
| `invalid_run_identifier` | 明确提供了非法任务标识 |
| `conflicting_run_identifier` | 多个来源或重复 JSON 字段产生矛盾/歧义 |
| `conflicting_session_identifier` | 任务所属的明确会话标识存在矛盾、重复字段或多值头歧义 |
| `legacy_run_unavailable` | 旧历史未记录任务边界，无法可靠补齐 |

HTTP 响应在已有 `X-Gateway-Trace-ID` 之外，对有效归属附带内部 `X-Gateway-Run-ID`。继续同一轮时仍复用调用方原来的 `X-Run-ID`，不要把这个内部查询 ID 当作新的外部任务标识。

## 稳定身份与未知会话

Run 内部身份由入站上游/客户端凭证哈希作用域、入站明确主会话标识的种类和值、外部 Run 值共同决定。主会话标识沿用现有会话提取优先级；来源字段名称不参与同值身份比较。

- 同一明确会话中的相同 Run 值聚合；不同明确会话复用 `run-1` 不会混组。
- 没有明确主会话标识时使用空会话作用域，同一上游/凭证下的相同 Run 值仍可表示一轮并发任务。其 Session 归属可以未知，网关不会为此合并会话。
- 没有 Session 的调用方若重复使用相同 Run 值，网关无法据时间区分轮次；每轮需要自行分配新值。
- 晚到的 `conversation.id`、父请求恢复、上下文编辑和会话移动不改写已经确定的 `run_id`。因此缺会话证据的请求与后来明确带会话标识的请求可能保守地落在不同 Run。
- 跨配置上游或跨客户端凭证的同值 Run 不自动合并。一次请求故障切换后的 Attempt 沿用请求的初始身份，不随切换目的地重建任务。
- 同一 Run 后来出现多个明确会话时，汇总标记 `session_state:conflict`；不重写请求因果关系，也不创建虚构会话。

任务归属在入站时确定。断点和重放的正文编辑不能修改 `metadata.run_id`；不合法的 Run 值也不能通过编辑悄悄变成另一值。出站隐私替换不会把占位符当作新的任务身份。

Session、Thread、Conversation 的相关 JSON 键重复（包括根字段、`metadata` 内字段、`conversation.id`），含会话身份的 `metadata` 对象重复，或对应会话头出现多个值时，Run 保持 `conflict`，不会把旧 Session 解析行为选中的首值当作确定作用域。同值重复字段也按歧义处理；不同字段来源提供同一有效身份仍可互证。检查仅覆盖实际参与会话归属的路径，不检查消息或工具内容里的同名字段。现有 Session 提取及原始报文不因此改变。

## 单次重放的任务归属

每个显式重放动作由服务器创建独立任务，`run.state:replay`、`sources:["gateway:replay"]`，内部身份为 `run:replay:<replay-id>`。复用同一幂等键的重试仍返回原来的重放动作。

原始或出站快照中的旧 Run 标识仍保留在完整报文中，以维持精确重放；服务器的观测归属覆盖这份旧证据，因此新请求不会误归入来源任务。可信重放标记放在服务器 context 中，客户端不能通过自定义头伪造。完整重放约束及持久注册边界见 [单请求重放契约](request-replay.md)。

## 任务查询

`GET /api/runs?session_id=<encoded-session-id>` 返回：

```json
{
  "revision": 42,
  "runs": [{
    "id": "run:<opaque-hash>",
    "external_id": "task-20260911-001",
    "sources": ["header:X-Run-ID"],
    "session_ids": ["demo-conversation"],
    "session_state": "known",
    "trace_ids": ["request-uuid"],
    "request_count": 1,
    "attempt_count": 2,
    "active_count": 0,
    "started_at": "2026-09-11T12:00:00+08:00",
    "last_at": "2026-09-11T12:00:00+08:00",
    "partial": false
  }],
  "unassociated_trace_ids": []
}
```

不传 `session_id` 时查询全部捕获；空结果使用空数组。未知/已空会话为 404，非法查询参数或重复 `session_id` 为 400，非 GET 为 405 并带 `Allow: GET`。

筛选后 `trace_ids`、请求/尝试/活动数量、时间只统计选中会话内的请求；`session_ids` 与 `session_state` 保留整轮已捕获的会话范围。`partial:true` 表示同一任务还有范围外请求。`session_state` 为 `unknown`、`known` 或 `conflict`；多个 `auto:` 等临时显示会话本身不代表存在多个已确认会话。

`started_at`/`last_at` 是筛选范围内最早/最晚请求的入站时间，`active_count` 统计 `running`/`pending` 请求。它们不宣称完整 Agent 任务何时开始、结束或成功。

`GET /api/history` 支持独立的精确 `run_id` 筛选；`q` 也检索内部 Run ID 及按当前请求策略投影后的外部 Run 标签。现有 Session、模型、状态和游标语义不变。

## 上游尝试

`route.attempts` 是尝试元数据的唯一权威来源，按 `sequence` 排列。保留旧字段 `provider_id`、`url`、`status_code`、`duration_ms`、`error`，新增：

| 字段 | 含义 |
| --- | --- |
| `id` / `trace_id` / `sequence` | 尝试身份、所属请求、从 1 开始的发送顺序 |
| `transport` | `http` 或 `websocket` |
| `source` | `gateway` 为新观测，`legacy_summary` 为旧摘要 |
| `status` | `running`、`done`、`error`、`canceled`、`abandoned`、`interrupted`；旧摘要为 `unknown` |
| `url` | 配置上游 Base URL，保留旧语义 |
| `request_url` | 该次实际出站 URL，包括挂载前缀；秘密 query 已掩码 |
| `started_at` | 真正开始传输尝试的时间，独立出站捕获完成后设置 |
| `headers_at` | HTTP 响应头返回时间，WebSocket 模型帧不制造该字段 |
| `duration_ms` | 旧版头部返回或发送失败耗时；WebSocket 无 HTTP 头，结束后为帧调用耗时 |
| `ended_at` / `total_duration_ms` | 真实响应结束、读取错误或主动关闭的时间及总耗时；无法观测时缺失 |

本地能力检查、路由/协议转换、鉴权或断点取消发生在实际发送前，因而没有 Attempt。传输失败（例如 DNS/TLS/写入失败）是实际发送尝试，但不能证明上游已经收到请求；没有上游 HTTP 响应时 `status_code` 缺失，不用网关自己的 502 代替它。

收到响应头后尝试仍在运行；所选响应的 EOF、读取错误或 Close 结束计时。故障切换前放弃的响应为 `abandoned`，不为了捕获失败正文无限延迟下一上游。已经输出部分 JSON/SSE 的响应不会再次切换。EOF 后才能确认的协议错误允许 `done` 单向补为 `error`，保留原 HTTP 状态、头部与结束时间及耗时；不会重新运行，也不允许失败变成功。

Responses WebSocket 每次真正开始发送 `response.create` 的帧对应一个 Attempt，握手本身不冒充模型请求。帧写入后可记录 101 表示已有连接；模型终止、取消或连接断开结束该次尝试。连接或 `stream_id` 不建立 Run。握手 `X-Run-ID` 适用于连接中的全部调用；逐轮使用 `metadata.run_id` 时应避免和固定头部矛盾。

Attempt 只覆盖本进程调用配置中转服务的传输边界。CPA、New API、Sub2API 或其他中转内部的重试/切换仍未知；不得把这张列表解释为中转内部完整供应商执行日志。

## 独立出站查看和下载

| 接口 | 返回 |
| --- | --- |
| `GET /api/attempts/{trace_id}` | `{trace_id, attempts:[]}`；存在但从未发送的请求为空数组 |
| `GET /api/attempts/{trace_id}/{attempt_id}` | `{trace_id, attempt, request}`；`request` 是该次独立出站 `RequestSnapshot` |
| `GET /api/attempts/{trace_id}/{attempt_id}/body` | 按请求隐私策略投影的完整正文附件；Base64 快照会解码为字节 |

接口都只读，返回 `Cache-Control:no-store`。未知请求/尝试或路径为 404；非 GET 为 405；旧摘要缺少该次正文、文件不可读或无法解码时，正文下载返回 422。详情仍返回 `request.unavailable` 解释缺失，绝不退回最后一次出站内容冒充旧尝试。下载使用固定安全文件名，不接受客户端文件路径，不把外部 Run ID 放入响应头。

每次尝试的出站正文独立保存，Provider、路径、别名/协议转换后的实际报文及去凭证的转发资料都可检查。现有 `GET /api/requests/{trace_id}` 的 `outgoing` 继续指向最后一次真正尝试的同一快照，原有导出与单次重放行为保持兼容。

本期独立保存每次出站请求及结果元数据；没有新增“为每次失败尝试读取完整响应正文”的行为。最终客户端响应和可用的转换前变体仍通过 [响应接口](response-metrics.md) 查看，不能把最终响应当作所有尝试的响应。

## 隐私、持久化与恢复

Run 和 Attempt 沿用所属请求冻结的隐私策略和命名空间。外部 Run 标签、尝试 URL/错误、请求头及完整正文先按策略投影；不保留原文时，外部 Run 原值不能另存在私有证据或 `X-Run-ID` 转发头中。任务分组或会话移动不能重新分配隐私 namespace，转换前原始响应的原有省略边界也保持。

元数据快照写版本 3，读取版本 1、2、3。每个独立尝试额外保存 API 隐藏的 `Forwarding`/`FilePath`，重启后仍能读取正确正文。旧记录不从正文、时间或会话补推 Run；旧 RouteAttempt 带 `source:legacy_summary`、稳定派生身份和未知完整时间，缺失的独立正文始终明确说明。

重启后活动尝试变为 `interrupted`，不伪造 `ended_at` 和 `total_duration_ms`；原有请求/重放同样中断而不补发。普通元数据继续约 200 ms 合并持久化，重放注册仍在发送前同步提交；增加任务/尝试不改变该边界。

历史清理包含请求所属的全部尝试文件，先持久化元数据再删文件；仍跳过活动请求，保护其他存活记录引用的文件与隐私映射。Run 汇总从存活请求派生，不留下空任务。清理请求不删除重放幂等键。

回退到仅支持 v1/v2 的旧程序需要恢复旧元数据与正文备份；不能降低 JSON `version` 字段假装 v3 兼容。存储、恢复与清理的完整约束见 [隐私和历史契约](privacy-history-tools.md)。

## 行为验证

- [Run 提取测试](../../internal/correlation/run_test.go)：多来源/重复值、非法与冲突标识、历史大小上限和会话冲突。
- [任务分组测试](../../internal/store/runs_test.go)：同轮并发、跨会话/凭证隔离、缺失/冲突、晚到会话移动、独立重放、只读 API 和深拷贝。
- [尝试测试](../../internal/store/attempts_test.go)：独立快照、结果冻结、协议失败补充、完整下载、隐私、旧版本迁移、中断恢复及文件清理。
- [编辑校验测试](../../internal/intercept/run_validation_test.go)：任务标识不可变，普通模型/正文编辑仍有效。
- [真实传输边界测试](../../internal/proxy/attempts_test.go)：受控 HTTP 故障切换、头部先到/正文未结束、取消、协议错误、精确重放的新任务、WS 断连/握手与控制帧能力检查。

界面通过 `RunNavigator.vue` 的任务选择筛选请求列表；API 结果绑定当前会话、失败可重试，画布保留完整会话关联。`AttemptCapture.vue` 绑定 Request + Attempt，切换取消旧读取；预览最多 65,536 字符，下载完整安全快照。任务筛选栏只在画布/详情显示，窄屏选择框单独一行，重放工作台继续优先保证编辑空间。实际操作与视口证据见 [本轮验收](../../output/playwright/call-layers/verification.md)。

这些测试使用受控数据和本地存储；不代表已完成任意远端供应商实例的真实接入验收。
