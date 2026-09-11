async (page) => {
  const origin = 'http://127.0.0.1:12341'
  if (!page.url().startsWith(origin + '/')) throw new Error('Isolated QA gateway required')
  await page.goto(origin + '/ui/')
  const checks = []
  const errors = []
  const requests = []
  page.on('pageerror', error => errors.push(String(error)))
  page.on('request', request => {
    if (request.url().includes('/api/responses/')) requests.push(request.url())
  })
  const check = (name, value) => {
    if (!value) throw new Error(name)
    checks.push({ name, passed: true })
  }
  const until = async (fn, name) => {
    const end = Date.now() + 10000
    while (Date.now() < end) {
      if (await fn()) return
      await page.waitForTimeout(40)
    }
    throw new Error('Timed out: ' + name)
  }
  const api = async path => {
    const response = await page.request.get(origin + path)
    if (!response.ok()) throw new Error(path + ' returned ' + response.status())
    return response.json()
  }
  const logs = (await api('/api/history?limit=200')).items
  const source = logs.find(log => log.summary === 'qa-large-json' && !log.replay)
  const stream = logs.find(log => log.summary === 'qa-large-sse')
  const empty = logs.find(log => log.summary === 'qa-empty')
  check('seeded captures are available', Boolean(source && stream && empty))

  const panel = title => page.getByRole('region', { name: title, exact: true })
  const settled = async title => {
    await panel(title).waitFor({ state: 'visible' })
    await until(async () => (await panel(title).getAttribute('aria-busy')) === 'false', title)
  }
  const body = title => panel(title).getByLabel(title + '正文', { exact: true })
  const range = title => panel(title).getByRole('navigation', { name: title + '分段导航' }).getByRole('status')
  const selectTrace = async trace => {
    await page.getByRole('button', { name: '管理', exact: true }).click()
    await page.getByRole('button', { name: '历史记录', exact: true }).click()
    const history = page.getByRole('region', { name: '历史查询与清理' })
    await history.getByRole('textbox', { name: '历史关键词' }).fill(trace)
    await history.getByRole('button', { name: '查询', exact: true }).click()
    const item = history.locator('article').filter({ hasText: trace.slice(0, 8) })
    await item.getByRole('button', { name: '查看', exact: true }).click()
    await settled('完整响应')
  }

  await page.setViewportSize({ width: 1440, height: 900 })
  await selectTrace(source.trace_id)
  const firstBody = await body('完整响应').textContent()
  const firstRange = await range('完整响应').innerText()
  check('large JSON starts with the actual first part', firstBody.includes('QA_BEGIN_完整响应'))
  check('rendered JSON is bounded', firstBody.length <= 262148)
  await panel('完整响应').getByRole('button', { name: '下一段', exact: true }).click()
  await settled('完整响应')
  check('next part changes the range', (await range('完整响应').innerText()) !== firstRange)
  await panel('完整响应').getByRole('button', { name: '上一段', exact: true }).click()
  await settled('完整响应')
  check('previous returns exact first text', (await body('完整响应').textContent()) === firstBody)
  await panel('完整响应').getByRole('button', { name: '查看末段', exact: true }).click()
  await settled('完整响应')
  const finalRange = await range('完整响应').innerText()
  check('JSON end marker beyond 2 MiB is reachable', (await body('完整响应').textContent()).includes('QA_END_完整响应'))
  check('final text has no replacement characters', !(await body('完整响应').textContent()).includes('\ufffd'))
  check('no next part at EOF', await panel('完整响应').getByRole('button', { name: '下一段', exact: true }).isDisabled())
  await panel('完整响应').getByRole('button', { name: '刷新', exact: true }).click()
  await settled('完整响应')
  check('refresh preserves the selected part', (await range('完整响应').innerText()) === finalRange)
  await page.screenshot({ path: 'output/playwright/foundation-refinement/desktop-response.png', fullPage: true })
  await panel('完整响应').getByRole('button', { name: '回到开头', exact: true }).click()
  await settled('完整响应')
  check('reset returns to the first byte range', (await range('完整响应').innerText()) === firstRange)

  await selectTrace(stream.trace_id)
  await panel('完整响应').getByRole('button', { name: '查看末段', exact: true }).click()
  await settled('完整响应')
  check('SSE end marker and completion event are visible', (await body('完整响应').textContent()).includes('QA_END_完整响应') && (await body('完整响应').textContent()).includes('[DONE]'))

  await selectTrace(source.trace_id)
  await page.getByRole('button', { name: '原始 / 出站', exact: true }).click()
  await page.getByRole('tab', { name: '重放', exact: true }).click()
  const workbench = page.getByRole('region', { name: '请求重放工作台' })
  await workbench.getByRole('button', { name: '对比响应', exact: true }).first().click()
  await settled('来源响应')
  await settled('重放响应')
  const replayFirst = await range('重放响应').innerText()
  await panel('来源响应').getByRole('button', { name: '查看末段', exact: true }).click()
  await settled('来源响应')
  check('source paging does not move replay', (await range('重放响应').innerText()) === replayFirst)
  check('source final part is visible in comparison', (await body('来源响应').textContent()).includes('QA_END_完整响应'))
  await panel('重放响应').getByRole('button', { name: '下一段', exact: true }).click()
  await settled('重放响应')
  check('replay can page independently', (await range('重放响应').innerText()) !== replayFirst)
  await page.screenshot({ path: 'output/playwright/foundation-refinement/comparison.png', fullPage: true })

  const viewports = []
  for (const [width, height] of [[1440, 900], [390, 844], [320, 640], [844, 390]]) {
    await page.setViewportSize({ width, height })
    for (const title of ['来源响应', '重放响应']) {
      const buttons = panel(title).getByRole('button')
      for (let index = 0; index < await buttons.count(); index++) {
        const button = buttons.nth(index)
        if (!await button.isEnabled()) continue
        await button.scrollIntoViewIfNeeded()
        await button.click({ trial: true })
      }
      const dimensions = await panel(title).evaluate(element => ({ width: element.clientWidth, scroll: element.scrollWidth }))
      check(title + ' fits ' + width + 'x' + height, dimensions.scroll <= dimensions.width + 1)
    }
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1)
    check('no page horizontal overflow ' + width + 'x' + height, !overflow)
    viewports.push({ width, height, reachable: true })
    if (width === 390) await page.screenshot({ path: 'output/playwright/foundation-refinement/mobile-comparison.png', fullPage: true })
    if (width === 844) await page.screenshot({ path: 'output/playwright/foundation-refinement/landscape-comparison.png', fullPage: true })
  }

  await page.setViewportSize({ width: 1440, height: 900 })
  await selectTrace(empty.trace_id)
  const actual = await api('/api/responses/' + empty.trace_id)
  const routePattern = '**/api/responses/' + empty.trace_id + '?**'
  await page.route(routePattern, route => route.fulfill({
    contentType: 'application/json',
    body: JSON.stringify({ ...actual, stored: 'missing', reason: '合成检查：响应文件已被清理', body: '' }),
  }))
  await panel('完整响应').getByRole('button', { name: '刷新', exact: true }).click()
  await settled('完整响应')
  check('missing-file state is explicit', (await panel('完整响应').innerText()).includes('响应文件已被清理'))
  await page.unroute(routePattern)
  await panel('完整响应').getByRole('button', { name: '刷新', exact: true }).click()
  await settled('完整响应')
  check('refresh recovers after missing-file fixture', await body('完整响应').isVisible())
  await page.route(routePattern, route => route.fulfill({
    contentType: 'application/json',
    body: JSON.stringify({ ...actual, bytes: 0, offset: 0, end: 0, next_offset: null, last_offset: 0, body: '', truncated: false }),
  }))
  await panel('完整响应').getByRole('button', { name: '刷新', exact: true }).click()
  await settled('完整响应')
  check('zero-byte state is readable', (await body('完整响应').textContent()).includes('空响应'))
  check('zero-byte pagination cannot advance', await panel('完整响应').getByRole('button', { name: '下一段', exact: true }).isDisabled())
  await page.unroute(routePattern)
  await page.reload()
  check('browser has no uncaught exceptions', errors.length === 0)
  check('UI issued bounded reads beyond 2 MiB', requests.some(url => Number(url.match(/[?&]offset=(\d+)/)?.[1] ?? 0) > 2 * 1024 * 1024 && /[?&]limit=262144(?:&|$)/.test(url)))
  return { checks, viewports, errors, source_trace: source.trace_id, stream_trace: stream.trace_id }
}
