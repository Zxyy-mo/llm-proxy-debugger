# 兼容接入界面验收

时间：2026-09-11。使用独立的 `compatible-provider-check` Playwright session、网关 `127.0.0.1:12349` 和受控模型列表服务 `127.0.0.1:28009`。网关运行内存模式，日志位于专用临时目录；没有访问用户真实上游。

## 实际界面行为

- 从 vLLM 预设添加 Provider，确认原生透传、全部能力未知、History / WebSocket 关闭，修改地址并保存。
- 未提供鉴权的模型查询返回安全的 401 提示，不显示模拟上游错误体中的密钥。
- 临时密钥查询得到完整 250 个模型，输入框立即清空，`/api/history` 仍为 0 条。
- 界面只显示前 100 项，但能搜索并选择 `controlled-model-249`；创建路由后服务端仍无新路由，直到用户保存。
- 将匹配模型改为 `personal-model`，填入已有路由时保留该别名，实际模型更新为 `controlled-model-248`。保存路由不使原模型列表失效，也没有把 models 能力从 unknown 升级为 supported。
- 编辑 Base URL 后立即清除列表和待用密钥，未保存时查询按钮禁用；保存新挂载路径后可重新查询。
- 另一操作者修改保存配置后，使用旧列表填路由会先发现配置变化、作废列表并提供重新加载按钮；路由草稿未被旧模型覆盖。
- 重新加载按钮能读取新的保存值并恢复查询。

## reload/save 竞态回归

[config-race.js](config-race.js) 挂起从真实网关取得的 GET 或 PUT 响应，验证两个方向的互斥。12 个断言全部通过，结构化输出见 [results.json](results.json) 的 `config_race`。

- 旧 GET 等待中：地址和保存按钮禁用，显示加载状态；程序化 form submit 也不会发出 PUT。
- GET 返回后：恢复编辑。
- PUT 已送达服务端但响应等待中：草稿编辑和重新加载禁用；程序化 click 也不会发出 GET。
- PUT 返回后：服务端和界面均保留新路由实际模型，不会被旧 GET 覆盖。

这一回归是在代码审查指出竞态后补充的；最后一次前端类型检查和构建包含修复。

## 布局与命中

[layout.js](layout.js) 在 1600×1000、1366×768、1024×768、768×600、390×844、320×640、844×390 下检查新增接入类型、Chat 能力声明、保存、查询、模型列表、填入路由 6 个控件，共 42 项。

所有控件在滚动后通过真实试点击与中心点命中检查，页面没有水平溢出，模型选择控件保持 128 px 高度。详见 [results.json](results.json) 的 `layout`。

- [桌面](provider-1600x1000.png)
- [320×640](provider-320x640.png)
- [844×390](provider-844x390.png)
- [跨标签页变更后的失效提示](stale-models.png)

已人工查看 320×640 与 844×390 截图；按钮和说明正常显示，长面板通过独立区域滚动。浏览器控制台检查为 0 errors / 0 warnings。

## 自动检查与边界

`go test ./internal/provider ./internal/proxy`、`go test -race ./internal/provider ./internal/proxy`、`npm --prefix web test`（当次 51 项）、`npm --prefix web run build` 和 `git diff --check` 均通过。

四种 profile 的 Chat JSON/SSE、分片函数参数、工具返回、usage、429 错误、alias/auth 由 `internal/proxy/compatible_providers_test.go` 的受控 HTTP 服务验证。该证据证明共享兼容协议处理，不证明任一真实 CPA、New API、Sub2API 或 vLLM 实例的版本能力。未实现完整 Agent/MCP 工作流重放或 SDK 自动插桩。
