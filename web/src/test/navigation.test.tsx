import { act, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'
import RootApp from '../RootApp'
import { isAppRoute, navigate } from '../navigation'

const json = (value: unknown, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } })
const session = { enabled: true, authenticated: true, user: { id: 'owner', username: 'owner', owner: true }, csrf_token: 'csrf', expires_at: new Date(Date.now() + 3600000).toISOString() }
const empty = { board: 'overall', period: 'today', start: '2026-11-16', end: '2026-11-17', generated_at: '2026-11-16T00:00:00Z', entries: [{ rank: 1, client_id: '7', display_name: '测试用户', avatar_id: '1', metric: '50.00' }] }
const catalog = { default_avatar_id: 1, avatars: [{ avatar_id: 1, path: 'images/001.webp' }] }
afterEach(() => { vi.unstubAllGlobals(); vi.restoreAllMocks(); window.history.replaceState({}, '', '/') })

it('keeps the document and console shell while sharing one session across tabs, language toggles, and public navigation', async () => {
  const focus = vi.spyOn(HTMLElement.prototype, 'focus')
  let finish!: (response: Response) => void
  const fetcher = vi.fn((path: RequestInfo | URL) => {
    if (String(path).includes('/admin/session')) return Promise.resolve(json(session))
    if (String(path).includes('/admin/users')) return new Promise<Response>((resolve) => { finish = resolve })
    if (String(path).includes('/admin/accounts')) return Promise.resolve(json({ owner: { username: 'owner' }, accounts: [] }))
    return Promise.resolve(json(String(path).includes('avatars') ? catalog : empty))
  })
  vi.stubGlobal('fetch', fetcher)
  const user = userEvent.setup()
  render(<RootApp />)
  await user.click(await screen.findByRole('link', { name: 'Console' }))
  const navigation = await screen.findByRole('navigation', { name: 'Console navigation' })
  await screen.findByRole('button', { name: 'Exclude' })
  const ids = Array.from(document.querySelectorAll('[id]')).map((node) => node.id)
  expect(new Set(ids).size).toBe(ids.length)
  expect(focus).toHaveBeenCalledWith({ preventScroll: true })
  const header = screen.getByRole('navigation', { name: 'Page navigation' })
  expect(within(header).getAllByRole('link').map((link) => link.textContent)).toEqual(['Leaderboards', 'Console'])
  expect(within(header).getByRole('link', { name: 'Console' })).toHaveAttribute('aria-current', 'page')
  expect(screen.getByRole('contentinfo')).toHaveTextContent(/^CPA Usage Ranking$/)
  expect(screen.getByRole('contentinfo').querySelector('img')).toHaveAttribute('src', '/assets/brand/logo.svg')
  await user.click(screen.getByRole('link', { name: 'Users' }))
  expect(await screen.findByRole('heading', { name: 'Users' })).toBeVisible()
  expect(screen.getByRole('navigation', { name: 'Console navigation' })).toBe(navigation)
  expect(screen.getByRole('main').textContent).not.toMatch(/Loading|Checking session|正在加载|正在检查会话/)
  expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  expect(document.querySelector('.admin-progress')).toBeNull()
  finish(json({ users: [], next_cursor: '' }))
  await user.click(screen.getByRole('link', { name: 'Administrators' }))
  expect(await screen.findByRole('heading', { name: 'Administrators' })).toBeVisible()
  window.history.back()
  await waitFor(() => expect(window.location.pathname).toBe('/admin/users'))
  expect(await screen.findByRole('heading', { name: 'Users' })).toBeVisible()
  window.history.forward()
  expect(await screen.findByRole('heading', { name: 'Administrators' })).toBeVisible()
  // Switch to Chinese in admin console and verify it persists when returning to public route.
  await user.click(screen.getByRole('button', { name: 'Switch language to Chinese' }))
  expect(screen.getByRole('heading', { name: '管理员管理' })).toBeVisible()
  const reads = fetcher.mock.calls.filter(([path]) => String(path) === '/v1/leaderboards/overall/today').length
  await user.click(within(screen.getByRole('navigation', { name: '页面导航' })).getByRole('link', { name: '排行榜' }))
  expect(screen.getByRole('heading', { name: 'CPA 用量榜' })).toBeVisible()
  await waitFor(() => expect(fetcher.mock.calls.filter(([path]) => String(path) === '/v1/leaderboards/overall/today').length).toBeGreaterThan(reads))
  expect(fetcher.mock.calls.filter(([path]) => String(path) === '/v1/admin/session')).toHaveLength(1)
})

