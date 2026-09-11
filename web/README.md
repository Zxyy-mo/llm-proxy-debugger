# LLM Debug Gateway Web UI

Vue 3 + TypeScript，使用 Vue Flow 与 Dagre 显示模型、工具及父调用引用。

```sh
npm ci
npm test
npm run build
```

构建后由仓库根目录的网关提供 `http://127.0.0.1:12337/ui/`，默认资源目录 `web/dist`。`npm run build` 包含类型检查；资源使用相对 base，支持 `/ui/` 挂载。

开发时运行 `npm run dev`，默认将 `/api/*` 与 `/api/ws` 代理到 `http://localhost:12337`。自定义后端：

```powershell
$env:BACKEND_URL = 'http://127.0.0.1:12666'
npm run dev
```

主要入口：

- **调用画布**：请求筛选/排序、显式选择聚焦、工具详情、缩放和图谱 JSON 导出；筛选保留完整因果图。
- **响应与详情**：优先查看结果、错误、完整响应、Token 来源和时序，直接进入上下文或编辑重放。
- **待处理**：HTTP 断点倒计时、完整 JSON/允许头编辑、Diff、校验、保存、放行或取消。
- **原始 / 出站**：完整捕获、安全 cURL、上下文变化、可恢复的单次重放，以及来源/结果的输出与指标评估。切换调查视图保留同一来源草稿。
- **管理**：历史搜索/清理、Provider 与模型路由、已保存 Response 查询、脱敏及工具 tracing 示例。
- **Dynamic Rules**：协议适配的指令注入、优先级、等待时间、超时策略、编辑/启停/删除。

画布以服务端证据连线，前端不按时间猜因果。实时增量与 HTTP 快照共同同步状态；修订号避免旧数据覆盖新数据。Token、缺失历史、脱敏投影和协议能力限制在相关视图中明确展示。

长正文/列表与管理表单可滚动，键盘支持节点选择。完整响应每次读取 256 KiB，支持独立翻页与直达末段；下载使用完整文件。请求差异保留数字字面量并限制解析/展示工作量。凭证仅存在于当前草稿和未确认提交的内存中。

API 与运行方式见 [项目 README](../README.md)，实际流程见 [使用指南](../docs/USAGE.md)，状态、恢复和视口证据见 [排障流程验收](../output/playwright/agent-debug-loop/verification.md)。
