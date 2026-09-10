async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const api = 'http://127.0.0.1:12338'
  const json = async url => (await page.request.get(url)).json()
  const rules = await json(api + '/api/rules')
  const rule = rules.find(item => item.path_match === '/responses')
  const flights = []
  const layouts = []
  await page.request.put(api + '/api/rules/' + rule.id, { data: { ...rule, disabled: false, wait_seconds: 120 } })
  try {
    for (let index = 0; index < 3; index++) {
      flights.push(page.request.post(api + '/v1/responses', { data: { model: 'qa-layout-' + index, input: 'Layout with three pending requests' }, timeout: 150000 }))
    }
    for (let i = 0; i < 80; i++) {
      if ((await json(api + '/api/interceptions')).requests.filter(item => item.model.startsWith('qa-layout-')).length === 3) break
      await page.waitForTimeout(25)
    }
    await page.getByRole('button', { name: /^待处理/ }).click()
    await page.getByLabel('待处理请求列表').getByRole('button', { name: /^qa-layout-0 / }).click()
    await page.getByRole('textbox', { name: '本次请求正文', exact: true }).waitFor()
    for (const [width, height] of [[1600,1000], [1366,768], [1024,768], [768,600], [390,844], [320,640], [844,390]]) {
      await page.setViewportSize({ width, height })
      const button = page.getByRole('button', { name: '放行', exact: true })
      await button.click({ trial: true, timeout: 2500 })
      const bounds = await button.boundingBox()
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)
      assert(bounds && bounds.x >= 0 && bounds.x + bounds.width <= width && bounds.y + bounds.height <= height && !overflow, 'unreachable action at ' + width + 'x' + height)
      const editorHeight = await page.getByLabel('拦截请求编辑区').evaluate(element => element.clientHeight)
      assert(editorHeight >= 80, 'body editor squeezed at ' + width + 'x' + height + ': ' + editorHeight)
      layouts.push({ width, height, queued_requests: 3, release_reachable: true, editor_height: editorHeight, horizontal_overflow: false })
      if (width === 390) await page.screenshot({ path: 'output/playwright/interception/mobile-editor.png', fullPage: true })
      if (width === 844) await page.screenshot({ path: 'output/playwright/interception/landscape-queue.png', fullPage: true })
    }
    await page.setViewportSize({ width: 1366, height: 900 })
  } finally {
    const queue = (await json(api + '/api/interceptions')).requests
    for (const item of queue.filter(item => item.model.startsWith('qa-layout-'))) {
      await page.request.post(api + '/api/interceptions/' + item.trace_id + '/cancel', { data: { revision: item.revision } })
    }
    await Promise.all(flights)
    await page.request.put(api + '/api/rules/' + rule.id, { data: rule })
  }
  return { status: 'PASS', layouts }
}
