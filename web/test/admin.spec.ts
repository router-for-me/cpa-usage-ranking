import { expect, test, type Page } from '@playwright/test'
import { readFileSync } from 'node:fs'
import { randomBytes } from 'node:crypto'
import { seedUsers } from './admin-fixtures'
import { seedReportingSeat } from './database-fixtures'

const config = process.env.RANKING_TEST_ADMIN_CONFIG
const owner: { username: string; password: string } = config ? JSON.parse(readFileSync(config, 'utf8')) : { username: '', password: '' }
test.skip(!config, 'Uses a separate enabled, isolated console service')
test.setTimeout(100_000)

// Respect the real production login budget rather than weakening the limiter.
let attempts: number[] = []
async function loginSlot() {
  attempts = attempts.filter((time) => Date.now() - time < 61_000)
  if (attempts.length >= 5) {
    await new Promise((resolve) => setTimeout(resolve, 61_000 - (Date.now() - attempts[0])))
    attempts = attempts.filter((time) => Date.now() - time < 61_000)
  }
  attempts.push(Date.now())
}

async function login(page: Page, credentials = owner) {
  await loginSlot()
  await page.goto('/admin/login')
  await page.getByLabel('Username', { exact: true }).fill(credentials.username)
  await page.getByLabel('Password', { exact: true }).fill(credentials.password)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('navigation', { name: 'Console navigation' })).toBeVisible()
}

for (const width of [1440, 375, 320]) {
  test(`owner login, public entry, console and logout at ${width}px`, async ({ page, context }, info) => {
    await page.setViewportSize({ width, height: width === 1440 ? 1000 : 812 })
    await page.goto('/')
    const entry = page.getByRole('link', { name: 'Sign in', exact: true })
    await expect(entry).toBeVisible()
    const brandBox = (await page.locator('.brand').boundingBox())!
    const rankingBox = (await page.getByRole('link', { name: 'Leaderboards', exact: true }).boundingBox())!
    const entryBox = (await entry.boundingBox())!
    expect(rankingBox.x + rankingBox.width).toBeLessThanOrEqual(entryBox.x)
    expect(Math.abs(rankingBox.y - entryBox.y)).toBeLessThan(2)
    if (width >= 761) expect(brandBox.x + brandBox.width).toBeLessThanOrEqual(rankingBox.x)
    else expect(brandBox.y + brandBox.height).toBeLessThanOrEqual(rankingBox.y)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await page.screenshot({ path: info.outputPath(`public-login-${width}.png`), fullPage: true })
    await entry.click()
    await loginSlot()
    await page.getByLabel('Username', { exact: true }).fill(owner.username)
    await page.getByLabel('Password', { exact: true }).fill(owner.password)
    await page.getByRole('button', { name: 'Sign in', exact: true }).click()
    await expect(page.getByRole('heading', { name: 'Leaderboards', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: /Annual|年度/ })).toHaveCount(0)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await page.screenshot({ path: info.outputPath(`console-${width}.png`), fullPage: true })
    await page.getByRole('navigation', { name: 'Page navigation' }).getByRole('link', { name: 'Leaderboards' }).click()
    const consoleEntry = page.getByRole('link', { name: 'Console', exact: true })
    await expect(consoleEntry).toBeVisible()
    await page.screenshot({ path: info.outputPath(`public-console-${width}.png`), fullPage: true })
    await consoleEntry.click()
    await page.getByRole('button', { name: 'Sign out', exact: true }).click()
    await expect(page.getByRole('link', { name: 'Sign in', exact: true })).toBeVisible()
    expect((await context.cookies()).filter((cookie) => cookie.name === 'ranking_admin')).toHaveLength(0)
  })
}

test('login failure, keyboard submission and session expiry', async ({ page, context }) => {
  await page.goto('/admin/login')
  await page.getByLabel('Username', { exact: true }).fill(owner.username)
  await page.getByLabel('Password', { exact: true }).fill('incorrect-password-for-test')
  await loginSlot()
  await page.getByLabel('Password', { exact: true }).press('Enter')
  await expect(page.getByRole('alert')).toContainText('Invalid username or password.')
  await page.getByLabel('Password', { exact: true }).fill(owner.password)
  await loginSlot()
  await page.getByLabel('Password', { exact: true }).press('Enter')
  await expect(page.getByRole('navigation', { name: 'Console navigation' })).toBeVisible()
  await context.clearCookies()
  await page.reload()
  await expect(page).toHaveURL(/\/admin\/login$/)
  await page.goto('/')
  await expect(page.getByRole('link', { name: 'Sign in', exact: true })).toBeVisible()
})

