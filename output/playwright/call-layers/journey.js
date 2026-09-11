async (page) => {
  const origin = page.url().split('/').slice(0, 3).join('/')
  if (!origin.startsWith('http://127.0.0.1:')) throw new Error('需要独立本地验收网关')
  const checks = []
  const errors = []
  const check = (name, passed) => { if (!passed) throw new Error(name); checks.push({ name, passed: true }) }
  const until = async (test, name) => {
    const deadline = Date.now() + 10000
    while (Date.now() < deadline) { if (await test()) return; await page.waitForTimeout(50) }
    throw new Error('等待超时：' + name)
  }
  const api = async path => {
    const response = await page.request.get(origin + path)
    if (!response.ok()) throw new Error(path + ': ' + response.status())
    return response.json()
  }
  page.on('pageerror', error => errors.push(String(error)))
  const history = (await api('/api/history?session_id=qa-four-layers&limit=200')).items
  const branch = history.find(log => log.summary === '分支：核对配置')
  check('已加载独立受控会话', history.length === 7 && Boolean(branch?.run_id))
  const settings = await api('/api/providers')
  const upstream = settings.default_target.split('/').slice(0, 3).join('/')
  const count = async () => (await (await page.request.get(upstream + '/state')).json()).count
  const before = await count()
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.getByTitle('qa-four-layers', { exact: true }).click()
  await page.getByRole('button', { name: '调用画布', exact: true }).click()
  const selector = page.getByRole('combobox', { name: '选择任务', exact: true })
  const list = page.getByLabel('请求列表', { exact: true })
  await selector.selectOption(branch.run_id)
  await until(async () => await list.getByRole('button').count() === 3, '同轮分支列表')
  const graphNodes = page.getByRole('button', { name: /^调用：/ })
  await until(async () => await graphNodes.count() === 7, '完整会话画布')
  check('任务筛选保留三个分支请求及七个完整图节点', await list.getByRole('button').count() === 3 && await graphNodes.count() === 7)
  check('任务汇总显示真实六次上游发送', (await page.getByRole('region', { name: '任务筛选' }).innerText()).includes('6 次上游尝试'))
  await list.getByRole('button', { name: /分支：核对配置/ }).click()
  await page.getByRole('button', { name: '响应与详情', exact: true }).click()
  const details = page.getByRole('region', { name: '响应与调用详情' })
  check('请求详情显示任务证据', (await details.getByRole('region', { name: '任务归属' }).innerText()).includes('header:X-Run-ID'))
  const route = details.locator('details[aria-label="路由与传输"]')
  if (!(await route.evaluate(el => el.open))) await route.locator('summary').first().click()
  const attempts = route.locator('li[aria-label="上游尝试"]')
  check('两次上游结果保留各自状态', await attempts.count() === 2 && (await attempts.nth(0).innerText()).includes('HTTP 503') && (await attempts.nth(1).innerText()).includes('HTTP 200'))
  await attempts.nth(0).getByRole('button', { name: '查看此次出站' }).click()
  const first = attempts.nth(0).getByRole('region', { name: '此次尝试的出站内容' })
  await first.getByLabel('此次出站正文').waitFor()
  check('第一次出站内容没有被备用上游覆盖', (await first.innerText()).includes('/primary/v1/chat/completions') && !(await first.innerText()).includes('/backup/v1/chat/completions'))
  check('逐次快照保留大整数和实际模型名', (await first.getByLabel('此次出站正文').innerText()).includes('9007199254740993') && (await first.innerText()).includes('actual-echo'))
  const downloaded = page.waitForEvent('download')
  await first.getByRole('link', { name: '下载此次出站正文' }).click()
  const download = await downloaded
  await download.saveAs('output/playwright/call-layers/first-attempt.body.json')
  check('可通过实际链接下载独立出站正文', (await download.failure()) === null)
  await attempts.nth(1).getByRole('button', { name: '查看此次出站' }).click()
  const second = attempts.nth(1).getByRole('region', { name: '此次尝试的出站内容' })
  await second.getByLabel('此次出站正文').waitFor()
  check('切换尝试只展示选中出站', await first.count() === 0 && (await second.innerText()).includes('/backup/v1/chat/completions'))
  await second.scrollIntoViewIfNeeded()
  await page.screenshot({ path: 'output/playwright/call-layers/desktop-attempts.png' })
  await page.reload()
  await selector.waitFor()
  await until(async () => (await selector.inputValue()) === branch.run_id && (await details.innerText()).includes(branch.trace_id), '重载后的选择')
  check('刷新保留任务筛选与正在调查的请求', (await selector.inputValue()) === branch.run_id && (await details.innerText()).includes(branch.trace_id))
  await selector.selectOption('__unassociated__')
  await until(async () => await list.getByRole('button').count() === 2, '未知与冲突请求')
  check('任务筛选不会自动替换当前调查请求', (await details.innerText()).includes(branch.trace_id))
  await list.getByRole('button', { name: /任务标识冲突/ }).click()
  check('冲突请求提供明确的未关联说明', (await details.getByRole('region', { name: '任务归属' }).innerText()).includes('任务标识无效或互相冲突'))

  // 受控网络错误检验重试与过期正文隔离；不触发任何模型执行。
  await page.route('**/api/runs?**', target => target.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"受控任务查询失败"}' }))
  await page.reload()
  await page.getByRole('region', { name: '任务筛选' }).getByRole('button', { name: '重试', exact: true }).waitFor()
  check('任务查询失败保留选择并提供重试', (await selector.inputValue()) === '__unassociated__')
  await page.unroute('**/api/runs?**')
  await page.getByRole('region', { name: '任务筛选' }).getByRole('button', { name: '重试', exact: true }).click()
  await until(async () => !(await page.getByRole('region', { name: '任务筛选' }).innerText()).includes('加载失败'), '任务查询恢复')
  await selector.selectOption(branch.run_id)
  await list.getByRole('button', { name: /分支：核对配置/ }).click()
  if (!(await route.evaluate(el => el.open))) await route.locator('summary').first().click()
  const failedURL = '**/api/attempts/' + branch.trace_id + '/' + branch.route.attempts[0].id
  await page.route(failedURL, target => target.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"受控读取失败"}' }))
  await attempts.nth(0).getByRole('button', { name: '查看此次出站' }).click()
  await attempts.nth(0).getByRole('button', { name: '重试读取' }).waitFor()
  check('出站读取失败不会残留另一尝试正文', await attempts.nth(0).getByLabel('此次出站正文').count() === 0)
  await page.unroute(failedURL)
  await attempts.nth(0).getByRole('button', { name: '重试读取' }).click()
  await first.getByLabel('此次出站正文').waitFor()
  check('出站读取可以独立恢复', (await first.innerText()).includes('/primary/v1/chat/completions'))
  check('查看、筛选、下载、刷新和错误重试没有创建上游调用', await count() === before)
  check('没有浏览器运行时错误', errors.length === 0)
  return { checks, errors, upstream_count: before, trace_id: branch.trace_id, run_id: branch.run_id }
}
