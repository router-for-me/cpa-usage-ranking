import { useEffect, useState, type FormEvent } from 'react'
import { formatLocalizedError, useI18n } from '../i18n'
import { AdminError, adminRequest, csrfRequest, jsonRequest, type AdminSession } from './api'
import { ConfirmDialog } from './ConfirmDialog'

type Account = { id: string; username: string; created_at: string }
type AccountsData = { owner: { username: string }; accounts: Account[] }

export function Accounts({ session, onExpired, active = true }: { session: AdminSession; onExpired: () => void; active?: boolean }) {
  const { locale, m } = useI18n()
  const [data, setData] = useState<AccountsData | null>(null)
  const [error, setError] = useState<unknown>(null)
  const [createError, setCreateError] = useState<unknown>(null)
  const [version, setVersion] = useState(0)
  const [loaded, setLoaded] = useState(-1)
  const loading = loaded !== version
  const [busy, setBusy] = useState('')
  const [removing, setRemoving] = useState<Account | null>(null)
  const errorMessage = error == null ? '' : formatLocalizedError(error, (msg) => msg.admin.errors.network, locale)
  const createErrorMessage = createError == null ? '' : formatLocalizedError(createError, (msg) => msg.admin.errors.network, locale)
  useEffect(() => {
    if (!active) return
    return () => { setRemoving(null); setVersion((old) => old + 1) }
  }, [active])
  useEffect(() => {
    if (!active) return
    let current = true
    adminRequest<AccountsData>('accounts').then((value) => {
      if (current) { setData(value); setError(null); setLoaded(version) }
    }).catch((err: unknown) => { if (current) { setError(err ?? true); setLoaded(version); if (err instanceof AdminError && err.status === 401) onExpired() } })
    return () => { current = false }
  }, [version, onExpired, active])
  async function create(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (busy) return
    const element = event.currentTarget
    const form = new FormData(element)
    setBusy('create'); setCreateError(null)
    try {
      await adminRequest('accounts', jsonRequest('POST', { username: form.get('username'), password: form.get('password') }, session.csrf_token))
      element.reset(); setVersion((old) => old + 1)
    } catch (err) { setCreateError(err ?? true); if (err instanceof AdminError && err.status === 401) onExpired() }
    finally { setBusy('') }
  }
  async function remove(account: Account) {
    if (busy) return
    setBusy(account.id); setError(null)
    try {
      await adminRequest(`accounts/${account.id}`, csrfRequest('DELETE', session.csrf_token))
      setVersion((old) => old + 1)
    } catch (err) { if (err instanceof AdminError && err.status === 401) onExpired(); throw err }
    finally { setBusy('') }
  }
  return <section aria-labelledby="accounts-title">
    <div className="admin-heading"><div><p className="eyebrow">{m.admin.accountsEyebrow}</p><h1 id="accounts-title">{m.admin.accountsTitle}</h1></div></div>
    {errorMessage && <div><p className="admin-error" role="alert">{errorMessage}</p><button className="admin-button" disabled={loading || !!busy} onClick={() => setVersion((old) => old + 1)}>{m.admin.reloadAccountsButton}</button></div>}
    <div className="admin-accounts-layout">
      <section className="admin-card admin-account-form" aria-labelledby="add-account-title"><h2 className="admin-account-title" id="add-account-title">{m.admin.addAccountTitle}</h2><form onSubmit={(event) => void create(event)} aria-busy={busy === 'create'}>
        <label htmlFor="account-username">{m.admin.usernameLabel}</label><input id="account-username" name="username" autoComplete="off" required minLength={3} maxLength={64} pattern="[A-Za-z0-9][A-Za-z0-9_.\-]{2,63}" />
        <label htmlFor="account-password">{m.admin.initialPasswordLabel}</label><input id="account-password" name="password" type="password" autoComplete="new-password" required minLength={12} maxLength={72} />
        {createErrorMessage && <p className="admin-error" role="alert">{createErrorMessage}</p>}
        <button className="admin-button admin-button--primary" type="submit" disabled={!!busy}>{busy === 'create' ? m.admin.addingBusy : m.admin.addAccountButton}</button>
      </form></section>
      <section className="admin-card admin-account-list" aria-labelledby="accounts-list-title" aria-busy={loading}>
        <h2 className="admin-account-title" id="accounts-list-title">{m.admin.accountListTitle}</h2>
        {!data && loading && <div className="admin-list-placeholder" aria-hidden="true" />}
        {data && (data.accounts.length === 0 ? <p className="admin-empty">{m.admin.emptyAccounts}</p> : <table className="admin-table"><caption className="sr-only">{m.admin.accountsCaption}</caption><colgroup><col className="admin-col-account-id" /><col className="admin-col-account" /><col className="admin-col-account-time" /><col className="admin-col-account-action" /></colgroup><thead><tr><th scope="col">{m.admin.colId}</th><th scope="col">{m.admin.usernameLabel}</th><th scope="col">{m.admin.colCreatedAt}</th><th scope="col">{m.admin.colAction}</th></tr></thead><tbody>{data.accounts.map((account) => <tr key={account.id}>
          <td data-label={m.admin.colId}>{account.id}</td><td data-label={m.admin.usernameLabel}>{account.username}</td><td data-label={m.admin.colCreatedAt}>{account.created_at.slice(0, 19).replace('T', ' ')}</td><td data-label={m.admin.colAction}><button className="admin-button admin-button--danger admin-button--moderation" disabled={loading || !!errorMessage || !!busy} onClick={() => setRemoving(account)}>{busy === account.id ? m.admin.deletingBusy : m.admin.deleteButton}</button></td>
        </tr>)}</tbody></table>)}
      </section>
    </div>
    {removing && <ConfirmDialog title={m.admin.confirmDeleteAdminTitle(removing.username)} detail={m.admin.confirmDeleteAdminDetail} onCancel={() => setRemoving(null)} onConfirm={() => remove(removing)} />}
  </section>
}
