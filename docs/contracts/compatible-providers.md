# 兼容上游接入、能力声明与模型发现

本契约扩展 [Provider 与传输契约](providers-transports.md)。CPA、New API、Sub2API、vLLM 共用原生协议、URL 拼接、鉴权和模型路由，不维护四套转发器。

## 接入模板与实例声明

`GET /api/provider-presets` 返回 `{presets,capability_source:"operator_declared",gateway}`。`presets` 包括 `custom`、`cpa`、`newapi`、`sub2api`、`vllm`；每项含 `id`、`name`、`description`、`base_url_placeholder`、`notes` 和 `defaults`。此接口不访问上游。

模板默认原生透传、Authorization / Bearer 鉴权、全部接口未知、历史和 WebSocket 关闭。自定义以外的模板建议使用对应的 `*_API_KEY` 环境变量名；实例免鉴权时可以删除 `key_env`。地址只给填写提示，不会自动填入或探测未知的远端。

| 类型 | 使用边界 | 示例 |
| --- | --- | --- |
| CPA | 当前没有确认具体同名项目和版本。地址、端口、鉴权及接口范围以用户实际实例为准。 | [provider-cpa.json](../../examples/provider-cpa.json) |
| New API | 使用实例公开的 API 地址和令牌；通道配置、模型权限决定实际可用范围。 | [provider-newapi.json](../../examples/provider-newapi.json) |
| Sub2API | 按实际版本开放的兼容接口接入，产品名称不保证开放全部协议。 | [provider-sub2api.json](../../examples/provider-sub2api.json) |
| vLLM | 使用 OpenAI 兼容服务地址。模型名称、工具调用及返回格式取决于模型和服务端启动配置。 | [provider-vllm.json](../../examples/provider-vllm.json) |

`Provider` 新增两个可选字段，旧配置缺失时继续工作：

```json
{
  "profile": "vllm",
  "capabilities": {
    "chat_completions": "unknown",
    "responses": "unsupported",
    "messages": "unknown",
    "models": "supported"
  }
}
```

这个片段仅说明声明格式，不代表 vLLM 的统一能力清单。`profile` 只允许以上五种名称；`capabilities` 只允许这四个字段，每个值只能为 `unknown`、`supported`、`unsupported`，空值等同未知。未知字段和类型错误的 JSON 返回 400，非法枚举值返回 422。

- **未知**：保留现有透传行为，不凭空声称支持。
- **支持**：操作者对这个实例的声明，不是自动验证结果。
- **不支持**：在访问对应接口前明确拒绝并说明 Provider 和能力名。

路径检查接受带或不带 `/v1` 的 Chat Completions、Responses、Messages、models 及其子资源路径；其他未知路径保留透传。`Provider.CheckEndpoint(path string) error` 的参数是转换后的逻辑 API 路径，尚未拼入 Base URL 的挂载前缀。转换模式实际发送 Chat Completions，因此检查 Chat 能力。Responses / Messages 的原生声明不能阻止一个本来可转为 Chat 的受支持请求，也不能绕过 Chat 的显式禁止。

`history` 与 `websocket` 仍为独立的显式开关，转换模式不提供原生 Responses WebSocket。切换界面上已有 Provider 的接入类型只更新说明，保留已填的地址、鉴权与能力，避免覆盖用户调整。

## 模型发现接口

`POST /api/provider-models` 只接受已保存的 `provider_id` 和可选临时密钥：

```json
{"provider_id":"local-vllm","api_key":"仅本次查询使用的密钥"}
```

服务端只会访问这个保存配置的 `GET /v1/models`，不接受临时 `base_url`。复用 Provider 的鉴权头、前缀、环境变量、TLS 配置与 URL 拼接：

| Base URL 路径 | 实际查询路径 |
| --- | --- |
| 空路径 | `/v1/models` |
| `/v1` 或 `/v1/` | `/v1/models` |
| `/gateway` | `/gateway/v1/models` |
| `/tenant%2Fone/v1` | `/tenant%2Fone/v1/models` |

临时密钥优先于环境变量，只存在本次调用的内存中；支持直接粘贴 Bearer 密钥。没有临时密钥时使用保存的 `key_env`；设置了变量名但进程中没有值返回 422。两者都没有时发送无鉴权查询，供本地免鉴权实例使用。管理请求自己的 Authorization 不会借用给上游。

成功响应示例：

```json
{
  "provider_id": "local-vllm",
  "provider": {
    "id": "local-vllm",
    "name": "本地 vLLM",
    "base_url": "http://127.0.0.1:8000/v1",
    "protocol": "passthrough",
    "profile": "vllm",
    "history": false,
    "websocket": false
  },
  "status_code": 200,
  "models": [{"id":"served-model","owned_by":"local","created":0}],
  "source": "provider_models",
  "checked_at": "2026-09-11T00:00:00Z"
}
```

`provider` 是本次查询使用的公开配置快照，不含密钥。列表按 ID 排序并去重，只返回 `id`、可选 `owned_by` 和 `created`。要求上游返回 JSON 对象中的 `data` 数组；缺失、null、非数组、部分 JSON 和错误响应均不能当成空列表。允许省略 `object`，提供时应为 `list`。ID 必须为非空字符串，无首尾空白或控制字符，UTF-8 长度不超过 512 字节；`owned_by` 同样有 512 字节及控制字符限制；`created` 为非负整数。

查询最长 30 秒，不跟随 HTTP 重定向，响应读取限制 2 MiB、模型列表限制 4096 项。超限时不返回局部模型列表。