test('owner manages ordinary accounts and deletion revokes the live session', async ({ page, browser, baseURL }, info) => {
  await page.setViewportSize({ width: 375, height: 812 })
  await login(page)
  await page.getByRole('link', { name: 'Administrators', exact: true }).click()
  const credentials = { username: `staff-${Date.now()}`, password: randomBytes(24).toString('base64url') }
  await page.getByLabel('Username', { exact: true }).fill(credentials.username)
  await page.getByLabel('Initial password', { exact: true }).fill(credentials.password)
  await page.getByRole('button', { name: 'Add administrator', exact: true }).click()
  const row = page.getByRole('row').filter({ hasText: credentials.username })
  await expect(row).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: info.outputPath('accounts-375.png'), fullPage: true })
  const ordinaryContext = await browser.newContext({ baseURL })
  try {
    const ordinary = await ordinaryContext.newPage()
    await login(ordinary, credentials)
    await expect(ordinary.getByRole('link', { name: 'Administrators', exact: true })).toHaveCount(0)
    expect((await ordinaryContext.request.get('/v1/admin/accounts')).status()).toBe(403)
    await ordinary.goto('/admin/accounts')
    await expect(ordinary.getByRole('heading', { name: 'Access denied' })).toBeVisible()
    await row.getByRole('button', { name: 'Delete', exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Confirm' }).click()
    await expect(row).toHaveCount(0)
    await expect(page.locator('.admin-notice')).toHaveCount(0)
    await ordinary.goto('/admin/leaderboards')
    await expect(ordinary).toHaveURL(/\/admin\/login$/)
  } finally { await ordinaryContext.close() }
})

test('lists real registered users with bounded paging and unified search', async ({ page }, info) => {
  const prefix = `adm3-${randomBytes(3).toString('hex')}`
  const users = seedUsers(prefix, 51)
  await page.setViewportSize({ width: 320, height: 812 })
  await login(page)
  await page.getByRole('link', { name: 'Users', exact: true }).click()
  await page.getByLabel('Display name or ID', { exact: true }).fill(prefix)
  await page.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(page.getByText('Page 1 · 50 users')).toBeVisible()
  await expect(page.getByText(users[0].name, { exact: true })).toBeVisible()
  expect(await page.locator('.admin-users img').count()).toBeGreaterThan(0)
  expect(await page.locator('.admin-users img[src="x"]').count()).toBe(0)
  await page.getByRole('button', { name: 'Next' }).focus()
  await page.keyboard.press('Enter')
  await expect(page.getByText('Page 2 · 1 user')).toBeVisible()
  await page.getByLabel('Display name or ID', { exact: true }).fill(users[0].name)
  await page.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(page.getByText('Page 1 · 1 user')).toBeVisible()
  await expect(page.getByText(users[0].name, { exact: true })).toBeVisible()
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await page.screenshot({ path: info.outputPath('users-320.png'), fullPage: true })
  await page.getByLabel('Display name or ID', { exact: true }).fill(`${prefix}-missing`)
  await page.getByRole('button', { name: 'Search', exact: true }).click()
  await expect(page.getByText('No matching users found.')).toBeVisible()
})

