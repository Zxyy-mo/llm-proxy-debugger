<script setup lang="ts">
import ResponsePanel from './ResponsePanel.vue'
defineProps<{ sourceTrace: string; replayTrace: string; status?: string }>()
</script>

<template>
  <div class="grid min-w-0 gap-3 xl:grid-cols-2" aria-label="重放响应对比">
    <ResponsePanel :key="sourceTrace" :trace-id="sourceTrace" title="来源响应" />
    <ResponsePanel :key="replayTrace" :trace-id="replayTrace" :status="status" title="重放响应" />
  </div>
</template>
