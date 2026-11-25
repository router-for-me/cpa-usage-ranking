import { useSyncExternalStore } from 'react'

const routes = new Set(['/', '/admin/login', '/admin/leaderboards', '/admin/users', '/admin/accounts'])
const changed = 'ranking:navigation'
const subscribe = (listener: () => void) => {
  window.addEventListener('popstate', listener)
  window.addEventListener(changed, listener)
  return () => { window.removeEventListener('popstate', listener); window.removeEventListener(changed, listener) }
}

function normalizePathname(pathname: string) {
  if (pathname.length > 1) return pathname.replace(/\/+$/, '') || '/'
  return pathname
}

function parseAppTarget(target: string): { pathname: string; href: string } | null {
  try {
    const url = new URL(target, window.location.origin)
    if (url.origin !== window.location.origin) return null
    const pathname = normalizePathname(url.pathname)
    if (!routes.has(pathname)) return null
    return { pathname, href: `${pathname}${url.search}${url.hash}` }
  } catch {
    return null
  }
}

export function usePathname() {
  return useSyncExternalStore(subscribe, () => normalizePathname(window.location.pathname))
}

export function navigate(path: string, replace = false) {
  const parsed = parseAppTarget(path)
  if (!parsed) throw new Error('Unknown application route')
  const current = `${normalizePathname(window.location.pathname)}${window.location.search}${window.location.hash}`
  if (current !== parsed.href || window.location.pathname !== parsed.pathname) {
    window.history[replace ? 'replaceState' : 'pushState']({}, '', parsed.href)
    window.dispatchEvent(new Event(changed))
    document.scrollingElement?.scrollTo?.({ top: 0, behavior: 'instant' })
  }
}

export function isAppRoute(path: string) { return parseAppTarget(path) !== null }
