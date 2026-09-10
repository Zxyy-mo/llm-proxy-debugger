<script setup lang="ts">
import { onMounted, onUnmounted, ref } from 'vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { requestJSON } from '@/lib/api'
import { formatDuration, statusLabel } from '@/lib/correlation'
import type { RequestLog } from '@/lib/types'

interface HistoryResult { items: RequestLog[]; total: number; next_cursor: string; storage: { enabled: boolean; last_saved_at?: string; error?: string } }
const emit = defineEmits<{ select: [log: RequestLog] }>()
const query = ref('')
const status = ref('')
const result = ref<HistoryResult | null>(null)
const cursor = ref('')
const error = ref('')
const notice = ref('')
const busy = ref(false)
const deleteTrace = ref('')
const cleanupBefore = ref('')
const confirmCleanup = ref(false)
let controller: AbortController | undefined

async function load(next = '') {
  controller?.abort()
  const request = controller = new AbortController()
  busy.value = true; error.value = ''
  try {
    const data = await requestJSON<HistoryResult>(`/api/history?${new URLSearchParams({ q: query.value, status: status.value, limit: '50', cursor: next })}`, 'GET', undefined, request.signal)
    if (!request.signal.aborted) { result.value = data; cursor.value = next }
  } catch (e) { if (!request.signal.aborted) error.value = String(e) }
  finally { if (!request.signal.aborted) busy.value = false }
}
async function remove(trace: string) {
  busy.value = true; error.value = ''
  try { await requestJSON(`/api/history/${encodeURIComponent(trace)}`, 'DELETE'); deleteTrace.value = ''; notice.value = '已删除记录及对应正文文件。'; await load() }
  catch (e) { error.value = String(e) }
  finally { busy.value = false }
}
async function cleanup() {
  if (!cleanupBefore.value) return
  busy.value = true; error.value = ''
  try {
    const value = await requestJSON<{ deleted: number; active_skipped: number }>('/api/history/cleanup', 'POST', { before: new Date(cleanupBefore.value).toISOString() })
    notice.value = `已清理 ${value.deleted} 条记录；跳过 ${value.active_skipped} 条进行中的请求。`
    confirmCleanup.value = false; await load()
  } catch (e) { error.value = String(e) }
  finally { busy.value = false }
}
onMounted(() => { void load() })
onUnmounted(() => controller?.abort())
</script>

<template>
  <section class="min-w-0 space-y-4" aria-label="历史查询与清理">
    <div class="flex flex-wrap items-center gap-2"><h2 class="text-sm font-semibold">历史记录</h2><span v-if="result" class="text-xs text-muted-foreground">{{ result.total.toLocaleString() }} 条 · {{ result.storage.enabled ? 'SQLite 持久化' : '当前仅保存在内存' }}</span><Button size="sm" variant="outline" class="ml-auto" :disabled="busy" @click="load()">刷新</Button></div>
    <p class="text-xs text-muted-foreground">按模型、路径、摘要、Trace、Response ID 或会话标识查询。重启前未结束的请求会标记为中断，不会自动补发。</p>
    <p v-if="result?.storage.error" role="alert" class="rounded border border-red-200 bg-red-50 p-3 text-xs text-red-900">历史保存失败：{{ result.storage.error }}</p>
    <form class="flex flex-wrap gap-2" @submit.prevent="load()"><Input v-model="query" aria-label="历史关键词" placeholder="搜索模型、路径、摘要或 Trace" class="min-w-40 flex-1" /><select v-model="status" aria-label="历史状态" class="rounded-md border bg-background px-2 text-xs"><option value="">全部状态</option><option value="done">已完成</option><option value="error">失败 / 中断</option><option value="canceled">已取消</option><option value="running">进行中</option><option value="pending">等待放行</option></select><Button :disabled="busy" type="submit">查询</Button></form>
    <p v-if="error" role="alert" class="text-xs text-destructive">{{ error }}</p><p v-if="notice" role="status" class="text-xs text-teal-800">{{ notice }}</p>
    <div class="divide-y rounded-lg border">
      <p v-if="result && !result.items.length" class="p-6 text-center text-xs text-muted-foreground">没有匹配的历史记录。</p>
      <article v-for="log in result?.items ?? []" :key="log.trace_id" class="flex min-w-0 flex-wrap items-center gap-3 p-3">
        <button class="min-w-0 flex-1 text-left hover:text-teal-700" @click="emit('select', log)"><p class="truncate text-xs font-medium">{{ log.model || log.path }} · {{ log.summary || log.trace_id }}</p><p class="mt-1 break-all text-[10px] text-muted-foreground">{{ new Date(log.time).toLocaleString() }} · {{ log.trace_id.slice(0, 8) }} · {{ statusLabel(log.status) }} {{ log.status_code || '' }} · {{ formatDuration(log.duration_ms) }}</p></button>
        <Button size="sm" variant="outline" @click="emit('select', log)">查看</Button>
        <template v-if="log.status !== 'running' && log.status !== 'pending'"><Button v-if="deleteTrace !== log.trace_id" size="sm" variant="ghost" :disabled="busy" @click="deleteTrace = log.trace_id">删除</Button><template v-else><Button size="sm" variant="destructive" :disabled="busy" @click="remove(log.trace_id)">确认删除</Button><Button size="sm" variant="ghost" @click="deleteTrace = ''">取消</Button></template></template>
      </article>
    </div>
    <div class="flex gap-2"><Button v-if="cursor" size="sm" variant="outline" :disabled="busy" @click="load()">返回第一页</Button><Button v-if="result?.next_cursor" size="sm" variant="outline" :disabled="busy" @click="load(result.next_cursor)">下一页</Button></div>
    <details class="rounded-lg border"><summary class="cursor-pointer p-3 text-xs font-semibold">清理历史</summary><div class="space-y-3 border-t p-3"><p class="text-xs text-muted-foreground">删除指定时间之前的已结束记录、正文文件与相关还原映射。进行中的请求会保留。</p><label class="block text-xs" for="cleanup-before">清理此时间之前的记录</label><input id="cleanup-before" v-model="cleanupBefore" type="datetime-local" class="max-w-full rounded border bg-background p-2 text-xs" /><div class="flex flex-wrap gap-2"><Button v-if="!confirmCleanup" size="sm" variant="outline" :disabled="!cleanupBefore || busy" @click="confirmCleanup = true">清理此前记录</Button><template v-else><Button size="sm" variant="destructive" :disabled="busy || !cleanupBefore" @click="cleanup">确认永久清理</Button><Button size="sm" variant="ghost" @click="confirmCleanup = false">取消</Button></template></div></div></details>
  </section>
</template>
