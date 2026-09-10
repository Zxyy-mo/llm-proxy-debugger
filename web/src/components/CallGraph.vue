<script setup lang="ts">
import { computed, nextTick, onUnmounted, ref, shallowRef, watch } from 'vue'
import { useMediaQuery, useResizeObserver } from '@vueuse/core'
import { VueFlow, useVueFlow, MarkerType, type Node, type Edge } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls, ControlButton } from '@vue-flow/controls'
import { MiniMap } from '@vue-flow/minimap'
import { graphlib, layout } from '@dagrejs/dagre'
import { DownloadIcon, GitBranchIcon, LayoutDashboardIcon, Loader2Icon, RefreshCwIcon, XIcon, ZoomInIcon, ZoomOutIcon, MaximizeIcon, TargetIcon } from 'lucide-vue-next'
import { Button } from '@/components/ui/button'
import { fetchGraph } from '@/lib/api'
import { linkLabel, warningLabel } from '@/lib/correlation'
import type { CallGraph as GraphData, GraphNode, LiveLog } from '@/lib/types'
import CallGraphNode from './CallGraphNode.vue'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import '@vue-flow/minimap/dist/style.css'

const props = defineProps<{
  sessionId: string | null
  refreshKey: number
  activeTraceId: string | null
  logs: LiveLog[]
}>()
const emit = defineEmits<{ select: [node: GraphNode] }>()
const nodes = shallowRef<Node<GraphNode>[]>([])
const edges = shallowRef<Edge[]>([])
const graph = shallowRef<GraphData | null>(null)
const loading = ref(false)
const error = ref('')
const reference = ref<GraphNode | null>(null)
const compact = useMediaQuery('(max-width: 767px)')
const canvas = ref<HTMLElement | null>(null)
const viewMode = ref<'all' | 'selected' | 'manual'>('all')
const { fitView, zoomIn, zoomOut, onNodesInitialized, onMoveStart } = useVueFlow('conversation-calls')
const liveLogs = computed(() => new Map(props.logs.map((log) => [log.trace_id, log])))
const requestCount = computed(() => graph.value?.nodes.filter((node) => node.kind === 'request').length ?? 0)
let topology = ''
let fitTimer: ReturnType<typeof setTimeout> | undefined
let timer: ReturnType<typeof setTimeout> | undefined
let controller: AbortController | undefined
let generation = 0
let refreshAgain = false

function liveNode(node: GraphNode): GraphNode {
  const log = node.trace_id ? liveLogs.value.get(node.trace_id) : undefined
  if (!log || (['done', 'error', 'canceled'].includes(node.status) && ['running', 'pending'].includes(log.status))) return node
  return {
    ...node, status: log.status, status_code: log.status_code, model: log.model || node.model,
    input_tokens: log.input_tokens ?? 0, output_tokens: log.output_tokens ?? 0,
    duration_ms: log.duration_ms, tool_use_count: log.tool_use_count ?? 0,
  }
}

function arrange(data: GraphData) {
  const dag = new graphlib.Graph()
  dag.setGraph({ rankdir: 'LR', ranksep: 110, nodesep: 48, marginx: 32, marginy: 32 })
  dag.setDefaultEdgeLabel(() => ({}))
  for (const node of data.nodes) dag.setNode(node.id, { width: 256, height: 190 })
  for (const edge of data.edges) dag.setEdge(edge.source, edge.target)
  layout(dag)
  nodes.value = data.nodes.map((node) => {
    const position = dag.node(node.id)
    return {
      id: node.id, type: 'call', data: liveNode(node),
      position: { x: position.x - 128, y: position.y - 95 },
      selected: node.trace_id === props.activeTraceId,
      ariaLabel: `${node.model || '调用'} ${node.label}`,
    }
  })
}

