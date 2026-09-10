async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const api = 'http://127.0.0.1:12338'
  const json = async url => (await page.request.get(url)).json()
  const upstream = async () => (await json('http://127.0.0.1:28001/')).records
  const waitInflight = async (expected, timeout = 5000) => {
    const deadline = Date.now() + timeout
    while (Date.now() < deadline) {
      const inflight = (await json('http://127.0.0.1:28001/')).inflight
      if (expected === 'any' ? inflight > 0 : inflight === expected) return inflight
      await page.waitForTimeout(50)
    }
    throw new Error('upstream inflight did not reach ' + expected)
  }
  const errors = []
  page.on('pageerror', error => errors.push(String(error)))
  const trace = (await page.evaluate(() => JSON.parse(localStorage.getItem('llm-debugger.selection') || '{}').trace))
  assert(trace, 'no selected trace')
  const results = {}

  await page.goto('http://localhost:5174/')
  await page.getByRole('heading', { name: '原始 / 出站请求', exact: true }).waitFor()
  await page.getByRole('tab', { name: '重放' }).click()
  const bench = page.getByRole('region', { name: '请求重放工作台' })
  await bench.waitFor()
  const body = bench.getByRole('textbox', { name: '重放正文', exact: true })
  const original = await body.inputValue()
  assert(original.includes('QA_INJECTED'), 'outgoing draft should contain the injected system')
  const run = bench.getByRole('button', { name: /原样重放|发送修改后的请求/ })
  assert(await run.isDisabled(), 'replay must wait for credentials')
  const benchText = await bench.innerText()
  assert(/尚未填写：(Authorization、key|key、Authorization)。/.test(benchText), 'missing credentials not listed')

  // Validation works before credentials are supplied and reports what is missing.
  await bench.getByRole('button', { name: '校验', exact: true }).click()
  await page.getByRole('status').filter({ hasText: '结构校验通过' }).waitFor()
  assert((await page.getByRole('status').innerText()).includes('仍需补充凭证'), 'validation should report missing credentials')

  // An invalid edit is rejected without executing anything.
  const beforeCount = (await upstream()).length
  await bench.getByLabel('请求头 Authorization', { exact: false }).fill('qa-replay-secret')
  await bench.getByLabel('查询参数 key', { exact: false }).fill('qa-key-replay')
  await body.fill('{"model":')
  await bench.getByRole('button', { name: '校验', exact: true }).click()
  await page.getByRole('alert').filter({ hasText: 'invalid JSON' }).waitFor()
  assert((await upstream()).length === beforeCount && (await json(api + '/api/replays')).length === 0, 'invalid draft must not execute')

  // A valid edit executes once, with transient credentials and no double injection.
  // Edit the text directly (no JSON.parse) so the large integer must survive
  // the editor's own formatting.
  assert(original.includes('9007199254740993'), 'formatted draft lost the large integer')
  await body.fill(original.replace("it's $HOME", 'Edited in the browser before replay'))
  await bench.getByRole('button', { name: '发送修改后的请求', exact: true }).click()
  await page.getByRole('status').filter({ hasText: '已发起重放' }).waitFor()
  await bench.getByText(/本次重放：已完成 · HTTP 200/).waitFor({ timeout: 10000 })
  const replays = await json(api + '/api/replays')
  assert(replays.length === 1 && replays[0].state === 'done' && replays[0].modified && replays[0].replay_of === trace && replays[0].source === 'outgoing', 'replay record wrong: ' + JSON.stringify(replays))
  const hits = await upstream()
  const hit = hits[hits.length - 1]
  assert(hits.length === beforeCount + 1 && hit.auth === 'replay' && hit.query === 'trace=qa&key=qa-key-replay' && hit.system_count === 1, 'upstream request wrong: ' + JSON.stringify(hit))
  assert(hit.body.includes('"input": "Edited in the browser before replay') && hit.body.includes('9007199254740993') && hit.body.includes('\\u4f60\\u597d'), 'edited body not sent faithfully: ' + hit.body)
  const replayTrace = replays[0].trace_id
  const replayLog = Object.values(await json(api + '/api/sessions')).flatMap(session => session.logs).find(log => log.trace_id === replayTrace)
  assert(replayLog && replayLog.replay && replayLog.replay.of === trace && replayLog.status === 'done', 'replay log missing provenance')
  const sourceLog = Object.values(await json(api + '/api/sessions')).flatMap(session => session.logs).find(log => log.trace_id === trace)
  assert(sourceLog.session_id === replayLog.session_id && !sourceLog.replay, 'source record changed or sessions differ')
  const leaked = JSON.stringify([await json(api + '/api/sessions'), await json(api + '/api/replays'), await json(api + '/api/requests/' + replayTrace)])
  assert(!leaked.includes('qa-replay-secret') && !leaked.includes('qa-key-replay'), 'transient credentials leaked into gateway records')
  assert(!(await page.evaluate(() => JSON.stringify(Object.entries(localStorage)) + JSON.stringify(Object.entries(sessionStorage)))).includes('qa-replay-secret'), 'credential persisted in browser storage')
  results.edited_replay = { trace: replayTrace, upstream_auth: hit.auth, injected_once: hit.system_count === 1 }
  await page.screenshot({ path: 'output/playwright/replay/workbench-done.png', fullPage: true })

  // Provenance: jump to the new call, inspect it, jump back to the source.
  await bench.getByRole('button', { name: '查看新调用', exact: true }).click()
  await page.getByRole('heading', { name: 'Details', exact: true }).waitFor()
  assert((await page.getByLabel('请求与响应报文').innerText()).includes('Edited in the browser before replay'), 'details view does not show the replayed request')
  await page.getByRole('button', { name: '调用画布', exact: true }).click()
  const inspector = page.getByRole('complementary', { name: '调用详情' })
  await inspector.getByRole('region', { name: '重放来源' }).waitFor()
  assert((await inspector.innerText()).includes('重放自实际出站请求，发送前有修改'), 'inspector provenance text missing')
  await page.getByText('重放来源', { exact: true }).first().waitFor()
  const replayNode = page.getByRole('button', { name: /^重放调用：/ })
  await replayNode.first().waitFor()
  assert((await replayNode.first().innerText()).includes('重放·已修改'), 'graph node badge missing')
  const graph = await json(api + '/api/graph?session_id=' + encodeURIComponent(sourceLog.session_id))
  const replayEdges = graph.edges.filter(edge => edge.kind === 'replay')
  assert(replayEdges.length === 1 && replayEdges[0].source === trace && replayEdges[0].target === replayTrace && graph.edges.length === 1, 'graph edges wrong: ' + JSON.stringify(graph.edges))
  await page.screenshot({ path: 'output/playwright/replay/graph-provenance.png', fullPage: true })
  await inspector.getByRole('button', { name: /查看被重放的请求/ }).click()
  await page.waitForFunction(id => JSON.parse(localStorage.getItem('llm-debugger.selection') || '{}').trace === id, trace)
  results.provenance = { inspector: true, graph_badge: true, replay_edges: replayEdges.length, jump_back: true }

  // Cancel a slow replay, then let one time out.
  await page.getByRole('button', { name: '原始 / 出站', exact: true }).click()
  await page.getByRole('tab', { name: '重放' }).click()
  await bench.getByLabel('请求头 Authorization', { exact: false }).fill('qa-replay-secret')
  await bench.getByLabel('查询参数 key', { exact: false }).fill('qa-key-replay')
  await body.fill(original.replace("it's $HOME", 'slow replay to cancel'))
  await bench.getByRole('button', { name: '发送修改后的请求', exact: true }).click()
  await bench.getByText(/本次重放：重放中/).waitFor()
  await waitInflight('any')
  await bench.getByRole('button', { name: '取消重放', exact: true }).click()
  await bench.getByText(/本次重放：已取消（手动）/).waitFor({ timeout: 10000 })
  await waitInflight(0)
  const canceled = (await json(api + '/api/replays')).find(item => item.reason === 'manual')
  const canceledLog = Object.values(await json(api + '/api/sessions')).flatMap(session => session.logs).find(log => log.trace_id === canceled.trace_id)
  assert(canceled.state === 'canceled' && canceledLog.status === 'canceled' && canceledLog.status_code === 499, 'cancel not reflected: ' + JSON.stringify(canceledLog))
  results.cancel = { state: canceled.state, log_status: canceledLog.status, status_code: canceledLog.status_code }

  await bench.getByLabel('超时（秒）').fill('2')
  await body.fill(original.replace("it's $HOME", 'slow replay to time out'))
  await bench.getByRole('button', { name: '发送修改后的请求', exact: true }).click()
  await bench.getByText(/本次重放：失败（超时） · HTTP 504/).waitFor({ timeout: 15000 })
  const timedOut = (await json(api + '/api/replays')).find(item => item.reason === 'timeout')
  assert(timedOut && timedOut.state === 'error' && timedOut.timeout_seconds === 2, 'timeout not recorded: ' + JSON.stringify(timedOut))
  results.timeout = { state: timedOut.state, status_code: timedOut.status_code }
  assert((await bench.innerText()).includes('此请求的重放记录 · 3'), 'history list did not show three replays')

  // Refresh and reconnect are read-only.
  const replayCount = (await json(api + '/api/replays')).length
  const upstreamCount = (await upstream()).length
  await page.reload()
  await page.getByRole('heading', { name: '原始 / 出站请求', exact: true }).waitFor()
  await page.waitForTimeout(1500)
  assert((await json(api + '/api/replays')).length === replayCount && (await upstream()).length === upstreamCount, 'reload executed a replay')
  results.reload_read_only = true
  results.page_errors = errors
  return JSON.stringify(results)
}
