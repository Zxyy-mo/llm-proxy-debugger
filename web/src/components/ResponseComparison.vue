<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { ArrowUpRightIcon, RefreshCwIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { APIError, fetchRequestLog, fetchResponse } from '@/lib/api'
import { statusLabel } from '@/lib/correlation'
import { timing } from '@/lib/metrics'
import { boundedResponseText, comparisonMetrics, responseJSONLimit, responseObservation, responseToolFacts } from '@/lib/responseComparison'
import type { ComparisonMetric, MetricValue } from '@/lib/responseComparison'
import type { ReplayRecord, RequestLog, ResponseSnapshot } from '@/lib/types'
import ResponsePanel from './ResponsePanel.vue'

const props = withDefaults(defineProps<{
  sourceTrace: string
  replayTrace: string
  status?: string
  sourceLog?: RequestLog | null
  replayLog?: RequestLog | null
  replayRecord?: ReplayRecord
  active?: boolean
}>(), { active: true })
const emit = defineEmits<{ select: [trace: string] }>()

interface Side {
  trace: string
  log: RequestLog | null
  response: ResponseSnapshot | null
  missing: boolean
  loading: boolean
  error: string
  responseError: string
}
function emptySide(trace: string): Side {
  return { trace, log: null, response: null, missing: false, loading: false, error: '', responseError: '' }
}
const source = ref(emptySide(props.sourceTrace))
const result = ref(emptySide(props.replayTrace))
let controller: AbortController | undefined

function currentLog(side: Side, supplied?: RequestLog | null): RequestLog | null {
  if (side.missing) return null
  if (supplied?.trace_id !== side.trace) return side.log
  if (!side.log || (supplied.revision ?? 0) >= (side.log.revision ?? 0)) return supplied
  return side.log
}
const sourceLog = computed(() => currentLog(source.value, props.sourceLog))
const replayLog = computed(() => currentLog(result.value, props.replayLog))
const loading = computed(() => source.value.loading || result.value.loading)
const record = computed(() => props.replayRecord?.trace_id === props.replayTrace && props.replayRecord.replay_of === props.sourceTrace ? props.replayRecord : undefined)
const provenance = computed(() => {
  const info = replayLog.value?.replay
  return info?.of === props.sourceTrace ? info : record.value
})
const metrics = computed(() => comparisonMetrics(sourceLog.value, replayLog.value))
const sides = computed(() => [
  { key: 'source', title: '来源调用', action: '查看来源调用', state: source.value, log: sourceLog.value, status: sourceLog.value?.status, error: sourceLog.value?.error },
  { key: 'result', title: '重放调用', action: '查看重放调用', state: result.value, log: replayLog.value, status: replayLog.value?.status ?? record.value?.state, error: replayLog.value?.error || record.value?.error },
].map(side => ({ ...side, observation: responseObservation(side.log, side.state.response), tools: responseToolFacts(side.log) })))

// Keep mounted raw panels and their page offsets across same-pair navigation,
// while hidden comparisons stop initiating reads in response to live updates.
const raw = ref({ sourceTrace: '', replayTrace: '', sourceStatus: undefined as string | undefined, replayStatus: undefined as string | undefined })
watch(() => [props.active, props.sourceTrace, props.replayTrace, sourceLog.value?.status, replayLog.value?.status, props.status] as const, () => {
  if (props.active) raw.value = { sourceTrace: props.sourceTrace, replayTrace: props.replayTrace, sourceStatus: sourceLog.value?.status, replayStatus: replayLog.value?.status ?? props.status }
}, { immediate: true })

function errorText(error: unknown): string {
  return boundedResponseText(error instanceof Error ? error.message : String(error), 1024).text
}

async function load() {
  if (!props.active) return
  controller?.abort()
  const request = controller = new AbortController()
  async function read(side: Side) {
    if (!side.trace) {
      side.missing = true
      side.error = '尚未取得调用 ID。'
      return
    }
    side.loading = true
    side.error = ''
    side.responseError = ''
    const [log, capture] = await Promise.allSettled([
      fetchRequestLog(side.trace, request.signal),
      fetchResponse(side.trace, request.signal, 'client', 0, responseJSONLimit),
    ])
    if (controller !== request || request.signal.aborted) return
    if (log.status === 'fulfilled') {
      side.log = log.value
      side.missing = false
    } else {
      side.missing = log.reason instanceof APIError && log.reason.status === 404
      if (side.missing) side.log = null
      side.error = side.missing ? '此调用已删除或不在当前历史中。' : `调用信息刷新失败：${errorText(log.reason)}`
    }
    if (capture.status === 'fulfilled') side.response = capture.value
    else {
      if (capture.reason instanceof APIError && capture.reason.status === 404) side.response = null
      side.responseError = `响应摘录不可用：${errorText(capture.reason)}。可尝试刷新或查看下方原文。`
    }
    side.loading = false
  }
  await Promise.all([read(source.value), read(result.value)])
}

watch(() => [props.sourceTrace, props.replayTrace, props.status, props.sourceLog?.status, props.replayLog?.status, props.replayRecord?.state, props.active] as const, () => {
  controller?.abort()
  if (source.value.trace !== props.sourceTrace) source.value = emptySide(props.sourceTrace)
  if (result.value.trace !== props.replayTrace) result.value = emptySide(props.replayTrace)
  source.value.loading = result.value.loading = false
  if (props.active) void load()
}, { immediate: true })
onUnmounted(() => controller?.abort())

function metricText(value: MetricValue, kind: ComparisonMetric['kind']): string {
  if (value.value === null) return value.pending ? '进行中' : '未知'
  return kind === 'time' ? timing(value.value) : `${value.provenance === 'estimated' ? '≈' : ''}${value.value.toLocaleString()}`
}
function deltaText(row: ComparisonMetric): string {
  if (row.delta === null) return '—'
  const sign = row.delta > 0 ? '+' : row.delta < 0 ? '−' : ''
  const count = Math.abs(row.delta)
  return `${row.kind === 'tokens' && row.source.provenance === 'estimated' ? '≈' : ''}${sign}${row.kind === 'time' ? timing(count) : count.toLocaleString()}`
}
</script>

<template>
  <div class="min-w-0 space-y-3" aria-label="重放响应对比">
    <section class="min-w-0 overflow-hidden rounded-lg border bg-card" aria-label="重放结果评估" :aria-busy="loading">
      <header class="flex flex-wrap items-center gap-2 border-b px-3 py-2">
        <h3 class="text-sm font-semibold">重放结果评估</h3>
        <Button variant="ghost" size="sm" class="ml-auto h-7 px-2 text-xs" :disabled="loading || !active" @click="load"><RefreshCwIcon class="mr-1 h-3 w-3" />刷新对比</Button>
        <p class="w-full text-xs leading-relaxed text-muted-foreground">
          <template v-if="provenance">重放自{{ provenance.source === 'original' ? '客户端原始请求' : '实际出站请求' }} · {{ provenance.modified ? '发送前有修改' : '发送前未修改' }}。</template>
          <template v-else>尚未取得匹配的重放来源信息。</template>
          下方差值为“重放 − 来源”。
        </p>
      </header>
      <div class="grid min-w-0 divide-y xl:grid-cols-2 xl:divide-x xl:divide-y-0">
        <section v-for="side in sides" :key="side.key" class="min-w-0 space-y-3 p-3" :aria-label="side.title">
          <div class="flex flex-wrap items-center gap-2">
            <h4 class="text-xs font-semibold">{{ side.title }}</h4>
            <Badge :variant="side.status === 'error' ? 'destructive' : 'secondary'" class="text-[10px]">{{ statusLabel(side.status) }}<span v-if="side.log?.status_code" class="ml-1">{{ side.log.status_code }}</span></Badge>
            <span v-if="side.state.loading" class="text-[10px] text-muted-foreground">读取中…</span>
          </div>
          <div class="space-y-1">
            <p class="break-words text-sm font-medium [overflow-wrap:anywhere]">{{ side.log?.model || '模型未提供' }}<span v-if="side.log?.route?.target_model && side.log.route.target_model !== side.log.model" class="text-xs font-normal text-muted-foreground"> → {{ side.log.route.target_model }}</span></p>
            <p class="select-text break-all font-mono text-[10px] text-muted-foreground">{{ side.state.trace || '尚无调用 ID' }}</p>
            <p v-if="side.log" class="break-words text-[11px] text-muted-foreground [overflow-wrap:anywhere]">{{ side.log.method }} {{ side.log.path }} · {{ side.log.type }}</p>
          </div>
          <Button variant="outline" size="sm" class="h-7 px-2 text-xs" :disabled="!side.state.trace || side.state.missing" @click="emit('select', side.state.trace)">{{ side.action }}<ArrowUpRightIcon class="ml-1 h-3 w-3" /></Button>
          <p v-if="side.state.error" role="alert" class="rounded border border-amber-200 bg-amber-50 p-2 text-xs leading-relaxed text-amber-950">{{ side.state.error }}</p>
          <div v-if="side.error || side.observation.providerError" role="alert" class="space-y-1 rounded border border-red-200 bg-red-50 p-2 text-xs leading-relaxed text-red-900">
            <p class="font-medium">错误信息</p>
            <p v-if="side.error" class="whitespace-pre-wrap break-words [overflow-wrap:anywhere]">{{ boundedResponseText(side.error, 2048).text }}</p>
            <p v-if="side.observation.providerError && side.observation.providerError !== side.error" class="whitespace-pre-wrap break-words [overflow-wrap:anywhere]">{{ side.observation.providerError }}</p>
          </div>
          <div class="space-y-1.5">
            <h5 class="text-xs font-semibold">已观察输出（预览）</h5>
            <p class="text-[10px] text-muted-foreground">{{ side.observation.origin }}</p>
            <pre v-if="side.observation.output" class="panel-scroll max-h-56 rounded border bg-muted/25 p-2 font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-words [overflow-wrap:anywhere]" tabindex="0" :aria-label="`${side.title}已观察输出`">{{ side.observation.output }}</pre>
            <p v-else class="rounded border bg-muted/25 p-2 text-xs leading-relaxed text-muted-foreground">{{ side.observation.empty }}</p>
          </div>
          <details v-if="side.observation.thinking" class="min-w-0 rounded border">
            <summary class="cursor-pointer p-2 text-xs font-medium">已返回的思考内容（预览）</summary>
            <pre class="panel-scroll max-h-40 border-t bg-violet-50/30 p-2 font-mono text-[11px] leading-relaxed whitespace-pre-wrap break-words [overflow-wrap:anywhere]" tabindex="0" :aria-label="`${side.title}已返回的思考内容`">{{ side.observation.thinking }}</pre>
          </details>
          <p v-if="side.state.responseError" class="text-[11px] leading-relaxed text-amber-800">{{ side.state.responseError }}</p>
          <ul v-if="side.observation.notes.length" class="space-y-1 rounded bg-amber-50/70 p-2 text-[11px] leading-relaxed text-amber-950">
            <li v-for="note in side.observation.notes" :key="note" class="break-words [overflow-wrap:anywhere]">{{ note }}</li>
          </ul>
          <p v-if="side.tools" class="text-[11px] leading-relaxed text-muted-foreground">{{ side.tools }}</p>
        </section>
      </div>
      <section class="min-w-0 border-t p-3" aria-label="耗时与 Token 对比">
        <h4 class="mb-2 text-xs font-semibold">耗时与 Token</h4>
        <table class="w-full table-fixed text-left text-[11px]">
          <thead class="text-muted-foreground"><tr><th scope="col" class="w-[28%] pb-2 pr-2 font-medium">指标</th><th scope="col" class="pb-2 pr-2 font-medium">来源</th><th scope="col" class="pb-2 pr-2 font-medium">重放</th><th scope="col" class="pb-2 font-medium">差值</th></tr></thead>
          <tbody>
            <tr v-for="row in metrics" :key="row.key" class="border-t align-top">
              <th scope="row" class="py-2 pr-2 font-medium break-words">{{ row.label }}</th>
              <td v-for="[name, value] in [['来源', row.source], ['重放', row.result]] as const" :key="name" class="py-2 pr-2 break-words [overflow-wrap:anywhere]">
                <span class="font-mono">{{ metricText(value, row.kind) }}</span>
                <span v-if="row.kind === 'tokens' && value.value !== null" class="mt-0.5 block text-[10px] text-muted-foreground">{{ value.provenance === 'usage' ? 'Provider usage' : '字符估算' }}<template v-if="value.pending"> · 进行中</template></span>
              </td>
              <td class="py-2 break-words [overflow-wrap:anywhere]" :title="row.note" :aria-label="row.delta === null ? `无差值：${row.note}` : undefined"><span class="font-mono">{{ deltaText(row) }}</span></td>
            </tr>
          </tbody>
        </table>
        <p class="mt-2 text-[10px] leading-relaxed text-muted-foreground">“—”表示没有可比值。Token 仅在来源、模型和 Provider 一致且调用结束后计算差值；字符估算始终保留估算标记。</p>
      </section>
      <p class="border-t bg-muted/20 px-3 py-2 text-[11px] leading-relaxed text-muted-foreground">输出和思考各显示最多 8,192 个字符，服务端日志也可能已截断。相同预览不代表完整响应相同；完整原文在下方独立翻页或下载。</p>
    </section>
    <div class="grid min-w-0 gap-3 xl:grid-cols-2">
      <ResponsePanel v-if="raw.sourceTrace" :key="`source:${raw.sourceTrace}`" :trace-id="raw.sourceTrace" :status="raw.sourceStatus" title="来源响应" />
      <ResponsePanel v-if="raw.replayTrace" :key="`result:${raw.replayTrace}`" :trace-id="raw.replayTrace" :status="raw.replayStatus" title="重放响应" />
    </div>
  </div>
</template>
