import { LocalizedError } from '../i18n'

export type Board = 'overall' | 'usage' | 'cache' | 'success' | 'ttft'
export type Period = 'today' | 'yesterday' | 'week' | 'month'
export type OverallDetails = { total_tokens: string; request_count: string; success_rate: string; cache_read_rate: string | null; ttft_mean_ms: string | null; latency_mean_ms: string | null }
export type Entry = { rank: number; client_id: string; display_name: string; avatar_id: string; metric: string; details?: OverallDetails }
export type Leaderboard = { board: Board; period: Period; start: string; end: string | null; generated_at: string; entries: Entry[] }
export type AvatarCatalog = { defaultPath: string; paths: Map<string, string> }

const object = (value: unknown): value is Record<string, unknown> => typeof value === 'object' && value !== null && !Array.isArray(value)

function hasEntryShape(value: unknown): value is Entry {
  return object(value)
    && typeof value.rank === 'number'
    && typeof value.client_id === 'string'
    && typeof value.display_name === 'string'
    && typeof value.avatar_id === 'string'
    && typeof value.metric === 'string'
}

export async function fetchLeaderboard(board: Board, period: Period, signal: AbortSignal): Promise<Leaderboard> {
  const controller = new AbortController()
  const cancel = () => controller.abort()
  signal.addEventListener('abort', cancel, { once: true })
  const timer = setTimeout(() => controller.abort(), 8_000)
  try {
    signal.throwIfAborted()
    const response = await fetch(`/v1/leaderboards/${board}/${period}`, { signal: controller.signal, cache: 'no-store', credentials: 'omit', headers: { Accept: 'application/json' } })
    if (!response.ok) {
      throw response.status === 503
        ? new LocalizedError((m) => m.leaderboardErrors.notReady)
        : new LocalizedError((m) => m.leaderboardErrors.statusFailed(response.status))
    }
    let data: unknown
    try { data = await response.json() } catch { throw new LocalizedError((m) => m.leaderboardErrors.invalidResponse) }
    if (!object(data) || typeof data.start !== 'string' || (data.end !== null && typeof data.end !== 'string') || typeof data.generated_at !== 'string' || !Array.isArray(data.entries)) {
      throw new LocalizedError((m) => m.leaderboardErrors.invalidResponse)
    }
    const entries = data.entries.filter(hasEntryShape)
    return { board, period, start: data.start, end: data.end, generated_at: data.generated_at, entries }
  } catch (error) {
    if (signal.aborted) throw signal.reason
    if (controller.signal.aborted) throw new LocalizedError((m) => m.leaderboardErrors.timeout)
    if (error instanceof TypeError) throw new LocalizedError((m) => m.leaderboardErrors.network)
    if (error instanceof Error) throw error
    throw new LocalizedError((m) => m.leaderboardErrors.generic)
  } finally {
    clearTimeout(timer)
    signal.removeEventListener('abort', cancel)
  }
}

// 头像只能来自同源目录列出的相对资源；缺项使用目录默认项。
export async function fetchAvatarCatalog(signal: AbortSignal): Promise<AvatarCatalog> {
  const response = await fetch('/avatars/catalog.json', { signal, cache: 'no-cache', credentials: 'omit' })
  if (!response.ok) throw new Error('avatar catalog unavailable')
  const value: unknown = await response.json()
  if (!object(value) || !Number.isSafeInteger(value.default_avatar_id) || !Array.isArray(value.avatars)) throw new Error('invalid avatar catalog')
  const paths = new Map<string, string>()
  for (const item of value.avatars) {
    if (!object(item) || !Number.isSafeInteger(item.avatar_id) || typeof item.path !== 'string' || !/^images\/[a-zA-Z0-9_./-]+$/.test(item.path) || item.path.split('/').some((part) => part === '.' || part === '..' || part === '')) continue
    paths.set(String(item.avatar_id), `/avatars/${item.path}`)
  }
  const defaultPath = paths.get(String(value.default_avatar_id))
  if (!defaultPath) throw new Error('invalid default avatar')
  return { defaultPath, paths }
}
