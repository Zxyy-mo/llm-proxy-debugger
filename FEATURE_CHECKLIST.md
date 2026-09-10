# LLM Debug Gateway 规划与实现清单

> 更新日期：2026-09-10
>
> 核对范围：早期产品构想、Trellis 任务记录、当前工作区代码和验证报告。
>
> 状态说明：`[x]` 已实现；`[ ]` 尚未实现；`[ ] 部分实现` 表示已有基础，但还不能完成原规划的完整使用流程。

## 一、最初规划的产品方向

目标是做一个供 Agent 应用工程师个人使用的高强度 LLM Gateway：观察客户端实际发送的上下文和 Provider 实际返回的内容，分析会话、工具调用、Token 与耗时，并允许在调试过程中修改流量。

### 第一阶段：可信的流量观察器

- [x] 将 LLM HTTP 请求转发到可配置的上游地址。
- [ ] 部分实现：支持 SSE 流式响应透传、解析和原始字节落盘；原始/出站请求有完整快照，日志及响应正文仍受 `-maxbody` 展示上限约束。
- [x] 在 Web UI 中查看请求、响应、状态码、错误、模型、协议、Token 和总耗时。
- [x] 解析 OpenAI Chat Completions、OpenAI Responses 和 Anthropic Messages 的 JSON/SSE 响应。
- [x] 展示接口明确返回的 reasoning/thinking 内容。
- [x] 统计模型返回的工具调用数量，并在原始报文中查看工具定义、调用参数和工具结果。
- [ ] 部分实现：Provider 返回 usage 时使用权威 Token；缺少 usage 时，流式增量使用字符数临时估算，尚未标注“准确/估算”。
- [ ] 部分实现：已分开统计人工/规则等待与上游耗时；TTFT 及网络、模型耗时的进一步细分尚未实现。
- [x] 从界面复制客户端原始请求或实际出站请求对应的安全 cURL；凭证以环境变量占位，大正文或二进制正文以文件精确导出。
- [ ] 数据库持久化和服务重启后的会话恢复。

### 第二阶段：可控的 LLM 调试器

- [ ] 相邻请求的上下文 Diff：新增、删除、压缩的消息，以及 system prompt、工具定义变化。
- [ ] 部分实现：已支持“仅重放单次模型请求”（原始或出站快照、编辑校验、本次凭证、幂等、取消、超时、重放来源标注）；“重放完整 Agent/工具流程”需要 M6 的真实工具执行记录。
- [x] 请求断点：命中规则后暂停出站转发。
- [x] 为拦截请求配置 1–3600 秒等待时间及服务端倒计时，默认 30 秒。
- [x] 在等待期间编辑完整 JSON 正文和允许编辑的请求头；支持校验、保存及保存并放行。
- [x] 展示原始请求与修改后请求的字段 Diff，以及完整原始/出站快照。
- [x] 对修改结果进行 JSON、协议结构和本地 tool call/tool result 配对校验；远端历史仍由 Provider 校验。
- [x] 手动放行或取消请求；超时后转发已保存内容或取消；版本校验防止过期和重复操作。
- [x] 客户端断开或取消后结束等待，并保留原请求的取消信号到上游。
- [x] 将人工/规则等待时间与上游耗时分开统计。
- [ ] 部分实现：动态规则支持路径和正文子串匹配，注入固定写入顶层 `system`；Chat Completions 与 Responses 的请求注入位置仍需按协议适配。
- [ ] 部分实现：规则支持修改、删除、启停；拦截按创建顺序首条命中，注入顺序叠加；尚无自定义优先级或拖拽排序。
- [ ] 请求正文的记录脱敏，例如在日志和页面中隐藏手机号。
- [ ] 出站脱敏，例如用稳定占位符替换敏感信息后再发送给 Provider。
- [ ] 跨历史消息、当前消息和工具结果保持同一敏感值的占位符一致，并支持受控还原。

### 第三阶段：完整的 Agent 调用视图

