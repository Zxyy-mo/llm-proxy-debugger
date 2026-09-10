<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { DownloadIcon, RefreshCwIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { fetchResponse, responseDownloadUrl } from '@/lib/api'
import { formatBody } from '@/lib/correlation'
import type { ResponseSnapshot } from '@/lib/types'

const props = withDefaults(defineProps<{ traceId: string; status?: string; title?: string; variant?: 'client' | 'upstream' }>(), { title: '完整响应', variant: 'client' })
const response = ref<ResponseSnapshot | null>(null)
const error = ref('')
const loading = ref(false)
let controller: AbortController | undefined
const content = computed(() => response.value?.type === 'json' && !response.value.body_encoding ? formatBody(response.value.body) : response.value?.body)

async function load() {
  controller?.abort()
  const request = controller = new AbortController()
  error.value = ''
  loading.value = true
  try {
    const data = await fetchResponse(props.traceId, request.signal, props.variant)
    if (!request.signal.aborted) response.value = data
  } catch (e) {
    if (!request.signal.aborted) error.value = String(e)
  } finally {
    if (!request.signal.aborted) loading.value = false
  }
}
watch(() => [props.traceId, props.status, props.variant], () => { response.value = null; void load() }, { immediate: true })
onUnmounted(() => controller?.abort())
</script>

<template>
  <section class="min-w-0 overflow-hidden rounded-lg border bg-card text-card-foreground" :aria-label="title">
    <div class="flex flex-wrap items-center gap-2 border-b px-3 py-2">
      <h3 class="text-xs font-semibold">{{ title }}</h3>
      <span v-if="response?.stored === 'file'" class="text-[10px] text-muted-foreground">{{ response.type.toUpperCase() }} · {{ response.bytes.toLocaleString() }} 字节</span>
      <div class="ml-auto flex flex-wrap items-center gap-2">
        <Button variant="ghost" size="sm" class="h-7 px-2 text-xs" :disabled="loading" @click="load"><RefreshCwIcon class="mr-1 h-3 w-3" />刷新</Button>
        <a v-if="response?.stored === 'file' && !response.receiving" :href="responseDownloadUrl(traceId, variant)" class="inline-flex min-h-7 items-center gap-1 rounded border px-2 text-xs font-medium hover:bg-muted" download><DownloadIcon class="h-3 w-3" />下载完整响应</a>
      </div>
    </div>
    <p v-if="error" role="alert" class="p-3 text-xs text-destructive">{{ error }}</p>
    <p v-else-if="loading && !response" class="p-3 text-xs text-muted-foreground">正在读取响应…</p>
    <template v-if="response">
      <p v-if="response.observation_warning" role="status" class="border-b bg-amber-50 px-3 py-2 text-[11px] leading-relaxed text-amber-900">{{ response.observation_warning }}</p>
      <p v-if="response.stored === 'missing'" class="p-3 text-xs text-muted-foreground">{{ response.reason || '没有保存的响应' }}</p>
      <template v-else>
        <div v-if="response.representation || response.redacted || response.decoded || response.body_encoding || response.truncated || response.receiving || !response.complete" class="space-y-1 border-b bg-muted/40 px-3 py-2 text-[11px] text-muted-foreground">
          <p v-if="response.representation === 'converted-from-openai'">这是转换后返回客户端的响应；转换前的报文在“路由与传输”中查看。网关生成的响应 ID 不能用于 Provider 历史查询。</p>
          <p v-if="response.representation === 'upstream-original'">转换前收到的上游原文。</p>
          <p v-if="response.representation === 'websocket-frames'">每行保存一个 WebSocket 消息，data 字符串保留原始消息内容和空白。字节数为 JSONL 文件大小。</p>
          <p v-if="response.redacted">{{ response.representation === 'redacted-output' ? '记录脱敏：保存的是完整输出投影，未保留原始事件流。' : '已按记录策略脱敏；下载内容也包含占位符。' }}</p>
          <p v-if="response.decoded">{{ response.content_encoding }} 已解压；下载保留解压后的完整字节。</p>
          <p v-if="response.body_encoding">非文本或未解压内容以 Base64 显示；下载保留原始字节。</p>
          <p v-if="response.truncated">正文较大，预览前 2 MiB；下载包含全部内容。</p>
          <p v-if="response.receiving">响应接收中；结束后可下载。</p>
          <p v-else-if="!response.complete">{{ response.reason || '响应未完整结束，仅保留已捕获的部分。' }}</p>
        </div>
        <pre class="max-h-[32rem] overflow-auto whitespace-pre-wrap break-words p-3 font-mono text-[11px] leading-relaxed [overflow-wrap:anywhere]" tabindex="0">{{ content || '（空响应）' }}</pre>
      </template>
    </template>
  </section>
</template>
