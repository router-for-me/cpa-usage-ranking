import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, it, vi } from 'vitest'
import { Users } from './Users'

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
const user = { id: '9007199254740993', display_name: '<img src=x>', avatar_id: '1', created_at: '2026-11-16T00:00:00Z', disabled: true, ranking_hidden: true }
afterEach(() => vi.unstubAllGlobals())

it('renders names as text, preserves ID precision and resets paging on query', async () => {
  const fetcher = vi.fn((path: RequestInfo | URL) => Promise.resolve(json(String(path).includes('avatars') ? { default_avatar_id: 1, avatars: [{ avatar_id: 1, path: 'images/001.webp' }] } : { users: [user], next_cursor: String(path).includes('after=') ? '' : user.id })))
  vi.stubGlobal('fetch', fetcher)
  const events = userEvent.setup()
  render(<Users session={{ enabled: true, authenticated: true, csrf_token: 'csrf' }} onExpired={vi.fn()} />)
  expect(await screen.findByText(user.id)).toBeVisible()
  expect(screen.getByText('<img src=x>')).toBeVisible()
  expect(document.querySelector('img[src=x]')).toBeNull()
  expect(screen.getByRole('button', { name: 'Unban' })).toBeEnabled()
  expect(screen.getByRole('button', { name: 'Unban' })).toHaveClass('admin-button--moderation')
  await events.click(screen.getByRole('button', { name: 'Next' }))
  expect(await screen.findByText('Page 2 · 1 user')).toBeVisible()
  expect(screen.queryByRole('combobox')).not.toBeInTheDocument()
  expect(screen.getAllByRole('searchbox')).toHaveLength(1)
  await events.type(screen.getByLabelText('Display name or ID'), 'literal%')
  await events.click(screen.getByRole('button', { name: 'Search' }))
  expect(await screen.findByText('Page 1 · 1 user')).toBeVisible()
  expect(fetcher.mock.calls.some(([path]) => String(path) === '/v1/admin/users?limit=50&q=literal%25')).toBe(true)
})

it('sends an explicit target state once and reports failed writes without changing the row', async () => {
  let finish!: (response: Response) => void
  const fetcher = vi.fn((path: RequestInfo | URL, init?: RequestInit) => init?.method === 'PUT' ? new Promise<Response>((resolve) => { finish = resolve }) : Promise.resolve(json(String(path).includes('avatars') ? {} : { users: [user], next_cursor: '' })))
  vi.stubGlobal('fetch', fetcher)
  const events = userEvent.setup()
  render(<Users session={{ enabled: true, authenticated: true, csrf_token: 'csrf' }} onExpired={vi.fn()} />)
  await events.click(await screen.findByRole('button', { name: 'Unban' }))
  expect(screen.getByRole('dialog')).toHaveTextContent('Unban')
  await events.click(screen.getByRole('button', { name: 'Confirm' }))
  expect(screen.getByRole('button', { name: 'Saving…' })).toBeDisabled()
  const writes = fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')
  expect(writes).toHaveLength(1)
  expect(JSON.parse(String(writes[0][1]?.body))).toEqual({ disabled: false })
  finish(json({}, 503))
  expect(await screen.findByRole('alert')).toHaveTextContent('Service temporarily unavailable')
  await events.click(screen.getByRole('button', { name: 'Cancel' }))
  expect(screen.getByRole('button', { name: 'Unban' })).toBeEnabled()
})

it('uses the global hidden filter and requires unbanning before restoration', async () => {
  const fetcher = vi.fn((path: RequestInfo | URL) => Promise.resolve(json(String(path).includes('avatars') ? {} : { users: [user], next_cursor: '' })))
  vi.stubGlobal('fetch', fetcher)
  render(<Users session={{ enabled: true, authenticated: true }} hiddenOnly onExpired={vi.fn()} />)
  expect(await screen.findByRole('button', { name: 'Restore' })).toBeDisabled()
  expect(screen.getByRole('region', { name: 'Excluded Users' })).toBeVisible()
  expect(screen.queryByRole('heading', { name: 'Excluded Users' })).not.toBeInTheDocument()
  expect(screen.queryByText('EXCLUDED USERS')).not.toBeInTheDocument()
  expect(screen.getByText('Unban first, then restore')).toBeVisible()
  expect(screen.getByRole('link', { name: 'Go to Users' })).toHaveAttribute('href', '/admin/users')
  expect(screen.queryByLabelText('Board Status')).not.toBeInTheDocument()
  expect(fetcher.mock.calls.some(([path]) => String(path) === '/v1/admin/users?limit=50&hidden=true')).toBe(true)
})

