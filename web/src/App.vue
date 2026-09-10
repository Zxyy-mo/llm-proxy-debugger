<script setup lang="ts">
import { ref, reactive, computed, defineAsyncComponent, watch, onMounted, onUnmounted } from 'vue'
import { useMediaQuery } from '@vueuse/core'
import {
  ResizablePanelGroup,
  ResizablePanel,
  ResizableHandle,
} from '@/components/ui/resizable'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Sheet, SheetContent, SheetHeader, SheetTitle, SheetDescription } from '@/components/ui/sheet'
import RequestInspector from '@/components/RequestInspector.vue'
import InterceptionPanel from '@/components/InterceptionPanel.vue'
import RequestAudit from '@/components/RequestAudit.vue'
import ResponsePanel from '@/components/ResponsePanel.vue'
import GatewaySettings from '@/components/GatewaySettings.vue'
import RoutingDetails from '@/components/RoutingDetails.vue'
import { formatTokens, totalTokenLabel, timing } from '@/lib/metrics'
import {
  PlusIcon,
  TerminalIcon,
  ActivityIcon,
  SettingsIcon,
  Code2Icon,
  BrainCircuitIcon,
  Trash2Icon,
  ChevronUpIcon,
  GitBranchIcon,
  FileTextIcon,
  PanelRightIcon,
  Maximize2Icon,
  Minimize2Icon,
  PauseCircleIcon,
  DiffIcon,
  PencilIcon,
} from 'lucide-vue-next'
import { fetchRules, fetchSessions, createRule, updateRule, deleteRule, connectWS } from '@/lib/api'
import { formatBody, statusLabel } from '@/lib/correlation'
import type {
  LiveLog,
  LiveSession,
  GraphNode,
  RequestLog,
  Rule,
  Session,
  WSEvent,
} from '@/lib/types'

function savedSelection(): Record<string, unknown> {
  try {
    const value: unknown = JSON.parse(localStorage.getItem('llm-debugger.selection') ?? '{}')
    return value && typeof value === 'object' ? value as Record<string, unknown> : {}
  } catch {
    return {}
  }
}
const saved = savedSelection()
const sessions = reactive<Record<string, LiveSession>>({})
const activeSessionId = ref<string | null>(typeof saved.session === 'string' ? saved.session : null)
const activeLogTraceId = ref<string | null>(typeof saved.trace === 'string' ? saved.trace : null)
const rules = ref<Rule[]>([])
const consoleLogs = ref<string[]>([])
const wsStatus = ref<'connecting' | 'open' | 'closed'>('connecting')
const activeView = ref<'graph' | 'details' | 'intercept' | 'audit' | 'manage'>(saved.view === 'details' || saved.view === 'intercept' || saved.view === 'audit' || saved.view === 'manage' ? saved.view : 'graph')
const graphScope = ref<'session' | 'all'>(saved.scope === 'all' ? 'all' : 'session')
const graphRefreshKey = ref(0)
const pendingRefreshKey = ref(0)
const replayRefreshKey = ref(0)
const mobileInspectorOpen = ref(false)
const showInspector = ref(true)
const narrowLayout = useMediaQuery('(max-width: 1023px)')
const stackPayloads = useMediaQuery('(max-width: 639px), (max-width: 1023px) and (min-height: 640px)')
const utilitiesPanel = ref<InstanceType<typeof ResizablePanel> | null>(null)
const utilitiesExpanded = ref(false)
const CallGraph = defineAsyncComponent(() => import('@/components/CallGraph.vue'))
const allLogs = computed(() => Object.values(sessions).flatMap((session) => session.logs))
const pendingCount = computed(() => allLogs.value.filter(log => log.status === 'pending').length)

let ws: ReturnType<typeof connectWS> | null = null
let refreshTimer: ReturnType<typeof setTimeout> | undefined
let snapshotPoll: ReturnType<typeof setInterval> | undefined
let refreshing = false
let refreshAgain = false
let disposed = false
const observedDuringRefresh = new Set<string>()

watch([activeSessionId, activeLogTraceId, activeView, graphScope], () => {
  try {
    localStorage.setItem('llm-debugger.selection', JSON.stringify({
      session: activeSessionId.value, trace: activeLogTraceId.value, view: activeView.value, scope: graphScope.value,
    }))
  } catch {
    // Selection still works when browser storage is disabled.
  }
})

const sessionList = computed(() =>
  Object.values(sessions).sort((a, b) => {
    const at = lastLogTime(a)
    const bt = lastLogTime(b)
    return bt - at
  }),
)

const activeSession = computed<LiveSession | null>(() =>
  activeSessionId.value ? sessions[activeSessionId.value] ?? null : null,
)

const activeLog = computed<LiveLog | null>(() => {
  const s = activeSession.value
  if (!s || !activeLogTraceId.value) return null
  return s.logs.find((l) => l.trace_id === activeLogTraceId.value) ?? null
})

function lastLogTime(s: Session): number {
  if (!s.logs?.length) return new Date(s.created_at).getTime()
  const last = s.logs[0]
  if (!last) return 0
  return new Date(last.time).getTime() || 0
}

function toLiveLog(log: RequestLog): LiveLog {
  return { ...log, status: log.status ?? (log.error || log.status_code >= 400 ? 'error' : 'done') }
}

function ensureSession(id: string): LiveSession {
  if (!sessions[id]) {
    sessions[id] = {
      id,
      created_at: new Date().toISOString(),
      logs: [],
    }
  }
  return sessions[id]
}

