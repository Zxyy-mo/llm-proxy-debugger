<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, watch } from 'vue'
import { ExternalLinkIcon, KeyRoundIcon, RepeatIcon, XCircleIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { APIError, cancelReplay, createReplay, fetchReplay, fetchReplays, validateReplay } from '@/lib/api'
import { formatBody, formatDuration, replayStateLabel } from '@/lib/correlation'
import { initialReplayOperation, reconcileReplayRecord, ReplayOperation, type ReplayOperationState } from '@/lib/replayOperation'
import type { Credential, ReplayCredential, ReplayRecord, ReplayRequest, ReplaySource, RequestSnapshot } from '@/lib/types'
import RequestDiff from './RequestDiff.vue'
import ResponseComparison from './ResponseComparison.vue'

const props = defineProps<{ traceId: string | null; source: ReplaySource; snapshot?: RequestSnapshot; refreshKey: number; active: boolean }>()
const emit = defineEmits<{ select: [trace: string]; source: [trace: string]; operation: [state: ReplayOperationState] }>()
const editableNames = ['Content-Type', 'Accept', 'Anthropic-Version', 'Anthropic-Beta', 'Openai-Beta']
const draftBody = ref('')
const draftHeaders = ref('{}')
const baselineBody = ref('')
const baselineHeaders = ref('{}')
// Only the current draft and an unresolved submitted attempt hold secrets.
// View navigation preserves this one component; changing source clears fields.
const credentialValues = reactive<Record<string, string>>({})
const timeoutSeconds = ref(600)
const error = ref('')
const notice = ref('')
const operation = ref(initialReplayOperation())
const comparison = ref<ReplayRecord | null>(null)
const history = ref<ReplayRecord[]>([])
const historyReady = ref(false)
const historyError = ref('')
const historyLoading = ref(false)
const requiredCredentialNames = ref<string[] | null>(null)
const resolvingCredentials = ref(false)
const requirementsError = ref('')
const validating = ref(false)
let poll: ReturnType<typeof setTimeout> | undefined
let historyController: AbortController | undefined
let requirementsController: AbortController | undefined
let validationController: AbortController | undefined
let requirementsTimer: ReturnType<typeof setTimeout> | undefined
let draftIdentity = ''
let initialized = false
let draftVersion = 0
let disposed = false

const runner = new ReplayOperation({
  create: createReplay, list: () => fetchReplays(), get: fetchReplay, cancel: cancelReplay,
  definiteRejection: e => e instanceof APIError && ((e.status >= 400 && e.status < 500) || e.status === 503),
}, state => {
  operation.value = state
  emit('operation', state)
  schedulePoll()
})
const current = computed(() => operation.value.record)
const busy = computed(() => operation.value.busy || validating.value)
const unresolved = computed(() => operation.value.phase === 'unknown' || operation.value.phase === 'submitting')
const encoded = computed(() => props.snapshot?.body_encoding === 'base64')
const credentials = computed<Credential[]>(() => (props.snapshot?.credentials ?? []).filter(item => requiredCredentialNames.value === null || requiredCredentialNames.value.includes(`${item.kind} ${item.name}`.toLowerCase())))
const signed = computed(() => credentials.value.filter(item => !item.replayable))
const dirty = computed(() => draftBody.value !== baselineBody.value || draftHeaders.value !== baselineHeaders.value)
const headerPreview = computed(() => {
  try { return parseHeaders() }
  catch { return editableHeaders(props.snapshot?.headers ?? {}) }
})
const missing = computed(() => credentials.value.filter(item => item.replayable && !credentialValues[credentialKey(item)]?.trim()))
const supported = computed(() => props.snapshot?.method === 'POST')
const runningHistory = computed(() => history.value.filter(item => item.state === 'running'))
const canRun = computed(() => Boolean(props.snapshot) && initialized && !busy.value && !resolvingCredentials.value && !requirementsError.value && historyReady.value && !historyLoading.value && supported.value && !props.snapshot?.unavailable && !(props.snapshot?.redacted && !props.snapshot.raw_retained) && !signed.value.length && !missing.value.length && !unresolved.value && current.value?.state !== 'running' && !runningHistory.value.length)

function credentialKey(item: Credential): string { return `${item.kind}:${item.name.toLowerCase()}` }
function editableHeaders(headers: Record<string, string>): Record<string, string> {
  const result: Record<string, string> = {}
  for (const [name, value] of Object.entries(headers)) {
    const canonical = editableNames.find(item => item.toLowerCase() === name.toLowerCase())
    if (canonical) result[canonical] = value
  }
  return result
}

function resetDraft() {
  requiredCredentialNames.value = null
  baselineBody.value = draftBody.value = encoded.value ? props.snapshot?.body ?? '' : formatBody(props.snapshot?.body)
  baselineHeaders.value = draftHeaders.value = JSON.stringify(editableHeaders(props.snapshot?.headers ?? {}), null, 2)
  for (const key of Object.keys(credentialValues)) delete credentialValues[key]
  timeoutSeconds.value = 600
  error.value = notice.value = requirementsError.value = ''
  initialized = Boolean(props.snapshot)
  // Deliberately leave the server operation and its immutable attempt alone.
}

function parseHeaders(): Record<string, string> {
  const value: unknown = JSON.parse(draftHeaders.value)
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.values(value).some(item => typeof item !== 'string')) throw new Error('请求头必须是名称到字符串值的 JSON 对象。')
  return value as Record<string, string>
}

