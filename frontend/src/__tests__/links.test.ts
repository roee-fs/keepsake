import { describe, expect, it } from 'vitest'
import { resolveLink } from '../lib/links'

// Mirrors tests of okf_core.links: the console and the store MUST resolve alike.
describe('resolveLink', () => {
  it.each([
    ['../infra/postgres.md', 'architecture/overview', 'infra/postgres'],
    ['ingest.md#backpressure', 'architecture/overview', 'architecture/ingest'],
    ['sibling', 'top', 'sibling'],
    ['/infra/postgres.md', 'a/b/c', 'infra/postgres'],
    ['./x/../y.md', 'a/b', 'a/y'],
  ])('resolves %s from %s to %s', (target, source, expected) => {
    expect(resolveLink(target, source)).toBe(expected)
  })

  it.each([
    ['https://example.com/a.md'],
    ['mailto:a@b.c'],
    ['//cdn.example.com/x'],
    ['#heading'],
    ['../../escape.md'],
    ['/../escape'],
  ])('names no concept for %s', (target) => {
    expect(resolveLink(target, 'a/b')).toBeNull()
  })

  it('names no concept for . from a top-level concept', () => {
    expect(resolveLink('.', 'top')).toBeNull()
  })
})
