import { readFileSync } from 'node:fs'
import { randomBytes } from 'node:crypto'
import { expect, test, type Page } from '@playwright/test'
import { seedReportingSeat } from './database-fixtures'

const config = process.env.RANKING_TEST_ADMIN_CONFIG
const credentials: { username: string; password: string } = config ? JSON.parse(readFileSync(config, 'utf8')) : { username: '', password: '' }
test.skip(!config, 'Requires an isolated enabled console')

async function login(page: Page) {
  await page.goto('/admin/login')
  await page.getByLabel('Username', { exact: true }).fill(credentials.username)
  await page.getByLabel('Password', { exact: true }).fill(credentials.password)
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('navigation', { name: 'Console navigation' })).toBeVisible()
}

test('places hidden-user search immediately below its tab without a duplicate heading', async ({ page, baseURL }, info) => {
  const name = `hidden-list-${randomBytes(3).toString('hex')}`
  const { id } = seedReportingSeat(name, 1)
  await login(page)
  const session = await (await page.request.get('/v1/admin/session')).json()
  expect((await page.request.put(`/v1/admin/users/${id}/ranking-visibility`, { headers: { Origin: baseURL!, 'X-CSRF-Token': session.csrf_token }, data: { hidden: true } })).status()).toBe(200)
  await page.getByRole('button', { name: 'Excluded Users', exact: true }).click()
  const region = page.getByRole('region', { name: 'Excluded Users' })
  await expect(region.getByRole('row').filter({ hasText: name })).toBeVisible()
  await expect(region.getByRole('heading')).toHaveCount(0)
  await expect(region.getByText('EXCLUDED USERS')).toHaveCount(0)
  for (const width of [1440, 375]) {
    await page.setViewportSize({ width, height: width === 1440 ? 1000 : 812 })
    const tab = (await page.getByRole('button', { name: 'Excluded Users', exact: true }).boundingBox())!
    const search = (await region.getByLabel('Display name or ID').boundingBox())!
    expect(search.y - tab.y - tab.height).toBeLessThan(35)
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
    await page.screenshot({ path: info.outputPath(`hidden-list-${width}.png`), fullPage: true })
  }
  await page.getByRole('link', { name: 'Users', exact: true }).click()
  await expect(page.getByRole('heading', { name: 'Users' })).toBeVisible()
})

test('keeps the login form compact without the removed explanation and reports real errors', async ({ page }, info) => {
  await page.goto('/admin/login')
  await expect(page.getByLabel('Username', { exact: true })).toBeVisible()
  await expect(page.getByText(/使用管理员账号登录|客户端密钥不能用于此处/)).toHaveCount(0)
  expect(await page.getByRole('heading', { name: 'Administrator Sign In' }).evaluate((node) => node.nextElementSibling?.tagName)).toBe('FORM')
  for (const width of [1440, 375]) {
    await page.setViewportSize({ width, height: width === 1440 ? 1000 : 812 })
    await page.screenshot({ path: info.outputPath(`login-${width}.png`), fullPage: true })
  }
  await page.getByLabel('Username', { exact: true }).fill(credentials.username)
  await page.getByLabel('Password', { exact: true }).fill('invalid-isolated-password')
  await page.getByRole('button', { name: 'Sign in', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('Invalid username or password.')
})

test('uses the same existing title typography in both administrator cards', async ({ page }, info) => {
  await login(page)
  await page.getByRole('link', { name: 'Administrators', exact: true }).click()
  const add = page.getByRole('region', { name: 'Add Administrator' })
  const list = page.getByRole('region', { name: 'Administrator List' })
  const typography = (node: Element) => { const css = getComputedStyle(node); return [css.fontFamily, css.fontSize, css.fontWeight, css.lineHeight] }
  for (const width of [1440, 375]) {
    await page.setViewportSize({ width, height: width === 1440 ? 1000 : 812 })
    const left = await add.getByRole('heading').evaluate(typography)
    const right = await list.getByRole('heading').evaluate(typography)
    expect(left).toEqual(right)
    expect(left[1]).toBe('20px')
    expect(left[2]).toBe('400')
    const leftBox = (await add.boundingBox())!
    const rightBox = (await list.boundingBox())!
    if (width === 1440) expect(Math.abs(rightBox.width - 2 * leftBox.width)).toBeLessThan(2)
    else expect(leftBox.y + leftBox.height).toBeLessThan(rightBox.y)
    await page.screenshot({ path: info.outputPath(`account-titles-${width}.png`), fullPage: true })
  }
})
