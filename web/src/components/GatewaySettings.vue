<script setup lang="ts">
import { ref } from 'vue'
import { Button } from '@/components/ui/button'
import HistoryPanel from './HistoryPanel.vue'
import PrivacyPanel from './PrivacyPanel.vue'
import ProviderPanel from './ProviderPanel.vue'
import type { RequestLog } from '@/lib/types'
withDefaults(defineProps<{ active?: boolean; selectedTrace?: string | null }>(), { active: true, selectedTrace: null })
const emit = defineEmits<{ select: [log: RequestLog] }>()
const tab = ref<'history' | 'privacy' | 'tracing' | 'providers'>('history')
const spanExample = JSON.stringify({
  "trace_id": "模型请求返回的 X-Gateway-Trace-ID",
  "span_id": "unique-execution-id",
  "call_id": "模型返回的 tool call id",
  "name": "lookup", "kind": "mcp", "status": "done",
  "started_at": "2026-09-10T10:00:00Z",
  "ended_at": "2026-09-10T10:00:00.125Z",
  "input": JSON.stringify({ query: 'example' }), "output": "found"
}, null, 2)
defineExpose({ openHistory: () => { tab.value = 'history' }, openProviders: () => { tab.value = 'providers' } })
</script>

<template>
  <div class="panel-scroll min-h-0 min-w-0 flex-1 space-y-4 p-3 sm:p-5" aria-label="网关管理">
    <nav class="flex flex-wrap gap-1 border-b pb-3" aria-label="管理功能"><Button size="sm" :variant="tab === 'history' ? 'secondary' : 'ghost'" @click="tab = 'history'">历史记录</Button><Button size="sm" :variant="tab === 'providers' ? 'secondary' : 'ghost'" @click="tab = 'providers'">Provider 与路由</Button><Button size="sm" :variant="tab === 'privacy' ? 'secondary' : 'ghost'" @click="tab = 'privacy'">脱敏策略</Button><Button size="sm" :variant="tab === 'tracing' ? 'secondary' : 'ghost'" @click="tab = 'tracing'">工具 tracing</Button></nav>
    <HistoryPanel v-show="tab === 'history'" :active="active && tab === 'history'" :selected-trace="selectedTrace" @select="log => emit('select', log)" />
    <PrivacyPanel v-if="tab === 'privacy'" />
    <ProviderPanel v-else-if="tab === 'providers'" />
    <section v-else-if="tab === 'tracing'" class="space-y-3 text-xs"><h2 class="text-sm font-semibold">接入真实工具执行记录</h2><p class="leading-relaxed text-muted-foreground">在 Agent 或 MCP 客户端执行工具时，向 <code>POST /api/tool-spans</code> 上报开始和完成状态。使用同一 span_id 更新一次执行，call_id 用于关联模型返回的工具调用。只有这里上报的时间才会显示为执行耗时。</p><pre class="overflow-auto whitespace-pre-wrap break-words rounded-lg border bg-muted/30 p-4 font-mono text-[11px]">{{ spanExample }}</pre><p class="text-muted-foreground">kind 支持 mcp、agent、tool；status 支持 running、done、error。开始时可以省略 ended_at，结束时必须提供。此接口记录执行事实，不调度或重跑工具。</p></section>
  </div>
</template>