function applyGraph(data: GraphData) {
  const signature = JSON.stringify([data.nodes.map((node) => node.id), data.edges.map((edge) => edge.id)])
  graph.value = data
  if (signature !== topology) {
    arrange(data)
    topology = signature
    scheduleFit()
  } else {
    const incoming = new Map(data.nodes.map((node) => [node.id, node]))
    nodes.value = nodes.value.map((node) => {
      const data = incoming.get(node.id) ?? node.data
      return { ...node, data: data ? liveNode(data) : undefined }
    })
  }
  edges.value = data.edges.map((edge): Edge => {
    // Replay edges are operator provenance, not conversation causality.
    const color = edge.kind === 'replay' ? '#8b5cf6' : edge.confidence === 'inferred' ? '#569ac2' : '#689c91'
    const dash = edge.kind === 'replay' ? '2 4' : edge.confidence === 'inferred' ? '6 4' : undefined
    return {
      ...edge,
      type: 'default', label: linkLabel(edge.kind),
      markerEnd: { type: MarkerType.ArrowClosed, color, width: 15, height: 15 },
      style: { stroke: color, strokeWidth: 1.6, strokeDasharray: dash },
      labelStyle: { fill: edge.kind === 'replay' ? '#6d28d9' : edge.confidence === 'inferred' ? '#397796' : '#4b756b', fontSize: 10 },
      labelBgStyle: { fill: '#f7faf9', fillOpacity: .95 }, labelBgPadding: [7, 4], labelBgBorderRadius: 4,
      selectable: false, updatable: false,
    }
  })
}

async function loadGraph() {
  if (props.sessionId === null) return
  if (controller) {
    refreshAgain = true
    return
  }
  const current = ++generation
  controller = new AbortController()
  loading.value = true
  try {
    const data = await fetchGraph(props.sessionId, controller.signal)
    if (current !== generation) return
    applyGraph(data)
    error.value = ''
  } catch (e) {
    if (current === generation && !(e instanceof DOMException && e.name === 'AbortError')) {
      error.value = e instanceof Error ? e.message : '无法加载调用图'
    }
  } finally {
    if (current === generation) {
      controller = undefined
      loading.value = false
      if (refreshAgain) {
        refreshAgain = false
        scheduleRefresh()
      }
    }
  }
}

function scheduleRefresh() {
  if (timer) return
  timer = setTimeout(() => {
    timer = undefined
    void loadGraph()
  }, 150)
}

function selectNode(node: GraphNode) {
  if (node.trace_id) {
    reference.value = null
    emit('select', node)
  } else {
    reference.value = node
  }
}

function relayout() {
  if (!graph.value) return
  arrange(graph.value)
  showAll()
}

function focusSelected() {
  viewMode.value = 'selected'
  scheduleFit()
}

function showAll() {
  viewMode.value = 'all'
  scheduleFit()
}

function scheduleFit() {
  clearTimeout(fitTimer)
  fitTimer = setTimeout(() => {
    if (!nodes.value.length || !canvas.value?.clientWidth || !canvas.value?.clientHeight) return
    if (viewMode.value === 'all') void fitView({ padding: .18, minZoom: .02, maxZoom: 1 })
    else if (viewMode.value === 'selected') fitSelected()
  }, 80)
}

function fitSelected() {
  const id = nodes.value.find((node) => node.id === props.activeTraceId)?.id ?? nodes.value[0]?.id
  if (id) void fitView({ nodes: [id], padding: .25, maxZoom: 1 })
}

function changeZoom(direction: 'in' | 'out') {
  viewMode.value = 'manual'
  if (direction === 'in') void zoomIn()
  else void zoomOut()
}

