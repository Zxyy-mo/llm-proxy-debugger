<script setup lang="ts">
import { onUnmounted, ref, watch } from 'vue'
import { CopyIcon, DownloadIcon, TerminalIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { bodyDownloadUrl, fetchCurlExport } from '@/lib/api'
import type { CurlExport, ReplaySource } from '@/lib/types'

const props = defineProps<{ traceId: string; source: ReplaySource; available: boolean }>()
const result = ref<CurlExport | null>(null)
const error = ref('')
const loading = ref(false)
const copied = ref('')
let controller: AbortController | undefined
let copiedTimer: ReturnType<typeof setTimeout> | undefined

async function load() {
  controller?.abort()
  const request = controller = new AbortController()
  error.value = ''
  result.value = null
  if (!props.available) return
  loading.value = true
  try {
    const data = await fetchCurlExport(props.traceId, props.source, request.signal)
    if (!request.signal.aborted) result.value = data
  } catch (e) {
    if (!request.signal.aborted) error.value = String(e)
  } finally {
    if (!request.signal.aborted) loading.value = false
  }
}

async function copy(text: string, label: string) {
  try {
    await navigator.clipboard.writeText(text)
    copied.value = label
  } catch {
    copied.value = ''
    error.value = '浏览器拒绝访问剪贴板，请手动选择命令文本复制。'
    return
  }
  clearTimeout(copiedTimer)
  copiedTimer = setTimeout(() => { copied.value = '' }, 2000)
}

function exportsBlock(data: CurlExport): string {
  return data.environment.map(item => `export ${item.name}='…'`).join('\n')
}

watch(() => [props.traceId, props.source, props.available], () => { void load() }, { immediate: true })
onUnmounted(() => { controller?.abort(); clearTimeout(copiedTimer) })
</script>

<template>
  <section class="min-w-0 rounded-lg border bg-card" aria-label="cURL 导出">
    <div class="flex flex-wrap items-center gap-2 border-b px-3 py-2">
      <TerminalIcon class="h-3.5 w-3.5 text-muted-foreground" />
      <h3 class="text-xs font-semibold">cURL · {{ source === 'original' ? '客户端原始请求 → 网关' : '实际出站请求 → 上游' }}</h3>
      <span v-if="copied" role="status" class="text-[10px] text-teal-700">已复制{{ copied }}</span>
      <div class="ml-auto flex flex-wrap gap-1">
        <Button v-if="result" size="sm" variant="outline" class="h-7 text-xs" @click="copy(result.command, '命令')"><CopyIcon class="mr-1 h-3 w-3" />复制命令</Button>
        <Button v-if="result?.environment.length" size="sm" variant="ghost" class="h-7 text-xs" @click="copy(exportsBlock(result), '环境变量模板')">复制变量模板</Button>
        <a v-if="result && result.body_mode !== 'none'" :href="bodyDownloadUrl(traceId, source)" :download="result.body_file || `replay-${traceId.slice(0, 8)}-${source}.bin`" class="inline-flex h-7 items-center rounded-md border px-2 text-xs hover:bg-accent"><DownloadIcon class="mr-1 h-3 w-3" />下载正文文件</a>
      </div>
    </div>
    <p v-if="!available" class="p-3 text-xs text-muted-foreground">此请求尚未出站，没有可导出的出站命令。</p>
    <p v-else-if="loading && !result" class="p-3 text-xs text-muted-foreground">正在生成命令…</p>
    <p v-if="error" role="alert" class="border-b p-3 text-xs text-destructive">{{ error }}</p>
    <template v-if="result">
      <p class="break-all border-b px-3 py-2 font-mono text-[11px]"><span class="text-muted-foreground">{{ result.destination.kind === 'gateway' ? '网关' : '上游' }} · </span>{{ result.destination.url }}</p>
      <div v-if="result.environment.length" class="border-b px-3 py-2 text-xs">
        <p class="font-semibold">运行前需要设置的环境变量</p>
        <ul class="mt-1 space-y-1">
          <li v-for="item in result.environment" :key="item.name" class="flex flex-wrap gap-x-2 break-all font-mono text-[11px]">
            <span class="font-semibold">{{ item.name }}</span>
            <span class="text-muted-foreground">{{ item.description }}</span>
            <span v-if="!item.replayable" class="text-amber-700">需要重新签名</span>
          </li>
        </ul>
      </div>
      <pre class="max-h-72 overflow-auto whitespace-pre-wrap break-words p-3 font-mono text-[11px] leading-relaxed [overflow-wrap:anywhere]" :aria-label="`${source === 'original' ? '原始' : '出站'} cURL 命令`" tabindex="0">{{ result.command }}</pre>
      <ul class="space-y-1 border-t px-3 py-2 text-[11px] leading-relaxed text-muted-foreground">
        <li v-for="note in result.notes" :key="note">{{ note }}</li>
        <li>正文 {{ result.body_bytes.toLocaleString() }} 字节 · {{ result.body_mode === 'inline' ? '内联在命令中' : result.body_mode === 'file' ? `从文件 ${result.body_file} 读取` : '无正文' }}</li>
      </ul>
    </template>
  </section>
</template>
