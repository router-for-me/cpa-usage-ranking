import { expect, test } from '@playwright/test'
import { withTestDatabase } from './database-fixtures'
import { oauthOrigin } from './test-paths'

const start = '/v1/clients/register/start'
const complete = '/v1/clients/register/complete'

test('registers and safely retries against the real binary without adding account UI', async ({ page, request }) => {
  await page.goto('/')
  const ticket = await page.evaluate(async (startPath) => {
    const raw = crypto.getRandomValues(new Uint8Array(32))
    const secret = btoa(String.fromCharCode(...raw)).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '')
    localStorage.setItem('registration-test-secret', secret)
    const response = await fetch(startPath, {
      method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${secret}` },
      body: JSON.stringify({ cpa_uuid: '550e8400-e29b-41d4-a716-446655440000', return_to: `${location.origin}/?from=client#settings` }),
    })
    return { status: response.status, cache: response.headers.get('Cache-Control'), body: await response.json() as { request_id: string; authorize_url: string } }
  }, start)
  expect(ticket.status).toBe(200)
  expect(ticket.cache).toBe('no-store')
  const id = ticket.body.request_id
  const authorizePath = `/auth/github/authorize?request_id=${id}`
  expect(ticket.body.authorize_url).toBe(oauthOrigin + authorizePath)
  const auth = await request.get(authorizePath, { maxRedirects: 0 })
  expect(auth.status()).toBe(302)
  expect(auth.headers()['referrer-policy']).toBe('no-referrer')
  expect(auth.headers().location).toMatch(/^https:\/\/github\.com\/login\/oauth\/authorize\?/)
  const completeInBrowser = () => page.evaluate(async ({ path, requestID }) => {
    const response = await fetch(path, {
      method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${localStorage.getItem('registration-test-secret')}` },
      body: JSON.stringify({ request_id: requestID }),
    })
    return { status: response.status, cache: response.headers.get('Cache-Control'), retryAfter: response.headers.get('Retry-After'), body: await response.text() }
  }, { path: complete, requestID: id })
  const pending = await completeInBrowser()
  expect(pending.status).toBe(202)
  expect(pending.retryAfter).toMatch(/^\d+$/)

  // Only this test DB is seeded at the approved boundary. Never follow the
  // GitHub redirect. Go tests exercise the entire callback with fake transport.
  const approved = withTestDatabase((db) => db.prepare(`UPDATE registration_requests SET stage='approved',
    github_id=(SELECT coalesce(max(github_id),0)+1 FROM clients),display_name='Browser 🙂',
    state=NULL,browser_hash=NULL,verifier=NULL WHERE id=? AND stage='authorizing'`).run(id).changes)
  expect(approved).toBe(1)
  const first = await completeInBrowser()
  const retry = await completeInBrowser()
  expect(first.status).toBe(201)
  expect(first.cache).toBe('no-store')
  expect(retry.status).toBe(200)
  expect(JSON.parse(first.body)).toEqual({ avatar_id: '1', client_id: expect.stringMatching(/^\d+$/), display_name: 'Browser 🙂' })
  expect(JSON.parse(retry.body)).toEqual(JSON.parse(first.body))
  const result = await page.evaluate(async () => {
    const day = new Date().toISOString().slice(0, 10)
    const snapshot = {
      schema_version: 1, revision: '1', items: [{
        model: 'claude-fable-5', total_tokens: '10', input_tokens: '8', output_tokens: '2',
        cache_read_tokens: '3', cache_write_tokens: '1', reasoning_tokens: '1',
        request_count: '2', success_count: '1', ttft_avg_ms: null, ttft_samples: '0',
        latency_avg_ms: 2, latency_samples: '2',
      }],
    }
    const report = async () => {
      const response = await fetch(`/v1/clients/report/${day}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${localStorage.getItem('registration-test-secret')}` },
        body: JSON.stringify(snapshot),
      })
      return { status: response.status, body: await response.json() as Record<string, unknown> }
    }
    const first = await report()
    const retry = await report()
    localStorage.removeItem('registration-test-secret')
    return { first, retry }
  })
  expect(result.first).toEqual({ status: 200, body: { revision: '1' } })
  expect(result.retry).toEqual(result.first)
  const rejected = await request.post(start, { data: {} })
  expect(rejected.status()).toBe(401)
  expect(await rejected.json()).toEqual({ error: { code: 'invalid_credential', message: 'Invalid client credential' } })
  expect((await request.post('/v1/clients/register', { data: {}, maxRedirects: 0 })).status()).toBe(404)
  await expect.poll(async () => {
    const response = await request.get('/v1/leaderboards/overall/today')
    if (response.status() !== 200) return false
    const data = await response.json() as { entries: { display_name: string }[] }
    return data.entries.some((row) => row.display_name === 'Browser 🙂')
  }).toBe(true)
  await page.reload()
  await expect(page.getByText('Browser 🙂')).toBeVisible()
  await expect(page.getByRole('button', { name: /Sign up|Sign in|注册|登录/ })).toHaveCount(0)
})
