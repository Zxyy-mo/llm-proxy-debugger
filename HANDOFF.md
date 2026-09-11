# 当前分支交接记录

日期：2026-09-11。当前开发分支为 `feat/agent-debugger-workflow`，从 `refactor/modular-structure` 的基础上继续演进。已拉取 `00ec270` 的基础交付，并在 `a9072cb` 完成会话隐私/清理、持久化重放登记、完整响应分页与实际 Agent 排障流程。

确认文档已在中文提交 `98514cc` 推送。本轮在此基础上实现 Run/Attempt 四层模型、兼容上游预设与模型发现，仍沿用该开发分支。

## 先看这些

- [README](README.md)：运行方式、主要 API、实际边界。
- [排障使用指南](docs/USAGE.md)：定位、上下文、重放、结果评估和恢复。
- [功能清单](FEATURE_CHECKLIST.md) / [交付路线](IMPLEMENTATION_ROADMAP.md)：当前阶段已交付内容。
- [开发与提交约定](CONTRIBUTING.md)：中文提交说明，以及重要函数和关键步骤的中文注释要求。
- [契约索引](docs/README.md)：精确的字段、状态机与限制。
- [验收报告](output/playwright/foundation/verification.md) / [结构化结果](output/playwright/foundation/results.json)：可复核证据。
- 最新证据：[基础修复](output/playwright/foundation-refinement/verification.md)、[排障流程](output/playwright/agent-debug-loop/verification.md)。
- 本轮证据：[四层调用](output/playwright/call-layers/verification.md)、[兼容接入](output/playwright/compatible-provider-setup/verification.md)。

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
| Run / Attempt | `internal/correlation/run.go`、`internal/store/{runs,attempts}.go`、`internal/proxy/attempts.go` |
| 接入预设与模型发现 | `internal/provider/capabilities.go`、`internal/proxy/provider_models.go`、`web/src/components/ProviderPanel.vue` |
| 注入与断点 | `internal/proxy/{injection,interception}.go`、`internal/intercept` |
| 脱敏 | `internal/privacy`、`internal/store/privacy.go` |
| 工具意图、结果与 Span | `internal/observation`、`internal/store/tools.go` |
| cURL 与模型重放 | `internal/export`、`internal/replay`、`internal/proxy/replay.go` |
| UI | `web/src/App.vue`、`web/src/components`、`web/src/lib/{api,types,metrics}.ts` |
| 调查与结果评估 | `web/src/lib/{investigation,replayOperation,requestFinder,responseComparison}.ts`、`RequestActions.vue`、`ResponseComparison.vue` |

## 运行和检查

```sh
npm --prefix web ci
npm --prefix web run build
go run ./cmd/proxy -listen 127.0.0.1:12337 -target http://127.0.0.1:28000/v1 -logdir log
```

管理页面：`http://127.0.0.1:12337/ui/`。生产构建后不需要单独运行 Vite。开发时可另启 `npm run dev`，`BACKEND_URL` 配置管理 API 的代理地址。

```sh
go test -race ./...
go vet ./...
npm --prefix web test
npm --prefix web run build
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
8. 新重放的登记与幂等键同步保存后才执行；失败返回 503 且不发送。普通元数据仍异步合并写入；中断请求不会自动补发，也不能承诺严格恰好一次完成。
9. 部署默认本机监听和 TLS 校验；管理 API 不是多租户安全边界。
10. Run 证据和请求/工具因果分开；缺失/冲突保持未关联，不跨上游或凭证作用域自动合并。重放生成独立 Run，不修改原始报文。
11. Attempt 只代表网关实际开始的发送；头部到达不表示完整结束。WebSocket 清理先记录真实失败，再取消内部读循环；控制帧也必须通过统一拨号能力检查。
12. v3 保存逐次出站文件与私有 forwarding/path；v1/v2 只读出已有事实。回滚旧程序需要原数据库与正文备份。
13. Provider 查询结果绑定当前保存配置，加载/保存互斥，临时密钥只在内存；配置模板和模型列表成功不证明真实接口全部可用。

实际排障流程已验收：请求定位、上下文检查、草稿与执行状态保持、可恢复的重放和结果评估均可使用。完整 Agent 执行器、更多 SDK 自动插桩、全协议转换或高容量存储仍作为独立需求定义。

## 本轮实现后的推进方向

四层调用与兼容接入的通用实现已完成，范围见 [实施路线](IMPLEMENTATION_ROADMAP.md#四层模型与兼容接入的交付范围)。下一步需要实际实例地址/凭证来验证 CPA、New API、Sub2API、vLLM 的版本和开放能力，不能把本轮受控测试当作四个平台的真实联调。完整 Agent/MCP 重放及自动 SDK 插桩继续作为独立范围。
