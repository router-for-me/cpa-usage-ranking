import { useEffect, useState, type FormEvent } from 'react'
import { fetchAvatarCatalog, type AvatarCatalog } from '../api/leaderboards'
import { Avatar } from '../components/LeaderboardTable'
import { formatLocalizedError, useI18n } from '../i18n'
import { AdminError, adminRequest, jsonRequest, type AdminSession } from './api'
import { Link } from '../Link'
import { ConfirmDialog } from './ConfirmDialog'

type User = { id: string; github_id?: string; server_id?: string; display_name: string; avatar_id: string; created_at: string; disabled: boolean; withdrawn?: boolean; ranking_hidden: boolean }
type UserPage = { users: User[]; next_cursor: string }

export function Users({ session, onExpired, hiddenOnly = false, active = true }: { session: AdminSession; onExpired: () => void; hiddenOnly?: boolean; active?: boolean }) {
  const { locale, m } = useI18n()
  const searchId = hiddenOnly ? 'hidden-users-search' : 'user-search-query'
  const [query, setQuery] = useState('limit=50')
  const [cursors, setCursors] = useState<string[]>([''])
  const [attempt, setAttempt] = useState(0)
  const [result, setResult] = useState<{ key: string; data?: UserPage; failed?: boolean; error?: unknown; page: number }>({ key: '', page: 1 })
  const [avatars, setAvatars] = useState<AvatarCatalog | null>(null)
  const [busy, setBusy] = useState<{ id: string; kind: 'ban' | 'visibility' } | null>(null)
  const [action, setAction] = useState<{ kind: 'ban' | 'exclude' | 'restore'; user: User } | null>(null)
  const cursor = cursors[cursors.length - 1]
  const path = `users?${query}${hiddenOnly ? '&hidden=true' : ''}${cursor ? `&after=${cursor}` : ''}`
  const key = `${path}/${attempt}`
  const loading = result.key !== key
  const data = result.data
  const users = Array.isArray(data?.users) ? data.users : []
  const stale = loading || !!result.failed
  const errorMessage = !loading && result.failed
    ? formatLocalizedError(result.error, (msg) => msg.admin.errors.network, locale)
    : ''
  useEffect(() => {
    if (!active) return
    return () => { setAction(null); setAttempt((old) => old + 1) }
  }, [active])
  useEffect(() => {
    if (!active) return
    let current = true
    adminRequest<UserPage>(path).then((value) => { if (current) setResult({ key, data: value, failed: false, error: undefined, page: cursors.length }) }).catch((err: unknown) => {
      if (current) { setResult((previous) => ({ ...previous, key, failed: true, error: err })); if (err instanceof AdminError && err.status === 401) onExpired() }
    })
    return () => { current = false }
  }, [path, key, onExpired, active, cursors.length])
  useEffect(() => {
    if (!active || avatars) return
    const controller = new AbortController()
    fetchAvatarCatalog(controller.signal).then(setAvatars).catch(() => { /* Reuse the neutral fallback. */ })
    return () => controller.abort()
  }, [active, avatars, key])
  function search(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const params = new URLSearchParams({ limit: '50' })
    const term = String(form.get('query')).trim()
    if (term) params.set('q', term)
    setCursors(['']); setQuery(params.toString()); setAttempt((old) => old + 1)
  }
  async function setBan(user: User) {
    if (busy) return
    setBusy({ id: user.id, kind: 'ban' })
    try {
      await adminRequest(`users/${user.id}/ban`, jsonRequest('PUT', { disabled: !user.disabled }, session.csrf_token))
      setAttempt((old) => old + 1)
    } catch (error) { if (error instanceof AdminError && error.status === 401) onExpired(); throw error }
    finally { setBusy(null) }
  }
  async function setRankingHidden(user: User, hidden: boolean) {
    if (busy || (!hidden && user.disabled)) return
    setBusy({ id: user.id, kind: 'visibility' })
    try {
      await adminRequest(`users/${user.id}/ranking-visibility`, jsonRequest('PUT', { hidden }, session.csrf_token))
      setAttempt((old) => old + 1)
    } catch (error) { if (error instanceof AdminError && error.status === 401) onExpired(); throw error }
    finally { setBusy(null) }
  }
  function renderSeatStatus(user: User) {
    return <div className="status-pills">
      {user.disabled && <span className="status-pill status-pill--danger">{m.admin.statusBanned}</span>}
      {user.withdrawn && <span className="status-pill status-pill--neutral">{m.admin.statusWithdrawn}</span>}
      {!user.disabled && !user.withdrawn && <span className="status-pill status-pill--ok">{m.admin.statusActive}</span>}
    </div>
  }
  function renderBoardStatus(user: User) {
    if (user.ranking_hidden) return <span className="status-pill status-pill--warn">{m.admin.statusExcluded}</span>
    if (user.disabled || user.withdrawn) return <span className="status-pill status-pill--neutral">{m.admin.statusOffBoard}</span>
    return <span className="status-pill status-pill--ok">{m.admin.statusListed}</span>
  }
  return <section aria-labelledby={hiddenOnly ? undefined : 'users-title'} aria-label={hiddenOnly ? m.admin.excludedRegionAria : undefined}>
    {!hiddenOnly && <div className="admin-heading"><div><p className="eyebrow">{m.admin.usersEyebrow}</p><h1 id="users-title">{m.admin.usersTitle}</h1></div></div>}
    <form className="admin-search" onSubmit={search}>
      <label className="sr-only" htmlFor={searchId}>{active ? m.admin.searchLabel : ''}</label><input id={searchId} name="query" type="search" placeholder={m.admin.searchPlaceholder} maxLength={80} autoComplete="off" />
      <button className="admin-button" type="submit" disabled={loading || !!busy}>{m.admin.searchButton}</button>
    </form>
    {!data && loading && <div className="admin-list-placeholder" aria-hidden="true" />}
    {errorMessage && <div className="admin-list-error"><p className="admin-error" role="alert">{errorMessage}</p><button className="admin-button" onClick={() => setAttempt((old) => old + 1)}>{m.admin.retryUsersButton}</button></div>}
    {data && <div className="admin-surface">
      {users.length === 0 ? <p className="admin-empty">{m.admin.emptyUsers}</p> : <table className="admin-table admin-users" aria-busy={loading}><caption className="sr-only">{m.admin.usersCaption}</caption><colgroup><col className="admin-col-id" /><col className="admin-col-user" /><col className="admin-col-time" /><col className="admin-col-status" /><col className="admin-col-status" /><col className="admin-col-action" /></colgroup><thead><tr><th scope="col">{m.admin.colId}</th><th scope="col">{m.admin.colUser}</th><th scope="col">{m.admin.colRegisteredAt}</th><th scope="col">{m.admin.colBanStatus}</th><th scope="col">{m.admin.colBoardStatus}</th><th scope="col">{m.admin.colAction}</th></tr></thead><tbody>{users.map((user) => <tr key={user.id}>
        <td data-label={m.admin.colId}>{user.id}</td><td data-label={m.admin.colUser}><div className="participant"><Avatar id={user.avatar_id} catalog={avatars} /><div className="participant-copy"><span>{user.display_name}</span>{user.server_id && <span className="participant-meta">{user.server_id}</span>}</div></div></td><td data-label={m.admin.colRegisteredAt}>{user.created_at.slice(0, 19).replace('T', ' ')}</td><td data-label={m.admin.colBanStatus}>{renderSeatStatus(user)}</td><td data-label={m.admin.colBoardStatus}>{renderBoardStatus(user)}</td><td data-label={m.admin.colAction}>{hiddenOnly ? <div className="admin-restore-actions"><button className="admin-button admin-button--moderation" disabled={stale || !!busy || user.disabled} onClick={() => setAction({ kind: 'restore', user })}>{busy?.id === user.id && busy.kind === 'visibility' ? m.admin.savingBusy : m.admin.restoreButton}</button>{user.disabled && <><p className="admin-note">{m.admin.unbanFirstNote}</p><Link href="/admin/users">{m.admin.goToUsersLink}</Link></>}</div> : <div className="admin-row-actions"><button className={`admin-button admin-button--moderation${user.disabled ? '' : ' admin-button--danger'}`} disabled={stale || !!busy} onClick={() => setAction({ kind: 'ban', user })}>{busy?.id === user.id && busy.kind === 'ban' ? m.admin.savingBusy : user.disabled ? m.admin.unbanButton : m.admin.banButton}</button><button className={`admin-button admin-button--moderation${user.ranking_hidden ? '' : ' admin-button--danger'}`} disabled={stale || !!busy || (user.ranking_hidden && user.disabled)} title={user.ranking_hidden && user.disabled ? m.admin.unbanFirstNote : undefined} onClick={() => setAction({ kind: user.ranking_hidden ? 'restore' : 'exclude', user })}>{busy?.id === user.id && busy.kind === 'visibility' ? m.admin.savingBusy : user.ranking_hidden ? m.admin.restoreButton : m.admin.excludeButton}</button></div>}</td>
      </tr>)}</tbody></table>}
      <div className="admin-pagination"><span>{m.admin.paginationSummary(result.page, users.length)}</span><button className="admin-button" disabled={stale || !!busy || cursors.length === 1} onClick={() => setCursors((old) => old.slice(0, -1))}>{m.admin.prevPage}</button><button className="admin-button" disabled={stale || !!busy || !data.next_cursor} onClick={() => setCursors((old) => [...old, data.next_cursor])}>{m.admin.nextPage}</button></div>
    </div>}
    {action && <ConfirmDialog title={action.kind === 'restore' ? m.admin.confirmRestoreTitle(action.user.display_name) : action.kind === 'exclude' ? m.admin.excludeConfirmTitle(action.user.display_name) : action.user.disabled ? m.admin.confirmUnbanTitle(action.user.display_name) : m.admin.confirmBanTitle(action.user.display_name)} detail={action.kind === 'ban' && !action.user.disabled ? m.admin.confirmBanDetail : action.kind === 'exclude' ? m.admin.excludeConfirmDetail : undefined} onCancel={() => setAction(null)} onConfirm={() => action.kind === 'ban' ? setBan(action.user) : setRankingHidden(action.user, action.kind === 'exclude')} />}
  </section>
}
