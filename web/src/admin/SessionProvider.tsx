import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { formatLocalizedError, useI18n } from '../i18n'
import { usePathname } from '../navigation'
import { adminRequest, type AdminSession } from './api'
import { SessionContext } from './sessionContext'

// One live in-memory session for this document, shared by public and console
// navigation. It is never persisted or trusted instead of server authorization.
export function SessionProvider({ children }: { children: ReactNode }) {
  const { locale } = useI18n()
  const pathname = usePathname()
  const [session, setSession] = useState<AdminSession | null>(null)
  const [rawError, setRawError] = useState<unknown>(null)
  const error = rawError == null ? '' : formatLocalizedError(rawError, (m) => m.admin.errors.network, locale)
  const generation = useRef(0)
  const active = useRef(false)
  const pending = useRef<Promise<void> | null>(null)
  const accept = useCallback((value: AdminSession) => {
    generation.current++
    pending.current = null
    const expired = value.expires_at && Date.parse(value.expires_at) <= Date.now()
    setSession(expired ? { enabled: value.enabled, authenticated: false } : value)
    setRawError(null)
  }, [])
  const refresh = useCallback(() => {
    if (pending.current) return pending.current
    const ticket = ++generation.current
    const request = adminRequest<AdminSession>('session').then((value) => {
      if (active.current && ticket === generation.current) accept(value)
    }).catch((err: unknown) => { if (active.current && ticket === generation.current) setRawError(err ?? true) }).finally(() => { if (pending.current === request) pending.current = null })
    pending.current = request
    return request
  }, [accept])
  useEffect(() => {
    active.current = true
    const expired = () => accept({ enabled: true, authenticated: false })
    void refresh()
    window.addEventListener('ranking:session-expired', expired)
    return () => { active.current = false; pending.current = null; window.removeEventListener('ranking:session-expired', expired) }
  }, [refresh, accept])
  // 首屏探测一次 session 后，仅当后台启用且处于已登录状态或管理路由时才保持轮询与焦点复检。
  const shouldMonitor = session !== null && session.enabled !== false && (session.authenticated === true || pathname.startsWith('/admin'))
  useEffect(() => {
    if (!shouldMonitor) return
    const check = () => { void refresh() }
    const visible = () => { if (document.visibilityState === 'visible') check() }
    window.addEventListener('focus', check)
    document.addEventListener('visibilitychange', visible)
    const interval = window.setInterval(check, 60_000)
    return () => {
      window.clearInterval(interval)
      window.removeEventListener('focus', check)
      document.removeEventListener('visibilitychange', visible)
    }
  }, [shouldMonitor, refresh])
  useEffect(() => {
    if (!session?.expires_at) return
    const timeout = window.setTimeout(() => accept({ enabled: session.enabled, authenticated: false }), Math.max(0, Date.parse(session.expires_at) - Date.now()))
    return () => window.clearTimeout(timeout)
  }, [session?.expires_at, session?.enabled, accept])
  return <SessionContext value={{ session, error, refresh, accept }}>{children}</SessionContext>
}
