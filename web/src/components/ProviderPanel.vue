<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { requestJSON } from '@/lib/api'
import { formatBody } from '@/lib/correlation'
import type { ProviderConfig } from '@/lib/types'

const config = ref<ProviderConfig>({ providers: [], routes: [] })
const defaultTarget = ref('')
const error = ref('')
const notice = ref('')
const busy = ref(false)
const ready = ref(false)
const controller = new AbortController()
const historyProvider = ref('')
const responseId = ref('')
const historyKey = ref('')
const historyBusy = ref(false)
const historyError = ref('')
const historyResult = ref<{ status_code: number; body: string; response_id: string } | null>(null)
const historyProviders = computed(() => config.value.providers.filter(p => p.history))
const fieldClass = 'h-9 w-full min-w-0 rounded-md border bg-background px-2 text-xs'

function displayConfig(value: ProviderConfig): ProviderConfig {
  const headers: Record<string, string> = { authorization: 'Authorization', 'x-api-key': 'X-API-Key', 'api-key': 'api-key' }
  return {
    providers: value.providers.map(p => ({ ...p, auth_header: headers[p.auth_header?.toLowerCase() || 'authorization'] || 'Authorization' })),
    routes: value.routes,
  }
}
async function load() {
  try {
    const result = await requestJSON<{ config: ProviderConfig; default_target: string }>('/api/providers', 'GET', undefined, controller.signal)
    config.value = displayConfig(result.config); defaultTarget.value = result.default_target; ready.value = true
  } catch (e) { if (!controller.signal.aborted) error.value = String(e) }
}
function nextId(prefix: string, values: { id: string }[]) { let index = 1; while (values.some(p => p.id === `${prefix}-${index}`)) index++; return `${prefix}-${index}` }
function addProvider() { config.value.providers.push({ id: nextId('provider', config.value.providers), name: '', base_url: '', protocol: 'passthrough', history: false, websocket: false, key_env: '', auth_header: 'Authorization', auth_scheme: '' }) }
function addRoute() { config.value.routes.push({ id: nextId('route', config.value.routes), model: '', provider_id: config.value.providers[0]?.id ?? '', target_model: '', priority: 0, disabled: false, failover: [] }) }
function setFallback(index: number, event: Event) { const route = config.value.routes[index]; if (route) route.failover = (event.target as HTMLInputElement).value.split(',').map(v => v.trim()).filter(Boolean) }
async function save() {
  busy.value = true; error.value = notice.value = ''
  try { const result = await requestJSON<{ config: ProviderConfig }>('/api/providers', 'PUT', config.value); config.value = displayConfig(result.config); notice.value = '已保存。后续请求使用新配置，等待中的请求及已有 WebSocket 连接保持原目的地。' }
  catch (e) { error.value = String(e) }
  finally { busy.value = false }
}
async function retrieve() {
  historyBusy.value = true; historyError.value = ''; historyResult.value = null
  const key = historyKey.value
  historyKey.value = ''
  try { historyResult.value = await requestJSON('/api/provider-history', 'POST', { provider_id: historyProvider.value, response_id: responseId.value.trim(), ...(key ? { api_key: key } : {}) }, controller.signal) }
  catch (e) { if (!controller.signal.aborted) historyError.value = String(e) }
  finally { historyBusy.value = false }
}
onMounted(() => { void load() })
onUnmounted(() => { controller.abort(); historyKey.value = '' })
</script>

