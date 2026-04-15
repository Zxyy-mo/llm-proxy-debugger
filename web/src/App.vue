<script setup lang="ts">
import { ref, onMounted, onUnmounted, nextTick } from 'vue'
import {
  ResizablePanelGroup,
  ResizablePanel,
  ResizableHandle,
} from '@/components/ui/resizable'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import { PlusIcon, TerminalIcon, ActivityIcon, SettingsIcon, Code2Icon, BrainCircuitIcon } from 'lucide-vue-next'

// Types
interface Session {
  id: string
  trace_id: string
  title: string
  tokens: number
  active: boolean
  time: string
  method: string
  path: string
}

const sessions = ref<Session[]>([])
const activeSessionId = ref<string | null>(null)

// For the active session
const thinkingContent = ref('')
const isThinking = ref(false)
const isRequestActive = ref(false)
const toolCalls = ref<any[]>([])
const consoleLogs = ref<string[]>([])

let ws: WebSocket | null = null

const scrollThinking = () => {
  // Try to scroll the thinking area down if possible
  // Normally would use a template ref, skipping for simplicity.
}

onMounted(() => {
  ws = new WebSocket('ws://localhost:12337/api/ws')
  
  ws.onopen = () => {
    consoleLogs.value.push('[System] 🟢 WebSocket connected to gateway: ws://localhost:12337/api/ws')
  }

  ws.onmessage = (event) => {
    try {
      const payload = JSON.parse(event.data)
      
      if (payload.event === 'request_start') {
        const timeStr = payload.time ? payload.time.split('T')[1].split('Z')[0] : ''
        consoleLogs.value.push(`[${timeStr}] 🔵 Request Started: ${payload.method} ${payload.path} [Trace: ${payload.trace_id.substring(0,8)}]`)
        
        sessions.value.forEach(s => s.active = false)
        sessions.value.unshift({
          id: payload.session_id,
          trace_id: payload.trace_id,
          title: `${payload.method} ${payload.path}`,
          tokens: 0,
          active: true,
          time: payload.time,
          method: payload.method,
          path: payload.path
        })
        activeSessionId.value = payload.session_id
        
        thinkingContent.value = ''
        isThinking.value = true
        isRequestActive.value = true
        toolCalls.value = []
      }
      
      else if (payload.event === 'sse_delta') {
        const s = sessions.value.find(s => s.trace_id === payload.trace_id)
        if (s && payload.metrics) {
          s.tokens = (payload.metrics.input_tokens || 0) + (payload.metrics.output_tokens || 0)
          
          if (payload.metrics.thinking_content) {
             thinkingContent.value = payload.metrics.thinking_content
          }

          if (payload.metrics.tool_use_count > toolCalls.value.length) {
            toolCalls.value.push({
               id: toolCalls.value.length + 1,
               name: 'Unknown Tool',
               params: 'Waiting for tool payload...',
               status: 'pending'
            })
          }
          isThinking.value = true
        }

        // If it's a tool_use block start from Anthropic
        if (payload.data && payload.data.includes('"type":"tool_use"')) {
           try {
             const dataJson = JSON.parse(payload.data)
             if (dataJson.type === 'tool_use' || dataJson.content_block?.type === 'tool_use') {
                const name = dataJson.name || dataJson.content_block?.name || 'Tool'
                // Update the last pend tool
                if (toolCalls.value.length > 0) {
                   toolCalls.value[toolCalls.value.length - 1].name = name
                   toolCalls.value[toolCalls.value.length - 1].params = JSON.stringify(dataJson.input || {})
                }
             }
           } catch(e) {}
        }
      }
      
      else if (payload.event === 'request_end') {
        consoleLogs.value.push(`[System] ⚪ Request Ended: [Trace: ${payload.trace_id.substring(0,8)}] Duration: ${payload.log?.duration_ms?.toFixed(1) || 0}ms`)
        isThinking.value = false
        isRequestActive.value = false
        if (toolCalls.value.length > 0) {
           toolCalls.value.forEach(t => {
             if (t.status === 'pending') t.status = 'success'
           })
        }
      }
    } catch(e) {
      consoleLogs.value.push(`[System] ❌ Msg Parse Error: ${e}`)
    }
  }

  ws.onclose = () => {
    consoleLogs.value.push('[System] 🔴 WebSocket disconnected. Auto-retry depends on browser.')
    isThinking.value = false
    isRequestActive.value = false
  }
})

