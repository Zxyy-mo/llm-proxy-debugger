# LLM Debug Gateway Web UI

Vue 3 + TypeScript，使用 Vue Flow 与 Dagre 显示模型、工具及父调用引用。

```powershell
npm ci
npm run build
```

构建后由仓库根目录的网关提供 `http://127.0.0.1:12337/ui/`，默认资源目录 `web/dist`。`npm run build` 包含类型检查；资源使用相对 base，支持 `/ui/` 挂载。

开发时运行 `npm run dev`，默认将 `/api/*` 与 `/api/ws` 代理到 `http://localhost:12337`。自定义后端：

```powershell
$env:BACKEND_URL = 'http://127.0.0.1:12666'
npm run dev
```

主要入口：

- **调用画布**：会话/全部调用、自动布局、节点选择、工具详情、缩放和图谱 JSON 导出。
- **思考与报文**：API 已返回内容、完整响应下载、准确/估算/未知 Token、首字节/首内容时序。
- **待处理**：HTTP 断点倒计时、完整 JSON/允许头编辑、Diff、校验、保存、放行或取消。
- **原始 / 出站**：完整捕获、安全 cURL、上下文变化、单次重放及来源响应对比。
- **管理**：历史搜索/清理、Provider 与模型路由、已保存 Response 查询、脱敏及工具 tracing 示例。
- **Dynamic Rules**：协议适配的指令注入、优先级、等待时间、超时策略、编辑/启停/删除。

画布以服务端证据连线，前端不按时间猜因果。实时增量与 HTTP 快照共同同步状态；修订号避免旧数据覆盖新数据。Token、缺失历史、脱敏投影和协议能力限制在相关视图中明确展示。

长正文/列表与管理表单可滚动，键盘支持节点选择。完整详情按需请求，默认最多预览 2 MiB，下载使用完整文件。凭证输入不写入浏览器持久存储。

API 与运行方式见 [项目 README](../README.md)，功能与视口证据见 [foundation 验证](../output/playwright/foundation/verification.md)。