function buildRequest(withCredentials: boolean): ReplayRequest {
  if (!props.traceId || !props.snapshot) throw new Error('完整请求尚未加载。')
  const request: ReplayRequest = { trace_id: props.traceId, source: props.source, timeout_seconds: Math.min(3600, Math.max(1, Math.round(timeoutSeconds.value) || 600)) }
  if (draftBody.value !== baselineBody.value) request.body = draftBody.value
  if (draftHeaders.value !== baselineHeaders.value) request.headers = parseHeaders()
  if (withCredentials) {
    const supplied: ReplayCredential[] = []
    for (const item of credentials.value) {
      let value = credentialValues[credentialKey(item)]?.trim()
      if (!value) continue
      if (item.scheme && !value.toLowerCase().startsWith(item.scheme.toLowerCase() + ' ')) value = `${item.scheme} ${value}`
      supplied.push({ kind: item.kind, name: item.name, value })
    }
    if (supplied.length) request.credentials = supplied
  }
  return request
}

async function validate() {
  validationController?.abort()
  const request = validationController = new AbortController()
  const version = draftVersion
  error.value = notice.value = ''
  validating.value = true
  try {
    const result = await validateReplay(buildRequest(false), request.signal)
    if (request.signal.aborted || version !== draftVersion) return
    requiredCredentialNames.value = result.missing_credentials.map(v => v.toLowerCase())
    requirementsError.value = ''
    notice.value = `结构校验通过：${result.modified ? '将发送修改后的内容' : '将原样发送'}，正文 ${result.body_bytes.toLocaleString()} 字节。` + (result.missing_credentials.length ? ` 仍需补充凭证：${result.missing_credentials.join('、')}。` : '')
  } catch (e) { if (!request.signal.aborted && version === draftVersion) error.value = String(e) }
  finally { if (request === validationController) validating.value = false }
}

async function resolveCredentials() {
  if (!props.active || !props.snapshot || !supported.value || disposed) return
  requirementsController?.abort()
  const request = requirementsController = new AbortController()
  const version = draftVersion
  resolvingCredentials.value = true
  requirementsError.value = ''
  try {
    const result = await validateReplay(buildRequest(false), request.signal)
    if (!request.signal.aborted && version === draftVersion) requiredCredentialNames.value = result.missing_credentials.map(v => v.toLowerCase())
  } catch (e) {
    if (!request.signal.aborted && version === draftVersion) { requiredCredentialNames.value = null; requirementsError.value = `请求尚未通过校验：${String(e)}` }
  } finally { if (request === requirementsController) resolvingCredentials.value = false }
}

