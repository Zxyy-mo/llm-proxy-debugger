async (page) => {
  const origin = 'http://127.0.0.1:12341'
  if (!page.url().startsWith(origin + '/')) throw new Error('Isolated operator gateway required')
  const checks = []
  const viewports = []
  const check = (name, value) => { if (!value) throw new Error(name); checks.push({ name, passed: true }) }
  const until = async (test, name, timeout = 10000) => {
    const end = Date.now() + timeout
    while (Date.now() < end) { if (await test()) return; await page.waitForTimeout(50) }
    throw new Error('Timed out: ' + name)
  }
  const api = async path => (await page.request.get(origin + path)).json()
  const all = (await api('/api/history?q=op-loop&limit=200')).items
  const failed = all.find(log => !log.replay && log.status === 'error' && log.summary.includes('op-loop-failure'))
  const parent = await api('/api/history/' + failed.correlation.parent_trace_id)
  const bench = page.getByRole('region', { name: '请求重放工作台' })
  const context = page.getByRole('region', { name: '相邻调用上下文对比' })

  await page.goto(origin + '/ui/')
  for (const [width, height] of [[1600, 1000], [1366, 768], [1024, 768], [768, 600], [390, 844], [320, 640], [844, 390]]) {
    await page.setViewportSize({ width, height })
    await page.keyboard.press('Escape')
    await page.getByRole('button', { name: '调用画布', exact: true }).click()
    const mobileSession = page.getByRole('combobox', { name: '选择会话', exact: true })
    if (await mobileSession.isVisible()) await mobileSession.selectOption('qa-operator-main')
    else await page.getByTitle('qa-operator-main', { exact: true }).click()
    const finder = page.getByRole('textbox', { name: '查找请求', exact: true })
    if (!await finder.isVisible()) await page.locator('summary').filter({ hasText: '筛选请求' }).click()
    await finder.fill('op-loop-failure')
    await page.getByRole('combobox', { name: '请求状态', exact: true }).selectOption('error')
    const mobileRequest = page.getByRole('combobox', { name: '当前请求', exact: true })
    if (await mobileRequest.isVisible()) await mobileRequest.selectOption(failed.trace_id)
    else await page.getByLabel('请求列表', { exact: true }).getByRole('button').filter({ hasText: 'op-loop-failure' }).click()
    await page.getByRole('button', { name: '上下文变化', exact: true }).first().click()
    await until(async () => (await context.innerText()).includes(parent.trace_id.slice(0, 8)), 'context at ' + width)
    await page.getByRole('tab', { name: '重放', exact: true }).click()
    await bench.waitFor({ state: 'visible' })
    const controls = bench.locator('button,input,textarea,select,a')
    let reached = 0
    for (let i = 0; i < await controls.count(); i++) {
      const control = controls.nth(i)
      if (!await control.isVisible() || !await control.isEnabled()) continue
      await control.scrollIntoViewIfNeeded()
      await control.click({ trial: true })
      reached++
    }
    const dimensions = await page.evaluate(() => ({ width: innerWidth, height: innerHeight, scrollWidth: document.documentElement.scrollWidth, scrollHeight: document.documentElement.scrollHeight }))
    check('no document overflow ' + width + 'x' + height, dimensions.scrollWidth <= width + 1 && dimensions.scrollHeight <= height + 1)
    check('replay controls reachable ' + width + 'x' + height, reached >= 4)
    const editor = bench.getByRole('textbox', { name: '重放正文', exact: true })
    check('editor keeps useful height ' + width + 'x' + height, (await editor.boundingBox()).height >= 120)
    viewports.push({ width, height, controls: reached })

    if (width === 390 || width === 844) {
      await bench.getByRole('button', { name: '重置草稿', exact: true }).click()
      await editor.fill((await editor.inputValue()).replace('qa-failure', 'qa-corrected viewport-' + width))
      await bench.getByLabel(/请求头 Authorization/).fill('synthetic-loop-seed')
      const run = bench.getByRole('button', { name: '发送修改后的请求', exact: true })
      await until(() => run.isEnabled(), 'small viewport replay ready')
      await run.click()
      await until(async () => (await bench.innerText()).includes('本次重放：已完成'), 'small viewport replay complete')
      await bench.getByRole('button', { name: '对比来源响应', exact: true }).click()
      const comparison = page.getByRole('region', { name: '重放结果评估' })
      await until(async () => (await comparison.innerText()).includes('viewport-' + width), 'small viewport result evaluation')
      await comparison.getByRole('button', { name: '查看重放调用', exact: true }).scrollIntoViewIfNeeded()
      await comparison.getByRole('button', { name: '查看重放调用', exact: true }).click({ trial: true })
      check('full correction loop works ' + width + 'x' + height, (await comparison.innerText()).includes('429') && (await comparison.innerText()).includes('200'))
      await page.screenshot({ path: 'output/playwright/agent-debug-loop/' + (width === 390 ? 'mobile-evaluation.png' : 'landscape-evaluation.png'), fullPage: false })
    }
  }

  // Explicit list selection focuses one readable node; a later manual zoom
  // survives a metric-only tool-span update.
  await page.setViewportSize({ width: 1600, height: 1000 })
  await page.getByRole('button', { name: '调用画布', exact: true }).click()
  await page.getByTitle('qa-operator-main', { exact: true }).click()
  const requestList = page.getByLabel('请求列表', { exact: true })
  await requestList.getByRole('button').filter({ hasText: 'op-loop-failure' }).click()
  const selectedNode = page.getByRole('button', { name: /^调用：.*op-loop-failure qa-failure/ })
  await until(async () => (await selectedNode.boundingBox())?.width >= 150, 'selected node readable')
  check('explicit request selection gives readable graph scale', (await selectedNode.boundingBox()).width >= 150)
  const graph = page.getByRole('region', { name: '调用路径画布' })
  const box = await graph.boundingBox()
  await page.mouse.move(box.x + box.width * 0.4, box.y + box.height * 0.5)
  await page.mouse.wheel(0, 160)
  await page.waitForTimeout(250)
  const transform = await page.locator('.vue-flow__transformationpane').getAttribute('style')
  const call = parent.tools[0]
  const update = await page.request.post(origin + '/api/tool-spans', { data: {
    trace_id: parent.trace_id, span_id: call.span_id, call_id: call.call_id, name: call.name,
    kind: 'mcp', status: 'done', started_at: '2026-09-11T00:00:00Z', ended_at: '2026-09-11T00:00:00.125Z',
    input: '{"query":"deployment guide"}', output: 'Found the deployment guide; metric refresh QA.',
  } })
  check('metric-only update accepted', update.ok())
  await page.waitForTimeout(500)
  check('manual graph camera survives metric refresh', await page.locator('.vue-flow__transformationpane').getAttribute('style') === transform)
  await page.getByRole('button', { name: '显示全图', exact: true }).click()
  await page.waitForTimeout(200)
  check('Show All still restores overview', (await selectedNode.boundingBox()).width < 150)
  await requestList.getByRole('button').filter({ hasText: 'op-loop-failure' }).click()
  await until(async () => (await selectedNode.boundingBox())?.width >= 150, 'same-row refocus')
  await page.screenshot({ path: 'output/playwright/agent-debug-loop/focused-investigation.png', fullPage: false })
  return { checks, viewports }
}
