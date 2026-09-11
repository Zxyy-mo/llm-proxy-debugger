<script setup lang="ts">
import type { RequestLog } from '@/lib/types'
import { runEvidence, runLabel } from '@/lib/callLayers'
defineProps<{ log: RequestLog }>()
</script>

<template>
  <section class="min-w-0 space-y-2 rounded-lg border bg-muted/20 p-3 text-xs" aria-label="任务归属">
    <p class="text-[10px] text-muted-foreground">会话 → 一轮任务 → 模型请求 → 上游尝试</p>
    <p class="break-words font-semibold">{{ runLabel(log.run_id, log.run) }}</p>
    <p class="break-words text-[11px] leading-relaxed text-muted-foreground">{{ runEvidence(log.run) }}</p>
    <details v-if="log.run_id" class="text-[10px] text-muted-foreground">
      <summary class="cursor-pointer">任务 ID 与请求归属</summary>
      <dl class="mt-2 space-y-1 break-all font-mono">
        <div><dt class="inline">会话：</dt><dd class="inline">{{ log.session_id }}</dd></div>
        <div><dt class="inline">Run：</dt><dd class="inline">{{ log.run_id }}</dd></div>
        <div><dt class="inline">Request / Trace：</dt><dd class="inline">{{ log.trace_id }}</dd></div>
      </dl>
    </details>
  </section>
</template>
