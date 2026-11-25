import { useEffect, useRef, useState, type FormEvent } from 'react'
import { formatLocalizedError, useI18n } from '../i18n'
import { Leaderboards } from './Leaderboards'
import { AdminError, adminRequest, csrfRequest, jsonRequest, type AdminSession } from './api'
import { Link } from '../Link'
import { navigate, usePathname } from '../navigation'
import { useSession } from './useSession'
import { Accounts } from './Accounts'
import { Users } from './Users'
import { SiteFooter } from '../components/SiteFooter'
import { SiteHeader } from '../components/SiteHeader'
import '../styles/app.css'
import './admin.css'

function Login() {
  const { locale, m } = useI18n()
  const { accept } = useSession()
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<unknown>(null)
  const errorMessage = error == null ? '' : formatLocalizedError(error, (msg) => msg.admin.errors.network, locale)
  const errorRef = useRef<HTMLParagraphElement>(null)
  const [showPassword, setShowPassword] = useState(false)
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const form = new FormData(event.currentTarget)
    setBusy(true); setError(null)
    try {
      const value = await adminRequest<AdminSession>('session', jsonRequest('POST', { username: form.get('username'), password: form.get('password') }))
      accept(value)
      navigate('/admin/leaderboards', true)
    } catch (err) { setError(err ?? true); setBusy(false) }
  }
  useEffect(() => { if (error != null) errorRef.current?.focus() }, [error])
  return <section className="admin-card admin-login" aria-labelledby="login-title">
    <p className="eyebrow">{m.admin.loginEyebrow}</p><h1 id="login-title">{m.admin.loginTitle}</h1>
    <form onSubmit={(event) => void submit(event)} aria-busy={busy}>
      <label htmlFor="username">{m.admin.usernameLabel}</label><input id="username" name="username" autoComplete="username" required maxLength={64} />
      <label htmlFor="password">{m.admin.passwordLabel}</label><input id="password" name="password" type={showPassword ? 'text' : 'password'} autoComplete="current-password" required maxLength={72} />
      <label className="admin-check"><input type="checkbox" checked={showPassword} onChange={(event) => setShowPassword(event.target.checked)} />{m.admin.showPassword}</label>
      {errorMessage && <p className="admin-error" role="alert" tabIndex={-1} ref={errorRef}>{errorMessage}</p>}
      <button className="admin-button admin-button--primary" type="submit" disabled={busy}>{busy ? m.admin.signingIn : m.admin.signInButton}</button>
    </form>
  </section>
}

export default function AdminApp() {
  const { locale, m } = useI18n()
  const { session, error, refresh, accept } = useSession()
  const path = usePathname()
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState<{ csrf?: string; error: unknown } | null>(null)
  const login = path === '/admin/login'
  const accounts = path === '/admin/accounts'
  const users = path === '/admin/users'
  const visibleActionError = actionError && actionError.csrf === session?.csrf_token
    ? formatLocalizedError(actionError.error, (msg) => msg.admin.errors.network, locale)
    : ''
  const main = useRef<HTMLElement>(null)
  useEffect(() => { main.current?.focus({ preventScroll: true }) }, [path])
  useEffect(() => {
    if (session?.enabled && !session.authenticated && !login) navigate('/admin/login', true)
    if (session?.authenticated && login) navigate('/admin/leaderboards', true)
  }, [session, login])
  async function logout() {
    if (busy) return
    const csrf = session?.csrf_token
    setBusy(true); setActionError(null)
    try { await adminRequest('session', csrfRequest('DELETE', csrf)); accept({ enabled: true, authenticated: false }); navigate('/') }
    catch (err) { if (!(err instanceof AdminError && err.status === 401)) { setActionError({ csrf, error: err ?? true }); void refresh() } }
    finally { setBusy(false) }
  }
  return <div className="app-shell admin-shell">
    <SiteHeader admin />
    <main id="admin-main" className="page-width admin-main" tabIndex={-1} ref={main}>
      {!session && !error && <div className="admin-list-placeholder" aria-hidden="true" />}
      {error && <div role="alert"><p className="admin-error">{error}</p><button className="admin-button" onClick={() => void refresh()}>{m.admin.retrySessionButton}</button></div>}
      {session && !session.enabled && <section className="admin-card"><h1>{m.admin.disabledTitle}</h1><Link className="primary-link" href="/">{m.admin.backToPublic}</Link></section>}
      {session?.enabled && !session.authenticated && login && <Login />}
      {session?.authenticated && !login && <>
        <div className="admin-nav-wrap"><nav className="admin-nav" aria-label={m.admin.navAria}><Link href="/admin/leaderboards" aria-current={!accounts && !users ? 'page' : undefined}>{m.admin.tabLeaderboards}</Link><Link href="/admin/users" aria-current={users ? 'page' : undefined}>{m.admin.tabUsers}</Link>{session.user?.owner && <Link href="/admin/accounts" aria-current={accounts ? 'page' : undefined}>{m.admin.tabAccounts}</Link>}</nav><div className="admin-identity"><span>{session.user?.username}</span><button className="logout-link" disabled={busy} onClick={() => void logout()}>{busy ? m.admin.signingOut : m.admin.signOut}</button></div></div>
        {visibleActionError && <p className="admin-error" role="alert">{visibleActionError}</p>}
        <div className="tab-view" hidden={accounts || users}><Leaderboards active={!accounts && !users} session={session} onExpired={refresh} /></div>
        <div className="tab-view" hidden={!users}><Users active={users} session={session} onExpired={refresh} /></div>
        {session.user?.owner ? <div className="tab-view" hidden={!accounts}><Accounts active={accounts} session={session} onExpired={refresh} /></div> : accounts && <section className="admin-card"><h1>{m.admin.forbiddenTitle}</h1><Link className="primary-link" href="/admin/leaderboards">{m.admin.backToBoards}</Link></section>}
      </>}
    </main>
    <SiteFooter />
  </div>
}
