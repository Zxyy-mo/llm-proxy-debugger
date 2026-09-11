<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { APIError, requestJSON } from '@/lib/api'
import { formatBody } from '@/lib/correlation'
import { displayProviderConfig, filterProviderModels, isSavedProvider, modelsResultIsCurrent, nextProviderSetupID, providerCapabilities, providerFingerprint, providerFromPreset, providerProfiles, routeFromProviderModel } from '@/lib/providerSetup'
import type { ProviderConfig, ProviderModelsResult, ProviderPreset, ProviderPresetsResponse, ProviderProfile } from '@/lib/types'

const config = ref<ProviderConfig>({ providers: [], routes: [] })
const savedConfig = ref<ProviderConfig>({ providers: [], routes: [] })
const presets = ref<ProviderPreset[]>([])
const presetChoice = ref<ProviderProfile>('custom')
const presetError = ref('')
const defaultTarget = ref('')
const error = ref('')
const notice = ref('')
const busy = ref(false)
const ready = ref(false)
const loading = ref(false)
const controller = new AbortController()
const modelProvider = ref('')
const modelKey = ref('')
const modelsBusy = ref(false)
const modelsApplying = ref(false)
const modelsError = ref('')
const modelsNotice = ref('')
const modelsNeedsReload = ref(false)
const modelsResult = ref<ProviderModelsResult | null>(null)
const modelFilter = ref('')
const selectedModel = ref('')
const routeTarget = ref('')
let modelsController: AbortController | null = null
let modelApplyController: AbortController | null = null
const historyProvider = ref('')
const responseId = ref('')
const historyKey = ref('')
const historyBusy = ref(false)
const historyError = ref('')
const historyResult = ref<{ status_code: number; body: string; response_id: string } | null>(null)
let historyController: AbortController | null = null
const historyProviders = computed(() => savedConfig.value.providers.filter(p => p.history))
const selectedDraftProvider = computed(() => config.value.providers.find(p => p.id === modelProvider.value))
const selectedSavedProvider = computed(() => savedConfig.value.providers.find(p => p.id === modelProvider.value))
const canQueryModels = computed(() => isSavedProvider(selectedDraftProvider.value, selectedSavedProvider.value) && selectedSavedProvider.value?.capabilities?.models !== 'unsupported')
const currentModels = computed(() => modelsResultIsCurrent(modelsResult.value, selectedDraftProvider.value, selectedSavedProvider.value))
const listedModels = computed(() => filterProviderModels(currentModels.value ? modelsResult.value?.models || [] : [], modelFilter.value))
const canUseModel = computed(() => currentModels.value && !modelsResult.value?.error && !!modelsResult.value?.models.some(model => model.id === selectedModel.value) && !loading.value && !busy.value && !modelsApplying.value)
const canQueryHistory = computed(() => {
  const saved = historyProviders.value.find(p => p.id === historyProvider.value)
  return !!saved && isSavedProvider(config.value.providers.find(p => p.id === saved.id), saved)
})
const hasChanges = computed(() => JSON.stringify(config.value) !== JSON.stringify(savedConfig.value))
const fieldClass = 'h-9 w-full min-w-0 rounded-md border bg-background px-2 text-xs'

