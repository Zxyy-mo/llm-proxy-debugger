// 在独立的 compatible-provider-check Playwright session 中运行；只试点击控件，不保存配置或调用模型。
async (page) => {
  if (!page.url().startsWith('http://127.0.0.1:12349/')) throw new Error('需要独立的 Provider 验收服务')
  const checks = []
  for (const [width, height] of [[1600, 1000], [1366, 768], [1024, 768], [768, 600], [390, 844], [320, 640], [844, 390]]) {
    await page.setViewportSize({ width, height })
    const controls = [
      ['combobox', '新增接入类型'],
      ['combobox', 'Chat Completions 能力声明'],
      ['button', '保存 Provider 与路由'],
      ['button', '查询模型列表'],
      ['listbox', '发现的模型'],
      ['button', '填入所选路由'],
    ]
    for (const [role, name] of controls) {
      const control = page.getByRole(role, { name, exact: true })
      await control.scrollIntoViewIfNeeded()
      await control.click({ trial: true })
      const hit = await control.evaluate(element => {
        const box = element.getBoundingClientRect()
        const x = box.x + box.width / 2
        const y = box.y + box.height / 2
        const top = document.elementFromPoint(x, y)
        return { x, y, width: box.width, height: box.height, reachable: x >= 0 && x < innerWidth && y >= 0 && y < innerHeight && (top === element || element.contains(top)) }
      })
      if (!hit.reachable) throw new Error(`${width}×${height}: ${name} 不可点击`)
      checks.push({ viewport: `${width}×${height}`, name, hit })
    }
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth > innerWidth + 1)
    if (overflow) throw new Error(`${width}×${height}: 页面水平溢出`)
    if (width === 320 || width === 844 || width === 1600) {
      await page.screenshot({ path: `output/playwright/compatible-provider-setup/provider-${width}x${height}.png`, fullPage: true })
    }
  }
  return { checked: checks.length, all_reachable: true, page_horizontal_overflow: false, checks }
}
