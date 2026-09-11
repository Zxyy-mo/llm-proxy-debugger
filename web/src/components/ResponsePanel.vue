<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { ChevronLeftIcon, ChevronRightIcon, DownloadIcon, RefreshCwIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { fetchResponse, responseDownloadUrl } from '@/lib/api'
import { formatBody } from '@/lib/correlation'
import type { ResponseSnapshot } from '@/lib/types'

const props = withDefaults(defineProps<{ traceId: string; status?: string; title?: string; variant?: 'client' | 'upstream' }>(), { title: '完整响应', variant: 'client' })
const response = ref<ResponseSnapshot | null>(null)
const error = ref('')
const loading = ref(false)
const pageOffset = ref(0)
const previousOffsets = ref<number[]>([])
const bodyElement = ref<HTMLElement | null>(null)
const pageBytes = 256 * 1024
let controller: AbortController | undefined
const wholeJSON = computed(() => {
  const data = response.value
  return data?.type === 'json' && data.offset === 0 && data.end === data.bytes && data.complete && !data.receiving && !data.body_encoding
})
const content = computed(() => {
  const body = response.value?.body ?? ''
  return wholeJSON.value ? formatBoundedJSON(body) : body
})
const byteRange = computed(() => {
  const data = response.value
  if (!data) return ''
  if (data.bytes === 0) return '0 字节'
  if (data.end === data.offset) return `已到末尾 · 共 ${data.bytes.toLocaleString()} 字节`
  return `字节 ${(data.offset + 1).toLocaleString()}–${data.end.toLocaleString()} / ${data.bytes.toLocaleString()}`
})

// Keep pretty-printing bounded as well as fetching: deeply nested JSON must
// not turn a small capture into a huge string of indentation in the DOM.
function formatBoundedJSON(body: string): string {
  let budget = pageBytes - body.length
  let depth = 0
  let quoted = false
  let escaped = false
  for (const char of body) {
    if (quoted) {
      if (escaped) escaped = false
      else if (char === '\\') escaped = true
      else if (char === '"') quoted = false
    } else if (char === '"') {
      quoted = true
    } else if (char === '{' || char === '[') {
      budget -= 1 + 2 * ++depth
    } else if (char === '}' || char === ']') {
      budget -= 1 + 2 * Math.max(0, --depth)
    } else if (char === ',') {
      budget -= 1 + 2 * depth
    } else if (char === ':') {
      budget--
    }
    if (budget < 0) return body
  }
  return formatBody(body)
}

type Navigation = 'refresh' | 'next' | 'previous' | 'first' | 'last'

async function load(offset = pageOffset.value, navigation: Navigation = 'refresh') {
  controller?.abort()
  const request = controller = new AbortController()
  error.value = ''
  loading.value = true
  try {
    const data = await fetchResponse(props.traceId, request.signal, props.variant, offset, pageBytes)
    if (request !== controller || request.signal.aborted) return
    if (navigation === 'first') previousOffsets.value = []
    else if (navigation === 'previous') previousOffsets.value.pop()
    else if ((navigation === 'next' || navigation === 'last') && offset !== pageOffset.value) previousOffsets.value.push(pageOffset.value)
    pageOffset.value = data.stored === 'file' ? data.offset : offset
    response.value = data
    if (navigation !== 'refresh') bodyElement.value?.scrollTo({ top: 0, left: 0 })
  } catch (e) {
    if (request === controller && !request.signal.aborted) error.value = String(e)
  } finally {
    if (request === controller && !request.signal.aborted) loading.value = false
  }
}

function previous() {
  const offset = previousOffsets.value.at(-1)
  if (offset !== undefined) void load(offset, 'previous')
}

watch(() => [props.traceId, props.variant, props.status] as const, (current, old) => {
  if (!old || current[0] !== old[0] || current[1] !== old[1]) {
    response.value = null
    pageOffset.value = 0
    previousOffsets.value = []
  }
  void load()
}, { immediate: true })
onUnmounted(() => controller?.abort())
</script>

<template>
  <section class="min-w-0 overflow-hidden rounded-lg border bg-card text-card-foreground" :aria-label="title" :aria-busy="loading">
    <div class="flex flex-wrap items-center gap-2 border-b px-3 py-2">
      <h3 class="text-xs font-semibold">{{ title }}</h3>
      <span v-if="response?.stored === 'file'" class="text-[10px] text-muted-foreground">{{ response.type.toUpperCase() }} · {{ response.bytes.toLocaleString() }} 字节</span>
      <div class="ml-auto flex flex-wrap items-center gap-2">
        <Button variant="ghost" size="sm" class="h-7 px-2 text-xs" :disabled="loading" @click="load()"><RefreshCwIcon class="mr-1 h-3 w-3" />刷新</Button>
        <a v-if="response?.stored === 'file' && !response.receiving" :href="responseDownloadUrl(traceId, variant)" class="inline-flex min-h-7 items-center gap-1 rounded border px-2 text-xs font-medium hover:bg-muted" download><DownloadIcon class="h-3 w-3" />下载完整响应</a>
      </div>
    </div>
    <p v-if="error" role="alert" class="p-3 text-xs text-destructive">{{ error }}</p>
    <p v-else-if="loading && !response" class="p-3 text-xs text-muted-foreground">正在读取响应…</p>
    <template v-if="response">
      <p v-if="response.observation_warning" role="status" class="border-b bg-amber-50 px-3 py-2 text-[11px] leading-relaxed text-amber-900">{{ response.observation_warning }}</p>
      <p v-if="response.stored === 'missing'" class="p-3 text-xs text-muted-foreground">{{ response.reason || '没有保存的响应' }}</p>
      <template v-else>
        <div v-if="response.representation || response.redacted || response.decoded || response.body_encoding || response.truncated || response.receiving || !response.complete || (response.type === 'json' && !wholeJSON)" class="space-y-1 border-b bg-muted/40 px-3 py-2 text-[11px] text-muted-foreground">
          <p v-if="response.representation === 'converted-from-openai'">这是转换后返回客户端的响应；转换前的报文在“路由与传输”中查看。网关生成的响应 ID 不能用于 Provider 历史查询。</p>
          <p v-if="response.representation === 'upstream-original'">转换前收到的上游原文。</p>
          <p v-if="response.representation === 'websocket-frames'">每行保存一个 WebSocket 消息，data 字符串保留原始消息内容和空白。字节数为 JSONL 文件大小。</p>
          <p v-if="response.redacted">{{ response.representation === 'redacted-output' ? '记录脱敏：保存的是完整输出投影，未保留原始事件流。' : '已按记录策略脱敏；下载内容也包含占位符。' }}</p>
          <p v-if="response.decoded">{{ response.content_encoding }} 已解压；下载保留解压后的完整字节。</p>
          <p v-if="response.body_encoding">当前段为二进制、未解压内容或不完整的 UTF-8 字节，以 Base64 显示；下载保留原始字节。</p>
          <p v-if="response.type === 'json' && !wholeJSON && !response.body_encoding">当前显示 JSON 的一部分，保留原始文本。</p>
          <p v-if="response.receiving">响应接收中；刷新可查看当前段的更新，结束后可下载。</p>
          <p v-else-if="!response.complete">{{ response.reason || '响应未完整结束，仅保留已捕获的部分。' }}</p>
        </div>
        <nav class="flex flex-wrap items-center gap-x-3 gap-y-2 border-b px-3 py-2" :aria-label="`${title}分段导航`">
          <p role="status" aria-live="polite" aria-atomic="true" class="min-w-0 text-[11px] text-muted-foreground">{{ byteRange }}<span v-if="loading"> · 读取中…</span></p>
          <div class="flex flex-wrap items-center gap-1.5">
            <Button variant="outline" size="sm" class="h-7 px-2 text-xs" :disabled="loading || !previousOffsets.length" title="返回刚才查看的段落" @click="previous"><ChevronLeftIcon class="mr-1 h-3 w-3" />上一段</Button>
            <Button variant="outline" size="sm" class="h-7 px-2 text-xs" :disabled="loading || response.next_offset === null" @click="response.next_offset !== null && load(response.next_offset, 'next')">下一段<ChevronRightIcon class="ml-1 h-3 w-3" /></Button>
            <Button variant="ghost" size="sm" class="h-7 px-2 text-xs" :disabled="loading || response.offset === 0" @click="load(0, 'first')">回到开头</Button>
            <Button variant="ghost" size="sm" class="h-7 px-2 text-xs" :disabled="loading || response.end >= response.bytes" @click="load(response.last_offset, 'last')">查看末段</Button>
          </div>
        </nav>
        <pre ref="bodyElement" class="panel-scroll max-h-[32rem] whitespace-pre-wrap break-words p-3 font-mono text-[11px] leading-relaxed [overflow-wrap:anywhere]" tabindex="0" :aria-label="`${title}正文`">{{ content || (response.bytes === 0 ? '（空响应）' : '（当前段没有内容）') }}</pre>
      </template>
    </template>
  </section>
</template>
