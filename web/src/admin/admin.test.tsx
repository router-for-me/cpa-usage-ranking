import { act, screen, waitFor, within } from '@testing-library/react'
import { renderWithSession as render } from '../test/renderWithSession'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'
import { AdminEntry } from './AdminEntry'
import AdminApp from './AdminApp'
import { Accounts } from './Accounts'

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
afterEach(() => { vi.unstubAllGlobals(); window.history.replaceState({}, '', '/') })

it('hides the disabled entry and shows login or console from session state', async () => {
  const disabledFetch = vi.fn(() => Promise.resolve(json({ enabled: false, authenticated: false })))
  vi.stubGlobal('fetch', disabledFetch)
  const first = render(<AdminEntry />)
  await waitFor(() => expect(disabledFetch).toHaveBeenCalledTimes(1))
  await act(async () => {})
  expect(screen.queryByRole('link')).not.toBeInTheDocument()
  first.unmount()
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ enabled: true, authenticated: false }))))
  const second = render(<AdminEntry />)
  expect(await screen.findByRole('link', { name: 'Sign in' })).toHaveAttribute('href', '/admin/login')
  second.unmount()
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ enabled: true, authenticated: true }))))
  render(<AdminEntry />)
  expect(await screen.findByRole('link', { name: 'Console' })).toHaveAttribute('href', '/admin/leaderboards')
})

it('ordinary administrator cannot render account management or fetch its data', async () => {
  window.history.replaceState({}, '', '/admin/accounts')
  const fetcher = vi.fn().mockResolvedValue(json({ enabled: true, authenticated: true, user: { id: '17', username: 'staff', owner: false } }))
  vi.stubGlobal('fetch', fetcher)
  render(<AdminApp />)
  expect(await screen.findByRole('heading', { name: 'Access denied' })).toBeVisible()
  expect(screen.queryByRole('link', { name: 'Administrators' })).not.toBeInTheDocument()
  expect(fetcher.mock.calls.every(([path]) => path === '/v1/admin/session')).toBe(true)
})

it('orders the add form before a separately named administrator list', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ owner: { username: 'owner' }, accounts: [{ id: '2', username: 'demo-iris', created_at: '2026-11-16T00:00:00Z' }] })))
  render(<Accounts session={{ enabled: true, authenticated: true }} onExpired={vi.fn()} />)
  const add = screen.getByRole('region', { name: 'Add Administrator' })
  const list = screen.getByRole('region', { name: 'Administrator List' })
  expect(add.parentElement).toBe(list.parentElement)
  expect(add.compareDocumentPosition(list) & Node.DOCUMENT_POSITION_FOLLOWING).not.toBe(0)
  expect(within(add).getByRole('heading', { name: 'Add Administrator' })).toHaveClass('admin-account-title')
  expect(within(list).getByRole('heading', { name: 'Administrator List' })).toHaveClass('admin-account-title')
  expect(within(add).getByLabelText('Initial password')).toBeVisible()
  expect(await within(list).findByText('demo-iris')).toBeVisible()
})

it('shows one administrator identity without role/config explanations and retains input validation', async () => {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json({ owner: { username: 'configuration-account' }, accounts: [{ id: '2', username: 'reviewer', created_at: '2026-11-16T00:00:00Z' }] })))
  render(<Accounts session={{ enabled: true, authenticated: true }} onExpired={vi.fn()} />)
  expect(await screen.findByText('reviewer')).toBeVisible()
  expect(screen.queryByText('configuration-account')).not.toBeInTheDocument()
  expect(screen.queryByText(/Primary admin|Ordinary admin|config file|read-only|主管理员|普通管理员|配置文件|只读|用户名 3–64/)).not.toBeInTheDocument()
  expect(screen.getByRole('heading', { name: 'Add Administrator' })).toBeVisible()
  expect(screen.getByLabelText('Username')).toHaveAttribute('minlength', '3')
  expect(screen.getByLabelText('Initial password')).toHaveAttribute('minlength', '12')
})

