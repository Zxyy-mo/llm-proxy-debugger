async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const api = 'http://127.0.0.1:12338'
  const rules = async () => (await page.request.get(api + '/api/rules')).json()
  const layouts = []
  await page.reload()
  await page.getByRole('tab', { name: 'Dynamic Rules 2', exact: true }).click()
  await page.getByRole('button', { name: '编辑规则 /responses', exact: true }).click()
  await page.getByRole('spinbutton', { name: '等待时间（秒）', exact: true }).fill('7')
  await page.getByRole('combobox', { name: '等待结束后', exact: true }).selectOption('cancel')
  await page.getByRole('button', { name: '保存规则', exact: true }).click()
  await page.getByRole('button', { name: '编辑规则 /responses', exact: true }).waitFor()
  let current
  for (let i = 0; i < 40; i++) {
    current = (await rules()).find(item => item.path_match === '/responses')
    if (current.wait_seconds === 7 && current.timeout_action === 'cancel') break
    await page.waitForTimeout(25)
  }
  assert(current.wait_seconds === 7 && current.timeout_action === 'cancel', 'rule editor did not save policy')
  const before = current.disabled
  await page.getByRole('switch', { name: '启用规则 /responses', exact: true }).click()
  for (let i = 0; i < 40; i++) {
    current = (await rules()).find(item => item.path_match === '/responses')
    if (current.disabled !== before) break
    await page.waitForTimeout(25)
  }
  assert(current.disabled !== before, 'rule enable switch did not persist')
  await page.getByRole('button', { name: '编辑规则 /responses', exact: true }).click()
  for (const [width, height] of [[1600,1000], [1366,768], [1024,768], [768,600], [390,844], [320,640], [844,390]]) {
    await page.setViewportSize({ width, height })
    const save = page.getByRole('button', { name: '保存规则', exact: true })
    await save.click({ trial: true, timeout: 2500 })
    const bounds = await save.boundingBox()
    assert(bounds && bounds.x >= 0 && bounds.x + bounds.width <= width && bounds.y + bounds.height <= height, 'rule Save clipped at ' + width + 'x' + height)
    assert(!await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), 'page overflows horizontally')
    layouts.push({ width, height, save_reachable: true })
    if (width === 390) await page.screenshot({ path: 'output/playwright/interception/mobile-rules.png', fullPage: true })
  }
  await page.setViewportSize({ width: 1366, height: 900 })
  await page.getByRole('button', { name: '收起', exact: true }).click()
  await page.getByRole('button', { name: '删除规则 /responses', exact: true }).click()
  for (let i = 0; i < 40; i++) {
    if (!(await rules()).some(item => item.path_match === '/responses')) break
    await page.waitForTimeout(25)
  }
  assert(!(await rules()).some(item => item.path_match === '/responses'), 'rule delete did not persist')
  return { status: 'PASS', edit_saved: true, enabled_changed: true, delete_saved: true, layouts }
}
