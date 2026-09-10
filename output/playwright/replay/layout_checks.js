async (page) => {
  const assert = (condition, message) => { if (!condition) throw new Error(message) }
  const errors = []
  page.on('pageerror', error => errors.push(String(error)))
  const layouts = []
  const inViewport = async (locator, width, height) => {
    await locator.scrollIntoViewIfNeeded()
    await locator.click({ trial: true, timeout: 2500 })
    const box = await locator.boundingBox()
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)
    return Boolean(box && box.x >= 0 && box.x + box.width <= width + 1 && box.y >= 0 && box.y + box.height <= height + 1 && !overflow)
  }
  await page.goto('http://localhost:5174/')
  await page.getByRole('heading', { name: '原始 / 出站请求', exact: true }).waitFor()
  for (const [width, height] of [[1600, 1000], [1366, 768], [1024, 768], [768, 600], [390, 844], [320, 640], [844, 390]]) {
    await page.setViewportSize({ width, height })
    await page.getByRole('tab', { name: '复制 cURL' }).click()
    const copy = page.getByRole('region', { name: 'cURL 导出' }).getByRole('button', { name: '复制命令' })
    await copy.waitFor()
    const copyOk = await inViewport(copy, width, height)
    await page.getByRole('tab', { name: '重放' }).click()
    const bench = page.getByRole('region', { name: '请求重放工作台' })
    await bench.getByLabel('请求头 Authorization', { exact: false }).fill('layout-check')
    await bench.getByLabel('查询参数 key', { exact: false }).fill('layout-check')
    const run = bench.getByRole('button', { name: /原样重放|发送修改后的请求/ })
    await run.waitFor()
    const runOk = await inViewport(run, width, height)
    const validate = bench.getByRole('button', { name: '校验', exact: true })
    const validateOk = await inViewport(validate, width, height)
    const credential = bench.getByLabel('请求头 Authorization', { exact: false })
    const credentialOk = await inViewport(credential, width, height)
    const editor = bench.getByRole('textbox', { name: '重放正文', exact: true })
    await editor.scrollIntoViewIfNeeded()
    const editorBox = await editor.boundingBox()
    const editorVisible = editorBox ? Math.min(editorBox.y + editorBox.height, height) - Math.max(editorBox.y, 0) : 0
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)
    layouts.push({ width, height, copy_reachable: copyOk, run_reachable: runOk, validate_reachable: validateOk, credential_reachable: credentialOk, editor_visible_height: Math.round(editorVisible), horizontal_overflow: overflow })
    assert(copyOk && runOk && validateOk && credentialOk && !overflow && editorVisible >= 96, 'layout failed at ' + width + 'x' + height + ': ' + JSON.stringify(layouts[layouts.length - 1]))
    if (width === 390) await page.screenshot({ path: 'output/playwright/replay/mobile-replay.png', fullPage: true })
    if (width === 844) await page.screenshot({ path: 'output/playwright/replay/landscape-replay.png', fullPage: false })
  }
  await page.setViewportSize({ width: 1366, height: 900 })
  return JSON.stringify({ status: 'PASS', layouts, page_errors: errors })
}
