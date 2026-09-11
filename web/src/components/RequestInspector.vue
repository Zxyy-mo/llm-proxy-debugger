<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { MousePointer2Icon, GitBranchIcon, BrainCircuitIcon, AlertCircleIcon, RepeatIcon } from 'lucide-vue-next'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { formatBody, formatDuration, linkLabel, sourceLabel, warningLabel, statusLabel } from '@/lib/correlation'
import type { LiveLog } from '@/lib/types'
import { formatTokens, timing } from '@/lib/metrics'
import ResponsePanel from './ResponsePanel.vue'
import ResponseComparison from './ResponseComparison.vue'
import ToolDetails from './ToolDetails.vue'
import RoutingDetails from './RoutingDetails.vue'
import RequestActions from './RequestActions.vue'
import RunDetails from './RunDetails.vue'
import type { InvestigationAction } from '@/lib/investigation'

const props = defineProps<{ log: LiveLog | null }>()
const emit = defineEmits<{ select: [trace: string]; action: [intent: InvestigationAction] }>()
const comparisonOpen = ref(false)
watch(() => props.log?.trace_id, () => { comparisonOpen.value = false })
const identifiers = computed(() => {
  const log = props.log
  if (!log) return []
  const c = log.correlation
  return [
    ['Request / Trace ID', log.trace_id], ['Session ID', log.session_id],
    ['Response ID', c?.response_id], ['Previous response', c?.previous_response_id],
    ['Parent trace', c?.parent_trace_id], ['Conversation', c?.conversation_id], ['Thread', c?.thread_id],
  ].filter((entry): entry is [string, string] => Boolean(entry[1]))
})
</script>

