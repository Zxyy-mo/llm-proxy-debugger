async (page) => {
  const origin = 'http://127.0.0.1:12341'
  if (!page.url().startsWith(origin + '/')) throw new Error('Isolated operator gateway required')
  const checks = []
  const check = (name, value) => { if (!value) throw new Error(name); checks.push({ name, passed: true }) }
  const until = async (test, name, timeout = 10000) => {
    const end = Date.now() + timeout
    while (Date.now() < end) { if (await test()) return; await page.waitForTimeout(50) }
    throw new Error('Timed out: ' + name)
  }
  const api = async path => {
    const response = await page.request.get(origin + path)
    if (!response.ok()) throw new Error(path + ': ' + response.status())
    return response.json()
  }
  await page.setViewportSize({ width: 1600, height: 1000 })
  await page.goto(origin + '/ui/')
  const logs = (await api('/api/history?q=op-loop&limit=200')).items
  const failed = logs.find(log => !log.replay && log.status === 'error' && log.summary.includes('op-loop-failure'))
  const delay = logs.find(log => !log.replay && log.summary === 'op-loop-delay-source')
  const corrected = logs.find(log => log.replay?.of === failed.trace_id && log.status === 'done')
  const selectTrace = async trace => {
    await page.keyboard.press('Escape')
    await page.getByRole('button', { name: '管理', exact: true }).click()
    await page.getByRole('button', { name: '历史记录', exact: true }).click()
    const history = page.getByRole('region', { name: '历史查询与清理' })
    await history.getByRole('textbox', { name: '历史关键词' }).fill(trace)
    await history.getByRole('button', { name: '查询', exact: true }).click()
    await history.locator('article').filter({ hasText: trace.slice(0, 8) }).getByRole('button', { name: '查看', exact: true }).click()
    await page.getByRole('region', { name: '响应与调用详情' }).waitFor()
  }
  const bench = page.getByRole('region', { name: '请求重放工作台' })
  await selectTrace(delay.trace_id)
  await page.getByRole('button', { name: '编辑并重放', exact: true }).first().click()
  const editor = bench.getByRole('textbox', { name: '重放正文', exact: true })
  await editor.waitFor()
  await bench.getByRole('button', { name: '重置草稿', exact: true }).click()
  await editor.fill((await editor.inputValue()).replace('op-loop-delay-source', 'op-delay op-loop-delay-source'))
  await bench.getByLabel(/请求头 Authorization/).fill('synthetic-loop-seed')
  const run = bench.getByRole('button', { name: '发送修改后的请求', exact: true })
  await until(() => run.isEnabled(), 'delayed comparison request ready')
  await run.click()
  await bench.getByRole('button', { name: '对比来源响应', exact: true }).click()
  const comparison = page.getByRole('region', { name: '重放结果评估' })
  const result = comparison.getByRole('region', { name: '重放调用', exact: true })
  await until(async () => (await result.innerText()).includes('进行中'), 'comparison initially running')
  check('comparison can be opened while executing', (await result.innerText()).includes('进行中'))
  await until(async () => (await result.innerText()).includes('已完成') && (await result.innerText()).includes('op-delay'), 'comparison follows completion', 25000)
  check('open comparison refreshes output and terminal state automatically', (await result.innerText()).includes('200'))
  const fullReplay = page.getByRole('region', { name: '重放响应', exact: true })
  await until(() => fullReplay.getByRole('link', { name: '下载完整响应' }).isVisible(), 'completed response downloadable')
  check('raw response follows the same terminal transition', await fullReplay.getByRole('link', { name: '下载完整响应' }).isVisible())

  // A missing source must not hide its surviving replay result.
  const missingLog = origin + '/api/history/' + failed.trace_id
  const missingBody = '**/api/responses/' + failed.trace_id + '?**'
  await page.route(missingLog, route => route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"synthetic deleted source"}' }))
  await page.route(missingBody, route => route.fulfill({ status: 404, contentType: 'application/json', body: '{"error":"synthetic deleted source"}' }))
  try {
    await selectTrace(corrected.trace_id)
    const sourceSide = comparison.getByRole('region', { name: '来源调用', exact: true })
    await until(async () => (await sourceSide.innerText()).includes('已删除'), 'missing source message')
    check('missing source is explicit and cannot be navigated', await sourceSide.getByRole('button', { name: '查看来源调用' }).isDisabled())
    check('surviving result remains readable', (await result.innerText()).includes('qa-corrected') && (await result.innerText()).includes('已完成'))
  } finally {
    await page.unroute(missingLog)
    await page.unroute(missingBody)
  }
  await comparison.getByRole('button', { name: '刷新对比' }).click()
  await until(() => comparison.getByRole('button', { name: '查看来源调用' }).isEnabled(), 'source recovery')
  check('source metadata can recover without losing result', (await result.innerText()).includes('qa-corrected'))

  // Exercise the actual editor path, not just the bounded helper in isolation.
  const raw = '{"model":"qa-debugger","messages":[{"role":"user","content":"op-loop-deep-editor"}],"seed":9007199254740993,"deep":' + '['.repeat(200) + '0' + ']'.repeat(200) + '}'
  const captured = await page.request.post(origin + '/v1/chat/completions', {
    headers: { 'Content-Type': 'application/json', Authorization: 'Bearer synthetic-loop-seed', 'X-Session-ID': 'qa-operator-deep' },
    data: raw,
  })
  check('deep but valid capture is forwarded', captured.ok())
  const deepTrace = captured.headers()['x-gateway-trace-id']
  await selectTrace(deepTrace)
  await page.getByRole('button', { name: '编辑并重放', exact: true }).first().click()
  await editor.waitFor()
  check('editor does not expand deep JSON', await editor.inputValue() === raw)
  const difference = bench.getByRole('region', { name: '请求差异' })
  check('incomplete structural comparison is disclosed', (await difference.innerText()).includes('嵌套层级过深') && !(await difference.innerText()).includes('正文与请求头没有内容差异'))
  await page.screenshot({ path: 'output/playwright/agent-debug-loop/deep-editor.png', fullPage: false })

  // Isolate initial empty/error states without touching stored captures.
  const storage = await page.evaluate(() => Object.entries(localStorage))
  const sessionsRoute = origin + '/api/sessions'
  await page.route(sessionsRoute, route => route.fulfill({ status: 503, contentType: 'application/json', body: '{"error":"Synthetic session service unavailable"}' }))
  try {
    await page.evaluate(() => localStorage.clear())
    await page.reload()
    const captureState = page.getByLabel('捕获状态', { exact: true })
    await captureState.getByRole('button', { name: '重试加载会话' }).waitFor()
    check('initial fetch failure is distinct from no captures', (await captureState.innerText()).includes('重试加载会话') && !(await captureState.innerText()).includes('开始捕获模型调用'))
    await page.unroute(sessionsRoute)
    await captureState.getByRole('button', { name: '重试加载会话' }).click()
    await page.getByTitle('qa-operator-main', { exact: true }).waitFor()
    check('initial fetch failure can be retried', await page.getByTitle('qa-operator-main', { exact: true }).isVisible())
    await page.route(sessionsRoute, route => route.fulfill({ contentType: 'application/json', body: '{}' }))
    await page.evaluate(() => localStorage.clear())
    await page.reload()
    await page.getByRole('heading', { name: '开始捕获模型调用' }).waitFor()
    check('empty capture state explains the next action', await page.getByRole('button', { name: '检查 Provider 与路由' }).isVisible())
    await page.getByRole('button', { name: '设置', exact: true }).click()
    check('header settings button is functional', await page.getByRole('button', { name: 'Provider 与路由', exact: true }).isVisible())
  } finally {
    await page.unroute(sessionsRoute)
    await page.evaluate(entries => { localStorage.clear(); for (const [key, value] of entries) localStorage.setItem(key, value) }, storage)
    await page.goto(origin + '/ui/')
  }
  return { checks, deep_trace: deepTrace, source_trace: failed.trace_id, surviving_replay: corrected.trace_id }
}
