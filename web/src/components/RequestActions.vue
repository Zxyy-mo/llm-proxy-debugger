<script setup lang="ts">
import { computed } from 'vue'
import { DiffIcon, RepeatIcon, ArrowLeftIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import type { RequestLog } from '@/lib/types'
import type { InvestigationAction } from '@/lib/investigation'

const props = defineProps<{ log: RequestLog }>()
const emit = defineEmits<{ action: [intent: InvestigationAction] }>()
const replayReason = computed(() => {
  if (props.log.method === 'WS') return 'WebSocket 含连接内状态，可查看上下文；重放需要新建独立 HTTP 模型请求。'
  if (props.log.method !== 'POST' || !/\/(messages|chat\/completions|responses)\/?$/.test(props.log.path)) return '此请求不属于可重放的 POST 模型生成接口。'
  return ''
})
function act(action: InvestigationAction['action']) { emit('action', { trace: props.log.trace_id, action }) }
</script>

<template>
  <div class="min-w-0 space-y-2" aria-label="调用操作">
    <div class="flex flex-wrap gap-1.5">
      <Button size="sm" variant="outline" class="h-8 gap-1 text-xs" @click="act('context')"><DiffIcon class="h-3.5 w-3.5" />上下文变化</Button>
      <Button size="sm" variant="outline" class="h-8 gap-1 text-xs" :disabled="Boolean(replayReason)" :title="replayReason" @click="act('replay')"><RepeatIcon class="h-3.5 w-3.5" />编辑并重放</Button>
      <template v-if="log.replay">
        <Button size="sm" variant="outline" class="h-8 text-xs" @click="act('compare')">对比来源</Button>
        <Button size="sm" variant="ghost" class="h-8 gap-1 text-xs" @click="act('source')"><ArrowLeftIcon class="h-3.5 w-3.5" />返回来源</Button>
      </template>
    </div>
    <p v-if="replayReason" class="text-[10px] leading-relaxed text-muted-foreground">{{ replayReason }}</p>
  </div>
</template>