// load 与保存及草稿编辑互斥，防止较早的 GET 覆盖后续保存；模板失败不阻止查看已有 Provider。
async function load() {
  if (loading.value || busy.value || modelsApplying.value) return
  invalidateModels()
  loading.value = true; error.value = presetError.value = notice.value = ''
  const [configuration, templates] = await Promise.allSettled([
    requestJSON<{ config: ProviderConfig; default_target: string }>('/api/providers', 'GET', undefined, controller.signal),
    requestJSON<ProviderPresetsResponse>('/api/provider-presets', 'GET', undefined, controller.signal),
  ])
  if (controller.signal.aborted) return
  if (configuration.status === 'fulfilled') {
    savedConfig.value = displayProviderConfig(configuration.value.config)
    config.value = displayProviderConfig(configuration.value.config)
    defaultTarget.value = configuration.value.default_target; ready.value = true
  } else error.value = String(configuration.reason)
  if (templates.status === 'fulfilled') presets.value = templates.value.presets
  else presetError.value = '接入模板加载失败，已有配置仍可编辑；可重新加载重试。'
  loading.value = false
}
function presetFor(profile?: ProviderProfile) { return presets.value.find(item => item.id === (profile || 'custom')) }
function addProvider() { config.value.providers.push(providerFromPreset(presetFor(presetChoice.value), config.value.providers)) }
function addRoute() { config.value.routes.push({ id: nextProviderSetupID('route', config.value.routes), model: '', provider_id: config.value.providers[0]?.id ?? '', target_model: '', priority: 0, disabled: false, failover: [] }) }
function setFallback(index: number, event: Event) { const route = config.value.routes[index]; if (route) route.failover = (event.target as HTMLInputElement).value.split(',').map(v => v.trim()).filter(Boolean) }
// save 只在没有旧读取或其他保存时提交；持久化报错保留草稿，不能把失败当成保存成功。
async function save() {
  if (!ready.value || loading.value || busy.value || modelsApplying.value) return
  busy.value = true; error.value = notice.value = ''
  try {
    const result = await requestJSON<{ config: ProviderConfig }>('/api/providers', 'PUT', config.value, controller.signal)
    if (controller.signal.aborted) return
    savedConfig.value = displayProviderConfig(result.config); config.value = displayProviderConfig(result.config)
    notice.value = '已保存。后续请求使用新配置，等待中的请求及已有 WebSocket 连接保持原目的地。'
    const firstProvider = savedConfig.value.providers[0]
    if (!modelProvider.value && firstProvider) modelProvider.value = firstProvider.id
  }
  catch (e) { error.value = String(e) }
  finally { busy.value = false }
}

// invalidateModels 在目的地、鉴权或声明变化时取消旧查询，清空密钥和过期列表；恢复旧字段也需要重新查询。
function invalidateModels(message = '', needsReload = false) {
  const hadResult = modelsBusy.value || !!modelsResult.value
  modelsController?.abort(); modelsController = null
  modelApplyController?.abort(); modelApplyController = null; modelsApplying.value = false
  modelsBusy.value = false; modelKey.value = ''; modelsResult.value = null
  modelsError.value = ''; selectedModel.value = ''; modelFilter.value = ''
  modelsNotice.value = hadResult ? message : ''
  modelsNeedsReload.value = needsReload
}

// discoverModels 只使用已保存的 Provider；响应必须与当前保存快照一致才能用于配置路由。
async function discoverModels() {
  if (!canQueryModels.value || loading.value || modelsBusy.value || modelsApplying.value || busy.value) return
  modelsController?.abort()
  const queryController = new AbortController()
  modelsController = queryController
  modelsBusy.value = true; modelsResult.value = null; modelsError.value = modelsNotice.value = ''; selectedModel.value = ''; modelsNeedsReload.value = false
  const providerID = modelProvider.value
  const key = modelKey.value
  modelKey.value = ''
  try {
    const result = await requestJSON<ProviderModelsResult>('/api/provider-models', 'POST', { provider_id: providerID, ...(key ? { api_key: key } : {}) }, queryController.signal)
    if (queryController.signal.aborted || modelsController !== queryController) return
    if (!modelsResultIsCurrent(result, selectedDraftProvider.value, selectedSavedProvider.value)) {
      modelsError.value = '查询使用的 Provider 与当前配置不一致，请重新加载配置后查询。'
      modelsNeedsReload.value = true
      return
    }
    modelsResult.value = result
    selectedModel.value = result.models[0]?.id || ''
  } catch (e) {
    if (!queryController.signal.aborted) {
      modelsError.value = String(e)
      modelsNeedsReload.value = e instanceof APIError && (e.status === 409 || e.status === 404)
    }
  }
  finally {
    if (modelsController === queryController) { modelsBusy.value = false; modelsController = null }
  }
}

