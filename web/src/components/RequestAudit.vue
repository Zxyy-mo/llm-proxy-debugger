<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { DiffIcon, RepeatIcon, TerminalIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { fetchRequestCapture } from '@/lib/api'
import { formatBody } from '@/lib/correlation'
import type { ReplaySource, RequestCapture, RequestLog } from '@/lib/types'
import CurlExportPanel from './CurlExportPanel.vue'
import ReplayWorkbench from './ReplayWorkbench.vue'
import RequestDiff from './RequestDiff.vue'
import ContextComparison from './ContextComparison.vue'

const props = defineProps<{ traceId: string | null; refreshKey: number; replayRefreshKey: number; logs: RequestLog[] }>()
const emit = defineEmits<{ select: [trace: string] }>()
const capture = ref<RequestCapture | null>(null)
const error = ref('')
const loading = ref(false)
const mode = ref<'compare' | 'context' | 'curl' | 'replay'>('compare')
const chosenSource = ref<ReplaySource | null>(null)
let controller: AbortController | undefined

const source = computed<ReplaySource>(() => chosenSource.value ?? (capture.value?.outgoing ? 'outgoing' : 'original'))
const snapshot = computed(() => capture.value ? (source.value === 'outgoing' ? capture.value.outgoing : capture.value.original) : undefined)

watch(() => [props.traceId, props.refreshKey], async () => {
  controller?.abort()
  const request = controller = new AbortController()
  const trace = props.traceId
  error.value = ''
  if (capture.value?.trace_id !== trace) {
    capture.value = null
    chosenSource.value = null
  }
  if (!trace) { loading.value = false; return }
  loading.value = true
  try {
    const result = await fetchRequestCapture(trace, request.signal)
    if (!request.signal.aborted) capture.value = result
  } catch (e) {
    if (!request.signal.aborted) error.value = String(e)
  } finally {
    if (!request.signal.aborted) loading.value = false
  }
}, { immediate: true })
onUnmounted(() => controller?.abort())
</script>

<template>
  <div class="panel-scroll min-h-0 min-w-0 flex-1 space-y-4 p-3 sm:p-4" aria-label="原始与出站请求" tabindex="0">
    <p v-if="error" role="alert" class="text-sm text-destructive">{{ error }}</p>
    <p v-if="loading && !capture" class="text-sm text-muted-foreground">正在加载完整请求…</p>
    <p v-else-if="!traceId" class="text-sm text-muted-foreground">选择一个请求，查看客户端原文和实际出站内容，或导出 cURL、发起重放。</p>
    <template v-if="capture">
      <div class="flex flex-wrap items-center justify-between gap-2">
        <div><h2 class="text-sm font-semibold">原始 / 出站请求</h2><p class="mt-1 text-xs text-muted-foreground">完整正文 · 凭证已遮盖，导出与重放时需重新提供</p></div>
        <span class="break-all font-mono text-[10px] text-muted-foreground">{{ capture.trace_id }}</span>
      </div>
      <div class="flex flex-wrap items-center gap-1 border-b pb-2" role="tablist" aria-label="请求操作">
        <Button :variant="mode === 'compare' ? 'secondary' : 'ghost'" size="sm" class="h-7 gap-1 text-xs" role="tab" :aria-selected="mode === 'compare'" @click="mode = 'compare'"><DiffIcon class="h-3 w-3" />对比</Button>
        <Button :variant="mode === 'context' ? 'secondary' : 'ghost'" size="sm" class="h-7 gap-1 text-xs" role="tab" :aria-selected="mode === 'context'" @click="mode = 'context'">上下文变化</Button>
        <Button :variant="mode === 'curl' ? 'secondary' : 'ghost'" size="sm" class="h-7 gap-1 text-xs" role="tab" :aria-selected="mode === 'curl'" @click="mode = 'curl'"><TerminalIcon class="h-3 w-3" />复制 cURL</Button>
        <Button :variant="mode === 'replay' ? 'secondary' : 'ghost'" size="sm" class="h-7 gap-1 text-xs" role="tab" :aria-selected="mode === 'replay'" @click="mode = 'replay'"><RepeatIcon class="h-3 w-3" />重放</Button>
        <div v-if="mode === 'curl' || mode === 'replay'" class="ml-auto flex items-center gap-2 text-xs">
          <label for="request-source" class="text-muted-foreground">来源</label>
          <select id="request-source" class="h-7 rounded-md border bg-background px-2 text-xs" :value="source" @change="chosenSource = ($event.target as HTMLSelectElement).value as ReplaySource">
            <option value="original">客户端原始请求</option>
            <option value="outgoing" :disabled="!capture.outgoing">实际出站请求{{ capture.outgoing ? '' : '（未出站）' }}</option>
          </select>
        </div>
      </div>
      <template v-if="mode === 'compare'">
        <RequestDiff v-if="capture.outgoing" :original-body="capture.original.body" :modified-body="capture.outgoing.body" :original-headers="capture.original.headers" :modified-headers="capture.outgoing.headers" />
        <p v-else class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">此请求尚未出站。请求放行后，实际出站内容会显示在这里。</p>
        <div class="grid min-w-0 gap-4 lg:grid-cols-2">
          <section v-for="item in [{ label: '客户端原始请求', value: capture.original }, { label: '实际出站请求', value: capture.outgoing }]" :key="item.label" class="min-w-0 rounded-lg border bg-card">
            <h3 class="border-b px-3 py-2 text-xs font-semibold">{{ item.label }}</h3>
            <template v-if="item.value">
              <p v-if="item.value.unavailable" role="alert" class="border-b bg-red-50 px-3 py-2 text-xs text-red-900">{{ item.value.unavailable }}</p>
              <p v-if="item.value.redacted" class="border-b bg-amber-50 px-3 py-2 text-xs text-amber-900">正文已脱敏；{{ item.value.raw_retained ? '原文按策略保留，原样重放使用保留内容。' : '原文未保留，无法精确重放此快照。' }}</p>
              <p v-if="item.value.body_encoding" class="border-b bg-amber-50 px-3 py-2 text-xs text-amber-900">压缩或二进制正文以 Base64 完整保存。</p>
              <p class="break-all border-b p-3 font-mono text-xs">{{ item.value.method }} {{ item.value.url }}</p>
              <p v-if="item.value.credentials?.length" class="border-b px-3 py-2 text-[11px] text-muted-foreground">携带凭证：{{ item.value.credentials.map(c => c.kind === 'header' ? `${c.name}${c.scheme ? ` (${c.scheme})` : ''}` : c.kind === 'query' ? `?${c.name}=` : 'URL userinfo').join('、') }} · 值未保存</p>
              <details class="border-b"><summary class="cursor-pointer px-3 py-2 text-xs">请求头 · Content-Length {{ item.value.content_length }}</summary><pre class="max-h-48 overflow-auto whitespace-pre-wrap break-words p-3 font-mono text-[11px]">{{ JSON.stringify(item.value.headers, null, 2) }}</pre></details>
              <pre class="max-h-[36rem] overflow-auto whitespace-pre-wrap break-words bg-muted/20 p-3 font-mono text-xs [overflow-wrap:anywhere]" :aria-label="item.label + '正文'" tabindex="0">{{ formatBody(item.value.body) || '(empty)' }}</pre>
            </template>
            <p v-else class="p-3 text-xs text-muted-foreground">未转发</p>
          </section>
        </div>
      </template>
      <template v-else-if="mode === 'curl'"><p v-if="snapshot?.method === 'WS'" class="rounded-lg border p-3 text-xs text-muted-foreground">WebSocket 消息包含连接内状态，不能直接导出为 HTTP cURL。可从对比视图复制完整原始消息，响应帧可在详情中下载。</p><CurlExportPanel v-else :trace-id="capture.trace_id" :source="source" :available="source === 'original' || Boolean(capture.outgoing)" /></template>
      <ContextComparison v-else-if="mode === 'context'" :trace-id="capture.trace_id" :logs="logs" />
      <template v-else-if="mode === 'replay'">
        <ReplayWorkbench v-if="snapshot" :trace-id="capture.trace_id" :source="source" :snapshot="snapshot" :refresh-key="replayRefreshKey" @select="trace => emit('select', trace)" />
        <p v-else class="rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">此请求从未出站，只能重放客户端原始请求。</p>
      </template>
    </template>
  </div>
</template>