test('bans and unbans a reporting identity and refreshes the five-minute cache', async ({ page, request, baseURL }, info) => {
  const name = `ban-${randomBytes(4).toString('hex')}`
  const { id, headers } = seedReportingSeat(name, 1)
  const day = new Date().toISOString().slice(0, 10)
  const report = { schema_version: 1, revision: '1', items: [{ model: 'claude-fable-5', total_tokens: '1234567', input_tokens: '1000', output_tokens: '100', cache_read_tokens: '500', cache_write_tokens: '0', reasoning_tokens: '0', request_count: '10', success_count: '9', ttft_avg_ms: 12, ttft_samples: '10', latency_avg_ms: 20, latency_samples: '10' }] }
  expect((await request.put(`/v1/clients/report/${day}`, { headers, data: report })).status()).toBe(200)
  await page.setViewportSize({ width: 375, height: 812 })
  await login(page)
  const session = await (await page.request.get('/v1/admin/session')).json()
  expect((await page.request.put(`/v1/admin/users/${id}/ban`, { headers: { Origin: baseURL!, 'X-CSRF-Token': session.csrf_token }, data: { disabled: false } })).status()).toBe(200)
  const visible = async () => ((await (await request.get('/v1/leaderboards/usage/today')).json()).entries as { client_id: string }[]).some((entry) => entry.client_id === id)
  await expect.poll(visible).toBe(true)
  await page.getByRole('link', { name: 'Users', exact: true }).click()
  await page.getByLabel('Display name or ID', { exact: true }).fill(name)
  await page.getByRole('button', { name: 'Search', exact: true }).click()
  let row = page.getByRole('row').filter({ hasText: name })
  expect(await row.getByRole('button', { name: 'Ban', exact: true }).evaluate((node) => [getComputedStyle(node).fontSize, getComputedStyle(node).fontWeight])).toEqual(['13px', '700'])
  await page.screenshot({ path: info.outputPath('ban-action-375.png'), fullPage: true })
  const ban = row.getByRole('button', { name: 'Ban', exact: true })
  await ban.click()
  const dialog = page.getByRole('dialog')
  await expect(dialog).toContainText('Blocks future reports and removes this client from all leaderboards while retaining stored metrics.')
  const mobileDialog = (await dialog.boundingBox())!
  const mobileShell = (await page.locator('.admin-shell').boundingBox())!
  expect(Math.abs(mobileDialog.x + mobileDialog.width / 2 - mobileShell.x - mobileShell.width / 2)).toBeLessThan(2)
  expect(Math.abs(mobileDialog.y + mobileDialog.height / 2 - 812 / 2)).toBeLessThan(2)
  await expect(dialog.getByRole('button', { name: 'Cancel' })).toBeFocused()
  await page.screenshot({ path: info.outputPath('confirm-ban-375.png'), fullPage: true })
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)
  await expect(ban).toBeFocused()
  await expect(row.getByText('Active', { exact: true })).toBeVisible()
  await ban.click()
  await page.getByRole('dialog').getByRole('button', { name: 'Confirm' }).click()
  row = page.getByRole('row').filter({ hasText: name })
  await expect(page.locator('.admin-notice')).toHaveCount(0)
  await expect(row.getByText('Banned', { exact: true })).toBeVisible()
  expect((await request.put(`/v1/clients/report/${day}`, { headers, data: report })).status()).toBe(403)
  await expect.poll(visible).toBe(false)
  await page.screenshot({ path: info.outputPath('banned-user-375.png'), fullPage: true })
  await row.getByRole('button', { name: 'Unban', exact: true }).click()
  await page.getByRole('dialog').getByRole('button', { name: 'Confirm' }).click()
  await expect(row.getByText('Active', { exact: true })).toBeVisible()
  await expect.poll(visible).toBe(true)
  expect((await request.put(`/v1/clients/report/${day}`, { headers, data: report })).status()).toBe(200)
})

