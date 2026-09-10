<script setup lang="ts">
import type { RequestLog } from '@/lib/types'
import { formatDuration } from '@/lib/correlation'
import ResponsePanel from './ResponsePanel.vue'
defineProps<{ log: RequestLog }>()
</script>

<template>
  <details v-if="log.route || log.websocket" class="min-w-0 rounded-lg border bg-card text-card-foreground" aria-label="路由与传输">
    <summary class="cursor-pointer p-3 text-xs font-semibold">路由与传输 · {{ log.route?.provider_id || '默认上游' }}<span v-if="log.route?.conversion" class="ml-2 font-normal text-muted-foreground">{{ log.route.conversion }}</span></summary>
    <div class="space-y-3 border-t p-3 text-xs">
      <p v-if="log.route?.target_model" class="break-all">模型映射：{{ log.route.original_model }} → {{ log.route.target_model }}</p>
      <ol v-if="log.route?.attempts.length" class="space-y-2"><li v-for="(attempt, index) in log.route.attempts" :key="index" class="space-y-1 rounded border p-2"><div class="flex flex-wrap gap-2"><span>{{ index + 1 }}. {{ attempt.provider_id || '默认上游' }}</span><span>HTTP {{ attempt.status_code || '未收到响应' }}</span><span v-if="!log.websocket" class="text-muted-foreground">响应头 {{ formatDuration(attempt.duration_ms) }}</span></div><p class="break-all font-mono text-[10px] text-muted-foreground">{{ attempt.url }}</p><p v-if="attempt.error" class="break-words text-destructive">{{ attempt.error }}</p></li></ol>
      <div v-if="log.websocket" class="space-y-1 rounded border p-3"><p>WebSocket · lane {{ log.websocket.stream_id || '（默认）' }}</p><p class="break-all text-[10px] text-muted-foreground">连接 {{ log.websocket.connection_id }}</p><p>{{ log.websocket.frames }} 个响应消息 · {{ log.websocket.received_bytes.toLocaleString() }} 原始字节</p><p class="text-[11px] text-muted-foreground">stream_id 仅用于连接内的队列关联；父调用依据 previous_response_id 或显式父 Trace。</p></div>
      <ResponsePanel v-if="log.route?.conversion" :trace-id="log.trace_id" :status="log.status" variant="upstream" title="转换前的上游响应" />
    </div>
  </details>
</template>
