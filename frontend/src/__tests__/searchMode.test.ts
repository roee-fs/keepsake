import { describe, expect, it } from 'vitest'
import { modeOf } from '../routes/concepts.index'

describe('modeOf', () => {
  it.each<[string, 'browse' | 'search' | 'grep']>([
    ['', 'browse'],
    ['auth', 'search'],
    ['/^auth.*/', 'grep'],
    ['/', 'grep'],
  ])('%s -> %s', (q, expected) => {
    expect(modeOf(q)).toBe(expected)
  })
})
