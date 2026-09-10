# LLM Debug Gateway

面向个人开发者的 LLM 调试网关：捕获真实请求与响应，查看会话、上下文、工具与耗时，在 HTTP 请求发出前编辑，或从已捕获快照重放一次模型请求。

当前产品基线是 **`refactor/modular-structure`**。M1–M8 的约定能力已实现；功能、支持边界和验收分别见 [清单](FEATURE_CHECKLIST.md)、[路线与决策](IMPLEMENTATION_ROADMAP.md)、[验证记录](output/playwright/foundation/verification.md)。

## 现在可以做什么

- **完整报文**：原始/出站请求、JSON/SSE 响应按文件保存，支持完整下载、gzip 标记和有界预览；转换前后响应分开查看。
- **可信指标**：每个 Token 计数标注 Provider usage、字符估算或未知；首响应头、首有效内容从实际出站计时，排除断点等待。
- **会话与上下文**：显式会话标识、父 Trace、前序 Response ID、唯一完整历史前缀关联；支持分支、迟到父调用及相邻请求的消息/system/tools 对比。
- **流量修改**：三种协议的 Prompt 注入、规则优先级、倒计时断点、正文与允许的请求头编辑、校验、放行和取消。
- **重放**：安全 cURL 导出、单次模型请求重放、幂等键、超时/取消及来源响应对比；已有出站快照固定目的地并跳过重复注入与转换。
- **脱敏**：记录与出站策略独立，支持默认邮箱/手机号和自定义正则、稳定占位符、原文保留选择及受控还原。
- **本地历史**：SQLite 元数据、报文文件、规则、路由、工具和重放状态恢复；关键词/模型/状态/会话查询及清理。
- **工具视图**：独立工具节点、模型可见的参数与后续结果；通过 `POST /api/tool-spans` 接收 MCP/Agent/工具执行记录，真实上报耗时与未知耗时分开。
- **Provider**：模型路由、别名、环境变量鉴权和显式备用上游；可选择将独立 Anthropic/Responses 文本与函数工具请求转换为 Chat Completions。
- **Responses WebSocket**：按调用记录消息、用量、工具、取消和断开；按已知 ID 查询 Provider 保存的 Response。

## 本地运行

需要 Go 1.24+、Node.js 22.12+ 和 npm。在仓库根目录用 PowerShell 执行：

```powershell
Set-Location web
npm ci
npm run build
Set-Location ..

go run ./cmd/proxy -listen 127.0.0.1:12337 -target http://127.0.0.1:28000/v1 -logdir log
```

打开 **http://127.0.0.1:12337/ui/**。构建后的管理页面直接由网关提供。将客户端 Base URL 设为 `http://127.0.0.1:12337/v1`，保留客户端鉴权，或在“管理 → Provider 与路由”配置环境变量凭证。

`-target` 和 Provider Base URL 均支持 origin、挂载路径或以 `/v1` 结尾的地址；标准 `/v1` 前缀不会重复拼接。URL 的转义路径及非凭证查询参数保持原有编码。

| 参数 | 默认值 | 作用 |
| --- | --- | --- |
| `-listen` | `127.0.0.1:12337` | 网关与管理页面监听地址 |
| `-target` | `http://127.0.0.1:28000` | 未命中模型路由时的上游 |
| `-logdir` | `log` | 日志与请求/响应文件 |
| `-data` | `<logdir>/gateway.db` | SQLite 元数据；`-` 关闭持久化 |
| `-maxbody` | `10240` | 日志/广播的正文预览字节数，不限制完整文件 |
| `-web` | `web/dist` | `/ui/` 静态资源目录；`-` 关闭 |
| `-insecure` | `false` | 本地调试时允许不可信上游证书；默认验证 TLS |

开发前端时可在 `web` 目录运行 `npm run dev`。Vite 默认代理管理 API 到 `http://localhost:12337`；自定义地址用 `$env:BACKEND_URL = 'http://127.0.0.1:12666'`。

Provider 配置只保存密钥**环境变量名**，不保存密钥值。先在启动网关的进程环境中设置该变量，再在表单填写变量名。原始请求中的鉴权头、Cookie 和密钥类查询参数不会成为可导出的凭证值。

## 会话与图谱

会话标识按请求头 `X-Session-ID` / `X-Conversation-ID` / `X-Thread-ID`、请求 JSON、`metadata`、URL 中的 thread/conversation 标识依次识别。没有证据的请求创建独立会话。客户端的 Authorization、X-API-Key、api-key 身份参与上游范围的关联隔离。

父调用证据按 `X-Parent-Trace-ID` / `metadata.parent_trace_id`、`previous_response_id`、唯一完整历史前缀匹配依次处理。共享会话、时间相近或共享系统提示都不构成父子关系。HTTP 响应附带 `X-Gateway-Trace-ID`。

