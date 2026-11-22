import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { seedReportingSeat } from './database-fixtures'

const config = process.env.RANKING_TEST_ADMIN_CONFIG
const credentials: { username: string; password: string } = config ? JSON.parse(readFileSync(config, 'utf8')) : { username: '', password: '' }

test('defaults to English, toggles EN/中文 in place without reload, and persists across refreshes', async ({ page }) => {
  let documents = 0
  page.on('request', (request) => {
    if (request.isNavigationRequest() && request.resourceType() === 'document') documents++
  })
  await page.route('**/v1/leaderboards/overall/today', (route) => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      board: 'overall',
      period: 'today',
      start: '2026-11-16',
      end: '2026-11-17',
      generated_at: '2026-11-16T12:00:00Z',
      entries: [{
        rank: 1,
        client_id: '1',
        display_name: 'Bilingual Probe',
        avatar_id: '1',
        metric: '88.50',
        details: {
          total_tokens: '123456',
          request_count: '42',
          success_rate: '95.00',
          cache_read_rate: '40.00',
          ttft_mean_ms: '120.00',
          latency_mean_ms: '540.00',
        },
      }],
    }),
  }))

  await page.goto('/')
  await expect(page.locator('html')).toHaveAttribute('lang', 'en')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('CPA Usage')
  await expect(page.getByRole('heading', { name: 'Today · Overall' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Switch language to English' })).toHaveAttribute('aria-pressed', 'true')
  await expect(page.locator('.metric-value .metric-unit')).toHaveText('pts')
  await expect(page.locator('.metric-value .metric-number')).toHaveAttribute('title', '88.50 pts')

  for (const width of [320, 375, 780, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }

  await page.getByRole('button', { name: 'Switch language to Chinese' }).click()
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('CPA 用量榜')
  await expect(page.getByRole('heading', { name: '今日 · 综合总榜' })).toBeVisible()
  await expect(page.locator('.metric-value .metric-unit')).toHaveText('分')
  await expect(page.locator('.metric-value .metric-number')).toHaveAttribute('title', '88.50 分')
  expect(await page.evaluate(() => localStorage.getItem('ranking_locale'))).toBe('zh')
  expect(documents).toBe(1)

  for (const width of [320, 375, 780, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }

  await page.reload()
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh-CN')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('CPA 用量榜')
  await expect(page.getByRole('button', { name: '切换为中文' })).toHaveAttribute('aria-pressed', 'true')

  await page.getByRole('button', { name: '切换为英文' }).click()
  await expect(page.locator('html')).toHaveAttribute('lang', 'en')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('CPA Usage')
  expect(await page.evaluate(() => localStorage.getItem('ranking_locale'))).toBe('en')
})

test('falls back to English when localStorage holds an unsupported locale', async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem('ranking_locale', 'fr'))
  await page.goto('/')
  await expect(page.locator('html')).toHaveAttribute('lang', 'en')
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('CPA Usage')
  await expect(page.getByRole('button', { name: 'Switch language to English' })).toHaveAttribute('aria-pressed', 'true')
})

test('persists locale across public and admin console routes without page reload or overflow', async ({ page, request }) => {
  test.skip(!config, 'Requires an isolated enabled console')

  const name = `i18n-${randomBytes(3).toString('hex')}`
  const { headers } = seedReportingSeat(name, 1)
  const day = new Date().toISOString().slice(0, 10)
  const report = {
    schema_version: 1,
    revision: '1',
    items: [{
      model: 'claude-fable-5',
      total_tokens: '10000',
      input_tokens: '8000',
      output_tokens: '2000',
      cache_read_tokens: '1000',
      cache_write_tokens: '0',
      reasoning_tokens: '0',
      request_count: '5',
      success_count: '5',
      ttft_avg_ms: 15,
      ttft_samples: '5',
      latency_avg_ms: 40,
      latency_samples: '5',
    }],
  }
  expect((await request.put(`/v1/clients/report/${day}`, { headers, data: report })).status()).toBe(200)

  let documents = 0
  page.on('request', (req) => {
    if (req.isNavigationRequest() && req.resourceType() === 'document') documents++
  })

  await page.goto('/')
  await page.getByRole('button', { name: 'Switch language to Chinese' }).click()
  await page.getByRole('navigation', { name: '页面导航' }).getByRole('link', { name: '登录' }).click()
  await expect(page.getByRole('heading', { name: '管理员登录' })).toBeVisible()

  // Toggle back to English on the login screen and sign in.
  await page.getByRole('button', { name: '切换为英文' }).click()
  await expect(page.getByRole('heading', { name: 'Administrator Sign In' })).toBeVisible()
  await page.getByLabel('Username', { exact: true }).fill(credentials.username)
  await page.getByLabel('Password', { exact: true }).fill(credentials.password)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()

  const consoleNav = page.getByRole('navigation', { name: 'Console navigation' })
  await expect(consoleNav).toBeVisible()
  await expect(page.getByRole('heading', { name: 'Leaderboards', exact: true })).toBeVisible()

  for (const width of [320, 375, 780, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }

  await consoleNav.getByRole('link', { name: 'Users', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Users', exact: true })).toBeVisible()
  for (const width of [320, 375, 780, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }

  await page.getByRole('button', { name: 'Switch language to Chinese' }).click()
  await expect(page.getByRole('heading', { name: '用户管理', exact: true })).toBeVisible()
  await page.getByRole('navigation', { name: '控制台导航' }).getByRole('link', { name: '管理员管理', exact: true }).click()
  await expect(page.getByRole('heading', { name: '管理员管理', exact: true })).toBeVisible()
  for (const width of [320, 375, 780, 1440]) {
    await page.setViewportSize({ width, height: 900 })
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  }
  await page.getByRole('navigation', { name: '页面导航' }).getByRole('link', { name: '排行榜', exact: true }).click()
  await expect(page.getByRole('heading', { level: 1 })).toHaveText('CPA 用量榜')
  expect(documents).toBe(1)
})
