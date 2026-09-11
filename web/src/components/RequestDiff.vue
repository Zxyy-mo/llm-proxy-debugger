<script setup lang="ts">
import { computed } from 'vue'
import { requestChanges } from '@/lib/requestDiff'

const props = defineProps<{
  originalBody: string
  modifiedBody: string
  originalHeaders?: Record<string, string>
  modifiedHeaders?: Record<string, string>
}>()
const diff = computed(() => requestChanges(props.originalBody, props.modifiedBody, props.originalHeaders ?? {}, props.modifiedHeaders ?? {}))
const fallbackNote = computed(() => {
  switch (diff.value.fallbackReason) {
    case 'invalid-json': return '正文包含无效 JSON，当前按原始文本比较。'
    case 'duplicate-keys': return '正文包含重复的 JSON 字段名，当前按原始文本比较。'
    case 'body-size': return '正文超过逐字段比较的大小上限，已跳过正文比较。'
    case 'depth': return '正文嵌套层级过深，已跳过正文比较。'
    case 'parse-work': return '正文结构过多，已跳过正文比较。'
    default: return ''
  }
})
</script>

<template>
  <section class="min-w-0 rounded-lg border bg-card" aria-label="请求差异">
    <h3 class="border-b px-3 py-2 text-xs font-semibold">变更对比 <span class="font-normal text-muted-foreground">· {{ diff.complete ? '' : '已列出 ' }}{{ diff.changes.length }} 项</span></h3>
    <p v-if="fallbackNote" class="border-b px-3 py-2 text-xs text-muted-foreground">{{ fallbackNote }}</p>
    <p v-if="!diff.changes.length" class="p-3 text-xs text-muted-foreground">{{ !diff.complete ? '已检查部分未发现差异；完整内容仍需核对。' : diff.bodyMode === 'text' ? '原始正文文本与请求头相同。' : '正文与请求头没有内容差异。' }}</p>
    <div v-else class="max-h-80 overflow-auto" tabindex="0" role="region" aria-label="请求变更列表">
      <div v-for="(change, index) in diff.changes" :key="index" class="border-b last:border-b-0">
        <p class="break-all bg-muted/40 px-3 py-1.5 font-mono text-[10px]">{{ change.path }}<span v-if="change.pathTruncated" class="font-sans text-muted-foreground">…（路径已截断）</span></p>
        <div class="grid min-w-0 sm:grid-cols-2">
          <div class="min-w-0 bg-red-50/70 p-3 text-red-950"><span class="text-[10px] text-red-700">− 原始{{ change.beforeTruncated ? '（仅显示开头）' : '' }}</span><pre class="mt-1 whitespace-pre-wrap break-words font-mono text-xs [overflow-wrap:anywhere]">{{ change.before }}</pre></div>
          <div class="min-w-0 bg-emerald-50/70 p-3 text-emerald-950"><span class="text-[10px] text-emerald-700">+ 修改后{{ change.afterTruncated ? '（仅显示开头）' : '' }}</span><pre class="mt-1 whitespace-pre-wrap break-words font-mono text-xs [overflow-wrap:anywhere]">{{ change.after }}</pre></div>
        </div>
      </div>
    </div>
    <p v-if="!diff.complete" class="p-3 text-xs text-muted-foreground">比较未覆盖所有内容，部分变更可能未列出；请在报文区域核对完整内容。</p>
    <p v-else-if="diff.truncated" class="p-3 text-xs text-muted-foreground">较长的路径或值仅显示开头；完整内容可在报文区域查看。</p>
  </section>
</template>
