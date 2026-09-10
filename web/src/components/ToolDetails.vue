<script setup lang="ts">
import { WrenchIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { formatBody } from '@/lib/correlation'
import { timing } from '@/lib/metrics'
import type { ToolCall } from '@/lib/types'
defineProps<{ tools: ToolCall[] }>()
const emit = defineEmits<{ select: [trace: string] }>()
const status = (call: ToolCall) => ({ requested: '已观察到调用', result_observed: '已观察到结果', running: '执行中', done: '已完成', error: '失败' })[call.status]
</script>

<template>
  <section v-if="tools.length" class="min-w-0 space-y-2" aria-label="工具调用详情">
    <h3 class="flex items-center gap-1.5 text-xs font-semibold"><WrenchIcon class="h-3.5 w-3.5 text-amber-700" />工具调用 · {{ tools.length }}</h3>
    <details v-for="call in tools" :key="call.id" class="min-w-0 rounded-lg border" open>
      <summary class="cursor-pointer break-words p-3 text-xs"><strong>{{ call.name || call.kind }}</strong> · {{ status(call) }}<span class="ml-2 text-muted-foreground">{{ timing(call.duration_ms) }}</span></summary>
      <div class="min-w-0 space-y-2 border-t p-3 text-[11px]">
        <p class="text-muted-foreground">{{ call.source === 'trace' ? 'SDK / MCP 上报的执行记录' : call.source === 'provider' ? 'Provider 返回的工具状态' : '来自模型流量；没有执行 tracing 时，耗时未知' }}</p>
        <p class="break-all font-mono text-[10px] text-muted-foreground">{{ call.call_id || call.span_id }}</p>
        <p v-if="call.truncated" class="text-amber-800">工具参数较长，预览已截断；完整内容见响应下载。</p>
        <p class="font-semibold">输入</p><pre class="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-muted/40 p-2 [overflow-wrap:anywhere]">{{ formatBody(call.input) || '未提供' }}</pre>
        <template v-if="call.output"><p class="font-semibold">结果</p><pre class="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-muted/40 p-2 [overflow-wrap:anywhere]">{{ formatBody(call.output) }}</pre></template>
        <p v-if="call.error" class="break-words text-destructive">{{ call.error }}</p>
        <Button v-if="call.result_trace" variant="outline" size="sm" class="h-7 text-xs" @click="emit('select', call.result_trace)">查看携带结果的请求</Button>
      </div>
    </details>
  </section>
</template>
