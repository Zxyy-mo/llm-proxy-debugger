import assert from 'node:assert/strict'
import test from 'node:test'
import { displayProviderConfig, filterProviderModels, isSavedProvider, modelsResultIsCurrent, normalizeProvider, providerFingerprint, providerFromPreset, routeFromProviderModel } from '../src/lib/providerSetup.ts'
import type { Provider, ProviderConfig, ProviderModelsResult, ProviderPreset } from '../src/lib/types'

function provider(values: Partial<Provider> = {}): Provider {
  return { id: 'relay', name: '中转', base_url: 'https://example.invalid/v1', protocol: 'passthrough', history: false, websocket: false, ...values }
}

function result(p: Provider = provider()): ProviderModelsResult {
  return { provider_id: p.id, provider: p, status_code: 200, models: [{ id: 'model' }], source: 'provider_models', checked_at: '2026-09-11T00:00:00Z' }
}

test('旧配置补齐未知状态，副本不会改写保存值或凭空开启能力', () => {
  const original: ProviderConfig = { providers: [provider()], routes: [{ id: 'route', model: '*', provider_id: 'relay', priority: 0, disabled: false, failover: ['other'] }] }
  const displayed = displayProviderConfig(original)
  assert.equal(displayed.providers[0]!.profile, 'custom')
  assert.deepEqual(displayed.providers[0]!.capabilities, { chat_completions: 'unknown', responses: 'unknown', messages: 'unknown', models: 'unknown' })
  assert.equal(isSavedProvider(displayed.providers[0], original.providers[0]), true)
  displayed.providers[0]!.capabilities!.models = 'supported'
  displayed.routes[0]!.failover!.push('more')
  assert.equal(original.providers[0]!.capabilities, undefined)
  assert.deepEqual(original.routes[0]!.failover, ['other'])
  assert.equal(isSavedProvider(displayed.providers[0], original.providers[0]), false)
})

test('新接入沿用模板并分配独立 ID，模板能力不会共享引用', () => {
  const defaults = provider({ id: '', name: 'vLLM', base_url: '', profile: 'vllm', key_env: 'VLLM_API_KEY', capabilities: { models: 'unknown' } })
  const preset: ProviderPreset = { id: 'vllm', name: 'vLLM', defaults, description: '', base_url_placeholder: '', notes: [] }
  const created = providerFromPreset(preset, [provider({ id: 'vllm-1' })])
  assert.equal(created.id, 'vllm-2')
  assert.equal(created.base_url, '')
  assert.equal(created.key_env, 'VLLM_API_KEY')
  assert.equal(created.history, false)
  assert.equal(created.websocket, false)
  created.capabilities!.models = 'supported'
  assert.equal(defaults.capabilities!.models, 'unknown')
  assert.equal(providerFromPreset(undefined, []).id, 'provider-1')
})

test('模型结果必须同时属于当前草稿、保存配置和实际查询配置', () => {
  const saved = provider()
  assert.equal(modelsResultIsCurrent(result(), normalizeProvider(saved), saved), true)
  for (const changed of [
    { base_url: 'https://other.invalid' }, { key_env: 'OTHER_KEY' }, { auth_header: 'X-API-Key' }, { auth_scheme: 'Token' },
    { protocol: 'openai' }, { capabilities: { models: 'unsupported' } }, { history: true }, { websocket: true }, { id: 'renamed' },
  ] satisfies Partial<Provider>[]) {
    assert.equal(modelsResultIsCurrent(result(), provider(changed), saved), false)
    assert.equal(modelsResultIsCurrent(result(provider(changed)), saved, saved), false)
    assert.notEqual(providerFingerprint(provider(changed)), providerFingerprint(saved))
  }
  assert.equal(modelsResultIsCurrent(result(), undefined, saved), false)
  assert.equal(modelsResultIsCurrent(result(), saved, undefined), false)
  assert.equal(modelsResultIsCurrent(null, saved, saved), false)
  assert.equal(modelsResultIsCurrent({ ...result(), provider_id: 'other' }, saved, saved), false)
  assert.equal(providerFingerprint(provider({ auth_header: 'authorization' })), providerFingerprint(saved))
})

test('模型填入路由保留客户别名，切换 Provider 后清理不兼容的备用配置', () => {
  const config: ProviderConfig = { providers: [provider(), provider({ id: 'same', protocol: 'passthrough' }), provider({ id: 'convert', protocol: 'openai' })], routes: [] }
  const old = { id: 'route-1', model: 'my-alias', provider_id: 'convert', target_model: 'old', priority: 10, disabled: true, failover: ['relay', 'same', 'convert', 'deleted'] }
  const route = routeFromProviderModel({ id: 'actual-model' }, config.providers[0]!, config, old)
  assert.deepEqual(route, { id: 'route-1', model: 'my-alias', provider_id: 'relay', target_model: 'actual-model', priority: 10, disabled: true, failover: ['same'] })
  assert.equal(old.provider_id, 'convert')
  assert.equal(old.target_model, 'old')
  config.routes.push(route)
  const created = routeFromProviderModel({ id: 'discovered-model' }, config.providers[0]!, config)
  assert.equal(created.id, 'route-2')
  assert.equal(created.model, 'discovered-model')
  assert.equal(created.target_model, 'discovered-model')
  for (const id of ['provider*model', 'm'.repeat(201), '模型'.repeat(50)]) {
    const aliased = routeFromProviderModel({ id }, config.providers[0]!, config)
    assert.equal(aliased.model, 'model-2')
    assert.equal(aliased.target_model, id)
  }
})

test('大模型列表只渲染有界结果，仍可搜索界面裁剪以外的模型', () => {
  const models = Array.from({ length: 4096 }, (_, index) => ({ id: `model-${index}`, owned_by: index > 4000 ? 'Local' : 'Relay' }))
  assert.equal(filterProviderModels(models, '').items.length, 100)
  assert.equal(filterProviderModels(models, '').matched, 4096)
  assert.equal(filterProviderModels(models, ' model-4095 ').items[0]!.id, 'model-4095')
  assert.equal(filterProviderModels(models, 'local').matched, 95)
  assert.equal(filterProviderModels(models, '', Infinity).items.length, 100)
  assert.equal(filterProviderModels(models, '', 10000).items.length, 200)
})