it('does not retry a failed account creation automatically', async () => {
  const fetcher = vi.fn((_path: RequestInfo | URL, init?: RequestInit) => Promise.resolve(init?.method === 'POST' ? json({}, 409) : json({ owner: { username: 'owner' }, accounts: [] })))
  const onExpired = vi.fn()
  vi.stubGlobal('fetch', fetcher)
  const user = userEvent.setup()
  render(<Accounts session={{ enabled: true, authenticated: true, csrf_token: 'test-csrf' }} onExpired={onExpired} />)
  expect(await screen.findByText('No administrators yet.')).toBeVisible()
  await user.type(screen.getByLabelText('Username'), 'staff')
  await user.type(screen.getByLabelText('Initial password'), 'isolated-test-password')
  await user.click(screen.getByRole('button', { name: 'Add administrator' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('Username already exists')
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1)
  expect(onExpired).not.toHaveBeenCalled()
})

it('allows password manager input, prevents duplicate login and re-translates login errors', async () => {
  window.history.replaceState({}, '', '/admin/login')
  let finish!: (response: Response) => void
  const fetcher = vi.fn((_path: RequestInfo | URL, init?: RequestInit) => init?.method === 'POST' ? new Promise<Response>((resolve) => { finish = resolve }) : Promise.resolve(json({ enabled: true, authenticated: false })))
  vi.stubGlobal('fetch', fetcher)
  const user = userEvent.setup()
  render(<AdminApp />)
  const username = await screen.findByLabelText('Username')
  expect(screen.queryByText(/使用管理员账号登录|客户端密钥不能用于此处/)).not.toBeInTheDocument()
  expect(screen.getByRole('heading', { name: 'Administrator Sign In' }).nextElementSibling?.tagName).toBe('FORM')
  const password = screen.getByLabelText('Password')
  expect(username).toHaveAttribute('autocomplete', 'username')
  expect(password).toHaveAttribute('autocomplete', 'current-password')
  await user.type(username, 'owner')
  await user.type(password, 'incorrect-test-password')
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  expect(screen.getByRole('button', { name: 'Signing in…' })).toBeDisabled()
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1)
  finish(json({}, 401))
  expect(await screen.findByRole('alert')).toHaveTextContent('Invalid username or password.')
  expect(screen.getByRole('button', { name: 'Sign in' })).toBeEnabled()
  await user.click(screen.getByRole('button', { name: 'Switch language to Chinese' }))
  expect(screen.getByRole('heading', { name: '管理员登录' })).toBeVisible()
  expect(screen.getByRole('alert')).toHaveTextContent('用户名或密码错误。')
})

it('clears expired logout state instead of showing a stale alert after re-login', async () => {
  window.history.replaceState({}, '', '/admin/leaderboards')
  let attempts = 0
  let expired = false
  const emptyBoard = { board: 'overall', period: 'today', start: '2026-11-16', end: '2026-11-17', generated_at: '2026-11-16T00:00:00Z', entries: [] }
  vi.stubGlobal('fetch', vi.fn((path: RequestInfo | URL, init?: RequestInit) => {
    const url = String(path)
    if (url === '/v1/admin/session' && init?.method === 'DELETE') {
      attempts++
      if (attempts === 1) { expired = true; return Promise.resolve(json({}, 500)) }
      return Promise.resolve(json({}, 401))
    }
    if (url === '/v1/admin/session' && init?.method === 'POST') {
      expired = false
      return Promise.resolve(json({ enabled: true, authenticated: true, user: { id: 'owner', username: 'owner', owner: true }, csrf_token: 'csrf-2' }))
    }
    if (url === '/v1/admin/session') {
      return Promise.resolve(json(expired ? { enabled: true, authenticated: false } : { enabled: true, authenticated: true, user: { id: 'owner', username: 'owner', owner: true }, csrf_token: 'csrf-1' }))
    }
    return Promise.resolve(json(url.includes('avatars') ? { default_avatar_id: 1, avatars: [] } : emptyBoard))
  }))
  const user = userEvent.setup()
  render(<AdminApp />)
  await user.click(await screen.findByRole('button', { name: 'Sign out' }))
  expect(await screen.findByRole('heading', { name: 'Administrator Sign In' })).toBeVisible()
  await user.type(screen.getByLabelText('Username'), 'owner')
  await user.type(screen.getByLabelText('Password'), 'correct-test-password')
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  expect(await screen.findByRole('navigation', { name: 'Console navigation' })).toBeVisible()
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
})

it('sends DELETE requests with only X-CSRF-Token and without Content-Type or JSON body', async () => {
  const deleteCalls: Array<{ url: string; init?: RequestInit }> = []
  vi.stubGlobal('fetch', vi.fn((path: RequestInfo | URL, init?: RequestInit) => {
    const url = String(path)
    if (init?.method === 'DELETE') {
      deleteCalls.push({ url, init })
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    if (url === '/v1/admin/accounts') {
      return Promise.resolve(json({ owner: { username: 'owner' }, accounts: [{ id: '9', username: 'staff', created_at: '2026-11-16T00:00:00Z' }] }))
    }
    if (url.startsWith('/v1/admin/users')) {
      return Promise.resolve(json({ users: [], next_cursor: '' }))
    }
    return Promise.resolve(json({ enabled: true, authenticated: true, user: { id: 'owner', username: 'owner', owner: true }, csrf_token: 'csrf-del' }))
  }))
  const user = userEvent.setup()
  const view = render(<Accounts session={{ enabled: true, authenticated: true, csrf_token: 'csrf-del' }} onExpired={vi.fn()} />)
  await user.click(await screen.findByRole('button', { name: 'Delete' }))
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  view.unmount()

  window.history.replaceState({}, '', '/admin/users')
  render(<AdminApp />)
  await user.click(await screen.findByRole('button', { name: 'Sign out' }))

  expect(deleteCalls.map((c) => c.url)).toEqual(['/v1/admin/accounts/9', '/v1/admin/session'])
  for (const call of deleteCalls) {
    expect(call.init?.body).toBeUndefined()
    expect(call.init?.headers).toEqual({ 'X-CSRF-Token': 'csrf-del' })
  }
})

it('shows specific HTTPS and same-origin proxy hints on 403 login errors', async () => {
  for (const [code, enHint, zhHint] of [
    ['https_required', /RANKING_TRUSTED_PROXIES.*X-Forwarded-Proto/, /RANKING_TRUSTED_PROXIES.*X-Forwarded-Proto/],
    ['same_origin_required', /Same-origin check failed.*X-Forwarded-Host/, /同源校验失败.*X-Forwarded-Host/],
  ] as const) {
    window.history.replaceState({}, '', '/admin/login')
    vi.stubGlobal('fetch', vi.fn((_path: RequestInfo | URL, init?: RequestInit) =>
      Promise.resolve(init?.method === 'POST'
        ? json({ error: { code, message: code } }, 403)
        : json({ enabled: true, authenticated: false }))))
    const user = userEvent.setup()
    const view = render(<AdminApp />)
    await user.type(await screen.findByLabelText('Username'), 'owner')
    await user.type(screen.getByLabelText('Password'), 'isolated-test-password')
    await user.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(enHint)
    await user.click(screen.getByRole('button', { name: 'Switch language to Chinese' }))
    expect(screen.getByRole('alert')).toHaveTextContent(zhHint)
    await user.click(screen.getByRole('button', { name: '切换为英文' }))
    view.unmount()
  }
})

it('releases a superseded in-flight session probe when session state is accepted', async () => {
  window.history.replaceState({}, '', '/admin/leaderboards')
  let resolveStale!: (response: Response) => void
  let sessionCalls = 0
  const fetcher = vi.fn((input: RequestInfo | URL) => {
    const url = String(input)
    if (url === '/v1/admin/session') {
      sessionCalls++
      if (sessionCalls === 1) {
        return Promise.resolve(json({ enabled: true, authenticated: true, user: { id: 'owner', username: 'owner', owner: true }, csrf_token: 'csrf' }))
      }
      if (sessionCalls === 2) {
        return new Promise<Response>((resolve) => { resolveStale = resolve })
      }
      return Promise.resolve(json({ enabled: false, authenticated: false }))
    }
    if (url.startsWith('/v1/admin/users')) {
      return Promise.resolve(json({}, 401))
    }
    return Promise.resolve(json({ board: 'overall', period: 'today', start: '2026-11-16', end: '2026-11-17', generated_at: '2026-11-16T00:00:00Z', entries: [] }))
  })
  vi.stubGlobal('fetch', fetcher)
  const user = userEvent.setup()
  render(<AdminApp />)
  const usersLink = await screen.findByRole('link', { name: 'Users' })
  await act(async () => {})
  await act(async () => {
    window.dispatchEvent(new Event('focus'))
  })
  expect(sessionCalls).toBe(2)

  await user.click(usersLink)
  expect(await screen.findByRole('heading', { name: 'Admin console is disabled' })).toBeVisible()
  expect(sessionCalls).toBe(3)

  await act(async () => {
    resolveStale(json({ enabled: true, authenticated: true, user: { id: 'owner', username: 'owner', owner: true }, csrf_token: 'stale' }))
  })
  expect(screen.getByRole('heading', { name: 'Admin console is disabled' })).toBeVisible()
  expect(sessionCalls).toBe(3)
})
