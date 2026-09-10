# 交接记录：当前进度与 M2 任务

日期：2026-09-10。写给接手 M2 的开发者或 Agent。读完本文再看 [IMPLEMENTATION_ROADMAP.md](IMPLEMENTATION_ROADMAP.md) 和 [FEATURE_CHECKLIST.md](FEATURE_CHECKLIST.md)。

## 1. 当前进度

| 里程碑 | 状态 | 证据 |
| --- | --- | --- |
| 会话自动关联与调用图 | 已完成 | [output/playwright/call-graph/verification.md](output/playwright/call-graph/verification.md) |
| 前端裁切与响应式布局修复 | 已完成 | [output/playwright/layout-fix/verification.md](output/playwright/layout-fix/verification.md) |
| P0 请求断点与倒计时编辑 | 已完成 | [output/playwright/interception/verification.md](output/playwright/interception/verification.md) |
| M1 安全 cURL 导出与单次请求重放 | 已完成（2026-09-10） | [output/playwright/replay/verification.md](output/playwright/replay/verification.md) |
| M2 完整响应与可信指标 | **待开始，本次交接** | [docs/plans/m2-response-metrics/prd.md](docs/plans/m2-response-metrics/prd.md) |
| M3–M8 | 待办 | 路线图 |

M1 交付的能力：捕获快照记录可转发请求头与凭证名称（不存凭证值）；`GET /api/requests/{trace}/curl` 与 `/body` 导出 POSIX 安全的命令和精确正文；`/api/replays` 用本次凭证重放单次模型请求（幂等 key、校验、取消、超时），新调用带 `replay` 来源并在画布上以独立虚点线显示。契约见 [docs/contracts/request-replay.md](docs/contracts/request-replay.md)。

## 2. 工作区状态（接手前必读）

- 分支 `refactor/modular-structure`。2026-09-10 已获授权整理提交并推送当前成果；以 Git 历史和 `git status` 为准。
- 会话关联、调用图、布局修复、断点和 M1 共享代理、存储与主界面的改动，作为一组完整功能提交，避免拆出不能独立构建的中间状态。
- `.trellis/`、助手配置及运行状态继续只保留在本地。可共享的已实现契约在 `docs/contracts/`，M2 规划在 `docs/plans/m2-response-metrics/`，可随代码一起推送。
- 所有会话、规则、快照、等待状态和重放记录都只在进程内存中，重启即清空（M5 之前如此）。
- `output/playwright/` 中的验证报告、可复用脚本和合成数据截图纳入版本控制；运行日志、实时服务原始记录及 `.playwright-cli/` 中的下载文件保持本地忽略。

## 3. 运行与验证

```bash
# 后端与前端
go run ./cmd/proxy -listen 127.0.0.1:12337 -target http://127.0.0.1:28000 -logdir log -maxbody 10240
cd web && npm ci && npm run dev          # http://localhost:5173（Vite 只监听 IPv6 localhost，用 localhost 而不是 127.0.0.1）

# 自动化验证（交付前必须全部通过）
go test -race ./... && go vet ./...
cd web && npm run build
```

前次验证使用网关 127.0.0.1:12337、界面 http://localhost:5173/ 。服务是否仍在运行需要现场检查；调试时按需启动，结束后只停止本次启动的进程。

浏览器验证的惯例：用隔离端口（网关 12338、Vite 5174、mock 上游 28001），mock 上游和脚本放在 `output/playwright/<专项>/`，用 `playwright-cli`（`~/.codex/skills/playwright/scripts/playwright_cli.sh --session <名> run-code --filename <脚本>`）执行；结果写成 `results.json` + `verification.md`。M1 的 `output/playwright/replay/` 目录是可直接照抄的模板（`mock_upstream.py`、`seed.py`、三个 run-code 脚本、`api_cases.py`）。

## 4. M2 交接：完整响应与可信指标

### 目标

看到并下载任一请求（含重放）的完整响应；知道 Token 数是 Provider usage 还是字符估算；看到上游首字节与首有效内容的时间，且不掺入人工/规则等待。

### 现状事实与代码入口

