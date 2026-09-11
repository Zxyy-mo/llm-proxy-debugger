<script setup lang="ts">
import { computed, onUnmounted, ref, watch } from 'vue'
import { Button } from '@/components/ui/button'
import { fetchRuns } from '@/lib/api'
import { UNASSOCIATED_RUN } from '@/lib/callLayers'
import type { RunsSnapshot, RunSummary } from '@/lib/types'

const props = defineProps<{ sessionId: string; refreshKey: number }>()
const selected = defineModel<string>({ required: true })
const data = ref<RunsSnapshot | null>(null)
const loading = ref(false)
const error = ref('')
let controller: AbortController | undefined
let timer: ReturnType<typeof setTimeout> | undefined
const selectedRun = computed(() => data.value?.runs.find(run => run.id === selected.value))
const sortedRuns = computed(() => [...(data.value?.runs ?? [])].sort((a, b) => Date.parse(b.last_at) - Date.parse(a.last_at)))

function label(run: RunSummary): string {
  if (run.id.startsWith('run:replay:')) return `重放任务 ${run.id.slice(-8)}`
  return run.external_id || `任务 ${run.id.slice(-8)}`
}

// load 丢弃已切换会话的旧响应；任务查询失败时保留当前选择，让用户可重试。
async function load() {
  controller?.abort()
  const request = controller = new AbortController()
  const session = props.sessionId
  loading.value = true
  error.value = ''
  try {
    const result = await fetchRuns(session, request.signal)
    if (!request.signal.aborted && props.sessionId === session) data.value = result
  } catch (e) {
    if (!request.signal.aborted) error.value = `任务列表加载失败：${String(e)}`
  } finally {
    if (controller === request) loading.value = false
  }
}

watch(() => props.sessionId, () => {
  clearTimeout(timer)
  data.value = null
  void load()
}, { immediate: true })
watch(() => props.refreshKey, () => {
  clearTimeout(timer)
  timer = setTimeout(() => { void load() }, 200)
})
onUnmounted(() => { clearTimeout(timer); controller?.abort() })
</script>

<template>
  <section class="run-navigator shrink-0 space-y-1 border-b bg-muted/20 px-3 py-2 text-[11px]" aria-label="任务筛选">
    <div class="run-selector-row flex min-w-0 items-center gap-2">
      <label for="active-run" class="shrink-0 font-medium">一轮任务</label>
      <select id="active-run" v-model="selected" class="h-8 min-w-0 flex-1 rounded-md border bg-background px-2 text-xs sm:max-w-sm" aria-label="选择任务">
        <option value="">全部任务与未关联请求</option>
        <option :value="UNASSOCIATED_RUN">未关联任务（{{ data?.unassociated_trace_ids.length ?? '…' }}）</option>
        <option v-if="selected && selected !== UNASSOCIATED_RUN && !selectedRun" :value="selected">{{ loading ? '正在读取选中任务…' : '选中任务已不在当前会话' }}</option>
        <option v-for="run in sortedRuns" :key="run.id" :value="run.id">{{ label(run) }} · {{ run.request_count }} 请求</option>
      </select>
      <span v-if="loading && !data" role="status" class="text-muted-foreground">加载中…</span>
    </div>
    <div class="run-count-row flex flex-wrap items-center justify-between gap-x-2 text-muted-foreground">
      <p v-if="selectedRun">{{ selectedRun.request_count }} 请求 · {{ selectedRun.attempt_count }} 次上游尝试 · {{ selectedRun.active_count }} 进行中</p>
      <p v-else-if="data">{{ data.runs.length }} 轮任务 · {{ data.unassociated_trace_ids.length }} 个请求未关联</p>
      <Button v-if="selected" size="sm" variant="ghost" class="h-6 px-1 text-[11px]" aria-label="清除任务筛选" @click="selected = ''">清除筛选</Button>
    </div>
    <div v-if="error" role="alert" class="flex flex-wrap items-center gap-2 text-destructive"><span class="min-w-0 break-words">{{ error }}</span><Button size="sm" variant="outline" class="h-7 text-[11px]" :disabled="loading" @click="load">重试</Button></div>
    <p v-else-if="selectedRun && (selectedRun.partial || selectedRun.session_state !== 'known')" class="break-words text-muted-foreground"><span v-if="selectedRun.partial">本任务还有当前会话外的请求。 </span><span v-if="selectedRun.session_state === 'unknown'">会话归属尚未确定。</span><span v-else-if="selectedRun.session_state === 'conflict'">会话归属存在冲突。</span></p>
    <p v-else-if="selected === UNASSOCIATED_RUN" class="text-muted-foreground">缺少有效任务标识的请求保留在这里；共享会话不会自动归为同一轮。</p>
    <p v-else-if="!selected" class="text-muted-foreground">任务筛选作用于请求列表，画布展示会话内的完整关联。</p>
  </section>
</template>

<style scoped>
/* 短横屏用可用宽度容纳统计；竖屏仍让选择框独占一行，避免只剩下拉箭头。 */
@media (min-width: 640px) and (max-height: 500px) {
  .run-navigator { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 12px; padding-top: 4px; padding-bottom: 4px; }
  .run-navigator > * { margin-top: 0; }
  .run-selector-row { flex: 1 1 220px; }
  .run-count-row { gap: 8px; }
}
</style>