function upsertLog(sessionId: string, trace_id: string, patch: Partial<LiveLog>) {
  const previousSession = findSessionByTrace(trace_id)
  const previous = previousSession?.logs.find((log) => log.trace_id === trace_id)
  if (patch.revision !== undefined && (previous?.revision ?? 0) > patch.revision) return
  if (previousSession && previousSession.id !== sessionId) {
    previousSession.logs = previousSession.logs.filter((log) => log.trace_id !== trace_id)
    if (activeSessionId.value === previousSession.id && activeLogTraceId.value === trace_id) activeSessionId.value = sessionId
    if (!previousSession.logs.length) delete sessions[previousSession.id]
  }
  const s = ensureSession(sessionId)
  const idx = s.logs.findIndex((l) => l.trace_id === trace_id)
  if (idx >= 0) {
    s.logs[idx] = { ...s.logs[idx], ...patch, session_id: sessionId } as LiveLog
  } else {
    s.logs.unshift({
      time: new Date().toISOString(),
      trace_id,
      type: 'HTTP',
      client_ip: '',
      method: patch.method ?? '',
      path: patch.path ?? '',
      status_code: 0,
      duration_ms: 0,
      user_agent: '',
      status: 'running',
      ...previous,
      ...patch,
      session_id: sessionId,
    } as LiveLog)
  }
}

function scheduleSessionRefresh() {
  if (refreshTimer || disposed) return
  refreshTimer = setTimeout(() => {
    refreshTimer = undefined
    void refreshSessions()
  }, 200)
}

async function refreshSessions() {
  if (disposed) return
  if (refreshing) {
    refreshAgain = true
    return
  }
  refreshing = true
  observedDuringRefresh.clear()
  const before = new Set(allLogs.value.map((log) => log.trace_id))
  try {
    const data = await fetchSessions()
    if (disposed) return
    const incoming = new Set<string>()
    for (const session of Object.values(data)) {
      const local = ensureSession(session.id)
      local.label = session.label
      local.created_at = session.created_at
      for (const log of session.logs ?? []) {
        incoming.add(log.trace_id)
        const current = findSessionByTrace(log.trace_id)?.logs.find((item) => item.trace_id === log.trace_id)
        const patch = toLiveLog(log)
        // Stream deltas are more recent than an in-flight HTTP snapshot.
        if (current?.status === 'running' && patch.status === 'running') {
          patch.input_tokens = current.input_tokens
          patch.output_tokens = current.output_tokens
          patch.thinking_tokens = current.thinking_tokens
          patch.token_sources = current.token_sources
          patch.thinking_content = current.thinking_content
          patch.response_body = current.response_body
          patch.tool_use_count = current.tool_use_count
          patch.type = current.type
        }
        upsertLog(log.session_id, log.trace_id, patch)
      }
    }
    for (const [id, session] of Object.entries(sessions)) {
      session.logs = session.logs.filter((log) => incoming.has(log.trace_id) || !before.has(log.trace_id) || observedDuringRefresh.has(log.trace_id))
      session.logs.sort((a, b) => Date.parse(b.time) - Date.parse(a.time))
      if (!session.logs.length) delete sessions[id]
    }
    const selected = activeLogTraceId.value ? findSessionByTrace(activeLogTraceId.value) : null
    if (selected) activeSessionId.value = selected.id
    else if (activeSessionId.value && sessions[activeSessionId.value]) {
      activeLogTraceId.value = sessions[activeSessionId.value]?.logs[0]?.trace_id ?? null
    }
    else if (!activeSessionId.value || !sessions[activeSessionId.value]) {
      const first = sessionList.value[0]
      if (first) selectSession(first.id)
      else {
        activeSessionId.value = null
        activeLogTraceId.value = null
      }
    }
    graphRefreshKey.value++
  } catch (e) {
    logConsole(`[System] ❌ 加载会话失败: ${e}`)
  } finally {
    refreshing = false
    if (refreshAgain) {
      refreshAgain = false
      scheduleSessionRefresh()
    }
  }
}

function logConsole(line: string) {
  consoleLogs.value.push(line)
  if (consoleLogs.value.length > 500) consoleLogs.value.splice(0, consoleLogs.value.length - 500)
}