test('ordinary admin excludes and restores globally while ban remains independent', async ({ page, request, browser, baseURL }, info) => {
  const name = `hidden-${randomBytes(4).toString('hex')}`
  const { id, headers: bearer } = seedReportingSeat(name, 2)
  const day = new Date().toISOString().slice(0, 10)
  const report = { schema_version: 1, revision: '1', items: [{ model: 'claude-fable-5', total_tokens: '1234567', input_tokens: '1000', output_tokens: '100', cache_read_tokens: '500', cache_write_tokens: '0', reasoning_tokens: '0', request_count: '10', success_count: '9', ttft_avg_ms: 12, ttft_samples: '10', latency_avg_ms: 20, latency_samples: '10' }] }
  expect((await request.put(`/v1/clients/report/${day}`, { headers: bearer, data: report })).status()).toBe(200)
  await login(page)
  const ownerSession = await (await page.request.get('/v1/admin/session')).json()
  const ownerHeaders = { Origin: baseURL!, 'X-CSRF-Token': ownerSession.csrf_token }
  const credentials = { username: `staff-${randomBytes(4).toString('hex')}`, password: randomBytes(24).toString('base64url') }
  const created = await page.request.post('/v1/admin/accounts', { headers: ownerHeaders, data: credentials })
  expect(created.status()).toBe(201)
  const account = await created.json()
  expect((await page.request.put(`/v1/admin/users/${id}/ban`, { headers: ownerHeaders, data: { disabled: false } })).status()).toBe(200)
  const visible = async () => ((await (await request.get('/v1/leaderboards/usage/today')).json()).entries as { client_id: string }[]).some((entry) => entry.client_id === id)
  await expect.poll(visible).toBe(true)
  const staffContext = await browser.newContext({ baseURL })
  try {
    const staff = await staffContext.newPage()
    await login(staff, credentials)
    let row = staff.getByRole('row').filter({ hasText: name })
    await expect(row).toBeVisible()
    expect(await row.getByRole('button', { name: 'Exclude', exact: true }).evaluate((node) => Number(getComputedStyle(node).fontWeight))).toBeGreaterThanOrEqual(700)
    for (const width of [1440, 375, 320]) {
      await staff.setViewportSize({ width, height: width === 1440 ? 1000 : 812 })
      expect(await staff.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
      const action = row.getByRole('button', { name: 'Exclude' })
      expect(await action.evaluate((node) => [getComputedStyle(node).fontSize, getComputedStyle(node).fontWeight])).toEqual(['13px', '700'])
      if (width === 1440) {
        const header = (await staff.getByRole('columnheader', { name: 'Action' }).boundingBox())!
        const button = (await action.boundingBox())!
        expect(Math.abs(header.x + header.width / 2 - button.x - button.width / 2)).toBeLessThan(2)
        expect(await staff.getByRole('columnheader', { name: 'Score' }).evaluate((node) => getComputedStyle(node).textAlign)).toBe('right')
      }
      await staff.screenshot({ path: info.outputPath(`managed-board-${width}.png`), fullPage: true })
    }
    for (const board of ['Token Usage', 'Cache Reuse', 'Reliability', 'First-Token Latency']) {
      await staff.getByRole('button', { name: board, exact: true }).click()
      await expect(staff.getByRole('row').filter({ hasText: name }).getByRole('button', { name: 'Exclude' })).toBeVisible()
      expect(await staff.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    }
    await staff.getByRole('button', { name: 'Token Usage', exact: true }).click()
    await expect(staff.getByRole('row').filter({ hasText: name }).getByRole('button', { name: 'Exclude' })).toBeVisible()
    await expect(staff.getByRole('heading', { name: 'Today · Token Usage', exact: true })).toBeVisible()
    await staff.screenshot({ path: info.outputPath('managed-usage-320.png'), fullPage: true })
    await staff.getByRole('button', { name: 'Overall', exact: true }).click()
    await expect(staff.getByRole('row').filter({ hasText: name }).getByRole('button', { name: 'Exclude' })).toBeVisible()
    await staff.setViewportSize({ width: 1440, height: 1000 })
    await row.getByRole('button', { name: 'Exclude', exact: true }).focus()
    await staff.keyboard.press('Enter')
    await expect(staff.getByRole('dialog')).toContainText('Removes this client from all leaderboards while keeping data and accepting future reports.')
    const desktopDialog = (await staff.getByRole('dialog').boundingBox())!
    const desktopShell = (await staff.locator('.admin-shell').boundingBox())!
    expect(Math.abs(desktopDialog.x + desktopDialog.width / 2 - desktopShell.x - desktopShell.width / 2)).toBeLessThan(2)
    expect(Math.abs(desktopDialog.y + desktopDialog.height / 2 - 1000 / 2)).toBeLessThan(2)
    expect(await staff.getByRole('dialog').getByRole('button', { name: 'Confirm' }).evaluate((node) => getComputedStyle(node).fontSize)).toBe('14px')
    await staff.screenshot({ path: info.outputPath('confirm-exclusion-1440.png'), fullPage: true })
    await staff.getByRole('dialog').getByRole('button', { name: 'Confirm' }).click()
    await expect(staff.getByRole('row').filter({ hasText: name })).toHaveCount(0)
    await expect(staff.locator('.admin-notice')).toHaveCount(0)
    await expect.poll(visible).toBe(false)
    report.revision = '2'; report.items[0].total_tokens = '7654321'
    expect((await request.put(`/v1/clients/report/${day}`, { headers: bearer, data: report })).status()).toBe(200)
    await staff.getByRole('button', { name: 'Excluded Users', exact: true }).click()
    await expect(staff.getByRole('region', { name: 'Excluded Users', exact: true })).toBeVisible()
    await expect(staff.getByRole('heading', { name: 'Excluded Users' })).toHaveCount(0)
    row = staff.getByRole('row').filter({ hasText: name })
    await expect(row.getByRole('button', { name: 'Restore' })).toBeEnabled()
    expect(await row.getByRole('button', { name: 'Restore' }).evaluate((node) => [getComputedStyle(node).fontSize, getComputedStyle(node).fontWeight])).toEqual(['13px', '700'])
    await page.getByRole('link', { name: 'Users', exact: true }).click()
    await page.getByLabel('Display name or ID', { exact: true }).fill(name)
    await page.getByRole('button', { name: 'Search', exact: true }).click()
    const ownerRow = page.getByRole('row').filter({ hasText: name })
    await ownerRow.getByRole('button', { name: 'Ban', exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Confirm' }).click()
    await expect(ownerRow.getByText('Banned', { exact: true })).toBeVisible()
    await staff.getByRole('button', { name: 'Active Board', exact: true }).click()
    await staff.getByRole('button', { name: 'Excluded Users', exact: true }).click()
    row = staff.getByRole('row').filter({ hasText: name })
    await expect(row.getByRole('button', { name: 'Restore' })).toBeDisabled()
    await expect(row.getByText('Unban first, then restore')).toBeVisible()
    const staffSession = await (await staff.request.get('/v1/admin/session')).json()
    expect((await staff.request.put(`/v1/admin/users/${id}/ranking-visibility`, { headers: { Origin: baseURL!, 'X-CSRF-Token': staffSession.csrf_token }, data: { hidden: false } })).status()).toBe(409)
    await staff.setViewportSize({ width: 375, height: 812 })
    await staff.screenshot({ path: info.outputPath('hidden-banned-375.png'), fullPage: true })
    await ownerRow.getByRole('button', { name: 'Unban', exact: true }).click()
    await page.getByRole('dialog').getByRole('button', { name: 'Confirm' }).click()
    await expect(ownerRow.getByText('Active', { exact: true })).toBeVisible()
    await expect(ownerRow.getByText('Excluded', { exact: true })).toBeVisible()
    await expect.poll(visible).toBe(false)
    await staff.getByRole('button', { name: 'Active Board', exact: true }).click()
    await staff.getByRole('button', { name: 'Excluded Users', exact: true }).click()
    row = staff.getByRole('row').filter({ hasText: name })
    await row.getByRole('button', { name: 'Restore' }).click()
    await staff.getByRole('dialog').getByRole('button', { name: 'Confirm' }).click()
    await expect(row).toHaveCount(0)
    await expect(staff.locator('.admin-notice')).toHaveCount(0)
    await expect.poll(visible).toBe(true)
    await staff.getByRole('button', { name: 'Active Board', exact: true }).click()
    await expect(staff.getByRole('row').filter({ hasText: name })).toBeVisible()
    await page.setViewportSize({ width: 375, height: 812 })
    await page.getByRole('navigation', { name: 'Page navigation' }).getByRole('link', { name: 'Leaderboards', exact: true }).click()
    await expect(page.getByRole('row').filter({ hasText: name })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Exclude', exact: true })).toHaveCount(0)
    await expect(page.getByText(/Original value|原值/, { exact: true })).toHaveCount(0)
    await page.screenshot({ path: info.outputPath('public-preserved-375.png'), fullPage: true })
  } finally {
    await staffContext.close()
    expect((await page.request.delete(`/v1/admin/accounts/${account.id}`, { headers: ownerHeaders, data: {} })).status()).toBe(204)
  }
})

test('expired public session changes the top right entry without navigation', async ({ page, context }) => {
  await page.clock.install()
  await login(page)
  await page.goto('/')
  await expect(page.getByRole('link', { name: 'Console', exact: true })).toBeVisible()
  await context.clearCookies()
  await page.clock.fastForward(61_000)
  await expect(page.getByRole('link', { name: 'Sign in', exact: true })).toBeVisible()
})