it('restores eligibility with an explicit false state and refreshes the user list', async () => {
  let restored = false
  const fetcher = vi.fn((path: RequestInfo | URL, init?: RequestInit) => {
    if (String(path).includes('avatars')) return Promise.resolve(json({}))
    if (init?.method === 'PUT') { restored = true; return Promise.resolve(json({ hidden: false })) }
    return Promise.resolve(json({ users: restored ? [] : [{ ...user, disabled: false }], next_cursor: '' }))
  })
  vi.stubGlobal('fetch', fetcher)
  const events = userEvent.setup()
  render(<Users session={{ enabled: true, authenticated: true, csrf_token: 'csrf' }} hiddenOnly onExpired={vi.fn()} />)
  await events.click(await screen.findByRole('button', { name: 'Restore' }))
  await events.click(screen.getByRole('button', { name: 'Confirm' }))
  expect(await screen.findByText('No matching users found.')).toBeVisible()
  expect(document.querySelector('.admin-notice')).toBeNull()
  expect(fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT').map(([, init]) => JSON.parse(String(init?.body)))).toEqual([{ hidden: false }])
})

it('keeps the two retained list labels distinct and targets the active search', async () => {
  vi.stubGlobal('fetch', vi.fn(() => Promise.resolve(json({ users: [], next_cursor: '' }))))
  const session = { enabled: true, authenticated: true }
  const expired = vi.fn()
  render(<><div hidden><Users session={session} onExpired={expired} hiddenOnly active={false} /></div><Users session={session} onExpired={expired} /></>)
  const search = screen.getByLabelText('Display name or ID')
  expect(search).toBeVisible()
  expect(search).toHaveAttribute('id', 'user-search-query')
  expect(screen.getByRole('region', { name: 'Users' })).toBeVisible()
  await screen.findByText('No matching users found.')
})

it('keeps the prior list visible but disables actions while revisiting and revalidating', async () => {
  let reads = 0
  let finish!: (response: Response) => void
  vi.stubGlobal('fetch', vi.fn((path: RequestInfo | URL) => {
    if (String(path).includes('avatars')) return Promise.resolve(json({}))
    reads++
    return reads === 1 ? Promise.resolve(json({ users: [user], next_cursor: '' })) : new Promise<Response>((resolve) => { finish = resolve })
  }))
  const session = { enabled: true, authenticated: true }
  const expired = vi.fn()
  const view = render(<Users session={session} onExpired={expired} />)
  const action = await screen.findByRole('button', { name: 'Unban' })
  view.rerender(<Users session={session} onExpired={expired} active={false} />)
  view.rerender(<Users session={session} onExpired={expired} active />)
  await waitFor(() => expect(finish).toBeDefined())
  expect(screen.getByText(user.id)).toBeVisible()
  expect(action).toBeDisabled()
  expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  await act(async () => finish(json({ users: [user], next_cursor: '' })))
  expect(action).toBeEnabled()
})

it('distinguishes load failure, retry and an empty list', async () => {
  let failed = false
  vi.stubGlobal('fetch', vi.fn((path: RequestInfo | URL) => {
    if (String(path).includes('avatars')) return Promise.resolve(json({}, 404))
    if (!failed) { failed = true; return Promise.resolve(json({}, 503)) }
    return Promise.resolve(json({ users: [], next_cursor: '' }))
  }))
  const events = userEvent.setup()
  render(<Users session={{ enabled: true, authenticated: true }} onExpired={vi.fn()} />)
  expect(await screen.findByRole('alert')).toHaveTextContent('Service temporarily unavailable')
  await events.click(screen.getByRole('button', { name: 'Retry user list' }))
  expect(await screen.findByText('No matching users found.')).toBeVisible()
})

it('shows server_id metadata, clear withdrawn/banned statuses, and exclude/restore controls on users list', async () => {
  const rows = [
    { id: '1', github_id: '101', server_id: '550e8400-e29b-41d4-a716-446655440001', display_name: 'Active User', avatar_id: '1', created_at: '2026-11-16T00:00:00Z', disabled: false, withdrawn: false, ranking_hidden: false },
    { id: '2', github_id: '102', server_id: '550e8400-e29b-41d4-a716-446655440002', display_name: 'Withdrawn User', avatar_id: '1', created_at: '2026-11-16T00:00:00Z', disabled: false, withdrawn: true, ranking_hidden: false },
    { id: '3', github_id: '103', server_id: '550e8400-e29b-41d4-a716-446655440003', display_name: 'Banned Hidden User', avatar_id: '1', created_at: '2026-11-16T00:00:00Z', disabled: true, withdrawn: false, ranking_hidden: true },
  ]
  const fetcher = vi.fn((path: RequestInfo | URL, init?: RequestInit) => {
    if (String(path).includes('avatars')) return Promise.resolve(json({}))
    if (init?.method === 'PUT') return Promise.resolve(json({ hidden: true }))
    return Promise.resolve(json({ users: rows, next_cursor: '' }))
  })
  vi.stubGlobal('fetch', fetcher)
  const events = userEvent.setup()
  render(<Users session={{ enabled: true, authenticated: true, csrf_token: 'csrf' }} onExpired={vi.fn()} />)
  expect(await screen.findByText('550e8400-e29b-41d4-a716-446655440001')).toBeVisible()
  expect(screen.getByText('Withdrawn')).toBeVisible()
  expect(screen.getByText('Off board')).toBeVisible()
  expect(screen.getByText('Listed')).toBeVisible()
  expect(screen.getByText('Excluded')).toBeVisible()
  // Banned + hidden user keeps Restore disabled until unbanned.
  expect(screen.getByRole('button', { name: 'Restore' })).toBeDisabled()
  // Active user can be excluded directly from /admin/users via ConfirmDialog.
  await events.click(screen.getAllByRole('button', { name: 'Exclude' })[0])
  expect(screen.getByRole('dialog')).toHaveTextContent('Exclude “Active User”?')
  await events.click(screen.getByRole('button', { name: 'Confirm' }))
  const putCalls = fetcher.mock.calls.filter(([, init]) => init?.method === 'PUT')
  expect(putCalls).toHaveLength(1)
  expect(String(putCalls[0][0])).toBe('/v1/admin/users/1/ranking-visibility')
  expect(JSON.parse(String(putCalls[0][1]?.body))).toEqual({ hidden: true })
})