function handleWsEvent(payload: WSEvent) {
  if (payload.event === 'interceptions_updated') {
    pendingRefreshKey.value++
    return
  }
  if (payload.event === 'replays_updated') {
    replayRefreshKey.value++
    return
  }
  if (payload.event === 'sessions_updated') {
    scheduleSessionRefresh()
    graphRefreshKey.value++
    return
  }
  if (refreshing) observedDuringRefresh.add(payload.trace_id)
  if (payload.event === 'request_updated') {
    upsertLog(payload.log.session_id, payload.trace_id, toLiveLog(payload.log))
    graphRefreshKey.value++
    return
  }
  if (payload.event === 'request_start') {
    const tShort = payload.trace_id.slice(0, 8)
    const timeStr = payload.time?.split('T')[1]?.split('Z')[0] ?? ''
    const replayMark = payload.log?.replay ? ` ↻ 重放自 ${payload.log.replay.of.slice(0, 8)}` : ''
    logConsole(`[${timeStr}] 🔵 ${payload.method} ${payload.path} [${tShort}]${replayMark}`)
    upsertLog(payload.session_id, payload.trace_id, {
      ...payload.log,
      method: payload.method,
      path: payload.path,
      time: payload.time,
      status: payload.log?.status ?? 'running',
    })
    if (!activeSessionId.value) {
      activeSessionId.value = payload.session_id
      activeLogTraceId.value = payload.trace_id
    }
    graphRefreshKey.value++
    return
  }

  if (payload.event === 'sse_delta') {
    const session = findSessionByTrace(payload.trace_id)
    if (!session) return
    if (session.logs.find((log) => log.trace_id === payload.trace_id)?.status !== 'running') return
    const m = payload.metrics
    upsertLog(session.id, payload.trace_id, {
      input_tokens: m.input_tokens,
      output_tokens: m.output_tokens,
      thinking_tokens: m.thinking_tokens,
      token_sources: m.token_sources,
      thinking_content: m.thinking_content,
      response_body: m.output_content,
      type: 'SSE',
      tool_use_count: m.tool_use_count,
      is_thinking_loop: m.is_thinking_loop,
    })
    return
  }

  if (payload.event === 'request_end') {
    const log = payload.log
    const dur = typeof log.duration_ms === 'number' ? log.duration_ms.toFixed(0) : '0'
    logConsole(`[System] ⚪ ${log.method} ${log.path} ${statusLabel(log.status)} (${dur}ms) [${payload.trace_id.slice(0, 8)}]`)
    upsertLog(log.session_id, log.trace_id, {
      ...toLiveLog(log),
    })
    graphRefreshKey.value++
  }
}

function findSessionByTrace(trace_id: string) {
  for (const s of Object.values(sessions)) {
    if (s.logs.some((l) => l.trace_id === trace_id)) return s
  }
  return null
}

function selectSession(id: string) {
  activeSessionId.value = id
  const s = sessions[id]
  activeLogTraceId.value = s?.logs?.[0]?.trace_id ?? null
}

function selectLog(trace_id: string) {
  activeLogTraceId.value = trace_id
  if (activeView.value === 'graph' && window.matchMedia('(max-width: 1279px)').matches) mobileInspectorOpen.value = true
}

function selectGraphNode(node: GraphNode) {
  if (!node.trace_id) return
  const session = findSessionByTrace(node.trace_id)
  if (session) activeSessionId.value = session.id
  else if (node.session_id) {
    activeSessionId.value = node.session_id
    scheduleSessionRefresh()
  }
  selectLog(node.trace_id)
}

function selectPendingRequest(trace: string) {
  const session = findSessionByTrace(trace)
  if (session) {
    activeSessionId.value = session.id
    activeLogTraceId.value = trace
  }
}

// Jump to a trace referenced by another view (replay source or result). The
// session may not be known yet right after a replay starts; the next snapshot
// refresh resolves it from the selected trace.
function selectTrace(trace: string, view?: 'details') {
  const session = findSessionByTrace(trace)
  if (session) activeSessionId.value = session.id
  else scheduleSessionRefresh()
  activeLogTraceId.value = trace
  if (view) activeView.value = view
}

function changeMobileSession(event: Event) {
  selectSession((event.target as HTMLSelectElement).value)
}

function changeMobileLog(event: Event) {
  activeLogTraceId.value = (event.target as HTMLSelectElement).value
}

function toggleUtilities() {
  utilitiesPanel.value?.resize(utilitiesExpanded.value ? 20 : 55)
}

function openHistoricalLog(log: RequestLog) {
  upsertLog(log.session_id, log.trace_id, toLiveLog(log))
  selectTrace(log.trace_id, 'details')
}

function toggleRuleForm() {
  showRuleForm.value = !showRuleForm.value
  if (showRuleForm.value && !utilitiesExpanded.value) utilitiesPanel.value?.resize(55)
}

function toggleInspector() {
  if (window.matchMedia('(max-width: 1279px)').matches) mobileInspectorOpen.value = true
  else showInspector.value = !showInspector.value
}

function totalTokens(log: LiveLog | null): string {
  if (!log) return '未知'
  return totalTokenLabel(log.input_tokens, log.output_tokens, log.token_sources)
}

onMounted(async () => {
  ws = connectWS(handleWsEvent, (status, err) => {
    if (status === 'open') {
      wsStatus.value = 'open'
      logConsole('[System] 🟢 WebSocket connected')
      void refreshSessions()
      pendingRefreshKey.value++
    } else if (status === 'close') {
      wsStatus.value = 'closed'
      logConsole('[System] 🔴 WebSocket disconnected')
    } else if (status === 'error') {
      logConsole(`[System] ❌ WS 错误: ${err}`)
    }
  })
  void refreshSessions()
  snapshotPoll = setInterval(scheduleSessionRefresh, 10000)
  try {
    rules.value = await fetchRules()
  } catch (e) {
    logConsole(`[System] ❌ 加载规则失败: ${e}`)
  }
})

onUnmounted(() => {
  disposed = true
  clearTimeout(refreshTimer)
  clearInterval(snapshotPoll)
  ws?.close()
})

// Rules form state
const showRuleForm = ref(false)
const editingRuleId = ref<string | null>(null)
const ruleError = ref('')
const ruleDraft = reactive<Omit<Rule, 'id'>>({
  priority: 0,
  path_match: '',
  body_match: '',
  inject_system: '',
  intercept: false,
  disabled: false,
  wait_seconds: 30,
  timeout_action: 'forward',
})
const submitting = ref(false)

