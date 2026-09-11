<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { Button } from '@/components/ui/button'
import { fetchAttempt } from '@/lib/api'
import { formatBody } from '@/lib/correlation'
import type { AttemptDetail } from '@/lib/types'

const props = defineProps<{ traceId: string; attemptId: string }>()
const detail = ref<AttemptDetail | null>(null)
const error = ref('')
const loading = ref(false)
let controller: AbortController | undefined
const limit = 64 * 1024
const preview = computed(() => {
  const body = detail.value?.request.body ?? ''
  let end = Math.min(body.length, limit)
  // 预览边界不切断 UTF-16 代理对；完整正文继续通过下载按原字节提供。
  if (end > 0 && (body.codePointAt(end - 1) ?? 0) > 0xffff) end--
  return formatBody(body.slice(0, end))
})
const clipped = computed(() => (detail.value?.request.body.length ?? 0) > limit)
const download = computed(() => `/api/attempts/${encodeURIComponent(props.traceId)}/${encodeURIComponent(props.attemptId)}/body`)

// load 将结果绑定到具体请求和尝试，快速切换时不会把上一份出站内容展示到新尝试下。
async function load() {
  controller?.abort()
  const request = controller = new AbortController()
  detail.value = null
  error.value = ''
  loading.value = true
  try {
    const result = await fetchAttempt(props.traceId, props.attemptId, request.signal)
    if (!request.signal.aborted) detail.value = result
  } catch (e) {
    if (!request.signal.aborted) error.value = `出站内容读取失败：${String(e)}`
  } finally {
    if (controller === request) loading.value = false
  }
}
watch(() => [props.traceId, props.attemptId], () => { void load() }, { immediate: true })
onUnmounted(() => controller?.abort())
</script>

<template>
  <section class="min-w-0 space-y-2 border-t pt-3" aria-label="此次尝试的出站内容">
    <p v-if="loading" role="status" class="text-muted-foreground">正在读取此次出站…</p>
    <div v-if="error" role="alert" class="space-y-2"><p class="break-words text-destructive">{{ error }}</p><Button size="sm" variant="outline" class="h-7 text-[11px]" @click="load">重试读取</Button></div>
    <template v-if="detail">
      <p v-if="detail.request.unavailable" role="status" class="break-words text-amber-800">{{ detail.request.unavailable }}</p>
      <template v-else>
        <p class="break-all font-mono text-[10px]">{{ detail.request.method }} {{ detail.request.url }}</p>
        <div class="flex flex-wrap items-center gap-3 text-[11px]"><a :href="download" class="inline-flex min-h-7 items-center rounded-md border bg-background px-2 py-1 font-medium hover:bg-accent" download>下载此次出站正文</a><span v-if="detail.request.redacted" class="text-muted-foreground">已按捕获时的隐私策略脱敏</span><span v-if="detail.request.body_encoding === 'base64'" class="text-muted-foreground">二进制内容显示为 Base64，下载恢复对应字节</span></div>
        <details class="rounded border"><summary class="cursor-pointer p-2 text-[11px]">出站请求头（凭证值已移除）</summary><pre class="max-h-52 overflow-auto whitespace-pre-wrap break-words border-t p-2 font-mono text-[10px] [overflow-wrap:anywhere]">{{ formatBody(JSON.stringify(detail.request.headers, null, 2)) }}</pre></details>
        <p v-if="clipped" class="text-[11px] text-muted-foreground">仅预览前 65,536 个字符；完整内容可下载。</p>
        <pre class="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded border bg-background p-3 font-mono text-[11px] [overflow-wrap:anywhere]" tabindex="0" aria-label="此次出站正文">{{ preview || '（无正文）' }}</pre>
      </template>
    </template>
  </section>
</template>
