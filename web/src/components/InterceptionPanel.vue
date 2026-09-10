<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { Clock3Icon, RefreshCwIcon, PauseCircleIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { APIError, editInterception, fetchInterception, fetchInterceptions } from '@/lib/api'
import { formatBody, formatDuration } from '@/lib/correlation'
import type { Interception, InterceptionEdit, InterceptionSummary } from '@/lib/types'
import RequestDiff from './RequestDiff.vue'

const props = defineProps<{ refreshKey: number; connected: boolean; active: boolean }>()
const emit = defineEmits<{ select: [trace: string] }>()
const requests = ref<InterceptionSummary[]>([])
const selectedTrace = ref('')
const detail = ref<Interception | null>(null)
const draftBody = ref('')
const draftHeaders = ref('{}')
const baselineBody = ref('')
const baselineHeaders = ref('{}')
const draftRevision = ref(0)
const loading = ref(false)
const busy = ref(false)
const conflict = ref(false)
const error = ref('')
const notice = ref('')
const now = ref(Date.now())
const clockOffset = ref(0)
const dirty = computed(() => draftBody.value !== baselineBody.value || draftHeaders.value !== baselineHeaders.value)
const remaining = computed(() => detail.value ? Math.max(0, Math.ceil((Date.parse(detail.value.deadline) - now.value - clockOffset.value) / 1000)) : 0)
const pending = computed(() => detail.value?.state === 'pending' && remaining.value > 0)
const canAct = computed(() => pending.value && !loading.value && !busy.value && !conflict.value)
const canEdit = computed(() => canAct.value && !detail.value?.edit_blocked_reason)
const headerPreview = computed(() => {
  try { return parseHeaders() }
  catch { return detail.value?.original_headers ?? {} }
})

type Draft = { body: string; headers: string; baseBody: string; baseHeaders: string; revision: number }
const drafts = new Map<string, Draft>()
let detailController: AbortController | undefined
let listController: AbortController | undefined
let poll: ReturnType<typeof setInterval> | undefined
let tick: ReturnType<typeof setInterval> | undefined
let refreshing = false
let refreshAgain = false
let disposed = false

function rememberDraft() {
  if (!detail.value || detail.value.trace_id !== selectedTrace.value) return
  drafts.set(selectedTrace.value, { body: draftBody.value, headers: draftHeaders.value, baseBody: baselineBody.value, baseHeaders: baselineHeaders.value, revision: draftRevision.value })
}

function acceptDetail(next: Interception, reset: boolean) {
  detail.value = next
  if (reset) {
    draftBody.value = baselineBody.value = formatBody(next.body)
    draftHeaders.value = baselineHeaders.value = JSON.stringify(next.headers, null, 2)
    draftRevision.value = next.revision
    conflict.value = false
    rememberDraft()
  } else {
    conflict.value = next.state === 'pending' && next.revision !== draftRevision.value
    if (next.state !== 'pending' && dirty.value) notice.value = '等待已结束。未保存的草稿没有发送，可以继续查看和复制。'
  }
}

async function loadDetail(trace: string, reset = false) {
  detailController?.abort()
  const controller = detailController = new AbortController()
  const switching = detail.value?.trace_id !== trace
  if (switching) detail.value = null
  loading.value = true
  try {
    const result = await fetchInterception(trace, controller.signal)
    if (controller.signal.aborted || selectedTrace.value !== trace) return
    clockOffset.value = Date.parse(result.server_time) - Date.now()
    const cached = switching && !reset ? drafts.get(trace) : undefined
    if (cached) {
      draftBody.value = cached.body
      draftHeaders.value = cached.headers
      baselineBody.value = cached.baseBody
      baselineHeaders.value = cached.baseHeaders
      draftRevision.value = cached.revision
      acceptDetail(result.request, false)
    } else {
      acceptDetail(result.request, reset || switching || !dirty.value)
    }
  } catch (e) {
    if (!controller.signal.aborted) error.value = String(e)
  } finally {
    if (!controller.signal.aborted) loading.value = false
  }
}

function selectRequest(trace: string) {
  if (trace === selectedTrace.value) return
  rememberDraft()
  selectedTrace.value = trace
  error.value = notice.value = ''
  if (props.active) emit('select', trace)
  void loadDetail(trace)
}

function selectFromMenu(event: Event) {
  selectRequest((event.target as HTMLSelectElement).value)
}

async function refresh() {
  if (disposed) return
  if (refreshing) { refreshAgain = true; return }
  refreshing = true
  const controller = listController = new AbortController()
  try {
    const result = await fetchInterceptions(controller.signal)
    if (controller.signal.aborted) return
    requests.value = result.requests
    clockOffset.value = Date.parse(result.server_time) - Date.now()
    if (!selectedTrace.value && result.requests[0]) selectRequest(result.requests[0].trace_id)
    else if (selectedTrace.value && !busy.value && !loading.value) {
      const fresh = result.requests.find(item => item.trace_id === selectedTrace.value)
      if (!detail.value || (fresh && fresh.revision !== detail.value.revision) || (!fresh && detail.value.state === 'pending')) {
        await loadDetail(selectedTrace.value)
      }
    }
  } catch (e) {
    if (!controller.signal.aborted) error.value = `加载待处理请求失败：${e}`
  } finally {
    refreshing = false
    if (refreshAgain && !disposed) { refreshAgain = false; void refresh() }
  }
}

function parseHeaders(): Record<string, string> {
  const value: unknown = JSON.parse(draftHeaders.value)
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.values(value).some(item => typeof item !== 'string')) {
    throw new Error('请求头必须是名称到字符串值的 JSON 对象。')
  }
  return value as Record<string, string>
}

