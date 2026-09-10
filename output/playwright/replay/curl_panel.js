async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const api = 'http://127.0.0.1:12338'
  const json = async url => (await page.request.get(url)).json()
  const errors = []
  page.on('pageerror', error => errors.push(String(error)))
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'])
  const sessions = await json(api + '/api/sessions')
  const logs = Object.values(sessions).flatMap(session => session.logs)
  const source = logs.find(log => log.summary && log.summary.includes('tail'))
  assert(source, 'seed request not found')

  // Start from a fresh mount so defaults (compare tab, outgoing source) are exercised.
  await page.goto('http://localhost:5174/')
  await page.getByLabel('会话列表').getByRole('button').first().waitFor()
  await page.getByLabel('会话列表').getByRole('button').filter({ hasText: source.session_id }).first().click()
  await page.getByLabel('请求列表').getByRole('button').filter({ hasText: /tail/ }).first().click()
  await page.getByRole('button', { name: '原始 / 出站', exact: true }).click()
  await page.getByRole('heading', { name: '原始 / 出站请求', exact: true }).waitFor()
  await page.getByRole('tab', { name: '对比' }).click()
  await page.getByRole('region', { name: '请求差异' }).waitFor()
  const compareText = await page.getByLabel('原始与出站请求').innerText()
  assert(compareText.includes('携带凭证') && compareText.includes('Authorization (Bearer)') && compareText.includes('?key='), 'credential names not listed in the compare view')
  assert(!compareText.includes('qa-auth-original') && !compareText.includes('qa-query-secret'), 'compare view leaked a secret')
  assert(compareText.includes('key=****'), 'display URL did not mask the query secret')

  // cURL export, outgoing source by default.
  await page.getByRole('tab', { name: '复制 cURL' }).click()
  const command = page.getByRole('region', { name: 'cURL 导出' })
  await command.getByRole('button', { name: '复制命令' }).waitFor()
  let text = await command.innerText()
  assert(text.includes('上游 · http://127.0.0.1:28001/v1/responses?trace=qa'), 'outgoing destination must be the upstream: ' + text.slice(0, 300))
  assert(text.includes('${REPLAY_AUTHORIZATION}') && text.includes('${REPLAY_QUERY_KEY}') && text.includes('QA_INJECTED'), 'outgoing command missing placeholders or injected content')
  assert(!text.includes('qa-auth-original') && !text.includes('qa-query-secret'), 'cURL panel leaked a secret')
  assert(!text.includes('5174'), 'command must not target the Vite origin')
  await command.getByRole('button', { name: '复制命令' }).click()
  await page.getByRole('status').filter({ hasText: '已复制命令' }).waitFor()
  const clipboard = await page.evaluate(() => navigator.clipboard.readText())
  const exported = await json(api + '/api/requests/' + source.trace_id + '/curl?source=outgoing')
  assert(clipboard === exported.command, 'clipboard content differs from the API command')

  // Switching the source targets the gateway and drops the injected system.
  await page.getByLabel('来源').selectOption('original')
  await command.getByText('网关 · http://127.0.0.1:12338/v1/responses?trace=qa').waitFor()
  text = await command.innerText()
  assert(!text.includes('QA_INJECTED') && text.includes('${REPLAY_AUTHORIZATION}'), 'original command must not include injected content')
  const [download] = await Promise.all([page.waitForEvent('download'), command.getByRole('link', { name: '下载正文文件' }).click()])
  const chunks = []
  for await (const chunk of await download.createReadStream()) chunks.push(chunk)
  let length = 0
  for (const chunk of chunks) length += chunk.length
  const bytes = new Uint8Array(length)
  let offset = 0
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length }
  const capture = await json(api + '/api/requests/' + source.trace_id)
  const downloadedText = await page.evaluate(data => new TextDecoder().decode(new Uint8Array(data)), Array.from(bytes))
  assert(downloadedText === capture.original.body && download.suggestedFilename().startsWith('replay-'), 'downloaded body differs from the capture: ' + download.suggestedFilename())
  await page.screenshot({ path: 'output/playwright/replay/curl-panel.png', fullPage: true })
  return JSON.stringify({ trace: source.trace_id, clipboard_matches_api: true, download: download.suggestedFilename(), body_bytes: bytes.length, page_errors: errors })
}
