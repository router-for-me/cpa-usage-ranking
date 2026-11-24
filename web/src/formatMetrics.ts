// Compact K/M/B and adaptive duration presentation for decimal-string
// counters. Never pass an int64 counter through Number before scaling it.
import { getLocale, getMessages, type Locale } from './i18n'

export type MetricKind = 'count' | 'duration' | 'percent' | 'points'

export function formatCount(value: string): string {
  if (!/^\d+$/.test(value)) return value
  const count = BigInt(value)
  if (count < 1000n) return count.toString()
  const units = [{ base: 1000n, suffix: 'K' }, { base: 1000000n, suffix: 'M' }, { base: 1000000000n, suffix: 'B' }]
  let index = count >= units[2].base ? 2 : count >= units[1].base ? 1 : 0
  let scaled = (count * 100n + units[index].base / 2n) / units[index].base
  if (scaled >= 100000n && index < units.length - 1) {
    index++
    scaled = (count * 100n + units[index].base / 2n) / units[index].base
  }
  return `${scaled / 100n}.${String(scaled % 100n).padStart(2, '0')} ${units[index].suffix}`
}

export function formatDuration(value: string): string {
  if (!/^\d+(?:\.\d+)?$/.test(value)) return value
  const [whole, fraction = ''] = value.split('.')
  const ticks = BigInt(whole) * 100n + BigInt(fraction.padEnd(2, '0').slice(0, 2))
  if (ticks === 0n) return '0 ms'
  if (ticks < 100n) return '<1 ms'
  if (ticks < 100000n) {
    const milliseconds = (ticks + 50n) / 100n
    if (milliseconds < 1000n) return `${milliseconds} ms`
  }
  if (ticks < 6000000n) {
    const places = ticks < 1000000n ? 100n : 10n
    const step = 100000n / places
    const seconds = (ticks + step / 2n) / step
    if (seconds < 60n * places) {
      const decimal = String(seconds % places).padStart(places === 100n ? 2 : 1, '0').replace(/0+$/, '')
      return `${seconds / places}${decimal ? `.${decimal}` : ''} s`
    }
  }
  let remaining = ticks / 100000n
  if (remaining < 60n) remaining = 60n
  const units = [{ size: 86400n, unit: 'd' }, { size: 3600n, unit: 'h' }, { size: 60n, unit: 'm' }, { size: 1n, unit: 's' }]
  const parts: string[] = []
  for (const { size, unit } of units) {
    const amount = remaining / size
    remaining %= size
    if (amount > 0n) {
      const number = amount >= 1000000000n ? Number(amount).toExponential(2) : parts.length && (unit === 'm' || unit === 's') ? String(amount).padStart(2, '0') : String(amount)
      parts.push(`${number} ${unit}`)
      if (parts.length === 2) break
    }
  }
  return parts.join(' ')
}

export function formatMetric(value: string | null | undefined, kind: MetricKind, locale: Locale = getLocale()): string {
  if (value == null) return '—'
  if (kind === 'count') return formatCount(value)
  if (kind === 'duration') return formatDuration(value)
  return `${value}${kind === 'percent' ? ' %' : ` ${getMessages(locale).table.pointsUnit}`}`
}

export function exactMetric(value: string | null | undefined, kind: MetricKind, locale: Locale = getLocale()): string {
  if (value == null) return '—'
  if (kind === 'count') return /^\d+$/.test(value) ? BigInt(value).toLocaleString('en-US') : value
  return `${value}${kind === 'duration' ? ' ms' : kind === 'percent' ? ' %' : ` ${getMessages(locale).table.pointsUnit}`}`
}
