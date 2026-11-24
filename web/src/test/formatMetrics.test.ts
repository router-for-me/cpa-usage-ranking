import { describe, expect, it } from 'vitest'
import { exactMetric, formatCount, formatDuration, formatMetric } from '../formatMetrics'

describe('compact counts with exact integer scaling', () => {
  it.each([
    ['0', '0'], ['999', '999'], ['1000', '1.00 K'], ['12345', '12.35 K'], ['999994', '999.99 K'], ['999995', '1.00 M'],
    ['1000000', '1.00 M'], ['999995000', '1.00 B'], ['123456789', '123.46 M'], ['1000000000', '1.00 B'],
    ['9007199254740993', '9007199.25 B'], ['9223372036854775807', '9223372036.85 B'],
  ])('%s → %s', (value, expected) => expect(formatCount(value)).toBe(expected))
  it('retains every digit in the expanded original', () => {
    expect(exactMetric('9007199254740993', 'count')).toBe('9,007,199,254,740,993')
    expect(exactMetric('9223372036854775807', 'count')).toBe('9,223,372,036,854,775,807')
  })
})

describe('adaptive durations', () => {
  it.each([
    ['0.00', '0 ms'], ['0.01', '<1 ms'], ['12.00', '12 ms'], ['12.50', '13 ms'], ['999.49', '999 ms'], ['999.50', '1 s'],
    ['1000.00', '1 s'], ['1234.00', '1.23 s'], ['9999.99', '10 s'], ['12345.00', '12.3 s'], ['59999.99', '1 m'],
    ['60000.00', '1 m'], ['61000.00', '1 m 01 s'], ['3661000.00', '1 h 01 m'], ['90000000.00', '1 d 1 h'],
  ])('%s → %s', (value, expected) => expect(formatDuration(value)).toBe(expected))
  it('keeps extreme finite values compact without losing the original milliseconds', () => {
    const large = `${'1' + '0'.repeat(308)}.00`
    expect(formatDuration(large).length).toBeLessThan(40)
    expect(exactMetric(large, 'duration')).toBe(`${large} ms`)
  })
  it('never turns missing measurements into zero or success scores into percentages', () => {
    expect(formatMetric(null, 'duration')).toBe('—')
    expect(formatMetric(undefined, 'count')).toBe('—')
    expect(formatMetric('0.00', 'percent')).toBe('0.00 %')
    expect(formatMetric('20.65', 'points')).toBe('20.65 pts')
    expect(formatMetric('20.65', 'points', 'zh')).toBe('20.65 分')
    expect(exactMetric('20.65', 'points')).toBe('20.65 pts')
    expect(exactMetric('20.65', 'points', 'zh')).toBe('20.65 分')
  })
  it('falls back safely without throwing on non-integer count or non-numeric duration', () => {
    expect(formatCount('12.34')).toBe('12.34')
    expect(exactMetric('12.34', 'count')).toBe('12.34')
    expect(formatDuration('N/A')).toBe('N/A')
  })
})