<template>
  <section class="min-w-0 space-y-5" aria-label="Provider 与路由">
    <div class="space-y-2"><h2 class="text-sm font-semibold">Provider 与模型路由</h2><p class="text-xs leading-relaxed text-muted-foreground">未命中路由时使用默认上游：<code class="break-all">{{ defaultTarget || '加载中…' }}</code>。地址支持包含 /v1 的 Base URL。</p></div>
    <p v-if="error" role="alert" class="break-words text-xs text-destructive">{{ error }}</p><p v-if="notice" role="status" class="text-xs text-teal-800">{{ notice }}</p>
    <form v-if="ready" class="space-y-5" @submit.prevent="save">
      <div class="space-y-3">
        <div class="flex flex-wrap items-center justify-between gap-2"><h3 class="text-xs font-semibold">上游配置</h3><Button size="sm" variant="outline" type="button" @click="addProvider">添加 Provider</Button></div>
        <p v-if="!config.providers.length" class="rounded-lg border border-dashed p-4 text-xs text-muted-foreground">当前仅使用启动参数中的默认上游。添加 Provider 后可按模型指定目的地。</p>
        <fieldset v-for="(p, index) in config.providers" :key="index" class="min-w-0 space-y-3 rounded-lg border p-3">
          <legend class="px-1 text-xs font-semibold">{{ p.name || p.id || `Provider ${index + 1}` }}</legend>
          <div class="grid min-w-0 gap-3 sm:grid-cols-2">
            <label class="min-w-0 space-y-1 text-xs"><span>Provider ID</span><Input v-model="p.id" required maxlength="100" aria-label="Provider ID" /></label>
            <label class="min-w-0 space-y-1 text-xs"><span>显示名称</span><Input v-model="p.name" aria-label="Provider 显示名称" /></label>
            <label class="min-w-0 space-y-1 text-xs sm:col-span-2"><span>Base URL</span><Input v-model="p.base_url" type="url" required placeholder="https://api.example.com/v1" aria-label="Provider Base URL" /></label>
            <label class="min-w-0 space-y-1 text-xs"><span>协议模式</span><select v-model="p.protocol" :class="fieldClass" aria-label="Provider 协议模式"><option value="passthrough">原生透传</option><option value="openai">转为 Chat Completions</option></select></label>
            <label class="min-w-0 space-y-1 text-xs"><span>密钥环境变量名</span><Input v-model="p.key_env" placeholder="LLM_PROVIDER_KEY（留空使用客户端鉴权）" autocomplete="off" aria-label="密钥环境变量名" /></label>
            <label class="min-w-0 space-y-1 text-xs"><span>鉴权请求头</span><select v-model="p.auth_header" :class="fieldClass" aria-label="鉴权请求头"><option value="Authorization">Authorization</option><option value="X-API-Key">X-API-Key</option><option value="api-key">api-key</option></select></label>
            <label class="min-w-0 space-y-1 text-xs"><span>鉴权前缀</span><Input v-model="p.auth_scheme" placeholder="Authorization 默认 Bearer" aria-label="鉴权前缀" /></label>
          </div>
          <div class="flex flex-wrap items-center gap-4 text-xs"><label class="flex items-center gap-2"><input v-model="p.websocket" type="checkbox" />支持 Responses WebSocket</label><label class="flex items-center gap-2"><input v-model="p.history" type="checkbox" />支持按 ID 查询已保存 Response</label><Button variant="ghost" size="sm" type="button" class="ml-auto text-destructive" @click="config.providers.splice(index, 1)">移除 Provider</Button></div>
        </fieldset>
        <p class="text-[11px] leading-relaxed text-muted-foreground">密钥在网关进程的环境变量中设置；这里仅保存变量名。能力开关由 Provider 的实际接口决定，保存配置不会发起模型调用。</p>
      </div>
      <div class="space-y-3">
        <div class="flex flex-wrap items-center justify-between gap-2"><h3 class="text-xs font-semibold">模型路由</h3><Button size="sm" variant="outline" type="button" :disabled="!config.providers.length" @click="addRoute">添加路由</Button></div>
        <p class="text-[11px] text-muted-foreground">优先级大的先匹配，同优先级按列表顺序。模型名支持精确匹配或末尾 *；路由在请求进入时确定。</p>
        <fieldset v-for="(route, index) in config.routes" :key="index" class="min-w-0 space-y-3 rounded-lg border p-3">
          <legend class="px-1 text-xs font-semibold">{{ route.id }}</legend>
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
      <Button type="submit" :disabled="busy">{{ busy ? '保存中…' : '保存 Provider 与路由' }}</Button>
    </form>
    <details class="rounded-lg border"><summary class="cursor-pointer p-3 text-xs font-semibold">协议与 WebSocket 支持范围</summary><div class="space-y-2 border-t p-3 text-xs leading-relaxed text-muted-foreground"><p>原生透传保留 HTTP 请求和响应的协议。转换模式支持 Anthropic Messages / Responses 的独立文本、函数工具调用和结果，返回 JSON 或 SSE。服务端历史、内置工具、多模态和签名推理需要原生透传；转换遇到不支持的字段会明确报错。</p><p>Responses WebSocket 按首个 response.create 的模型选择 Provider，同一连接内可以使用多个 stream_id。每个 lane 按顺序关联请求，lane 名本身不构成父子关系。断开时未结束的调用标记中断；帧可以下载，断点编辑与单次重放使用 HTTP 接口。</p></div></details>
    <section class="space-y-3 border-t pt-4" aria-label="Provider 历史查询">
      <h3 class="text-sm font-semibold">查询已保存的 Response</h3><p class="text-xs leading-relaxed text-muted-foreground">输入 Provider 实际生成的 Response ID，读取其公开的保存记录。查询不会创建模型请求，也不会导入为新的调用。请先保存上方配置。</p>
      <form class="grid gap-3 sm:grid-cols-2" @submit.prevent="retrieve">
        <label class="space-y-1 text-xs"><span>历史 Provider</span><select v-model="historyProvider" required :class="fieldClass" aria-label="历史 Provider"><option disabled value="">选择已启用历史能力的 Provider</option><option v-for="p in historyProviders" :key="p.id" :value="p.id">{{ p.name || p.id }}</option></select></label>
        <label class="space-y-1 text-xs"><span>Response ID</span><Input v-model="responseId" required placeholder="resp_…" aria-label="历史 Response ID" /></label>
        <label class="space-y-1 text-xs sm:col-span-2"><span>本次查询密钥（已配置环境变量时可留空）</span><Input v-model="historyKey" type="password" autocomplete="off" aria-label="本次历史查询密钥" /></label>
        <div><Button type="submit" variant="outline" :disabled="historyBusy || !historyProvider || !responseId.trim()">{{ historyBusy ? '查询中…' : '查询 Provider 历史' }}</Button></div>
      </form>
      <p v-if="historyError" role="alert" class="text-xs text-destructive">{{ historyError }}</p>
      <div v-if="historyResult" class="space-y-2 rounded-lg border p-3"><p class="text-xs" :class="historyResult.status_code >= 400 ? 'text-destructive' : 'text-muted-foreground'">Provider 返回 HTTP {{ historyResult.status_code }} · {{ historyResult.response_id }}</p><pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words font-mono text-[11px] [overflow-wrap:anywhere]">{{ formatBody(historyResult.body) }}</pre></div>
    </section>
  </section>
</template>
