async (page) => {
  const checks = []
  const check = (name, passed) => { if (!passed) throw new Error(name); checks.push({ name, passed: true }) }
  const until = async (test, name) => {
    const deadline = Date.now() + 10000
    while (Date.now() < deadline) { if (await test()) return; await page.waitForTimeout(50) }
    throw new Error('等待超时：' + name)
  }
  const origin = page.url().split('/').slice(0, 3).join('/')
  const api = async path => (await page.request.get(origin + path)).json()
  const settings = await api('/api/providers')
  const upstream = settings.default_target.split('/').slice(0, 3).join('/')
  const count = async () => (await (await page.request.get(upstream + '/state')).json()).count
  const source = (await api('/api/history?session_id=qa-four-layers&limit=200')).items.find(log => log.summary === '分支：核对配置')
  const before = await count()
  // 仅在独立验收浏览器中构造“从未打开工作台”的已保存选择，验证 null 不被重载改写。
  await page.evaluate(() => {
    const saved = JSON.parse(localStorage.getItem('llm-debugger.selection') || '{}')
    saved.auditTrace = null
    localStorage.setItem('llm-debugger.selection', JSON.stringify(saved))
  })
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.reload()
  await page.getByRole('region', { name: '任务归属' }).waitFor()
  check('未开启工作台的空来源在刷新后仍为空', await page.getByRole('button', { name: /^返回重放工作台/ }).count() === 0)
  await page.getByRole('button', { name: '编辑并重放', exact: true }).click()
  const bench = page.getByRole('region', { name: '请求重放工作台' })
  const editor = bench.getByRole('textbox', { name: '重放正文', exact: true })
  await editor.waitFor()
  check('进入重放后保留捕获正文的大整数', (await editor.inputValue()).includes('9007199254740993'))
  check('工作台不占用任务筛选栏空间', await page.getByRole('region', { name: '任务筛选' }).count() === 0)
  const credential = bench.getByLabel(/请求头 Authorization/)
  await credential.fill('synthetic-layers-credential')
  const send = bench.getByRole('button', { name: '原样重放', exact: true })
  await until(() => send.isEnabled(), '原样重放可发送')
  for (const [width, height] of [[320, 640], [844, 390]]) {
    await page.setViewportSize({ width, height })
    await editor.scrollIntoViewIfNeeded()
    const box = await editor.boundingBox()
    check(`${width}×${height} 重放正文保持可用高度`, box && box.height >= 120)
    await send.scrollIntoViewIfNeeded()
    await send.click({ trial: true })
  }
  await page.setViewportSize({ width: 1440, height: 900 })
  await send.click()
  await until(async () => (await bench.innerText()).includes('本次重放：已完成'), '新重放结束')
  const replays = (await api('/api/replays')).filter(record => record.replay_of === source.trace_id).sort((a, b) => b.created_at.localeCompare(a.created_at))
  const replay = replays[0]
  const result = await api('/api/history/' + replay.trace_id)
  check('一次重放仅增加一次已固定备用上游的请求', await count() === before + 1 && result.route.attempts.length === 1)
  check('精确重放创建独立任务并保留来源', result.run_id !== source.run_id && result.run.state === 'replay' && result.replay.of === source.trace_id)
  const originalCapture = await api('/api/requests/' + source.trace_id)
  const replayCapture = await api('/api/requests/' + replay.trace_id)
  check('新任务没有修改重放正文', originalCapture.outgoing.body === replayCapture.original.body)
  await bench.getByRole('button', { name: '查看新调用', exact: true }).click()
  await page.getByRole('region', { name: '任务归属' }).waitFor()
  check('结果详情显示重放任务来源说明', (await page.getByRole('region', { name: '任务归属' }).innerText()).includes('这次显式重放创建了独立任务'))
  await page.screenshot({ path: 'output/playwright/call-layers/replay-new-run.png' })
  await page.reload()
  await page.getByRole('region', { name: '任务归属' }).waitFor()
  check('刷新结果不会再次执行', await count() === before + 1)
  check('临时重放凭证未写入浏览器存储', !(await page.evaluate(() => JSON.stringify(Object.entries(localStorage)) + JSON.stringify(Object.entries(sessionStorage)))).includes('synthetic-layers-credential'))
  return { checks, source_trace: source.trace_id, replay_trace: replay.trace_id, replay_run: result.run_id }
}
