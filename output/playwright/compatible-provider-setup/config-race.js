// 挂起真实管理 API 响应，检查重新加载与保存互斥，避免旧 GET 覆盖后续保存。
async (page) => {
  const base = 'http://127.0.0.1:12349'
  if (!page.url().startsWith(base + '/')) throw new Error('需要独立的 Provider 验收服务')
  const save = page.getByRole('button', { name: '保存 Provider 与路由', exact: true })
  const reload = page.getByRole('button', { name: '重新加载已保存配置', exact: true })
  const address = page.getByRole('textbox', { name: 'Provider Base URL', exact: true })
  const checks = []
  const check = (name, passed) => { if (!passed) throw new Error(name); checks.push({ name, passed }) }

  // 通过另一操作者修改实例，得到真实的过期模型列表与重新加载入口。
  async function showReloadButton(suffix) {
    await page.getByRole('combobox', { name: '模型查询 Provider', exact: true }).selectOption('vllm-1')
    await page.getByRole('textbox', { name: '本次模型查询密钥', exact: true }).fill('controlled-ui-key')
    const listed = page.waitForResponse(r => r.url().endsWith('/api/provider-models'))
    await page.getByRole('button', { name: '查询模型列表', exact: true }).click()
    await listed
    const current = (await (await page.request.get(base + '/api/providers')).json()).config
    current.providers[0].name += suffix
    check('外部配置修改成功' + suffix, (await page.request.put(base + '/api/providers', { data: current })).ok())
    await page.getByRole('button', { name: /^(使用模型新建路由|填入所选路由)$/ }).click()
    await reload.waitFor()
  }

  await showReloadButton('-race-read')
  let releaseRead
  let enteredRead
  const readGate = new Promise(resolve => { releaseRead = resolve })
  const readEntered = new Promise(resolve => { enteredRead = resolve })
  const readHandler = async route => {
    if (route.request().method() !== 'GET') return route.continue()
    const oldResponse = await route.fetch()
    enteredRead()
    await readGate
    await route.fulfill({ response: oldResponse })
  }
  let putsDuringRead = 0
  const countPuts = request => { if (request.url() === base + '/api/providers' && request.method() === 'PUT') putsDuringRead++ }
  page.on('request', countPuts)
  await page.route(base + '/api/providers', readHandler)
  try {
    const loaded = page.waitForResponse(r => r.url() === base + '/api/providers' && r.request().method() === 'GET')
    await reload.click()
    await readEntered
    check('旧 GET 未返回时禁止编辑地址', await address.isDisabled())
    check('旧 GET 未返回时禁止保存', await save.isDisabled())
    check('旧 GET 未返回时显示加载状态', await page.getByText('正在加载已保存配置…', { exact: true }).isVisible())
    await save.evaluate(button => button.form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })))
    await page.waitForTimeout(50)
    check('保存 handler 拒绝读取期间的程序化 submit', putsDuringRead === 0)
    releaseRead()
    await loaded
    await page.waitForFunction(() => !document.querySelector('[aria-label="Provider Base URL"]').matches(':disabled'))
    check('GET 结束后恢复编辑', await address.isEnabled())
  } finally {
    releaseRead()
    await page.unroute(base + '/api/providers', readHandler)
    page.off('request', countPuts)
  }

  await showReloadButton('-race-save')
  await page.getByRole('textbox', { name: '实际模型名', exact: true }).fill('controlled-model-247')
  let releaseSave
  let enteredSave
  const saveGate = new Promise(resolve => { releaseSave = resolve })
  const saveEntered = new Promise(resolve => { enteredSave = resolve })
  const saveHandler = async route => {
    if (route.request().method() !== 'PUT') return route.continue()
    const response = await route.fetch()
    enteredSave()
    await saveGate
    await route.fulfill({ response })
  }
  let getsDuringSave = 0
  const countGets = request => { if (request.url() === base + '/api/providers' && request.method() === 'GET') getsDuringSave++ }
  page.on('request', countGets)
  await page.route(base + '/api/providers', saveHandler)
  try {
    const saved = page.waitForResponse(r => r.url() === base + '/api/providers' && r.request().method() === 'PUT')
    await save.click()
    await saveEntered
    check('PUT 尚未确认时禁止重新加载', await reload.isDisabled())
    check('PUT 尚未确认时禁止草稿编辑', await address.isDisabled())
    await reload.dispatchEvent('click')
    await page.waitForTimeout(50)
    check('load handler 拒绝保存期间的程序化 click', getsDuringSave === 0)
    releaseSave()
    await saved
    await page.waitForFunction(() => !document.querySelector('[aria-label="Provider Base URL"]').matches(':disabled'))
    const current = (await (await page.request.get(base + '/api/providers')).json()).config
    check('保存后服务端保留新路由', current.routes[0].target_model === 'controlled-model-247')
    check('保存后界面与服务端一致', await page.getByRole('textbox', { name: '实际模型名', exact: true }).inputValue() === current.routes[0].target_model)
  } finally {
    releaseSave()
    await page.unroute(base + '/api/providers', saveHandler)
    page.off('request', countGets)
  }
  return { passed: checks.length, checks }
}