图中有模型请求、工具节点及缺失父调用引用。实线代表明确 ID，虚线代表历史推断，重放来源单独标注。响应查询不冒充响应生成者；未知远端历史明确显示缺口。只展示 API 明确返回的 reasoning/thinking。

## 主要 API

所有管理接口使用 `/api/` 前缀，正文为 JSON；它们面向本机调试，不自带多用户鉴权。

| API | 用途 |
| --- | --- |
| `GET /api/sessions` | 会话和有界日志快照 |
| `GET /api/graph?session_id=…` | 全部或指定会话的节点/边 |
| `/api/ws` | 界面实时事件；与上游 LLM WebSocket 分开 |
| `GET/POST /api/rules`，`PUT/DELETE /api/rules/{id}` | 注入和断点规则 |
| `GET /api/interceptions`，`GET/PATCH /api/interceptions/{trace}` | 等待队列和编辑草稿 |
| `POST /api/interceptions/{trace}/validate`、`/release`、`/cancel` | 校验、放行、取消；必须传最新 `revision` |
| `GET /api/requests/{trace}`、`/body`、`/curl` | 完整原始/出站请求、正文下载、安全 cURL |
| `GET /api/responses/{trace}`、`/download` | 完整响应；`variant=client\|upstream`，详情可用 `limit` 限制预览 |
| `GET /api/context-diff/{trace}?base=…` | 默认比较已捕获父调用，或手动选定基线 |
| `POST /api/replays`、`/validate` | 单次重放及预校验 |
| `GET /api/replays`、`GET /api/replays/{id}`、`POST /api/replays/{id}/cancel` | 重放记录和取消 |
| `GET/PUT /api/privacy`，`POST /api/privacy/restore` | 记录/出站策略与受控还原 |
| `GET /api/history`，`GET/DELETE /api/history/{trace}` | 搜索历史、读取或删除一个终态记录 |
| `POST /api/history/cleanup` | 按会话/时间清理；跳过活动请求 |
| `POST /api/tool-spans` | 外部执行记录上报 |
| `GET/PUT /api/providers` | Provider、模型路由和能力配置 |
| `POST /api/provider-history` | 按 ID 读取 Provider 保存的 Response |

详细字段、错误码及行为见 [接口契约目录](docs/README.md)。

## 使用边界与存储

- 默认开启 SQLite 持久化，元数据约每 200 ms 合并写入，正常退出刷新；异常中止可能丢失尚未写入的最近元数据。恢复中的活动请求标记中断，网关不会自动补发；客户端自身仍可能重试。
- SQLite 当前保存一份合并 JSON 元数据快照，历史筛选使用内存索引，正文另存文件。适用于个人调试；尚未实现按表增量存储和大规模分页数据库查询。
- `-data -` 仅关闭元数据持久化，仍可生成报文文件。备份/迁移需要同时保留数据库和捕获目录；本版本记录绝对文件路径，迁移后应保持可解析的捕获路径。
- JSON 响应处理及流式文本累积仍会使用内存。列表/广播采用有界预览；完整文件不是零内存转发保证。
- 单个 SSE 事件超过 2 MiB 时停止解析并显示 `observation_warning`；原生转发与原始捕获继续。部分指标显示未知，脱敏输出投影标注不完整。
- 记录脱敏不修改返回客户端的响应。启用时响应文件保存脱敏 JSON/输出投影，不保留原始 SSE/WS 事件；`retain_raw` 主要控制请求原文和还原映射。默认保留原文，策略详情见 [脱敏与历史契约](docs/contracts/privacy-history-tools.md)。
- 协议转换只支持明确的文本/函数子集；远端 Responses 上下文、保存/后台执行、多模态、Provider 内置工具、签名 reasoning 等语义使用原生透传。转换产生的 Response ID 不能查询 Provider 历史。
- WebSocket 观测限定 Responses，连接固定到首个 Provider，支持 `stream_id` 分组；不提供帧断点编辑或帧重放。
- 历史适配读取已知 ID 对应的保存记录，不枚举账号全部历史。工具 tracing 需要应用主动上报，网关不会执行完整 Agent/MCP 工作流。

## 验证

```powershell
go test -race ./...
go vet ./...
Set-Location web
npm run build
```

本轮包含 Go 回归与 race、生产构建、25 项浏览器功能断言、5 种视口可达性、13 项重启恢复检查、历史/文件清理，以及真实 `glm-5.3-flash` 中转的 JSON/SSE 检查。真实中转的 Responses、历史和 WebSocket 能力未据此宣称通过；这些能力有独立 mock 协议测试。

证据、脚本及截图见 [foundation 验证记录](output/playwright/foundation/verification.md)。开发入口见 [HANDOFF.md](HANDOFF.md)。
