<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { requestJSON } from '@/lib/api'

interface Policy { record: boolean; outbound: boolean; retain_raw: boolean; allow_reveal: boolean; patterns: { name: string; expression: string }[] }
const policy = ref<Policy | null>(null)
const patterns = ref('')
const error = ref('')
const notice = ref('')
const busy = ref(false)
const trace = ref('')
const source = ref('')
const restored = ref('')
async function load() { try { policy.value = await requestJSON<Policy>('/api/privacy'); patterns.value = JSON.stringify(policy.value.patterns, null, 2) } catch (e) { error.value = String(e) } }
async function save() {
  if (!policy.value) return
  busy.value = true; error.value = ''; notice.value = ''; restored.value = ''
  try { const value = { ...policy.value, patterns: JSON.parse(patterns.value), allow_reveal: policy.value.retain_raw && policy.value.allow_reveal }; policy.value = await requestJSON<Policy>('/api/privacy', 'PUT', value); notice.value = '已保存。新策略从后续请求开始生效，正在等待的请求保持原策略。' }
  catch (e) { error.value = String(e) }
  finally { busy.value = false }
}
async function restore() {
  busy.value = true; error.value = ''; restored.value = ''
  try { const result = await requestJSON<{ body: string }>('/api/privacy/restore', 'POST', { trace_id: trace.value.trim(), body: source.value }); restored.value = result.body }
  catch (e) { error.value = String(e) }
  finally { busy.value = false }
}
onMounted(() => { void load() })
</script>

<template>
  <section class="min-w-0 space-y-4" aria-label="脱敏策略">
    <h2 class="text-sm font-semibold">脱敏与原文保留</h2>
    <p class="text-xs leading-relaxed text-muted-foreground">记录脱敏控制页面和导出内容；出站脱敏会修改发送给上游的 JSON 字符串。同一鉴权范围内的相同值使用稳定占位符，历史消息和工具结果保持一致。</p>
    <p v-if="error" role="alert" class="text-xs text-destructive">{{ error }}</p><p v-if="notice" role="status" class="text-xs text-teal-800">{{ notice }}</p>
    <form v-if="policy" class="space-y-4" @submit.prevent="save">
      <div class="space-y-3 rounded-lg border p-4 text-xs">
        <label class="flex items-start gap-2"><input v-model="policy.record" type="checkbox" /><span>启用记录脱敏：隐藏请求正文中的匹配值，响应结束后显示脱敏结果。</span></label>
        <label class="flex items-start gap-2"><input v-model="policy.outbound" type="checkbox" /><span>启用出站脱敏：发送前替换匹配值，会改变 Provider 收到的上下文。</span></label>
        <label class="flex items-start gap-2"><input v-model="policy.retain_raw" type="checkbox" /><span>保留请求原文和还原映射，用于原样重放及受控还原。</span></label>
        <label class="flex items-start gap-2"><input v-model="policy.allow_reveal" :disabled="!policy.retain_raw" type="checkbox" /><span>允许通过下方操作显式还原占位符。</span></label>
      </div>
      <p class="text-[11px] leading-relaxed text-muted-foreground">启用记录脱敏后，流式原文不广播、不落盘；保存脱敏后的完整输出与思考内容。断点仍可放行或取消，正文编辑关闭。关闭原文保留后，已有历史不会自动删除，可在历史管理中清理。</p>
      <div class="space-y-2"><label for="privacy-patterns" class="text-xs font-semibold">匹配模式（JSON，Go 正则表达式）</label><Textarea id="privacy-patterns" v-model="patterns" rows="10" spellcheck="false" class="font-mono text-xs" /><p class="text-[11px] text-muted-foreground">默认包含邮箱和中国大陆手机号，可添加带名称的自定义表达式。</p></div>
      <Button type="submit" :disabled="busy">保存策略</Button>
    </form>
    <details v-if="policy?.allow_reveal" class="rounded-lg border"><summary class="cursor-pointer p-3 text-xs font-semibold">受控还原</summary><div class="space-y-3 border-t p-3"><p class="text-xs text-muted-foreground">使用该请求所属鉴权范围内保留的映射，只还原输入的占位符。结果不会自动发送给上游。</p><Input v-model="trace" aria-label="还原用的 Trace ID" placeholder="Trace ID" /><Textarea v-model="source" aria-label="待还原内容" placeholder="粘贴包含 [PRIVATE_…] 占位符的内容" rows="5" class="font-mono text-xs" /><Button :disabled="busy || !trace.trim() || !source" @click="restore">还原内容</Button><Textarea v-if="restored" :model-value="restored" readonly aria-label="还原结果" rows="5" class="font-mono text-xs" /></div></details>
  </section>
</template>
