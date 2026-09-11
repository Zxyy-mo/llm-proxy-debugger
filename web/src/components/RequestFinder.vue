<script setup lang="ts">
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import type { RequestOrder } from '@/lib/requestFinder'

defineProps<{ matched: number; total: number }>()
const query = defineModel<string>('query', { required: true })
const status = defineModel<string>('status', { required: true })
const order = defineModel<RequestOrder>('order', { required: true })
function reset() { query.value = ''; status.value = ''; order.value = 'newest' }
</script>

<template>
  <div class="min-w-0 space-y-2" aria-label="请求筛选">
    <Input v-model="query" aria-label="查找请求" placeholder="模型、摘要、路径或 Trace" class="h-8 min-w-0 text-xs" />
    <div class="grid min-w-0 grid-cols-2 gap-2">
      <select v-model="status" aria-label="请求状态" class="h-8 min-w-0 rounded-md border bg-background px-1.5 text-[11px]">
        <option value="">全部状态</option><option value="error">失败 / 中断</option><option value="pending">等待放行</option><option value="running">进行中</option><option value="done">已完成</option><option value="canceled">已取消</option>
      </select>
      <select v-model="order" aria-label="请求排序" class="h-8 min-w-0 rounded-md border bg-background px-1.5 text-[11px]">
        <option value="newest">最新在前</option><option value="oldest">最早在前</option><option value="duration-desc">耗时最长在前</option><option value="duration-asc">耗时最短在前</option>
      </select>
    </div>
    <div class="flex min-w-0 flex-wrap items-center justify-between gap-1 text-[10px] text-muted-foreground">
      <span aria-live="polite">当前会话 {{ matched }} / {{ total }} 条</span>
      <Button v-if="query || status || order !== 'newest'" variant="ghost" size="sm" class="h-6 px-2 text-[10px]" @click="reset">清除筛选</Button>
    </div>
  </div>
</template>