// useDiscoveredModel 填入路由前再次读取保存配置，防止查询完成后其他标签页改动上游；仅更新草稿，不执行模型。
async function useDiscoveredModel() {
  if (!canUseModel.value || !selectedDraftProvider.value) return
  const boundResult = modelsResult.value
  const model = boundResult?.models.find(item => item.id === selectedModel.value)
  if (!model) return
  const applyController = new AbortController()
  modelApplyController = applyController; modelsApplying.value = true; modelsError.value = ''
  try {
    const current = await requestJSON<{ config: ProviderConfig }>('/api/providers', 'GET', undefined, applyController.signal)
    if (applyController.signal.aborted || modelsResult.value !== boundResult || !selectedDraftProvider.value) return
    if (!modelsResultIsCurrent(boundResult, selectedDraftProvider.value, current.config.providers.find(p => p.id === boundResult?.provider_id))) {
      invalidateModels('已保存的 Provider 配置已变化，原模型列表已失效。请重新加载配置后查询。', true)
      return
    }
    const index = config.value.routes.findIndex(route => route.id === routeTarget.value)
    const existing = index >= 0 ? config.value.routes[index] : undefined
    const route = routeFromProviderModel(model, selectedDraftProvider.value, config.value, existing)
    const removedFallbacks = (existing?.failover?.length || 0) - (route.failover?.length || 0)
    if (index >= 0) config.value.routes[index] = route
    else config.value.routes.push(route)
    routeTarget.value = route.id
    modelsNotice.value = `已${existing ? '填入' : '创建'}路由 ${route.id}，请保存 Provider 与路由后生效。${removedFallbacks ? `已移除 ${removedFallbacks} 个不兼容的备用 Provider。` : ''}`
    document.getElementById('provider-route-editor')?.scrollIntoView({ block: 'nearest' })
  } catch (e) { if (!applyController.signal.aborted) modelsError.value = String(e) }
  finally { if (modelApplyController === applyController) { modelsApplying.value = false; modelApplyController = null } }
}

// retrieve 的临时密钥与结果只属于这次配置快照，切换或编辑 Provider 后立即失效。
async function retrieve() {
  if (!canQueryHistory.value || loading.value || busy.value || historyBusy.value) return
  const snapshot = savedConfig.value.providers.find(p => p.id === historyProvider.value)
  if (!snapshot) return
  const queryController = new AbortController()
  historyController = queryController
  historyBusy.value = true; historyError.value = ''; historyResult.value = null
  const key = historyKey.value
  historyKey.value = ''
  try {
    const result = await requestJSON<{ status_code: number; body: string; response_id: string }>('/api/provider-history', 'POST', { provider_id: snapshot.id, response_id: responseId.value.trim(), ...(key ? { api_key: key } : {}) }, queryController.signal)
    if (!queryController.signal.aborted && historyController === queryController) historyResult.value = result
  }
  catch (e) { if (!queryController.signal.aborted) historyError.value = String(e) }
  finally { if (historyController === queryController) { historyBusy.value = false; historyController = null } }
}

watch([() => modelProvider.value, () => providerFingerprint(selectedDraftProvider.value), () => providerFingerprint(selectedSavedProvider.value)], () => invalidateModels('Provider 配置已变化，原模型列表已失效，请保存后重新查询。'), { flush: 'sync' })
watch([() => historyProvider.value, () => responseId.value, () => providerFingerprint(config.value.providers.find(p => p.id === historyProvider.value)), () => providerFingerprint(savedConfig.value.providers.find(p => p.id === historyProvider.value))], () => {
  historyController?.abort(); historyController = null; historyBusy.value = false
  historyKey.value = ''; historyResult.value = null; historyError.value = ''
}, { flush: 'sync' })
watch(listedModels, value => { if (!value.items.some(model => model.id === selectedModel.value)) selectedModel.value = value.items[0]?.id || '' })
watch(() => config.value.routes.map(route => route.id), ids => { if (routeTarget.value && !ids.includes(routeTarget.value)) routeTarget.value = '' })
onMounted(() => { void load() })
onUnmounted(() => { controller.abort(); modelsController?.abort(); modelApplyController?.abort(); historyController?.abort(); modelKey.value = historyKey.value = '' })
</script>

