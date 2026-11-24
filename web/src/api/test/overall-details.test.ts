import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchLeaderboard } from '../leaderboards'

const details = { total_tokens: '9223372036854775807', request_count: '9007199254740993', success_rate: '75.00', cache_read_rate: '25.13', ttft_mean_ms: '10.33', latency_mean_ms: '200.50' }
function response(extra: Record<string, unknown>, topLevel: Record<string, unknown> = {}, headers: HeadersInit = { 'Content-Type': 'application/json' }) {
  vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({
    board: 'overall',
    period: 'today',
    start: '2026-11-16',
    end: '2026-11-17',
    generated_at: '2026-11-16T12:00:00Z',
    entries: [{ rank: 1, client_id: '1', avatar_id: '1', display_name: '真实数据', metric: '51.16', ...extra }],
    ...topLevel,
  }), { headers })))
}
afterEach(() => vi.unstubAllGlobals())

describe('overall details and relaxed response contract', () => {
  it('preserves int64 strings and all six detail values', async () => {
    response({ details })
    expect((await fetchLeaderboard('overall', 'today', new AbortController().signal)).entries[0].details).toEqual(details)
  })
  it('distinguishes null measurements from measured zero', async () => {
    const values = { ...details, cache_read_rate: null, ttft_mean_ms: null, latency_mean_ms: '0.00' }
    response({ details: values })
    expect((await fetchLeaderboard('overall', 'today', new AbortController().signal)).entries[0].details).toEqual(values)
  })
  it('accepts an older response without inventing detail values', async () => {
    response({})
    expect((await fetchLeaderboard('overall', 'today', new AbortController().signal)).entries[0].details).toBeUndefined()
  })
  it('accepts open-ended periods (end: null), omitted content-type, and entries beyond 100', async () => {
    const entries = Array.from({ length: 105 }, (_, i) => ({
      rank: i + 1,
      client_id: String(i + 1),
      avatar_id: '1',
      display_name: `user-${i + 1}`,
      metric: '50.00',
    }))
    response({}, { end: null, entries }, {})
    const board = await fetchLeaderboard('overall', 'today', new AbortController().signal)
    expect(board.end).toBeNull()
    expect(board.entries).toHaveLength(105)
  })
  it('filters out entries missing basic fields without failing the entire board', async () => {
    response({}, {
      entries: [
        { rank: 1, client_id: '1', avatar_id: '1', display_name: 'valid', metric: '51.16' },
        { rank: 'bad', client_id: '2' },
        null,
      ],
    })
    const board = await fetchLeaderboard('overall', 'today', new AbortController().signal)
    expect(board.entries).toHaveLength(1)
    expect(board.entries[0].display_name).toBe('valid')
  })
  it.each([
    { start: 123 },
    { end: 123 },
    { generated_at: null },
    { entries: 'not-an-array' },
  ])('rejects invalid top-level envelope %j', async (invalid) => {
    response({}, invalid)
    await expect(fetchLeaderboard('overall', 'today', new AbortController().signal)).rejects.toThrow('Invalid leaderboard response. Please retry shortly.')
  })
})
