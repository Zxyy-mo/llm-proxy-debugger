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
</script>

<template>
  <section class="min-w-0 rounded-lg border bg-card" aria-label="请求差异">
    <h3 class="border-b px-3 py-2 text-xs font-semibold">变更对比 <span class="font-normal text-muted-foreground">· {{ diff.changes.length }} 项{{ diff.truncated ? '以上' : '' }}</span></h3>
    <p v-if="!diff.changes.length" class="p-3 text-xs text-muted-foreground">正文与请求头没有内容差异。</p>
    <div v-else class="max-h-80 overflow-auto">
      <div v-for="change in diff.changes" :key="change.path" class="border-b last:border-b-0">
        <p class="break-all bg-muted/40 px-3 py-1.5 font-mono text-[10px]">{{ change.path }}</p>
        <div class="grid min-w-0 sm:grid-cols-2">
          <div class="min-w-0 bg-red-50/70 p-3 text-red-950"><span class="text-[10px] text-red-700">− 原始</span><pre class="mt-1 whitespace-pre-wrap break-words font-mono text-xs [overflow-wrap:anywhere]">{{ change.before }}</pre></div>
          <div class="min-w-0 bg-emerald-50/70 p-3 text-emerald-950"><span class="text-[10px] text-emerald-700">+ 修改后</span><pre class="mt-1 whitespace-pre-wrap break-words font-mono text-xs [overflow-wrap:anywhere]">{{ change.after }}</pre></div>
        </div>
      </div>
    </div>
    <p v-if="diff.truncated" class="p-3 text-xs text-muted-foreground">已显示前 200 项变更；完整正文可在报文区域查看。</p>
  </section>
</template>