async function run() {
  if (!canRun.value) return
  error.value = notice.value = ''
  try {
    const request = buildRequest(true)
    request.idempotency_key = crypto.randomUUID()
    await runner.start(request)
    if (current.value) { notice.value = '重放已记录。可查看新调用，或在这里对比来源响应。'; void loadHistory() }
  } catch (e) { error.value = String(e) }
}

async function recover(retry = false) {
  await runner.recover(retry)
  if (current.value) { notice.value = '已找回本次重放，没有另建一次请求。'; void loadHistory() }
}

async function cancel(record: ReplayRecord | null = current.value) {
  if (!record) return
  const result = await runner.cancel(record)
  if (result) notice.value = result.state === 'canceled' ? '已取消重放。' : `重放已结束：${describe(result)}`
  void loadHistory()
}

function schedulePoll() {
  clearTimeout(poll)
  if (disposed || current.value?.state !== 'running') return
  poll = setTimeout(async () => { await runner.refresh(); schedulePoll() }, 1000)
}

async function loadHistory() {
  historyController?.abort()
  if (!props.traceId) { history.value = []; historyReady.value = false; historyLoading.value = false; return }
  const request = historyController = new AbortController()
  const trace = props.traceId
  historyLoading.value = true
  historyError.value = ''
  try {
    const records = await fetchReplays(request.signal)
    if (request.signal.aborted || trace !== props.traceId) return
    history.value = records.filter(item => item.replay_of === trace).reverse()
    historyReady.value = true
    runner.observe(records, trace)
  } catch (e) {
    if (!request.signal.aborted && trace === props.traceId) { historyReady.value = false; historyError.value = `读取重放记录失败：${String(e)}。请重试，确认没有进行中的重放。` }
  } finally { if (request === historyController) historyLoading.value = false }
}

function describe(record: ReplayRecord): string {
  const reason = record.reason === 'timeout' ? '（超时）' : record.reason === 'manual' ? '（手动）' : record.reason === 'gateway_restarted' ? '（网关重启）' : record.reason === 'shutdown' ? '（网关关闭）' : ''
  return `${replayStateLabel(record.state)}${reason}${record.status_code ? ` · HTTP ${record.status_code}` : ''}${record.error ? ` · ${record.error}` : ''}`
}
function elapsed(record: ReplayRecord): string { return record.finished_at ? formatDuration(Date.parse(record.finished_at) - Date.parse(record.created_at)) : '' }
function selectComparison(record: ReplayRecord) { comparison.value = reconcileReplayRecord(record, current.value, history.value) }

watch([current, history], () => {
  comparison.value = reconcileReplayRecord(comparison.value, current.value, history.value)
})

watch(() => [props.traceId, props.source, props.snapshot] as const, () => {
  const identity = `${props.traceId ?? ''}:${props.source}`
  if (identity !== draftIdentity) {
    draftIdentity = identity; initialized = false; comparison.value = null
    history.value = []; historyReady.value = false
    resetDraft()
    void loadHistory()
  } else if (!initialized && props.snapshot) resetDraft()
}, { immediate: true })
watch(() => [props.traceId, props.source, draftBody.value, draftHeaders.value, props.active, props.snapshot] as const, () => {
  draftVersion++
  validationController?.abort(); validating.value = false
  requirementsController?.abort(); clearTimeout(requirementsTimer)
  resolvingCredentials.value = Boolean(props.active && props.snapshot && supported.value)
  if (!resolvingCredentials.value) return
  requirementsTimer = setTimeout(() => { void resolveCredentials() }, 250)
}, { immediate: true })
watch(() => props.active, active => { if (active) void loadHistory() })
watch(() => props.refreshKey, () => { if (props.active || current.value?.state === 'running') void loadHistory() })
onUnmounted(() => { disposed = true; runner.dispose(); clearTimeout(poll); clearTimeout(requirementsTimer); historyController?.abort(); requirementsController?.abort(); validationController?.abort() })
defineExpose({ cancelOperation: () => cancel() })
</script>

