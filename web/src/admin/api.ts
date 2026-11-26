import { LocalizedError, type Messages } from '../i18n'

export type AdminSession = { enabled: boolean; authenticated: boolean; user?: { id: string; username: string; owner: boolean }; csrf_token?: string; expires_at?: string }

export class AdminError extends LocalizedError {
  readonly status: number
  constructor(status: number, format: (messages: Messages) => string) {
    super(format)
    this.name = 'AdminError'
    this.status = status
  }
}

export async function adminRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const controller = new AbortController()
  const timeout = window.setTimeout(() => controller.abort(), 10_000)
  try {
    const response = await fetch(`/v1/admin/${path}`, { ...init, credentials: 'same-origin', cache: 'no-store', signal: controller.signal })
    if (!response.ok) {
      if (response.status === 401 && !(path === 'session' && init.method === 'POST')) window.dispatchEvent(new Event('ranking:session-expired'))
      const status = response.status
      let code = ''
      if (status === 403) {
        try {
          const payload = await response.json() as { error?: { code?: unknown } }
          if (typeof payload?.error?.code === 'string') code = payload.error.code
        } catch {
          // Fall back to generic 403 error message when body is not JSON.
        }
      }
      const formats: Record<number, (messages: Messages) => string> = {
        400: (m) => m.admin.errors.badRequest,
        401: (m) => path === 'session' && init.method === 'POST' ? m.admin.errors.invalidCredentials : m.admin.errors.sessionExpired,
        403: (m) => code === 'https_required'
          ? m.admin.errors.httpsRequired
          : code === 'same_origin_required'
            ? m.admin.errors.sameOriginRequired
            : m.admin.errors.forbidden,
        404: (m) => m.admin.errors.notFound,
        409: (m) => path.startsWith('users/') ? m.admin.errors.userStillBanned : m.admin.errors.usernameExists,
        429: (m) => m.admin.errors.rateLimited,
      }
      throw new AdminError(status, formats[status] ?? ((m) => m.admin.errors.unavailable(status)))
    }
    return response.status === 204 ? undefined as T : await response.json() as T
  } catch (error) {
    if (error instanceof AdminError) throw error
    throw new LocalizedError((m) => m.admin.errors.network)
  } finally { window.clearTimeout(timeout) }
}

export function jsonRequest(method: string, value: unknown, csrf?: string): RequestInit {
  return { method, headers: { 'Content-Type': 'application/json', ...(csrf ? { 'X-CSRF-Token': csrf } : {}) }, body: JSON.stringify(value) }
}

export function csrfRequest(method: string, csrf?: string): RequestInit {
  return { method, headers: csrf ? { 'X-CSRF-Token': csrf } : {} }
}