function resetRuleDraft() {
  ruleDraft.priority = 0
  ruleDraft.path_match = ''
  ruleDraft.body_match = ''
  ruleDraft.inject_system = ''
  ruleDraft.intercept = false
  ruleDraft.disabled = false
  ruleDraft.wait_seconds = 30
  ruleDraft.timeout_action = 'forward'
  editingRuleId.value = null
  ruleError.value = ''
}

function editRule(rule: Rule) {
  const { id, ...fields } = rule
  editingRuleId.value = id
  Object.assign(ruleDraft, fields)
  ruleError.value = ''
  showRuleForm.value = true
  if (!utilitiesExpanded.value) utilitiesPanel.value?.resize(55)
}

async function setRuleEnabled(rule: Rule, enabled: boolean) {
  submitting.value = true
  ruleError.value = ''
  try {
    const { id, ...fields } = rule
    const updated = await updateRule(id, { ...fields, disabled: !enabled })
    rules.value = rules.value.map(item => item.id === id ? updated : item)
  } catch (e) { ruleError.value = String(e) }
  finally { submitting.value = false }
}

async function removeRule(rule: Rule) {
  submitting.value = true
  ruleError.value = ''
  try {
    await deleteRule(rule.id)
    rules.value = rules.value.filter(item => item.id !== rule.id)
    if (editingRuleId.value === rule.id) { resetRuleDraft(); showRuleForm.value = false }
  } catch (e) { ruleError.value = String(e) }
  finally { submitting.value = false }
}

