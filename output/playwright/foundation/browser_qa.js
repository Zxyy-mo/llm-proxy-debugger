// Run this function through Chrome DevTools evaluate_script on the isolated
// synthetic gateway at http://127.0.0.1:12340/ui/. It never targets a real LLM.
async function foundationQA() {
  if (location.origin !== 'http://127.0.0.1:12340') throw new Error('Synthetic QA origin required')
  const results = window.foundationResults = { checks: [], traces: {}, started_at: new Date().toISOString() }
  const check = (name, value) => { if (!value) throw new Error(name); results.checks.push({ name, passed: true }) }
  const pause = ms => new Promise(resolve => setTimeout(resolve, ms))
  const until = async (test, name, timeout = 8000) => { const start = Date.now(); while (Date.now() - start < timeout) { if (await test()) return; await pause(60) } throw new Error('Timeout: ' + name) }
  const api = async (path, method = 'GET', body) => {
    const response = await fetch(path, { method, headers: body === undefined ? {} : { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) })
    const data = await response.json()
    if (!response.ok) throw new Error(path + ': ' + JSON.stringify(data))
    return data
  }
  const button = (text, root = document) => [...root.querySelectorAll('button,[role=tab]')].find(e => e.textContent.trim() === text)
  const click = async (text, root = document) => { const element = button(text, root); if (!element) throw new Error('Button missing: ' + text); element.scrollIntoView({ block: 'center' }); element.click(); await pause(120) }
  const fill = (selector, value) => { const element = document.querySelector(selector); if (!element) throw new Error('Field missing: ' + selector); element.value = value; element.dispatchEvent(new Event(element.tagName === 'SELECT' ? 'change' : 'input', { bubbles: true })) }
  const modelRequest = async (path, body) => {
    const response = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: 'Bearer qa-browser-client' }, body: JSON.stringify(body) })
    const raw = await response.text(); check('request ' + path + ' ' + body.model, response.status === 200)
    return { trace: response.headers.get('X-Gateway-Trace-ID'), raw, json: body.stream ? null : JSON.parse(raw) }
  }
  const selectTrace = async trace => {
    await click('管理'); await until(() => button('历史记录'), 'management tabs'); await click('历史记录')
    fill('[aria-label="历史关键词"]', trace); await click('查询', document.querySelector('[aria-label="历史查询与清理"]'))
    await until(() => [...document.querySelectorAll('article')].some(e => e.textContent.includes(trace.slice(0, 8))), 'history search')
    const row = [...document.querySelectorAll('article')].find(e => e.textContent.includes(trace.slice(0, 8)))
    await click('查看', row); await until(() => document.querySelector('[aria-label="完整响应"]'), 'response panel')
  }
  const config = {
    providers: [
      { id: 'qa-native', name: '本地原生', base_url: 'http://127.0.0.1:28002/v1', protocol: 'passthrough', key_env: 'GATEWAY_QA_KEY', history: true, websocket: true },
      { id: 'qa-convert', name: '文本与工具转换', base_url: 'http://127.0.0.1:28002/v1', protocol: 'openai', key_env: 'GATEWAY_QA_KEY', history: false, websocket: false },
    ],
    routes: [
      { id: 'native-route', model: 'qa-*', provider_id: 'qa-native', priority: 10, disabled: false },
      { id: 'convert-route', model: 'convert-*', target_model: 'qa-chat', provider_id: 'qa-convert', priority: 20, disabled: false },
    ],
  }
  await api('/api/providers', 'PUT', config)
  for (const rule of await api('/api/rules')) await fetch('/api/rules/' + rule.id, { method: 'DELETE' })
  const baselinePolicy = await api('/api/privacy')
  await api('/api/privacy', 'PUT', { ...baselinePolicy, record: false, outbound: false, retain_raw: true, allow_reveal: false })
  const parent = await modelRequest('/v1/responses', { model: 'qa-native', input: 'context parent' })
  const child = await modelRequest('/v1/responses', { model: 'qa-native', input: 'context child', instructions: '新系统指令', previous_response_id: parent.json.id })
  results.traces.parent = parent.trace; results.traces.child = child.trace
  const childLog = await api('/api/history/' + child.trace)
  check('explicit parent linkage', childLog.correlation.parent_trace_id === parent.trace)
  const diff = await api('/api/context-diff/' + child.trace)
  check('context diff identifies system change and remote context', diff.diff.system.changed && diff.diff.remote_context && diff.base_trace_id === parent.trace)

  const large = await modelRequest('/v1/responses', { model: 'qa-native', input: 'large byte fidelity', stream: true })
  results.traces.large = large.trace
  const downloaded = await fetch('/api/responses/' + large.trace + '/download').then(r => r.text())
  check('large SSE full download equals forwarded bytes', downloaded === large.raw && new TextEncoder().encode(downloaded).length > 100000)
  const largeLog = await api('/api/history/' + large.trace)
  check('accurate tokens and bounded log preview', largeLog.token_sources.input === 'usage' && largeLog.token_sources.output === 'usage' && largeLog.response_body.length < 15000 && largeLog.ttfc_ms >= 0)

  const tools = await modelRequest('/v1/responses', { model: 'qa-native', input: 'tool lookup' })
  results.traces.tools = tools.trace
  const toolLog = await api('/api/history/' + tools.trace)
  const tool = toolLog.tools[0]
  check('tool call observed without fabricated duration', tool.name === 'lookup' && tool.duration_ms === undefined)
  const span = await api('/api/tool-spans', 'POST', { trace_id: tools.trace, span_id: 'qa-span-' + tools.trace, call_id: tool.call_id, name: 'lookup', kind: 'mcp', status: 'done', started_at: '2026-09-10T10:00:00Z', ended_at: '2026-09-10T10:00:00.125Z', input: '{"query":"synthetic"}', output: 'found' })
  check('real reported span duration', span.tools[0].duration_ms === 125 && span.tools[0].source === 'trace')
  const graph = await api('/api/graph')
  check('independent tool graph node', graph.nodes.some(n => n.kind === 'tool' && n.tool?.span_id === 'qa-span-' + tools.trace))

  const converted = await modelRequest('/v1/messages', { model: 'convert-demo', max_tokens: 64, messages: [{ role: 'user', content: 'conversion JSON' }] })
  const convertedSSE = await modelRequest('/v1/responses', { model: 'convert-demo', input: 'conversion SSE', stream: true, store: false })
  results.traces.converted = converted.trace; results.traces.convertedSSE = convertedSSE.trace
  check('Anthropic JSON conversion', converted.json.type === 'message' && converted.json.content[0].text.includes('conversion JSON'))
  check('Responses SSE conversion', convertedSSE.raw.includes('response.completed') && !convertedSSE.raw.includes('gateway_conversion_error'))
  const clientVariant = await api('/api/responses/' + converted.trace)
  const upstreamVariant = await api('/api/responses/' + converted.trace + '?variant=upstream')
  check('conversion variants are distinct and labeled', clientVariant.representation === 'converted-from-openai' && upstreamVariant.representation === 'upstream-original' && JSON.parse(upstreamVariant.body).choices.length === 1)

  await new Promise((resolve, reject) => {
    const socket = new WebSocket('ws://127.0.0.1:12340/v1/responses')
    const timeout = setTimeout(() => { socket.close(); reject(new Error('WebSocket timeout')) }, 8000)
    socket.onerror = () => reject(new Error('WebSocket error'))
    socket.onopen = () => socket.send(JSON.stringify({ type: 'response.create', model: 'qa-native', stream_id: 'browser-lane', input: 'websocket' }))
    socket.onmessage = event => { const body = JSON.parse(event.data); if (body.type === 'error') { clearTimeout(timeout); socket.close(); reject(new Error(event.data)) } if (body.type === 'response.completed') { results.ws_response_id = body.response.id; clearTimeout(timeout); socket.close(); resolve() } }
  })
  const wsLogs = (await api('/api/history?limit=200')).items
  const wsLog = wsLogs.find(log => log.correlation.response_id === results.ws_response_id)
  results.traces.websocket = wsLog.trace_id
  check('browser WebSocket call is recorded', wsLog.websocket.stream_id === 'browser-lane' && wsLog.websocket.frames === 3 && wsLog.status === 'done')

  await selectTrace(child.trace)
  check('history UI opens full response', document.querySelector('[aria-label="完整响应"]').textContent.includes('完整响应'))
  await click('原始 / 出站'); await until(() => button('上下文变化'), 'audit tabs'); await click('上下文变化')
  await until(() => document.querySelector('[aria-label="相邻调用上下文对比"]')?.textContent.includes('新增'), 'context UI')
  check('context UI shows evidence and remote warning', document.querySelector('[aria-label="相邻调用上下文对比"]').textContent.includes('服务端上下文'))

  await selectTrace(parent.trace); await click('原始 / 出站'); await until(() => button('重放'), 'replay tab'); await click('重放')
  await until(() => button('原样重放') && !button('原样重放').disabled, 'server-managed credentials resolve')
  check('replay UI uses provider environment credentials', !document.querySelector('[id^="credential-header"]'))
  await click('原样重放')
  await until(() => document.querySelector('[aria-label="请求重放工作台"]')?.textContent.includes('本次重放：已完成'), 'replay finishes')
  const replays = await api('/api/replays'); const replay = replays.find(r => r.replay_of === parent.trace && r.state === 'done')
  results.traces.replay = replay.trace_id
  await click('对比来源响应')
  await until(() => document.querySelector('[aria-label="重放响应对比"]'), 'response comparison')
  check('replay response comparison renders', Boolean(document.querySelector('[aria-label="重放响应对比"]')))

  await click('管理'); await until(() => button('脱敏策略'), 'privacy tab'); await click('脱敏策略')
  await until(() => document.querySelector('[aria-label="脱敏策略"] input[type=checkbox]'), 'privacy form')
  const checkboxes = document.querySelectorAll('[aria-label="脱敏策略"] input[type=checkbox]')
  if (!checkboxes[0].checked) checkboxes[0].click()
  if (checkboxes[2].checked) checkboxes[2].click()
  await click('保存策略'); await until(() => document.querySelector('[role=status]')?.textContent.includes('已保存'), 'privacy save')
  const privateRequest = await modelRequest('/v1/responses', { model: 'qa-native', input: '邮件 qa.private@example.com' })
  results.traces.private = privateRequest.trace
  const privateCapture = await api('/api/requests/' + privateRequest.trace)
  const privateResponse = await api('/api/responses/' + privateRequest.trace)
  check('privacy display excludes raw request and response', !JSON.stringify(privateCapture).includes('qa.private@example.com') && !privateResponse.body.includes('qa.private@example.com') && privateResponse.body.includes('PRIVATE_email'))
  check('record-only privacy preserves actual response', privateRequest.raw.includes('qa.private@example.com'))

  await click('工具 tracing'); await until(() => document.querySelector('[aria-label="网关管理"] pre'), 'tracing example')
  const example = JSON.parse(document.querySelector('[aria-label="网关管理"] pre').textContent)
  check('tracing example is valid nested JSON', JSON.parse(example.input).query === 'example')
  await api('/api/privacy', 'PUT', { ...baselinePolicy, record: false, outbound: false, retain_raw: true, allow_reveal: false })
  results.finished_at = new Date().toISOString()
  sessionStorage.setItem('foundation-qa-results', JSON.stringify(results))
  return results
}