<template>
  <section class="min-w-0 space-y-3 rounded-lg border bg-card p-3" aria-label="请求重放工作台">
    <div class="flex flex-wrap items-center gap-2">
      <RepeatIcon class="h-3.5 w-3.5 text-violet-700" />
      <h3 class="text-xs font-semibold">重放 · {{ source === 'original' ? '客户端原始请求（再次经过网关规则）' : '实际出站请求（不再重复注入或断点）' }}</h3>
      <Button size="sm" variant="ghost" class="ml-auto h-7 text-xs" :disabled="busy || !snapshot" @click="resetDraft">重置草稿</Button>
    </div>
    <p class="text-[11px] leading-relaxed text-muted-foreground">只发送这一次模型 API 请求，不会调度本地工具或 Agent 流程；请求里的 Provider 内置工具仍可能由 Provider 执行。原记录保持不变。</p>
    <p v-if="error" role="alert" class="rounded-md border border-red-200 bg-red-50 p-3 text-xs text-red-900">{{ error }}</p>
    <p v-if="notice" role="status" class="rounded-md border border-teal-200 bg-teal-50 p-3 text-xs text-teal-900">{{ notice }}</p>
    <p v-if="operation.error" role="alert" class="rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">{{ operation.error }}</p>
    <div v-if="unresolved" class="space-y-2 rounded-md border border-amber-200 bg-amber-50 p-3 text-xs" aria-live="polite">
      <p class="font-semibold">{{ operation.phase === 'submitting' ? '正在提交本次重放…' : '本次重放的发送结果尚未确认' }}</p>
      <p class="break-words leading-relaxed">来源 {{ operation.sourceTrace?.slice(0, 8) }} · {{ operation.source === 'original' ? '原始请求' : '出站请求' }}。当前编辑和重置不会改变已提交的内容；确认结果前不能发起另一重放。</p>
      <div v-if="operation.phase === 'unknown'" class="flex flex-wrap gap-2">
        <Button size="sm" variant="outline" :disabled="operation.busy" @click="recover(false)">恢复本次重放</Button>
        <Button size="sm" variant="outline" :disabled="operation.busy" @click="recover(true)">使用原次提交重试</Button>
      </div>
    </div>
    <div v-if="current" class="space-y-2 rounded-md border p-3 text-xs" :class="current.state === 'running' ? 'border-violet-200 bg-violet-50/60' : current.state === 'done' ? 'border-teal-200 bg-teal-50/60' : 'border-amber-200 bg-amber-50/60'" aria-live="polite">
      <div class="flex flex-wrap items-center gap-2">
        <span class="font-semibold">本次重放：{{ describe(current) }}</span>
        <span v-if="elapsed(current)" class="text-muted-foreground">{{ elapsed(current) }}</span>
        <span class="font-mono text-[10px] text-muted-foreground">{{ current.trace_id.slice(0, 8) }}</span>
      </div>
      <p class="text-[11px] text-muted-foreground">来源 {{ current.replay_of.slice(0, 8) }} · {{ current.source === 'original' ? '客户端原始请求' : '实际出站请求' }} · {{ current.modified ? '发送前已修改' : '原样发送' }}</p>
      <p v-if="current.replay_of !== traceId" class="break-words text-amber-900">此重放来自另一个请求 {{ current.replay_of.slice(0, 8) }}，不会因切换编辑来源而停止。<Button variant="link" size="sm" class="h-auto px-1 py-0 text-xs" @click="emit('source', current.replay_of)">返回该来源</Button></p>
      <div class="flex flex-wrap gap-2">
        <Button size="sm" variant="outline" class="h-7 text-xs" @click="emit('select', current.trace_id)"><ExternalLinkIcon class="mr-1 h-3 w-3" />查看新调用</Button>
        <Button size="sm" variant="outline" class="h-7 text-xs" @click="selectComparison(current)">对比来源响应</Button>
        <Button v-if="current.state === 'running'" size="sm" variant="destructive" class="h-7 text-xs" :disabled="operation.busy" @click="cancel()"><XCircleIcon class="mr-1 h-3 w-3" />取消重放</Button>
      </div>
    </div>
    <ResponseComparison v-if="comparison" :source-trace="comparison.replay_of" :replay-trace="comparison.trace_id" :status="comparison.state" :replay-record="comparison" :active="active" @select="trace => emit('select', trace)" />

    <template v-if="snapshot && supported">
      <p v-if="snapshot.unavailable" role="alert" class="rounded border border-red-200 bg-red-50 p-3 text-xs text-red-900">请求正文不可用：{{ snapshot.unavailable }}</p>
      <p v-if="signed.length" class="rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">{{ signed.map(item => `${item.name}（${item.scheme}）`).join('、') }} 使用按请求签名的鉴权方案，无法用静态凭证重放。</p>
      <div v-if="encoded" class="rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">压缩或二进制正文只能原样重放，无法编辑。</div>
      <div v-else class="space-y-2">
        <div class="flex flex-wrap items-center gap-2"><Label for="replay-body" class="text-xs font-semibold">重放正文</Label><span class="text-[10px] text-muted-foreground">{{ dirty ? '已修改，将发送编辑后的内容' : '未修改，将发送捕获的原始字节' }}</span></div>
        <p v-if="snapshot.redacted" class="text-[11px] text-amber-800">正文已脱敏，编辑关闭。{{ snapshot.raw_retained ? '原样重放会使用保留的原文。' : '原文没有保留，不能精确重放此快照。' }}</p>
        <Textarea id="replay-body" v-model="draftBody" :disabled="snapshot.redacted || Boolean(snapshot.unavailable)" spellcheck="false" rows="10" class="min-h-32 resize-y font-mono text-xs leading-relaxed" />
      </div>
      <details class="rounded-lg border bg-background"><summary class="cursor-pointer px-3 py-2 text-xs font-semibold">编辑允许的请求头</summary><div class="space-y-2 border-t p-3"><p class="text-[11px] leading-relaxed text-muted-foreground">可编辑 Content-Type、Accept、Anthropic-Version、Anthropic-Beta 和 OpenAI-Beta；其余请求头按捕获内容原样发送。</p><Label for="replay-headers" class="sr-only">重放请求头</Label><Textarea id="replay-headers" v-model="draftHeaders" spellcheck="false" rows="4" class="font-mono text-xs" /></div></details>
      <div v-if="credentials.length" class="space-y-2 rounded-lg border bg-background p-3">
        <div class="flex items-center gap-2 text-xs font-semibold"><KeyRoundIcon class="h-3.5 w-3.5 text-amber-700" />本次重放的凭证</div>
        <p class="text-[11px] leading-relaxed text-muted-foreground">凭证仅留在当前编辑内存中，不写入日志、快照或浏览器存储。切换到另一编辑来源会清空输入；已提交且结果未明的请求会保留到确认结果。</p>
        <div v-for="item in credentials" :key="credentialKey(item)" class="space-y-1">
          <Label :for="`credential-${credentialKey(item)}`" class="text-xs">{{ item.kind === 'header' ? `请求头 ${item.name}` : item.kind === 'query' ? `查询参数 ${item.name}` : 'URL user:password' }}<span v-if="item.scheme" class="ml-1 text-muted-foreground"> · {{ item.scheme }} 之后的完整值{{ item.replayable ? '' : '（无法重放）' }}</span></Label>
          <Input :id="`credential-${credentialKey(item)}`" v-model="credentialValues[credentialKey(item)]" type="password" autocomplete="off" spellcheck="false" :disabled="!item.replayable" :placeholder="item.scheme ? `${item.scheme} 之后的凭证` : '凭证值'" class="font-mono text-xs" />
        </div>
      </div>
      <p v-else class="text-[11px] text-muted-foreground">{{ resolvingCredentials ? '正在核对目标与鉴权要求…' : '无需补充本次凭证；如 Provider 配置了密钥环境变量，将在出站时使用。' }}</p>
      <p v-if="requirementsError" role="status" class="rounded border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">{{ requirementsError }}<Button variant="link" size="sm" class="h-auto px-1 py-0 text-xs" :disabled="resolvingCredentials" @click="resolveCredentials">重新校验</Button></p>
      <p v-if="historyError" role="alert" class="rounded border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">{{ historyError }}<Button variant="link" size="sm" class="h-auto px-1 py-0 text-xs" :disabled="historyLoading" @click="loadHistory">重试读取记录</Button></p>
      <div class="flex flex-wrap items-end gap-2">
        <div class="space-y-1"><Label for="replay-timeout" class="text-xs">超时（秒）</Label><Input id="replay-timeout" v-model.number="timeoutSeconds" type="number" min="1" max="3600" step="1" class="h-8 w-28 text-xs" /></div>
        <div class="ml-auto flex flex-wrap gap-2">
          <Button size="sm" variant="outline" :disabled="busy || !snapshot" @click="validate">校验</Button>
          <Button size="sm" :disabled="!canRun" :title="missing.length ? `请先填写：${missing.map(item => item.name).join('、')}` : ''" @click="run"><RepeatIcon class="mr-1 h-3.5 w-3.5" />{{ dirty ? '发送修改后的请求' : '原样重放' }}</Button>
        </div>
      </div>
      <p v-if="missing.length" class="text-[11px] text-amber-800">尚未填写：{{ missing.map(item => item.name).join('、') }}。</p>
      <p v-if="current?.state === 'running' || runningHistory.length" class="text-[11px] text-muted-foreground">已有重放进行中。可继续编辑；本次结束或取消后才能再次发送。</p>
      <RequestDiff v-if="!encoded && active" :original-body="snapshot.body" :modified-body="draftBody" :original-headers="editableHeaders(snapshot.headers)" :modified-headers="headerPreview" />
    </template>
    <p v-else-if="snapshot?.method === 'WS'" class="text-xs leading-relaxed text-muted-foreground">此快照是 WebSocket 消息，包含连接内状态。可以查看上下文与响应；重放工作台发送独立的 HTTP 模型请求。</p>
    <p v-else-if="snapshot" class="text-xs leading-relaxed text-muted-foreground">此请求不是可重放的 POST 模型生成调用。可以查看完整报文和上下文，或导出 cURL。</p>
    <p v-else class="text-xs text-muted-foreground">{{ traceId ? '正在等待完整请求…' : '选择一个调用，准备重放。' }}</p>
    <div v-if="history.length" class="rounded-lg border bg-background">
      <h4 class="border-b px-3 py-2 text-xs font-semibold">此请求的重放记录 · {{ history.length }}</h4>
      <ul class="max-h-64 overflow-auto text-xs" aria-label="重放记录列表" tabindex="0">
        <li v-for="record in history" :key="record.id" class="flex flex-wrap items-center gap-2 border-b px-3 py-2 last:border-b-0">
          <span class="font-mono text-[10px] text-muted-foreground">{{ new Date(record.created_at).toLocaleTimeString() }}</span>
          <span>{{ record.source === 'original' ? '原始' : '出站' }}{{ record.modified ? ' · 已修改' : '' }}</span>
          <span class="min-w-0 flex-1 break-words" :class="record.state === 'error' ? 'text-red-700' : record.state === 'canceled' ? 'text-zinc-500' : ''">{{ describe(record) }}</span>
          <Button size="sm" variant="ghost" class="h-7 px-2 text-[11px]" @click="emit('select', record.trace_id)">查看 {{ record.trace_id.slice(0, 8) }}</Button>
          <Button size="sm" variant="ghost" class="h-7 px-2 text-[11px]" @click="selectComparison(record)">对比响应</Button>
          <Button v-if="record.state === 'running' && record.id !== current?.id" size="sm" variant="destructive" class="h-7 px-2 text-[11px]" :disabled="operation.busy" @click="cancel(record)">取消此重放</Button>
        </li>
      </ul>
    </div>
  </section>
</template>
