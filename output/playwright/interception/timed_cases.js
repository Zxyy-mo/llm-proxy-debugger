async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const api = 'http://127.0.0.1:12338'
  const json = async url => (await page.request.get(url)).json()
  const rules = await json(api + '/api/rules')
  let rule = rules.find(item => item.path_match === '/responses')
  const results = []
  const errors = []
  const onError = error => errors.push(String(error))
  page.on('pageerror', onError)
  async function configure(patch) {
    const response = await page.request.put(api + '/api/rules/' + rule.id, { data: { ...rule, ...patch } })
    assert(response.status() === 200, 'rule configuration failed')
    rule = await response.json()
  }
  async function waitPending(model) {
    for (let i = 0; i < 100; i++) {
      const listing = await json(api + '/api/interceptions')
      const found = listing.requests.find(item => item.model === model)
      if (found) return found
      await page.waitForTimeout(25)
    }
    throw new Error('pending request not found: ' + model)
  }
  async function select(model) {
    await page.getByRole('button', { name: /^待处理/ }).click()
    const button = page.getByLabel('待处理请求列表').getByRole('button', { name: new RegExp('^' + model + ' ') })
    await button.click()
    await page.getByRole('textbox', { name: '本次请求正文', exact: true }).waitFor()
  }
  const request = (model, client = page.request) => client.post(api + '/v1/responses', {
    data: { model, input: model, metadata: { qa_case: model } },
    headers: { Authorization: 'Bearer qa-auth-original' }, timeout: 120000,
  })
  async function logFor(trace) {
    for (let i = 0; i < 100; i++) {
      const sessions = await json(api + '/api/sessions')
      const log = Object.values(sessions).flatMap(session => session.logs).find(item => item.trace_id === trace)
      if (log && log.status !== 'pending' && log.status !== 'running') return log
      await page.waitForTimeout(25)
    }
    throw new Error('terminal log not found')
  }
  let hitCount = (await json('http://127.0.0.1:28001/__requests')).length
  for (const policy of ['forward', 'cancel']) {
    await configure({ disabled: false, wait_seconds: 4, timeout_action: policy })
    const model = 'qa-timeout-' + policy
    const pendingResponse = request(model)
    const pending = await waitPending(model)
    await select(model)
    const response = await pendingResponse
    assert(response.status() === (policy === 'forward' ? 200 : 504), 'wrong timeout response')
    await page.getByText(policy === 'forward' ? '已到期转发' : '已到期取消', { exact: true }).waitFor()
    const log = await logFor(pending.trace_id)
    assert(log.interception.reason === 'timeout' && log.wait_duration_ms >= 3900, 'timeout accounting incorrect')
    if (policy === 'forward') hitCount++
    assert((await json('http://127.0.0.1:28001/__requests')).length === hitCount, 'timeout cancellation reached upstream')
    results.push({ case: model, status: response.status(), state: log.status, wait_ms: log.wait_duration_ms })
  }
  await configure({ wait_seconds: 60, timeout_action: 'forward' })
  const manualResponse = request('qa-manual-cancel')
  const manual = await waitPending('qa-manual-cancel')
  await select('qa-manual-cancel')
  await page.getByRole('button', { name: '取消请求', exact: true }).click()
  assert((await manualResponse).status() === 409, 'manual cancel did not return 409')
  assert((await logFor(manual.trace_id)).status === 'canceled', 'manual cancellation missing from log')
  assert(!(await json(api + '/api/requests/' + manual.trace_id)).outgoing, 'manual cancel has an outgoing capture')
  results.push({ case: 'manual-cancel', status: 409, outgoing: false })

  const conflictResponse = request('qa-conflict')
  const conflict = await waitPending('qa-conflict')
  await select('qa-conflict')
  const draft = JSON.stringify({ model: 'qa-conflict', input: 'local unsaved draft' }, null, 2)
  await page.getByRole('textbox', { name: '本次请求正文', exact: true }).fill(draft)
  const remoteBody = JSON.stringify({ model: 'qa-conflict', input: 'saved from another tab' })
  const remote = await page.request.patch(api + '/api/interceptions/' + conflict.trace_id, { data: { revision: 1, body: remoteBody } })
  assert(remote.status() === 200, 'remote edit failed')
  const reload = page.getByRole('button', { name: '加载最新版本（覆盖草稿）', exact: true })
  await reload.waitFor()
  assert(await page.getByRole('textbox', { name: '本次请求正文', exact: true }).inputValue() === draft, 'conflict destroyed local draft')
  assert(await page.getByRole('button', { name: '保存并放行', exact: true }).isDisabled(), 'stale draft remained releasable')
  await page.screenshot({ path: 'output/playwright/interception/revision-conflict.png', fullPage: true })
  await reload.click()
  await page.waitForFunction(() => {
    const field = document.querySelector('#intercept-body')
    if (!field) return false
    try { return JSON.parse(field.value).input === 'saved from another tab' }
    catch { return false }
  })
  assert(JSON.parse(await page.getByRole('textbox', { name: '本次请求正文', exact: true }).inputValue()).input === 'saved from another tab', 'latest version did not load')
  await page.getByRole('button', { name: '取消请求', exact: true }).click()
  assert((await conflictResponse).status() === 409, 'conflict cleanup failed')
  results.push({ case: 'revision-conflict', local_draft_preserved: true, stale_release_disabled: true })

  const client = await page.context().browser().newContext()
  const disconnectResponse = request('qa-disconnect', client.request).then(response => response.status()).catch(() => 'disconnected')
  const disconnected = await waitPending('qa-disconnect')
  await select('qa-disconnect')
  await client.close()
  await disconnectResponse
  const canceled = await logFor(disconnected.trace_id)
  assert(canceled.status_code === 499 && canceled.interception.reason === 'client_disconnected', 'real client disconnect did not cancel the request')
  assert(!(await json(api + '/api/requests/' + disconnected.trace_id)).outgoing, 'disconnected request was forwarded')
  await page.getByText('客户端已断开，请求已取消', { exact: true }).waitFor()
  results.push({ case: 'client-disconnect', status: 499, outgoing: false })

  const existingResponse = request('qa-rule-snapshot')
  const existing = await waitPending('qa-rule-snapshot')
  await configure({ disabled: true })
  const unblocked = await request('qa-disabled-rule')
  assert(unblocked.status() === 200, 'disabled rule still intercepted future traffic')
  hitCount++
  assert((await json(api + '/api/interceptions/' + existing.trace_id)).request.state === 'pending', 'rule change altered an existing pending request')
  await select('qa-rule-snapshot')
  await page.getByRole('button', { name: '取消请求', exact: true }).click()
  await existingResponse
  assert((await json('http://127.0.0.1:28001/__requests')).length === hitCount, 'a canceled request leaked to upstream')
  assert((await json(api + '/api/interceptions')).requests.length === 0, 'pending queue did not drain')
  results.push({ case: 'rule-disabled', future_request_forwarded: true, pending_policy_unchanged: true })
  await page.getByRole('button', { name: '调用画布', exact: true }).click()
  await page.getByRole('combobox', { name: '调用图范围' }).selectOption('all')
  await page.waitForTimeout(400)
  await page.screenshot({ path: 'output/playwright/interception/final-graph.png', fullPage: true })
  page.off('pageerror', onError)
  assert(errors.length === 0, 'application errors: ' + errors.join('; '))
  return { status: 'PASS', upstream_requests: hitCount, page_errors: errors, results }
}
