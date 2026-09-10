<script setup lang="ts">
import { Handle, Position } from '@vue-flow/core'
import { BotIcon, Link2Icon, AlertCircleIcon, WrenchIcon, RepeatIcon } from 'lucide-vue-next'
import { formatDuration, linkLabel, warningLabel, statusLabel } from '@/lib/correlation'
import type { GraphNode } from '@/lib/types'
import { totalTokenLabel, tokenLabel, timing } from '@/lib/metrics'

defineProps<{ data: GraphNode; selected: boolean }>()
defineEmits<{ activate: [node: GraphNode] }>()
</script>

<template>
  <article
    class="call-node"
    role="button"
    tabindex="0"
    :class="{ 'is-selected': selected, 'is-reference': data.kind === 'reference', 'is-error': data.status === 'error', 'is-replay': Boolean(data.replay) }"
    :aria-label="`${data.kind === 'reference' ? '父调用引用' : data.kind === 'tool' ? '工具调用' : data.replay ? '重放调用' : '调用'}：${data.model || ''} ${data.label}`"
    @keydown.enter.stop.prevent="$emit('activate', data)"
    @keydown.space.stop.prevent="$emit('activate', data)"
  >
    <Handle type="target" :position="Position.Left" :connectable="false" />
    <Handle type="source" :position="Position.Right" :connectable="false" />
    <template v-if="data.kind === 'reference'">
      <div class="flex items-center gap-2 text-amber-800">
        <Link2Icon class="h-4 w-4" />
        <span class="text-xs font-semibold">{{ data.trace_id ? '其他会话的父调用' : data.correlation.warning === 'cycle' ? '循环引用已跳过' : data.correlation.warning === 'ambiguous_parent' ? '父调用无法确定' : '未捕获的父调用' }}</span>
      </div>
      <p class="mt-3 line-clamp-2 break-all font-mono text-[11px] text-amber-950" :title="data.label">{{ data.label }}</p>
      <p class="mt-3 text-[10px] leading-relaxed text-amber-800/80">{{ data.trace_id ? '点击查看这个调用的详情' : warningLabel(data.correlation.warning) || '捕获到对应响应后自动补全' }}</p>
    </template>
    <template v-else-if="data.kind === 'tool' && data.tool">
      <div class="flex items-center gap-2 text-amber-800"><WrenchIcon class="h-4 w-4" /><span class="truncate text-xs font-semibold">{{ data.tool.name || data.tool.kind }}</span></div>
      <p class="mt-3 text-xs">{{ data.tool.status === 'requested' ? '已观察到调用，等待结果' : data.tool.status === 'result_observed' ? '已观察到工具结果' : statusLabel(data.tool.status) }}</p>
      <p class="mt-2 break-all font-mono text-[10px] text-zinc-400">{{ data.tool.call_id || data.tool.span_id }}</p>
      <div class="mt-3 border-t pt-2 text-[10px] text-muted-foreground">{{ data.tool.source === 'trace' ? '执行 tracing' : 'API 可见记录' }} · 耗时 {{ timing(data.tool.duration_ms) }}</div>
    </template>
    <template v-else>
      <div class="flex items-center gap-2">
        <span class="flex h-6 w-6 shrink-0 items-center justify-center rounded-md bg-teal-50 text-teal-700"><BotIcon class="h-3.5 w-3.5" /></span>
        <span class="min-w-0 flex-1 truncate text-xs font-semibold" :title="data.model">{{ data.model || data.protocol || 'LLM 请求' }}</span>
        <span class="status-dot" :class="`status-${data.status}`" />
        <span class="text-[10px] text-muted-foreground">{{ data.status === 'done' ? data.status_code || '完成' : statusLabel(data.status) }}</span>
      </div>
      <p class="mt-3 line-clamp-2 min-h-9 text-xs leading-[18px] text-zinc-700" :title="data.label">{{ data.label }}</p>
      <p class="mt-1 truncate font-mono text-[9px] text-zinc-400">{{ data.method }} {{ data.path }}</p>
      <div class="mt-3 flex items-center gap-3 border-t border-zinc-100 pt-2 text-[10px] text-zinc-500">
        <span class="font-mono" :title="`输入：${tokenLabel(data.token_sources?.input)}；输出：${tokenLabel(data.token_sources?.output)}`">{{ totalTokenLabel(data.input_tokens, data.output_tokens, data.token_sources) }} <span class="font-sans">tk</span></span>
        <span class="font-mono">{{ data.status === 'running' || data.status === 'pending' ? '…' : formatDuration(data.duration_ms) }}</span>
        <span v-if="data.tool_use_count" class="flex items-center gap-1"><WrenchIcon class="h-2.5 w-2.5" />{{ data.tool_use_count }}</span>
        <AlertCircleIcon v-if="data.correlation.warning" class="ml-auto h-3 w-3 text-amber-600" :aria-label="warningLabel(data.correlation.warning)" />
      </div>
      <div class="mt-2 flex items-center justify-between gap-2 text-[9px]">
        <span :class="data.correlation.confidence === 'inferred' ? 'text-sky-600' : 'text-teal-700'">{{ linkLabel(data.correlation.link_source) }}</span>
        <span v-if="data.replay" class="flex items-center gap-1 rounded bg-violet-100 px-1 py-0.5 text-violet-800" :title="`重放自 ${data.replay.of}`"><RepeatIcon class="h-2.5 w-2.5" />重放{{ data.replay.modified ? '·已修改' : '' }}</span>
        <span class="font-mono text-zinc-400">{{ data.trace_id?.slice(0, 8) }}</span>
      </div>
    </template>
  </article>
</template>

<style scoped>
.call-node { width: 256px; min-height: 174px; border: 1px solid #dce4e3; border-radius: 12px; padding: 14px 16px; background: #fff; box-shadow: 0 3px 10px #162b2610; transition: box-shadow 150ms, border-color 150ms; }
.call-node:hover { border-color: #81b4a9; box-shadow: 0 5px 18px #162b2617; }
.call-node.is-selected { border-color: #0f766e; box-shadow: 0 0 0 2px #0f766e24, 0 5px 18px #162b2617; }
.call-node.is-error { border-color: #f3b5b5; }
.call-node.is-error.is-selected { box-shadow: 0 0 0 2px #dc262625; }
.call-node.is-reference { background: #fffcf4; border-style: dashed; border-color: #d6bb7d; }
.call-node.is-replay { border-color: #c4b5fd; }
.call-node.is-replay.is-selected { border-color: #6d28d9; box-shadow: 0 0 0 2px #6d28d924, 0 5px 18px #162b2617; }
.status-dot { width: 5px; height: 5px; border-radius: 50%; background: #0d9488; }
.status-error { background: #dc2626; }
.status-pending { background: #d97706; }
.status-canceled { background: #71717a; }
.status-running { background: #3b82f6; animation: pulse 1.5s ease-in-out infinite; }
:deep(.vue-flow__handle) { width: 7px; height: 7px; background: #8dafaa; border: 2px solid #fff; }
@keyframes pulse { 50% { opacity: .35; } }
@media (prefers-reduced-motion: reduce) { .status-running { animation: none; } .call-node { transition: none; } }
</style>