- [x] 根据会话 ID、前序响应 ID、父 Trace ID 和完整历史前缀自动关联跨请求调用。
- [x] 绘制可交互调用路径图，展示分支、缺失父调用和后续补链。
- [ ] 部分实现：可以从模型流量中观察工具调用意图、参数和后续工具结果，目前主要体现为原始报文、计数及历史关联。
- [ ] 将每个 tool call/tool result 结构化为独立节点，并展示关联、状态和耗时。
- [ ] MCP 客户端、MCP Server 或 Agent SDK tracing 接入；当前无法观察 MCP 内部执行、重试和真实耗时。
- [ ] 多 Provider 路由、按模型或规则选择 Provider、故障切换和负载策略。
- [ ] Provider 之间的请求/响应协议转换。
- [ ] 获取 Provider 账号侧历史；当前只展示实际经过本 Gateway 的流量。
- [ ] 捕获 WebSocket 帧内的 LLM 请求并纳入会话和调用图；当前仅做字节透传。

## 二、当前已经实现的功能

### 代理与记录

- [x] 通过 `-listen`、`-target`、`-logdir` 和 `-maxbody` 配置本地代理。
- [x] HTTP、JSON 和 SSE 请求/响应透传。
- [x] WebSocket 双向字节透传。
- [x] 为每个 HTTP 请求生成 Trace ID，并通过 `X-Gateway-Trace-ID` 返回给客户端。
- [x] 记录请求时间、客户端地址、方法、路径、查询参数（密钥类参数遮盖）、部分请求头、请求正文、响应正文、状态码、错误和总耗时。
- [x] `Authorization` 和 `X-API-Key` 在展示/结构化日志中遮盖；原始鉴权仍用于正常转发。快照只记录凭证名称和方案，不保存凭证值。
- [x] Zap 结构化日志、文件轮转，以及按 Trace ID 保存完整原始 SSE 字节。
- [x] 上游连接池复用，支持长时间 HTTP/SSE 请求。

### 协议与指标

- [x] OpenAI Chat Completions JSON/SSE 内容、reasoning、usage 和工具调用解析。
- [x] OpenAI Responses JSON/SSE 内容、reasoning summary、usage、Response ID、Conversation ID 和多类工具调用解析。
- [x] Anthropic Messages JSON/SSE 文本、thinking、usage、Message ID 和 tool use 解析。
- [x] 实时累积输入、输出、思考内容和工具调用数量。
- [x] Provider 最终 usage 覆盖流式字符估算，避免重复累计最终 Token。
- [x] 识别 HTTP 错误、代理错误及已识别 LLM SSE 的异常中止。
- [x] 基于连续 thinking 事件提供简单的 thinking-loop 提示。

### 会话自动关联

- [x] 保留手动 `X-Session-ID` 分组。
- [x] 识别请求头、JSON、metadata 和 URL 中的 session、conversation、thread 标识。
- [x] 使用响应中的 Response ID 和 Conversation ID 建立后续关联。
- [x] 支持 `X-Parent-Trace-ID`、`metadata.parent_trace_id` 和 `previous_response_id` 精确父子关系。
- [x] 对客户端重复发送的完整历史执行唯一前缀匹配，生成推断关系。
- [x] 支持文本内容块、tool call 和 tool result 的历史归一化。
- [x] 没有关联证据时创建隔离会话，不因共同 system prompt 或时间相近而误连。
- [x] 按上游地址和鉴权身份隔离隐式关联索引。
- [x] 处理分支、迟到父请求、缺失父引用、重复 ID、歧义候选和循环引用。
- [x] 区分会话共同归属与直接父子因果关系。

### 调用图和 Web UI

- [x] `GET /api/sessions` 提供当前进程中的会话与请求快照。
- [x] `GET /api/graph` 提供全部会话或指定会话的节点和边。
- [x] WebSocket 实时同步请求开始、SSE 增量、请求完成和关联修复事件。
- [x] WebSocket 断线自动重连，并重新获取当前进程的权威快照。
- [x] Vue Flow + Dagre 调用画布，支持自动布局、平移、缩放、显示全图和节点拖动。
- [x] 支持当前会话/全部会话切换、节点选择、节点定位和图谱 JSON 导出。
- [x] 节点详情展示状态、Token、耗时、工具数量、关联依据、警告、请求、响应和已返回思考内容。
- [x] 实线展示显式父引用，虚线展示历史内容推断。
- [x] 缺失父调用显示为引用节点，父请求稍后出现时自动修复。
- [x] 页面刷新后恢复选中的会话、节点、视图和图谱范围。
- [x] 桌面、平板、手机和横屏布局；长正文、长列表和规则面板可滚动。
- [x] 图谱随窗口、分隔面板和节点拓扑变化重新适配，同时尽量保留用户手动视口。