| 事实 | 位置 |
| --- | --- |
| JSON 响应：`ModifyResponse` 读到完整正文（含 gzip 解码），但 `recorder.captureJSON` 只保留 `-maxbody` 字节给 `RequestLog.response_body`；关联提取用的是完整正文 | `internal/proxy/handler.go` ModifyResponse、`internal/proxy/recorder.go` captureJSON |
| SSE：原始字节逐块追加到 `{logdir}/sse/{trace}.log`（完整），日志里只有截断后的累计文本；没有读取/下载接口 | `internal/proxy/recorder.go` dumpRaw |
| Token：`len([]rune(...))` 是字符估算；`Metrics.IsFinalOutputTokens`（及 thinking）表示 usage 权威值，累加器用它覆盖估算；没有“来源”字段 | `internal/protocol/{openai,anthropic,responses}.go`、`internal/protocol/accumulator.go` |
| 耗时：`upstreamStarted` 在 `captureTransport` 发出请求时记录，得到 `upstream_duration_ms`；`wait_duration_ms` 来自断点等待。响应头到达是 `ModifyResponse`，首字节是 recorder 首次 `Write`，首有效内容是累加器 `OutputContent`/`ThinkingContent` 首次非空——这些时刻目前都没记录 | `internal/proxy/handler.go`、`internal/proxy/recorder.go` |
| 重放与来源的关系已在 `RequestLog.replay` 中，可用于“重放结果与来源响应对比” | `internal/store/requests.go`、`web/src/components/ReplayWorkbench.vue` |
| 前端完整正文展示可复用不解析数字的 `formatBody` | `web/src/lib/correlation.ts` |

### 建议方案（可调整，但要在 spec 里写清楚）

- 存储：SSE 直接读现有落盘文件；JSON 二选一——record 内存全量（注意内存增长）或落盘 `{logdir}/responses/{trace}.json`。存解码后的字节并标注；不要把替换字符当内容。
- 接口：`GET /api/responses/{trace}`（元数据 + 完整正文或“未保存”的明确原因）、`GET /api/responses/{trace}/download`（精确字节、稳定文件名），对齐 M1 的 `/api/requests/{trace}/body`。
- `RequestLog` 新字段：`token_sources: {input, output, thinking}`（`usage` / `estimated` / `unknown`）、`ttfb_ms`、`ttfc_ms`、`response_bytes`、`response_stored`。错误、取消、无输出时省略时序字段，前端显示“未知”；Provider 排队/模型耗时只有 Provider 返回时才显示。
- 前端：详情/调用详情展示完整响应与下载；Token 旁标注来源；耗时区块加首字节/首内容；重放工作台加“对比来源响应”。
- 验收与顺序见 [PRD](docs/plans/m2-response-metrics/prd.md)。交付物：后端 + 界面 + `.trellis/spec/backend/response-metrics.md` + `output/playwright/response-metrics/verification.md` + README/清单/路线图更新。

### 经验与坑（M1 实际踩过）

- 测试 fixture 不要靠请求头或调用顺序区分行为：重放会原样重发请求头，`-race` 下调用顺序会变。按（可编辑的）正文内容分支，最稳。
- mock 上游断开检测是异步的，断言“上游已停止”要轮询而不是立刻判断。
- `playwright-cli run-code` 的沙箱里没有 `Buffer`、`TextDecoder`、动态 `import`；脚本用 `return` 返回结果（`console.log` 不会显示）；页面内 `fetch` 上游会被 CORS 拦，用 `page.request`；下载用 `download.createReadStream()`。组件状态会跨脚本残留，脚本开头先 `page.goto` 重新挂载。
- 用 bash heredoc 写含中文的文件会偶发出现 U+FFFD 乱码，改用文件写入工具或 Python 脚本文件；提交前 `grep -rn $'\xef\xbf\xbd'` 检查。
- 前端 JSON 编辑不要 `JSON.parse` 再 `stringify`（大整数丢精度），用 `formatBody`。
- Trellis 流程：先读取已有任务和工作流，补齐 PRD、设计、执行计划与上下文清单；M2 规划完成时仍保持 `planning`。进入实现前完成评审，再执行 `task.py start 09-10-response-metrics`；实现后的验证、规范同步和归档按当时工作流执行。

## 5. 相关文档索引

- 产品与 API：[README.md](README.md)
- 清单与路线：[FEATURE_CHECKLIST.md](FEATURE_CHECKLIST.md)、[IMPLEMENTATION_ROADMAP.md](IMPLEMENTATION_ROADMAP.md)
- 后端契约：[conversation-graph.md](docs/contracts/conversation-graph.md)、[request-interception.md](docs/contracts/request-interception.md)、[request-replay.md](docs/contracts/request-replay.md)
- 前端约定：[layout-guidelines.md](docs/contracts/layout-guidelines.md)
- 本地开发日志：`.trellis/workspace/xiaoyu/journal-1.md`（不发布）
