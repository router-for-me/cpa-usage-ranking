import { expect, test } from '@playwright/test'
import { seedReportingSeat } from './database-fixtures'

test('shows real submitted data across approved boards and periods', async ({ page, request }, info) => {
  const { headers } = seedReportingSeat('真实测试参与者', 2)
  const day = new Date().toISOString().slice(0, 10)
  const report = await request.put(`/v1/clients/report/${day}`, { headers, data: {
    schema_version: 1, revision: '1', items: [{ model: 'claude-fable-5', total_tokens: '9007199254740993', input_tokens: '10', output_tokens: '0', cache_read_tokens: '5', cache_write_tokens: '0', reasoning_tokens: '0', request_count: '2', success_count: '1', ttft_avg_ms: 12, ttft_samples: '1', latency_avg_ms: null, latency_samples: '0' }],
  } })
  expect(report.status()).toBe(200)
  await expect.poll(async () => {
    const response = await request.get('/v1/leaderboards/usage/today')
    if (response.status() !== 200) return false
    const value = await response.json() as { entries: { display_name: string; metric: string }[] }
    return value.entries.some((entry) => entry.display_name === '真实测试参与者' && entry.metric === '9007199254740993')
  }).toBe(true)
  await page.route('**/avatars/images/002.webp', (route) => route.fulfill({ status: 404, body: '' }), { times: 1 })
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Today · Overall' })).toBeVisible()
  await expect(page.getByText('真实测试参与者')).toBeVisible()
  await expect(page.getByRole('row', { name: /真实测试参与者/ }).locator('.avatar img')).toHaveAttribute('src', '/avatars/images/001.webp')
  const row = page.getByRole('row', { name: /真实测试参与者/ })
  await expect(row.locator('.detail-cell')).toHaveText(['9007199.25 B', '2', '50.00 %', '50.00 %', '12 ms', '—'])
  await expect(row.getByRole('button')).toHaveCount(0)
  await expect(page.getByRole('button', { name: 'Today' })).toHaveAttribute('aria-pressed', 'true')
  await page.getByRole('button', { name: 'Token Usage' }).click()
  await expect(page.getByRole('heading', { name: 'Today · Token Usage' })).toBeVisible()
  await expect(page.getByText('9007199.25')).toBeVisible()
  await page.screenshot({ path: info.outputPath('real-desktop.png'), fullPage: true })
  await page.getByRole('button', { name: 'Cache Reuse' }).click()
  await expect(page.getByRole('columnheader', { name: 'Cache Hit Rate' })).toBeVisible()
  await expect(page.getByText('50.00')).toBeVisible()
  await page.getByRole('button', { name: 'Reliability' }).click()
  await expect(page.getByRole('columnheader', { name: 'Reliability Score' })).toBeVisible()
  await page.getByRole('button', { name: 'First-Token Latency' }).click()
  await expect(page.getByRole('columnheader', { name: 'Avg TTFT' })).toBeVisible()
  await expect(page.locator('.metric-value').filter({ hasText: '12 ms' })).toBeVisible()
  await page.getByRole('button', { name: 'Yesterday' }).click()
  await expect(page.getByRole('heading', { name: 'Yesterday · First-Token Latency' })).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Waiting for first usage report' })).toBeVisible()
  for (const period of ['Weekly', 'Monthly']) {
    await page.getByRole('button', { name: period }).click()
    await expect(page.getByRole('heading', { name: `${period} · First-Token Latency` })).toBeVisible()
    // This test submitted only today; complete previous periods must exclude it.
    await expect(page.getByText('真实测试参与者')).toHaveCount(0)
  }
  await page.getByRole('button', { name: 'Today' }).click()
  await expect(page.getByRole('heading', { name: 'Today · First-Token Latency' })).toBeVisible()
  await expect(page.getByText('真实测试参与者')).toBeVisible()
  await expect(page.getByRole('button', { name: /Annual|All-time|年度|累计/ })).toHaveCount(0)
  await expect(page.getByRole('textbox')).toHaveCount(0)
  await expect(page.getByRole('combobox')).toHaveCount(0)
  await page.setViewportSize({ width: 375, height: 812 })
  await page.emulateMedia({ reducedMotion: 'reduce' })
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.getByRole('button', { name: 'Token Usage' }).click()
  await expect(page.getByText('9007199.25')).toBeVisible()
  await page.screenshot({ path: info.outputPath('real-mobile.png'), fullPage: true })
})

test('retains correctly labeled old data on races and retries', async ({ page, request }) => {
  await expect.poll(async () => (await request.get('/readyz')).status()).toBe(200)
  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Today · Overall' })).toBeVisible()
  let unblock!: () => void
  let intercepted!: () => void
  const blocked = new Promise<void>((resolve) => { unblock = resolve })
  const started = new Promise<void>((resolve) => { intercepted = resolve })
  await page.route('**/v1/leaderboards/usage/today', async (route) => {
    intercepted()
    await blocked
    await route.continue().catch(() => {})
  }, { times: 1 })
  await page.getByRole('button', { name: 'Token Usage' }).click()
  await started
  await expect(page.getByText(/Loading/)).toHaveCount(0)
  await expect(page.getByRole('heading', { name: 'Today · Overall', exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Overall' })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.getByRole('heading', { name: /Today · Token Usage/ })).toHaveCount(0)
  await page.getByRole('button', { name: 'Reliability' }).click()
  await expect(page.getByRole('heading', { name: 'Today · Reliability' })).toBeVisible()
  unblock()
  await expect(page.getByRole('columnheader', { name: 'Reliability Score' })).toBeVisible()
  await page.route('**/v1/leaderboards/cache/today', (route) => route.fulfill({ status: 503, contentType: 'application/json', body: '{}' }), { times: 1 })
  await page.getByRole('button', { name: 'Cache Reuse' }).click()
  await expect(page.getByText(/Leaderboard cache is not ready yet/)).toBeVisible()
  await expect(page.getByRole('heading', { name: /Today · Reliability/ })).toContainText('Previous result')
  await page.getByRole('button', { name: 'Retry leaderboard' }).click()
  await expect(page.getByRole('heading', { name: 'Today · Cache Reuse' })).toBeVisible()
})
