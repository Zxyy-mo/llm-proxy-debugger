async (page) => {
  const origin = 'http://127.0.0.1:12341'
  if (!page.url().startsWith(origin + '/')) throw new Error('Isolated operator gateway required')
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.goto(origin + '/ui/')
  const checks = []
  const check = (name, value) => { if (!value) throw new Error(name); checks.push({ name, passed: true }) }
  const until = async (test, name, timeout = 10000) => {
    const end = Date.now() + timeout
    while (Date.now() < end) { if (await test()) return; await page.waitForTimeout(50) }
    throw new Error('Timed out: ' + name)
  }
  const api = async path => (await page.request.get(origin + path)).json()
  const upstream = async () => (await page.request.get('http://127.0.0.1:28003/')).json()
  const countTag = async tag => (await upstream()).requests.filter(item => item.tag === tag).length
  const logs = (await api('/api/history?q=op-loop&limit=200')).items
  const delay = logs.find(log => !log.replay && log.summary === 'op-loop-delay-source')
  const uncertain = logs.find(log => !log.replay && log.summary === 'op-loop-uncertain-source')
  check('operation fixtures are available', Boolean(delay && uncertain))
  const bench = page.getByRole('region', { name: '请求重放工作台' })
  const editor = bench.getByRole('textbox', { name: '重放正文', exact: true })
  const selectTrace = async trace => {
    await page.getByRole('button', { name: '管理', exact: true }).click()
    await page.getByRole('button', { name: '历史记录', exact: true }).click()
    const history = page.getByRole('region', { name: '历史查询与清理' })
    await history.getByRole('textbox', { name: '历史关键词' }).fill(trace)
    await history.getByRole('button', { name: '查询', exact: true }).click()
    await history.locator('article').filter({ hasText: trace.slice(0, 8) }).getByRole('button', { name: '查看', exact: true }).click()
    await page.getByRole('button', { name: '编辑并重放', exact: true }).first().click()
    await editor.waitFor({ state: 'visible' })
  }
  const credential = () => bench.getByLabel(/请求头 Authorization/)
  const run = () => bench.getByRole('button', { name: /^(原样重放|发送修改后的请求)$/ })
  const current = async trace => (await api('/api/replays')).filter(record => record.replay_of === trace).sort((a, b) => b.created_at.localeCompare(a.created_at))[0]

  await selectTrace(delay.trace_id)
  await credential().fill('synthetic-loop-seed')
  await editor.fill((await editor.inputValue()).replace('op-loop-delay-source', 'op-delay op-loop-delay-source'))
  await until(() => run().isEnabled(), 'delayed replay ready')
  const beforeDelay = await countTag('operator-delay_source')
  await run().click()
  await until(async () => (await current(delay.trace_id))?.state === 'running', 'running replay')
  const first = await current(delay.trace_id)
  await bench.getByRole('button', { name: '重置草稿', exact: true }).click()
  check('reset keeps the active cancellation control', await bench.getByRole('button', { name: '取消重放', exact: true }).isVisible())
  check('reset does not enable another run', await run().isDisabled())
  await page.getByRole('tab', { name: '上下文变化', exact: true }).click()
  await page.getByRole('button', { name: '调用画布', exact: true }).click()
  check('running operation remains visible outside the editor', await page.getByRole('button', { name: '取消重放', exact: true }).isVisible())
  await page.getByRole('button', { name: /^返回重放工作台/ }).click()
  await bench.getByRole('button', { name: '取消重放', exact: true }).click()
  await until(async () => (await api('/api/replays/' + first.id)).state === 'canceled', 'manual cancellation')
  await until(async () => (await upstream()).inflight === 0, 'upstream cancellation')
  check('reset/navigation created exactly one execution', await countTag('operator-delay_source') === beforeDelay + 1)

  // An already accepted run is found after reload; the page never resumes it.
  await credential().fill('synthetic-loop-seed')
  await editor.fill((await editor.inputValue()).replace('op-loop-delay-source', 'op-delay op-loop-delay-source'))
  await until(() => run().isEnabled(), 'second deliberate run ready')
  await run().click()
  await until(async () => (await current(delay.trace_id))?.state === 'running', 'second running replay')
  const second = await current(delay.trace_id)
  const beforeReload = await countTag('operator-delay_source')
  await page.reload()
  await until(() => bench.getByRole('button', { name: '取消重放', exact: true }).isVisible(), 'running replay restored after reload')
  check('reload does not send another model request', await countTag('operator-delay_source') === beforeReload)
  await bench.getByRole('button', { name: '取消重放', exact: true }).click()
  await until(async () => (await api('/api/replays/' + second.id)).state === 'canceled', 'restored cancellation')
  await until(async () => (await upstream()).inflight === 0, 'restored upstream cancellation')
  check('restored operation keeps its identity', second.id !== first.id && await countTag('operator-delay_source') === beforeDelay + 2)

  const ambiguity = []
  for (const mode of ['both-lost', 'retry-503']) {
    await selectTrace(uncertain.trace_id)
    await bench.getByRole('button', { name: '重置草稿', exact: true }).click()
    check(mode + ' new source has no carried credential', await credential().inputValue() === '')
    await credential().fill('synthetic-loop-seed')
    await until(() => run().isEnabled(), mode + ' ready')
    let blockList = false
    let posts = 0
    const keys = []
    const routePattern = origin + '/api/replays'
    await page.route(routePattern, async route => {
      const request = route.request()
      if (request.method() === 'POST') {
        posts++
        blockList = true
        keys.push(request.postDataJSON().idempotency_key)
        if (mode === 'retry-503' && posts === 2) {
          await route.fulfill({ status: 503, contentType: 'application/json', body: '{"error":"Synthetic temporary outage after the first accepted response was lost"}' })
        } else {
          await route.fetch()
          await route.abort('failed')
        }
      } else if (blockList) await route.abort('failed')
      else await route.continue()
    })
    const before = await countTag('operator-uncertain_source')
    await run().click()
    await until(async () => posts === 2 && await bench.getByRole('button', { name: '恢复本次重放', exact: true }).isVisible(), mode + ' recoverable uncertainty')
    check(mode + ' preserves one key across hidden retry', keys.length === 2 && keys[0] === keys[1])
    check(mode + ' blocks a new implicit action', await run().isDisabled())
    await page.screenshot({ path: 'output/playwright/agent-debug-loop/uncertain-' + mode + '.png', fullPage: true })
    blockList = false
    await bench.getByRole('button', { name: '恢复本次重放', exact: true }).click()
    await until(async () => (await bench.innerText()).includes('已找回本次重放'), mode + ' recovery')
    check(mode + ' recovery is read-only and sends once', posts === 2 && await countTag('operator-uncertain_source') === before + 1)
    const recovered = (await api('/api/replays')).find(record => record.idempotency_key === keys[0])
    check(mode + ' resolves the accepted record', recovered?.replay_of === uncertain.trace_id && recovered.state === 'done')
    ambiguity.push({ mode, key: keys[0], trace: recovered.trace_id, posts, executions: 1 })
    await page.unroute(routePattern)
  }
  check('no replay credential is persisted in browser storage', !(await page.evaluate(() => JSON.stringify(Object.entries(localStorage)) + JSON.stringify(Object.entries(sessionStorage)))).includes('synthetic-loop-seed'))
  return { checks, ambiguity, canceled: [first.id, second.id] }
}
