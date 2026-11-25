import { useEffect } from 'react'
import App from './App'
import AdminApp from './admin/AdminApp'
import { SessionProvider } from './admin/SessionProvider'
import { usePathname } from './navigation'

export default function RootApp() {
  const pathname = usePathname()
  const admin = pathname.startsWith('/admin/')
  useEffect(() => {
    if (window.location.pathname !== pathname) {
      window.history.replaceState({}, '', `${pathname}${window.location.search}${window.location.hash}`)
    }
  }, [pathname])
  return <SessionProvider>
    <div className="route-view" hidden={admin}><App active={!admin} /></div>
    {admin && <div className="route-view"><AdminApp /></div>}
  </SessionProvider>
}