async function act(action: 'save' | 'validate' | 'release' | 'cancel') {
  if (!canAct.value || !detail.value || ((action === 'save' || action === 'validate') && !canEdit.value)) return
  const trace = detail.value.trace_id
  error.value = notice.value = ''
  busy.value = true
  try {
    const edit: InterceptionEdit = { revision: draftRevision.value }
    if (action !== 'cancel') {
      if (draftBody.value !== baselineBody.value || action === 'validate') edit.body = draftBody.value
      if (draftHeaders.value !== baselineHeaders.value || action === 'validate') edit.headers = parseHeaders()
    }
    const result = await editInterception(trace, action, edit)
    if (selectedTrace.value !== trace || disposed) return
    clockOffset.value = Date.parse(result.server_time) - Date.now()
    if (action === 'validate') notice.value = '结构校验通过；尚未保存或放行。服务端保存的上下文仍由上游校验。'
    else {
      acceptDetail(result.request, true)
      notice.value = action === 'save' ? '已保存。倒计时保持不变。' : action === 'release' ? '已放行。' : '已取消，不会转发。'
    }
  } catch (e) {
    if (selectedTrace.value !== trace || disposed) return
    error.value = e instanceof APIError && e.status === 409 ? '请求已变化或等待已结束。已保留草稿，请查看最新状态。' : String(e)
    if (e instanceof APIError && e.status === 409) await loadDetail(trace)
  } finally {
    busy.value = false
    void refresh()
  }
}

function formatDraft() {
  try { draftBody.value = JSON.stringify(JSON.parse(draftBody.value), null, 2); error.value = '' }
  catch (e) { error.value = `JSON 格式错误：${e}` }
}

function outcome(item: Interception): string {
  if (item.state === 'pending') return remaining.value ? '等待处理' : '等待时间已到，正在同步结果…'
  if (item.reason === 'client_disconnected') return '客户端已断开，请求已取消'
  if (item.reason === 'gateway_restarted') return '网关已重启，请求已中断'
  if (item.reason === 'timeout') return item.state === 'released' ? '已到期转发' : '已到期取消'
  return item.state === 'released' ? '已放行' : '已取消'
}

