<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import {
  ResizablePanelGroup,
  ResizablePanel,
  ResizableHandle,
} from '@/components/ui/resizable'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
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
import {
  PlusIcon,
  TerminalIcon,
  ActivityIcon,
  SettingsIcon,
  Code2Icon,
  BrainCircuitIcon,
  Trash2Icon,
  ChevronDownIcon,
  ChevronUpIcon,
} from 'lucide-vue-next'
import { fetchRules, fetchSessions, createRule, connectWS } from '@/lib/api'
import type {
  LiveLog,
  RequestLog,
  Rule,
  Session,
  WSEvent,
} from '@/lib/types'

const sessions = reactive<Record<string, Session & { logs: LiveLog[] }>>({})
const activeSessionId = ref<string | null>(null)
const activeLogTraceId = ref<string | null>(null)
const rules = ref<Rule[]>([])
const consoleLogs = ref<string[]>([])
const wsStatus = ref<'connecting' | 'open' | 'closed'>('connecting')

let ws: WebSocket | null = null

const sessionList = computed(() =>
  Object.values(sessions).sort((a, b) => {
    const at = lastLogTime(a)
    const bt = lastLogTime(b)
    return bt - at
  }),
)

const activeSession = computed<(Session & { logs: LiveLog[] }) | null>(() =>
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
  return new Date(last.time).getTime() || 0
}

function toLiveLog(log: RequestLog, status: LiveLog['status'] = 'done'): LiveLog {
  return { ...log, status }
}

function ensureSession(id: string): Session & { logs: LiveLog[] } {
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
  const s = ensureSession(sessionId)
  const idx = s.logs.findIndex((l) => l.trace_id === trace_id)
  if (idx >= 0) {
    s.logs[idx] = { ...s.logs[idx], ...patch }
  } else {
    s.logs.unshift({
      time: new Date().toISOString(),
      trace_id,
      session_id: sessionId,
      type: 'HTTP',
      client_ip: '',
      method: patch.method ?? '',
      path: patch.path ?? '',
      status_code: 0,
      duration_ms: 0,
      user_agent: '',
      status: 'running',
      ...patch,
    } as LiveLog)
  }
}

function logConsole(line: string) {
  consoleLogs.value.push(line)
  if (consoleLogs.value.length > 500) consoleLogs.value.splice(0, consoleLogs.value.length - 500)
}

