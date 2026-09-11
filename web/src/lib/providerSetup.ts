import type { Provider, ProviderConfig, ProviderModel, ProviderModelsResult, ProviderPreset, ProviderRoute } from './types'

export const providerCapabilities = [
  { key: 'chat_completions', label: 'Chat Completions' },
  { key: 'responses', label: 'Responses' },
  { key: 'messages', label: 'Anthropic Messages' },
  { key: 'models', label: '模型列表' },
] as const

export const providerProfiles = [
  { id: 'custom', name: '自定义兼容接口' },
  { id: 'cpa', name: 'CPA' },
  { id: 'newapi', name: 'New API' },
  { id: 'sub2api', name: 'Sub2API' },
  { id: 'vllm', name: 'vLLM' },
] as const

// normalizeProvider 补齐界面需要的字段；缺失能力仍是未知，不根据模板名称升级支持状态。
export function normalizeProvider(value: Provider): Provider {
  const headers: Record<string, string> = { authorization: 'Authorization', 'x-api-key': 'X-API-Key', 'api-key': 'api-key' }
  return {
    ...value, profile: value.profile || 'custom',
    auth_header: headers[value.auth_header?.toLowerCase() || 'authorization'] || value.auth_header,
    key_env: value.key_env || '', auth_scheme: value.auth_scheme || '',
    capabilities: {
      chat_completions: value.capabilities?.chat_completions || 'unknown',
      responses: value.capabilities?.responses || 'unknown',
      messages: value.capabilities?.messages || 'unknown',
      models: value.capabilities?.models || 'unknown',
    },
  }
}

export function displayProviderConfig(value: ProviderConfig): ProviderConfig {
  return {
    providers: value.providers.map(normalizeProvider),
    routes: value.routes.map(route => ({ ...route, failover: [...(route.failover || [])] })),
  }
}

// providerFingerprint 只比较公开配置；结果必须对应同一保存版本，不能沿用换地址/鉴权前的列表。
export function providerFingerprint(value: Provider | undefined): string {
  if (!value) return ''
  const p = normalizeProvider(value)
  return JSON.stringify([
    p.id, p.name, p.base_url, p.protocol, p.profile, p.key_env, p.auth_header, p.auth_scheme,
    p.history, p.websocket, ...providerCapabilities.map(capability => p.capabilities?.[capability.key]),
  ])
}

export function isSavedProvider(draft: Provider | undefined, saved: Provider | undefined): boolean {
  return !!draft && !!saved && providerFingerprint(draft) === providerFingerprint(saved)
}

// modelsResultIsCurrent 同时验证草稿、保存值与实际查询快照，防止跨标签页改配置后误用列表。
export function modelsResultIsCurrent(result: ProviderModelsResult | null, draft: Provider | undefined, saved: Provider | undefined): boolean {
  return !!result && isSavedProvider(draft, saved) && result.provider_id === draft?.id && result.provider_id === result.provider.id && providerFingerprint(result.provider) === providerFingerprint(saved)
}

export function nextProviderSetupID(prefix: string, values: { id: string }[]): string {
  let index = 1
  while (values.some(value => value.id === `${prefix}-${index}`)) index++
  return `${prefix}-${index}`
}

export function providerFromPreset(preset: ProviderPreset | undefined, existing: Provider[]): Provider {
  return normalizeProvider({
    ...(preset?.defaults || { id: '', name: '', base_url: '', protocol: 'passthrough', history: false, websocket: false }),
    id: nextProviderSetupID(preset?.id === 'custom' || !preset ? 'provider' : preset.id, existing),
  })
}

// routeFromProviderModel 保留已有匹配别名和优先级；切换上游时仅保留协议一致且不指向自身的备用项。
export function routeFromProviderModel(model: ProviderModel, target: Provider, config: ProviderConfig, existing?: ProviderRoute): ProviderRoute {
  const id = existing?.id || nextProviderSetupID('route', config.routes)
  return {
    id, model: existing?.model || (new TextEncoder().encode(model.id).length <= 200 && !model.id.includes('*') ? model.id : id.replace('route-', 'model-')),
    provider_id: target.id, target_model: model.id, priority: existing?.priority ?? 0, disabled: existing?.disabled ?? false,
    failover: (existing?.failover || []).filter(fallback => fallback !== target.id && config.providers.some(p => p.id === fallback && p.protocol === target.protocol)),
  }
}

// filterProviderModels 限制一次渲染的选项数；仍允许搜索整个已查询列表，不把界面裁剪当作上游返回截断。
export function filterProviderModels(models: ProviderModel[], query: string, limit = 100): { items: ProviderModel[]; matched: number } {
  const search = query.trim().toLocaleLowerCase()
  const matches = search ? models.filter(model => `${model.id}\n${model.owned_by || ''}`.toLocaleLowerCase().includes(search)) : models
  return { items: matches.slice(0, Math.max(1, Math.min(Number.isFinite(limit) ? limit : 100, 200))), matched: matches.length }
}
