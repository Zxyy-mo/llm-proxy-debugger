<script setup lang="ts">
import { ref, watch } from 'vue'
import type { RequestLog } from '@/lib/types'
import { formatDuration } from '@/lib/correlation'
import { attemptLabel } from '@/lib/callLayers'
import { Button } from '@/components/ui/button'
import ResponsePanel from './ResponsePanel.vue'
import AttemptCapture from './AttemptCapture.vue'
const props = defineProps<{ log: RequestLog }>()
const selectedAttempt = ref('')
watch(() => props.log.trace_id, () => { selectedAttempt.value = '' })
</script>

<template>
  <details v-if="log.route || log.websocket" class="min-w-0 rounded-lg border bg-card text-card-foreground" aria-label="路由与传输">
    <summary class="cursor-pointer p-3 text-xs font-semibold">路由与上游尝试 · {{ log.route?.provider_id || '默认上游' }}<span v-if="log.route?.attempts.length" class="ml-2 font-normal text-muted-foreground">{{ log.route.attempts.length }} 次</span><span v-if="log.route?.conversion" class="ml-2 font-normal text-muted-foreground">{{ log.route.conversion }}</span></summary>
    <div class="space-y-3 border-t p-3 text-xs">
      <p v-if="log.route?.target_model" class="break-all">模型映射：{{ log.route.original_model }} → {{ log.route.target_model }}</p>
      <p class="text-[11px] leading-relaxed text-muted-foreground">以下为网关向配置上游实际发出的尝试。中转服务内部的重试与供应商切换未在此观测。</p>
      <p v-if="!log.route?.attempts.length" class="text-muted-foreground">尚无已发送的上游尝试。</p>
      <ol v-else class="space-y-3">
        <li v-for="(attempt, index) in log.route?.attempts" :key="attempt.id || index" class="min-w-0 space-y-2 rounded border p-3" aria-label="上游尝试">
          <div class="flex flex-wrap items-center gap-2"><span class="font-medium">{{ attempt.sequence || index + 1 }}. {{ attempt.provider_id || '默认上游' }}</span><span class="rounded bg-muted px-1.5 py-0.5 text-[10px]" :class="attempt.status === 'error' || attempt.status === 'interrupted' ? 'text-destructive' : 'text-muted-foreground'">{{ attemptLabel(attempt) }}</span><span>{{ attempt.transport === 'websocket' ? 'WebSocket' : attempt.status_code ? `HTTP ${attempt.status_code}` : '未收到响应头' }}</span></div>
          <p class="break-all font-mono text-[10px] text-muted-foreground">{{ attempt.request_url || attempt.url }}</p>
          <div class="flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
            <span v-if="attempt.transport !== 'websocket' && !log.websocket">{{ attempt.headers_at || !attempt.id ? '响应头' : '发送阶段' }} {{ attempt.status === 'running' && !attempt.headers_at ? '等待中' : formatDuration(attempt.duration_ms) }}</span>
            <span>结束耗时 {{ attempt.total_duration_ms !== undefined ? formatDuration(attempt.total_duration_ms) : attempt.status === 'running' ? '进行中' : '未知' }}</span>
          </div>
          <p v-if="attempt.error" class="break-words text-destructive">{{ attempt.error }}</p>
          <p v-if="!attempt.id || attempt.source === 'legacy_summary'" class="text-[11px] text-muted-foreground">旧记录仅保留摘要，缺少逐次出站内容和完整计时。</p>
          <template v-else>
            <div class="flex flex-wrap items-center justify-between gap-2"><span class="break-all font-mono text-[10px] text-muted-foreground">Attempt {{ attempt.id }}</span><Button size="sm" variant="outline" class="h-7 shrink-0 text-[11px]" :aria-expanded="selectedAttempt === attempt.id" @click="selectedAttempt = selectedAttempt === attempt.id ? '' : attempt.id">{{ selectedAttempt === attempt.id ? '收起出站内容' : '查看此次出站' }}</Button></div>
            <AttemptCapture v-if="selectedAttempt === attempt.id" :trace-id="log.trace_id" :attempt-id="attempt.id" />
          </template>
        </li>
      </ol>
      <div v-if="log.websocket" class="space-y-1 rounded border p-3"><p>WebSocket · lane {{ log.websocket.stream_id || '（默认）' }}</p><p class="break-all text-[10px] text-muted-foreground">连接 {{ log.websocket.connection_id }}</p><p>{{ log.websocket.frames }} 个响应消息 · {{ log.websocket.received_bytes.toLocaleString() }} 原始字节</p><p class="text-[11px] text-muted-foreground">stream_id 仅用于连接内的队列关联；父调用依据 previous_response_id 或显式父 Trace。</p></div>
      <ResponsePanel v-if="log.route?.conversion" :trace-id="log.trace_id" :status="log.status" variant="upstream" title="转换前的上游响应" />
    </div>
  </details>
</template>