<template>
  <aside class="flex h-full min-h-0 flex-col bg-card" aria-label="调用详情">
    <div class="flex h-11 shrink-0 items-center gap-2 border-b px-4 text-xs font-semibold">
      <GitBranchIcon class="h-3.5 w-3.5 text-teal-700" />
      调用详情
      <span v-if="log" class="ml-auto font-mono font-normal text-muted-foreground">{{ log.trace_id.slice(0, 8) }}</span>
    </div>
    <div v-if="!log" class="flex flex-1 flex-col items-center justify-center gap-3 p-8 text-center text-muted-foreground">
      <MousePointer2Icon class="h-6 w-6 text-zinc-300" />
      <p class="text-sm">选择一个调用节点</p>
      <p class="max-w-52 text-xs leading-relaxed">查看关联依据、请求报文、模型响应与已返回的思考内容。</p>
    </div>
    <div v-else class="min-h-0 flex-1 space-y-5 overflow-auto p-4 text-xs">
      <div>
        <div class="mb-2 flex flex-wrap items-center gap-2">
          <span class="text-sm font-semibold">{{ log.model || '模型未提供' }}</span>
          <Badge :variant="log.status === 'error' ? 'destructive' : 'secondary'" class="text-[10px]">
            {{ statusLabel(log.status) }}
            <span v-if="log.status_code" class="ml-1">{{ log.status_code }}</span>
          </Badge>
        </div>
        <p class="break-words text-muted-foreground leading-relaxed">{{ log.summary || `${log.method} ${log.path}` }}</p>
        <p class="mt-2 break-all font-mono text-[10px] text-muted-foreground">{{ log.method }} {{ log.path }} · {{ log.type }}</p>
      </div>

      <RequestActions :log="log" @action="intent => emit('action', intent)" />
      <RunDetails :log="log" />
      <div v-if="log.error" class="rounded-lg border border-red-200 bg-red-50 p-3 text-red-800">
        <div class="mb-1 font-semibold">请求错误</div>
        <p class="break-words leading-relaxed">{{ log.error }}</p>
      </div>

      <div class="grid grid-cols-2 gap-px overflow-hidden rounded-lg border bg-border">
        <div v-for="metric in [
          ['输入 tokens', formatTokens(log.input_tokens, log.token_sources?.input)],
          ['输出 tokens', formatTokens(log.output_tokens, log.token_sources?.output)],
          ['总耗时', log.status === 'running' || log.status === 'pending' ? statusLabel(log.status) : formatDuration(log.duration_ms)],
          ['工具调用', String(log.tool_use_count ?? 0)],
          ['人工 / 规则等待', log.status === 'pending' ? '等待中' : formatDuration(log.wait_duration_ms ?? 0)],
          ['上游耗时', log.status === 'running' ? '进行中' : formatDuration(log.upstream_duration_ms ?? 0)],
          ['上游首字节', timing(log.ttfb_ms)],
          ['首有效内容', timing(log.ttfc_ms)],
        ]" :key="metric[0]" class="bg-card p-3">
          <div class="text-[10px] text-muted-foreground">{{ metric[0] }}</div>
          <div class="mt-1 font-mono text-sm font-medium">{{ metric[1] }}</div>
        </div>
      </div>

      <p v-if="log.observation_warning" role="status" class="rounded-lg bg-amber-50 p-3 leading-relaxed text-amber-900">{{ log.observation_warning }}</p>

      <section v-if="log.replay" class="space-y-2 rounded-lg border border-violet-200 bg-violet-50/60 p-3" aria-label="重放来源">
        <div class="flex items-center gap-1.5 font-semibold text-violet-900"><RepeatIcon class="h-3.5 w-3.5" />操作者重放</div>
        <p class="leading-relaxed text-violet-950">重放自{{ log.replay.source === 'original' ? '客户端原始请求' : '实际出站请求' }}{{ log.replay.modified ? '，发送前有修改' : '，内容未修改' }}。这是来源标注，不代表对话上的父子关系。</p>
        <Button size="sm" variant="outline" class="h-7 text-xs" @click="emit('select', log.replay.of)">查看被重放的请求 {{ log.replay.of.slice(0, 8) }}</Button>
      </section>

      <section class="space-y-2">
        <h3 class="font-semibold">关联依据</h3>
        <div class="rounded-lg border bg-muted/30 p-3 leading-relaxed">
          <div class="flex items-center gap-1.5">
            <span class="h-1.5 w-1.5 rounded-full" :class="log.correlation?.confidence === 'inferred' ? 'bg-sky-500' : 'bg-teal-600'" />
            {{ linkLabel(log.correlation?.link_source) }}
            <span v-if="log.correlation?.confidence === 'inferred'" class="text-muted-foreground">· 推断</span>
          </div>
          <p class="mt-1 text-[10px] text-muted-foreground">会话归属：{{ sourceLabel(log.correlation?.session_source) }}</p>
          <p v-if="!log.correlation?.link_source" class="mt-1 text-[10px] text-muted-foreground">当前没有直接父调用的证据。</p>
        </div>
        <p v-if="log.correlation?.warning" class="flex gap-2 rounded-lg bg-amber-50 p-3 text-amber-900 leading-relaxed">
          <AlertCircleIcon class="mt-0.5 h-3.5 w-3.5 shrink-0" />
          {{ warningLabel(log.correlation.warning) }}
        </p>
        <dl class="space-y-2">
          <div v-for="[label, value] in identifiers" :key="label">
            <dt class="text-[10px] text-muted-foreground">{{ label }}</dt>
            <dd class="mt-0.5 select-text break-all font-mono text-[10px] leading-relaxed">{{ value }}</dd>
          </div>
        </dl>
      </section>

      <details v-if="log.thinking_content" class="group rounded-lg border" open>
        <summary class="flex cursor-pointer items-center gap-2 p-3 font-semibold">
          <BrainCircuitIcon class="h-3.5 w-3.5 text-violet-600" /> 已返回的思考内容
          <span class="ml-auto font-mono font-normal text-muted-foreground">{{ formatTokens(log.thinking_tokens, log.token_sources?.thinking) }}</span>
        </summary>
        <pre class="max-h-72 overflow-auto whitespace-pre-wrap break-words border-t bg-violet-50/40 p-3 text-[11px] leading-relaxed text-violet-950">{{ log.thinking_content }}</pre>
      </details>

      <ToolDetails :tools="log.tools ?? []" @select="trace => emit('select', trace)" />
      <RoutingDetails :log="log" />
      <ResponsePanel :trace-id="log.trace_id" :status="log.status" />
      <details v-if="log.replay" class="rounded-lg border" :open="comparisonOpen" @toggle="comparisonOpen = ($event.target as HTMLDetailsElement).open">
        <summary class="cursor-pointer p-3 font-semibold">对比来源响应</summary>
        <ResponseComparison :source-trace="log.replay.of" :replay-trace="log.trace_id" :status="log.status" :replay-log="log" :active="comparisonOpen" @select="trace => emit('select', trace)" />
      </details>
      <details class="rounded-lg border">
        <summary class="cursor-pointer p-3 font-semibold">请求报文</summary>
        <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words border-t bg-muted/30 p-3 font-mono text-[11px] leading-relaxed">{{ formatBody(log.request_body) || '无请求正文' }}</pre>
      </details>
    </div>
  </aside>
</template>
