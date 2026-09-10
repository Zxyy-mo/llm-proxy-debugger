# 当前分支交接记录

日期：2026-09-10。以 `refactor/modular-structure` 为持续开发基线。原有基础与 M1 保留，M2–M8 本轮约定能力已完成，代码与验收材料随本分支维护。

## 先看这些

- [README](README.md)：运行方式、主要 API、实际边界。
- [功能清单](FEATURE_CHECKLIST.md) / [交付路线](IMPLEMENTATION_ROADMAP.md)：当前阶段已交付内容。
- [契约索引](docs/README.md)：精确的字段、状态机与限制。
- [验收报告](output/playwright/foundation/verification.md) / [结构化结果](output/playwright/foundation/results.json)：可复核证据。

旧文档中的“仅内存、一个上游、原始 WebSocket 隧道、M2 待开始”已经过时，不应再作为评估当前完成度的依据。

## 代码入口

| 范围 | 入口 |
| --- | --- |
| CLI、同端口 UI、退出 | `cmd/proxy/main.go`、`internal/config` |
| 原生 HTTP、响应捕获与时序 | `internal/proxy/handler.go`、`recorder.go`、`internal/protocol`、`internal/sse` |
| Provider、路由、历史查询 | `internal/provider`、`internal/proxy/providers.go`、`internal/store/routes.go` |
| 请求/响应转换 | `internal/adapter`、`internal/proxy/conversion.go` |
| Responses WebSocket | `internal/proxy/websocket.go` |
| 请求/响应文件、SQLite、历史 | `internal/store/{bodyfiles,requests,responses,persistence,history}.go` |
| 关联与上下文 | `internal/correlation`、`internal/contextdiff`、`internal/store/{correlation,context,graph}.go` |
| 注入与断点 | `internal/proxy/{injection,interception}.go`、`internal/intercept` |
| 脱敏 | `internal/privacy`、`internal/store/privacy.go` |
| 工具意图、结果与 Span | `internal/observation`、`internal/store/tools.go` |
| cURL 与模型重放 | `internal/export`、`internal/replay`、`internal/proxy/replay.go` |
| UI | `web/src/App.vue`、`web/src/components`、`web/src/lib/{api,types,metrics}.ts` |

## 运行和检查

```powershell
Set-Location web
npm ci
npm run build
Set-Location ..
go run ./cmd/proxy -listen 127.0.0.1:12337 -target http://127.0.0.1:28000/v1 -logdir log
```

管理页面：`http://127.0.0.1:12337/ui/`。生产构建后不需要单独运行 Vite。开发时可另启 `npm run dev`，`BACKEND_URL` 配置管理 API 的代理地址。

```powershell
go test -race ./...
go vet ./...
Set-Location web
npm run build
```

Windows 文本读写始终显式 UTF-8。`Get-Content` 必须带 `-Encoding UTF8`；修改文件不得转成 ANSI/GBK。编辑 JSON 正文用保留数字文本的格式化函数，不经 JavaScript 浮点数往返处理。

## 验证资产与服务约定

`output/playwright/foundation/` 包含合成 mock、浏览器脚本、视口脚本、重启/清理脚本、真实中转 smoke 脚本和截图。完整说明见验证报告。

- 合成上游默认 `127.0.0.1:28002`，隔离网关 `127.0.0.1:12340`，数据在 `output/foundation-qa/`。
- 真实中转测试使用独立网关 `127.0.0.1:12339` 和 `output/live-foundation/`。实际密钥仅经测试进程环境传入，不写报告、脚本、设置或 Git。
- 交付结束时停止本轮自建的测试服务。再次验证须先检查端口/PID与可执行路径，不能结束其他开发进程。
- 强制重启测试应使用 `restart_qa.py hold` 这样的不重试客户端；浏览器自身可能在 TCP 断开后重试 POST。
- Chrome 设备模拟可能重载页面，脚本结果保存在 sessionStorage 或导出的报告中；截图全为合成数据。
- 仅报告、脚本和合成截图纳入版本控制；数据库、真实流量、密钥及原始下载继续忽略。

## 需要继续守住的契约

1. 新代码不得从有界日志预览反向构造原始请求；重放、下载使用完整捕获文件。
2. 出站重放固定原 Provider/地址，不重复注入、路由别名或协议转换。凭证来自当前配置的环境变量或本次临时输入。
3. Token 与时间需要来源；不能从字符或调用间隔伪造模型/工具内部耗时。
4. 工具 Span ID 与模型 call ID 不是同一身份；一个 call 的不同执行 Span 不得覆盖彼此。
5. Responses 查询与生成、会话归属与父子、WebSocket lane 与对话谱系必须分开。
6. 记录脱敏使用请求进入时的策略。事件分片不能绕过脱敏；保存的投影和真正原文必须标记清楚。
7. SQLite 当前是合并元数据快照，不应在文档中描述为已完成高规模规范化表结构。
8. 已落盘历史、幂等键和工具记录可恢复；中断请求不会自动补发。异步写入仍有崩溃窗口，不能承诺跨断电的严格恰好一次。
9. 部署默认本机监听和 TLS 校验；管理 API 不是多租户安全边界。

当前没有要求继续完成的 M2–M8 实现待办。完整 Agent 工作流、更多 SDK 自动插桩、全协议转换或高容量存储应作为独立需求定义。