| 情况 | 管理 API 状态 | 结果 |
| --- | --- | --- |
| 方法不支持 | 405 | `Allow: POST` |
| 输入不合法、未知字段或请求超过 16 KiB | 400 | 固定错误说明 |
| Provider 不存在 | 404 | 要求先保存 |
| 能力为不支持，或鉴权配置无效 | 422 | 发送前失败 |
| 查询期间 Provider 被修改或移除 | 409 | 丢弃过期结果 |
| 连接失败 | 502 | 不回显底层网络错误 |
| 超时 | 504 | 不超过 30 秒；更短的调用方截止时间同样生效 |
| 请求或网关关闭导致查询取消 | 408 | 查询取消说明 |
| 上游非 2xx（含重定向） | 200 | 保留真实 `status_code`，`models:[]` 和安全 `error/error_code` |
| 2xx 但响应无效、不可读或超限 | 200 | 保留真实 `status_code`，`models:[]` 和安全 `error/error_code` |

上游结果错误码为 `upstream_rejected`、`upstream_redirect`、`unreadable_response`、`response_too_large`、`invalid_response`。不把错误页、错误对象、Location 或其他未知上游字段复制到界面，避免错误体回显凭证。所有管理响应为 `Cache-Control: no-store`。

模型查询不产生 Session / Run / Request / Attempt，不写请求日志、配置或持久化模型缓存，不升级任何能力声明。返回一个模型名称只证明本次列表中存在该名称，不证明它可调用、支持工具调用、支持所有参数或已完成真实验收。

## 界面状态与使用流程

1. 在「Provider 与模型路由」选择接入类型，添加 Provider，填写地址及环境变量名，按已知实例事实设置能力。
2. 保存配置。只有与已保存值一致的 Provider 可以查询模型。
3. 在「查询模型并配置路由」显式查询；密钥框提交后立即清空，卸载面板时清空并取消请求。
4. 搜索列表并新建路由，或填入已有路由。已有路由保留客户端匹配别名、优先级与暂停状态，实际模型名改为所选 ID；切换 Provider 时清理自指或不同协议的备用项并说明移除数量。
5. 检查路由草稿并保存。应用模型不会直接执行模型调用。

列表结果同时绑定当前编辑值、保存值和服务端返回的查询快照。修改 Provider 的地址、鉴权、ID、协议或能力后，取消进行中的查询、清空临时密钥并作废结果；即使恢复旧字段也要重新查询。更换 Provider 不沿用上一项结果。仅修改路由不会把同一 Provider 的列表作废。应用模型前还会读取一次当前保存配置；其他浏览器标签页在查询完成后改动了这个 Provider，也会使旧结果作废，需要重新加载配置。

列表最多一次显示 100 个匹配选项，搜索仍覆盖全部模型，并标明匹配总数。超过路由匹配上限（UTF-8 200 字节）或包含 `*` 的模型 ID 会使用可编辑的本地别名，完整 ID 保存在实际模型名中。

重新加载和保存配置互斥；读取配置期间暂停草稿编辑、保存、模型应用和查询，防止较早返回的 GET 覆盖一次较新的保存结果。加载中有明确状态，结束后恢复控件。

## 可复现配置与受控验证

四份示例均为完整配置文档。替换示例域名、端口和 `replace-with-actual-model-id`，在网关进程环境设置所需密钥。`PUT /api/providers` 会替换整个配置，已有配置应先导出并把示例内容合并到当前配置中，再提交：

```sh
curl -fsS http://127.0.0.1:12337/api/providers > provider-backup.json
curl -fsS -X PUT -H 'Content-Type: application/json' \
  --data-binary @provider-config.json http://127.0.0.1:12337/api/providers
curl -fsS -X POST -H 'Content-Type: application/json' \
  --data '{"provider_id":"local-vllm"}' http://127.0.0.1:12337/api/provider-models
```

这里的管理端口应替换为实际 `-listen` 地址。示例中的 Provider ID 不同，应与查询的 `provider_id` 一致。没有为这四类服务提供真实地址或凭证；当前证据来自受控 Go HTTP 服务与界面验证，不能称为四个平台真实联调通过。

```sh
go test ./internal/provider ./internal/proxy
npm --prefix web test
npm --prefix web run build
```

- `internal/provider/capabilities_test.go`：旧配置、三态枚举、逻辑路径和保守预设。
- `internal/proxy/provider_models_test.go`：保存配置、URL/鉴权、控制字符、重定向、错误体、列表边界、超时和配置变动。
- `internal/proxy/compatible_providers_test.go`：四种 profile 的受控 Chat JSON/SSE、分片函数参数、工具返回、usage、错误、模型别名与鉴权。同一模拟服务验证共享协议行为，不冒充四个真实实例。
- `web/tests/providerSetup.test.ts`：快照失效、模型列表边界、路由应用与别名保留。

当前只能观察本代理对配置的中转站发出的执行尝试。中转站内部的供应商切换、重试和工具执行仍需它提供额外关联证据。本阶段不实现 Agent / MCP 完整工作流重放或自动 SDK 插桩。

## 兼容与回滚

新增字段可选；原有协议模式、路由、历史、WebSocket、隐私及重放语义保持独立。旧版本的严格配置 API 可能拒绝新增字段，回滚前导出配置并移除 `profile` 和 `capabilities`，再按旧版本导入。这个子功能不修改历史存储布局，也不重新执行已保存请求。