### 动态规则

- [x] `GET /api/rules` 列出当前进程内规则。
- [x] `POST /api/rules` 创建规则；`PUT /api/rules/{id}` 修改，`DELETE /api/rules/{id}` 删除。
- [x] 按 `path_match` 和可选 `body_match` 子串匹配请求。
- [x] 将 `inject_system` 追加或写入 JSON 请求的 `system` 字段后再转发。
- [x] `intercept` 开关实际执行请求等待，支持等待秒数、超时策略、待处理列表和完整编辑流程。
- [x] 编辑、启停和删除已创建规则；修改只影响后续请求。
- [ ] 规则持久化；当前重启服务后规则会恢复为代码中的默认值。

## 三、已经完成并验证的专项任务

### 自动会话关联与调用图

- [x] JSON 和 SSE 响应链、分支历史及工具回合关联。
- [x] 标识优先级、凭证隔离、冲突、缺失/迟到父调用、歧义历史和防循环。
- [x] 在展示正文截断前提取关联元数据。
- [x] 响应转发字节保持一致。
- [x] Go race 测试、Go vet、Vue 生产构建、浏览器交互和真实兼容接口验证通过。

验证记录：[output/playwright/call-graph/verification.md](output/playwright/call-graph/verification.md)

### 前端内容裁切与响应式布局修复

- [x] 修复规则提交按钮不可达、Console 标签裁切和长正文滚动问题。
- [x] 修复窄屏请求选择和请求/响应面板布局。
- [x] 修复容器变化或新增节点后的调用图适配。
- [x] 7 种视口、长内容、长列表、32 节点图和生产构建验证通过。

验证记录：[output/playwright/layout-fix/verification.md](output/playwright/layout-fix/verification.md)

### 安全 cURL 导出与单次请求重放

- [x] 原始/出站快照记录可转发请求头、凭证名称与方案，凭证值不保存；密钥类查询参数在展示中遮盖。
- [x] cURL 命令区分网关与上游目的地，所有值引号保护，凭证以环境变量占位，签名类鉴权标注无法静态重放；大正文/二进制以文件精确导出并可下载。
- [x] 真实 `curl` 逐字节回放验证；出站快照重放不重复注入、不重复拼接前缀、不再次进入断点；原始快照重放按新请求经过规则。
- [x] 单次重放的编辑校验、本次凭证、幂等 key、服务端超时与取消、失败状态，以及日志/图谱中的重放来源标注。
- [x] Go race 测试、Vue 构建、浏览器复制/下载/编辑重放/取消/超时/刷新只读，以及 7 种视口可达性验证通过。

验证记录：[output/playwright/replay/verification.md](output/playwright/replay/verification.md)

### 请求断点与倒计时编辑

- [x] 服务端倒计时、保存候选正文、手动放行/取消、超时策略和客户端断开处理。
- [x] 原始/出站完整快照、鉴权保留、内容长度更新、结构校验及字段差异。
- [x] 多窗口版本冲突、草稿保留、刷新恢复已保存内容和原请求取消信号。
- [x] 修改后的历史重新关联；取消请求不会建立成功历史索引。
- [x] 并发 race 检查、浏览器真实连接取消，以及 7 种视口和多请求队列布局检查。

验证记录：[output/playwright/interception/verification.md](output/playwright/interception/verification.md)

## 四、当前明确限制

