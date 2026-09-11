async (page) => {
  const origin = 'http://127.0.0.1:12341'
  if (!page.url().startsWith(origin + '/')) throw new Error('Isolated operator gateway required')
  const checks = []
  const check = (name, value) => { if (!value) throw new Error(name); checks.push({ name, passed: true }) }
  await page.setViewportSize({ width: 1600, height: 1000 })
  const stored = await page.evaluate(() => JSON.parse(localStorage.getItem('llm-debugger.selection') || '{}'))
  await page.getByRole('button', { name: '设置', exact: true }).focus()
  await page.keyboard.press('Enter')
  await page.getByRole('button', { name: 'Provider 与路由', exact: true }).waitFor()
  check('settings action works with keyboard activation', await page.getByRole('button', { name: 'Provider 与路由', exact: true }).isVisible())
  await page.getByRole('button', { name: '调用画布', exact: true }).focus()
  await page.keyboard.press('Enter')
  const finder = page.getByRole('textbox', { name: '查找请求', exact: true })
  await finder.fill('no-such-request-in-this-synthetic-run')
  const list = page.getByLabel('请求列表', { exact: true })
  check('no-match state is explicit', (await list.innerText()).includes('没有匹配的请求'))
  const after = await page.evaluate(() => JSON.parse(localStorage.getItem('llm-debugger.selection') || '{}'))
  check('no-match filtering preserves the selected trace', Boolean(stored.trace) && stored.trace === after.trace)
  await finder.fill('')
  await page.getByRole('combobox', { name: '请求状态', exact: true }).selectOption('')
  check('clearing filters restores request choices', await list.getByRole('button').count() > 0)
  return { checks, trace: after.trace }
}