watch(() => props.refreshKey, () => { void refresh() })
watch(() => props.active, active => { if (active && selectedTrace.value) emit('select', selectedTrace.value) })
onMounted(() => {
  void refresh()
  poll = setInterval(() => { void refresh() }, 2000)
  tick = setInterval(() => { now.value = Date.now() }, 250)
})
onUnmounted(() => {
  disposed = true
  clearInterval(poll)
  clearInterval(tick)
  detailController?.abort()
  listController?.abort()
})
</script>

<template>
  <div class="interception-workbench flex min-h-0 min-w-0 flex-1 flex-col overflow-auto" aria-label="请求拦截工作台">
    <aside class="interception-queue flex shrink-0 flex-col border-b bg-muted/20">
      <div class="flex min-w-0 shrink-0 items-center gap-2 px-3 py-2 text-xs font-semibold"><PauseCircleIcon class="h-4 w-4 shrink-0 text-amber-600" /><span class="shrink-0 whitespace-nowrap">待处理 · {{ requests.length }}</span><select class="queue-selector h-7 min-w-0 flex-1 rounded border bg-background px-1 text-[11px] font-normal" aria-label="选择待处理请求" :value="requests.some(item => item.trace_id === selectedTrace) ? selectedTrace : ''" @change="selectFromMenu"><option value="" disabled>{{ requests.length ? '选择请求' : '暂无请求' }}</option><option v-for="item in requests" :key="item.trace_id" :value="item.trace_id">{{ item.model || item.method }} · {{ item.trace_id.slice(0, 8) }}</option></select><Button variant="ghost" size="icon" class="ml-auto h-7 w-7 shrink-0" aria-label="刷新待处理请求" @click="refresh"><RefreshCwIcon class="h-3.5 w-3.5" /></Button></div>
      <div class="queue-list panel-scroll min-h-0 flex-1 space-y-1 px-2 pb-2" aria-label="待处理请求列表" tabindex="0">
        <button v-for="item in requests" :key="item.trace_id" type="button" class="block w-full min-w-0 rounded-md border p-2 text-left text-xs" :class="selectedTrace === item.trace_id ? 'border-amber-400 bg-amber-50' : 'bg-card hover:bg-accent'" :aria-pressed="selectedTrace === item.trace_id" @click="selectRequest(item.trace_id)">
          <span class="block truncate font-semibold">{{ item.model || item.method }}</span><span class="mt-1 block truncate font-mono text-[10px] text-muted-foreground">{{ item.path }} · {{ item.trace_id.slice(0, 8) }}</span>
        </button>
        <p v-if="!requests.length" class="px-1 py-2 text-xs leading-relaxed text-muted-foreground">暂无待处理请求。启用拦截规则后，匹配的请求会在这里等待。</p>
      </div>
    </aside>
    <div class="flex min-h-[220px] min-w-0 flex-1 flex-col">
      <div v-if="!detail" class="panel-scroll flex-1 space-y-3 p-6 text-sm text-muted-foreground"><p>{{ loading ? '正在加载完整请求…' : '选择一个待处理请求，编辑后保存、放行或取消。' }}</p><p v-if="error" role="alert" class="text-destructive">{{ error }}</p></div>
      <template v-else>
        <div class="flex shrink-0 flex-wrap items-center gap-2 border-b bg-card px-3 py-2">
          <Clock3Icon class="h-4 w-4 text-amber-600" /><span class="text-xs font-semibold">{{ outcome(detail) }}</span>
          <span v-if="detail.state === 'pending'" role="timer" aria-label="转发倒计时" class="rounded bg-amber-100 px-2 py-1 font-mono text-sm font-semibold text-amber-900">{{ remaining }}s</span>
          <span v-else class="text-[11px] text-muted-foreground">等待 {{ formatDuration(detail.wait_duration_ms) }}</span>
          <span class="ml-auto break-all font-mono text-[10px] text-muted-foreground">{{ detail.trace_id.slice(0, 8) }}</span>
        </div>
        <div class="panel-scroll min-h-0 min-w-0 flex-1 space-y-3 p-3 sm:p-4" aria-label="拦截请求编辑区" tabindex="0">
          <p class="text-xs leading-relaxed text-muted-foreground">{{ detail.timeout_action === 'forward' ? '到期自动转发已保存内容。未保存的草稿不会发送。' : '到期自动取消。请在倒计时结束前放行。' }} 保存不会延长等待时间。</p>
          <p v-if="!connected" class="text-xs text-amber-800">实时连接正在重连，列表仍会定时刷新。</p>
          <p v-if="detail.edit_blocked_reason" class="rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">{{ detail.edit_blocked_reason }}</p>
          <p v-if="error" role="alert" class="rounded-md border border-red-200 bg-red-50 p-3 text-xs text-red-900">{{ error }}</p>
          <p v-if="notice" role="status" class="rounded-md border border-teal-200 bg-teal-50 p-3 text-xs text-teal-900">{{ notice }}</p>
          <div v-if="conflict" class="space-y-2 rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-950"><p>其他操作已保存新版本。当前草稿保留，需要加载最新版本后继续。</p><Button size="sm" variant="outline" @click="loadDetail(selectedTrace, true)">加载最新版本（覆盖草稿）</Button></div>
          <div class="space-y-2">
            <div class="flex flex-wrap items-center gap-2"><label for="intercept-body" class="text-xs font-semibold">本次请求正文</label><span class="text-[10px] text-muted-foreground">{{ dirty ? '有未保存修改' : '已保存的内容' }}</span><Button variant="ghost" size="sm" class="ml-auto h-7 text-xs" :disabled="!canEdit" @click="formatDraft">格式化 JSON</Button></div>
            <Textarea id="intercept-body" v-model="draftBody" :readonly="!canEdit" spellcheck="false" rows="12" class="min-h-40 resize-y font-mono text-xs leading-relaxed" />
          </div>
          <details class="rounded-lg border bg-card"><summary class="cursor-pointer px-3 py-2 text-xs font-semibold">编辑允许的请求头</summary><div class="space-y-2 border-t p-3"><p class="text-[11px] leading-relaxed text-muted-foreground">可编辑 Content-Type、Accept、Anthropic-Version、Anthropic-Beta 和 OpenAI-Beta。鉴权与会话标识保留原值。</p><label for="intercept-headers" class="sr-only">本次请求头</label><Textarea id="intercept-headers" v-model="draftHeaders" :readonly="!canEdit" spellcheck="false" rows="5" class="font-mono text-xs" /></div></details>
          <RequestDiff :original-body="detail.original_body" :modified-body="draftBody" :original-headers="detail.original_headers" :modified-headers="headerPreview" />
          <details class="rounded-lg border bg-card"><summary class="cursor-pointer px-3 py-2 text-xs font-semibold">查看完整客户端原始正文</summary><pre class="max-h-80 overflow-auto whitespace-pre-wrap break-words border-t p-3 font-mono text-xs [overflow-wrap:anywhere]">{{ formatBody(detail.original_body) }}</pre></details>
        </div>
        <div class="flex shrink-0 flex-wrap items-center gap-2 border-t bg-card p-3">
          <Button size="sm" variant="outline" :disabled="!canEdit" @click="act('validate')">校验</Button>
          <Button size="sm" variant="outline" :disabled="!canEdit || !dirty" @click="act('save')">保存修改</Button>
          <Button size="sm" class="sm:ml-auto" :disabled="!canAct" @click="act('release')">{{ dirty ? '保存并放行' : '放行' }}</Button>
          <Button size="sm" variant="destructive" :disabled="!canAct" @click="act('cancel')">取消请求</Button>
        </div>
      </template>
    </div>
  </div>
</template>

<style scoped>
.queue-list { display: none; }
@media (min-width: 1024px), (min-width: 640px) and (max-height: 500px) {
  .interception-workbench { flex-direction: row; }
  .interception-queue { width: 14rem; border-bottom: 0; border-right: 1px solid hsl(var(--border)); }
  .queue-list { display: block; }
  .queue-selector { display: none; }
}
@media (min-width: 640px) and (max-width: 1023px) and (max-height: 500px) {
  .interception-queue { width: 10rem; }
}
</style>
