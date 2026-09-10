async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const json = async (url) => (await page.request.get(url)).json()
  const hits = await json('http://127.0.0.1:28001/__requests')
  assert(hits.length === 1 && hits[0].auth_preserved && hits[0].accept === 'application/json', 'single release or header preservation failed')
  const payload = JSON.parse(hits[0].body)
  assert(payload.input === 'Edited in the browser', 'wrong saved candidate forwarded')
  assert(hits[0].content_length === await page.evaluate(text => new TextEncoder().encode(text).length, hits[0].body), 'outgoing length mismatch')
  const sessions = await json('http://127.0.0.1:12338/api/sessions')
  const log = Object.values(sessions).flatMap(session => session.logs)[0]
  assert(log.status === 'done' && log.wait_duration_ms > 0 && log.upstream_duration_ms > 0, 'final state or timing missing')
  const capture = await json('http://127.0.0.1:12338/api/requests/' + log.trace_id)
  assert(capture.original.body.includes('TAIL_MARKER') && capture.outgoing.body === hits[0].body, 'full capture not retained')
  assert(!JSON.stringify(capture).includes('qa-auth-original'), 'authorization exposed')
  await page.getByRole('button', { name: '原始 / 出站', exact: true }).click()
  await page.getByRole('heading', { name: '原始 / 出站请求', exact: true }).waitFor()
  await page.screenshot({ path: 'output/playwright/interception/desktop-audit.png', fullPage: true })
  console.log(JSON.stringify({ status: 'PASS', trace: log.trace_id, forwarded_count: hits.length, original_bytes: capture.original.body.length, display_limit: 128, credentials_preserved: true, outgoing_length_correct: true, wait_ms: log.wait_duration_ms, upstream_ms: log.upstream_duration_ms }))
}