async function submitRule() {
  if (!ruleDraft.path_match.trim()) {
    ruleError.value = 'path_match 不能为空'
    return
  }
  submitting.value = true
  ruleError.value = ''
  try {
    const created = editingRuleId.value ? await updateRule(editingRuleId.value, { ...ruleDraft }) : await createRule({ ...ruleDraft })
    rules.value = editingRuleId.value ? rules.value.map(rule => rule.id === editingRuleId.value ? created : rule) : [...rules.value, created]
    resetRuleDraft()
    showRuleForm.value = false
  } catch (e) {
    ruleError.value = String(e)
    logConsole(`[System] ❌ 新建规则失败: ${e}`)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="h-dvh min-h-0 w-full min-w-0 flex flex-col bg-background text-foreground overflow-hidden">
    <!-- Header -->
    <header class="min-h-14 border-b flex flex-wrap items-center gap-2 px-3 py-2 justify-between bg-card shrink-0">
      <div class="min-w-0 flex items-center gap-2">
        <ActivityIcon class="h-5 w-5 text-primary" />
        <h1 class="whitespace-nowrap text-sm sm:text-lg font-semibold tracking-tight">
          LLM Debugger
          <span class="hidden md:inline text-muted-foreground font-normal text-sm">Gateway</span>
        </h1>
        <Badge
          :variant="wsStatus === 'open' ? 'default' : 'secondary'"
          class="ml-2 h-5 text-[10px]"
        >
          WS: {{ wsStatus }}
        </Badge>
      </div>
      <div class="flex items-center space-x-2">
        <select class="h-8 max-w-24 rounded-md border bg-background px-2 text-[10px] md:hidden" aria-label="选择会话" :value="activeSessionId ?? ''" @change="changeMobileSession">
          <option v-if="!sessionList.length" value="">暂无会话</option>
          <option v-for="session in sessionList" :key="session.id" :value="session.id">{{ session.label || session.id }}</option>
        </select>
        <Button variant="ghost" size="icon" class="hidden sm:inline-flex" aria-label="设置">
          <SettingsIcon class="h-4 w-4" />
        </Button>
      </div>
    </header>

    <!-- Main Layout -->
    <div class="min-h-0 min-w-0 flex-1 overflow-hidden">
      <ResizablePanelGroup direction="horizontal" class="h-full">
        <!-- Sidebar: Sessions + Logs -->
        <ResizablePanel :default-size="20" :min-size="14" :max-size="35" class="hidden md:flex min-h-0 min-w-0 flex-col bg-muted/30">
          <div class="p-3 flex min-h-0 min-w-0 flex-col h-full space-y-3">
            <div class="rounded-lg border border-teal-100 bg-teal-50/60 px-3 py-2.5">
              <div class="flex items-center gap-2 text-xs font-semibold text-teal-900"><GitBranchIcon class="h-3.5 w-3.5" />会话自动关联</div>
              <p class="mt-1 text-[10px] leading-relaxed text-teal-800/70">根据会话标识与历史上下文归档</p>
            </div>

            <!-- Sessions -->
            <div class="flex flex-col min-h-0" style="flex: 1 1 0">
              <h3 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground px-1 mb-1">
                Sessions ({{ sessionList.length }})
              </h3>
              <div class="panel-scroll flex-1 -mx-1 px-1" aria-label="会话列表" tabindex="0">
                <div class="space-y-1 pb-2">
                  <button
                    v-for="s in sessionList"
                    :key="s.id"
                    type="button"
                    @click="selectSession(s.id)"
                    class="w-full text-left p-2 rounded-md border text-sm cursor-pointer transition-colors"
                    :title="s.id"
                    :aria-pressed="activeSessionId === s.id"
                    :class="activeSessionId === s.id
                      ? 'bg-primary/10 border-primary/40'
                      : 'bg-card border-border hover:bg-accent/50'"
                  >
                    <div class="font-medium truncate text-xs">{{ s.label || s.id }}</div>
                    <div class="mt-1 truncate font-mono text-[9px] text-muted-foreground">{{ s.id }}</div>
                    <div class="flex items-center justify-between mt-1">
                      <span class="text-[10px] text-muted-foreground">{{ s.logs.length }} requests</span>
                      <Badge variant="secondary" class="text-[10px] px-1 py-0 h-4">
                        {{ new Date(s.created_at).toLocaleTimeString() }}
                      </Badge>
                    </div>
                  </button>
                  <div v-if="!sessionList.length" class="text-xs text-muted-foreground italic px-1 py-2">
                    No sessions yet.
                  </div>
                </div>
              </div>
            </div>

            <!-- Logs in active session -->
            <div class="flex flex-col min-h-0 border-t pt-3" style="flex: 1 1 0">
              <h3 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground px-1 mb-1">
                <template v-if="activeSession">
                  Logs ({{ activeSession.logs.length }})
                </template>
                <template v-else>Logs</template>
              </h3>
              <div class="panel-scroll flex-1 -mx-1 px-1" aria-label="请求列表" tabindex="0">
                <div class="space-y-1 pb-2">
                  <button
                    v-for="log in activeSession?.logs ?? []"
                    :key="log.trace_id"
                    type="button"
                    @click="selectLog(log.trace_id)"
                    class="w-full text-left p-2 rounded-md border text-xs cursor-pointer transition-colors"
                    :aria-pressed="activeLogTraceId === log.trace_id"
                    :class="activeLogTraceId === log.trace_id
                      ? 'bg-primary/10 border-primary/40'
                      : 'bg-card border-border hover:bg-accent/50'"
                  >
                    <div class="flex items-center justify-between">
                      <span class="min-w-0 truncate pr-2 font-mono font-semibold">{{ log.model || log.method }}</span>
                      <Badge
                        :variant="log.status === 'running' ? 'secondary' : log.status === 'error' ? 'destructive' : 'default'"
                        class="h-4 text-[10px] px-1"
                      >
                        {{ log.status === 'running' ? '●' : log.status === 'pending' || log.status === 'canceled' ? statusLabel(log.status) : log.status_code || statusLabel(log.status) }}
                      </Badge>
                    </div>
                    <div class="truncate text-muted-foreground mt-0.5 font-mono">{{ log.path }}</div>
                    <div v-if="log.summary" class="mt-1 truncate text-[10px]">{{ log.summary }}</div>
                    <div class="flex items-center justify-between mt-1 text-[10px] text-muted-foreground">
                      <span>{{ totalTokens(log) }} tk</span>
                      <span>{{ log.duration_ms ? `${log.duration_ms.toFixed(0)}ms` : '...' }}</span>
                    </div>
                  </button>
                  <div v-if="activeSession && !activeSession.logs.length" class="text-xs text-muted-foreground italic px-1 py-2">
                    No logs in this session.
                  </div>
                  <div v-if="!activeSession" class="text-xs text-muted-foreground italic px-1 py-2">
                    Select a session.
                  </div>
                </div>
              </div>
            </div>
          </div>
        </ResizablePanel>

        <ResizableHandle with-handle class="hidden md:flex" />

        <!-- Main Content -->
        <ResizablePanel :default-size="80" class="flex min-h-0 min-w-0 flex-col h-full bg-background">
          <ResizablePanelGroup direction="vertical">
            <!-- Top: Thinking + Tools -->
            <ResizablePanel :default-size="80" :min-size="25" class="flex min-h-0 min-w-0 flex-col">
              <div class="flex min-h-11 shrink-0 flex-wrap items-center gap-1 border-b bg-card px-2 py-1.5">
                <Button :variant="activeView === 'graph' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'graph'" @click="activeView = 'graph'">
                  <GitBranchIcon class="h-3.5 w-3.5" />调用画布
                </Button>
                <Button :variant="activeView === 'details' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'details'" @click="activeView = 'details'">
                  <FileTextIcon class="h-3.5 w-3.5" />思考与报文
                </Button>
                <Button :variant="activeView === 'intercept' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'intercept'" @click="activeView = 'intercept'">
                  <PauseCircleIcon class="h-3.5 w-3.5 text-amber-600" />待处理 <Badge v-if="pendingCount" variant="secondary" class="h-4 px-1 text-[10px]">{{ pendingCount }}</Badge>
                </Button>
                <Button :variant="activeView === 'audit' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'audit'" @click="activeView = 'audit'">
                  <DiffIcon class="h-3.5 w-3.5" />原始 / 出站
                </Button>
                <Button :variant="activeView === 'manage' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'manage'" @click="activeView = 'manage'"><SettingsIcon class="h-3.5 w-3.5" />管理</Button>
                <select v-if="activeView === 'graph'" v-model="graphScope" aria-label="调用图范围" class="ml-auto h-7 rounded-md border bg-background px-2 text-[11px]">
                  <option value="session">当前会话</option>
                  <option value="all">全部会话</option>
                </select>
                <Button v-if="activeView === 'graph'" variant="ghost" size="icon" class="h-7 w-7 shrink-0" aria-label="切换调用详情" title="显示或收起调用详情" @click="toggleInspector"><PanelRightIcon class="h-3.5 w-3.5" /></Button>
              </div>
              <div v-if="narrowLayout && activeView !== 'intercept' && activeView !== 'manage'" class="flex shrink-0 items-center gap-2 border-b bg-card px-3 py-2">
                <label for="active-request" class="shrink-0 text-[11px] text-muted-foreground">当前请求</label>
                <select id="active-request" class="h-7 min-w-0 flex-1 rounded-md border bg-background px-2 text-xs" :value="activeLogTraceId ?? ''" @change="changeMobileLog">
                  <option v-if="!activeSession?.logs.length" value="">暂无请求</option>
                  <option v-for="log in activeSession?.logs ?? []" :key="log.trace_id" :value="log.trace_id">{{ log.model || log.method }} · {{ log.summary || log.path }}</option>
                </select>
              </div>
              <div v-if="activeView === 'graph'" class="flex min-h-0 flex-1 overflow-hidden">
                <CallGraph
                  class="min-w-0 flex-1"
                  :session-id="graphScope === 'all' ? '' : activeSession?.id ?? null"
                  :refresh-key="graphRefreshKey"
                  :active-trace-id="activeLogTraceId"
                  :logs="allLogs"
                  @select="selectGraphNode"
                />
                <RequestInspector v-if="showInspector" class="hidden w-80 shrink-0 border-l xl:flex" :log="activeLog" @select="selectTrace" />
              </div>
              <ResizablePanelGroup v-else-if="activeView === 'details'" :direction="stackPayloads ? 'vertical' : 'horizontal'" class="min-h-0 min-w-0 flex-1">
                <!-- Thinking Stream -->
                <ResizablePanel :default-size="50" :min-size="15" class="flex min-h-0 min-w-0 flex-col border-r relative">
                  <div class="min-h-10 border-b flex flex-wrap items-center gap-2 px-3 py-2 bg-muted/20 shrink-0 justify-between">
                    <div class="flex shrink-0 items-center">
                      <BrainCircuitIcon class="h-4 w-4 mr-2 text-muted-foreground" />
                      <h2 class="text-sm font-medium">Thinking Stream</h2>
                    </div>
                    <div v-if="activeLog" class="flex flex-wrap items-center gap-1 text-[10px] text-muted-foreground">
                      <Badge variant="secondary" class="h-4 text-[10px] px-1">in {{ formatTokens(activeLog.input_tokens, activeLog.token_sources?.input) }}</Badge>
                      <Badge variant="secondary" class="h-4 text-[10px] px-1">out {{ formatTokens(activeLog.output_tokens, activeLog.token_sources?.output) }}</Badge>
                      <Badge variant="secondary" class="h-4 text-[10px] px-1">think {{ formatTokens(activeLog.thinking_tokens, activeLog.token_sources?.thinking) }}</Badge>
                      <Badge v-if="activeLog.is_thinking_loop" variant="destructive" class="h-4 text-[10px] px-1">loop!</Badge>
                    </div>
                  </div>
                  <div class="panel-scroll wrap-content flex-1 p-4" aria-label="思考内容" tabindex="0">
                    <div
                      v-if="activeLog?.thinking_content"
                      class="text-sm border-l-2 pl-3 py-1 border-primary/50 bg-primary/5 italic whitespace-pre-wrap break-words"
                    >
                      {{ activeLog.thinking_content }}
                    </div>
                    <div v-else-if="activeLog && activeLog.status === 'running'" class="text-sm text-muted-foreground italic pl-3 flex items-center space-x-2">
                      <div class="animate-pulse flex space-x-1 items-center h-4 text-primary">
                        <div class="w-1.5 h-1.5 bg-current rounded-full"></div>
                        <div class="w-1.5 h-1.5 bg-current rounded-full"></div>
                        <div class="w-1.5 h-1.5 bg-current rounded-full"></div>
                      </div>
                      <span>Waiting for thinking stream...</span>
                    </div>
                    <div v-else-if="activeLog" class="text-sm text-muted-foreground italic pl-3">
                      No thinking content for this request.
                    </div>
                    <div v-else class="text-sm text-muted-foreground italic pl-3">
                      Select a log to view its thinking stream.
                    </div>
                  </div>
                </ResizablePanel>

                <ResizableHandle with-handle />

                <!-- Tools & Actions / Details -->
                <ResizablePanel :default-size="50" :min-size="15" class="flex min-h-0 min-w-0 flex-col">
                  <div class="min-h-10 border-b flex flex-wrap items-center px-3 py-2 bg-muted/20 shrink-0">
                    <Code2Icon class="h-4 w-4 mr-2 text-muted-foreground" />
                    <h2 class="text-sm font-medium">Details</h2>
                    <Badge v-if="activeLog" variant="secondary" class="ml-2 h-4 text-[10px] px-1">
                      tools: {{ activeLog.tool_use_count ?? 0 }}
                    </Badge>
                  </div>
                  <div class="panel-scroll wrap-content flex-1 p-4 bg-zinc-950 text-zinc-50" aria-label="请求与响应报文" tabindex="0">
                    <div v-if="!activeLog" class="text-sm text-zinc-500 italic">
                      No log selected.
                    </div>
                    <div v-else class="space-y-3 font-mono text-xs">
                      <div class="rounded-md border border-zinc-800 bg-zinc-900/50 p-3">
                        <div class="text-zinc-500 mb-1">Trace</div>
                        <div class="text-emerald-400 break-all">{{ activeLog.trace_id }}</div>
                      </div>
                      <div class="rounded-md border border-zinc-800 bg-zinc-900/50 p-3">
                        <div class="text-zinc-500 mb-1">Request</div>
                        <pre class="text-blue-300 break-words whitespace-pre-wrap">{{ formatBody(activeLog.request_body) || '(empty)' }}</pre>
                      </div>
                      <div class="flex flex-wrap gap-3 text-[11px] text-zinc-300"><span>首字节 {{ timing(activeLog.ttfb_ms) }}</span><span>首内容 {{ timing(activeLog.ttfc_ms) }}</span></div>
                      <ResponsePanel :trace-id="activeLog.trace_id" :status="activeLog.status" />
                      <RoutingDetails :log="activeLog" />
                      <div v-if="activeLog.error" class="rounded-md border border-red-800 bg-red-950/40 p-3">
                        <div class="text-red-400 mb-1">Error</div>
                        <pre class="text-red-300 break-words whitespace-pre-wrap">{{ activeLog.error }}</pre>
                      </div>
                    </div>
                  </div>
                </ResizablePanel>
              </ResizablePanelGroup>
              <InterceptionPanel v-show="activeView === 'intercept'" :active="activeView === 'intercept'" :refresh-key="pendingRefreshKey" :connected="wsStatus === 'open'" @select="selectPendingRequest" />
              <RequestAudit v-if="activeView === 'audit'" :trace-id="activeLogTraceId" :logs="allLogs" :refresh-key="graphRefreshKey" :replay-refresh-key="replayRefreshKey" @select="trace => selectTrace(trace, 'details')" />
              <GatewaySettings v-if="activeView === 'manage'" @select="openHistoricalLog" />
            </ResizablePanel>

            <ResizableHandle with-handle />

            <!-- Bottom: Tabs -->
            <ResizablePanel ref="utilitiesPanel" :default-size="20" :min-size="10" class="flex min-h-0 min-w-0 flex-col bg-card/50" @resize="(size) => utilitiesExpanded = size > 35">
              <Tabs default-value="terminal" class="w-full min-h-0 min-w-0 flex flex-col h-full">
                <div class="border-b px-2 flex items-center gap-1 bg-muted/30 shrink-0">
                  <TabsList class="h-9 min-w-0 bg-transparent p-0 space-x-1">
                    <TabsTrigger
                      value="terminal"
                      class="data-[state=active]:bg-background data-[state=active]:shadow-sm rounded-t-md rounded-b-none border-b-0 h-9 px-4 text-xs"
                    >
                      <TerminalIcon class="h-3.5 w-3.5 mr-1.5" />
                      Console
                    </TabsTrigger>
                    <TabsTrigger
                      value="rules"
                      class="data-[state=active]:bg-background data-[state=active]:shadow-sm rounded-t-md rounded-b-none border-b-0 h-9 px-4 text-xs"
                    >
                      Dynamic Rules
                      <Badge variant="secondary" class="ml-1.5 h-4 text-[10px] px-1">{{ rules.length }}</Badge>
                    </TabsTrigger>
                  </TabsList>
                  <Button variant="ghost" size="icon" class="ml-auto h-7 w-7 shrink-0" :aria-label="utilitiesExpanded ? '收起底部面板' : '展开底部面板'" @click="toggleUtilities">
                    <component :is="utilitiesExpanded ? Minimize2Icon : Maximize2Icon" class="h-3.5 w-3.5" />
                  </Button>
                </div>
                <div class="min-h-0 min-w-0 flex-1 overflow-hidden">
                  <TabsContent value="terminal" class="h-full m-0 border-0 p-0 outline-none">
                    <div class="panel-scroll wrap-content h-full bg-black text-green-400 font-mono text-xs p-3" aria-label="控制台日志" tabindex="0">
                      <div
                        v-for="(log, i) in consoleLogs"
                        :key="i"
                        class="break-words mt-1"
                        :class="log.includes('❌') ? 'text-red-400' : log.includes('[System]') ? 'text-zinc-500' : 'text-green-400'"
                      >
                        {{ log }}
                      </div>
                      <div v-if="!consoleLogs.length" class="text-zinc-600 italic">Waiting for connection...</div>
                    </div>
                  </TabsContent>
                  <TabsContent value="rules" class="panel-scroll h-full m-0 border-0 p-0 outline-none" aria-label="动态规则">
                    <div class="sticky top-0 z-10 flex flex-wrap items-center justify-between gap-2 px-3 py-2 border-b bg-card">
                      <span class="text-[11px] text-muted-foreground">
                        优先级高的先执行，同级按创建顺序；首条拦截规则决定等待策略
                      </span>
                      <Button size="sm" variant="outline" @click="toggleRuleForm">
                        <component :is="showRuleForm ? ChevronUpIcon : PlusIcon" class="h-3.5 w-3.5 mr-1" />
                        {{ showRuleForm ? '收起' : '新建规则' }}
                      </Button>
                    </div>
                    <p v-if="ruleError" role="alert" class="mx-3 my-2 rounded border border-red-200 bg-red-50 p-2 text-xs text-red-900">{{ ruleError }}</p>
                    <div v-if="showRuleForm" class="p-3 border-b bg-muted/10 grid grid-cols-1 sm:grid-cols-2 gap-3">
                      <p v-if="editingRuleId" class="text-xs font-semibold sm:col-span-2">编辑规则 · 修改仅影响后续请求</p>
                      <div class="space-y-1">
                        <Label for="rule-path" class="text-xs">path_match</Label>
                        <Input id="rule-path" v-model="ruleDraft.path_match" placeholder="/messages" />
                      </div>
                      <div class="space-y-1">
                        <Label for="rule-body" class="text-xs">body_match</Label>
                        <Input id="rule-body" v-model="ruleDraft.body_match" placeholder="(可选) 子串匹配" />
                      </div>
                      <div class="space-y-1 sm:col-span-2">
                        <Label for="rule-system" class="text-xs">inject_system</Label>
                        <Textarea id="rule-system" v-model="ruleDraft.inject_system" rows="2" placeholder="自动按协议追加到 system、系统消息或 instructions" />
                      </div>
                      <div class="space-y-1"><Label for="rule-priority" class="text-xs">优先级（越大越先执行）</Label><Input id="rule-priority" v-model.number="ruleDraft.priority" type="number" min="-100000" max="100000" step="1" /></div>
                      <div class="flex flex-wrap items-center gap-2 sm:col-span-2">
                        <Switch v-model="ruleDraft.intercept" id="intercept" />
                        <Label for="intercept" class="min-w-0 flex-1 text-xs">拦截请求，等待编辑或放行</Label>
                      </div>
                      <div v-if="ruleDraft.intercept" class="space-y-1"><Label for="rule-wait" class="text-xs">等待时间（秒）</Label><Input id="rule-wait" v-model.number="ruleDraft.wait_seconds" type="number" min="1" max="3600" step="1" /></div>
                      <div v-if="ruleDraft.intercept" class="space-y-1"><Label for="rule-timeout" class="text-xs">等待结束后</Label><select id="rule-timeout" v-model="ruleDraft.timeout_action" class="h-9 w-full rounded-md border bg-background px-3 text-sm"><option value="forward">转发已保存内容</option><option value="cancel">取消请求</option></select></div>
                      <div class="flex flex-wrap items-center justify-end gap-2 sm:col-span-2">
                        <Button size="sm" variant="ghost" @click="resetRuleDraft">重置</Button>
                        <Button size="sm" :disabled="submitting" @click="submitRule">
                          {{ submitting ? '提交中...' : editingRuleId ? '保存规则' : '提交' }}
                        </Button>
                      </div>
                    </div>
                    <div class="min-w-0">
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead class="h-8 text-xs">path_match</TableHead>
                            <TableHead class="h-8 text-xs">优先级</TableHead>
                            <TableHead class="h-8 text-xs">body_match</TableHead>
                            <TableHead class="h-8 text-xs">inject_system</TableHead>
                            <TableHead class="h-8 text-xs">拦截</TableHead>
                            <TableHead class="h-8 text-xs">启用</TableHead>
                            <TableHead class="h-8 text-xs">操作</TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          <TableRow v-for="r in [...rules].sort((a, b) => b.priority - a.priority)" :key="r.id">
                            <TableCell class="text-xs font-mono py-2">{{ r.path_match || '—' }}</TableCell>
                            <TableCell class="text-xs font-mono py-2">{{ r.priority }}</TableCell>
                            <TableCell class="text-xs font-mono py-2 max-w-40 truncate" :title="r.body_match">{{ r.body_match || '—' }}</TableCell>
                            <TableCell class="text-xs py-2 max-w-60 truncate" :title="r.inject_system">{{ r.inject_system || '—' }}</TableCell>
                            <TableCell class="py-2">
                              <Badge :variant="r.intercept ? 'default' : 'secondary'" class="text-[10px] h-4 px-1">
                                {{ r.intercept ? `${r.wait_seconds}s · ${r.timeout_action === 'cancel' ? '到期取消' : '到期转发'}` : '关闭' }}
                              </Badge>
                            </TableCell>
                            <TableCell class="py-2"><Switch :model-value="!r.disabled" :disabled="submitting" :aria-label="`启用规则 ${r.path_match}`" @update:model-value="value => setRuleEnabled(r, value)" /></TableCell>
                            <TableCell class="py-2">
                              <div class="flex gap-1">
                              <Button variant="ghost" size="icon" class="h-6 w-6" :disabled="submitting" :aria-label="`编辑规则 ${r.path_match}`" @click="editRule(r)"><PencilIcon class="h-3.5 w-3.5" /></Button>
                              <Button
                                variant="ghost"
                                size="icon"
                                class="h-6 w-6"
                                :disabled="submitting"
                                :aria-label="`删除规则 ${r.path_match}`"
                                @click="removeRule(r)"
                              >
                                <Trash2Icon class="h-3.5 w-3.5" />
                              </Button>
                              </div>
                            </TableCell>
                          </TableRow>
                          <TableRow v-if="!rules.length">
                            <TableCell colspan="7" class="text-xs text-muted-foreground italic text-center py-4">
                              No active dynamic rules configured.
                            </TableCell>
                          </TableRow>
                        </TableBody>
                      </Table>
                    </div>
                  </TabsContent>
                </div>
              </Tabs>
            </ResizablePanel>
          </ResizablePanelGroup>
        </ResizablePanel>
      </ResizablePanelGroup>
    </div>
    <Sheet v-model:open="mobileInspectorOpen">
      <SheetContent side="right" class="flex w-full flex-col gap-0 p-0 sm:max-w-md">
        <SheetHeader class="shrink-0 border-b p-4 text-left">
          <SheetTitle class="text-sm">查看调用</SheetTitle>
          <SheetDescription class="text-xs">关联依据与模型返回的内容</SheetDescription>
        </SheetHeader>
        <RequestInspector class="min-h-0 flex-1" :log="activeLog" @select="selectTrace" />
      </SheetContent>
    </Sheet>
  </div>
</template>
