<script setup lang="ts">
import { ref, reactive, computed, defineAsyncComponent, watch, onMounted, onUnmounted, nextTick } from 'vue'
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
import RequestFinder from '@/components/RequestFinder.vue'
import RequestActions from '@/components/RequestActions.vue'
import ResponseComparison from '@/components/ResponseComparison.vue'
import ToolDetails from '@/components/ToolDetails.vue'
import { formatTokens, totalTokenLabel, timing } from '@/lib/metrics'
import { findRequests, type RequestOrder } from '@/lib/requestFinder'
import type { AuditMode, InvestigationAction } from '@/lib/investigation'
import { initialReplayOperation, type ReplayOperationState } from '@/lib/replayOperation'
import {
  PlusIcon,
  TerminalIcon,
  ActivityIcon,
  SettingsIcon,
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
import { APIError, fetchRules, fetchSessions, fetchRequestLog, createRule, updateRule, deleteRule, connectWS } from '@/lib/api'
import { formatBody, formatDuration, statusLabel } from '@/lib/correlation'
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
const graphFocusKey = ref(0)
const pendingRefreshKey = ref(0)
const replayRefreshKey = ref(0)
const mobileInspectorOpen = ref(false)
const showInspector = ref(true)
const narrowLayout = useMediaQuery('(max-width: 1023px)')
const requestQuery = ref('')
const requestStatus = ref('')
const requestOrder = ref<RequestOrder>('newest')
const mobileFinderOpen = ref(false)
const sessionLoading = ref(false)
const sessionsLoaded = ref(false)
const sessionError = ref('')
const selectionLoading = ref(false)
const selectionError = ref('')
const auditSourceTraceId = ref<string | null>(typeof saved.auditTrace === 'string' ? saved.auditTrace : activeLogTraceId.value)
const auditMode = ref<AuditMode>(saved.auditMode === 'context' || saved.auditMode === 'curl' || saved.auditMode === 'replay' ? saved.auditMode : 'compare')
const replayOperation = ref<ReplayOperationState>(initialReplayOperation())
const requestAudit = ref<InstanceType<typeof RequestAudit> | null>(null)
const gatewaySettings = ref<InstanceType<typeof GatewaySettings> | null>(null)
const historyReturnAvailable = ref(false)
const comparisonElement = ref<HTMLElement | null>(null)
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
let selectionController: AbortController | undefined
const observedDuringRefresh = new Set<string>()

watch([activeSessionId, activeLogTraceId, activeView, graphScope, auditSourceTraceId, auditMode], () => {
  try {
    localStorage.setItem('llm-debugger.selection', JSON.stringify({
      session: activeSessionId.value, trace: activeLogTraceId.value, view: activeView.value, scope: graphScope.value,
      auditTrace: auditSourceTraceId.value, auditMode: auditMode.value,
    }))
  } catch {
    // Selection still works when browser storage is disabled.
  }
})
watch(activeView, view => { if (view !== 'graph') graphFocusKey.value = 0 })

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
const filteredLogs = computed(() => findRequests(activeSession.value?.logs ?? [], requestQuery.value, requestStatus.value, requestOrder.value))
const selectedOutsideFilter = computed(() => Boolean(activeLog.value && !filteredLogs.value.some(log => log.trace_id === activeLogTraceId.value)))
const comparisonSourceLog = computed(() => {
  const trace = activeLog.value?.replay?.of
  return trace ? findSessionByTrace(trace)?.logs.find(log => log.trace_id === trace) ?? null : null
})
const operationNeedsAttention = computed(() => replayOperation.value.phase === 'submitting' || replayOperation.value.phase === 'unknown' || replayOperation.value.record?.state === 'running')
const showOperationBanner = computed(() => operationNeedsAttention.value && !(activeView.value === 'audit' && auditMode.value === 'replay'))

function clearRequestFilter() { requestQuery.value = ''; requestStatus.value = ''; requestOrder.value = 'newest' }

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
  sessionLoading.value = true
  observedDuringRefresh.clear()
  const before = new Set(allLogs.value.map((log) => log.trace_id))
  try {
    const data = await fetchSessions()
    if (disposed) return
    sessionsLoaded.value = true
    sessionError.value = ''
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
    else if (activeLogTraceId.value) {
      // A replay/source link can be newer than this in-flight snapshot.
      // Resolve it explicitly; never replace it with an unrelated latest call.
      if (!selectionLoading.value && !selectionError.value) void resolveSelectedTrace(activeLogTraceId.value)
    }
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
    sessionError.value = `加载会话失败：${String(e)}`
    logConsole(`[System] ❌ 加载会话失败: ${e}`)
  } finally {
    refreshing = false
    sessionLoading.value = false
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
  selectionController?.abort()
  selectionLoading.value = false
  selectionError.value = ''
  activeSessionId.value = id
  const s = sessions[id]
  activeLogTraceId.value = s?.logs?.[0]?.trace_id ?? null
  if (activeView.value === 'audit') auditSourceTraceId.value = activeLogTraceId.value
}

function selectLog(trace_id: string, focusFromList = true) {
  void selectTrace(trace_id)
  if (activeView.value === 'audit') auditSourceTraceId.value = trace_id
  if (activeView.value === 'graph' && focusFromList) graphFocusKey.value++
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
  selectLog(node.trace_id, false)
}

function selectPendingRequest(trace: string) {
  void selectTrace(trace)
}

async function resolveSelectedTrace(trace: string) {
  selectionController?.abort()
  const request = selectionController = new AbortController()
  selectionLoading.value = true
  selectionError.value = ''
  try {
    const log = await fetchRequestLog(trace, request.signal)
    if (request.signal.aborted || activeLogTraceId.value !== trace) return
    upsertLog(log.session_id, log.trace_id, toLiveLog(log))
    // A newer live revision may have already moved this trace while the
    // single-log read was in flight. Follow the accepted local record.
    activeSessionId.value = findSessionByTrace(trace)?.id ?? log.session_id
  } catch (e) {
    if (!request.signal.aborted && activeLogTraceId.value === trace) selectionError.value = e instanceof APIError && e.status === 404 ? `请求 ${trace.slice(0, 8)} 已删除或尚未保存。可以重试，或选择其他请求。` : `无法读取选中的请求：${String(e)}`
  } finally {
    if (request === selectionController) { selectionLoading.value = false; selectionController = undefined }
  }
}

async function selectTrace(trace: string, view?: 'details') {
  selectionController?.abort()
  selectionLoading.value = false
  selectionError.value = ''
  const session = findSessionByTrace(trace)
  if (session) activeSessionId.value = session.id
  activeLogTraceId.value = trace
  if (view) activeView.value = view
  if (!session) await resolveSelectedTrace(trace)
}

async function investigate(intent: InvestigationAction) {
  mobileInspectorOpen.value = false
  if (intent.action === 'context' || intent.action === 'replay') {
    auditSourceTraceId.value = intent.trace
    auditMode.value = intent.action
    activeView.value = 'audit'
    await selectTrace(intent.trace)
    return
  }
  const log = findSessionByTrace(intent.trace)?.logs.find(item => item.trace_id === intent.trace)
  await selectTrace(intent.action === 'source' && log?.replay ? log.replay.of : intent.trace, 'details')
  if (intent.action === 'compare') {
    await nextTick()
    comparisonElement.value?.scrollIntoView({ block: 'start', behavior: 'smooth' })
    comparisonElement.value?.focus({ preventScroll: true })
  }
}

function openAuditForSelection() {
  auditSourceTraceId.value = activeLogTraceId.value
  activeView.value = 'audit'
  if (auditSourceTraceId.value) void selectTrace(auditSourceTraceId.value)
}

function returnToWorkbench(trace = auditSourceTraceId.value) {
  if (!trace) return
  auditSourceTraceId.value = trace
  auditMode.value = 'replay'
  activeView.value = 'audit'
  mobileInspectorOpen.value = false
  void selectTrace(trace)
}

function returnToHistory() {
  gatewaySettings.value?.openHistory()
  activeView.value = 'manage'
  mobileInspectorOpen.value = false
}

function openProviderSettings() {
  gatewaySettings.value?.openProviders()
  activeView.value = 'manage'
}

function changeMobileSession(event: Event) {
  selectSession((event.target as HTMLSelectElement).value)
}

function changeMobileLog(event: Event) {
  mobileFinderOpen.value = false
  selectLog((event.target as HTMLSelectElement).value)
}

function toggleUtilities() {
  utilitiesPanel.value?.resize(utilitiesExpanded.value ? 20 : 55)
}

function openHistoricalLog(log: RequestLog) {
  historyReturnAvailable.value = true
  upsertLog(log.session_id, log.trace_id, toLiveLog(log))
  void selectTrace(log.trace_id, 'details')
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
  selectionController?.abort()
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
        <Button variant="ghost" size="icon" class="h-8 w-8" aria-label="设置" @click="activeView = 'manage'">
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
                  <div v-if="!sessionList.length" class="text-xs text-muted-foreground px-1 py-2">
                    {{ sessionLoading ? '正在加载会话…' : sessionError ? '会话加载失败，请重试。' : '还没有捕获的会话。' }}
                  </div>
                </div>
              </div>
            </div>

            <!-- Logs in active session -->
            <div class="flex flex-col min-h-0 border-t pt-3" style="flex: 1 1 0">
              <h3 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground px-1 mb-1">
                <template v-if="activeSession">
                  请求 ({{ filteredLogs.length }} / {{ activeSession.logs.length }})
                </template>
                <template v-else>Logs</template>
              </h3>
              <RequestFinder v-if="!narrowLayout" v-model:query="requestQuery" v-model:status="requestStatus" v-model:order="requestOrder" :matched="filteredLogs.length" :total="activeSession?.logs.length ?? 0" class="mb-2" />
              <p v-if="selectedOutsideFilter && !narrowLayout" class="mb-2 text-[10px] leading-relaxed text-amber-800">当前请求不在筛选结果中，已保留选择。<button type="button" class="underline" @click="clearRequestFilter">清除筛选</button></p>
              <div class="panel-scroll flex-1 -mx-1 px-1" aria-label="请求列表" tabindex="0">
                <div class="space-y-1 pb-2">
                  <button
                    v-for="log in filteredLogs"
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
                      <span>{{ log.status === 'running' || log.status === 'pending' ? statusLabel(log.status) : formatDuration(log.duration_ms) }}</span>
                    </div>
                  </button>
                  <div v-if="activeSession && !filteredLogs.length" class="text-xs text-muted-foreground px-1 py-2">
                    {{ activeSession.logs.length ? '没有匹配的请求。' : '此会话还没有请求。' }}
                    <Button v-if="activeSession.logs.length" variant="link" size="sm" class="h-auto px-0 py-1 text-xs" @click="clearRequestFilter">清除筛选</Button>
                  </div>
                  <div v-if="!activeSession" class="text-xs text-muted-foreground italic px-1 py-2">
                    选择一个会话查看请求。
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
                  <FileTextIcon class="h-3.5 w-3.5" />响应与详情
                </Button>
                <Button :variant="activeView === 'intercept' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'intercept'" @click="activeView = 'intercept'">
                  <PauseCircleIcon class="h-3.5 w-3.5 text-amber-600" />待处理 <Badge v-if="pendingCount" variant="secondary" class="h-4 px-1 text-[10px]">{{ pendingCount }}</Badge>
                </Button>
                <Button :variant="activeView === 'audit' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'audit'" @click="openAuditForSelection">
                  <DiffIcon class="h-3.5 w-3.5" />原始 / 出站
                </Button>
                <Button :variant="activeView === 'manage' ? 'secondary' : 'ghost'" size="sm" class="h-8 gap-1.5 text-xs" :aria-pressed="activeView === 'manage'" @click="activeView = 'manage'"><SettingsIcon class="h-3.5 w-3.5" />管理</Button>
                <select v-if="activeView === 'graph'" v-model="graphScope" aria-label="调用图范围" class="ml-auto h-7 rounded-md border bg-background px-2 text-[11px]">
                  <option value="session">当前会话</option>
                  <option value="all">全部会话</option>
                </select>
                <Button v-if="activeView === 'graph'" variant="ghost" size="icon" class="h-7 w-7 shrink-0" aria-label="切换调用详情" title="显示或收起调用详情" @click="toggleInspector"><PanelRightIcon class="h-3.5 w-3.5" /></Button>
              </div>
              <div v-if="sessionError && sessionList.length" role="alert" class="flex shrink-0 flex-wrap items-center gap-2 border-b bg-amber-50 px-3 py-2 text-xs text-amber-900"><span class="min-w-0 flex-1 break-words">{{ sessionError }}，当前保留上次加载的数据。</span><Button size="sm" variant="outline" class="h-7 text-xs" :disabled="sessionLoading" @click="refreshSessions">重试加载会话</Button></div>
              <div v-if="selectionError" role="alert" class="flex shrink-0 flex-wrap items-center gap-2 border-b bg-amber-50 px-3 py-2 text-xs text-amber-900"><span class="min-w-0 flex-1 break-words">{{ selectionError }}</span><Button size="sm" variant="outline" class="h-7 text-xs" :disabled="selectionLoading" @click="activeLogTraceId && resolveSelectedTrace(activeLogTraceId)">重试读取请求</Button></div>
              <div v-if="(auditSourceTraceId && activeView !== 'audit') || (historyReturnAvailable && activeView !== 'manage') || showOperationBanner" class="flex shrink-0 flex-wrap items-center gap-1.5 border-b bg-violet-50/40 px-3 py-1.5 text-[11px]" aria-label="调查导航">
                <Button v-if="auditSourceTraceId && activeView !== 'audit'" size="sm" variant="ghost" class="h-7 text-[11px]" @click="returnToWorkbench()">返回重放工作台 · {{ auditSourceTraceId.slice(0, 8) }}</Button>
                <Button v-if="historyReturnAvailable && activeView !== 'manage'" size="sm" variant="ghost" class="h-7 text-[11px]" @click="returnToHistory">返回历史查询</Button>
                <template v-if="showOperationBanner"><span role="status" class="break-words text-violet-900">{{ replayOperation.phase === 'unknown' ? '有一笔重放尚未确认发送结果' : replayOperation.phase === 'submitting' ? '正在提交重放' : '有一笔重放进行中' }}</span><Button size="sm" variant="outline" class="h-7 text-[11px]" @click="returnToWorkbench(replayOperation.sourceTrace)">查看本次重放</Button><Button v-if="replayOperation.record?.state === 'running'" size="sm" variant="destructive" class="h-7 text-[11px]" :disabled="replayOperation.busy" @click="requestAudit?.cancelOperation()">取消重放</Button></template>
              </div>
              <details v-if="narrowLayout && activeView !== 'intercept' && activeView !== 'manage'" class="shrink-0 border-b bg-card px-3" :open="mobileFinderOpen" @toggle="mobileFinderOpen = ($event.target as HTMLDetailsElement).open">
                <summary class="cursor-pointer py-2 text-[11px] font-medium">筛选请求 · {{ filteredLogs.length }} / {{ activeSession?.logs.length ?? 0 }}<span v-if="requestStatus || requestQuery" class="ml-2 text-teal-800">已筛选</span></summary>
                <RequestFinder v-model:query="requestQuery" v-model:status="requestStatus" v-model:order="requestOrder" :matched="filteredLogs.length" :total="activeSession?.logs.length ?? 0" class="pb-2" />
                <p v-if="selectedOutsideFilter" class="pb-2 text-[10px] text-amber-800">当前请求不在筛选结果中，已保留选择。</p>
              </details>
              <div v-if="narrowLayout && activeView !== 'intercept' && activeView !== 'manage'" class="flex shrink-0 items-center gap-2 border-b bg-card px-3 py-2">
                <label for="active-request" class="shrink-0 text-[11px] text-muted-foreground">当前请求</label>
                <select id="active-request" class="h-7 min-w-0 flex-1 rounded-md border bg-background px-2 text-xs" :value="activeLogTraceId ?? ''" @change="changeMobileLog">
                  <option v-if="!filteredLogs.length" value="" disabled>{{ activeSession?.logs.length ? '没有匹配的请求' : '暂无请求' }}</option>
                  <option v-if="activeLogTraceId && !filteredLogs.some(log => log.trace_id === activeLogTraceId)" :value="activeLogTraceId">{{ activeLog ? `${activeLog.model || activeLog.method}（筛选外）` : `请求 ${activeLogTraceId.slice(0, 8)}` }}</option>
                  <option v-for="log in filteredLogs" :key="log.trace_id" :value="log.trace_id">{{ statusLabel(log.status) }} · {{ log.model || log.method }} · {{ log.summary || log.path }}</option>
                </select>
              </div>
              <div v-if="(activeView === 'graph' || activeView === 'details') && !sessionList.length" class="panel-scroll flex min-h-0 flex-1 flex-col items-center justify-center gap-3 p-5 text-center" aria-label="捕获状态">
                <p v-if="sessionLoading && !sessionsLoaded" role="status" class="text-sm text-muted-foreground">正在加载捕获的调用…</p>
                <template v-else-if="sessionError"><p role="alert" class="max-w-xl break-words text-sm text-destructive">{{ sessionError }}</p><Button size="sm" variant="outline" :disabled="sessionLoading" @click="refreshSessions">重试加载会话</Button></template>
                <template v-else><h2 class="text-base font-semibold">开始捕获模型调用</h2><p class="max-w-md text-sm leading-relaxed text-muted-foreground">将 SDK 的 base_url 指向网关监听地址，发送一次模型请求。调用会自动出现在这里，随后可以查看上下文并重放。</p><div class="flex flex-wrap justify-center gap-2"><Button size="sm" variant="outline" @click="openProviderSettings">检查 Provider 与路由</Button><Button size="sm" variant="outline" :disabled="sessionLoading" @click="refreshSessions">刷新会话</Button></div></template>
              </div>
              <div v-else-if="activeView === 'graph'" class="flex min-h-0 flex-1 overflow-hidden">
                <CallGraph
                  class="min-w-0 flex-1"
                  :session-id="graphScope === 'all' ? '' : activeSession?.id ?? null"
                  :refresh-key="graphRefreshKey"
                  :focus-key="graphFocusKey"
                  :active-trace-id="activeLogTraceId"
                  :logs="allLogs"
                  @select="selectGraphNode"
                />
                <RequestInspector v-if="showInspector" class="hidden w-80 shrink-0 border-l xl:flex" :log="activeLog" @select="selectTrace" @action="investigate" />
              </div>
              <section v-else-if="activeView === 'details'" class="panel-scroll min-h-0 min-w-0 flex-1 space-y-4 p-3 sm:p-4" aria-label="响应与调用详情" tabindex="0">
                <p v-if="!activeLog" role="status" class="p-5 text-sm text-muted-foreground">{{ selectionLoading ? '正在读取选中的请求…' : selectionError ? '此请求暂不可用，可重试或选择其他请求。' : '选择一个请求查看响应与调用详情。' }}</p>
                <template v-else>
                  <div class="space-y-3 rounded-lg border bg-card p-3 sm:p-4">
                    <div class="flex flex-wrap items-center gap-2"><h2 class="min-w-0 break-words text-sm font-semibold">{{ activeLog.model || activeLog.path }}</h2><Badge :variant="activeLog.status === 'error' ? 'destructive' : 'secondary'" class="text-[10px]">{{ statusLabel(activeLog.status) }} {{ activeLog.status_code || '' }}</Badge><span class="break-all font-mono text-[10px] text-muted-foreground">{{ activeLog.trace_id }}</span></div>
                    <p class="break-words text-xs leading-relaxed text-muted-foreground">{{ activeLog.summary || `${activeLog.method} ${activeLog.path}` }}</p>
                    <RequestActions :log="activeLog" @action="investigate" />
                    <div class="flex flex-wrap gap-x-4 gap-y-2 text-[11px] text-muted-foreground"><span>输入 {{ formatTokens(activeLog.input_tokens, activeLog.token_sources?.input) }}</span><span>输出 {{ formatTokens(activeLog.output_tokens, activeLog.token_sources?.output) }}</span><span>总耗时 {{ activeLog.status === 'running' || activeLog.status === 'pending' ? statusLabel(activeLog.status) : formatDuration(activeLog.duration_ms) }}</span><span>等待 {{ activeLog.status === 'pending' ? '等待中' : timing(activeLog.wait_duration_ms) }}</span><span>上游 {{ activeLog.status === 'running' ? '进行中' : timing(activeLog.upstream_duration_ms) }}</span><span>首内容 {{ timing(activeLog.ttfc_ms) }}</span></div>
                    <p v-if="activeLog.error" role="alert" class="break-words rounded border border-red-200 bg-red-50 p-3 text-xs text-red-900">{{ activeLog.error }}</p>
                  </div>
                  <div v-if="activeLog.replay" ref="comparisonElement" class="min-w-0 outline-none" tabindex="-1" aria-label="来源与重放结果">
                    <ResponseComparison :source-trace="activeLog.replay.of" :replay-trace="activeLog.trace_id" :status="activeLog.status" :source-log="comparisonSourceLog" :replay-log="activeLog" @select="trace => selectTrace(trace, 'details')" />
                  </div>
                  <ResponsePanel v-else :trace-id="activeLog.trace_id" :status="activeLog.status" />
                  <details v-if="activeLog.thinking_content" class="rounded-lg border bg-card">
                    <summary class="cursor-pointer p-3 text-xs font-semibold"><BrainCircuitIcon class="mr-1 inline h-3.5 w-3.5 text-violet-600" />已返回的思考内容 <span class="font-normal text-muted-foreground">{{ formatTokens(activeLog.thinking_tokens, activeLog.token_sources?.thinking) }}</span><Badge v-if="activeLog.is_thinking_loop" variant="destructive" class="ml-2 text-[10px]">检测到重复思考</Badge></summary>
                    <pre class="panel-scroll max-h-96 whitespace-pre-wrap break-words border-t p-3 text-xs leading-relaxed [overflow-wrap:anywhere]" tabindex="0" aria-label="思考内容">{{ activeLog.thinking_content }}</pre>
                  </details>
                  <ToolDetails :tools="activeLog.tools ?? []" @select="trace => selectTrace(trace, 'details')" />
                  <RoutingDetails :log="activeLog" />
                  <details class="rounded-lg border bg-card">
                    <summary class="cursor-pointer p-3 text-xs font-semibold">请求报文 · 日志预览</summary>
                    <div class="space-y-2 border-t p-3"><p class="text-[11px] text-muted-foreground">这是日志中的有界预览。完整正文、原始 / 出站差异和 cURL 在请求操作中查看。</p><Button size="sm" variant="outline" class="h-7 text-xs" @click="auditMode = 'compare'; openAuditForSelection()">查看完整请求</Button><pre class="panel-scroll max-h-96 whitespace-pre-wrap break-words font-mono text-[11px] leading-relaxed [overflow-wrap:anywhere]" aria-label="请求正文预览" tabindex="0">{{ formatBody(activeLog.request_body) || '（无请求正文）' }}</pre></div>
                  </details>
                </template>
              </section>
              <InterceptionPanel v-show="activeView === 'intercept'" :active="activeView === 'intercept'" :refresh-key="pendingRefreshKey" :connected="wsStatus === 'open'" @select="selectPendingRequest" />
              <RequestAudit ref="requestAudit" v-show="activeView === 'audit'" v-model:mode="auditMode" :trace-id="auditSourceTraceId" :logs="allLogs" :refresh-key="graphRefreshKey" :replay-refresh-key="replayRefreshKey" :active="activeView === 'audit'" @select="trace => selectTrace(trace, 'details')" @source="returnToWorkbench" @operation="state => replayOperation = state" />
              <GatewaySettings ref="gatewaySettings" v-show="activeView === 'manage'" :active="activeView === 'manage'" :selected-trace="activeLogTraceId" @select="openHistoricalLog" />
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
        <RequestInspector class="min-h-0 flex-1" :log="activeLog" @select="selectTrace" @action="investigate" />
      </SheetContent>
    </Sheet>
  </div>
</template>
