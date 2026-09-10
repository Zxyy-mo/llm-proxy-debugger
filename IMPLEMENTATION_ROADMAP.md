# 实施路线与当前交付

更新日期：2026-09-10。**`refactor/modular-structure` 是后续产品的唯一开发基线。** 本轮已完成 M2–M8 的约定实现、界面和验证；无需重新实现已有会话、图谱、断点或重放能力。

## 交付状态

| 阶段 | 当前可用行为 | 对应契约 |
| --- | --- | --- |
| 基础与 M1 | 三协议观测、会话/父子关联、图谱、HTTP 断点、安全 cURL、单次重放 | [关联](docs/contracts/conversation-graph.md)、[断点](docs/contracts/request-interception.md)、[重放](docs/contracts/request-replay.md) |
| M2 | 完整 JSON/SSE 文件、下载、Token 来源、排除等待的首响应时序、来源响应对比 | [响应与指标](docs/contracts/response-metrics.md) |
| M3 | 相邻上下文对齐、system/tools 变化、远端缺口、协议注入与规则优先级 | [上下文与规则](docs/contracts/context-rules.md) |
| M4 | 记录/出站脱敏、稳定替换、原文保留及受控还原 | [脱敏、历史与工具](docs/contracts/privacy-history-tools.md) |
| M5 | SQLite 元数据、报文文件恢复、搜索/清理、中断状态与重放幂等恢复 | 同上 |
| M6 | 独立工具节点、参数与结果、MCP/Agent/tool Span 接收和真实耗时 | 同上 |
| M7 | Provider 管理、模型路由/别名、显式备用上游、文本/函数协议转换 | [Provider 与传输](docs/contracts/providers-transports.md) |
| M8 | Responses WebSocket 调用观测，已保存 Response 按 ID 查询 | 同上 |

每一项都有后端、操作界面、契约和验证。详细勾选项见 [FEATURE_CHECKLIST.md](FEATURE_CHECKLIST.md)；实测证据见 [foundation/verification.md](output/playwright/foundation/verification.md)。

## 已确定的实现选择

1. **保持真实报文与展示投影的区分。** 完整请求/响应按文件保存；列表和实时事件只传有界内容。gzip、脱敏、转换前后及 WebSocket JSONL 都有明确标记。
2. **指标带来源。** Token 分 usage、字符估算、未知；计时从上游开始，不推算 Provider 内部排队或纯模型时间。超大 SSE 解析受限时不保留“完整准确观测”的假象。
3. **关系需要证据。** 会话归属、真实父子、重放来源、工具执行分别记录；远端历史、歧义和缺失节点不靠时间补猜。
4. **策略在请求进入时确定。** 规则、脱敏与所选目的地的变更影响后续请求；等待中的请求不因管理面板改动改投其他上游。
5. **支持子集显式化。** 原生透传承载 Provider 自身协议；转换只覆盖明确可映射的文本/函数语义，遇到不支持字段或输出明确失败。
6. **恢复状态，不恢复执行。** 已落盘记录、索引、配置、工具和幂等键恢复；之前活动的模型请求/重放标记中断，不自动执行。
7. **先提供可接入的 tracing 契约。** 外部应用主动上报工具执行 Span，网关不自行调度工具，也不把 API 间隔当工具耗时。

这些选择及已知容量边界也记录在 [foundation 设计记录](docs/plans/foundation.md)。

## 当前版本的能力边界

- M6 提供结构化工具记录和 Span 接口，尚无各家 SDK 的自动插桩包或完整 Agent 流程重放执行器。
- M7 提供模型匹配、优先级、别名及有限故障切换；尚无负载均衡调度，协议转换也不宣称覆盖多模态、内置工具或远端状态。
- M8 的历史为按已知 ID 读取保存的 Response，非账号枚举；WebSocket 为 Responses 调用观测，帧编辑/帧重放未实现。
- 元数据目前是 SQLite 单行 JSON 合并快照，查询在内存筛选，正文另存文件；不是海量记录的 SQL 查询架构。
- HTTP JSON 与流式文本累积仍使用内存。持久化默认约 200 ms 合并写，突然中止可能丢失最近尚未落盘的状态，包括刚创建的幂等键。
- 模型未公开的内部推理、未经过网关也未上报的工具执行，不属于可推断的已知数据。

后续工作应由实际使用需求决定：大规模历史可考虑增量表结构与数据库筛选；新的 Provider/SDK 可增加独立能力适配。它们不再与本轮已完成的基础功能混列为“M2 还没开始”。

## 验证范围

Go race/vet 和 Vue 构建通过；浏览器覆盖功能、桌面/窄屏/短屏布局；实际进程重启验证报文哈希、配置、图谱/Span 和幂等状态；真实 `glm-5.3-flash` 测试验证 Chat Completions 的 JSON/SSE 中转与完整下载。

Responses WebSocket、Provider 历史、转换与故障切换有受控 mock/协议测试。真实中转是否提供这些能力仍取决于其接口，当前报告不将 mock 通过等同于该 Provider 已通过。