function exportGraph() {
  if (!graph.value) return
  const data = { ...graph.value, nodes: graph.value.nodes.map(liveNode) }
  const url = URL.createObjectURL(new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' }))
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `call-graph-${Date.now()}.json`
  anchor.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

onNodesInitialized(scheduleFit)
useResizeObserver(canvas, scheduleFit)
onMoveStart(({ event }) => {
  if (event && ('sourceEvent' in event ? event.sourceEvent : event)) viewMode.value = 'manual'
})

watch(() => props.sessionId, () => {
  generation++
  controller?.abort()
  controller = undefined
  clearTimeout(timer)
  timer = undefined
  refreshAgain = false
  viewMode.value = 'all'
  topology = ''
  nodes.value = []
  edges.value = []
  graph.value = null
  reference.value = null
  error.value = ''
  loading.value = false
  void loadGraph()
}, { immediate: true })

watch(() => props.refreshKey, scheduleRefresh)
watch([liveLogs, () => props.activeTraceId], () => {
  nodes.value = nodes.value.map((node) => ({
    ...node, data: node.data ? liveNode(node.data) : undefined, selected: node.data?.trace_id === props.activeTraceId,
  }))
})
watch(() => props.activeTraceId, () => {
  if (viewMode.value === 'selected') void nextTick(scheduleFit)
})

onUnmounted(() => {
  generation++
  controller?.abort()
  clearTimeout(timer)
  clearTimeout(fitTimer)
})
</script>

<template>
  <section class="graph-panel relative flex h-full min-h-0 min-w-0 flex-col" aria-label="调用路径画布">
    <div class="flex min-h-11 shrink-0 flex-wrap items-center gap-2 border-b bg-white px-3 py-1.5">
      <span class="mr-auto text-[11px] text-muted-foreground">{{ requestCount }} 次调用 <span class="mx-1 text-zinc-300">/</span> {{ graph?.edges.length ?? 0 }} 条关联</span>
      <Loader2Icon v-if="loading" class="h-3.5 w-3.5 animate-spin text-teal-600" aria-label="正在加载调用图" />
      <Button variant="ghost" size="sm" class="h-7 gap-1 px-2 text-[10px]" :aria-pressed="viewMode === 'all'" :disabled="!nodes.length" @click="showAll"><MaximizeIcon class="h-3 w-3" />显示全图</Button>
      <Button variant="ghost" size="icon" class="h-7 w-7" aria-label="刷新调用图" title="刷新调用图" :disabled="loading" @click="loadGraph"><RefreshCwIcon class="h-3.5 w-3.5" /></Button>
      <Button variant="ghost" size="icon" class="h-7 w-7" aria-label="重新布局" title="重新布局并适应画布" :disabled="!nodes.length" @click="relayout"><LayoutDashboardIcon class="h-3.5 w-3.5" /></Button>
      <Button variant="outline" size="sm" class="h-7 gap-1.5 px-2 text-[10px]" :disabled="!nodes.length" @click="exportGraph"><DownloadIcon class="h-3 w-3" /><span class="hidden sm:inline">导出 JSON</span></Button>
    </div>
    <div class="flex shrink-0 flex-wrap gap-x-4 gap-y-1 border-b border-zinc-100 bg-white/80 px-4 py-2 text-[10px] text-zinc-500">
      <span class="flex items-center gap-1.5"><i class="w-4 border-t-2 border-teal-600/70" />明确 ID</span>
      <span class="flex items-center gap-1.5"><i class="w-4 border-t-2 border-dashed border-sky-500/70" />历史推断</span>
      <span class="flex items-center gap-1.5"><i class="h-2 w-2 rounded-sm border border-dashed border-amber-500" />父调用引用</span>
      <span class="flex items-center gap-1.5"><i class="w-4 border-t-2 border-dotted border-violet-500/80" />重放来源</span>
    </div>
    <div ref="canvas" class="relative min-h-0 min-w-0 flex-1">
      <VueFlow
        id="conversation-calls" v-model:nodes="nodes" v-model:edges="edges"
        :min-zoom=".02" :max-zoom="2" :nodes-connectable="false" :edges-updatable="false"
        :delete-key-code="null" :connect-on-click="false" :nodes-focusable="false"
        @node-click="({ node }) => selectNode(node.data as GraphNode)"
      >
        <Background :gap="20" :size="1" pattern-color="#cddad6" />
        <Controls v-if="nodes.length" :show-interactive="false" :show-zoom="false" :show-fit-view="false">
          <ControlButton aria-label="放大画布" title="放大画布" @click="changeZoom('in')"><ZoomInIcon /></ControlButton>
          <ControlButton aria-label="缩小画布" title="缩小画布" @click="changeZoom('out')"><ZoomOutIcon /></ControlButton>
          <ControlButton aria-label="适应画布" title="适应画布" @click="showAll"><MaximizeIcon /></ControlButton>
          <ControlButton aria-label="定位选中调用" title="定位选中调用" @click="focusSelected"><TargetIcon /></ControlButton>
        </Controls>
        <MiniMap v-if="nodes.length > 4 && !compact" pannable zoomable :node-color="(node) => node.data?.kind === 'reference' ? '#e9d6aa' : '#80aca0'" />
        <template #node-call="{ data, selected }">
          <CallGraphNode :data="data" :selected="selected" @activate="selectNode" />
        </template>
      </VueFlow>
      <div v-if="!nodes.length && !error" class="pointer-events-none absolute inset-0 flex flex-col items-center justify-center gap-3 p-8 text-center">
        <div class="rounded-2xl border border-teal-100 bg-white p-4 text-teal-700 shadow-sm"><GitBranchIcon class="h-7 w-7" /></div>
        <h3 class="mt-1 text-sm font-semibold text-zinc-700">{{ loading ? '正在还原调用路径…' : '让对话的来路清晰可见' }}</h3>
        <p class="max-w-72 text-xs leading-relaxed text-zinc-500">{{ loading ? '加载已捕获的请求与关联。' : '通过网关发送请求后，会话 ID、前序响应和历史消息会在这里连成调用路径。' }}</p>
      </div>
      <div v-if="error" role="alert" class="absolute left-4 right-4 top-4 flex items-center gap-3 rounded-lg border border-red-200 bg-red-50 px-4 py-3 text-xs text-red-800">
        <span class="flex-1">{{ error }}</span><Button variant="outline" size="sm" class="h-7" @click="loadGraph">重试</Button>
      </div>
      <div v-if="reference" class="absolute bottom-4 left-14 right-4 rounded-lg border border-amber-200 bg-amber-50 p-3 text-xs text-amber-950 shadow-sm">
        <button class="float-right p-0.5" aria-label="关闭父调用说明" @click="reference = null"><XIcon class="h-3.5 w-3.5" /></button>
        <p class="mb-1 break-all font-mono">{{ reference.label }}</p>
        <p class="pr-5 leading-relaxed">{{ warningLabel(reference.correlation.warning) || '这个父调用尚未捕获，捕获后将自动补全。' }}</p>
      </div>
      <span v-else-if="nodes.length" class="pointer-events-none absolute bottom-4 left-14 text-[10px] text-zinc-400">拖动画布 · 滚轮缩放 · 点击节点查看详情</span>
    </div>
  </section>
</template>

<style scoped>
.graph-panel { background: #f7faf9; }
:deep(.vue-flow__controls) { overflow: hidden; border: 1px solid #e0e7e3; border-radius: 8px; box-shadow: 0 2px 6px #1830260a; }
:deep(.vue-flow__controls-button) { background: #fff; border-color: #edf0ee; color: #4d655c; width: 27px; height: 27px; }
:deep(.vue-flow__minimap) { border: 1px solid #dce6e0; border-radius: 8px; overflow: hidden; box-shadow: none; }
:deep(.vue-flow__minimap svg) { width: 130px; height: 85px; }
:deep(.vue-flow__node:focus-visible) { outline: 2px solid #0f766e; outline-offset: 4px; border-radius: 12px; }
</style>