<template>
  <section class="min-w-0 space-y-5" aria-label="Provider 与路由">
    <div class="space-y-2"><h2 class="text-sm font-semibold">Provider 与模型路由</h2><p class="text-xs leading-relaxed text-muted-foreground">选择接入类型、填写实例地址并保存，再查询模型并设置路由。未命中路由时使用默认上游：<code class="break-all">{{ defaultTarget || '加载中…' }}</code>。</p></div>
    <p v-if="error" role="alert" class="break-words text-xs text-destructive">{{ error }}</p><p v-if="notice" role="status" class="text-xs text-teal-800">{{ notice }}</p>
    <p v-if="loading" role="status" class="text-xs text-muted-foreground">正在加载已保存配置…</p>
    <p v-if="presetError" role="alert" class="text-xs text-destructive">{{ presetError }}</p>
    <Button v-if="error || presetError" type="button" size="sm" variant="outline" :disabled="loading || busy || modelsApplying" @click="load">{{ loading ? '加载中…' : '重新加载配置与模板' }}</Button>
    <form v-if="ready" class="space-y-5" @submit.prevent="save">
      <fieldset :disabled="loading || busy || modelsApplying" class="min-w-0 space-y-5">
      <div class="space-y-3">
        <div class="flex flex-wrap items-end justify-between gap-3">
          <h3 class="text-xs font-semibold">上游配置</h3>
          <div class="flex min-w-0 flex-wrap items-end gap-2">
            <label class="min-w-0 space-y-1 text-xs"><span>新增接入类型</span><select v-model="presetChoice" :class="fieldClass" aria-label="新增接入类型"><option v-for="profile in providerProfiles" :key="profile.id" :value="profile.id" :disabled="profile.id !== 'custom' && !presetFor(profile.id)">{{ profile.name }}</option></select></label>
            <Button size="sm" variant="outline" type="button" :disabled="config.providers.length >= 64" @click="addProvider">添加 Provider</Button>
          </div>
        </div>
        <p v-if="!config.providers.length" class="rounded-lg border border-dashed p-4 text-xs text-muted-foreground">当前仅使用启动参数中的默认上游。添加 Provider 后可按模型指定目的地。</p>
        <fieldset v-for="(p, index) in config.providers" :key="index" class="min-w-0 space-y-3 rounded-lg border p-3">
          <legend class="max-w-full break-all px-1 text-xs font-semibold">{{ p.name || p.id || `Provider ${index + 1}` }}</legend>
          <div class="grid min-w-0 gap-3 sm:grid-cols-2">
            <label class="min-w-0 space-y-1 text-xs sm:col-span-2"><span>接入类型</span><select v-model="p.profile" :class="fieldClass" aria-label="Provider 接入类型"><option v-for="profile in providerProfiles" :key="profile.id" :value="profile.id">{{ profile.name }}</option></select></label>
            <label class="min-w-0 space-y-1 text-xs"><span>Provider ID</span><Input v-model="p.id" required maxlength="100" aria-label="Provider ID" /></label>
            <label class="min-w-0 space-y-1 text-xs"><span>显示名称</span><Input v-model="p.name" aria-label="Provider 显示名称" /></label>
            <label class="min-w-0 space-y-1 text-xs sm:col-span-2"><span>Base URL</span><Input v-model="p.base_url" type="url" required :placeholder="presetFor(p.profile)?.base_url_placeholder || 'https://api.example.com/v1'" aria-label="Provider Base URL" /></label>
            <label class="min-w-0 space-y-1 text-xs"><span>协议模式</span><select v-model="p.protocol" :class="fieldClass" aria-label="Provider 协议模式"><option value="passthrough">原生透传</option><option value="openai">转为 Chat Completions</option></select></label>
            <label class="min-w-0 space-y-1 text-xs"><span>密钥环境变量名</span><Input v-model="p.key_env" placeholder="LLM_PROVIDER_KEY（留空使用客户端鉴权）" autocomplete="off" aria-label="密钥环境变量名" /></label>
            <label class="min-w-0 space-y-1 text-xs"><span>鉴权请求头</span><select v-model="p.auth_header" :class="fieldClass" aria-label="鉴权请求头"><option value="Authorization">Authorization</option><option value="X-API-Key">X-API-Key</option><option value="api-key">api-key</option></select></label>
            <label class="min-w-0 space-y-1 text-xs"><span>鉴权前缀</span><Input v-model="p.auth_scheme" placeholder="Authorization 默认 Bearer" aria-label="鉴权前缀" /></label>
          </div>
          <div v-if="presetFor(p.profile)" class="space-y-1 rounded-md bg-muted/50 p-2 text-[11px] leading-relaxed text-muted-foreground">
            <p>{{ presetFor(p.profile)?.description }}</p>
            <p v-for="note in presetFor(p.profile)?.notes" :key="note">{{ note }}</p>
          </div>
          <fieldset v-if="p.capabilities" class="min-w-0 space-y-2 border-t pt-3">
            <legend class="text-xs font-semibold">实例接口能力声明</legend>
            <p class="text-[11px] leading-relaxed text-muted-foreground">按实例的实际能力填写。未知允许继续透传，明确不支持会阻止对应接口请求；接入类型和查询模型成功都不等于完成验证。</p>
            <div class="grid min-w-0 gap-3 sm:grid-cols-2">
              <label v-for="capability in providerCapabilities" :key="capability.key" class="min-w-0 space-y-1 text-xs"><span>{{ capability.label }}</span><select v-model="p.capabilities[capability.key]" :class="fieldClass" :aria-label="`${capability.label} 能力声明`"><option value="unknown">未知 · 保留透传</option><option value="supported">支持 · 操作者声明</option><option value="unsupported">不支持 · 阻止该接口</option></select></label>
            </div>
            <p v-if="p.protocol === 'openai'" class="text-[11px] leading-relaxed text-muted-foreground">转换模式按实际发送的 Chat Completions 检查能力。Responses / Messages 原生声明不代替转换后的 Chat 能力。</p>
          </fieldset>
          <div class="flex flex-wrap items-center gap-4 text-xs"><label class="flex items-center gap-2"><input v-model="p.websocket" type="checkbox" />支持 Responses WebSocket</label><label class="flex items-center gap-2"><input v-model="p.history" type="checkbox" />支持按 ID 查询已保存 Response</label><Button variant="ghost" size="sm" type="button" class="ml-auto text-destructive" @click="config.providers.splice(index, 1)">移除 Provider</Button></div>
        </fieldset>
        <p class="text-[11px] leading-relaxed text-muted-foreground">密钥在网关进程的环境变量中设置；这里仅保存变量名。地址支持 origin、/v1 和挂载前缀。切换已有配置的接入类型只更换说明；请按实例调整地址和鉴权。保存配置不会发起模型调用。</p>
      </div>
      <div id="provider-route-editor" class="space-y-3 scroll-mt-4">
        <div class="flex flex-wrap items-center justify-between gap-2"><h3 class="text-xs font-semibold">模型路由</h3><Button size="sm" variant="outline" type="button" :disabled="!config.providers.length" @click="addRoute">添加路由</Button></div>
        <p class="text-[11px] text-muted-foreground">优先级大的先匹配，同优先级按列表顺序。模型名支持精确匹配或末尾 *；路由在请求进入时确定。</p>
        <fieldset v-for="(route, index) in config.routes" :key="index" class="min-w-0 space-y-3 rounded-lg border p-3">
          <legend class="max-w-full break-all px-1 text-xs font-semibold">{{ route.id }}</legend>
          <div class="grid min-w-0 gap-3 sm:grid-cols-2">
            <label class="space-y-1 text-xs"><span>路由 ID</span><Input v-model="route.id" required aria-label="路由 ID" /></label>
            <label class="space-y-1 text-xs"><span>匹配模型</span><Input v-model="route.model" required placeholder="glm-*" aria-label="匹配模型" /></label>
            <label class="space-y-1 text-xs"><span>Provider</span><select v-model="route.provider_id" required :class="fieldClass" aria-label="路由 Provider"><option v-for="p in config.providers" :key="p.id" :value="p.id">{{ p.name || p.id }}</option></select></label>
            <label class="space-y-1 text-xs"><span>实际模型名（可选）</span><Input v-model="route.target_model" placeholder="留空保留请求中的模型名" aria-label="实际模型名" /></label>
            <label class="space-y-1 text-xs"><span>优先级</span><Input v-model.number="route.priority" type="number" step="1" aria-label="路由优先级" /></label>
            <label class="space-y-1 text-xs"><span>备用 Provider ID（最多 3 个）</span><Input :model-value="route.failover?.join(', ')" placeholder="provider-2, provider-3" aria-label="备用 Provider ID" @input="setFallback(index, $event)" /></label>
          </div>
          <div class="flex flex-wrap items-center justify-between gap-3"><label class="flex items-center gap-2 text-xs"><input v-model="route.disabled" type="checkbox" />暂停此路由</label><Button type="button" size="sm" variant="ghost" class="text-destructive" @click="config.routes.splice(index, 1)">移除路由</Button></div>
        </fieldset>
        <p class="text-[11px] leading-relaxed text-muted-foreground">HTTP 仅在连接错误或 502 / 503 / 504、且未输出响应时尝试显式配置的备用上游。备用请求也可能产生 Provider 费用。WebSocket 连接固定使用首个 Provider，不自动补发。</p>
      </div>
      <div class="flex flex-wrap items-center gap-3"><Button type="submit" :disabled="busy">{{ busy ? '保存中…' : '保存 Provider 与路由' }}</Button><span v-if="hasChanges" class="text-xs text-amber-700">有未保存的修改</span></div>
      </fieldset>
    </form>
    <section class="min-w-0 space-y-3 rounded-lg border p-3" aria-label="Provider 模型发现">
      <h3 class="text-sm font-semibold">查询模型并配置路由</h3>
      <p class="text-xs leading-relaxed text-muted-foreground">仅对已保存的 Provider 查询模型列表；不执行模型、不创建调用记录。列表仅代表本次接口返回，模型实际可调用性、工具调用和其他接口需另行验证。</p>
      <p v-if="!savedConfig.providers.length" class="text-xs text-muted-foreground">先添加并保存一个 Provider，再在这里选择模型。</p>
      <form v-else class="grid min-w-0 gap-3 sm:grid-cols-2" @submit.prevent="discoverModels">
        <label class="min-w-0 space-y-1 text-xs"><span>查询 Provider</span><select v-model="modelProvider" required :disabled="loading || busy || modelsBusy || modelsApplying" :class="fieldClass" aria-label="模型查询 Provider"><option disabled value="">选择已保存的 Provider</option><option v-for="p in savedConfig.providers" :key="p.id" :value="p.id">{{ p.name || p.id }}</option></select></label>
        <label class="min-w-0 space-y-1 text-xs"><span>仅本次查询的密钥（可选）</span><Input v-model="modelKey" type="password" autocomplete="off" :disabled="loading || busy || modelsBusy || !canQueryModels" placeholder="留空使用已配置的环境变量" aria-label="本次模型查询密钥" /></label>
        <p v-if="modelProvider && !canQueryModels" class="text-xs text-amber-700 sm:col-span-2" role="status">{{ selectedSavedProvider?.capabilities?.models === 'unsupported' ? '此实例声明不支持模型列表；请调整能力声明并保存后再查询。' : '此 Provider 的配置尚未保存或已移除，请先保存后查询。' }}</p>
        <div class="flex flex-wrap items-center gap-2 sm:col-span-2"><Button type="submit" variant="outline" :disabled="loading || modelsBusy || modelsApplying || !canQueryModels || busy">{{ modelsBusy ? '查询中…' : '查询模型列表' }}</Button><Button v-if="modelsBusy" type="button" variant="ghost" @click="invalidateModels('已取消模型查询。')">取消查询</Button></div>
      </form>
      <p v-if="modelsError" role="alert" class="break-words text-xs text-destructive">{{ modelsError }}</p>
      <p v-if="modelsNotice" role="status" class="break-words text-xs text-teal-800">{{ modelsNotice }}</p>
      <Button v-if="modelsNeedsReload" type="button" size="sm" variant="outline" :disabled="loading || busy || modelsApplying" @click="load">重新加载已保存配置</Button>
      <div v-if="currentModels && modelsResult" class="min-w-0 space-y-3 border-t pt-3">
        <p class="break-words text-xs text-muted-foreground">{{ modelsResult.provider.name || modelsResult.provider_id }} · HTTP {{ modelsResult.status_code }} · {{ new Date(modelsResult.checked_at).toLocaleString() }}</p>
        <p v-if="modelsResult.error" role="alert" class="text-xs text-destructive">{{ modelsResult.error }}</p>
        <template v-else>
          <p v-if="!modelsResult.models.length" class="text-xs text-muted-foreground">Provider 返回了空模型列表；请检查实例的模型配置与令牌可见范围。</p>
          <template v-else>
            <label class="block min-w-0 space-y-1 text-xs"><span>搜索模型（{{ modelsResult.models.length }} 个）</span><Input v-model="modelFilter" :disabled="modelsApplying" placeholder="模型名称或所属方" aria-label="搜索 Provider 模型" /></label>
            <label class="block min-w-0 space-y-1 text-xs"><span>选择模型</span><select v-model="selectedModel" :disabled="modelsApplying" size="5" class="h-32 w-full min-w-0 rounded-md border bg-background p-1 text-xs" aria-label="发现的模型"><option v-for="model in listedModels.items" :key="model.id" :value="model.id">{{ model.id }}{{ model.owned_by ? ` · ${model.owned_by}` : '' }}</option></select></label>
            <p v-if="listedModels.matched > listedModels.items.length" class="text-[11px] text-muted-foreground">匹配 {{ listedModels.matched }} 个，当前显示前 {{ listedModels.items.length }} 个；输入更具体的名称可查找其余模型。</p>
            <p v-if="!listedModels.matched" class="text-xs text-muted-foreground">没有匹配的模型。</p>
            <p v-if="selectedModel" class="break-all text-[11px] text-muted-foreground">实际模型名：{{ selectedModel }}</p>
            <div class="grid min-w-0 gap-3 sm:grid-cols-2">
              <label class="min-w-0 space-y-1 text-xs"><span>应用到路由草稿</span><select v-model="routeTarget" :disabled="modelsApplying" :class="fieldClass" aria-label="模型应用目标路由"><option value="">新建模型路由</option><option v-for="route in config.routes" :key="route.id" :value="route.id">{{ route.id }} · {{ route.model || '未设置匹配模型' }}</option></select></label>
              <div class="flex items-end"><Button type="button" variant="outline" :disabled="!canUseModel" @click="useDiscoveredModel">{{ modelsApplying ? '核对保存配置…' : routeTarget ? '填入所选路由' : '使用模型新建路由' }}</Button></div>
            </div>
            <p class="text-[11px] leading-relaxed text-muted-foreground">填入已有路由会保留匹配模型别名和优先级；切换 Provider 时清理不兼容的备用项。修改后需要保存才生效。</p>
          </template>
        </template>
      </div>
    </section>
    <details class="rounded-lg border"><summary class="cursor-pointer p-3 text-xs font-semibold">协议与 WebSocket 支持范围</summary><div class="space-y-2 border-t p-3 text-xs leading-relaxed text-muted-foreground"><p>原生透传保留 HTTP 请求和响应的协议。转换模式支持 Anthropic Messages / Responses 的独立文本、函数工具调用和结果，返回 JSON 或 SSE。服务端历史、内置工具、多模态和签名推理需要原生透传；转换遇到不支持的字段会明确报错。</p><p>Responses WebSocket 按首个 response.create 的模型选择 Provider，同一连接内可以使用多个 stream_id。每个 lane 按顺序关联请求，lane 名本身不构成父子关系。断开时未结束的调用标记中断；帧可以下载，断点编辑与单次重放使用 HTTP 接口。</p></div></details>
    <section class="space-y-3 border-t pt-4" aria-label="Provider 历史查询">
      <h3 class="text-sm font-semibold">查询已保存的 Response</h3><p class="text-xs leading-relaxed text-muted-foreground">输入 Provider 实际生成的 Response ID，读取其公开的保存记录。查询不会创建模型请求，也不会导入为新的调用。请先保存上方配置。</p>
      <form class="grid gap-3 sm:grid-cols-2" @submit.prevent="retrieve">
        <label class="space-y-1 text-xs"><span>历史 Provider</span><select v-model="historyProvider" required :class="fieldClass" aria-label="历史 Provider"><option disabled value="">选择已启用历史能力的 Provider</option><option v-for="p in historyProviders" :key="p.id" :value="p.id">{{ p.name || p.id }}</option></select></label>
        <label class="space-y-1 text-xs"><span>Response ID</span><Input v-model="responseId" required placeholder="resp_…" aria-label="历史 Response ID" /></label>
        <label class="space-y-1 text-xs sm:col-span-2"><span>本次查询密钥（已配置环境变量时可留空）</span><Input v-model="historyKey" type="password" autocomplete="off" aria-label="本次历史查询密钥" /></label>
        <p v-if="historyProvider && !canQueryHistory" class="text-xs text-amber-700 sm:col-span-2">此 Provider 的配置尚未保存，请保存后再查询历史。</p>
        <div><Button type="submit" variant="outline" :disabled="loading || busy || historyBusy || !canQueryHistory || !responseId.trim()">{{ historyBusy ? '查询中…' : '查询 Provider 历史' }}</Button></div>
      </form>
      <p v-if="historyError" role="alert" class="text-xs text-destructive">{{ historyError }}</p>
      <div v-if="historyResult" class="space-y-2 rounded-lg border p-3"><p class="text-xs" :class="historyResult.status_code >= 400 ? 'text-destructive' : 'text-muted-foreground'">Provider 返回 HTTP {{ historyResult.status_code }} · {{ historyResult.response_id }}</p><pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] [overflow-wrap:anywhere]">{{ formatBody(historyResult.body) }}</pre></div>
    </section>
  </section>
</template>