function handleWsEvent(payload: WSEvent) {
  if (payload.event === 'request_start') {
    const tShort = payload.trace_id.slice(0, 8)
    const timeStr = payload.time?.split('T')[1]?.split('Z')[0] ?? ''
    logConsole(`[${timeStr}] 🔵 ${payload.method} ${payload.path} [${tShort}]`)
    upsertLog(payload.session_id, payload.trace_id, {
      method: payload.method,
      path: payload.path,
      time: payload.time,
      status: 'running',
    })
    activeSessionId.value = payload.session_id
    activeLogTraceId.value = payload.trace_id
    return
  }

  if (payload.event === 'sse_delta') {
    const session = findSessionByTrace(payload.trace_id)
    if (!session) return
    const m = payload.metrics
    upsertLog(session.id, payload.trace_id, {
      input_tokens: m.input_tokens,
      output_tokens: m.output_tokens,
      thinking_tokens: m.thinking_tokens,
      thinking_content: m.thinking_content,
      tool_use_count: m.tool_use_count,
      is_thinking_loop: m.is_thinking_loop,
    })
    return
  }

  if (payload.event === 'request_end') {
    const log = payload.log
    const dur = typeof log.duration_ms === 'number' ? log.duration_ms.toFixed(0) : '0'
    logConsole(`[System] ⚪ ${log.method} ${log.path} done (${dur}ms) [${payload.trace_id.slice(0, 8)}]`)
    upsertLog(log.session_id, log.trace_id, {
      ...toLiveLog(log, log.error ? 'error' : 'done'),
    })
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
}

function totalTokens(log: LiveLog | null): number {
  if (!log) return 0
  return (log.input_tokens ?? 0) + (log.output_tokens ?? 0)
}

onMounted(async () => {
  try {
    const data = await fetchSessions()
    for (const [id, s] of Object.entries(data)) {
      sessions[id] = {
        ...s,
        logs: (s.logs ?? []).map((l) => toLiveLog(l)).reverse(),
      }
    }
    const first = sessionList.value[0]
    if (first) selectSession(first.id)
  } catch (e) {
    logConsole(`[System] ❌ 加载会话失败: ${e}`)
  }

  try {
    rules.value = await fetchRules()
  } catch (e) {
    logConsole(`[System] ❌ 加载规则失败: ${e}`)
  }

  ws = connectWS(handleWsEvent, (status, err) => {
    if (status === 'open') {
      wsStatus.value = 'open'
      logConsole('[System] 🟢 WebSocket connected')
    } else if (status === 'close') {
      wsStatus.value = 'closed'
      logConsole('[System] 🔴 WebSocket disconnected')
    } else if (status === 'error') {
      logConsole(`[System] ❌ WS 错误: ${err}`)
    }
  })
})

onUnmounted(() => {
  ws?.close()
})

// Rules form state
const showRuleForm = ref(false)
const ruleDraft = reactive<Omit<Rule, 'id'>>({
  path_match: '',
  body_match: '',
  inject_system: '',
  intercept: false,
})
const submitting = ref(false)

function resetRuleDraft() {
  ruleDraft.path_match = ''
  ruleDraft.body_match = ''
  ruleDraft.inject_system = ''
  ruleDraft.intercept = false
}

async function submitRule() {
  if (!ruleDraft.path_match.trim()) {
    logConsole('[System] ❌ path_match 不能为空')
    return
  }
  submitting.value = true
  try {
    const created = await createRule({ ...ruleDraft })
    rules.value = [...rules.value, created]
    resetRuleDraft()
    showRuleForm.value = false
  } catch (e) {
    logConsole(`[System] ❌ 新建规则失败: ${e}`)
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <div class="h-screen w-full flex flex-col bg-background text-foreground overflow-hidden">
    <!-- Header -->
    <header class="h-14 border-b flex items-center px-4 justify-between bg-card shrink-0">
      <div class="flex items-center space-x-2">
        <ActivityIcon class="h-5 w-5 text-primary" />
        <h1 class="text-lg font-semibold tracking-tight">
          LLM Debugger
          <span class="text-muted-foreground font-normal text-sm">Gateway</span>
        </h1>
        <Badge
          :variant="wsStatus === 'open' ? 'default' : 'secondary'"
          class="ml-2 h-5 text-[10px]"
        >
          WS: {{ wsStatus }}
        </Badge>
      </div>
      <div class="flex items-center space-x-2">
        <Button variant="ghost" size="icon">
          <SettingsIcon class="h-4 w-4" />
        </Button>
      </div>
    </header>

    <!-- Main Layout -->
    <div class="flex-1 overflow-hidden">
      <ResizablePanelGroup direction="horizontal" class="h-full">
        <!-- Sidebar: Sessions + Logs -->
        <ResizablePanel :default-size="25" :min-size="18" :max-size="40" class="flex flex-col bg-muted/30">
          <div class="p-3 flex flex-col h-full space-y-3">
            <Button
              class="w-full justify-start shadow-sm"
              variant="default"
              disabled
              title="会话由上游客户端的 X-Session-ID 请求头决定；前端不手工创建。"
            >
              <PlusIcon class="mr-2 h-4 w-4" />
              New Session
            </Button>

            <!-- Sessions -->
            <div class="flex flex-col min-h-0" style="flex: 1 1 0">
              <h3 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground px-1 mb-1">
                Sessions ({{ sessionList.length }})
              </h3>
              <ScrollArea class="flex-1 -mx-1 px-1">
                <div class="space-y-1 pb-2">
                  <div
                    v-for="s in sessionList"
                    :key="s.id"
                    @click="selectSession(s.id)"
                    class="p-2 rounded-md border text-sm cursor-pointer transition-colors"
                    :class="activeSessionId === s.id
                      ? 'bg-primary/10 border-primary/40'
                      : 'bg-card border-border hover:bg-accent/50'"
                  >
                    <div class="font-medium truncate font-mono text-xs">{{ s.id }}</div>
                    <div class="flex items-center justify-between mt-1">
                      <span class="text-[10px] text-muted-foreground">{{ s.logs.length }} requests</span>
                      <Badge variant="secondary" class="text-[10px] px-1 py-0 h-4">
                        {{ new Date(s.created_at).toLocaleTimeString() }}
                      </Badge>
                    </div>
                  </div>
                  <div v-if="!sessionList.length" class="text-xs text-muted-foreground italic px-1 py-2">
                    No sessions yet.
                  </div>
                </div>
              </ScrollArea>
            </div>

            <!-- Logs in active session -->
            <div class="flex flex-col min-h-0 border-t pt-3" style="flex: 1 1 0">
              <h3 class="text-xs font-semibold uppercase tracking-wider text-muted-foreground px-1 mb-1">
                <template v-if="activeSession">
                  Logs ({{ activeSession.logs.length }})
                </template>
                <template v-else>Logs</template>
              </h3>
              <ScrollArea class="flex-1 -mx-1 px-1">
                <div class="space-y-1 pb-2">
                  <div
                    v-for="log in activeSession?.logs ?? []"
                    :key="log.trace_id"
                    @click="selectLog(log.trace_id)"
                    class="p-2 rounded-md border text-xs cursor-pointer transition-colors"
                    :class="activeLogTraceId === log.trace_id
                      ? 'bg-primary/10 border-primary/40'
                      : 'bg-card border-border hover:bg-accent/50'"
                  >
                    <div class="flex items-center justify-between">
                      <span class="font-mono font-semibold">{{ log.method }}</span>
                      <Badge
                        :variant="log.status === 'running' ? 'secondary' : log.status === 'error' ? 'destructive' : 'default'"
                        class="h-4 text-[10px] px-1"
                      >
                        {{ log.status === 'running' ? '●' : log.status_code || log.status }}
                      </Badge>
                    </div>
                    <div class="truncate text-muted-foreground mt-0.5 font-mono">{{ log.path }}</div>
                    <div class="flex items-center justify-between mt-1 text-[10px] text-muted-foreground">
                      <span>{{ totalTokens(log) }} tk</span>
                      <span>{{ log.duration_ms ? `${log.duration_ms.toFixed(0)}ms` : '...' }}</span>
                    </div>
                  </div>
                  <div v-if="activeSession && !activeSession.logs.length" class="text-xs text-muted-foreground italic px-1 py-2">
                    No logs in this session.
                  </div>
                  <div v-if="!activeSession" class="text-xs text-muted-foreground italic px-1 py-2">
                    Select a session.
                  </div>
                </div>
              </ScrollArea>
            </div>
          </div>
        </ResizablePanel>

        <ResizableHandle with-handle />

        <!-- Main Content -->
        <ResizablePanel :default-size="75" class="flex flex-col h-full bg-background">
          <ResizablePanelGroup direction="vertical">
            <!-- Top: Thinking + Tools -->
            <ResizablePanel :default-size="75" class="flex">
              <ResizablePanelGroup direction="horizontal">
                <!-- Thinking Stream -->
                <ResizablePanel :default-size="50" class="flex flex-col border-r h-full relative">
                  <div class="h-10 border-b flex items-center px-4 bg-muted/20 shrink-0 justify-between">
                    <div class="flex items-center">
                      <BrainCircuitIcon class="h-4 w-4 mr-2 text-muted-foreground" />
                      <h2 class="text-sm font-medium">Thinking Stream</h2>
                    </div>
                    <div v-if="activeLog" class="flex items-center gap-1 text-[10px] text-muted-foreground">
                      <Badge variant="secondary" class="h-4 text-[10px] px-1">in {{ activeLog.input_tokens ?? 0 }}</Badge>
                      <Badge variant="secondary" class="h-4 text-[10px] px-1">out {{ activeLog.output_tokens ?? 0 }}</Badge>
                      <Badge variant="secondary" class="h-4 text-[10px] px-1">think {{ activeLog.thinking_tokens ?? 0 }}</Badge>
                      <Badge v-if="activeLog.is_thinking_loop" variant="destructive" class="h-4 text-[10px] px-1">loop!</Badge>
                    </div>
                  </div>
                  <ScrollArea class="flex-1 p-4">
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
                  </ScrollArea>
                </ResizablePanel>

                <ResizableHandle with-handle />

                <!-- Tools & Actions / Details -->
                <ResizablePanel :default-size="50" class="flex flex-col h-full">
                  <div class="h-10 border-b flex items-center px-4 bg-muted/20 shrink-0">
                    <Code2Icon class="h-4 w-4 mr-2 text-muted-foreground" />
                    <h2 class="text-sm font-medium">Details</h2>
                    <Badge v-if="activeLog" variant="secondary" class="ml-2 h-4 text-[10px] px-1">
                      tools: {{ activeLog.tool_use_count ?? 0 }}
                    </Badge>
                  </div>
                  <ScrollArea class="flex-1 p-4 bg-zinc-950 text-zinc-50">
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
                        <pre class="text-blue-300 break-words whitespace-pre-wrap">{{ activeLog.request_body || '(empty)' }}</pre>
                      </div>
                      <div class="rounded-md border border-zinc-800 bg-zinc-900/50 p-3">
                        <div class="text-zinc-500 mb-1">Response</div>
                        <pre class="text-zinc-300 break-words whitespace-pre-wrap">{{ activeLog.response_body || (activeLog.status === 'running' ? '(streaming...)' : '(empty)') }}</pre>
                      </div>
                      <div v-if="activeLog.error" class="rounded-md border border-red-800 bg-red-950/40 p-3">
                        <div class="text-red-400 mb-1">Error</div>
                        <pre class="text-red-300 break-words whitespace-pre-wrap">{{ activeLog.error }}</pre>
                      </div>
                    </div>
                  </ScrollArea>
                </ResizablePanel>
              </ResizablePanelGroup>
            </ResizablePanel>

            <ResizableHandle with-handle />

            <!-- Bottom: Tabs -->
            <ResizablePanel :default-size="25" class="flex flex-col bg-card/50">
              <Tabs default-value="terminal" class="w-full flex flex-col h-full">
                <div class="border-b px-2 flex items-center bg-muted/30 shrink-0">
                  <TabsList class="h-9 bg-transparent p-0 space-x-1">
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
                </div>
                <div class="flex-1 overflow-hidden">
                  <TabsContent value="terminal" class="h-full m-0 border-0 p-0 outline-none">
                    <ScrollArea class="h-full bg-black text-green-400 font-mono text-xs p-3">
                      <div
                        v-for="(log, i) in consoleLogs"
                        :key="i"
                        class="break-words mt-1"
                        :class="log.includes('❌') ? 'text-red-400' : log.includes('[System]') ? 'text-zinc-500' : 'text-green-400'"
                      >
                        {{ log }}
                      </div>
                      <div v-if="!consoleLogs.length" class="text-zinc-600 italic">Waiting for connection...</div>
                    </ScrollArea>
                  </TabsContent>
                  <TabsContent value="rules" class="h-full m-0 border-0 p-0 outline-none flex flex-col">
                    <div class="flex items-center justify-between px-3 py-2 border-b bg-muted/20 shrink-0">
                      <span class="text-xs text-muted-foreground">
                        GET + POST 可用；删除/修改待后端支持
                      </span>
                      <Button size="sm" variant="outline" @click="showRuleForm = !showRuleForm">
                        <component :is="showRuleForm ? ChevronUpIcon : PlusIcon" class="h-3.5 w-3.5 mr-1" />
                        {{ showRuleForm ? '收起' : '新建规则' }}
                      </Button>
                    </div>
                    <div v-if="showRuleForm" class="p-3 border-b bg-muted/10 grid grid-cols-2 gap-3 shrink-0">
                      <div class="space-y-1">
                        <Label class="text-xs">path_match</Label>
                        <Input v-model="ruleDraft.path_match" placeholder="/messages" />
                      </div>
                      <div class="space-y-1">
                        <Label class="text-xs">body_match</Label>
                        <Input v-model="ruleDraft.body_match" placeholder="(可选) 子串匹配" />
                      </div>
                      <div class="space-y-1 col-span-2">
                        <Label class="text-xs">inject_system</Label>
                        <Textarea v-model="ruleDraft.inject_system" rows="2" placeholder="追加到请求 system 字段的文本" />
                      </div>
                      <div class="flex items-center space-x-2 col-span-2">
                        <Switch v-model="ruleDraft.intercept" id="intercept" />
                        <Label for="intercept" class="text-xs">intercept（仅标记；后端目前未实现拦截）</Label>
                        <div class="flex-1" />
                        <Button size="sm" variant="ghost" @click="resetRuleDraft">重置</Button>
                        <Button size="sm" :disabled="submitting" @click="submitRule">
                          {{ submitting ? '提交中...' : '提交' }}
                        </Button>
                      </div>
                    </div>
                    <ScrollArea class="flex-1">
                      <Table>
                        <TableHeader>
                          <TableRow>
                            <TableHead class="h-8 text-xs">path_match</TableHead>
                            <TableHead class="h-8 text-xs">body_match</TableHead>
                            <TableHead class="h-8 text-xs">inject_system</TableHead>
                            <TableHead class="h-8 text-xs w-20">intercept</TableHead>
                            <TableHead class="h-8 text-xs w-10"></TableHead>
                          </TableRow>
                        </TableHeader>
                        <TableBody>
                          <TableRow v-for="r in rules" :key="r.id">
                            <TableCell class="text-xs font-mono py-2">{{ r.path_match || '—' }}</TableCell>
                            <TableCell class="text-xs font-mono py-2 max-w-40 truncate" :title="r.body_match">{{ r.body_match || '—' }}</TableCell>
                            <TableCell class="text-xs py-2 max-w-60 truncate" :title="r.inject_system">{{ r.inject_system || '—' }}</TableCell>
                            <TableCell class="py-2">
                              <Badge :variant="r.intercept ? 'default' : 'secondary'" class="text-[10px] h-4 px-1">
                                {{ r.intercept ? 'on' : 'off' }}
                              </Badge>
                            </TableCell>
                            <TableCell class="py-2">
                              <Button
                                variant="ghost"
                                size="icon"
                                class="h-6 w-6"
                                disabled
                                title="后端暂无 DELETE /api/rules/:id"
                              >
                                <Trash2Icon class="h-3.5 w-3.5" />
                              </Button>
                            </TableCell>
                          </TableRow>
                          <TableRow v-if="!rules.length">
                            <TableCell colspan="5" class="text-xs text-muted-foreground italic text-center py-4">
                              No active dynamic rules configured.
                            </TableCell>
                          </TableRow>
                        </TableBody>
                      </Table>
                    </ScrollArea>
                  </TabsContent>
                </div>
              </Tabs>
            </ResizablePanel>
          </ResizablePanelGroup>
        </ResizablePanel>
      </ResizablePanelGroup>
    </div>
  </div>
</template>
