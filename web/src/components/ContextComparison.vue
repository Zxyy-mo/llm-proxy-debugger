<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { APIError, fetchContextDiff } from '@/lib/api'
import { formatBody } from '@/lib/correlation'
import { findRequests } from '@/lib/requestFinder'
import type { ContextDifference, RequestLog } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'

const props = withDefaults(defineProps<{ traceId: string; logs: RequestLog[]; active?: boolean }>(), { active: true })
const base = ref('')
const candidateQuery = ref('')
const showUnchanged = ref(false)
const result = ref<ContextDifference | null>(null)
const error = ref('')
const loading = ref(false)
const noParent = ref(false)
let controller: AbortController | undefined
const candidates = computed(() => findRequests(props.logs.filter(log => log.trace_id !== props.traceId), candidateQuery.value, '', 'newest'))
const currentSession = computed(() => props.logs.find(log => log.trace_id === props.traceId)?.session_id)
const sameSession = computed(() => candidates.value.filter(log => log.session_id === currentSession.value))
const otherSessions = computed(() => candidates.value.filter(log => log.session_id !== currentSession.value))
const changes = computed(() => result.value?.diff.messages.filter(message => showUnchanged.value || message.kind !== 'unchanged') ?? [])
watch(() => props.traceId, () => { base.value = ''; candidateQuery.value = ''; result.value = null })
async function load() {
  controller?.abort()
  if (!props.active) { loading.value = false; return }
  const request = controller = new AbortController()
  error.value = ''; noParent.value = false; result.value = null; loading.value = true
  try {
    const value = await fetchContextDiff(props.traceId, base.value, request.signal)
    if (!request.signal.aborted) result.value = value
  } catch (e) {
    if (!request.signal.aborted) {
      if (e instanceof APIError && e.status === 409 && !base.value) noParent.value = true
      else error.value = String(e)
    }
  }
  finally { if (!request.signal.aborted) loading.value = false }
}
watch(() => [props.traceId, base.value, props.active], () => { void load() }, { immediate: true })
onUnmounted(() => controller?.abort())
</script>

