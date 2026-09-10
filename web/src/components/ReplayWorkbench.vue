<script setup lang="ts">
import { computed, onUnmounted, reactive, ref, watch } from 'vue'
import { ExternalLinkIcon, KeyRoundIcon, RepeatIcon, XCircleIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { APIError, cancelReplay, createReplay, fetchReplay, fetchReplays, validateReplay } from '@/lib/api'
import { formatBody, formatDuration, replayStateLabel } from '@/lib/correlation'
import type { Credential, ReplayCredential, ReplayRecord, ReplayRequest, ReplaySource, RequestSnapshot } from '@/lib/types'
import RequestDiff from './RequestDiff.vue'

const props = defineProps<{ traceId: string; source: ReplaySource; snapshot: RequestSnapshot; refreshKey: number }>()
const emit = defineEmits<{ select: [trace: string] }>()
const editableNames = ['Content-Type', 'Accept', 'Anthropic-Version', 'Anthropic-Beta', 'Openai-Beta']

const draftBody = ref('')
const draftHeaders = ref('{}')
const baselineBody = ref('')
const baselineHeaders = ref('{}')
// Credential values live only in this component's memory for the current
// draft. They are never written to storage, logs or exported commands.
const credentialValues = reactive<Record<string, string>>({})
const timeoutSeconds = ref(600)
const busy = ref(false)
const error = ref('')
const notice = ref('')
const current = ref<ReplayRecord | null>(null)
const history = ref<ReplayRecord[]>([])
let poll: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined
let disposed = false

const encoded = computed(() => props.snapshot.body_encoding === 'base64')
const credentials = computed<Credential[]>(() => props.snapshot.credentials ?? [])
const signed = computed(() => credentials.value.filter(item => !item.replayable))
const dirty = computed(() => draftBody.value !== baselineBody.value || draftHeaders.value !== baselineHeaders.value)
const headerPreview = computed(() => {
  try { return parseHeaders() }
  catch { return editableHeaders(props.snapshot.headers) }
})
const missing = computed(() => credentials.value.filter(item => item.replayable && !credentialValues[credentialKey(item)]?.trim()))
const canRun = computed(() => !busy.value && !signed.value.length && !missing.value.length && current.value?.state !== 'running')

function credentialKey(item: Credential): string {
  return `${item.kind}:${item.name.toLowerCase()}`
}

function editableHeaders(headers: Record<string, string>): Record<string, string> {
  const result: Record<string, string> = {}
  for (const [name, value] of Object.entries(headers)) {
    const canonical = editableNames.find(item => item.toLowerCase() === name.toLowerCase())
    if (canonical) result[canonical] = value
  }
  return result
}

function resetDraft() {
  baselineBody.value = draftBody.value = encoded.value ? props.snapshot.body : formatBody(props.snapshot.body)
  baselineHeaders.value = draftHeaders.value = JSON.stringify(editableHeaders(props.snapshot.headers), null, 2)
  for (const key of Object.keys(credentialValues)) delete credentialValues[key]
  error.value = notice.value = ''
  current.value = null
}

function parseHeaders(): Record<string, string> {
  const value: unknown = JSON.parse(draftHeaders.value)
  if (!value || typeof value !== 'object' || Array.isArray(value) || Object.values(value).some(item => typeof item !== 'string')) {
    throw new Error('请求头必须是名称到字符串值的 JSON 对象。')
  }
  return value as Record<string, string>
}

function buildRequest(withCredentials: boolean): ReplayRequest {
  const request: ReplayRequest = { trace_id: props.traceId, source: props.source, timeout_seconds: Math.min(3600, Math.max(1, Math.round(timeoutSeconds.value) || 600)) }
  if (draftBody.value !== baselineBody.value) request.body = draftBody.value
  if (draftHeaders.value !== baselineHeaders.value) request.headers = parseHeaders()
  if (withCredentials) {
    const supplied: ReplayCredential[] = []
    for (const item of credentials.value) {
      let value = credentialValues[credentialKey(item)]?.trim()
      if (!value) continue
      // The field asks for the part after the scheme; a pasted full header
      // value that already starts with it is sent unchanged.
      if (item.scheme && !value.toLowerCase().startsWith(item.scheme.toLowerCase() + ' ')) value = `${item.scheme} ${value}`
      supplied.push({ kind: item.kind, name: item.name, value })
    }
    if (supplied.length) request.credentials = supplied
  }
  return request
}

async function validate() {
  error.value = notice.value = ''
  busy.value = true
  try {
    const result = await validateReplay(buildRequest(false))
    notice.value = `结构校验通过：${result.modified ? '将发送修改后的内容' : '将原样发送'}，正文 ${result.body_bytes.toLocaleString()} 字节。` + (result.missing_credentials.length ? ` 仍需补充凭证：${result.missing_credentials.join('、')}。` : '')
  } catch (e) {
    error.value = String(e)
  } finally {
    busy.value = false
  }
}

async function run() {
  if (!canRun.value) return
  error.value = notice.value = ''
  busy.value = true
  try {
    const request = buildRequest(true)
    request.idempotency_key = crypto.randomUUID()
    const record = await createReplay(request)
    current.value = record
    notice.value = `已发起重放，新的调用 ${record.trace_id.slice(0, 8)} 正在记录；可在画布中查看，或点击“查看新调用”。`
    schedulePoll()
    void loadHistory()
  } catch (e) {
    error.value = e instanceof APIError && e.status === 422 ? `无法重放：${e.message}` : String(e)
  } finally {
    busy.value = false
  }
}

async function cancel() {
  if (!current.value || current.value.state !== 'running') return
  busy.value = true
  try {
    current.value = await cancelReplay(current.value.id)
    notice.value = '已取消重放。'
  } catch (e) {
    error.value = String(e)
  } finally {
    busy.value = false
    void loadHistory()
  }
}

async function refreshCurrent() {
  if (!current.value || current.value.state !== 'running' || disposed) return
  try {
    current.value = await fetchReplay(current.value.id)
  } catch {
    // The list refresh will retry; a transient failure must not clear state.
  }
  if (current.value?.state === 'running') schedulePoll()
}

function schedulePoll() {
  clearTimeout(poll)
  poll = setTimeout(() => { void refreshCurrent() }, 1000)
}

async function loadHistory() {
  controller?.abort()
  const request = controller = new AbortController()
  try {
    const records = await fetchReplays(request.signal)
    if (request.signal.aborted) return
    history.value = records.filter(item => item.replay_of === props.traceId).reverse()
    if (current.value) {
      const fresh = records.find(item => item.id === current.value?.id)
      if (fresh) current.value = fresh
    }
  } catch {
    // History is informational only.
  }
}

function describe(record: ReplayRecord): string {
  const reason = record.reason === 'timeout' ? '（超时）' : record.reason === 'manual' ? '（手动）' : ''
  return `${replayStateLabel(record.state)}${reason}${record.status_code ? ` · HTTP ${record.status_code}` : ''}${record.error ? ` · ${record.error}` : ''}`
}

function elapsed(record: ReplayRecord): string {
  if (!record.finished_at) return ''
  return formatDuration(Date.parse(record.finished_at) - Date.parse(record.created_at))
}

watch(() => [props.traceId, props.source], () => { resetDraft(); void loadHistory() }, { immediate: true })
watch(() => props.refreshKey, () => { void loadHistory(); void refreshCurrent() })
onUnmounted(() => { disposed = true; clearTimeout(poll); controller?.abort() })
</script>

<template>
  <section class="min-w-0 space-y-3 rounded-lg border bg-card p-3" aria-label="请求重放工作台">
    <div class="flex flex-wrap items-center gap-2">
      <RepeatIcon class="h-3.5 w-3.5 text-violet-700" />
      <h3 class="text-xs font-semibold">重放 · {{ source === 'original' ? '客户端原始请求（再次经过网关规则）' : '实际出站请求（不再重复注入或断点）' }}</h3>
      <Button size="sm" variant="ghost" class="ml-auto h-7 text-xs" :disabled="busy" @click="resetDraft">重置草稿</Button>
    </div>
    <p class="text-[11px] leading-relaxed text-muted-foreground">只发送这一次模型 API 请求，不会调度本地工具或 Agent 流程；请求里的 Provider 内置工具仍可能由 Provider 执行。历史记录保持不变，每次发起都会生成新的调用并标注重放来源。</p>
    <p v-if="signed.length" class="rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">{{ signed.map(item => `${item.name}（${item.scheme}）`).join('、') }} 使用按请求签名的鉴权方案，无法用静态凭证重放。</p>
    <p v-if="error" role="alert" class="rounded-md border border-red-200 bg-red-50 p-3 text-xs text-red-900">{{ error }}</p>
    <p v-if="notice" role="status" class="rounded-md border border-teal-200 bg-teal-50 p-3 text-xs text-teal-900">{{ notice }}</p>

    <div v-if="current" class="rounded-md border p-3 text-xs" :class="current.state === 'running' ? 'border-violet-200 bg-violet-50/60' : current.state === 'done' ? 'border-teal-200 bg-teal-50/60' : 'border-amber-200 bg-amber-50/60'" aria-live="polite">
      <div class="flex flex-wrap items-center gap-2">
        <span class="font-semibold">本次重放：{{ describe(current) }}</span>
        <span v-if="elapsed(current)" class="text-muted-foreground">{{ elapsed(current) }}</span>
        <span class="font-mono text-[10px] text-muted-foreground">{{ current.trace_id.slice(0, 8) }}</span>
        <div class="ml-auto flex gap-1">
          <Button size="sm" variant="outline" class="h-7 text-xs" @click="emit('select', current.trace_id)"><ExternalLinkIcon class="mr-1 h-3 w-3" />查看新调用</Button>
          <Button v-if="current.state === 'running'" size="sm" variant="destructive" class="h-7 text-xs" :disabled="busy" @click="cancel"><XCircleIcon class="mr-1 h-3 w-3" />取消重放</Button>
        </div>
      </div>
    </div>

    <div v-if="encoded" class="rounded-md border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">压缩或二进制正文只能原样重放，无法编辑。</div>
    <div v-else class="space-y-2">
      <div class="flex flex-wrap items-center gap-2"><Label for="replay-body" class="text-xs font-semibold">重放正文</Label><span class="text-[10px] text-muted-foreground">{{ dirty ? '已修改，将发送编辑后的内容' : '未修改，将发送捕获的原始字节' }}</span></div>
      <Textarea id="replay-body" v-model="draftBody" spellcheck="false" rows="10" class="min-h-32 resize-y font-mono text-xs leading-relaxed" />
    </div>
    <details class="rounded-lg border bg-background"><summary class="cursor-pointer px-3 py-2 text-xs font-semibold">编辑允许的请求头</summary><div class="space-y-2 border-t p-3"><p class="text-[11px] leading-relaxed text-muted-foreground">可编辑 Content-Type、Accept、Anthropic-Version、Anthropic-Beta 和 OpenAI-Beta；其余请求头按捕获内容原样发送。</p><Label for="replay-headers" class="sr-only">重放请求头</Label><Textarea id="replay-headers" v-model="draftHeaders" spellcheck="false" rows="4" class="font-mono text-xs" /></div></details>

    <div v-if="credentials.length" class="space-y-2 rounded-lg border bg-background p-3">
      <div class="flex items-center gap-2 text-xs font-semibold"><KeyRoundIcon class="h-3.5 w-3.5 text-amber-700" />本次重放的凭证</div>
      <p class="text-[11px] leading-relaxed text-muted-foreground">网关没有保存凭证。这里输入的值只用于这一次重放，不会写入日志、快照或浏览器存储；界面里遮盖后的值不能作为凭证。</p>
      <div v-for="item in credentials" :key="credentialKey(item)" class="space-y-1">
        <Label :for="`credential-${credentialKey(item)}`" class="text-xs">{{ item.kind === 'header' ? `请求头 ${item.name}` : item.kind === 'query' ? `查询参数 ${item.name}` : 'URL user:password' }}<span v-if="item.scheme" class="ml-1 text-muted-foreground"> · {{ item.scheme }} 之后的完整值{{ item.replayable ? '' : '（无法重放）' }}</span></Label>
        <Input :id="`credential-${credentialKey(item)}`" v-model="credentialValues[credentialKey(item)]" type="password" autocomplete="off" spellcheck="false" :disabled="!item.replayable" :placeholder="item.scheme ? `${item.scheme} 之后的凭证` : '凭证值'" class="font-mono text-xs" />
      </div>
    </div>
    <p v-else class="text-[11px] text-muted-foreground">捕获的请求没有携带凭证；重放将不带鉴权发送（适用于本地或无鉴权上游）。</p>

    <div class="flex flex-wrap items-end gap-2">
      <div class="space-y-1"><Label for="replay-timeout" class="text-xs">超时（秒）</Label><Input id="replay-timeout" v-model.number="timeoutSeconds" type="number" min="1" max="3600" step="1" class="h-8 w-28 text-xs" /></div>
      <div class="ml-auto flex flex-wrap gap-2">
        <Button size="sm" variant="outline" :disabled="busy" @click="validate">校验</Button>
        <Button size="sm" :disabled="!canRun" :title="missing.length ? `请先填写：${missing.map(item => item.name).join('、')}` : ''" @click="run"><RepeatIcon class="mr-1 h-3.5 w-3.5" />{{ dirty ? '发送修改后的请求' : '原样重放' }}</Button>
      </div>
    </div>
    <p v-if="missing.length" class="text-[11px] text-amber-800">尚未填写：{{ missing.map(item => item.name).join('、') }}。</p>

    <RequestDiff v-if="!encoded" :original-body="snapshot.body" :modified-body="draftBody" :original-headers="editableHeaders(snapshot.headers)" :modified-headers="headerPreview" />

    <div v-if="history.length" class="rounded-lg border bg-background">
      <h4 class="border-b px-3 py-2 text-xs font-semibold">此请求的重放记录 · {{ history.length }}</h4>
      <ul class="max-h-48 overflow-auto text-xs" aria-label="重放记录列表">
        <li v-for="record in history" :key="record.id" class="flex flex-wrap items-center gap-2 border-b px-3 py-2 last:border-b-0">
          <span class="font-mono text-[10px] text-muted-foreground">{{ new Date(record.created_at).toLocaleTimeString() }}</span>
          <span>{{ record.source === 'original' ? '原始' : '出站' }}{{ record.modified ? ' · 已修改' : '' }}</span>
          <span class="min-w-0 flex-1 truncate" :class="record.state === 'error' ? 'text-red-700' : record.state === 'canceled' ? 'text-zinc-500' : ''">{{ describe(record) }}</span>
          <Button size="sm" variant="ghost" class="h-6 px-2 text-[11px]" @click="emit('select', record.trace_id)">查看 {{ record.trace_id.slice(0, 8) }}</Button>
        </li>
      </ul>
    </div>
  </section>
</template>