- [ ] 会话、关联索引、规则、等待状态、完整请求快照和重放记录只保存在内存中，Gateway 重启后清空。
- [ ] Web UI 日志与响应正文仍可能被 `-maxbody` 截断；原始/出站请求可完整查看，压缩或二进制正文以 Base64 保存；仅 SSE 原始流独立落盘。
- [ ] 一个运行中的 Gateway 实例只配置一个固定上游 `-target`。
- [ ] WebSocket 帧没有协议解析、记录或调用图关联。
- [ ] 工具执行和 MCP 内部过程不经过 LLM HTTP 接口时无法观察。
- 产品边界：只能展示 Provider/API 明确返回的 reasoning/thinking；模型未公开的内部推理不作为待实现功能。
- 当前配置：动态规则默认包含 `/messages` 的 system 注入；界面支持将它停用或删除。
- 工程状态：2026-09-10 已整理会话关联、调用图、布局修复、请求拦截及 cURL 导出/重放的功能提交；M2 保持规划阶段。

## 五、后续建议顺序

- [x] P0：完成请求断点的服务端状态机、等待队列、超时和客户端取消处理。
- [x] P0：实现 Web UI 的待处理请求列表、编辑、Diff、校验、放行和取消。
- [x] P0：同时保存“客户端原始请求”和“实际出站请求”，为调试、cURL 和审计提供可靠来源。
- [x] M1 / P1：补齐可重放请求信息与单次鉴权来源，实现安全 cURL 复制和单次模型请求重放；出站快照重放不重复注入/拼接前缀，记录独立重放来源。
- [ ] M2 / P1（下一项）：完整响应查看与下载，Token 准确/估算/未知标记，TTFT 与响应时序。
- [ ] M3 / P1：相邻请求上下文 Diff、三种协议的请求注入适配、可配置规则优先级。
- [ ] M4 / P1：记录脱敏、出站脱敏、跨上下文稳定占位符与受控还原，明确原文保存策略。
- [ ] M5 / P1：持久化、重启恢复、历史查询与清理；中断请求不自动补发。
- [ ] M6 / P2：独立工具节点及 MCP/Agent tracing；在具备真实执行记录后定义完整 Agent 流程的重放模式。
- [ ] M7 / P2：多 Provider 路由、故障策略及按能力支持的协议转换。
- [ ] M8 / P2：选定 LLM WebSocket 协议的帧解析，以及有官方接口支持的 Provider 历史适配。
- [ ] 工程任务：补完 `.trellis/spec/backend/` 中仍为模板的项目规范，并更新 Trellis 的 bootstrap task 状态。

实施范围、依赖和整体验收定义见 [IMPLEMENTATION_ROADMAP.md](IMPLEMENTATION_ROADMAP.md)。M1 已完成（[任务 PRD](docs/contracts/request-replay.md)、[验证记录](output/playwright/replay/verification.md)）；下一项是 M2。

本次核对补入 M2 的观察数据缺口和 M3 的请求协议适配，避免后续清单只追踪新界面而遗漏最初构想。原文保存策略在 M5 建库前确定；模型内部未公开推理属于产品边界，不计入完成度。

## 六、状态依据

- 交接记录与 M2 任务入口：[HANDOFF.md](HANDOFF.md)、[docs/plans/m2-response-metrics/prd.md](docs/plans/m2-response-metrics/prd.md)
- 产品说明和 API：[README.md](README.md)
- 会话/图谱专项任务：[docs/contracts/conversation-graph.md](docs/contracts/conversation-graph.md)
- 前端布局专项任务：[docs/contracts/layout-guidelines.md](docs/contracts/layout-guidelines.md)
- 请求拦截契约：[docs/contracts/request-interception.md](docs/contracts/request-interception.md)
- cURL 导出与重放契约：[docs/contracts/request-replay.md](docs/contracts/request-replay.md)
- 会话与图谱契约：[docs/contracts/conversation-graph.md](docs/contracts/conversation-graph.md)
- HTTP 转发和动态注入：[internal/proxy/handler.go](internal/proxy/handler.go)
- 导出与重放实现：[internal/export](internal/export)、[internal/replay](internal/replay)、[internal/proxy/replay.go](internal/proxy/replay.go)
- 规则模型及内存存储：[internal/store/store.go](internal/store/store.go)
- 前端规则和调用视图：[web/src/App.vue](web/src/App.vue)