<template>
  <section class="min-w-0 space-y-3" aria-label="相邻调用上下文对比">
    <div class="space-y-2">
      <div class="flex flex-wrap items-center justify-between gap-2"><label for="context-base" class="text-xs font-semibold">比较对象</label><Button size="sm" variant="outline" class="h-7 text-xs" :disabled="loading" @click="load">刷新对比</Button></div>
      <Input v-model="candidateQuery" aria-label="筛选比较对象" placeholder="查找模型、摘要或 Trace" class="h-8 text-xs" />
      <select id="context-base" v-model="base" class="h-9 w-full min-w-0 rounded-md border bg-background px-2 text-xs">
        <option value="">自动选择已捕获的父调用</option>
        <option v-if="base && !candidates.some(log => log.trace_id === base)" :value="base">已选 {{ base.slice(0, 8) }}（当前筛选外）</option>
        <optgroup v-if="sameSession.length" label="当前会话"><option v-for="log in sameSession" :key="log.trace_id" :value="log.trace_id">{{ new Date(log.time).toLocaleTimeString() }} · {{ log.model || log.path }} · {{ log.summary?.slice(0, 55) || log.trace_id.slice(0, 8) }}</option></optgroup>
        <optgroup v-if="otherSessions.length" label="其他会话（手动比较，不代表父子关系）"><option v-for="log in otherSessions" :key="log.trace_id" :value="log.trace_id">{{ new Date(log.time).toLocaleTimeString() }} · {{ log.model || log.path }} · {{ log.summary?.slice(0, 55) || log.trace_id.slice(0, 8) }}</option></optgroup>
      </select>
      <p class="text-[11px] text-muted-foreground">比较实际出站上下文；未出站时使用客户端原文。自动比较依据父调用关系，手动选择可跨分支比较。</p>
      <p v-if="candidateQuery && !candidates.length" class="text-[11px] text-muted-foreground">没有匹配的比较对象。<Button size="sm" variant="link" class="h-auto px-1 py-0 text-[11px]" @click="candidateQuery = ''">清除比较筛选</Button></p>
    </div>
    <p v-if="loading" class="text-xs text-muted-foreground">正在对齐消息…</p>
    <p v-if="noParent" role="status" class="rounded border bg-muted/30 p-3 text-xs leading-relaxed">没有已捕获的父调用。{{ logs.length > 1 ? '可在上方手动选择比较对象，选择不会改变调用关系。' : '继续捕获相关调用后，再选择比较对象。' }}</p>
    <p v-if="error" role="status" class="rounded border bg-muted/30 p-3 text-xs">{{ error }}</p>
    <template v-if="result">
      <p class="break-all text-[11px] text-muted-foreground">{{ result.base_trace_id.slice(0, 8) }}（{{ result.base_source === 'outgoing' ? '出站' : '原始' }}）→ {{ traceId.slice(0, 8) }}（{{ result.source === 'outgoing' ? '出站' : '原始' }}） · {{ result.selected_by === 'parent' ? '父调用' : '手动选择' }}</p>
      <p v-if="result.diff.remote_context" class="rounded border border-amber-200 bg-amber-50 p-3 text-xs text-amber-900">请求引用了服务端上下文。这里仅比较网关捕获的消息；未携带的远端历史无法判断。</p>
      <p v-if="result.diff.limited" class="rounded border p-3 text-xs">消息数量较多，已对齐相同前缀和后缀；中间区域按新增／删除展示。</p>
      <div class="flex flex-wrap gap-3 text-xs"><span class="text-emerald-700">新增 {{ result.diff.added }}</span><span class="text-red-700">删除 {{ result.diff.removed }}</span><span class="text-muted-foreground">相同 {{ result.diff.unchanged }}</span></div>
      <details v-for="field in [{ name: '系统指令', diff: result.diff.system }, { name: '工具定义', diff: result.diff.tools }]" :key="field.name" class="rounded-lg border" :open="field.diff.changed">
        <summary class="cursor-pointer p-3 text-xs font-semibold">{{ field.name }} · {{ field.diff.changed ? '有变化' : '相同' }}</summary>
        <div class="grid min-w-0 gap-px border-t bg-border lg:grid-cols-2">
          <div v-for="item in [{ label: '比较对象', body: field.diff.before }, { label: '当前请求', body: field.diff.after }]" :key="item.label" class="min-w-0 bg-card p-3"><p class="mb-2 text-[10px] text-muted-foreground">{{ item.label }}</p><pre class="max-h-72 overflow-auto whitespace-pre-wrap break-words text-[11px] [overflow-wrap:anywhere]">{{ formatBody(item.body) }}</pre></div>
        </div>
      </details>
      <label class="flex items-center gap-2 text-xs"><input v-model="showUnchanged" type="checkbox" />显示相同消息</label>
      <p v-if="!changes.length" class="p-3 text-xs text-muted-foreground">消息上下文没有变化。</p>
      <details v-for="(change, index) in changes" :key="index" class="min-w-0 rounded-lg border" :class="change.kind === 'added' ? 'border-emerald-200 bg-emerald-50/40' : change.kind === 'removed' ? 'border-red-200 bg-red-50/40' : ''">
        <summary class="cursor-pointer break-words p-3 text-xs"><strong>{{ change.kind === 'added' ? '新增' : change.kind === 'removed' ? '删除' : '相同' }}</strong> · {{ change.before_index !== undefined ? `原 #${change.before_index + 1}` : '' }} {{ change.after_index !== undefined ? `现 #${change.after_index + 1}` : '' }} <span class="ml-2 text-muted-foreground">{{ change.content.slice(0, 100) }}</span></summary>
        <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words border-t p-3 text-[11px] [overflow-wrap:anywhere]">{{ formatBody(change.content) }}</pre>
      </details>
    </template>
  </section>
</template>
