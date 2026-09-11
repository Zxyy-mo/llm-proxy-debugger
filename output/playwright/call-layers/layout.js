async (page) => {
  const checks = []
  const viewports = [[320, 568], [320, 640], [390, 844], [844, 390], [768, 1024], [1024, 768], [1440, 900]]
  const check = (name, passed, detail) => { if (!passed) throw new Error(name + ': ' + JSON.stringify(detail)); checks.push({ name, passed: true }) }
  const usable = async (locator, name, minimumWidth = 24) => {
    await locator.scrollIntoViewIfNeeded()
    await locator.click({ trial: true })
    const result = await locator.evaluate(el => {
      const box = el.getBoundingClientRect()
      const top = document.elementFromPoint(box.left + box.width / 2, box.top + box.height / 2)
      return { width: box.width, height: box.height, x: box.left, y: box.top, hit: Boolean(top && (top === el || el.contains(top))), viewport: [innerWidth, innerHeight] }
    })
    check(name, result.width >= minimumWidth && result.height >= 14 && result.hit && result.y >= 0 && result.y + result.height <= result.viewport[1] + 1, result)
  }
  const details = page.getByRole('region', { name: '响应与调用详情' })
  const route = details.locator('details[aria-label="路由与传输"]')
  const attempts = route.locator('li[aria-label="上游尝试"]')
  if (!(await route.evaluate(el => el.open))) await route.locator('summary').first().click()
  const open = attempts.nth(0).getByRole('button', { name: '查看此次出站' })
  if (await open.count()) await open.click()
  await attempts.nth(0).getByLabel('此次出站正文').waitFor()
  for (const [width, height] of viewports) {
    await page.setViewportSize({ width, height })
    await page.waitForTimeout(150)
    await usable(page.getByRole('combobox', { name: '选择任务' }), `${width}×${height} 任务选择`, 100)
    await usable(page.getByRole('button', { name: '响应与详情', exact: true }), `${width}×${height} 详情导航`, 70)
    await usable(route.locator('summary').first(), `${width}×${height} 尝试展开`, 140)
    await usable(attempts.nth(0).getByRole('button', { name: '收起出站内容' }), `${width}×${height} 出站内容切换`, 80)
    await usable(attempts.nth(0).getByRole('link', { name: '下载此次出站正文' }), `${width}×${height} 正文下载`, 90)
    const content = await details.boundingBox()
    check(`${width}×${height} 正文区域保持可读高度`, content && content.height >= 96, content)
    const overflow = await page.evaluate(() => ({ document: document.documentElement.scrollWidth > innerWidth + 1, details: [...document.querySelectorAll('[aria-label="响应与调用详情"], [aria-label="此次尝试的出站内容"]')].filter(el => el.getBoundingClientRect().height > 0).some(el => el.scrollWidth > el.clientWidth + 1) }))
    check(`${width}×${height} 无横向溢出`, !overflow.document && !overflow.details, overflow)
    if (width === 320 && height === 640) await page.screenshot({ path: 'output/playwright/call-layers/mobile-attempts.png' })
    if (width === 844) await page.screenshot({ path: 'output/playwright/call-layers/landscape-attempts.png' })
  }
  return { checks, viewports }
}