onUnmounted(() => {
  if (ws) ws.close()
})
</script>

<template>
  <div class="h-screen w-full flex flex-col bg-background text-foreground overflow-hidden">
    <!-- Header -->
    <header class="h-14 border-b flex items-center px-4 justify-between bg-card shrink-0">
      <div class="flex items-center space-x-2">
        <ActivityIcon class="h-5 w-5 text-primary" />
        <h1 class="text-lg font-semibold tracking-tight">LLM Debugger <span class="text-muted-foreground font-normal text-sm">Gateway</span></h1>
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
        <!-- Sidebar -->
        <ResizablePanel :default-size="20" :min-size="15" :max-size="30" class="flex flex-col bg-muted/30">
          <div class="p-4 flex flex-col h-full space-y-4">
            <Button class="w-full justify-start shadow-sm" variant="default">
              <PlusIcon class="mr-2 h-4 w-4" />
              New Session
            </Button>
            
            <div class="flex-1 flex flex-col space-y-2 overflow-hidden">
              <h3 class="text-sm font-medium text-muted-foreground px-1">Recent Sessions</h3>
              <ScrollArea class="flex-1 -mx-2 px-2">
                <div class="space-y-1 pb-4">
                  <div v-for="s in sessions" :key="s.id" 
                       class="p-3 rounded-lg border text-sm cursor-pointer transition-colors"
                       :class="s.active ? 'bg-primary/10 border-primary/30 text-primary-foreground' : 'bg-card border-border hover:bg-accent/50 text-foreground'">
                    <div class="font-medium truncate">{{ s.title }}</div>
                    <div class="flex items-center justify-between mt-2">
                      <span class="text-xs text-muted-foreground font-mono">{{ s.id }}</span>
                      <Badge variant="secondary" class="text-[10px] px-1 py-0 h-4">{{ s.tokens }} tk</Badge>
                    </div>
                  </div>
                </div>
              </ScrollArea>
            </div>
          </div>
        </ResizablePanel>

        <ResizableHandle with-handle />

        <!-- Main Content Area -->
        <ResizablePanel :default-size="80" class="flex flex-col h-full bg-background">
          <ResizablePanelGroup direction="vertical">
            <!-- Top Split: Thinking Stream & Tool Actions -->
            <ResizablePanel :default-size="75" class="flex">
              <ResizablePanelGroup direction="horizontal">
                
                <!-- Thinking Stream -->
                <ResizablePanel :default-size="50" class="flex flex-col border-r h-full relative">
                  <div class="h-10 border-b flex items-center px-4 bg-muted/20 shrink-0">
                    <BrainCircuitIcon class="h-4 w-4 mr-2 text-muted-foreground" />
                    <h2 class="text-sm font-medium">Thinking Stream</h2>
                  </div>
                  <ScrollArea class="flex-1 p-4">
                    <div class="space-y-4">
                      
                      <!-- Thinking Content Block -->
                      <div v-if="thinkingContent" class="text-sm border-l-2 pl-3 py-1 border-primary/50 bg-primary/5 text-primary-foreground italic whitespace-pre-wrap break-words">
                        {{ thinkingContent }}
                      </div>
                      
                      <!-- Empty State -->
                      <div v-else-if="!isThinking && !isRequestActive" class="text-sm text-muted-foreground italic pl-3 border-l-2 border-transparent">
                        Waiting for thinking stream...
                      </div>

                      <div v-if="isThinking" class="animate-pulse flex space-x-1 items-center h-4 text-primary pl-3 mt-4">
                        <div class="w-1.5 h-1.5 bg-current rounded-full"></div>
                        <div class="w-1.5 h-1.5 bg-current rounded-full"></div>
                        <div class="w-1.5 h-1.5 bg-current rounded-full"></div>
                      </div>
                    </div>
                  </ScrollArea>
                </ResizablePanel>

                <ResizableHandle with-handle />

                <!-- Tool & Action -->
                <ResizablePanel :default-size="50" class="flex flex-col h-full">
                  <div class="h-10 border-b flex items-center px-4 bg-muted/20 shrink-0">
                    <Code2Icon class="h-4 w-4 mr-2 text-muted-foreground" />
                    <h2 class="text-sm font-medium">Tools & Actions</h2>
                  </div>
                  <ScrollArea class="flex-1 p-4 bg-zinc-950 text-zinc-50 dark:bg-zinc-950/50">
                    <div class="space-y-4">
                      <div v-for="call in toolCalls" :key="call.id" class="rounded-md border border-zinc-800 bg-zinc-900/50 overflow-hidden font-mono text-xs">
                        <div class="flex items-center justify-between px-3 py-2 border-b border-zinc-800 bg-zinc-900">
                          <div class="flex items-center space-x-2">
                            <span class="text-zinc-400">Call:</span>
                            <span class="text-emerald-400 font-semibold">{{ call.name }}</span>
                          </div>
                          <Badge :variant="call.status === 'success' ? 'default' : 'secondary'" 
                                 class="h-5 text-[10px]"
                                 :class="call.status === 'success' ? 'bg-emerald-500/20 text-emerald-400 hover:bg-emerald-500/30' : 'bg-zinc-800 text-zinc-400'">
                            {{ call.status }}
                          </Badge>
                        </div>
                        <div class="p-3 space-y-2">
                          <div>
                            <div class="text-zinc-500 mb-1">Parameters:</div>
                            <pre class="text-blue-300 break-words whitespace-pre-wrap">{{ call.params }}</pre>
                          </div>
                          <div v-if="call.result">
                            <div class="text-zinc-500 mb-1 mt-2">Result:</div>
                            <pre class="text-zinc-300 break-words whitespace-pre-wrap">{{ call.result }}</pre>
                          </div>
                        </div>
                      </div>
                    </div>
                  </ScrollArea>
                </ResizablePanel>

              </ResizablePanelGroup>
            </ResizablePanel>

            <ResizableHandle with-handle />

            <!-- Bottom: Terminal / Control -->
            <ResizablePanel :default-size="25" class="flex flex-col bg-card/50">
              <Tabs default-value="terminal" class="w-full flex flex-col h-full">
                <div class="border-b px-2 flex items-center bg-muted/30 shrink-0">
                  <TabsList class="h-9 bg-transparent p-0 space-x-1">
                    <TabsTrigger value="terminal" class="data-[state=active]:bg-background data-[state=active]:shadow-sm rounded-t-md rounded-b-none border-b-0 h-9 px-4 text-xs">
                      <TerminalIcon class="h-3.5 w-3.5 mr-1.5" />
                      Console
                    </TabsTrigger>
                    <TabsTrigger value="rules" class="data-[state=active]:bg-background data-[state=active]:shadow-sm rounded-t-md rounded-b-none border-b-0 h-9 px-4 text-xs">
                      Dynamic Rules
                    </TabsTrigger>
                  </TabsList>
                </div>
                <div class="flex-1 overflow-hidden">
                  <TabsContent value="terminal" class="h-full m-0 border-0 p-0 outline-none">
                    <ScrollArea class="h-full bg-black text-green-400 font-mono text-xs p-3">
                      <div v-for="(log, i) in consoleLogs" :key="i" class="break-words mt-1" :class="log.includes('[System] ❌') ? 'text-red-400' : (log.includes('[System]') ? 'text-zinc-500' : 'text-green-400')">
                        {{ log }}
                      </div>
                      <div v-if="consoleLogs.length === 0" class="text-zinc-600 italic">Waiting for connection...</div>
                    </ScrollArea>
                  </TabsContent>
                  <TabsContent value="rules" class="h-full m-0 border-0 p-4 outline-none">
                    <div class="text-sm text-muted-foreground flex h-full items-center justify-center">
                      No active dynamic rules configured.
                    </div>
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