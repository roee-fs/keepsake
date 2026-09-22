import { beforeAll, describe, expect, it } from 'vitest'
import { safeRedirectTarget } from '../lib/session'

// safeRedirectTarget only reads window.location.origin at call time, so a bare stub
// is enough and no jsdom is needed.
beforeAll(() => {
  ;(globalThis as unknown as { window: Window }).window = {
    location: { origin: 'https://keepsake.test' },
  } as Window
})

describe('safeRedirectTarget', () => {
  it.each<[string | undefined, string]>([
    [undefined, '/'],
    ['', '/'],
    ['/', '/'],
    ['/concepts/foo?q=bar', '/concepts/foo?q=bar'],
    ['//evil.com', '/'],
    // The case that mattered: a `startsWith('//')` check does not catch this one.
    ['/\\evil.com', '/'],
    ['https://evil.com', '/'],
    ['https://evil.com/concepts', '/'],
    ['javascript:alert(1)', '/'],
  ])('%s -> %s', (candidate, expected) => {
    expect(safeRedirectTarget(candidate)).toBe(expected)
  })
})