it('invalidates the shared identity on a protected 401 instead of trusting the previous screen', async () => {
  let expired = false
  vi.stubGlobal('fetch', vi.fn((path: RequestInfo | URL) => {
    if (String(path).includes('/admin/session')) return Promise.resolve(json(expired ? { enabled: true, authenticated: false } : session))
    if (String(path).includes('/admin/users')) { expired = true; return Promise.resolve(json({}, 401)) }
    return Promise.resolve(json(String(path).includes('avatars') ? catalog : empty))
  }))
  window.history.replaceState({}, '', '/admin/leaderboards')
  const user = userEvent.setup()
  render(<RootApp />)
  await user.click(await screen.findByRole('link', { name: 'Users' }))
  expect(await screen.findByRole('heading', { name: 'Administrator Sign In' })).toBeVisible()
  expect(window.location.pathname).toBe('/admin/login')
  expect(screen.queryByRole('navigation', { name: 'Console navigation' })).not.toBeInTheDocument()
  const header = screen.getByRole('navigation', { name: 'Page navigation' })
  expect(within(header).getByRole('link', { name: 'Sign in' })).toHaveAttribute('aria-current', 'page')
  await user.click(within(header).getByRole('link', { name: 'Leaderboards' }))
  expect(await screen.findByRole('link', { name: 'Sign in' })).toBeVisible()
})

it('does not fetch public leaderboards or avatars while the public view is hidden', async () => {
  const fetcher = vi.fn((path: RequestInfo | URL) => Promise.resolve(json(String(path).includes('/admin/session') ? { enabled: true, authenticated: false } : String(path).includes('avatars') ? catalog : empty)))
  vi.stubGlobal('fetch', fetcher)
  window.history.replaceState({}, '', '/admin/login')
  const user = userEvent.setup()
  render(<RootApp />)
  expect(await screen.findByRole('heading', { name: 'Administrator Sign In' })).toBeVisible()
  expect(fetcher.mock.calls.map(([path]) => String(path))).toEqual(['/v1/admin/session'])
  await user.click(within(screen.getByRole('navigation', { name: 'Page navigation' })).getByRole('link', { name: 'Leaderboards' }))
  expect(await screen.findByRole('heading', { name: 'Today · Overall' })).toBeVisible()
  const publicReads = fetcher.mock.calls.filter(([path]) => String(path) === '/v1/leaderboards/overall/today').length
  expect(publicReads).toBe(1)
  await user.click(within(screen.getByRole('navigation', { name: 'Page navigation' })).getByRole('link', { name: 'Sign in' }))
  expect(await screen.findByRole('heading', { name: 'Administrator Sign In' })).toBeVisible()
  expect(fetcher.mock.calls.filter(([path]) => String(path) === '/v1/leaderboards/overall/today')).toHaveLength(publicReads)
})

it('normalizes trailing slashes while preserving query and hash in navigation', () => {
  expect(isAppRoute('/admin/users/?q=alice#row-1')).toBe(true)
  expect(isAppRoute('https://external.example/admin/users')).toBe(false)
  navigate('/admin/users/?q=alice#row-1')
  expect(window.location.pathname).toBe('/admin/users')
  expect(window.location.search).toBe('?q=alice')
  expect(window.location.hash).toBe('#row-1')
})

it('stops session polling and focus checks on public pages when admin is disabled or visitor is anonymous', async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true })
  try {
    const fetcher = vi.fn((path: RequestInfo | URL) =>
      Promise.resolve(json(String(path).includes('/admin/session') ? { enabled: true, authenticated: false } : String(path).includes('avatars') ? catalog : empty)))
    vi.stubGlobal('fetch', fetcher)
    const view = render(<RootApp />)
    await waitFor(() => expect(fetcher.mock.calls.filter(([path]) => String(path) === '/v1/admin/session')).toHaveLength(1))
    await act(async () => {
      window.dispatchEvent(new Event('focus'))
      await vi.advanceTimersByTimeAsync(120_000)
    })
    expect(fetcher.mock.calls.filter(([path]) => String(path) === '/v1/admin/session')).toHaveLength(1)
    view.unmount()

    // On /admin/login with admin enabled, focus and 60s timer continue monitoring session state.
    fetcher.mockClear()
    window.history.replaceState({}, '', '/admin/login')
    render(<RootApp />)
    await waitFor(() => expect(fetcher.mock.calls.filter(([path]) => String(path) === '/v1/admin/session')).toHaveLength(1))
    await act(async () => {
      window.dispatchEvent(new Event('focus'))
    })
    await waitFor(() => expect(fetcher.mock.calls.filter(([path]) => String(path) === '/v1/admin/session')).toHaveLength(2))
  } finally {
    vi.useRealTimers()
  }
})
